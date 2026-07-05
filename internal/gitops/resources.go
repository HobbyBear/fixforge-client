package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type BranchOption struct {
	Name    string `json:"name"`
	Current bool   `json:"current,omitempty"`
	Local   bool   `json:"local,omitempty"`
	Remote  bool   `json:"remote,omitempty"`
}

type HistoryEntry struct {
	Hash     string `json:"hash"`
	Short    string `json:"short"`
	Author   string `json:"author"`
	Relative string `json:"relative"`
	Subject  string `json:"subject"`
}

type StashEntry struct {
	Ref      string           `json:"ref"`
	Hash     string           `json:"hash"`
	Relative string           `json:"relative"`
	Subject  string           `json:"subject"`
	Files    []map[string]any `json:"files"`
}

func Branches(ctx context.Context, root string) (map[string]any, error) {
	_, _ = gitOutput(ctx, root, "fetch", "origin", "--prune")
	current, _ := gitOutput(ctx, root, "branch", "--show-current")
	current = strings.TrimSpace(current)
	localOut, _ := gitOutput(ctx, root, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	remoteOut, _ := gitOutput(ctx, root, "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin")

	seen := map[string]*BranchOption{}
	add := func(name string, local, remote bool) {
		name = strings.TrimSpace(strings.TrimPrefix(name, "origin/"))
		if name == "" || name == "HEAD" || strings.Contains(name, " -> ") {
			return
		}
		item := seen[name]
		if item == nil {
			item = &BranchOption{Name: name}
			seen[name] = item
		}
		item.Local = item.Local || local
		item.Remote = item.Remote || remote
		item.Current = item.Current || name == current
	}
	for _, line := range strings.Split(localOut, "\n") {
		add(line, true, false)
	}
	for _, line := range strings.Split(remoteOut, "\n") {
		add(line, false, true)
	}
	if current != "" {
		add(current, true, false)
	}

	branches := make([]string, 0, len(seen))
	for name := range seen {
		branches = append(branches, name)
	}
	sort.Strings(branches)
	options := make([]BranchOption, 0, len(branches))
	for _, name := range branches {
		options = append(options, *seen[name])
	}
	return map[string]any{"branches": branches, "current_branch": current, "branch_options": options}, nil
}

func Status(ctx context.Context, root string) (map[string]any, error) {
	branches, err := Branches(ctx, root)
	if err != nil {
		return nil, err
	}
	current, _ := currentBranch(ctx, root)
	upstream, _ := gitOutput(ctx, root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	upstream = strings.TrimSpace(upstream)
	if upstream == "" && current != "" && branchRefExists(ctx, root, "refs/remotes/origin/"+current) {
		upstream = "origin/" + current
	}
	ahead, behind := 0, 0
	if upstream != "" {
		if out, err := gitOutput(ctx, root, "rev-list", "--left-right", "--count", "HEAD..."+upstream); err == nil {
			fields := strings.Fields(out)
			if len(fields) >= 2 {
				ahead = atoi(fields[0])
				behind = atoi(fields[1])
			}
		}
	}
	entries, _ := ChangedFiles(ctx, root)
	insertions, deletions := diffLineStats(ctx, root)
	payload := map[string]any{
		"object":         "git.status",
		"ok":             true,
		"current_branch": current,
		"upstream":       upstream,
		"ahead":          ahead,
		"behind":         behind,
		"has_remote":     upstream != "",
		"changed_count":  len(entries),
		"insertions":     insertions,
		"deletions":      deletions,
	}
	for key, value := range branches {
		payload[key] = value
	}
	return payload, nil
}

func CheckoutBranch(ctx context.Context, root, branch string) (map[string]any, error) {
	branch, err := cleanBranch(branch)
	if err != nil {
		return nil, err
	}
	if err := checkoutBranch(ctx, root, branch); err != nil {
		return nil, err
	}
	return Branches(ctx, root)
}

func CreateBranch(ctx context.Context, root, branch string) (map[string]any, error) {
	branch, err := cleanBranch(branch)
	if err != nil {
		return nil, err
	}
	current, _ := gitOutput(ctx, root, "branch", "--show-current")
	if strings.TrimSpace(current) == branch {
		return nil, fmt.Errorf("branch %s is already current", branch)
	}
	if branchRefExists(ctx, root, "refs/heads/"+branch) {
		return nil, fmt.Errorf("local branch %s already exists", branch)
	}
	_, _ = gitOutput(ctx, root, "fetch", "origin", "--prune")
	if branchRefExists(ctx, root, "refs/remotes/origin/"+branch) {
		return nil, fmt.Errorf("remote branch origin/%s already exists; checkout it instead", branch)
	}
	if out, err := gitOutput(ctx, root, "checkout", "-b", branch); err != nil {
		return nil, fmt.Errorf("create branch %s failed: %w: %s", branch, err, strings.TrimSpace(out))
	}
	hash, _ := gitOutput(ctx, root, "rev-parse", "HEAD")
	return withBranchSnapshot(ctx, root, map[string]any{
		"object":         "git.create_branch_result",
		"ok":             true,
		"branch":         branch,
		"created_branch": branch,
		"current_branch": branch,
		"hash":           strings.TrimSpace(hash),
	}), nil
}

func CommitFiles(ctx context.Context, root string, files []string, message string) (map[string]any, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil, fmt.Errorf("commit message is required")
	}
	paths, err := cleanPaths(root, files)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("at least one file must be selected")
	}
	if out, err := gitOutput(ctx, root, append([]string{"add", "--"}, paths...)...); err != nil {
		return nil, fmt.Errorf("git add failed: %w: %s", err, strings.TrimSpace(out))
	}
	diffArgs := append([]string{"diff", "--cached", "--name-only", "--"}, paths...)
	out, err := gitOutput(ctx, root, diffArgs...)
	if err != nil {
		return nil, fmt.Errorf("git diff --cached failed: %w: %s", err, strings.TrimSpace(out))
	}
	if strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("selected files have no staged changes")
	}
	commitArgs := []string{
		"-c", "user.name=fixforge",
		"-c", "user.email=fixforge@local",
		"commit", "--no-verify", "-m", message, "--",
	}
	commitArgs = append(commitArgs, paths...)
	if out, err := gitOutput(ctx, root, commitArgs...); err != nil {
		return nil, fmt.Errorf("git commit failed: %w: %s", err, strings.TrimSpace(out))
	}
	hash, _ := gitOutput(ctx, root, "rev-parse", "HEAD")
	branch, _ := currentBranch(ctx, root)
	return map[string]any{
		"object": "git.commit_result",
		"ok":     true,
		"branch": branch,
		"hash":   strings.TrimSpace(hash),
		"files":  paths,
	}, nil
}

