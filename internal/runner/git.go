package runner

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// matchNoChanges checks both git phrasing variants for "no changes to commit".
func matchNoChanges(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "nothing to commit") ||
		strings.Contains(lower, "nothing added to commit")
}

// GitOps wraps git operations within a project's local clone.
type GitOps struct {
	repoPath string
	logger   *slog.Logger
}

// NewGitOps creates a new GitOps.
func NewGitOps(repoPath string, logger *slog.Logger) *GitOps {
	return &GitOps{repoPath: repoPath, logger: logger}
}

func (g *GitOps) runGit(args ...string) (string, error) {
	cmdLine := "git -C " + g.repoPath + " " + strings.Join(args, " ")
	g.logger.Info("[git] " + cmdLine)
	cmd := exec.Command("git", append([]string{"-C", g.repoPath}, args...)...)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if err != nil {
		return outStr, fmt.Errorf("%s: %w\n%s", cmdLine, err, outStr)
	}
	if outStr != "" {
		g.logger.Info("[git] " + cmdLine + " → " + truncate(outStr, 200))
	}
	return outStr, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Fetch fetches the latest from all remotes.
func (g *GitOps) Fetch() error {
	_, err := g.runGit("fetch", "--all", "--prune")
	return err
}

func (g *GitOps) CurrentBranch() string {
	if out, err := g.runGit("branch", "--show-current"); err == nil && strings.TrimSpace(out) != "" {
		return strings.TrimSpace(out)
	}
	if out, err := g.runGit("rev-parse", "--abbrev-ref", "HEAD"); err == nil && strings.TrimSpace(out) != "" {
		return strings.TrimSpace(out)
	}
	return ""
}

// WorktreesDir returns the .fixforge/worktrees directory path.
func (g *GitOps) WorktreesDir() string {
	return filepath.Join(g.repoPath, ".fixforge", "worktrees")
}

func (g *GitOps) CreateRunWorktree(run CloudRun) (string, string, error) {
	if err := g.Fetch(); err != nil {
		return "", "", err
	}
	wtPath := filepath.Join(g.WorktreesDir(), safePathName(run.RunID))
	runBranch := fmt.Sprintf("fixforge/%s/%s-run-%03d", safeBranchPart(run.TaskID), safeBranchPart(run.Stage), run.RunNumber)
	if run.RunNumber <= 0 {
		runBranch = fmt.Sprintf("fixforge/%s/%s", safeBranchPart(run.TaskID), safeBranchPart(run.RunID))
	}
	g.runGit("worktree", "prune")
	if existing, _ := g.findWorktree(runBranch); existing != "" {
		return "", "", fmt.Errorf("run branch %s already has worktree %s", runBranch, existing)
	}
	baseRef := "origin/" + run.FeatureBranch
	if strings.TrimSpace(run.FeatureBranch) == "" || !g.remoteRefExists(baseRef) {
		baseRef = "origin/" + run.BaseBranch
	}
	if strings.TrimSpace(baseRef) == "origin/" || !g.remoteRefExists(baseRef) {
		return "", "", fmt.Errorf("neither feature branch %q nor base branch %q exists on origin", run.FeatureBranch, run.BaseBranch)
	}
	if err := os.MkdirAll(g.WorktreesDir(), 0o755); err != nil {
		return "", "", fmt.Errorf("create worktrees dir: %w", err)
	}
	if _, err := g.runGit("worktree", "add", "-b", runBranch, wtPath, baseRef); err != nil {
		return "", "", fmt.Errorf("create worktree: %w", err)
	}
	if err := g.overlayLocalWorkingCopy(wtPath); err != nil {
		return "", "", fmt.Errorf("overlay local working copy: %w", err)
	}
	return wtPath, runBranch, nil
}

func (g *GitOps) overlayLocalWorkingCopy(worktreePath string) error {
	paths := map[string]bool{}
	for _, args := range [][]string{
		{"diff", "--name-only", "-z", "HEAD", "--"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
		{"ls-files", "--others", "--ignored", "--exclude-standard", "-z"},
	} {
		items, err := g.gitPathList(args...)
		if err != nil {
			return err
		}
		for _, item := range items {
			if shouldOverlayLocalPath(item) {
				paths[item] = true
			}
		}
	}
	for rel := range paths {
		src := filepath.Join(g.repoPath, filepath.FromSlash(rel))
		dst := filepath.Join(worktreePath, filepath.FromSlash(rel))
		if err := copyLocalOverlayFile(src, dst); err != nil {
			return err
		}
	}
	if len(paths) > 0 {
		g.logger.Info("[git] overlaid local working copy files", "count", len(paths), "worktree", worktreePath)
	}
	return nil
}

func (g *GitOps) gitPathList(args ...string) ([]string, error) {
	cmdLine := "git -C " + g.repoPath + " " + strings.Join(args, " ")
	g.logger.Info("[git] " + cmdLine)
	cmd := exec.Command("git", append([]string{"-C", g.repoPath}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmdLine, err)
	}
	parts := strings.Split(string(out), "\x00")
	var paths []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			paths = append(paths, filepath.ToSlash(part))
		}
	}
	return paths, nil
}

