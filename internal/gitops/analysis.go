package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxAnalysisDiffBytes = 2 << 20

type AnalysisSelection struct {
	Mode         string   `json:"mode"`
	BaseRef      string   `json:"base_ref"`
	HeadRef      string   `json:"head_ref"`
	Strategy     string   `json:"strategy"`
	CommitHashes []string `json:"commit_hashes"`
}

type AnalysisSnapshot struct {
	Comparison map[string]any `json:"comparison"`
	Branches   map[string]any `json:"branches"`
	Commits    string         `json:"commits"`
	Stat       string         `json:"stat"`
	Diff       string         `json:"diff"`
}

func AnalysisBranchOptions(ctx context.Context, root string) (map[string]any, error) {
	return branches(ctx, root, false)
}

// RemoteAnalysisBranchOptions returns only origin tracking branches. The
// caller is responsible for fetching while holding the repository lock.
func RemoteAnalysisBranchOptions(ctx context.Context, root string) (map[string]any, error) {
	payload, err := branches(ctx, root, false)
	if err != nil {
		return nil, err
	}
	options, _ := payload["branch_options"].([]BranchOption)
	remoteOptions := make([]BranchOption, 0, len(options))
	remoteBranches := make([]string, 0, len(options))
	for _, option := range options {
		if !option.Remote {
			continue
		}
		option.Local = false
		option.Current = false
		remoteOptions = append(remoteOptions, option)
		remoteBranches = append(remoteBranches, option.Name)
	}
	return map[string]any{
		"branches":       remoteBranches,
		"current_branch": "",
		"branch_options": remoteOptions,
	}, nil
}

func ResolveAnalysisSelection(ctx context.Context, root string, selection AnalysisSelection) (map[string]any, error) {
	mode := strings.TrimSpace(selection.Mode)
	switch mode {
	case "branches", "ai":
		strategy := strings.TrimSpace(selection.Strategy)
		if strategy == "" {
			strategy = "merge_base"
		}
		if strategy != "merge_base" && strategy != "direct" {
			return nil, fmt.Errorf("comparison strategy must be merge_base or direct")
		}
		baseRef, baseSHA, err := resolveAnalysisRef(ctx, root, selection.BaseRef)
		if err != nil {
			return nil, fmt.Errorf("invalid base ref: %w", err)
		}
		headRef, headSHA, err := resolveAnalysisRef(ctx, root, selection.HeadRef)
		if err != nil {
			return nil, fmt.Errorf("invalid head ref: %w", err)
		}
		if baseRef == headRef {
			return nil, fmt.Errorf("base and head refs must be different")
		}
		return map[string]any{
			"mode":     "branch_compare",
			"base_ref": baseRef,
			"head_ref": headRef,
			"base_sha": baseSHA,
			"head_sha": headSHA,
			"strategy": strategy,
		}, nil
	case "commits":
		return resolveCommitSelection(ctx, root, selection.CommitHashes)
	case "working_tree":
		return resolveWorkingTreeSelection(ctx, root)
	default:
		return nil, fmt.Errorf("analysis mode must be branches, commits, ai, or working_tree")
	}
}

// ResolveRemoteAnalysisSelection resolves branch labels exclusively through
// origin tracking refs and records immutable SHAs for the rest of the analysis.
// The caller is responsible for fetching and locking the repository first.
func ResolveRemoteAnalysisSelection(ctx context.Context, root string, selection AnalysisSelection) (map[string]any, error) {
	mode := strings.TrimSpace(selection.Mode)
	if mode != "branches" && mode != "ai" {
		return ResolveAnalysisSelection(ctx, root, selection)
	}
	strategy := strings.TrimSpace(selection.Strategy)
	if strategy == "" {
		strategy = "merge_base"
	}
	if strategy != "merge_base" && strategy != "direct" {
		return nil, fmt.Errorf("comparison strategy must be merge_base or direct")
	}
	baseRef, baseSHA, err := resolveRemoteAnalysisRef(ctx, root, selection.BaseRef)
	if err != nil {
		return nil, fmt.Errorf("invalid remote base ref: %w", err)
	}
	headRef, headSHA, err := resolveRemoteAnalysisRef(ctx, root, selection.HeadRef)
	if err != nil {
		return nil, fmt.Errorf("invalid remote head ref: %w", err)
	}
	if baseRef == headRef {
		return nil, fmt.Errorf("base and head refs must be different")
	}
	return map[string]any{
		"mode":     "branch_compare",
		"base_ref": baseRef,
		"head_ref": headRef,
		"base_sha": baseSHA,
		"head_sha": headSHA,
		"strategy": strategy,
	}, nil
}

