#!/usr/bin/env python3
import argparse
import base64
import hashlib
import html
import json
import re
import subprocess
import sys
from pathlib import Path, PurePosixPath
from typing import Any, Dict, Iterable, List, Optional, Sequence, Tuple


SENSITIVE_MARKER = re.compile(
    r"-----BEGIN [A-Z ]*PRIVATE KEY-----|"
    r"\bBearer\s+[A-Za-z0-9._~+/=-]{12,}",
    re.I,
)
SENSITIVE_ASSIGNMENT = re.compile(
    r"\b(?P<key>password|passwd|token|secret|api[_-]?key|authorization)\b"
    r"[ \t]*[:=][ \t]*(?:"
    r"(?P<quote>['\"])(?P<quoted>[^\r\n'\"]{8,})(?P=quote)|"
    r"(?P<bare>[^\s,;]+)"
    r")",
    re.I,
)
CODE_EXPRESSION = re.compile(
    r"^[&*]?[_A-Za-z][_A-Za-z0-9]*"
    r"(?:(?:\.|::|->)[_A-Za-z][_A-Za-z0-9]*)*"
    r"(?:[ \t]*(?:\(|\[|\{).*)?"
    r"(?:[ \t]*[)}\]]+)?$"
)
REFERENCE_VALUE = re.compile(
    r"^(?:\$[_A-Za-z][_A-Za-z0-9]*|\$\{[^\r\n]+\}|\$\([^\r\n]+\)|"
    r"\{\{[^\r\n]+\}\}|<[^\r\n]+>)$"
)
CONFIG_SUFFIXES = {
    ".env", ".yaml", ".yml", ".toml", ".ini", ".conf", ".cfg", ".properties",
    ".sh", ".bash", ".zsh", ".fish", ".ps1",
}
SAFE_SENSITIVE_VALUES = {"REDACTED", "MASKED", "NONE", "NOT_REQUIRED"}
REF_VALUE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/@{}+~-]{0,254}$")
UNIT_KINDS = {"method", "function", "class", "block", "config", "sql", "file"}
STATUS_LABELS = {
    "A": "新增",
    "M": "修改",
    "D": "删除",
    "R": "重命名",
    "C": "复制",
    "T": "类型变化",
}


def project_root() -> Path:
    return Path(__file__).resolve().parents[4]


def fail(category: str, detail: str) -> int:
    print(
        "[CODE_CHANGE_VISUALIZER] stage=render level=error result=failed "
        f"error={category} detail={detail}"
    )
    return 1


def run_git(root: Path, *args: str, text: bool = True) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["git", "-C", str(root), *args],
        text=text,
        capture_output=True,
        check=False,
    )


def git_text(root: Path, *args: str) -> str:
    proc = run_git(root, *args)
    if proc.returncode != 0:
        raise ValueError(f"git {' '.join(args)} failed: {proc.stderr.strip()}")
    return proc.stdout


def git_bytes(root: Path, *args: str) -> bytes:
    proc = run_git(root, *args, text=False)
    if proc.returncode != 0:
        detail = proc.stderr.decode("utf-8", errors="replace").strip()
        raise ValueError(f"git {' '.join(args)} failed: {detail}")
    return proc.stdout


