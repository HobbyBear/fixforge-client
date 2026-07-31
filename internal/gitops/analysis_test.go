package gitops

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBranchAnalysisBuildsAuthoritativeContext(t *testing.T) {
	repo := t.TempDir()
	analysisGit(t, repo, "init", "-q", "-b", "main")
	analysisGit(t, repo, "config", "user.email", "test@example.com")
	analysisGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "value.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "value.txt")
	analysisGit(t, repo, "commit", "-qm", "base")
	analysisGit(t, repo, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(repo, "value.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "value.txt")
	analysisGit(t, repo, "commit", "-qm", "feature change")

	comparison, err := ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{
		Mode: "branches", BaseRef: "main", HeadRef: "feature", Strategy: "merge_base",
	})
	if err != nil {
		t.Fatal(err)
	}
	contextJSON, err := AnalysisContext(context.Background(), repo, comparison)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot AnalysisSnapshot
	if err := json.Unmarshal([]byte(contextJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Comparison["base_ref"] != "main" || snapshot.Comparison["head_ref"] != "feature" {
		t.Fatalf("unexpected comparison context: %#v", snapshot.Comparison)
	}
	if !strings.Contains(snapshot.Diff, "-base") || !strings.Contains(snapshot.Diff, "+feature") {
		t.Fatalf("context does not contain the authoritative diff: %s", snapshot.Diff)
	}
	if !strings.Contains(snapshot.Commits, "feature change") {
		t.Fatalf("context does not contain comparison commits: %s", snapshot.Commits)
	}
}

func TestResolveCommitAnalysisRequiresContiguousRange(t *testing.T) {
	repo := t.TempDir()
	analysisGit(t, repo, "init", "-q", "-b", "master")
	analysisGit(t, repo, "config", "user.email", "test@example.com")
	analysisGit(t, repo, "config", "user.name", "Test")
	hashes := make([]string, 0, 4)
	for i, value := range []string{"one", "two", "three", "four"} {
		if err := os.WriteFile(filepath.Join(repo, "value.txt"), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		analysisGit(t, repo, "add", "value.txt")
		analysisGit(t, repo, "commit", "-qm", value)
		hashes = append(hashes, analysisGit(t, repo, "rev-parse", "HEAD"))
		_ = i
	}

	comparison, err := ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{
		Mode: "commits", CommitHashes: []string{hashes[3], hashes[2]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if comparison["base_ref"] != hashes[1] || comparison["head_ref"] != hashes[3] || comparison["strategy"] != "direct" {
		t.Fatalf("unexpected comparison: %#v", comparison)
	}

	_, err = ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{
		Mode: "commits", CommitHashes: []string{hashes[3], hashes[1]},
	})
	if err == nil || !strings.Contains(err.Error(), "contiguous") {
		t.Fatalf("expected contiguous range error, got %v", err)
	}
}

func TestResolveWorkingTreeAnalysisIsReadOnlyAndIgnoresUntrackedFiles(t *testing.T) {
	repo := t.TempDir()
	analysisGit(t, repo, "init", "-q", "-b", "main")
	analysisGit(t, repo, "config", "user.email", "test@example.com")
	analysisGit(t, repo, "config", "user.name", "Test")
	trackedPath := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(trackedPath, []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "tracked.txt")
	analysisGit(t, repo, "commit", "-qm", "base")
	if err := os.WriteFile(trackedPath, []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "tracked.txt")
	if err := os.WriteFile(trackedPath, []byte("worktree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	headBefore := analysisGit(t, repo, "rev-parse", "HEAD")
	statusBefore := analysisGit(t, repo, "status", "--porcelain=v1")
	indexBefore := analysisGit(t, repo, "diff", "--cached")
	comparison, err := ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{Mode: "working_tree"})
	if err != nil {
		t.Fatal(err)
	}
	if comparison["mode"] != "working_tree" || comparison["base_ref"] != headBefore || comparison["head_ref"] != "WORKTREE" {
		t.Fatalf("unexpected working tree comparison: %#v", comparison)
	}
	if got := analysisGit(t, repo, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD changed from %q to %q", headBefore, got)
	}
	if got := analysisGit(t, repo, "status", "--porcelain=v1"); got != statusBefore {
		t.Fatalf("working tree status changed:\nbefore: %s\nafter: %s", statusBefore, got)
	}
	if got := analysisGit(t, repo, "diff", "--cached"); got != indexBefore {
		t.Fatalf("index changed:\nbefore: %s\nafter: %s", indexBefore, got)
	}
}

func TestResolveWorkingTreeAnalysisRejectsOnlyUntrackedFiles(t *testing.T) {
	repo := t.TempDir()
	analysisGit(t, repo, "init", "-q", "-b", "main")
	analysisGit(t, repo, "config", "user.email", "test@example.com")
	analysisGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "tracked.txt")
	analysisGit(t, repo, "commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{Mode: "working_tree"})
	if err == nil || !strings.Contains(err.Error(), "no tracked uncommitted changes") {
		t.Fatalf("expected tracked changes error, got %v", err)
	}
}

func analysisGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