func AddFiles(ctx context.Context, root string, files []string) (map[string]any, error) {
	paths, err := cleanPaths(root, files)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("at least one file must be selected")
	}
	if out, err := gitOutput(ctx, root, append([]string{"add", "--"}, paths...)...); err != nil {
		return nil, fmt.Errorf("git add failed: %w: %s", err, strings.TrimSpace(out))
	}
	return withBranchSnapshot(ctx, root, map[string]any{
		"object": "git.add_result",
		"ok":     true,
		"files":  paths,
	}), nil
}

func RestoreFiles(ctx context.Context, root string, files []string) (map[string]any, error) {
	paths, err := cleanPaths(root, files)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("at least one file must be selected")
	}
	tracked := make([]string, 0, len(paths))
	skipped := make([]string, 0)
	for _, path := range paths {
		if pathExistsInHead(ctx, root, path) {
			tracked = append(tracked, path)
			continue
		}
		skipped = append(skipped, path)
	}
	if len(tracked) == 0 {
		return nil, fmt.Errorf("selected files are not tracked; delete untracked files instead")
	}
	if out, err := gitOutput(ctx, root, append([]string{"restore", "--staged", "--worktree", "--"}, tracked...)...); err != nil {
		return nil, fmt.Errorf("git restore failed: %w: %s", err, strings.TrimSpace(out))
	}
	return withBranchSnapshot(ctx, root, map[string]any{
		"object":  "git.restore_result",
		"ok":      true,
		"files":   tracked,
		"skipped": skipped,
	}), nil
}

func DeleteFiles(ctx context.Context, root string, files []string) (map[string]any, error) {
	paths, err := cleanPaths(root, files)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("at least one file must be selected")
	}
	deleted := make([]string, 0, len(paths))
	missing := make([]string, 0)
	for _, path := range paths {
		target := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(target)
		if os.IsNotExist(err) {
			missing = append(missing, path)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("delete %s failed: %w", path, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("delete only supports files; %s is a directory", path)
		}
		if err := os.Remove(target); err != nil {
			return nil, fmt.Errorf("delete %s failed: %w", path, err)
		}
		deleted = append(deleted, path)
	}
	if len(deleted) == 0 {
		return nil, fmt.Errorf("selected files are already missing")
	}
	return withBranchSnapshot(ctx, root, map[string]any{
		"object":  "git.delete_result",
		"ok":      true,
		"files":   deleted,
		"missing": missing,
	}), nil
}

