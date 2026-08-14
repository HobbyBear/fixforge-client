package gitops

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestCreateAnalysisSnapshotHasDetachedHeadAndNoRemoteOrSourceRef(t *testing.T) {
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
	if err := os.WriteFile(trackedPath, []byte("snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	comparison, err := ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{Mode: "working_tree"})
	if err != nil {
		t.Fatal(err)
	}
	refsBefore := analysisGit(t, repo, "show-ref")
	storeRoot := t.TempDir()
	materialized, err := CreateAnalysisSnapshot(context.Background(), repo, storeRoot, "cca_snapshot_test", comparison)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot, err := ResolveAnalysisSnapshotRoot(storeRoot, materialized)
	if err != nil {
		t.Fatal(err)
	}
	if refsAfter := analysisGit(t, repo, "show-ref"); refsAfter != refsBefore {
		t.Fatalf("source refs changed:\nbefore=%s\nafter=%s", refsBefore, refsAfter)
	}
	if remote := analysisGit(t, snapshotRoot, "remote"); remote != "" {
		t.Fatalf("snapshot remote = %q", remote)
	}
	if branch := analysisGit(t, snapshotRoot, "branch", "--show-current"); branch != "" {
		t.Fatalf("snapshot branch = %q", branch)
	}
	if materialized["mode"] != "branch_compare" || materialized["source_mode"] != "working_tree" {
		t.Fatalf("materialized comparison mode = %#v", materialized)
	}
	if _, err := os.Stat(filepath.Join(snapshotRoot, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("snapshot still depends on source object store: %v", err)
	}
	if commits := analysisGit(t, snapshotRoot, "rev-list", "--count", "HEAD"); commits != "2" {
		t.Fatalf("snapshot commit count = %q, want two materialized endpoints", commits)
	}
	if materialized["source_base_sha"] != comparison["base_sha"] || materialized["source_head_sha"] != comparison["head_sha"] {
		t.Fatalf("snapshot source identity = %#v, want %#v", materialized, comparison)
	}
	content, err := os.ReadFile(filepath.Join(snapshotRoot, "tracked.txt"))
	if err != nil || strings.ReplaceAll(string(content), "\r\n", "\n") != "snapshot\n" {
		t.Fatalf("snapshot content = %q, err=%v", content, err)
	}
	if err := os.WriteFile(trackedPath, []byte("later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(filepath.Join(snapshotRoot, "tracked.txt"))
	if err != nil || strings.ReplaceAll(string(content), "\r\n", "\n") != "snapshot\n" {
		t.Fatalf("snapshot changed with source: content=%q err=%v", content, err)
	}
	if err := ValidateMaterializedAnalysisSnapshot(context.Background(), snapshotRoot, materialized); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	source, err := AnalysisSource(context.Background(), snapshotRoot, materialized, "tracked.txt")
	if err != nil || !strings.Contains(fmt.Sprint(source["content"]), "snapshot") {
		t.Fatalf("self-contained snapshot source = %#v, err=%v", source, err)
	}
	analysisGit(t, snapshotRoot, "remote", "add", "origin", repo)
	if err := ValidateMaterializedAnalysisSnapshot(context.Background(), snapshotRoot, materialized); err == nil || !strings.Contains(err.Error(), "remote") {
		t.Fatalf("snapshot remote validation error = %v", err)
	}
}

func TestCreateBranchAnalysisSnapshotSurvivesCheckoutAndSourceRemoval(t *testing.T) {
	repo := t.TempDir()
	analysisGit(t, repo, "init", "-q", "-b", "main")
	analysisGit(t, repo, "config", "user.email", "test@example.com")
	analysisGit(t, repo, "config", "user.name", "Test")
	path := filepath.Join(repo, "service.go")
	if err := os.WriteFile(path, []byte("package service\n\nconst Value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "service.go")
	analysisGit(t, repo, "commit", "-qm", "base")
	analysisGit(t, repo, "checkout", "-qb", "feature")
	if err := os.WriteFile(path, []byte("package service\n\nconst Value = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "commit", "-qam", "feature change")
	comparison, err := ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{
		Mode: "branches", BaseRef: "main", HeadRef: "feature", Strategy: "merge_base",
	})
	if err != nil {
		t.Fatal(err)
	}
	storeRoot := t.TempDir()
	materialized, err := CreateAnalysisSnapshot(context.Background(), repo, storeRoot, "cca_branch_snapshot", comparison)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot, err := ResolveAnalysisSnapshotRoot(storeRoot, materialized)
	if err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "checkout", "main")
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	contextJSON, err := AnalysisContext(context.Background(), snapshotRoot, materialized)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contextJSON, "+const Value = 2") || !strings.Contains(contextJSON, "feature change") {
		t.Fatalf("materialized branch context lost locked evidence: %s", contextJSON)
	}
	if materialized["source_strategy"] != "merge_base" || materialized["strategy"] != "direct" {
		t.Fatalf("snapshot strategy metadata = %#v", materialized)
	}
	if commits := analysisGit(t, snapshotRoot, "rev-list", "--count", "HEAD"); commits != "2" {
		t.Fatalf("snapshot commit count = %q, want two materialized endpoints", commits)
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

func TestWorkingTreeAnalysisFingerprintProtectsSourceSnapshot(t *testing.T) {
	repo := t.TempDir()
	analysisGit(t, repo, "init", "-q", "-b", "main")
	analysisGit(t, repo, "config", "user.email", "test@example.com")
	analysisGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc value() int { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "main.go")
	analysisGit(t, repo, "commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc value() int { return 2 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	comparison, err := ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{Mode: "working_tree"})
	if err != nil {
		t.Fatal(err)
	}
	if comparison["snapshot_fingerprint"] == "" {
		t.Fatalf("comparison has no snapshot fingerprint: %#v", comparison)
	}
	if err := ValidateAnalysisSnapshot(context.Background(), repo, comparison); err != nil {
		t.Fatalf("fresh snapshot rejected: %v", err)
	}
	source, err := AnalysisSource(context.Background(), repo, comparison, "main.go")
	if err != nil || !strings.Contains(source["content"].(string), "return 2") {
		t.Fatalf("source = %#v, err = %v", source, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc value() int { return 3 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAnalysisSnapshot(context.Background(), repo, comparison); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed snapshot validation error = %v", err)
	}
}

func TestAnalysisSourceReadsDeletedFileFromLockedBaseline(t *testing.T) {
	repo := t.TempDir()
	analysisGit(t, repo, "init", "-q", "-b", "main")
	analysisGit(t, repo, "config", "user.email", "test@example.com")
	analysisGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "removed.go"), []byte("package removed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "removed.go")
	analysisGit(t, repo, "commit", "-qm", "base")
	if err := os.Remove(filepath.Join(repo, "removed.go")); err != nil {
		t.Fatal(err)
	}
	comparison, err := ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{Mode: "working_tree"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := AnalysisSource(context.Background(), repo, comparison, "removed.go")
	if err != nil || !strings.Contains(source["content"].(string), "package removed") {
		t.Fatalf("source = %#v, err = %v", source, err)
	}
}

func TestResolveWorkingTreeAnalysisLocksRenamedDestination(t *testing.T) {
	repo := t.TempDir()
	analysisGit(t, repo, "init", "-q", "-b", "main")
	analysisGit(t, repo, "config", "user.email", "test@example.com")
	analysisGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "old.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysisGit(t, repo, "add", "old.txt")
	analysisGit(t, repo, "commit", "-qm", "base")
	analysisGit(t, repo, "mv", "old.txt", "new.txt")

	comparison, err := ResolveAnalysisSelection(context.Background(), repo, AnalysisSelection{Mode: "working_tree"})
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := comparison["changed_paths"].([]string)
	if len(paths) != 1 || paths[0] != "new.txt" {
		t.Fatalf("changed paths = %#v, want new.txt", paths)
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