func resolveWorkingTreeSelection(ctx context.Context, root string) (map[string]any, error) {
	_, baseSHA, err := resolveAnalysisRef(ctx, root, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("working tree has no commit to use as a baseline: %w", err)
	}
	entries, err := ChangedFiles(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("read working tree changes: %w", err)
	}
	changedPaths := make([]string, 0, len(entries))
	for _, entry := range entries {
		path, _ := entry["path"].(string)
		path = strings.TrimSpace(path)
		if path == "" || entry["type"] == "directory" || entry["status"] == "untracked" {
			continue
		}
		changedPaths = append(changedPaths, path)
	}
	if len(changedPaths) == 0 {
		return nil, fmt.Errorf("working tree has no tracked uncommitted changes")
	}
	snapshotFingerprint, err := workingTreeFingerprint(ctx, root, baseSHA, changedPaths)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"mode":                 "working_tree",
		"base_ref":             baseSHA,
		"base_sha":             baseSHA,
		"head_ref":             "WORKTREE",
		"head_sha":             baseSHA,
		"strategy":             "working_tree",
		"changed_paths":        changedPaths,
		"snapshot_fingerprint": snapshotFingerprint,
	}, nil
}

// AnalysisCandidates returns the local-only choices exposed to the comparison
// selector. Git remains authoritative: the model may choose an item, but it
// cannot introduce a ref or commit that is absent from this payload.
func AnalysisCandidates(ctx context.Context, root string) (map[string]any, error) {
	branchData, err := AnalysisBranchOptions(ctx, root)
	if err != nil {
		return nil, err
	}
	workingTree := map[string]any{"available": false}
	if comparison, workingErr := resolveWorkingTreeSelection(ctx, root); workingErr == nil {
		workingTree = map[string]any{
			"available":      true,
			"changed_paths":  comparison["changed_paths"],
			"base_sha":       comparison["base_sha"],
			"comparison_ref": "WORKTREE",
		}
	}
	return map[string]any{
		"branches":     branchData,
		"working_tree": workingTree,
	}, nil
}

// ValidateAnalysisSnapshot verifies that a tracked working-tree comparison is
// still byte-for-byte identical to the snapshot selected before the model ran.
// Commit comparisons are immutable once their SHAs have been resolved.
func ValidateAnalysisSnapshot(ctx context.Context, root string, comparison map[string]any) error {
	if strings.TrimSpace(fmt.Sprint(comparison["mode"])) != "working_tree" {
		return nil
	}
	expected := strings.TrimSpace(fmt.Sprint(comparison["snapshot_fingerprint"]))
	if expected == "" {
		return fmt.Errorf("working tree comparison is missing snapshot_fingerprint")
	}
	baseSHA := strings.TrimSpace(fmt.Sprint(comparison["base_sha"]))
	if baseSHA == "" {
		baseSHA = strings.TrimSpace(fmt.Sprint(comparison["base_ref"]))
	}
	paths := stringsFromAny(comparison["changed_paths"])
	actual, err := workingTreeFingerprint(ctx, root, baseSHA, paths)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("local working tree changed after this analysis snapshot was locked")
	}
	return nil
}