def load_object(path: Path, label: str, required: bool = True) -> Dict[str, Any]:
    if not path.is_file():
        if required:
            raise ValueError(f"missing {label}: {path.name}")
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except Exception as exc:
        raise ValueError(f"invalid {label} JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise ValueError(f"{label} must be a JSON object")
    return data


def safe_repo_path(raw: Any, allow_none: bool = False) -> Optional[str]:
    if raw is None and allow_none:
        return None
    if not isinstance(raw, str):
        raise ValueError("code path must be a string")
    value = raw.replace("\\", "/").strip()
    pure = PurePosixPath(value)
    if (
        not value
        or "\x00" in value
        or "\n" in value
        or pure.is_absolute()
        or any(part in ("", ".", "..") for part in pure.parts)
    ):
        raise ValueError(f"unsafe relative path: {raw}")
    return pure.as_posix()


def ensure_worktree_path(root: Path, relative: str, require_file: bool) -> Path:
    candidate = root.joinpath(*PurePosixPath(relative).parts)
    try:
        resolved = candidate.resolve(strict=require_file)
        resolved.relative_to(root.resolve())
    except (FileNotFoundError, ValueError) as exc:
        raise ValueError(f"path is missing or escapes project root: {relative}") from exc
    if require_file and not resolved.is_file():
        raise ValueError(f"code reference is not a file: {relative}")
    return resolved


def path_matches(path: str, scope: str) -> bool:
    return path == scope or path.startswith(scope.rstrip("/") + "/")


def iter_strings(value: Any) -> Iterable[str]:
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield str(key)
            yield from iter_strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from iter_strings(item)


def config_like_path(path: Optional[str]) -> bool:
    if not path:
        return True
    name = PurePosixPath(path).name.lower()
    return name == ".env" or name.startswith(".env.") or PurePosixPath(name).suffix in CONFIG_SUFFIXES


def contains_possible_credential(text: str, path: Optional[str] = None) -> bool:
    if SENSITIVE_MARKER.search(text):
        return True
    for match in SENSITIVE_ASSIGNMENT.finditer(text):
        value = match.group("quoted") if match.group("quoted") is not None else match.group("bare")
        value = value.strip()
        if len(value) < 8 or value.upper() in SAFE_SENSITIVE_VALUES or REFERENCE_VALUE.fullmatch(value):
            continue
        if match.group("quoted") is None and CODE_EXPRESSION.fullmatch(value):
            if path is not None and not config_like_path(path):
                continue
            if path is None and re.search(r"(?:\.|::|->|\(|\[|\{)", value):
                continue
        return True
    return False


def require_list(data: Dict[str, Any], key: str, allow_empty: bool = False) -> List[Any]:
    value = data.get(key)
    if not isinstance(value, list) or (not allow_empty and not value):
        kind = "a" if allow_empty else "a non-empty"
        raise ValueError(f"walkthrough.{key} must be {kind} list")
    return value


def resolve_ref(root: Path, raw: Any, label: str) -> str:
    if not isinstance(raw, str) or not REF_VALUE.fullmatch(raw) or raw.startswith("-"):
        raise ValueError(f"comparison.{label} is invalid")
    proc = run_git(root, "rev-parse", "--verify", f"{raw}^{{commit}}")
    if proc.returncode != 0:
        raise ValueError(f"comparison.{label} does not resolve to a commit: {raw}")
    sha = proc.stdout.strip().lower()
    if not re.fullmatch(r"[0-9a-f]{40,64}", sha):
        raise ValueError(f"comparison.{label} resolved to an invalid commit")
    return sha


def parse_name_status(data: bytes) -> List[Dict[str, Optional[str]]]:
    fields = [item.decode("utf-8", errors="surrogateescape") for item in data.split(b"\0") if item]
    records: List[Dict[str, Optional[str]]] = []
    index = 0
    while index < len(fields):
        raw_status = fields[index]
        index += 1
        status = raw_status[:1]
        if status in ("R", "C"):
            if index + 1 >= len(fields):
                raise ValueError("git name-status output is truncated")
            old_path = safe_repo_path(fields[index])
            new_path = safe_repo_path(fields[index + 1])
            index += 2
        else:
            if index >= len(fields):
                raise ValueError("git name-status output is truncated")
            path = safe_repo_path(fields[index])
            index += 1
            old_path = None if status == "A" else path
            new_path = None if status == "D" else path
        if status not in STATUS_LABELS:
            raise ValueError(f"unsupported git change status: {raw_status}")
        records.append({"status": status, "old_file": old_path, "new_file": new_path})
    return records


def git_blob(root: Path, commit: str, relative: Optional[str]) -> Optional[bytes]:
    if relative is None:
        return None
    proc = run_git(root, "show", f"{commit}:{relative}", text=False)
    if proc.returncode != 0:
        return None
    return proc.stdout


def worktree_blob(root: Path, relative: Optional[str]) -> Optional[bytes]:
    if relative is None:
        return None
    path = ensure_worktree_path(root, relative, require_file=True)
    return path.read_bytes()


def is_binary(value: Optional[bytes]) -> bool:
    return value is not None and b"\x00" in value[:8192]


def decode_source(value: Optional[bytes]) -> str:
    if value is None:
        return ""
    return value.decode("utf-8", errors="replace")


def record_in_scope(record: Dict[str, Optional[str]], scopes: Sequence[str]) -> bool:
    paths = [path for path in (record["old_file"], record["new_file"]) if path]
    return not scopes or any(path_matches(path, scope) for path in paths for scope in scopes)


def record_excluded(record: Dict[str, Optional[str]], excludes: Sequence[str]) -> bool:
    paths = [path for path in (record["old_file"], record["new_file"]) if path]
    return bool(paths) and all(any(path_matches(path, excluded) for excluded in excludes) for path in paths)


def relative_if_inside(root: Path, path: Path) -> Optional[str]:
    try:
        return path.resolve().relative_to(root.resolve()).as_posix()
    except ValueError:
        return None


def build_comparison(
    root: Path,
    data: Dict[str, Any],
    baseline: Dict[str, Any],
    automatic_excludes: Sequence[str],
) -> Tuple[Dict[str, Any], List[Dict[str, Any]]]:
    comparison = data.get("comparison")
    if not isinstance(comparison, dict):
        raise ValueError("walkthrough.comparison must be an object")
    mode = comparison.get("mode")
    if mode not in ("working_tree", "branch_compare"):
        raise ValueError("comparison.mode must be working_tree or branch_compare")

    raw_scopes = comparison.get("scope_paths", baseline.get("scope_paths", []))
    if not isinstance(raw_scopes, list):
        raise ValueError("comparison.scope_paths must be a list")
    scopes = sorted({safe_repo_path(item) for item in raw_scopes})
    raw_excludes = comparison.get("exclude_paths", [])
    if not isinstance(raw_excludes, list):
        raise ValueError("comparison.exclude_paths must be a list")
    excludes = sorted({safe_repo_path(item) for item in [*raw_excludes, *automatic_excludes] if item})

    dirty_paths = baseline.get("dirty_scope_paths", []) if baseline else []
    accepted_dirty = baseline.get("accepted_dirty_paths", []) if baseline else []
    if not isinstance(dirty_paths, list) or not isinstance(accepted_dirty, list):
        raise ValueError("baseline dirty fields must be lists")

    if mode == "working_tree":
        base_ref = comparison.get("base_ref") or baseline.get("base_commit") or "HEAD"
        base_sha = resolve_ref(root, base_ref, "base_ref")
        head_sha = resolve_ref(root, "HEAD", "head_ref")
        compare_sha = base_sha
        strategy = "working_tree"
        status_data = git_bytes(root, "diff", "--name-status", "-z", "--find-renames", compare_sha)
        records = parse_name_status(status_data)
        head_label = "WORKTREE"
    else:
        base_ref = comparison.get("base_ref")
        head_ref = comparison.get("head_ref")
        base_sha = resolve_ref(root, comparison.get("base_sha") or base_ref, "base_ref")
        head_sha = resolve_ref(root, comparison.get("head_sha") or head_ref, "head_ref")
        strategy = comparison.get("strategy", "merge_base")
        if strategy not in ("merge_base", "direct"):
            raise ValueError("comparison.strategy must be merge_base or direct")
        if strategy == "merge_base":
            compare_sha = git_text(root, "merge-base", base_sha, head_sha).strip()
        else:
            compare_sha = base_sha
        status_data = git_bytes(
            root,
            "diff",
            "--name-status",
            "-z",
            "--find-renames",
            compare_sha,
            head_sha,
        )
        records = parse_name_status(status_data)
        head_label = str(head_ref)

    raw_locked_paths = comparison.get("changed_paths")
    locked_paths: List[str] = []
    if raw_locked_paths is not None:
        if not isinstance(raw_locked_paths, list):
            raise ValueError("comparison.changed_paths must be a list")
        locked_paths = list(dict.fromkeys(safe_repo_path(item) for item in raw_locked_paths))
        available_paths = {
            path
            for record in records
            for path in (record.get("old_file"), record.get("new_file"))
            if path
        }
        missing_paths = [path for path in locked_paths if path not in available_paths]
        if missing_paths:
            raise ValueError(f"locked working tree files changed or disappeared: {', '.join(missing_paths)}")
        locked_set = set(locked_paths)
        records = [
            record for record in records
            if any(path in locked_set for path in (record.get("old_file"), record.get("new_file")) if path)
        ]

    records = [
        record for record in records
        if record_in_scope(record, scopes) and not record_excluded(record, excludes)
    ]
    if not records:
        raise ValueError("comparison contains no changed files")

    if mode == "working_tree":
        expected_snapshot = str(comparison.get("snapshot_fingerprint") or "").strip()
        if expected_snapshot:
            snapshot_paths = locked_paths or [
                str(record.get("new_file") or record.get("old_file")) for record in records
            ]
            snapshot_diff = git_bytes(
                root,
                "diff",
                "--binary",
                "--find-renames",
                base_sha,
                "--",
                *snapshot_paths,
            )
            actual_snapshot = hashlib.sha256(base_sha.encode() + b"\0" + snapshot_diff).hexdigest()
            if actual_snapshot != expected_snapshot:
                raise ValueError("local working tree changed after this analysis snapshot was locked")

    normalized: List[Dict[str, Any]] = []
    fingerprint = hashlib.sha256()
    fingerprint.update(f"{mode}\0{base_sha}\0{head_sha}\0{compare_sha}".encode())
    for record in records:
        old_file = record.get("old_file")
        new_file = record.get("new_file")
        paths = [path for path in (old_file, new_file) if path]
        if mode == "working_tree":
            old_blob = git_blob(root, compare_sha, old_file)
            new_blob = worktree_blob(root, new_file)
            proc = run_git(
                root,
                "diff",
                "--no-ext-diff",
                "--find-renames",
                "--unified=4",
                compare_sha,
                "--",
                *paths,
            )
            if proc.returncode != 0:
                raise ValueError(f"git diff failed for {' -> '.join(paths)}: {proc.stderr.strip()}")
            diff = proc.stdout
        else:
            old_blob = git_blob(root, compare_sha, old_file)
            new_blob = git_blob(root, head_sha, new_file)
            proc = run_git(
                root,
                "diff",
                "--no-ext-diff",
                "--find-renames",
                "--unified=4",
                compare_sha,
                head_sha,
                "--",
                *paths,
            )
            if proc.returncode != 0:
                raise ValueError(f"git diff failed for {' -> '.join(paths)}: {proc.stderr.strip()}")
            diff = proc.stdout
        if not diff.strip():
            raise ValueError(f"changed file has no readable diff: {' -> '.join(paths)}")
        old_source = decode_source(old_blob)
        new_source = decode_source(new_blob)
        binary = is_binary(old_blob) or is_binary(new_blob) or "Binary files " in diff
        fingerprint.update(diff.encode("utf-8", errors="replace"))
        normalized.append(
            {
                **record,
                "old_source": old_source,
                "new_source": new_source,
                "binary": binary,
                "diff": diff,
                "diff_lines": [] if binary else parse_diff_lines(diff),
                "was_dirty": any(
                    path_matches(path, str(dirty))
                    for path in paths
                    for dirty in dirty_paths
                ),
                "dirty_accepted": any(
                    path_matches(path, str(accepted))
                    for path in paths
                    for accepted in accepted_dirty
                ),
            }
        )

    return (
        {
            "mode": mode,
            "strategy": strategy,
            "base_ref": str(base_ref),
            "head_ref": head_label,
            "base_sha": base_sha,
            "head_sha": head_sha,
            "compare_sha": compare_sha,
            "scope_paths": scopes,
            "changed_paths": locked_paths or [
                str(record.get("new_file") or record.get("old_file")) for record in records
            ],
            "snapshot_fingerprint": str(comparison.get("snapshot_fingerprint") or ""),
            "fingerprint": fingerprint.hexdigest(),
        },
        normalized,
    )


def parse_diff_lines(text: str) -> List[Dict[str, Any]]:
    parsed: List[Dict[str, Any]] = []
    old_line = 0
    new_line = 0
    in_hunk = False
    for raw in text.splitlines():
        hunk = re.match(r"^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@", raw)
        if hunk:
            old_line = int(hunk.group(1))
            new_line = int(hunk.group(2))
            in_hunk = True
            parsed.append({"kind": "meta", "old_line": None, "new_line": None, "code": hunk.group(0)})
            continue
        if not in_hunk or raw.startswith(("diff ", "index ", "---", "+++", "new file", "deleted file", "similarity ", "rename ")):
            parsed.append({"kind": "meta", "old_line": None, "new_line": None, "code": raw})
            continue
        if raw.startswith("+"):
            parsed.append({"kind": "add", "old_line": None, "new_line": new_line, "code": raw[1:]})
            new_line += 1
        elif raw.startswith("-"):
            parsed.append({"kind": "del", "old_line": old_line, "new_line": None, "code": raw[1:]})
            old_line += 1
        elif raw.startswith(" "):
            parsed.append({"kind": "ctx", "old_line": old_line, "new_line": new_line, "code": raw[1:]})
            old_line += 1
            new_line += 1
        else:
            parsed.append({"kind": "meta", "old_line": None, "new_line": None, "code": raw})
    return parsed


def line_in_range(line: Optional[int], value: Optional[Tuple[int, int]]) -> bool:
    return line is not None and value is not None and value[0] <= line <= value[1]


def optional_text(data: Dict[str, Any], key: str, fallback: str) -> str:
    value = data.get(key)
    if isinstance(value, str) and value.strip():
        return value.strip()
    return fallback


def advisory_range(raw: Any) -> Optional[Tuple[int, int]]:
    """Read a model range as a matching hint, never as trusted structure."""
    if (
        isinstance(raw, list)
        and len(raw) == 2
        and all(isinstance(item, int) and not isinstance(item, bool) for item in raw)
        and raw[0] >= 1
        and raw[1] >= raw[0]
    ):
        return raw[0], raw[1]
    return None


def diff_hunk_ranges(diff_lines: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    """Derive non-overlapping review ranges from real Git hunks."""
    hunks: List[List[Dict[str, Any]]] = []
    current: Optional[List[Dict[str, Any]]] = None
    for item in diff_lines:
        if item["kind"] == "meta" and str(item.get("code", "")).startswith("@@"):
            if current is not None:
                hunks.append(current)
            current = []
            continue
        if item["kind"] in ("add", "del"):
            if current is None:
                current = []
            current.append(item)
    if current is not None:
        hunks.append(current)

    result = []
    for rows in hunks:
        old_lines = [item["old_line"] for item in rows if item["kind"] == "del" and item.get("old_line")]
        new_lines = [item["new_line"] for item in rows if item["kind"] == "add" and item.get("new_line")]
        if not old_lines and not new_lines:
            continue
        result.append(
            {
                "old_range": (min(old_lines), max(old_lines)) if old_lines else None,
                "new_range": (min(new_lines), max(new_lines)) if new_lines else None,
                "changed_keys": {
                    ("old", item["old_line"])
                    for item in rows
                    if item["kind"] == "del" and item.get("old_line") and item["code"].strip()
                }.union(
                    {
                        ("new", item["new_line"])
                        for item in rows
                        if item["kind"] == "add" and item.get("new_line") and item["code"].strip()
                    }
                ),
            }
        )
    return result


def candidate_overlap(candidate: Dict[str, Any], hunk: Dict[str, Any]) -> int:
    old_range = advisory_range(candidate.get("old_range"))
    new_range = advisory_range(candidate.get("new_range"))
    return sum(
        1
        for side, line in hunk["changed_keys"]
        if line_in_range(line, old_range if side == "old" else new_range)
    )


def has_advisory_range(candidate: Dict[str, Any]) -> bool:
    return advisory_range(candidate.get("old_range")) is not None or advisory_range(candidate.get("new_range")) is not None


def fallback_unit_id(path: str, hunk_index: int, used: Dict[str, Dict[str, Any]]) -> str:
    path_digest = hashlib.sha1(path.encode("utf-8", errors="replace")).hexdigest()[:10]
    base = f"git-change.{path_digest}.{hunk_index + 1}"
    candidate = base
    suffix = 2
    while candidate in used:
        candidate = f"{base}.{suffix}"
        suffix += 1
    return candidate


def display_path(change: Dict[str, Any]) -> str:
    old_file = change.get("old_file")
    new_file = change.get("new_file")
    if old_file and new_file and old_file != new_file:
        return f"{old_file} -> {new_file}"
    return str(new_file or old_file)


def change_key(old_file: Optional[str], new_file: Optional[str]) -> Tuple[str, str]:
    return old_file or "", new_file or ""


def normalize_change_declaration(raw: Any) -> Dict[str, Any]:
    if not isinstance(raw, dict):
        raise ValueError("each walkthrough change must be an object")
    shorthand = raw.get("file")
    old_file = raw.get("old_file", shorthand)
    new_file = raw.get("new_file", shorthand)
    old_file = safe_repo_path(old_file, allow_none=True)
    new_file = safe_repo_path(new_file, allow_none=True)
    if old_file is None and new_file is None:
        raise ValueError("each change requires old_file or new_file")
    return {**raw, "old_file": old_file, "new_file": new_file}


def validate_detail_items(
    data: Dict[str, Any],
    key: str,
    unit_ids: Dict[str, Dict[str, Any]],
) -> List[Dict[str, Any]]:
    items = require_list(data, key, allow_empty=True)
    normalized = []
    for index, item in enumerate(items):
        if not isinstance(item, dict):
            raise ValueError(f"walkthrough.{key}[{index}] must be an object")
        targets = item.get("code_targets", [])
        if not isinstance(targets, list) or not all(isinstance(target, str) for target in targets):
            raise ValueError(f"walkthrough.{key}[{index}].code_targets must be a list")
        missing = sorted(set(targets) - set(unit_ids))
        if missing:
            raise ValueError(f"walkthrough.{key}[{index}] references unknown units: {missing}")
        normalized.append(item)
    return normalized


LOG_CALL = re.compile(
    r"\b(?:slog|[\w.]*log(?:ger)?|zap|ctxlog)[^;\n]*?\."
    r"(Debugf?|Infof?|Warnf?|Warningf?|Errorf?|Fatalf?)\s*\(\s*(?:[\"']([^\"']+)[\"']|([A-Za-z_]\w*))",
    re.I,
)
SLOG_LOG_CALL = re.compile(r"\bslog\.Log\s*\([^,]+,\s*([^,]+),\s*[\"']([^\"']+)[\"']", re.I)
DATABASE_CODE = re.compile(
    r"\b(?:CREATE\s+(?:UNIQUE\s+)?(?:TABLE|INDEX)|ALTER\s+TABLE|DROP\s+(?:TABLE|INDEX)|"
    r"INSERT\s+INTO|UPDATE\s+[\w`\".]+|DELETE\s+FROM|SELECT\b.*\bFROM|\b(?:Raw|Exec|Query|Migrate|AutoMigrate)\s*\(|"
    r"\.(?:Create|Save|Updates?|Delete|First|Find|Where|Scan|Model|Table)\s*\(|"
    r"(?:gorm|sqlx|database/sql|bson|db:)\b)",
    re.I,
)
PERSISTENCE_PATH = re.compile(r"(?:^|/)(?:migrations?|schema|models?|entity|dao|dal|repository|store)(?:/|$)|\.sql$", re.I)
PERSISTENCE_DECLARATION = re.compile(
    r"(?:(?i:\bgorm\b|\bdb\b|\bcolumn\b|\bindex\b|\bprimary\b|\btablename\b|type\s+\w+\s+struct)|^\s*[A-Z]\w+\s+[\w*\[\].]+)"
)


def resolve_string_constant(name: str, source: str) -> Optional[str]:
    match = re.search(rf"\b{re.escape(name)}\s*(?::=|=)\s*[\"']([^\"']+)[\"']", source)
    return match.group(1) if match else None


def executable_position(code: str, position: int) -> bool:
    """Reject logger-looking text that occurs inside a string or after a comment marker."""
    quote = None
    escaped = False
    index = 0
    while index < position:
        char = code[index]
        if quote:
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == quote:
                quote = None
            index += 1
            continue
        if char in ('"', "'", "`"):
            quote = char
            index += 1
            continue
        if code.startswith(("//", "/*", "<!--", "--"), index) or (
            char == "#" and (index == 0 or code[index - 1].isspace())
        ):
            return False
        index += 1
    return quote is None


def parse_log_call(code: str, source: str) -> Optional[Tuple[str, str]]:
    match = LOG_CALL.search(code)
    if match and executable_position(code, match.start()):
        raw_level = match.group(1).upper().removesuffix("F").replace("WARNING", "WARN")
        event = match.group(2) or resolve_string_constant(match.group(3), source)
        return (raw_level, event) if event else None
    match = SLOG_LOG_CALL.search(code)
    if match and executable_position(code, match.start()):
        level = match.group(1).split(".")[-1].upper().removeprefix("LEVEL")
        return level, match.group(2)
    return None


def log_candidate(diff_lines: List[Dict[str, Any]], index: int) -> str:
    current = diff_lines[index]
    code = current["code"]
    if not code.lstrip().startswith("."):
        return code
    side = "new_line" if current["kind"] == "add" else "old_line"
    expected = current.get(side)
    parts = [code]
    for previous in reversed(diff_lines[max(0, index - 3):index]):
        previous_line = previous.get(side)
        if expected is None or previous_line != expected - 1:
            break
        parts.insert(0, previous["code"])
        expected = previous_line
        if re.search(r"\b(?:slog|[\w.]*log(?:ger)?|zap|ctxlog)\b", previous["code"], re.I):
            break
    return " ".join(parts)


def change_kind(sides: set) -> str:
    if sides == {"add"}:
        return "新增"
    if sides == {"del"}:
        return "删除"
    return "修改"


def equivalent_sql(path: str, added: str, deleted: str, kind: str, source_context: str) -> str:
    source = added or deleted
    if path.lower().endswith(".sql"):
        return source
    quoted_sql = re.search(
        r"(?:Raw|Exec|Query)\s*\(\s*[\"'`]((?:SELECT|INSERT|UPDATE|DELETE|CREATE|ALTER|DROP)\b.+?)[\"'`]",
        source,
        re.I,
    )
    if quoted_sql:
        return quoted_sql.group(1)
    variable_call = re.search(r"(?:Raw|Exec|Query)\s*\(\s*([A-Za-z_]\w*)", source)
    if variable_call:
        assignment = re.search(
            rf"\b{re.escape(variable_call.group(1))}\s*(?::=|=)\s*`([^`]+)`",
            source_context,
            re.S,
        )
        if assignment:
            return assignment.group(1).strip()
    table_match = re.search(r"\.Table\s*\(\s*[\"']([^\"']+)[\"']", source)
    table = table_match.group(1) if table_match else "<table_from_model>"
    if re.search(r"\.Create\s*\(", source):
        statement = f"INSERT INTO {table} (<columns>) VALUES (<runtime_values>);"
    elif re.search(r"\.(?:Save|Updates?)\s*\(", source):
        statement = f"UPDATE {table} SET <changed_columns> = <runtime_values> WHERE <code_condition>;"
    elif re.search(r"\.Delete\s*\(", source):
        statement = f"DELETE FROM {table} WHERE <code_condition>;"
    elif re.search(r"\.(?:Find|First|Scan|Where)\s*\(", source):
        statement = f"SELECT * FROM {table} WHERE <code_condition>;"
    else:
        statement = f"ALTER TABLE {table} {('ADD OR MODIFY' if kind != '删除' else 'DROP')} <column_or_index_from_model_diff>;"
    return f"-- 参数化等价 SQL；占位符来自运行时变量，来源：{path}\n{statement}"


def unique_strings(values: List[str]) -> List[str]:
    result = []
    seen = set()
    for value in values:
        normalized = value.strip().strip('`"[]')
        if normalized and normalized.lower() not in seen:
            seen.add(normalized.lower())
            result.append(normalized)
    return result


def database_structure_summary(path: str, added: str, deleted: str) -> Dict[str, Any]:
    """Return table/column facts that are useful in the storage side panel."""
    source = added or deleted
    tables = []
    identifier = r"[`\"\[]?([A-Za-z_][\w$]*(?:\.[A-Za-z_][\w$]*)?)[`\"\]]?"
    for pattern in (
        rf"\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?{identifier}",
        rf"\bALTER\s+TABLE\s+{identifier}",
        rf"\bINSERT\s+INTO\s+{identifier}",
        rf"\bUPDATE\s+{identifier}",
        rf"\bDELETE\s+FROM\s+{identifier}",
        rf"\bFROM\s+{identifier}",
        r"\.Table\s*\(\s*[\"']([^\"']+)[\"']",
    ):
        tables.extend(re.findall(pattern, source, re.I))

    fields = []
    fields.extend(
        re.findall(
            rf"\bALTER\s+TABLE\s+{identifier}\s+ADD\s+(?:COLUMN\s+)?{identifier}",
            added,
            re.I,
        )
    )
    # ALTER TABLE has two captures; only the second one is the added column.
    fields = [value[-1] if isinstance(value, tuple) else value for value in fields]
    for match in re.finditer(rf"\bINSERT\s+INTO\s+{identifier}\s*\(([^)]+)\)", added, re.I | re.S):
        fields.extend(part.strip() for part in match.group(2).split(','))
    for match in re.finditer(
        rf"\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?{identifier}\s*\((.*)\)\s*;?",
        added,
        re.I | re.S,
    ):
        body = match.group(2)
        for definition in re.split(r",|\n", body):
            field = re.match(r"\s*[`\"\[]?([A-Za-z_]\w*)", definition)
            if field and field.group(1).upper() not in {"PRIMARY", "UNIQUE", "KEY", "INDEX", "CONSTRAINT", "FOREIGN", "CHECK"}:
                fields.append(field.group(1))
    fields.extend(re.findall(r'(?:gorm:\"[^\"]*?column:|db:\")([A-Za-z_]\w*)', added, re.I))
    if PERSISTENCE_PATH.search(path):
        for line in added.splitlines():
            if re.search(r'(?:column:|db:\")', line, re.I):
                continue
            declaration = re.match(r"\s*([A-Z][A-Za-z0-9_]*)\s+[\w*\[\].]+(?:\s+`[^`]+`)?\s*$", line)
            if declaration and not re.match(r"\s*type\s+", line):
                fields.append(declaration.group(1))

    tables = unique_strings(tables)
    fields = unique_strings(fields)
    summary: Dict[str, Any] = {}
    if tables:
        summary["表"] = tables
    if fields:
        summary["字段"] = fields
    if re.search(r"\bCREATE\s+TABLE\b", added, re.I):
        summary["类型"] = "新建表"
    elif fields and added:
        summary["类型"] = "新增字段"
    elif re.search(r"\b(?:CREATE|DROP)\s+(?:UNIQUE\s+)?INDEX\b", source, re.I):
        summary["类型"] = "索引变更"
    return summary


def code_backed_impacts(changes: List[Dict[str, Any]]) -> Tuple[List[Dict[str, Any]], List[Dict[str, Any]], set, Dict[str, set]]:
    """Extract database and log facts only from changed source lines and their units."""
    database: Dict[Tuple[str, str], Dict[str, Any]] = {}
    logs: Dict[Tuple[str, str], Dict[str, Any]] = {}
    database_targets = set()
    log_targets: Dict[str, set] = {}
    for change in changes:
        path = change.get("new_file") or change.get("old_file") or ""
        for line_index, line in enumerate(change["diff_lines"]):
            if line["kind"] not in ("add", "del"):
                continue
            unit_id = unit_for_line(change["units"], line)
            if not unit_id:
                continue
            is_sql_file = path.lower().endswith(".sql")
            if DATABASE_CODE.search(line["code"]) or (PERSISTENCE_PATH.search(path) and (is_sql_file or PERSISTENCE_DECLARATION.search(line["code"]))):
                key = (path, unit_id)
                item = database.setdefault(
                    key,
                    {
                        "对象": path,
                        "类型": "数据库访问或结构变更",
                        "变更": set(),
                        "_sql_add": [],
                        "_sql_del": [],
                        "_source_context": change["new_source"] or change["old_source"],
                        "code_targets": [unit_id],
                    },
                )
                item["变更"].add(line["kind"])
                item[f"_sql_{line['kind']}"].append(line["code"].strip())
                database_targets.add(unit_id)
            log_call = parse_log_call(log_candidate(change["diff_lines"], line_index), change["new_source"] or change["old_source"])
            if log_call:
                level, event = log_call
                key = (event, unit_id)
                item = logs.setdefault(key, {"事件": event, "级别": set(), "变更": set(), "代码行": line.get("new_line") or line.get("old_line"), "代码侧": "new" if line["kind"] == "add" else "old", "触发条件": "日志调用所在的变更代码分支。", "字段": "请跳转代码区块查看实际传入字段。", "用途": "由实际代码差异识别。", "code_targets": [unit_id]})
                if line["kind"] == "add":
                    item["代码行"] = line.get("new_line")
                    item["代码侧"] = "new"
                item["级别"].add(level)
                item["变更"].add(line["kind"])
                log_targets.setdefault(event, set()).add(unit_id)
    for item in database.values():
        item["变更"] = change_kind(item["变更"])
        added_sql = "\n".join(value for value in item.pop("_sql_add") if value)
        deleted_sql = "\n".join(value for value in item.pop("_sql_del") if value)
        source_context = str(item.pop("_source_context"))
        item["SQL"] = equivalent_sql(str(item["对象"]), added_sql, deleted_sql, str(item["变更"]), source_context)
        item.update(database_structure_summary(str(item["对象"]), added_sql, deleted_sql))
    for item in logs.values():
        item["级别"] = " / ".join(sorted(item["级别"]))
        item["变更"] = change_kind(item["变更"])
    return list(database.values()), list(logs.values()), database_targets, log_targets


def apply_code_backed_impacts(data: Dict[str, Any], changes: List[Dict[str, Any]]) -> None:
    database, logs, database_targets, log_targets = code_backed_impacts(changes)
    # Generated entries are the complete source of truth. Manual entries only enrich
    # matching evidence. Unsupported model declarations are ignored so a hallucinated
    # side-panel item cannot fail an otherwise valid code walkthrough.
    manual_database: Dict[str, Dict[str, Any]] = {}
    for item in data.get("database_changes", []):
        if not isinstance(item, dict):
            continue
        targets = item.get("code_targets", [])
        if not isinstance(targets, list):
            continue
        for target in {value for value in targets if isinstance(value, str)}.intersection(database_targets):
            manual_database[target] = item
    manual_logs: Dict[Tuple[str, str], Dict[str, Any]] = {}
    for item in data.get("log_points", []):
        if not isinstance(item, dict):
            continue
        event = str(item.get("事件", ""))
        targets = item.get("code_targets", [])
        if not isinstance(targets, list):
            continue
        for target in {value for value in targets if isinstance(value, str)}.intersection(log_targets.get(event, set())):
            manual_logs[(event, target)] = item
    for item in database:
        targets = item["code_targets"]
        manual = next((manual_database[target] for target in targets if target in manual_database), {})
        protected = {"对象", "表", "字段", "类型", "变更", "SQL", "code_targets"}
        item.update({key: value for key, value in manual.items() if key not in protected})
        item["code_targets"] = targets
    for item in logs:
        targets = item["code_targets"]
        manual = next((manual_logs[(item["事件"], target)] for target in targets if (item["事件"], target) in manual_logs), {})
        protected = {"事件", "级别", "变更", "代码行", "代码侧", "code_targets"}
        item.update({key: value for key, value in manual.items() if key not in protected})
        item["code_targets"] = targets
    data["database_changes"] = database
    data["log_points"] = logs


def validate_walkthrough(
    root: Path,
    data: Dict[str, Any],
    comparison: Dict[str, Any],
    discovered: List[Dict[str, Any]],
) -> Tuple[List[Dict[str, Any]], Dict[str, Dict[str, Any]]]:
    if data.get("version") != 2:
        raise ValueError("walkthrough version must be 2")

    for value in iter_strings(data):
        if contains_possible_credential(value):
            raise ValueError("walkthrough contains a possible credential or secret")
    for change in discovered:
        path = display_path(change)
        rendered_diff = "\n".join(str(line.get("code", "")) for line in change["diff_lines"])
        if contains_possible_credential(rendered_diff, path):
            raise ValueError(f"git diff contains a possible credential: {display_path(change)}")
        if (
            comparison["mode"] == "working_tree"
            and change["was_dirty"]
            and change["dirty_accepted"]
            and contains_possible_credential(change["new_source"], path)
        ):
            raise ValueError(f"source contains a possible credential: {display_path(change)}")

    data["title"] = optional_text(data, "title", "代码变更分析")
    data["summary"] = optional_text(
        data,
        "summary",
        f"基于锁定的 Git 比较识别出 {len(discovered)} 个变更文件。",
    )
    flows = data.get("flows") if isinstance(data.get("flows"), list) else []
    declared = []
    raw_changes = data.get("changes") if isinstance(data.get("changes"), list) else []
    for item in raw_changes:
        try:
            declared.append(normalize_change_declaration(item))
        except ValueError:
            # File discovery is owned by Git. A malformed model declaration has
            # no authority over the comparison and is safe to ignore.
            continue

    discovered_by_key = {
        change_key(item.get("old_file"), item.get("new_file")): item for item in discovered
    }
    declared_by_key: Dict[Tuple[str, str], Dict[str, Any]] = {}
    declared_by_path: Dict[str, Dict[str, Any]] = {}
    for item in declared:
        key = change_key(item["old_file"], item["new_file"])
        declared_by_key.setdefault(key, item)
        for path in (item.get("old_file"), item.get("new_file")):
            if path:
                declared_by_path.setdefault(path, item)

    unit_ids: Dict[str, Dict[str, Any]] = {}
    model_unit_ids: Dict[str, str] = {}
    normalized_changes = []
    for file_index, (key, git_change) in enumerate(discovered_by_key.items()):
        raw = declared_by_key.get(key)
        if raw is None:
            raw = next(
                (
                    declared_by_path[path]
                    for path in (git_change.get("new_file"), git_change.get("old_file"))
                    if path in declared_by_path
                ),
                {},
            )
        path = display_path(git_change)
        status = STATUS_LABELS.get(git_change["status"], git_change["status"])
        purpose = optional_text(raw, "purpose", f"{status} {path}。")
        implementation = optional_text(raw, "implementation", "变更范围由锁定的 Git 差异确定。")
        attribution = "diff"
        if git_change["was_dirty"] and comparison["mode"] == "working_tree":
            if not git_change["dirty_accepted"]:
                raise ValueError(
                    f"dirty baseline file must be accepted: {path}"
                )
            attribution = "final_only"

        raw_units = [item for item in raw.get("units", []) if isinstance(item, dict)] if isinstance(raw, dict) else []
        hunks = [] if git_change["binary"] else diff_hunk_ranges(git_change["diff_lines"])
        if not hunks:
            hunks = [{"old_range": None, "new_range": None, "changed_keys": set()}]
        if attribution == "final_only":
            for hunk in hunks:
                hunk["old_range"] = None

        assignments: List[Optional[int]] = [None] * len(hunks)
        used_candidates = set()
        for hunk_index, hunk in enumerate(hunks):
            scored = sorted(
                (
                    (candidate_overlap(candidate, hunk), candidate_index)
                    for candidate_index, candidate in enumerate(raw_units)
                    if candidate_index not in used_candidates
                ),
                key=lambda item: (-item[0], item[1]),
            )
            if scored and scored[0][0] > 0:
                assignments[hunk_index] = scored[0][1]
                used_candidates.add(scored[0][1])
        unassigned_candidates = [
            index
            for index, candidate in enumerate(raw_units)
            if index not in used_candidates and not has_advisory_range(candidate)
        ]
        for hunk_index, assignment in enumerate(assignments):
            if assignment is None and unassigned_candidates:
                candidate_index = unassigned_candidates.pop(0)
                assignments[hunk_index] = candidate_index
                used_candidates.add(candidate_index)

        units = []
        for hunk_index, hunk in enumerate(hunks):
            candidate_index = assignments[hunk_index]
            raw_unit = raw_units[candidate_index] if candidate_index is not None else {}
            proposed_id = raw_unit.get("id")
            if isinstance(proposed_id, str):
                proposed_id = proposed_id.strip()
            unit_id = fallback_unit_id(path, hunk_index, unit_ids)
            if isinstance(proposed_id, str) and proposed_id:
                model_unit_ids.setdefault(proposed_id, unit_id)

            kind = raw_unit.get("kind")
            if git_change["binary"] or (hunk["old_range"] is None and hunk["new_range"] is None):
                kind = "file"
            elif kind not in UNIT_KINDS:
                kind = "sql" if path.lower().endswith(".sql") else "block"
            symbol = optional_text(raw_unit, "symbol", f"{PurePosixPath(path).name} 差异区块 {hunk_index + 1}")
            title = optional_text(raw_unit, "title", f"{status}区块 {hunk_index + 1}")
            meaning = optional_text(raw_unit, "meaning", implementation)
            reason = optional_text(raw_unit, "reason", purpose)
            impact = optional_text(raw_unit, "impact", f"影响范围以 {path} 的该 Git 差异区块为准。")
            unit = {
                **raw_unit,
                "id": unit_id,
                "kind": kind,
                "symbol": symbol,
                "title": title,
                "meaning": meaning,
                "reason": reason,
                "impact": impact,
                "old_range": hunk["old_range"],
                "new_range": hunk["new_range"],
                "file_index": file_index,
                "display_file": path,
            }
            units.append(unit)
            unit_ids[unit_id] = unit

        normalized_changes.append(
            {
                **git_change,
                "purpose": purpose,
                "implementation": implementation,
                "attribution": attribution,
                "final_only_reason": optional_text(
                    raw,
                    "final_only_reason",
                    "基线已包含未归属修改，因此仅展示最终源码。" if attribution == "final_only" else "",
                ),
                "units": units,
            }
        )

    normalized_flows = []
    for flow_index, flow in enumerate(flows):
        if not isinstance(flow, dict):
            continue
        title = optional_text(flow, "title", f"变更链路 {flow_index + 1}")
        description = optional_text(flow, "description", "由模型分析的变更链路。")
        steps = flow.get("steps")
        if not isinstance(steps, list):
            continue
        normalized_steps = []
        for step in steps:
            if not isinstance(step, dict):
                continue
            target = step.get("unit_id")
            target = model_unit_ids.get(target, target)
            if target not in unit_ids:
                continue
            normalized_steps.append(
                {
                    **step,
                    "label": optional_text(step, "label", unit_ids[target]["title"]),
                    "explanation": optional_text(step, "explanation", unit_ids[target]["meaning"]),
                    "unit_id": target,
                }
            )
        if normalized_steps:
            normalized_flows.append({**flow, "title": title, "description": description, "steps": normalized_steps})
    data["flows"] = normalized_flows
    for key in ("database_changes", "config_changes", "api_changes", "log_points"):
        retained = []
        raw_items = data.get(key) if isinstance(data.get(key), list) else []
        for item in raw_items:
            if not isinstance(item, dict):
                continue
            targets = item.get("code_targets")
            if not isinstance(targets, list):
                targets = []
            filtered = []
            for target in targets:
                target = model_unit_ids.get(target, target)
                if target in unit_ids and target not in filtered:
                    filtered.append(target)
            if targets and not filtered:
                continue
            retained.append({**item, "code_targets": filtered})
        data[key] = retained
    apply_code_backed_impacts(data, normalized_changes)
    for key in ("database_changes", "config_changes", "api_changes", "log_points"):
        data[key] = validate_detail_items(data, key, unit_ids)
    return normalized_changes, unit_ids


def range_label(value: Optional[Tuple[int, int]]) -> str:
    if value is None:
        return "-"
    return str(value[0]) if value[0] == value[1] else f"{value[0]}-{value[1]}"


def unit_for_line(units: List[Dict[str, Any]], item: Dict[str, Any]) -> Optional[str]:
    candidates = []
    for unit in units:
        if line_in_range(item.get("old_line"), unit["old_range"]) or line_in_range(item.get("new_line"), unit["new_range"]):
            candidates.append(unit["id"])
    return candidates[0] if len(candidates) == 1 else None


def unit_note_anchors(units: List[Dict[str, Any]], rows: List[Dict[str, Any]]) -> Dict[str, int]:
    row_units = [unit_for_line(units, item) for item in rows]
    anchors: Dict[str, int] = {}
    for preferred_kind in ("add", "del", None):
        for index, (item, unit_id) in enumerate(zip(rows, row_units)):
            if not unit_id or unit_id in anchors:
                continue
            if preferred_kind is not None and item["kind"] != preferred_kind:
                continue
            anchors[unit_id] = index
    return anchors


def render_ai_note(unit: Dict[str, Any]) -> str:
    unit_id = html.escape(unit["id"])
    return (
        f'<div class="ai-note-row" data-ai-note-for="{unit_id}">'
        '<span class="ai-note-line" aria-hidden="true"></span>'
        f'<button type="button" class="ai-note" data-unit-target="{unit_id}">'
        f'<span class="ai-badge">AI</span><strong>本次变更说明</strong>'
        f'<span class="ai-note-text">{html.escape(unit["meaning"])}</span></button></div>'
    )


def final_source_rows(source: str, units: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    lines = source.splitlines()
    selected = set()
    for unit in units:
        value = unit["new_range"]
        if value:
            selected.update(range(max(1, value[0] - 3), min(len(lines), value[1] + 3) + 1))
    parsed = []
    previous = 0
    for number in sorted(selected):
        if previous and number > previous + 1:
            parsed.append({"kind": "meta", "old_line": None, "new_line": None, "code": "..."})
        parsed.append({"kind": "ctx", "old_line": None, "new_line": number, "code": lines[number - 1]})
        previous = number
    return parsed


def render_tree(changes: List[Dict[str, Any]]) -> str:
    root: Dict[str, Any] = {"dirs": {}, "files": []}
    for index, change in enumerate(changes):
        path = change.get("new_file") or change.get("old_file")
        parts = PurePosixPath(str(path)).parts
        node = root
        for part in parts[:-1]:
            node = node["dirs"].setdefault(part, {"dirs": {}, "files": []})
        node["files"].append((parts[-1], index, change))

    def walk(node: Dict[str, Any], depth: int = 0) -> str:
        output = []
        for name, child in sorted(node["dirs"].items()):
            output.append(
                f'<details class="tree-folder" open><summary><svg><use href="#icon-chevron"></use></svg>'
                f'<svg class="folder-icon"><use href="#icon-folder"></use></svg><span>{html.escape(name)}</span></summary>'
                f'<div class="tree-children">{walk(child, depth + 1)}</div></details>'
            )
        for name, index, change in sorted(node["files"], key=lambda item: item[0]):
            output.append(file_button(index, change, label=name, css="tree-file"))
        return "".join(output)

    return walk(root)


def file_button(index: int, change: Dict[str, Any], label: Optional[str] = None, css: str = "") -> str:
    path = display_path(change)
    label = label or path
    status = STATUS_LABELS.get(change["status"], change["status"])
    suffix = PurePosixPath(path).suffix.lower()
    icon_name = "database" if suffix == ".sql" else "file-code" if suffix in (".go", ".py", ".js", ".ts", ".tsx", ".java") else "file-text"
    return (
        f'<button type="button" data-code-target="code-{index}" class="file-button {css}" title="{html.escape(path)}">'
        f'<svg class="file-icon file-{suffix[1:] or "plain"}"><use href="#icon-{icon_name}"></use></svg>'
        f'<span class="file-label">{html.escape(label)}</span><span class="file-status status-{change["status"].lower()}">{status}</span>'
        f'<small>{len(change["units"])} 个区块</small></button>'
    )


def render_value(value: Any) -> str:
    if isinstance(value, list):
        return "、".join(html.escape(str(item)) for item in value) or "-"
    if isinstance(value, dict):
        return html.escape(json.dumps(value, ensure_ascii=False, sort_keys=True))
    return html.escape(str(value))


def render_detail_cards(items: List[Dict[str, Any]], unit_ids: Dict[str, Dict[str, Any]]) -> str:
    cards = []
    for index, item in enumerate(items):
        targets = item.get("code_targets", [])
        rows = []
        for key, value in item.items():
            if key == "code_targets":
                continue
            rows.append(f"<dt>{html.escape(str(key))}</dt><dd>{render_value(value)}</dd>")
        links = "".join(
            f'<button type="button" class="code-link" data-unit-target="{html.escape(target)}">'
            f'{html.escape(unit_ids[target]["symbol"])}</button>'
            for target in targets
        )
        cards.append(
            f'<article class="detail-card"><header><span>#{index + 1}</span>{links}</header><dl>{"".join(rows)}</dl></article>'
        )
    return "".join(cards)


def first_target(item: Dict[str, Any], unit_ids: Dict[str, Dict[str, Any]]) -> Optional[str]:
    return next((target for target in item.get("code_targets", []) if target in unit_ids), None)


def source_button(item: Dict[str, Any], unit_ids: Dict[str, Dict[str, Any]], label: str = "查看来源代码") -> str:
    target = first_target(item, unit_ids)
    if not target:
        return ""
    return f'<button type="button" class="source-link" data-unit-target="{html.escape(target)}">{html.escape(label)}</button>'


def render_database_panel(data: Dict[str, Any], unit_ids: Dict[str, Dict[str, Any]]) -> str:
    structure = []
    configuration = []
    for item in data["database_changes"]:
        text = " ".join(str(value) for key, value in item.items() if key != "code_targets")
        is_configuration = str(item.get("分类", "")) == "配置表" or bool(
            re.search(r"(?:配置|config|setting|option|parameter)", text, re.I)
        )
        target = configuration if is_configuration else structure
        sql = str(item.get("SQL") or item.get("sql") or "-- 代码涉及数据库操作，具体运行时参数请查看来源代码")
        title = str(item.get("对象") or "数据库变更")
        target.append(
            '<article class="sql-block">'
            f'<header><div><span>{html.escape(str(item.get("变更", "变更")))}</span><h3>{html.escape(title)}</h3></div>'
            f'<div>{source_button(item, unit_ids)}<button type="button" class="icon-button copy-sql" data-copy-text="{html.escape(sql, quote=True)}" title="复制 SQL"><svg><use href="#icon-copy"></use></svg></button></div></header>'
            f'<pre><code>{html.escape(sql)}</code></pre></article>'
        )
    empty = '<div class="empty-state">本次代码差异未发现此类数据库变更</div>'
    return (
        '<section class="side-panel database-panel" data-side-panel="storage">'
        '<header class="side-panel-heading"><div><span>DATABASE</span><h2>数据库变更 SQL</h2></div>'
        '<button type="button" class="icon-button" id="copy-all-sql" title="复制全部 SQL"><svg><use href="#icon-copy"></use></svg></button></header>'
        f'<div class="sql-section"><h3>表结构与数据变动 <span>{len(structure)}</span></h3>{"".join(structure) or empty}</div>'
        f'<div class="sql-section"><h3>数据库配置表变动 <span>{len(configuration)}</span></h3>{"".join(configuration) or empty}</div></section>'
    )


def log_scenario(item: Dict[str, Any]) -> str:
    trigger = str(item.get("触发条件") or "日志调用所在的代码分支")
    purpose = str(item.get("用途") or "用于确认该代码路径是否执行")
    change = str(item.get("变更") or "变更")
    level = str(item.get("级别") or "LOG")
    return f"{trigger}；{purpose}。本次为{change}，级别 {level}。"


def render_logs_panel(data: Dict[str, Any], unit_ids: Dict[str, Dict[str, Any]]) -> str:
    rows = []
    for item in data["log_points"]:
        keyword = str(item.get("事件") or "")
        target = first_target(item, unit_ids) or ""
        source_line = str(item.get("代码行") or "")
        source_side = str(item.get("代码侧") or "new")
        rows.append(
            '<tr>'
            f'<td><div class="keyword-cell"><button type="button" class="log-keyword" data-log-unit="{html.escape(target)}" data-log-line="{html.escape(source_line)}" data-log-side="{html.escape(source_side)}">{html.escape(keyword)}</button>'
            f'<button type="button" class="icon-button copy-keyword" data-copy-text="{html.escape(keyword, quote=True)}" title="复制搜索关键词"><svg><use href="#icon-copy"></use></svg></button></div></td>'
            f'<td>{html.escape(log_scenario(item))}</td></tr>'
        )
    empty = '<tr><td colspan="2" class="empty-state">本次代码差异未发现日志调用变更</td></tr>'
    return (
        '<section class="side-panel logs-panel" data-side-panel="logs" hidden>'
        '<header class="side-panel-heading"><div><span>LOGS</span><h2>关键日志定位</h2></div>'
        '<button type="button" class="icon-button" id="copy-all-keywords" title="复制全部关键词"><svg><use href="#icon-copy"></use></svg></button></header>'
        '<div class="log-toolbar"><svg><use href="#icon-search"></use></svg><input id="log-filter" type="search" placeholder="筛选关键词或场景"></div>'
        f'<div class="log-table-wrap"><table class="log-table"><thead><tr><th>日志搜索关键词</th><th>使用场景与主要证实的问题</th></tr></thead><tbody>{"".join(rows) or empty}</tbody></table></div></section>'
    )


def render_impact_panel(data: Dict[str, Any], unit_ids: Dict[str, Dict[str, Any]]) -> str:
    cards = []
    for item in data["api_changes"]:
        url = str(item.get("URL") or item.get("接口") or "未声明 URL")
        fields = []
        for label, keys in (
            ("参数变化", ("参数", "请求参数", "请求变化", "入参")),
            ("响应变化", ("响应", "响应变化", "返回变化")),
            ("错误码", ("错误码", "错误变化", "错误")),
        ):
            value = next((item.get(key) for key in keys if item.get(key) not in (None, "", [])), None)
            if value is not None:
                fields.append(f'<dt>{label}</dt><dd>{render_value(value)}</dd>')
        cards.append(
            '<article class="api-change">'
            f'<header><code>{html.escape(url)}</code>{source_button(item, unit_ids)}</header>'
            f'<dl>{"".join(fields) or "<dt>变化</dt><dd>未提供参数、响应或错误码变化</dd>"}</dl></article>'
        )
    content = "".join(cards) or '<div class="empty-state">本次没有公共接口变化</div>'
    return (
        '<section class="side-panel api-panel" data-side-panel="api" hidden>'
        '<header class="side-panel-heading"><div><span>API CHANGES</span><h2>接口变动</h2></div></header>'
        f'<div class="api-list">{content}</div></section>'
    )


def render_impact_sidebar(data: Dict[str, Any], changes: List[Dict[str, Any]], unit_ids: Dict[str, Dict[str, Any]]) -> str:
    return (
        '<aside class="impact-sidebar"><nav class="impact-tabs">'
        f'<button type="button" data-side-target="storage" aria-selected="true">数据库 <span>{len(data["database_changes"])}</span></button>'
        f'<button type="button" data-side-target="logs">日志 <span>{len(data["log_points"])}</span></button>'
        f'<button type="button" data-side-target="api">接口变动 <span>{len(data["api_changes"])}</span></button></nav>'
        f'<div class="side-scroll">{render_database_panel(data, unit_ids)}{render_logs_panel(data, unit_ids)}{render_impact_panel(data, unit_ids)}</div></aside>'
    )


def render_icon_sprite() -> str:
    return '''<svg class="icon-sprite" aria-hidden="true"><defs>
<symbol id="icon-folder" viewBox="0 0 24 24"><path d="M3 6h6l2 2h10v11H3z"/><path d="M3 6v13"/></symbol>
<symbol id="icon-file-code" viewBox="0 0 24 24"><path d="M6 2h8l4 4v16H6z"/><path d="M14 2v5h5M10 12l-2 2 2 2m4-4 2 2-2 2"/></symbol>
<symbol id="icon-file-text" viewBox="0 0 24 24"><path d="M6 2h8l4 4v16H6z"/><path d="M14 2v5h5M9 12h6M9 16h6"/></symbol>
<symbol id="icon-database" viewBox="0 0 24 24"><ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v6c0 1.7 3.6 3 8 3s8-1.3 8-3V5M4 11v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/></symbol>
<symbol id="icon-chevron" viewBox="0 0 24 24"><path d="m9 18 6-6-6-6"/></symbol><symbol id="icon-search" viewBox="0 0 24 24"><circle cx="11" cy="11" r="7"/><path d="m20 20-4-4"/></symbol>
<symbol id="icon-copy" viewBox="0 0 24 24"><rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V5a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h3"/></symbol>
<symbol id="icon-arrow-left" viewBox="0 0 24 24"><path d="m15 18-6-6 6-6M9 12h11"/></symbol><symbol id="icon-terminal" viewBox="0 0 24 24"><path d="m5 7 4 4-4 4M11 17h8"/></symbol>
<symbol id="icon-alert" viewBox="0 0 24 24"><path d="M12 3 2 21h20zM12 9v5M12 18h.01"/></symbol><symbol id="icon-check" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="m8 12 3 3 5-6"/></symbol>
<symbol id="icon-clock" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7v6l4 2"/></symbol><symbol id="icon-target" viewBox="0 0 24 24"><circle cx="12" cy="12" r="8"/><circle cx="12" cy="12" r="3"/></symbol>
<symbol id="icon-puzzle" viewBox="0 0 24 24"><path d="M8 3h4a3 3 0 1 1 3 3v3h3a3 3 0 1 1 0 6h-3v6H9v-4a3 3 0 1 1-3-3H3V8h5z"/></symbol>
<symbol id="icon-comments" viewBox="0 0 24 24"><path d="M4 4h16v12H8l-4 4z"/></symbol><symbol id="icon-settings" viewBox="0 0 24 24"><circle cx="12" cy="12" r="3"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3M5 5l2 2M17 17l2 2M19 5l-2 2M7 17l-2 2"/></symbol>
</defs></svg>'''


def render_workbench_document(
    data: Dict[str, Any],
    comparison: Dict[str, Any],
    changes: List[Dict[str, Any]],
    unit_ids: Dict[str, Dict[str, Any]],
    title: str,
    summary: str,
    compare_label: str,
    font_css: str,
    code_sections: List[str],
    review_details: List[str],
    feedback_meta: Dict[str, Any],
    additions: int,
    deletions: int,
) -> str:
    return f'''<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; connect-src 'none'; font-src data:; object-src 'none'; base-uri 'none'">
<title>{title} · 代码变更理解</title><style>
{font_css}
:root{{--bg:#0d100f;--panel:#111514;--raised:#171b1a;--raised-2:#1b201e;--line:#2a302d;--line-soft:#202623;--ink:#e4e9e5;--muted:#8c9790;--amber:#f5a30b;--green:#59c985;--red:#ef6058;--blue:#68a5f7;--code:#111614;--code-ink:#cad5ce}}
*{{box-sizing:border-box;scrollbar-width:thin;scrollbar-color:#3b4540 #0d100f}}*::-webkit-scrollbar{{width:10px;height:10px}}*::-webkit-scrollbar-track{{background:#0d100f}}*::-webkit-scrollbar-thumb{{border:2px solid #0d100f;border-radius:6px;background:#3b4540}}*::-webkit-scrollbar-thumb:hover{{background:#56635d}}*::-webkit-scrollbar-corner{{background:#0d100f}}html,body{{margin:0;width:100%;height:100%;overflow:hidden;background:var(--bg);color:var(--ink);font:14px/1.5 "Microsoft YaHei","PingFang SC","Noto Sans CJK SC","Walkthrough CJK",Arial,sans-serif;letter-spacing:0}}button,input,textarea{{font:inherit;letter-spacing:0}}button{{cursor:pointer}}button:disabled{{cursor:not-allowed;opacity:.38}}svg{{width:18px;height:18px;fill:none;stroke:currentColor;stroke-width:1.7;stroke-linecap:round;stroke-linejoin:round}}[hidden]{{display:none!important}}.icon-sprite{{position:absolute;width:0;height:0;overflow:hidden}}.shell{{display:grid;grid-template-rows:70px minmax(0,1fr);height:100vh;max-width:1800px;margin:auto;border-inline:1px solid #202522;background:var(--bg)}}
.topbar{{display:grid;grid-template-columns:318px minmax(0,1fr) 420px;border-bottom:1px solid var(--line);background:#0f1211}}.product{{display:flex;align-items:center;gap:10px;padding:0 18px;border-right:1px solid var(--line);color:var(--amber);font-size:18px;font-weight:800}}.product-mark{{display:grid;place-items:center;width:34px;height:34px;border-radius:7px;background:#171b1a;color:var(--amber);font-size:20px;font-weight:900;font-style:italic}}.change-title{{display:flex;align-items:center;justify-content:space-between;min-width:0;padding:0 24px}}.change-title>div{{min-width:0}}.change-title h1{{margin:0 0 3px;font-size:18px}}.change-title p{{margin:0;color:#a7b0aa;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}}.active-mode{{align-self:stretch;display:flex;align-items:center;padding:0 30px;border:0;border-bottom:3px solid var(--amber);background:transparent;color:var(--amber);font-weight:700}}.header-actions{{display:flex;align-items:center;justify-content:flex-end;gap:10px;padding:0 14px;border-left:1px solid var(--line)}}.change-stats{{margin-right:auto;color:#d7ddd9;white-space:nowrap;font-size:12px}}.change-stats .add{{color:var(--green);margin-left:16px}}.change-stats .del{{color:var(--red);margin-left:7px}}.header-actions button{{height:36px;border:1px solid #343b37;border-radius:7px;background:#151918;color:#d9dedb;padding:0 13px}}.header-actions #copy-feedback{{border-color:#8e5c0b;color:#ffc04d}}.header-actions .review-status{{height:auto;padding:2px 4px;border:0;background:transparent;color:#77827b;white-space:nowrap;font-size:11px}}.review-count{{color:var(--amber);margin-left:3px}}.icon-button{{display:inline-grid!important;place-items:center;width:32px!important;height:32px!important;padding:0!important;border:1px solid #343b37!important;border-radius:6px!important;background:#141817!important;color:#aeb8b1!important}}
.body-grid{{display:grid;grid-template-columns:318px minmax(0,1fr) 420px;min-height:0}}
.header-actions{{min-width:0;overflow-x:auto;scrollbar-width:none}}.header-actions::-webkit-scrollbar{{display:none}}.header-actions button{{flex:0 0 auto;white-space:nowrap}}.header-actions #copy-feedback,.header-actions #export-feedback{{min-width:82px}}
.file-nav{{min-width:0;overflow:auto;padding:14px 10px;border-right:1px solid var(--line);background:#101413}}.search-box{{display:flex;align-items:center;gap:8px;height:36px;margin-bottom:13px;padding:0 10px;border:1px solid #303633;border-radius:6px;background:#131716;color:var(--muted)}}.search-box svg{{width:15px}}.search-box input{{min-width:0;width:100%;border:0;outline:0;background:transparent;color:var(--ink);font-size:12px}}.tree-folder summary{{display:flex;align-items:center;gap:5px;padding:6px 4px;color:#bdc6c0;cursor:pointer;list-style:none}}.tree-folder summary::-webkit-details-marker{{display:none}}.tree-folder summary>svg:first-child{{width:13px;transition:transform .15s}}.tree-folder[open]>summary>svg:first-child{{transform:rotate(90deg)}}.tree-folder .folder-icon{{width:16px;color:#a3ada6}}.tree-children{{padding-left:13px}}.file-button{{display:grid;grid-template-columns:18px minmax(0,1fr) auto;align-items:center;gap:7px;width:100%;min-height:36px;margin:2px 0;padding:5px 7px;border:0;border-left:3px solid transparent;border-radius:5px;background:transparent;color:#b8c1bb;text-align:left}}.file-button:hover{{background:#1b201e}}.file-button.active{{border-left-color:var(--amber);background:#252a27;color:#ffb72d}}.file-button .file-icon{{width:16px;color:#8fb6a0}}.file-button .file-sql{{color:#e6a52b}}.file-label{{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:12px}}.file-status{{padding:2px 5px;border-radius:4px;background:#2b2718;color:#d9a328;font-size:10px}}.file-status.status-a{{background:#173426;color:#54c981}}.file-status.status-d{{background:#3d201f;color:#ef716a}}.file-button small{{grid-column:2/4;color:#65716a;font-size:10px}}
.center-shell{{display:flex;min-width:0;min-height:0;flex-direction:column;background:#0f1311}}.metrics{{display:grid;grid-template-columns:1.1fr 1.1fr .9fr .9fr;gap:12px;padding:13px 14px;border-bottom:1px solid var(--line)}}.metric-card{{display:grid;grid-template-columns:34px minmax(0,1fr);gap:10px;min-height:104px;padding:14px;border:1px solid #2b312e;border-radius:8px;background:linear-gradient(135deg,#1a1f1d,#151918);color:var(--ink);text-align:left}}.metric-card:hover{{border-color:#5a4725}}.metric-card>svg{{width:27px;height:27px;color:var(--amber)}}.metric-card:nth-child(2)>svg{{color:#7d9cff}}.metric-card strong,.metric-card span,.metric-card small,.metric-card b{{display:block}}.metric-card strong{{margin-bottom:7px;color:#dbe1dd;font-size:12px}}.metric-card span{{color:#aeb8b1;font-size:12px}}.metric-card b{{font-size:24px;font-weight:500}}.metric-card small{{margin-top:8px;color:#d89d28}}.workspace-stack{{min-height:0;flex:1;overflow:auto;padding:0 14px 14px}}.workspace-panel{{min-height:100%;border:1px solid var(--line);border-radius:8px;background:#121615;overflow:hidden}}
.code-section>header{{display:flex;align-items:center;justify-content:space-between;gap:15px;min-height:52px;padding:8px 14px;border-bottom:1px solid var(--line);background:#151918}}.file-heading{{display:flex;align-items:center;gap:10px;min-width:0}}.file-heading>svg{{color:#74aa83}}.file-heading span{{display:block;color:#7c8880;font-size:10px}}.file-heading h1{{margin:0;font-size:14px}}.file-heading-actions{{display:flex;align-items:center;gap:8px}}.status-badge{{padding:2px 6px;border-radius:4px;background:#213328;color:#64cc8a;font-size:10px}}.status-d{{background:#3b2020;color:#f17a73}}.implementation{{display:grid;grid-template-columns:auto minmax(0,1fr);gap:5px 12px;padding:9px 14px;border-bottom:1px solid var(--line);background:#111514}}.implementation strong{{color:#8d9991;font-size:11px}}.implementation span{{color:#d9dfdb;font-size:12px}}.implementation small{{grid-column:2;color:#76827b}}.boundary{{margin:0;padding:8px 14px;border-left:3px solid var(--amber);background:#2a2417;color:#d8c18c}}.code-layout{{min-width:0;background:var(--code)}}.diff-view{{min-width:0;overflow:auto;padding-bottom:30px;color:var(--code-ink);font:12px/1.58 ui-monospace,SFMono-Regular,Consolas,"Microsoft YaHei","PingFang SC","Walkthrough CJK",monospace;tab-size:4}}.diff-head,.diff-line,.ai-note-row{{display:grid;grid-template-columns:42px 42px 25px minmax(max-content,1fr);min-height:23px}}.diff-head{{position:sticky;top:0;z-index:3;background:#1a1f1d;color:#6f7b74;border-bottom:1px solid #303733}}.diff-head span,.diff-line>span{{padding:3px 6px;text-align:right}}.diff-head span:last-child{{text-align:left}}.diff-line{{width:100%;margin:0;padding:0;border:0;background:transparent;color:inherit;text-align:left}}.diff-line code{{padding:2px 15px 2px 5px;white-space:pre}}.diff-line .sign{{text-align:left}}.diff-line.add{{background:#163626}}.diff-line.del{{background:#482320}}.diff-line.meta{{background:#182027;color:#7795ad}}button.diff-line:hover,.diff-line.unit-active{{outline:1px solid #d79a26;outline-offset:-1px;background:#534319}}.diff-line.log-active{{outline:2px solid #f5a30b;outline-offset:-2px;animation:logFlash 1.8s ease}}@keyframes logFlash{{0%,100%{{filter:none}}35%{{filter:brightness(1.8)}}}}.ai-note-row{{align-items:stretch;background:#101513}}.ai-note-line{{position:relative;grid-column:1/4;min-height:44px}}.ai-note-line::after{{content:"";position:absolute;right:0;top:0;width:18px;height:20px;border-right:1px solid #8e641b;border-bottom:1px solid #8e641b}}.ai-note{{grid-column:4;display:grid;grid-template-columns:auto auto minmax(0,1fr);align-items:start;gap:8px;width:min(760px,calc(100% - 18px));margin:6px 12px 10px 5px;padding:9px 11px;border:1px solid #4a3c21;border-left:3px solid var(--amber);border-radius:5px;background:#1a1f1d;color:#aeb9b2;text-align:left;font-size:11px;white-space:normal}}.ai-note:hover{{border-color:#8f6720;background:#222824}}.ai-badge{{padding:2px 6px;border-radius:4px;background:#2d2819;color:#f1b33f;font-weight:700}}.ai-note strong{{padding-top:2px;color:#63ca88;white-space:nowrap}}.ai-note-text{{min-width:0;padding-top:2px;overflow-wrap:anywhere}}.binary-note{{padding:30px;color:var(--muted)}}
.workspace-heading{{display:flex;align-items:center;gap:13px;min-height:82px;padding:13px 16px;border-bottom:1px solid var(--line);background:#151918}}.workspace-heading>div:nth-child(2){{min-width:0;flex:1}}.workspace-heading span{{color:var(--amber);font-size:9px;font-weight:700}}.workspace-heading h2{{margin:1px 0;font-size:17px}}.workspace-heading p{{margin:0;color:var(--muted);font-size:11px}}.back-code{{display:grid;place-items:center;width:32px;height:32px;border:1px solid #343b37;border-radius:6px;background:#111514;color:#c3cbc6}}.secondary-action{{display:flex;align-items:center;gap:7px;height:34px;border:1px solid #6e4b16;border-radius:6px;background:#211c13;color:#f1b33d;padding:0 11px;font-size:11px}}.secondary-action svg{{width:15px}}.sql-section{{padding:16px}}.sql-section>h3{{margin:0 0 10px;font-size:13px}}.sql-section>h3 span{{margin-left:6px;color:var(--amber)}}.sql-block{{margin-bottom:12px;border:1px solid #2e3531;border-radius:7px;overflow:hidden;background:#0e1210}}.sql-block header{{display:flex;align-items:center;justify-content:space-between;gap:10px;padding:9px 12px;border-bottom:1px solid #2b312e;background:#171b1a}}.sql-block header>div{{display:flex;align-items:center;gap:8px}}.sql-block header span{{padding:2px 5px;border-radius:4px;background:#243125;color:#6dce91;font-size:9px}}.sql-block h3{{margin:0;font-size:12px}}.source-link,.impact-list .source-link{{border:0;background:transparent;color:#d69d31;padding:2px 4px;font-size:10px}}.sql-block pre{{margin:0;padding:14px;overflow:auto;color:#a9d6b7;font:12px/1.65 ui-monospace,SFMono-Regular,Consolas,monospace}}.empty-state{{padding:22px;border:1px dashed #323936;border-radius:6px;color:#69756e;text-align:center}}.log-toolbar{{display:flex;align-items:center;gap:8px;margin:14px;padding:0 11px;height:36px;border:1px solid #303733;border-radius:6px;background:#101412;color:var(--muted)}}.log-toolbar svg{{width:15px}}.log-toolbar input{{width:100%;border:0;outline:0;background:transparent;color:var(--ink)}}.log-table-wrap{{margin:0 14px 16px;overflow:auto;border:1px solid #303733;border-radius:7px}}.log-table{{width:100%;border-collapse:collapse;table-layout:fixed}}.log-table th,.log-table td{{padding:12px 14px;border-bottom:1px solid #29302c;text-align:left;vertical-align:top}}.log-table th:first-child,.log-table td:first-child{{width:36%}}.log-table th{{background:#181d1b;color:#f0b23d;font-size:11px}}.log-table td{{color:#b7c1ba;font-size:12px}}.keyword-cell{{display:flex;align-items:center;gap:6px}}.log-keyword{{min-width:0;border:0;background:transparent;color:#82b3ef;padding:0;text-align:left;font:11px/1.4 ui-monospace,SFMono-Regular,Consolas,monospace;overflow-wrap:anywhere}}.copy-keyword{{margin-left:auto;flex:0 0 auto}}.detail-grid{{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px;padding:14px}}.detail-card{{padding:13px;border:1px solid #303733;border-radius:7px;background:#171b1a}}.detail-card header{{display:flex;justify-content:space-between;gap:7px}}.detail-card dl{{display:grid;grid-template-columns:100px 1fr}}.detail-card dt,.detail-card dd{{padding:6px 0;border-bottom:1px solid #282e2b}}.detail-card dt{{color:var(--muted)}}.detail-card dd{{margin:0}}
.impact-sidebar{{min-width:0;border-left:1px solid var(--line);background:#101413}}.impact-tabs{{display:flex;height:52px;border-bottom:1px solid var(--line)}}.impact-tabs button{{flex:1;border:0;border-bottom:2px solid transparent;background:transparent;color:#929d96;font-size:12px}}.impact-tabs button[aria-selected=true]{{border-bottom-color:var(--amber);color:#f1b23e}}.side-scroll{{height:calc(100% - 52px);overflow:auto;padding:12px}}.impact-card{{margin-bottom:9px;padding:13px;border:1px solid #2b322e;border-radius:7px;background:#171b1a}}.impact-card h2{{display:flex;align-items:center;gap:8px;margin:0 0 8px;font-size:12px}}.impact-card h2 svg{{width:16px;color:#d39a30}}.impact-card.positive h2 svg{{color:#55cb82}}.impact-card p{{margin:5px 0;color:#a7b1aa;font-size:11px}}.impact-list{{display:grid;gap:5px}}.impact-list .source-link{{text-align:left;color:#aab5ae;font-family:ui-monospace,SFMono-Regular,Consolas,monospace}}.card-more{{margin-top:9px;border:0;background:transparent;color:#dca130;padding:0;font-size:10px}}.review-drawer{{position:fixed;inset:70px 0 0 auto;z-index:30;width:min(420px,100vw);padding:16px;border-left:1px solid #3a423d;background:#121615;box-shadow:-12px 0 30px #0008;overflow:auto}}.drawer-head{{display:flex;align-items:center;justify-content:space-between;margin-bottom:13px}}.unit-meta{{display:flex;justify-content:space-between;color:var(--muted);font-size:10px}}.unit-detail h2{{margin:7px 0;font-size:16px}}.unit-detail h3{{margin:14px 0 3px;color:#849088;font-size:10px;text-transform:uppercase}}.unit-detail p{{margin:0;color:#bdc6c0}}.ranges{{color:#6ea4e7!important;font-family:ui-monospace,SFMono-Regular,Consolas,monospace}}.review-box{{margin-top:18px;padding-top:14px;border-top:1px solid var(--line)}}.review-box label{{display:block;margin-bottom:6px;font-size:11px;font-weight:700}}.review-check{{display:flex!important;align-items:center;gap:7px;padding:8px;background:#1a1f1d}}.review-box textarea{{width:100%;padding:8px;border:1px solid #39413c;border-radius:6px;background:#0f1311;color:var(--ink);resize:vertical}}.copy-status{{position:fixed;right:16px;bottom:16px;z-index:60;padding:8px 11px;border-radius:6px;background:#252b28;color:#fff;box-shadow:0 5px 18px #0008}}
.workspace-stack{{padding:14px}}.impact-tabs button span{{margin-left:3px;color:#69756e;font-size:9px}}.side-scroll{{padding:0}}.side-panel{{min-height:100%;background:#101413;color:var(--ink)}}.side-panel-heading{{display:flex;align-items:center;justify-content:space-between;gap:10px;padding:12px;border-bottom:1px solid var(--line);background:#141817}}.side-panel-heading span{{color:var(--amber);font-size:9px;font-weight:700}}.side-panel-heading h2{{margin:2px 0 0;font-size:14px}}.side-panel .sql-section{{padding:12px}}.side-panel .sql-section>h3{{font-size:11px}}.side-panel .sql-block header{{align-items:flex-start;padding:8px}}.side-panel .sql-block header>div:first-child{{min-width:0;display:block}}.side-panel .sql-block header h3{{margin-top:4px;overflow-wrap:anywhere}}.side-panel .sql-block pre{{padding:10px;font-size:10px;white-space:pre-wrap;overflow-wrap:anywhere}}.side-panel .source-link{{display:block;margin-top:5px;text-align:left}}.side-panel .log-toolbar{{margin:10px}}.side-panel .log-table-wrap{{margin:0 10px 10px}}.side-panel .log-table th,.side-panel .log-table td{{padding:9px 8px;font-size:10px;overflow-wrap:anywhere}}.side-panel .log-table th:first-child,.side-panel .log-table td:first-child{{width:43%}}.side-panel .log-keyword{{font-size:10px}}.side-panel .copy-keyword{{display:none!important}}.api-list{{display:grid;gap:10px;padding:12px}}.api-change{{border:1px solid #303733;border-radius:7px;background:#171b1a;overflow:hidden}}.api-change>header{{display:flex;align-items:center;justify-content:space-between;gap:8px;padding:10px;border-bottom:1px solid #2a302d}}.api-change>header code{{color:#77afea;font-size:11px;overflow-wrap:anywhere}}.api-change dl{{display:grid;grid-template-columns:76px minmax(0,1fr);margin:0;padding:4px 10px 10px}}.api-change dt,.api-change dd{{padding:7px 0;border-bottom:1px solid #282e2b;font-size:10px}}.api-change dt{{color:#8c9790}}.api-change dd{{margin:0;color:#c2cbc5;overflow-wrap:anywhere}}
@media(max-width:1200px){{.topbar{{grid-template-columns:270px minmax(0,1fr)}}.header-actions{{grid-column:2;position:absolute;right:6px;top:12px;border:0}}.change-title{{padding-right:260px}}.body-grid{{grid-template-columns:270px minmax(0,1fr)}}.impact-sidebar{{display:none}}.metrics{{grid-template-columns:repeat(2,minmax(0,1fr))}}}}
@media(max-width:720px){{html,body{{overflow:auto}}.shell{{display:block;height:auto;min-height:100vh}}.topbar{{display:flex;height:auto;min-height:118px;flex-wrap:wrap;padding:8px}}.product{{width:100%;border:0;padding:0}}.change-title{{width:100%;padding:0}}.active-mode{{display:none}}.header-actions{{position:static;width:100%;padding:0}}.change-stats{{display:none}}.body-grid{{display:block}}.file-nav{{max-height:250px;border-right:0;border-bottom:1px solid var(--line)}}.center-shell{{min-height:700px}}.metrics{{grid-template-columns:1fr 1fr;padding:9px}}.metric-card{{min-height:90px}}.workspace-stack{{padding:0 8px 8px}}.impact-sidebar{{display:block;min-height:420px;border-left:0;border-top:1px solid var(--line)}}.side-scroll{{height:auto;min-height:368px}}.implementation{{grid-template-columns:1fr}}.implementation small{{grid-column:1}}.log-table th:first-child,.log-table td:first-child{{width:44%}}.workspace-heading{{align-items:flex-start;flex-wrap:wrap}}.secondary-action{{margin-left:45px}}.review-drawer{{inset:0;width:100%}}}}
</style></head><body>{render_icon_sprite()}<div class="shell">
<header class="topbar"><div class="product">FixForge</div>
<div class="change-title"><div><h1>代码变更理解</h1><p>{html.escape(compare_label)} · {summary}</p></div></div>
<div class="header-actions"><div class="change-stats"><strong>{len(changes)} files changed</strong><span class="add">+{additions}</span><span class="del">-{deletions}</span></div><button type="button" class="review-status" id="open-comments" title="查看批注">批注 <strong class="review-count">0</strong></button><button type="button" id="copy-feedback">复制给 AI</button><button type="button" id="export-feedback">导出批注</button></div></header>
<div class="body-grid">
<aside class="file-nav"><label class="search-box"><svg><use href="#icon-search"></use></svg><input id="file-filter" type="search" placeholder="搜索文件或模块"></label><div data-file-nav="tree">{render_tree(changes)}</div></aside>
<main class="center-shell"><div class="workspace-stack"><section class="workspace-panel code-workspace-panel" data-view-panel="code"><div class="code-main">{"".join(code_sections)}</div></section></div></main>{render_impact_sidebar(data, changes, unit_ids)}</div></div>
<aside class="review-drawer" id="review-drawer" hidden><div class="drawer-head"><strong>代码区块说明与批注</strong><button type="button" class="icon-button" id="close-drawer" title="关闭">×</button></div>{"".join(review_details)}</aside>
<script>(()=>{{
const meta={safe_script_json(feedback_meta)};const sidePanels=[...document.querySelectorAll('[data-side-panel]')];const sideTargets=[...document.querySelectorAll('[data-side-target]')];const codePanels=[...document.querySelectorAll('[data-code-panel]')];const fileButtons=[...document.querySelectorAll('.file-button')];const drawer=document.getElementById('review-drawer');
function notify(text){{const old=document.querySelector('.copy-status');if(old)old.remove();const el=document.createElement('div');el.className='copy-status';el.textContent=text;document.body.appendChild(el);setTimeout(()=>el.remove(),1800)}}
async function copyText(value,message='已复制'){{try{{await navigator.clipboard.writeText(value)}}catch{{const area=document.createElement('textarea');area.value=value;document.body.appendChild(area);area.select();document.execCommand('copy');area.remove()}}notify(message)}}
function showSide(name,writeHash=true){{sidePanels.forEach(panel=>panel.hidden=panel.dataset.sidePanel!==name);sideTargets.forEach(button=>button.setAttribute('aria-selected',String(button.dataset.sideTarget===name)));if(writeHash)history.pushState(null,'','#side-'+name)}}
function showFileWithoutReset(id){{codePanels.forEach(panel=>panel.hidden=panel.id!==id);fileButtons.forEach(button=>button.classList.toggle('active',button.dataset.codeTarget===id))}}
function clearUnitSelection(writeHash=true){{drawer.hidden=true;document.querySelectorAll('[data-unit-detail]').forEach(detail=>detail.hidden=true);document.querySelectorAll('.diff-line.unit-active,.diff-line.log-active').forEach(line=>line.classList.remove('unit-active','log-active'));const panel=document.querySelector('[data-code-panel]:not([hidden])');if(writeHash&&panel)history.pushState(null,'','#'+panel.id)}}
function showFile(id){{clearUnitSelection(false);showFileWithoutReset(id);history.pushState(null,'','#'+id)}}
function showUnit(id,writeHash=true){{const unit=meta.units.find(item=>item.id===id);if(!unit)return;showFileWithoutReset('code-'+unit.file_index);document.querySelectorAll('[data-unit-detail]').forEach(detail=>detail.hidden=detail.id!=='unit-'+id);drawer.hidden=false;document.querySelectorAll('.diff-line.log-active').forEach(line=>line.classList.remove('log-active'));document.querySelectorAll('.diff-line[data-unit-target]').forEach(line=>line.classList.toggle('unit-active',line.dataset.unitTarget===id));document.querySelector('.diff-line[data-unit-target="'+CSS.escape(id)+'"]')?.scrollIntoView({{block:'center',behavior:'smooth'}});if(writeHash)history.pushState(null,'','#unit-'+encodeURIComponent(id))}}
function toggleUnit(id){{const selected=[...document.querySelectorAll('.diff-line.unit-active')].some(line=>line.dataset.unitTarget===id);if(selected)clearUnitSelection();else showUnit(id)}}
function showLogLine(unit,line,side){{showUnit(unit,false);drawer.hidden=true;const panel=document.querySelector('[data-code-panel]:not([hidden])');const target=panel?.querySelector('[data-'+(side==='old'?'old':'new')+'-line="'+CSS.escape(line)+'"]');document.querySelectorAll('.diff-line.log-active').forEach(node=>node.classList.remove('log-active'));if(target){{target.classList.add('log-active');target.scrollIntoView({{block:'center',behavior:'smooth'}})}}history.pushState(null,'','#unit-'+encodeURIComponent(unit))}}
sideTargets.forEach(button=>button.addEventListener('click',()=>showSide(button.dataset.sideTarget)));fileButtons.forEach(button=>button.addEventListener('click',()=>showFile(button.dataset.codeTarget)));document.querySelectorAll('.diff-line[data-unit-target]').forEach(line=>line.addEventListener('click',()=>toggleUnit(line.dataset.unitTarget)));document.querySelectorAll('[data-unit-target]:not(.diff-line)').forEach(button=>button.addEventListener('click',()=>{{if(button.dataset.unitTarget)showUnit(button.dataset.unitTarget)}}));
document.querySelectorAll('[data-log-unit]').forEach(button=>button.addEventListener('click',()=>showLogLine(button.dataset.logUnit,button.dataset.logLine,button.dataset.logSide)));
document.querySelectorAll('[data-copy-text]').forEach(button=>button.addEventListener('click',event=>{{event.stopPropagation();copyText(button.dataset.copyText)}}));document.getElementById('close-drawer').addEventListener('click',()=>clearUnitSelection());document.addEventListener('keydown',event=>{{if(event.key==='Escape'&&!drawer.hidden)clearUnitSelection()}});
document.getElementById('file-filter').addEventListener('input',event=>{{const query=event.target.value.trim().toLowerCase();fileButtons.forEach(button=>button.hidden=Boolean(query)&&!button.title.toLowerCase().includes(query));document.querySelectorAll('.tree-folder').forEach(folder=>{{folder.hidden=![...folder.querySelectorAll('.file-button')].some(button=>!button.hidden)}})}});
const logFilter=document.getElementById('log-filter');if(logFilter)logFilter.addEventListener('input',event=>{{const query=event.target.value.trim().toLowerCase();document.querySelectorAll('.log-table tbody tr').forEach(row=>row.hidden=Boolean(query)&&!row.textContent.toLowerCase().includes(query))}});
const copyAllSql=document.getElementById('copy-all-sql');if(copyAllSql)copyAllSql.addEventListener('click',()=>copyText([...document.querySelectorAll('.sql-block pre')].map(node=>node.textContent).join('\\n\\n'),'全部 SQL 已复制'));
const copyAllKeywords=document.getElementById('copy-all-keywords');if(copyAllKeywords)copyAllKeywords.addEventListener('click',()=>copyText([...document.querySelectorAll('.log-keyword')].map(node=>node.textContent.trim()).join('\\n'),'日志关键词已复制'));
const storageKey='code-change-review:'+meta.comparison.fingerprint;function collect(){{return{{version:1,comparison:meta.comparison,comments:meta.units.map(unit=>{{const comment=document.querySelector('[data-review-comment="'+CSS.escape(unit.id)+'"]')?.value.trim()||'';return{{...unit,comment}}}}).filter(item=>item.comment)}}}}function persist(){{const value=collect();document.querySelector('.review-count').textContent=String(value.comments.length);try{{localStorage.setItem(storageKey,JSON.stringify(value))}}catch{{}}}}function restore(){{try{{const saved=JSON.parse(localStorage.getItem(storageKey)||'null');(saved?.comments||[]).forEach(item=>{{const text=document.querySelector('[data-review-comment="'+CSS.escape(item.id)+'"]');if(text)text.value=item.comment||''}})}}catch{{}}persist()}}
document.getElementById('open-comments').addEventListener('click',()=>{{const first=collect().comments[0]?.id||meta.units[0]?.id;if(first)showUnit(first)}});
document.querySelectorAll('[data-review-comment]').forEach(node=>node.addEventListener('input',persist));function feedbackText(value){{return '请根据以下代码区块批注修改代码，完成后重新测试并生成新版代码图解：\\n\\n'+JSON.stringify(value,null,2)}}document.getElementById('copy-feedback').addEventListener('click',()=>copyText(feedbackText(collect()),'批注已复制'));document.getElementById('export-feedback').addEventListener('click',()=>{{const blob=new Blob([JSON.stringify(collect(),null,2)+'\\n'],{{type:'application/json'}});const link=document.createElement('a');link.href=URL.createObjectURL(blob);link.download='review-feedback.json';document.body.appendChild(link);link.click();link.remove();setTimeout(()=>URL.revokeObjectURL(link.href),0);notify('批注文件已导出')}});
function restoreHash(){{const hash=decodeURIComponent(location.hash);clearUnitSelection(false);if(hash.startsWith('#unit-'))showUnit(hash.slice(6),false);else if(hash.startsWith('#side-'))showSide(hash.slice(6),false);else if(/^#code-[0-9]+$/.test(hash))showFileWithoutReset(hash.slice(1));else{{if(codePanels[0])showFileWithoutReset(codePanels[0].id);showSide('storage',false)}}}}if(codePanels[0])showFileWithoutReset(codePanels[0].id);showSide('storage',false);restoreHash();window.addEventListener('popstate',restoreHash);restore();
}})();</script></body></html>'''


def safe_script_json(value: Any) -> str:
    return (
        json.dumps(value, ensure_ascii=False, separators=(",", ":"))
        .replace("<", "\\u003c")
        .replace(">", "\\u003e")
        .replace("&", "\\u0026")
    )


def build_view_model(
    data: Dict[str, Any],
    comparison: Dict[str, Any],
    changes: List[Dict[str, Any]],
    unit_ids: Dict[str, Dict[str, Any]],
) -> Dict[str, Any]:
    files = []
    additions = 0
    deletions = 0
    for file_index, change in enumerate(changes):
        units = change["units"]
        source_rows = (
            final_source_rows(change["new_source"], units)
            if change["attribution"] == "final_only"
            else change["diff_lines"]
        )
        anchors = unit_note_anchors(units, source_rows)
        rows = []
        for row_index, raw in enumerate(source_rows):
            row = {
                "kind": raw["kind"],
                "old_line": raw.get("old_line"),
                "new_line": raw.get("new_line"),
                "code": raw["code"],
                "unit_id": unit_for_line(units, raw) or "",
            }
            anchored = [unit_id for unit_id, anchor_index in anchors.items() if anchor_index == row_index]
            if anchored:
                row["note_unit_ids"] = anchored
            rows.append(row)
            if raw["kind"] == "add":
                additions += 1
            elif raw["kind"] == "del":
                deletions += 1
        files.append(
            {
                "index": file_index,
                "status": change["status"],
                "old_file": change.get("old_file"),
                "new_file": change.get("new_file"),
                "display_file": display_path(change),
                "binary": change["binary"],
                "purpose": change["purpose"],
                "implementation": change["implementation"],
                "attribution": change["attribution"],
                "final_only_reason": change["final_only_reason"],
                "rows": rows,
                "units": units,
            }
        )
    return {
        "version": 1,
        "title": data["title"],
        "summary": data["summary"],
        "comparison": comparison,
        "stats": {"files": len(files), "additions": additions, "deletions": deletions},
        "files": files,
        "units": list(unit_ids.values()),
        "flows": data.get("flows", []),
        "database_changes": data.get("database_changes", []),
        "config_changes": data.get("config_changes", []),
        "api_changes": data.get("api_changes", []),
        "log_points": data.get("log_points", []),
    }


def render_html(
    data: Dict[str, Any],
    comparison: Dict[str, Any],
    changes: List[Dict[str, Any]],
    unit_ids: Dict[str, Dict[str, Any]],
    font_data: str = "",
) -> str:
    code_sections = []
    feedback_units = []
    review_details = []
    for file_index, change in enumerate(changes):
        units = change["units"]
        unit_notes = {unit["id"]: render_ai_note(unit) for unit in units}
        for unit in units:
            review_details.append(
                f'<article class="unit-detail" id="unit-{html.escape(unit["id"])}" data-unit-detail hidden>'
                f'<div class="unit-meta"><span>{html.escape(unit["kind"])}</span>'
                f'<code>{html.escape(unit["symbol"])}</code></div>'
                f'<h2>{html.escape(unit["title"])}</h2>'
                f'<p class="ranges">旧 {range_label(unit["old_range"])} · 新 {range_label(unit["new_range"])}</p>'
                f'<h3>这个区块做什么</h3><p>{html.escape(unit["meaning"])}</p>'
                f'<h3>为什么这样改</h3><p>{html.escape(unit["reason"])}</p>'
                f'<h3>影响范围</h3><p>{html.escape(unit["impact"])}</p>'
                '<section class="review-box">'
                f'<label for="comment-{html.escape(unit["id"])}">批注意见</label>'
                f'<textarea id="comment-{html.escape(unit["id"])}" data-review-comment="{html.escape(unit["id"])}" '
                'rows="5" placeholder="说明问题、期望结果或必须遵守的约束"></textarea></section></article>'
            )
            feedback_units.append(
                {
                    "id": unit["id"],
                    "file": unit["display_file"],
                    "kind": unit["kind"],
                    "symbol": unit["symbol"],
                    "title": unit["title"],
                    "old_range": list(unit["old_range"]) if unit["old_range"] else None,
                    "new_range": list(unit["new_range"]) if unit["new_range"] else None,
                    "file_index": file_index,
                }
            )

        if change["binary"]:
            diff_rows = '<div class="binary-note">二进制文件已变化，页面不展示内容。</div>' + "".join(unit_notes.values())
        else:
            rows = final_source_rows(change["new_source"], units) if change["attribution"] == "final_only" else change["diff_lines"]
            note_anchors = unit_note_anchors(units, rows)
            anchored_units = set(note_anchors)
            rendered_rows = []
            for row_index, item in enumerate(rows):
                old_number = item.get("old_line") or ""
                new_number = item.get("new_line") or ""
                sign = "+" if item["kind"] == "add" else "-" if item["kind"] == "del" else ""
                unit_id = unit_for_line(units, item)
                attrs = f' data-unit-target="{html.escape(unit_id)}"' if unit_id else ""
                if item.get("new_line"):
                    attrs += f' data-new-line="{item["new_line"]}"'
                if item.get("old_line"):
                    attrs += f' data-old-line="{item["old_line"]}"'
                body = (
                    f'<span>{old_number}</span><span>{new_number}</span><span class="sign">{sign}</span>'
                    f'<code>{html.escape(item["code"])}</code>'
                )
                if unit_id and item["kind"] in ("add", "del"):
                    rendered_rows.append(f'<button type="button" class="diff-line {item["kind"]}"{attrs}>{body}</button>')
                else:
                    rendered_rows.append(f'<div class="diff-line {item["kind"]}"{attrs}>{body}</div>')
                if unit_id and note_anchors.get(unit_id) == row_index:
                    rendered_rows.append(unit_notes[unit_id])
            rendered_rows.extend(note for unit_id, note in unit_notes.items() if unit_id not in anchored_units)
            diff_rows = "".join(rendered_rows)

        boundary = ""
        if change["attribution"] == "final_only":
            boundary = (
                '<p class="boundary">该文件在实施基线前已有改动，只展示明确标注的最终源码区块，'
                f'不会把完整差异归因于本次 AI 修改。{html.escape(change["final_only_reason"])}</p>'
            )
        code_sections.append(
            f'<section id="code-{file_index}" class="code-section" data-code-panel hidden>'
            f'<header><div class="file-heading"><svg><use href="#icon-file-code"></use></svg><div>'
            f'<span>{html.escape(str(PurePosixPath(display_path(change)).parent))}</span>'
            f'<h1>{html.escape(PurePosixPath(display_path(change)).name)}</h1></div></div>'
            f'<div class="file-heading-actions"><span class="status-badge status-{change["status"].lower()}">'
            f'{STATUS_LABELS.get(change["status"], change["status"])}</span>'
            f'<button type="button" class="icon-button" data-copy-text="{html.escape(display_path(change), quote=True)}" title="复制路径"><svg><use href="#icon-copy"></use></svg></button></div></header>'
            f'<div class="implementation"><strong>变更目标</strong><span>{html.escape(change["purpose"])}</span><small>{html.escape(change["implementation"])}</small></div>{boundary}'
            f'<div class="code-layout"><div class="diff-view"><div class="diff-head"><span>旧</span><span>新</span><span></span><span>代码</span></div>{diff_rows}</div></div>'
            '</section>'
        )

    title = html.escape(data["title"])
    summary = html.escape(data["summary"])
    mode_label = "本地未提交改动" if comparison["mode"] == "working_tree" else "分支比较"
    compare_label = (
        f'{comparison["base_ref"]} → 工作区'
        if comparison["mode"] == "working_tree"
        else f'{comparison["base_ref"]} → {comparison["head_ref"]}'
    )
    font_css = ""
    if font_data:
        font_css = (
            "@font-face{font-family:'Walkthrough CJK';"
            f"src:url(data:font/woff2;base64,{font_data}) format('woff2');font-style:normal;font-weight:100 900;}}"
        )
    feedback_meta = {
        "version": 1,
        "comparison": {
            "mode": comparison["mode"],
            "base_ref": comparison["base_ref"],
            "head_ref": comparison["head_ref"],
            "base_sha": comparison["base_sha"],
            "head_sha": comparison["head_sha"],
            "compare_sha": comparison["compare_sha"],
            "fingerprint": comparison["fingerprint"],
        },
        "units": feedback_units,
    }
    additions = sum(1 for change in changes for line in change["diff_lines"] if line["kind"] == "add")
    deletions = sum(1 for change in changes for line in change["diff_lines"] if line["kind"] == "del")
    return render_workbench_document(
        data,
        comparison,
        changes,
        unit_ids,
        title,
        summary,
        compare_label,
        font_css,
        code_sections,
        review_details,
        feedback_meta,
        additions,
        deletions,
    )



def main() -> int:
    parser = argparse.ArgumentParser(description="生成支持本地改动和分支比较的离线代码变更图解")
    parser.add_argument("--change-dir", required=True)
    parser.add_argument("--repo-root")
    parser.add_argument("--input")
    parser.add_argument("--baseline")
    parser.add_argument("--output")
    parser.add_argument("--output-format", choices=("html", "json"), default="html")
    parser.add_argument("--font", help="可选 WOFF2 字体，将以 data URL 内嵌")
    parser.add_argument("--validate-only", action="store_true")
    args = parser.parse_args()

    root = Path(args.repo_root).resolve() if args.repo_root else project_root().resolve()
    change_dir = Path(args.change_dir).resolve()
    input_path = Path(args.input).resolve() if args.input else change_dir / "walkthrough.json"
    baseline_path = Path(args.baseline).resolve() if args.baseline else change_dir / "baseline.json"
    output_path = Path(args.output).resolve() if args.output else change_dir / "walkthrough.html"

    try:
        for path in (change_dir, input_path, output_path):
            path.relative_to(root)
        data = load_object(input_path, "walkthrough")
        baseline = load_object(baseline_path, "baseline", required=False)
        automatic_excludes = [
            value for value in (
                relative_if_inside(root, change_dir),
                relative_if_inside(root, input_path),
                relative_if_inside(root, baseline_path),
                relative_if_inside(root, output_path),
            ) if value
        ]
        comparison, discovered = build_comparison(root, data, baseline, automatic_excludes)
        changes, unit_ids = validate_walkthrough(root, data, comparison, discovered)
        if args.validate_only:
            print(
                "[CODE_CHANGE_VISUALIZER] stage=validate result=complete "
                f"mode={comparison['mode']} files={len(changes)} units={len(unit_ids)} "
                f"fingerprint={comparison['fingerprint'][:12]}"
            )
            return 0
        if args.output_format == "json":
            rendered = json.dumps(
                build_view_model(data, comparison, changes, unit_ids),
                ensure_ascii=False,
                separators=(",", ":"),
            )
        else:
            font_data = ""
            if args.font:
                font_path = Path(args.font).resolve()
                if not font_path.is_file() or font_path.suffix.lower() != ".woff2":
                    raise ValueError("font must be an existing WOFF2 file")
                font_data = base64.b64encode(font_path.read_bytes()).decode("ascii")
            rendered = render_html(data, comparison, changes, unit_ids, font_data)
    except (ValueError, OSError) as exc:
        return fail("invalid_walkthrough", str(exc))

    output_path.parent.mkdir(parents=True, exist_ok=True)
    temporary = output_path.with_suffix(output_path.suffix + ".tmp")
    temporary.write_text(rendered, encoding="utf-8")
    temporary.replace(output_path)
    print(
        "[CODE_CHANGE_VISUALIZER] stage=render result=complete "
        f"mode={comparison['mode']} files={len(changes)} units={len(unit_ids)} "
        f"output={output_path.relative_to(root)}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
