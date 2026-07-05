#!/usr/bin/env bash
set -euo pipefail

repo="${FIXFORGE_CLIENT_GITHUB_REPO:-HobbyBear/fixforge-client}"
version="${FIXFORGE_CLIENT_VERSION:-latest}"
install_dir="${FIXFORGE_CLIENT_INSTALL_DIR:-$HOME/.local/bin}"

usage() {
  cat <<'USAGE'
Install FixForge Client from GitHub Releases.

Usage:
  install.sh [--repo owner/fixforge-client] [--version v0.1.0] [--install-dir DIR] [client args...]

Examples:
  curl -fsSL https://github.com/HobbyBear/fixforge-client/releases/latest/download/install.sh | bash
  curl -fsSL https://github.com/HobbyBear/fixforge-client/releases/latest/download/install.sh | bash -s -- connect --server http://localhost:8080 --token xxx --project-name demo --local-path .
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      repo="${2:?missing value for --repo}"
      shift 2
      ;;
    --version)
      version="${2:?missing value for --version}"
      shift 2
      ;;
    --install-dir)
      install_dir="${2:?missing value for --install-dir}"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      break
      ;;
  esac
done

normalize_repo() {
  local value="$1"
  value="${value#https://github.com/}"
  value="${value#http://github.com/}"
  value="${value#git@github.com:}"
  value="${value%.git}"
  value="${value#/}"
  value="${value%/}"
  printf '%s' "$value"
}

detect_os() {
  case "$(uname -s)" in
    Linux) printf 'linux' ;;
    Darwin) printf 'darwin' ;;
    *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64' ;;
    arm64|aarch64) printf 'arm64' ;;
    *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
  esac
}

latest_version() {
  curl -fsSL \
    -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/$repo/releases/latest" |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
    head -n 1
}

checksum_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  else
    shasum -a 256 "$file" | awk '{print $1}'
  fi
}

path_expr() {
  local dir="$1"
  case "$dir" in
    "$HOME"/*) printf '$HOME%s' "${dir#"$HOME"}" ;;
    *) printf '%s' "$dir" ;;
  esac
}

path_profile_files() {
  local shell_name="${SHELL##*/}"
  local files=()
  add_file() {
    local existing
    for existing in "${files[@]}"; do
      [[ "$existing" == "$1" ]] && return
    done
    files+=("$1")
  }
  case "$shell_name" in
    zsh)
      add_file "$HOME/.zshrc"
      add_file "$HOME/.zprofile"
      ;;
    bash)
      add_file "$HOME/.bashrc"
      add_file "$HOME/.profile"
      ;;
    *)
      add_file "$HOME/.profile"
      ;;
  esac
  if [[ "$(uname -s)" == "Darwin" ]]; then
    add_file "$HOME/.zshrc"
    add_file "$HOME/.zprofile"
  fi
  printf '%s\n' "${files[@]}"
}

ensure_path() {
  local dir="$1"
  local expr
  expr="$(path_expr "$dir")"
  case ":$PATH:" in
    *":$dir:"*) ;;
    *) export PATH="$dir:$PATH" ;;
  esac

  local updated=""
  while IFS= read -r profile; do
    [[ -n "$profile" ]] || continue
    mkdir -p "$(dirname "$profile")"
    touch "$profile"
    if grep -F "$dir" "$profile" >/dev/null 2>&1 || grep -F "$expr" "$profile" >/dev/null 2>&1; then
      continue
    fi
    printf '\n# FixForge Client\nexport PATH="%s:$PATH"\n' "$expr" >> "$profile"
    updated="$updated $profile"
  done < <(path_profile_files)

  if [[ -n "$updated" ]]; then
    echo "Added $dir to PATH in:${updated}"
    echo "Restart your terminal, or run: source ${updated##* }"
  fi
}

repo="$(normalize_repo "$repo")"
if [[ "$version" == "latest" ]]; then
  version="$(latest_version)"
fi
if [[ -z "$version" ]]; then
  echo "failed to resolve fixforge-client version" >&2
  exit 1
fi

os="$(detect_os)"
arch="$(detect_arch)"
asset="fixforge-client_${version}_${os}_${arch}.tar.gz"
base_url="https://github.com/$repo/releases/download/$version"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

archive="$tmp_dir/$asset"
echo "Downloading $asset from $repo..."
curl -fL "$base_url/$asset" -o "$archive"

if curl -fsSL "$base_url/checksums.txt" -o "$tmp_dir/checksums.txt"; then
  expected="$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset {print $1}' "$tmp_dir/checksums.txt" | head -n 1)"
  if [[ -n "$expected" ]]; then
    actual="$(checksum_file "$archive")"
    if [[ "$actual" != "$expected" ]]; then
      echo "checksum mismatch for $asset" >&2
      exit 1
    fi
  fi
fi

tar -xzf "$archive" -C "$tmp_dir"
mkdir -p "$install_dir"
install -m 0755 "$tmp_dir/fixforge-client" "$install_dir/fixforge-client"

echo "Installed fixforge-client $version to $install_dir/fixforge-client"
ensure_path "$install_dir"

if [[ $# -gt 0 ]]; then
  "$install_dir/fixforge-client" "$@"
fi
