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

func TestResolveRemoteBranchAnalysisLocksFetchedSHAs(t *testing.T) {
	repo := t.TempDir()
	analysisGit(t, repo, "init", "-q", "-b", "main")
	analysisGit(t, repo, "config", "user.email", "test@example.com")
	analysisGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "value.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "value.txt")
	analysisGit(t, repo, "commit", "-qm", "base")
	baseSHA := analysisGit(t, repo, "rev-parse", "HEAD")
	analysisGit(t, repo, "update-ref", "refs/remotes/origin/main", baseSHA)

	analysisGit(t, repo, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(repo, "value.txt"), []byte("local feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "value.txt")
	analysisGit(t, repo, "commit", "-qm", "local feature")
	localFeatureSHA := analysisGit(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "value.txt"), []byte("remote feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "value.txt")
	analysisGit(t, repo, "commit", "-qm", "remote feature")
	remoteFeatureSHA := analysisGit(t, repo, "rev-parse", "HEAD")
	analysisGit(t, repo, "update-ref", "refs/remotes/origin/feature", remoteFeatureSHA)
	analysisGit(t, repo, "reset", "--hard", localFeatureSHA)

	comparison, err := ResolveRemoteAnalysisSelection(context.Background(), repo, AnalysisSelection{
		Mode: "branches", BaseRef: "main", HeadRef: "feature", Strategy: "merge_base",
	})
	if err != nil {
		t.Fatal(err)
	}
	if comparison["base_ref"] != "main" || comparison["head_ref"] != "feature" || comparison["base_sha"] != baseSHA || comparison["head_sha"] != remoteFeatureSHA {
		t.Fatalf("unexpected locked remote comparison: %#v", comparison)
	}

	// A later fetch may move the tracking ref, but this analysis must keep the
	// snapshot selected at task start.
	analysisGit(t, repo, "update-ref", "refs/remotes/origin/feature", localFeatureSHA)
	contextJSON, err := AnalysisContext(context.Background(), repo, comparison)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot AnalysisSnapshot
	if err := json.Unmarshal([]byte(contextJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Comparison["head_sha"] != remoteFeatureSHA || !strings.Contains(snapshot.Diff, "+remote feature") {
		t.Fatalf("analysis did not use locked remote SHA: comparison=%#v diff=%s", snapshot.Comparison, snapshot.Diff)
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
	if got := strings.Join(comparison["changed_paths"].([]string), "\n"); got != "tracked.txt" {
		t.Fatalf("changed paths = %q", got)
	}
	if _, exists := comparison["untracked_paths"]; exists {
		t.Fatalf("untracked paths must not be part of comparison: %#v", comparison)
	}
	if _, exists := comparison["include_untracked"]; exists {
		t.Fatalf("untracked inclusion flag must not be present: %#v", comparison)
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

	statusBefore := analysisGit(t, repo, "status", "--porcelain=v1")
	_, err := ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{Mode: "working_tree"})
	if err == nil || !strings.Contains(err.Error(), "no tracked uncommitted changes") {
		t.Fatalf("expected tracked changes error, got %v", err)
	}
	if got := analysisGit(t, repo, "status", "--porcelain=v1"); got != statusBefore {
		t.Fatalf("working tree status changed:\nbefore: %s\nafter: %s", statusBefore, got)
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