func History(ctx context.Context, root string, limit int) (map[string]any, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	format := "%H%x1f%h%x1f%an%x1f%ar%x1f%s%x1e"
	out, err := gitOutput(ctx, root, "log", "-n", fmt.Sprint(limit), "--pretty=format:"+format)
	if err != nil {
		return nil, fmt.Errorf("git log failed: %w: %s", err, strings.TrimSpace(out))
	}
	entries := make([]HistoryEntry, 0)
	for _, raw := range strings.Split(out, "\x1e") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, "\x1f")
		if len(parts) < 5 {
			continue
		}
		entries = append(entries, HistoryEntry{
			Hash:     strings.TrimSpace(parts[0]),
			Short:    strings.TrimSpace(parts[1]),
			Author:   strings.TrimSpace(parts[2]),
			Relative: strings.TrimSpace(parts[3]),
			Subject:  strings.TrimSpace(parts[4]),
		})
	}
	return map[string]any{"object": "git.history", "ok": true, "data": entries}, nil
}

func PushBranch(ctx context.Context, root, branch string) (map[string]any, error) {
	var err error
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch, err = currentBranch(ctx, root)
		if err != nil {
			return nil, err
		}
	} else if branch, err = cleanBranch(branch); err != nil {
		return nil, err
	}
	if out, err := gitOutput(ctx, root, "push", "origin", branch); err != nil {
		return nil, fmt.Errorf("git push origin %s failed: %w: %s", branch, err, strings.TrimSpace(out))
	}
	hash, _ := gitOutput(ctx, root, "rev-parse", "HEAD")
	return map[string]any{
		"object": "git.push_result",
		"ok":     true,
		"branch": branch,
		"hash":   strings.TrimSpace(hash),
	}, nil
}

func PullBranch(ctx context.Context, root, branch string) (map[string]any, error) {
	var err error
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch, err = currentBranch(ctx, root)
		if err != nil {
			return nil, err
		}
	} else if branch, err = cleanBranch(branch); err != nil {
		return nil, err
	}
	current, err := currentBranch(ctx, root)
	if err != nil {
		return nil, err
	}
	if current != branch {
		if err := checkoutBranch(ctx, root, branch); err != nil {
			return nil, err
		}
	}
	if out, err := gitOutput(ctx, root, "pull", "--ff-only", "origin", branch); err != nil {
		return nil, fmt.Errorf("git pull --ff-only origin %s failed: %w: %s", branch, err, strings.TrimSpace(out))
	}
	hash, _ := gitOutput(ctx, root, "rev-parse", "HEAD")
	return withBranchSnapshot(ctx, root, map[string]any{
		"object":         "git.pull_result",
		"ok":             true,
		"branch":         branch,
		"current_branch": branch,
		"hash":           strings.TrimSpace(hash),
	}), nil
}

func StashChanges(ctx context.Context, root string, files []string, message string) (map[string]any, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "FixForge workspace stash"
	}
	paths, err := cleanPaths(root, files)
	if err != nil {
		return nil, err
	}
	args := []string{"stash", "push", "-u", "-m", message}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	out, err := gitOutput(ctx, root, args...)
	if err != nil {
		return nil, fmt.Errorf("git stash failed: %w: %s", err, strings.TrimSpace(out))
	}
	if strings.Contains(out, "No local changes to save") {
		return nil, fmt.Errorf("no local changes to stash")
	}
	hash, _ := gitOutput(ctx, root, "rev-parse", "stash@{0}")
	return withBranchSnapshot(ctx, root, map[string]any{
		"object":  "git.stash_result",
		"ok":      true,
		"message": message,
		"hash":    strings.TrimSpace(hash),
		"files":   paths,
	}), nil
}