// AnalysisSource reads a complete file from the locked comparison. Branch and
// commit analyses use the immutable head SHA. Working-tree analyses are only
// readable while the local snapshot fingerprint still matches.
func AnalysisSource(ctx context.Context, root string, comparison map[string]any, path string) (map[string]any, error) {
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if path == "" || path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
		return nil, fmt.Errorf("invalid analysis source path")
	}
	mode := strings.TrimSpace(fmt.Sprint(comparison["mode"]))
	var content string
	if codeAnalysisString(comparison["snapshot_id"]) != "" {
		if err := ValidateMaterializedAnalysisSnapshot(ctx, root, comparison); err != nil {
			return nil, err
		}
		snapshotSHA := codeAnalysisString(comparison["snapshot_sha"], comparison["head_sha"])
		value, err := gitOutput(ctx, gitCommandRoot(ctx, root), "show", snapshotSHA+":"+path)
		if err != nil {
			baseSHA := codeAnalysisString(comparison["compare_sha"], comparison["base_sha"], comparison["base_ref"])
			value, err = gitOutput(ctx, gitCommandRoot(ctx, root), "show", baseSHA+":"+path)
			if err != nil {
				return nil, fmt.Errorf("read materialized analysis source: %w", err)
			}
		}
		content = value
	} else if mode == "working_tree" {
		repoRoot := gitCommandRoot(ctx, root)
		changed := map[string]bool{}
		for _, changedPath := range stringsFromAny(comparison["changed_paths"]) {
			changed[filepath.ToSlash(changedPath)] = true
		}
		if changed[path] {
			if err := ValidateAnalysisSnapshot(ctx, root, comparison); err != nil {
				return nil, err
			}
			fullPath := filepath.Join(repoRoot, filepath.FromSlash(path))
			rel, err := filepath.Rel(repoRoot, fullPath)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("analysis source path escapes repository")
			}
			data, readErr := os.ReadFile(fullPath)
			if readErr == nil {
				content = string(data)
			} else if os.IsNotExist(readErr) {
				baseSHA := codeAnalysisString(comparison["compare_sha"], comparison["base_sha"], comparison["base_ref"])
				value, showErr := gitOutput(ctx, repoRoot, "show", baseSHA+":"+path)
				if showErr != nil {
					return nil, fmt.Errorf("read deleted analysis source: %w", showErr)
				}
				content = value
			} else {
				return nil, fmt.Errorf("read analysis source: %w", readErr)
			}
		} else {
			baseSHA := codeAnalysisString(comparison["base_sha"], comparison["base_ref"])
			value, err := gitOutput(ctx, repoRoot, "show", baseSHA+":"+path)
			if err != nil {
				return nil, fmt.Errorf("read locked analysis context source: %w", err)
			}
			content = value
		}
	} else {
		headSHA := strings.TrimSpace(fmt.Sprint(comparison["head_sha"]))
		if headSHA == "" {
			return nil, fmt.Errorf("analysis comparison is missing head_sha")
		}
		value, err := gitOutput(ctx, gitCommandRoot(ctx, root), "show", headSHA+":"+path)
		if err != nil {
			baseSHA := codeAnalysisString(comparison["compare_sha"], comparison["base_sha"], comparison["base_ref"])
			value, err = gitOutput(ctx, gitCommandRoot(ctx, root), "show", baseSHA+":"+path)
			if err != nil {
				return nil, fmt.Errorf("read locked analysis source: %w", err)
			}
		}
		content = value
	}
	return map[string]any{
		"path":        path,
		"content":     content,
		"fingerprint": comparison["snapshot_fingerprint"],
		"mode":        mode,
	}, nil
}

func codeAnalysisString(values ...any) string {
	for _, value := range values {
		if text := strings.TrimSpace(fmt.Sprint(value)); text != "" && text != "<nil>" {
			return text
		}
	}
	return ""
}

func workingTreeFingerprint(ctx context.Context, root, baseSHA string, paths []string) (string, error) {
	baseSHA = strings.TrimSpace(baseSHA)
	if baseSHA == "" {
		return "", fmt.Errorf("working tree snapshot is missing base SHA")
	}
	cleaned, err := cleanPaths(root, paths)
	if err != nil {
		return "", err
	}
	if len(cleaned) == 0 {
		return "", fmt.Errorf("working tree snapshot has no tracked paths")
	}
	args := append([]string{"diff", "--binary", "--find-renames", baseSHA, "--"}, cleaned...)
	diff, err := gitOutput(ctx, gitCommandRoot(ctx, root), args...)
	if err != nil {
		return "", fmt.Errorf("lock working tree diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		return "", fmt.Errorf("working tree snapshot has no readable diff")
	}
	return workingTreeDiffFingerprint(baseSHA, diff), nil
}

func workingTreeDiffFingerprint(baseSHA, diff string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(baseSHA) + "\x00" + diff))
	return fmt.Sprintf("%x", sum[:])
}

