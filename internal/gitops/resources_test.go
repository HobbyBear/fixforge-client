package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitFilesCommitsOnlySelectedPaths(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	writeFile(t, repo, "a.txt", "one\n")
	writeFile(t, repo, "b.txt", "one\n")
	runGit(t, repo, "add", "--", "a.txt", "b.txt")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	writeFile(t, repo, "a.txt", "two\n")
	writeFile(t, repo, "b.txt", "two\n")
	result, err := CommitFiles(context.Background(), repo, []string{"a.txt"}, "update a")
	if err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	if strings.TrimSpace(result["hash"].(string)) == "" {
		t.Fatalf("CommitFiles returned empty hash: %#v", result)
	}

	status := runGit(t, repo, "status", "--porcelain")
	if !strings.Contains(status, " M b.txt") {
		t.Fatalf("expected b.txt to remain modified, status:\n%s", status)
	}
	if strings.Contains(status, "a.txt") {
		t.Fatalf("expected a.txt to be committed, status:\n%s", status)
	}
}

func TestCreateBranchAndMergeToBranch(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	runGit(t, repo, "init")
	writeFile(t, repo, "base.txt", "base\n")
	runGit(t, repo, "add", "--", "base.txt")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	baseBranch := strings.TrimSpace(runGit(t, repo, "branch", "--show-current"))
	if baseBranch == "" {
		t.Fatal("expected initial branch")
	}

	created, err := CreateBranch(ctx, repo, "feature/test")
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if created["current_branch"] != "feature/test" {
		t.Fatalf("CreateBranch current_branch = %#v", created["current_branch"])
	}
	writeFile(t, repo, "feature.txt", "feature\n")
	if _, err := CommitFiles(ctx, repo, []string{"feature.txt"}, "feature work"); err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}

	merged, err := MergeToBranch(ctx, repo, baseBranch)
	if err != nil {
		t.Fatalf("MergeToBranch: %v", err)
	}
	if merged["source"] != "feature/test" || merged["target"] != baseBranch {
		t.Fatalf("unexpected merge result: %#v", merged)
	}
	if current := strings.TrimSpace(runGit(t, repo, "branch", "--show-current")); current != baseBranch {
		t.Fatalf("current branch = %q, want %q", current, baseBranch)
	}
	if got := strings.ReplaceAll(readFile(t, repo, "feature.txt"), "\r\n", "\n"); got != "feature\n" {
		t.Fatalf("feature.txt = %q", got)
	}
}

func TestParseGitStatusADPrefersDeleted(t *testing.T) {
	entries := parseGitStatusZ([]byte("AD staged-then-deleted.txt\x00A  added.txt\x00?? untracked.txt\x00"))
	statuses := map[string]string{}
	for _, entry := range entries {
		statuses[entry["path"].(string)] = entry["status"].(string)
	}
	if statuses["staged-then-deleted.txt"] != "deleted" {
		t.Fatalf("AD status = %q, want deleted", statuses["staged-then-deleted.txt"])
	}
	if statuses["added.txt"] != "added" {
		t.Fatalf("A status = %q, want added", statuses["added.txt"])
	}
	if statuses["untracked.txt"] != "untracked" {
		t.Fatalf("?? status = %q, want untracked", statuses["untracked.txt"])
	}
}

func TestFileDiffIncludesRawDiff(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	writeFile(t, repo, "a.txt", "one\n")
	runGit(t, repo, "add", "--", "a.txt")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	writeFile(t, repo, "a.txt", "two\n")
	payload, err := FileDiff(context.Background(), repo, "a.txt")
	if err != nil {
		t.Fatalf("FileDiff: %v", err)
	}
	before, _ := payload["before"].(*string)
	after, _ := payload["after"].(*string)
	raw, _ := payload["raw"].(*string)
	if before == nil || *before != "one\n" {
		t.Fatalf("before = %#v, want one", before)
	}
	if after == nil || *after != "two\n" {
		t.Fatalf("after = %#v, want two", after)
	}
	if raw == nil || !strings.Contains(*raw, "-one") || !strings.Contains(*raw, "+two") {
		t.Fatalf("raw diff missing expected hunks: %#v", raw)
	}
}

func TestFileDiffFromSubdirUsesRepoRelativeWorkingTreeFile(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	appRoot := filepath.Join(repo, "apps", "chat")
	commonFile := filepath.Join("apps", "chat_common", "service", "prompt_service.go")
	if err := os.MkdirAll(appRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, commonFile, "one\n")
	runGit(t, repo, "init")
	runGit(t, repo, "add", "--", ".")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	writeFile(t, repo, commonFile, "one\ns12312312\n")
	entries, err := ChangedFiles(ctx, appRoot)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(entries) != 1 || entries[0]["path"] != commonFile {
		t.Fatalf("changed entries = %#v, want %s", entries, commonFile)
	}
	payload, err := FileDiff(ctx, appRoot, commonFile)
	if err != nil {
		t.Fatalf("FileDiff: %v", err)
	}
	after, _ := payload["after"].(*string)
	raw, _ := payload["raw"].(*string)
	if after == nil || !strings.Contains(*after, "s12312312") {
		t.Fatalf("after missing working tree edit: %#v", after)
	}
	if raw == nil || !strings.Contains(*raw, "+s12312312") {
		t.Fatalf("raw diff missing working tree edit: %#v", raw)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