func Stashes(ctx context.Context, root string, limit int) (map[string]any, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	format := "%gd%x1f%H%x1f%cr%x1f%s%x1e"
	out, err := gitOutput(ctx, root, "stash", "list", "-n", fmt.Sprint(limit), "--pretty=format:"+format)
	if err != nil {
		return nil, fmt.Errorf("git stash list failed: %w: %s", err, strings.TrimSpace(out))
	}
	entries := make([]StashEntry, 0)
	for _, raw := range strings.Split(out, "\x1e") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, "\x1f")
		if len(parts) < 4 {
			continue
		}
		ref := strings.TrimSpace(parts[0])
		entries = append(entries, StashEntry{
			Ref:      ref,
			Hash:     strings.TrimSpace(parts[1]),
			Relative: strings.TrimSpace(parts[2]),
			Subject:  strings.TrimSpace(parts[3]),
			Files:    stashFiles(ctx, root, ref),
		})
	}
	return map[string]any{"object": "git.stashes", "ok": true, "data": entries}, nil
}

func MergeBranch(ctx context.Context, root, sourceBranch string) (map[string]any, error) {
	sourceBranch, err := cleanBranch(sourceBranch)
	if err != nil {
		return nil, err
	}
	targetBranch, err := currentBranch(ctx, root)
	if err != nil {
		return nil, err
	}
	if sourceBranch == targetBranch {
		return nil, fmt.Errorf("source branch and current branch are both %s", sourceBranch)
	}
	mergeRef := sourceBranch
	if _, err := gitOutput(ctx, root, "rev-parse", "--verify", "refs/heads/"+sourceBranch); err != nil {
		if out, fetchErr := gitOutput(ctx, root, "fetch", "origin", sourceBranch); fetchErr != nil {
			return nil, fmt.Errorf("fetch origin %s failed: %w: %s", sourceBranch, fetchErr, strings.TrimSpace(out))
		}
		mergeRef = "origin/" + sourceBranch
	}
	if out, err := gitOutput(ctx, root, "merge", "--no-edit", mergeRef); err != nil {
		return nil, fmt.Errorf("git merge %s failed: %w: %s", mergeRef, err, strings.TrimSpace(out))
	}
	hash, _ := gitOutput(ctx, root, "rev-parse", "HEAD")
	return map[string]any{
		"object": "git.merge_result",
		"ok":     true,
		"source": sourceBranch,
		"target": targetBranch,
		"hash":   strings.TrimSpace(hash),
	}, nil
}

func MergeToBranch(ctx context.Context, root, targetBranch string) (map[string]any, error) {
	targetBranch, err := cleanBranch(targetBranch)
	if err != nil {
		return nil, err
	}
	sourceBranch, err := currentBranch(ctx, root)
	if err != nil {
		return nil, err
	}
	if sourceBranch == targetBranch {
		return nil, fmt.Errorf("source branch and target branch are both %s", sourceBranch)
	}
	if err := checkoutBranch(ctx, root, targetBranch); err != nil {
		return nil, err
	}
	if out, err := gitOutput(ctx, root, "merge", "--no-edit", sourceBranch); err != nil {
		return nil, fmt.Errorf("git merge %s into %s failed: %w: %s", sourceBranch, targetBranch, err, strings.TrimSpace(out))
	}
	hash, _ := gitOutput(ctx, root, "rev-parse", "HEAD")
	return withBranchSnapshot(ctx, root, map[string]any{
		"object":         "git.merge_result",
		"ok":             true,
		"source":         sourceBranch,
		"target":         targetBranch,
		"current_branch": targetBranch,
		"hash":           strings.TrimSpace(hash),
	}), nil
}

func ChangedFiles(ctx context.Context, root string) ([]map[string]any, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all").Output()
	if err != nil {
		return nil, err
	}
	return parseGitStatusZ(out), nil
}

func FileDiff(ctx context.Context, root, rel string) (map[string]any, error) {
	workPath, gitPath, err := ResolveWorktreePath(ctx, root, rel)
	if err != nil {
		return nil, err
	}
	gitRoot := root
	if topLevel, _ := gitTopLevelAndPrefix(ctx, root); topLevel != "" {
		gitRoot = topLevel
	}
	beforeBytes, beforeErr := exec.CommandContext(ctx, "git", "-C", gitRoot, "show", "HEAD:"+gitPath).Output()
	before := string(beforeBytes)
	afterBytes, afterErr := os.ReadFile(workPath)
	if afterErr != nil {
		if indexBytes, indexErr := exec.CommandContext(ctx, "git", "-C", gitRoot, "show", ":"+gitPath).Output(); indexErr == nil {
			afterBytes = indexBytes
			afterErr = nil
		}
	}
	rawBytes, rawErr := exec.CommandContext(ctx, "git", "-C", gitRoot, "diff", "--no-ext-diff", "--no-color", "HEAD", "--", gitPath).Output()

	var beforePtr *string
	if beforeErr == nil {
		beforePtr = &before
	}
	var afterPtr *string
	if afterErr == nil {
		s := string(afterBytes)
		afterPtr = &s
	}
	var rawPtr *string
	if rawErr == nil && len(rawBytes) > 0 {
		s := string(rawBytes)
		rawPtr = &s
	}
	return map[string]any{
		"object": "session.environment.filesystem.file_diff",
		"path":   gitPath,
		"before": beforePtr,
		"after":  afterPtr,
		"raw":    rawPtr,
	}, nil
}