func shouldOverlayLocalPath(rel string) bool {
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	if rel == "" {
		return false
	}
	first := rel
	if idx := strings.IndexByte(rel, '/'); idx >= 0 {
		first = rel[:idx]
	}
	return first != ".git" && first != ".fixforge"
}

func copyLocalOverlayFile(src, dst string) error {
	info, err := os.Lstat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		_ = os.Remove(dst)
		return os.Symlink(target, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode().Perm())
}

func (g *GitOps) remoteRefExists(ref string) bool {
	_, err := g.runGit("rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

func (g *GitOps) findWorktree(branch string) (string, error) {
	out, err := g.runGit("worktree", "list")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "["+branch+"]") {
			return strings.Fields(strings.TrimSpace(line))[0], nil
		}
	}
	return "", nil
}

// CommitAll stages and commits in the worktree. Returns empty hash if nothing changed.
func (g *GitOps) CommitAll(worktreePath, message string) (string, error) {
	cmd := func(args ...string) (string, error) {
		a := append([]string{"-C", worktreePath}, args...)
		cmdLine := "git " + strings.Join(a, " ")
		g.logger.Info("[git] " + cmdLine)
		c := exec.Command("git", a...)
		out, e := c.CombinedOutput()
		s := strings.TrimSpace(string(out))
		if e != nil {
			return s, fmt.Errorf("%s: %w\n%s", cmdLine, e, s)
		}
		if s != "" {
			g.logger.Info("[git] " + cmdLine + " → " + truncate(s, 200))
		}
		return s, nil
	}

	if _, err := cmd("add", "-A"); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}

	// Check if there's anything to commit. `git diff --quiet` exits with 1
	// when changes exist, so use name-only output to avoid treating changes as
	// command failure.
	out, err := cmd("diff", "--cached", "--name-only")
	if err != nil {
		return "", fmt.Errorf("git diff --cached: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return "", nil
	}

	out, err = cmd("commit", "-m", message)
	if err != nil {
		if matchNoChanges(out) {
			return "", nil
		}
		return "", fmt.Errorf("git commit: %w", err)
	}

	out, err = cmd("rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return out, nil
}

// DiffStat returns the diff stat.
func (g *GitOps) DiffStat(worktreePath string) (string, error) {
	cmd := exec.Command("git", "-C", worktreePath, "diff", "--stat", "HEAD")
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out)), nil
}

func (g *GitOps) ChangedFiles(worktreePath string) string {
	cmd := exec.Command("git", "-C", worktreePath, "diff", "--name-only", "HEAD")
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func (g *GitOps) DiffSummary(worktreePath string) string {
	cmd := exec.Command("git", "-C", worktreePath, "diff", "--stat", "--summary", "HEAD")
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func (g *GitOps) MergeBranchToFeature(runBranch, featureBranch, message string) (string, error) {
	g.runGit("fetch", "origin", featureBranch)

	if _, err := g.runGit("checkout", featureBranch); err != nil {
		g.logger.Info("[git] creating feature branch from origin/" + featureBranch)
		if _, err := g.runGit("checkout", "-b", featureBranch, "origin/"+featureBranch); err != nil {
			return "", fmt.Errorf("checkout %s: %w", featureBranch, err)
		}
	}

	g.runGit("pull", "origin", featureBranch)

	if _, err := g.runGit("merge", "--squash", runBranch); err != nil {
		g.runGit("merge", "--abort")
		g.runGit("reset", "--hard", "HEAD")
		return "", fmt.Errorf("merge --squash %s: %w", runBranch, err)
	}

	out, err := g.runGit("commit", "-m", message)
	if err != nil {
		if matchNoChanges(out) {
			g.runGit("reset", "HEAD")
			g.logger.Info("[git] nothing to merge — task branch has no new changes relative to target")
			return "", nil
		}
		return "", fmt.Errorf("commit merge: %w", err)
	}

	hash, _ := g.runGit("rev-parse", "HEAD")
	g.logger.Info("[git] merge complete", "feature", featureBranch, "hash", hash, "from", runBranch)
	return hash, nil
}

func safePathName(s string) string {
	return strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-").Replace(strings.TrimSpace(s))
}

func safeBranchPart(s string) string {
	s = strings.Trim(strings.TrimSpace(s), "/")
	if s == "" {
		return "run"
	}
	replacer := strings.NewReplacer(" ", "-", "\\", "-", ":", "-", "~", "-", "^", "-", "?", "-", "*", "-", "[", "-", "]", "-")
	return replacer.Replace(s)
}