func stringsFromAny(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func AnalysisContext(ctx context.Context, root string, comparison map[string]any) (string, error) {
	baseRef := strings.TrimSpace(fmt.Sprint(comparison["base_ref"]))
	headRef := strings.TrimSpace(fmt.Sprint(comparison["head_ref"]))
	strategy := strings.TrimSpace(fmt.Sprint(comparison["strategy"]))
	baseRef, baseSHA, err := resolveLockedAnalysisRef(ctx, root, baseRef, comparison["base_sha"])
	if err != nil {
		return "", err
	}
	headRef, headSHA, err := resolveLockedAnalysisRef(ctx, root, headRef, comparison["head_sha"])
	if err != nil {
		return "", err
	}
	compareSHA := baseSHA
	if strategy == "merge_base" {
		out, mergeErr := gitOutput(ctx, root, "merge-base", baseSHA, headSHA)
		if mergeErr != nil || strings.TrimSpace(out) == "" {
			return "", fmt.Errorf("cannot find merge base for %s and %s", baseRef, headRef)
		}
		compareSHA = strings.TrimSpace(out)
	}
	stat, err := gitOutput(ctx, root, "diff", "--find-renames", "--stat", compareSHA, headSHA, "--")
	if err != nil {
		return "", fmt.Errorf("git diff stat failed: %w", err)
	}
	diff, err := gitOutput(ctx, root, "diff", "--find-renames", "--binary", "--unified=35", compareSHA, headSHA, "--")
	if err != nil {
		return "", fmt.Errorf("git diff failed: %w", err)
	}
	if len(diff) > maxAnalysisDiffBytes {
		return "", fmt.Errorf("comparison diff is too large for AI analysis (%d bytes, limit %d)", len(diff), maxAnalysisDiffBytes)
	}
	if strings.TrimSpace(diff) == "" {
		return "", fmt.Errorf("selected comparison has no code changes")
	}
	commits, _ := gitOutput(ctx, root, "log", "--format=%H %s", compareSHA+".."+headSHA)
	if lockedCommits := codeAnalysisString(comparison["source_commits"]); lockedCommits != "" {
		commits = lockedCommits
	}
	branchData, _ := branches(ctx, root, false)
	snapshot := AnalysisSnapshot{
		Comparison: map[string]any{
			"mode":        "branch_compare",
			"base_ref":    baseRef,
			"head_ref":    headRef,
			"strategy":    strategy,
			"base_sha":    baseSHA,
			"head_sha":    headSHA,
			"compare_sha": compareSHA,
		},
		Branches: branchData,
		Commits:  strings.TrimSpace(commits),
		Stat:     strings.TrimSpace(stat),
		Diff:     diff,
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func resolveCommitSelection(ctx context.Context, root string, hashes []string) (map[string]any, error) {
	if len(hashes) == 0 {
		return nil, fmt.Errorf("select at least one commit")
	}
	if len(hashes) > 30 {
		return nil, fmt.Errorf("at most 30 commits can be analyzed at once")
	}
	verified := make([]string, 0, len(hashes))
	seen := map[string]bool{}
	for _, hash := range hashes {
		_, sha, err := resolveAnalysisRef(ctx, root, hash)
		if err != nil {
			return nil, fmt.Errorf("invalid selected commit %q: %w", hash, err)
		}
		if !seen[sha] {
			verified = append(verified, sha)
			seen[sha] = true
		}
	}
	newest := verified[0]
	oldest := verified[len(verified)-1]
	parent, err := verifyCommit(ctx, root, oldest+"^")
	if err != nil {
		return nil, fmt.Errorf("the oldest selected commit has no parent and cannot be used as a comparison base")
	}
	rawRange, err := gitOutput(ctx, root, "rev-list", "--first-parent", "--reverse", parent+".."+newest)
	if err != nil {
		return nil, fmt.Errorf("selected commit range is invalid: %w", err)
	}
	rangeHashes := nonEmptyLines(rawRange)
	expected := make([]string, len(verified))
	for i := range verified {
		expected[len(verified)-1-i] = verified[i]
	}
	if strings.Join(rangeHashes, "\n") != strings.Join(expected, "\n") {
		return nil, fmt.Errorf("selected commits must form one contiguous first-parent range")
	}
	return map[string]any{
		"mode":     "branch_compare",
		"base_ref": parent,
		"head_ref": newest,
		"strategy": "direct",
	}, nil
}

func resolveAnalysisRef(ctx context.Context, root, raw string) (string, string, error) {
	ref, err := cleanRevision(raw)
	if err != nil {
		return "", "", err
	}
	if sha, verifyErr := verifyCommit(ctx, root, ref); verifyErr == nil {
		return ref, sha, nil
	}
	if !strings.HasPrefix(ref, "origin/") {
		remote := "origin/" + ref
		if sha, verifyErr := verifyCommit(ctx, root, remote); verifyErr == nil {
			return remote, sha, nil
		}
	}
	return "", "", fmt.Errorf("ref %q does not resolve to a commit", raw)
}

func resolveRemoteAnalysisRef(ctx context.Context, root, raw string) (string, string, error) {
	ref := strings.TrimSpace(raw)
	ref = strings.TrimPrefix(ref, "refs/remotes/origin/")
	ref = strings.TrimPrefix(ref, "origin/")
	branch, err := cleanBranch(ref)
	if err != nil {
		return "", "", err
	}
	remoteRef := "refs/remotes/origin/" + branch
	sha, err := verifyCommit(ctx, root, remoteRef)
	if err != nil {
		return "", "", fmt.Errorf("remote branch origin/%s does not resolve to a commit", branch)
	}
	return branch, sha, nil
}

func resolveLockedAnalysisRef(ctx context.Context, root, label string, locked any) (string, string, error) {
	lockedSHA := strings.TrimSpace(fmt.Sprint(locked))
	if lockedSHA == "" || lockedSHA == "<nil>" {
		return resolveAnalysisRef(ctx, root, label)
	}
	sha, err := verifyCommit(ctx, root, lockedSHA)
	if err != nil {
		return "", "", fmt.Errorf("locked ref %q is no longer available: %w", label, err)
	}
	return label, sha, nil
}

func nonEmptyLines(raw string) []string {
	lines := make([]string, 0)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