func ResolveWorktreePath(ctx context.Context, root, rel string) (string, string, error) {
	paths, err := cleanPaths(root, []string{rel})
	if err != nil {
		return "", "", err
	}
	if len(paths) != 1 {
		return "", "", fmt.Errorf("path is required")
	}
	clean := paths[0]
	rootPath, err := safeAbsJoin(root, clean)
	if err != nil {
		return "", "", err
	}
	topLevel, prefix := gitTopLevelAndPrefix(ctx, root)
	if topLevel == "" {
		return rootPath, clean, nil
	}
	gitPath := clean
	if fileExists(rootPath) {
		if prefix != "" && !strings.HasPrefix(clean, prefix) {
			gitPath = filepath.ToSlash(filepath.Join(prefix, clean))
		}
		return rootPath, gitPath, nil
	}
	candidates := []string{clean}
	if prefix != "" && !strings.HasPrefix(clean, prefix) {
		candidates = append(candidates, filepath.ToSlash(filepath.Join(prefix, clean)))
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		candidate = cleanGitPath(candidate)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		target, err := safeAbsJoin(topLevel, candidate)
		if err != nil {
			continue
		}
		if fileExists(target) {
			return target, candidate, nil
		}
	}
	return rootPath, gitPath, nil
}

func cleanBranch(branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", fmt.Errorf("branch is required")
	}
	if strings.HasPrefix(branch, "-") ||
		strings.HasPrefix(branch, "/") ||
		strings.HasSuffix(branch, "/") ||
		strings.HasSuffix(branch, ".lock") ||
		strings.Contains(branch, "..") ||
		strings.Contains(branch, "//") ||
		strings.Contains(branch, "@{") ||
		strings.ContainsAny(branch, " \t\n\r~^:?*[\\") {
		return "", fmt.Errorf("invalid branch name %q", branch)
	}
	return branch, nil
}

func cleanPaths(root string, files []string) ([]string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(files))
	for _, file := range files {
		clean := strings.TrimSpace(filepath.ToSlash(file))
		clean = strings.TrimPrefix(filepath.Clean("/"+clean), string(filepath.Separator))
		clean = filepath.ToSlash(clean)
		if clean == "" || clean == "." {
			continue
		}
		targetAbs, err := filepath.Abs(filepath.Join(rootAbs, filepath.FromSlash(clean)))
		if err != nil {
			return nil, err
		}
		if targetAbs != rootAbs && !strings.HasPrefix(targetAbs, rootAbs+string(filepath.Separator)) {
			return nil, fmt.Errorf("path escapes workspace: %s", file)
		}
		if !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	sort.Strings(out)
	return out, nil
}

func safeAbsJoin(root, rel string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rel = cleanGitPath(rel)
	targetAbs, err := filepath.Abs(filepath.Join(rootAbs, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	if targetAbs != rootAbs && !strings.HasPrefix(targetAbs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return targetAbs, nil
}

func cleanGitPath(path string) string {
	clean := strings.TrimSpace(filepath.ToSlash(path))
	clean = strings.TrimPrefix(filepath.Clean("/"+clean), string(filepath.Separator))
	return filepath.ToSlash(clean)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func gitTopLevelAndPrefix(ctx context.Context, root string) (string, string) {
	out, err := gitOutput(ctx, root, "rev-parse", "--show-toplevel", "--show-prefix")
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	if len(lines) < 1 {
		return "", ""
	}
	topLevel := strings.TrimSpace(lines[0])
	if topLevel == "" {
		return "", ""
	}
	prefix := ""
	if len(lines) > 1 {
		prefix = cleanGitPath(lines[1])
		if prefix != "" {
			prefix = strings.TrimSuffix(prefix, "/") + "/"
		}
	}
	return topLevel, prefix
}

func currentBranch(ctx context.Context, root string) (string, error) {
	out, err := gitOutput(ctx, root, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		return "", fmt.Errorf("current checkout is detached; choose a branch first")
	}
	return branch, nil
}

func checkoutBranch(ctx context.Context, root, branch string) error {
	current, _ := gitOutput(ctx, root, "branch", "--show-current")
	if strings.TrimSpace(current) == branch {
		return nil
	}
	if branchRefExists(ctx, root, "refs/heads/"+branch) {
		if out, err := gitOutput(ctx, root, "checkout", branch); err != nil {
			return fmt.Errorf("checkout local branch %s failed: %w: %s", branch, err, strings.TrimSpace(out))
		}
		return nil
	}
	if out, err := gitOutput(ctx, root, "fetch", "origin", branch); err != nil {
		return fmt.Errorf("fetch origin %s failed: %w: %s", branch, err, strings.TrimSpace(out))
	}
	if out, err := gitOutput(ctx, root, "checkout", "-b", branch, "origin/"+branch); err != nil {
		return fmt.Errorf("checkout remote branch %s failed: %w: %s", branch, err, strings.TrimSpace(out))
	}
	return nil
}

func branchRefExists(ctx context.Context, root, ref string) bool {
	_, err := gitOutput(ctx, root, "rev-parse", "--verify", ref)
	return err == nil
}

func pathExistsInHead(ctx context.Context, root, path string) bool {
	_, err := gitOutput(ctx, root, "cat-file", "-e", "HEAD:"+path)
	return err == nil
}

func withBranchSnapshot(ctx context.Context, root string, result map[string]any) map[string]any {
	snapshot, err := Branches(ctx, root)
	if err != nil {
		return result
	}
	for key, value := range snapshot {
		result[key] = value
	}
	return result
}

func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func atoi(raw string) int {
	n := 0
	for _, r := range strings.TrimSpace(raw) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func diffLineStats(ctx context.Context, root string) (int, int) {
	out, err := gitOutput(ctx, root, "diff", "--numstat", "HEAD", "--")
	if err != nil {
		return 0, 0
	}
	insertions, deletions := 0, 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "-" || fields[1] == "-" {
			continue
		}
		insertions += atoi(fields[0])
		deletions += atoi(fields[1])
	}
	return insertions, deletions
}

func stashFiles(ctx context.Context, root, ref string) []map[string]any {
	if ref == "" {
		return nil
	}
	out, err := gitOutput(ctx, root, "stash", "show", "--name-status", "--include-untracked", "--format=", ref)
	if err != nil {
		return nil
	}
	files := make([]map[string]any, 0)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		code := strings.TrimSpace(fields[0])
		path := filepath.ToSlash(fields[len(fields)-1])
		if path == "" {
			continue
		}
		files = append(files, map[string]any{
			"id":         ref + ":" + path,
			"object":     "git.stash.file",
			"path":       path,
			"name":       filepath.Base(path),
			"type":       "file",
			"status":     statusFromGitCode(code),
			"git_status": code,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i]["path"].(string) < files[j]["path"].(string) })
	return files
}

func parseGitStatusZ(out []byte) []map[string]any {
	parts := bytes.Split(out, []byte{0})
	files := make([]map[string]any, 0, len(parts))
	for i := 0; i < len(parts); i++ {
		item := string(parts[i])
		if len(item) < 4 {
			continue
		}
		rawCode := item[:2]
		indexStatus := rawCode[0]
		worktreeStatus := rawCode[1]
		code := strings.TrimSpace(rawCode)
		p := filepath.ToSlash(strings.TrimSpace(item[3:]))
		if p == "" {
			continue
		}
		if indexStatus == 'R' || indexStatus == 'C' || worktreeStatus == 'R' || worktreeStatus == 'C' {
			i++
		}
		status := statusFromGitCode(rawCode)
		files = append(files, map[string]any{
			"id":         p,
			"object":     "session.environment.filesystem.entry",
			"path":       p,
			"name":       filepath.Base(p),
			"type":       "file",
			"status":     status,
			"git_status": code,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i]["path"].(string) < files[j]["path"].(string) })
	return files
}

func statusFromGitCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return "modified"
	}
	if code == "??" {
		return "untracked"
	}
	if strings.Contains(code, "D") {
		return "deleted"
	}
	if strings.ContainsAny(code, "RC") {
		return "renamed"
	}
	if strings.Contains(code, "A") || strings.Contains(code, "?") {
		return "added"
	}
	return "modified"
}
