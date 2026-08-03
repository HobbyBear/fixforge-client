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

func TestCommitFilesCompletesResolvedMerge(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	runGit(t, repo, "init")
	writeFile(t, repo, "shared.txt", "base\n")
	runGit(t, repo, "add", "--", "shared.txt")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	baseBranch := strings.TrimSpace(runGit(t, repo, "branch", "--show-current"))

	runGit(t, repo, "checkout", "-b", "target")
	writeFile(t, repo, "shared.txt", "target\n")
	runGit(t, repo, "add", "--", "shared.txt")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "target change")
	runGit(t, repo, "checkout", baseBranch)
	writeFile(t, repo, "shared.txt", "workspace\n")
	runGit(t, repo, "add", "--", "shared.txt")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "workspace change")

	cmd := exec.Command("git", "-C", repo, "merge", "target")
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "CONFLICT") {
		t.Fatalf("git merge should conflict, err=%v output=%s", err, output)
	}
	writeFile(t, repo, "shared.txt", "resolved\n")
	if _, err := CommitFiles(ctx, repo, []string{"shared.txt"}, "resolve target merge"); err != nil {
		t.Fatalf("CommitFiles resolved merge: %v", err)
	}
	if cmd := exec.Command("git", "-C", repo, "rev-parse", "--verify", "MERGE_HEAD"); cmd.Run() == nil {
		t.Fatal("MERGE_HEAD still exists after committing the resolution")
	}
	if parents := strings.Fields(strings.TrimSpace(runGit(t, repo, "show", "-s", "--format=%P", "HEAD"))); len(parents) != 2 {
		t.Fatalf("merge commit parents = %v, want 2", parents)
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
	staged := map[string]bool{}
	for _, entry := range entries {
		statuses[entry["path"].(string)] = entry["status"].(string)
		staged[entry["path"].(string)] = entry["staged"].(bool)
	}
	if statuses["staged-then-deleted.txt"] != "deleted" {
		t.Fatalf("AD status = %q, want deleted", statuses["staged-then-deleted.txt"])
	}
	if statuses["added.txt"] != "added" {
		t.Fatalf("A status = %q, want added", statuses["added.txt"])
	}
	if !staged["added.txt"] {
		t.Fatalf("added.txt staged = false, want true")
	}
	if statuses["untracked.txt"] != "untracked" {
		t.Fatalf("?? status = %q, want untracked", statuses["untracked.txt"])
	}
	if staged["untracked.txt"] {
		t.Fatalf("untracked.txt staged = true, want false")
	}
}

func TestChangedFilesMarksAddedFilesAsStaged(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	writeFile(t, repo, "a.txt", "one\n")
	runGit(t, repo, "add", "--", "a.txt")

	entries, err := ChangedFiles(context.Background(), repo)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %#v, want one staged file", entries)
	}
	if entries[0]["path"] != "a.txt" || entries[0]["status"] != "added" || entries[0]["staged"] != true {
		t.Fatalf("unexpected staged entry: %#v", entries[0])
	}
}

func TestAddFilesFromSubdirThenChangedFilesReturnsStaged(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	appRoot := filepath.Join(repo, "apps", "chat")
	target := filepath.Join("apps", "chat", "new.txt")
	runGit(t, repo, "init")
	writeFile(t, repo, filepath.Join("apps", "chat", "README.md"), "initial\n")
	runGit(t, repo, "add", "--", ".")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	writeFile(t, repo, target, "new\n")
	before, err := ChangedFiles(ctx, appRoot)
	if err != nil {
		t.Fatalf("ChangedFiles before add: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("before entries = %#v, want one untracked file", before)
	}
	selectedPath := before[0]["path"].(string)
	if _, err := AddFiles(ctx, appRoot, []string{selectedPath}); err != nil {
		t.Fatalf("AddFiles(%q): %v", selectedPath, err)
	}
	after, err := ChangedFiles(ctx, appRoot)
	if err != nil {
		t.Fatalf("ChangedFiles after add: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("after entries = %#v, want one staged file", after)
	}
	if after[0]["path"] != selectedPath || after[0]["staged"] != true {
		t.Fatalf("unexpected after add entry: selected=%q entry=%#v", selectedPath, after[0])
	}
}

func TestAddFilesFromSubdirStagesRepoRootPath(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	appRoot := filepath.Join(repo, "apps", "chat")
	target := filepath.Join(".repomind", "modules", "sd-generation.md")
	runGit(t, repo, "init")
	writeFile(t, repo, filepath.Join("apps", "chat", "README.md"), "initial\n")
	runGit(t, repo, "add", "--", ".")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	writeFile(t, repo, target, "generated\n")
	before, err := ChangedFiles(ctx, appRoot)
	if err != nil {
		t.Fatalf("ChangedFiles before add: %v", err)
	}
	selectedPath := ""
	for _, entry := range before {
		if entry["path"] == filepath.ToSlash(target) {
			selectedPath = entry["path"].(string)
			break
		}
	}
	if selectedPath == "" {
		t.Fatalf("ChangedFiles entries = %#v, want %s", before, filepath.ToSlash(target))
	}
	if _, err := AddFiles(ctx, appRoot, []string{selectedPath}); err != nil {
		t.Fatalf("AddFiles(%q): %v", selectedPath, err)
	}
	status := runGit(t, repo, "status", "--short", "--", filepath.ToSlash(target), filepath.ToSlash(filepath.Join("apps", "chat", target)))
	if !strings.Contains(status, "A  "+filepath.ToSlash(target)) {
		t.Fatalf("status = %q, want staged repo-root path %s", status, filepath.ToSlash(target))
	}
	if strings.Contains(status, filepath.ToSlash(filepath.Join("apps", "chat", target))) {
		t.Fatalf("status = %q, staged app-root path unexpectedly", status)
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

func TestRestoreFilesFromSubdirAcceptsRepoRelativePath(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	appRoot := filepath.Join(repo, "apps", "chat")
	target := filepath.Join("apps", "chat", ".repomind", ".kb-format.json")
	runGit(t, repo, "init")
	writeFile(t, repo, target, "one\n")
	runGit(t, repo, "add", "--", target)
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	writeFile(t, repo, target, "two\n")
	if _, err := RestoreFiles(ctx, appRoot, []string{filepath.ToSlash(target)}); err != nil {
		t.Fatalf("RestoreFiles: %v", err)
	}
	if got := strings.ReplaceAll(readFile(t, repo, target), "\r\n", "\n"); got != "one\n" {
		t.Fatalf("restored file = %q, want one", got)
	}
}

func TestStashChangesFromSubdirAcceptsRepoRelativePath(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	appRoot := filepath.Join(repo, "apps", "chat")
	target := filepath.Join("apps", "chat", ".repomind", ".kb-format.json")
	runGit(t, repo, "init")
	writeFile(t, repo, filepath.Join("apps", "chat", "README.md"), "initial\n")
	runGit(t, repo, "add", "--", ".")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	writeFile(t, repo, target, "generated\n")
	result, err := StashChanges(ctx, appRoot, []string{filepath.ToSlash(target)}, "stash generated metadata")
	if err != nil {
		t.Fatalf("StashChanges: %v", err)
	}
	if strings.TrimSpace(result["hash"].(string)) == "" {
		t.Fatalf("StashChanges returned empty hash: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(repo, target)); !os.IsNotExist(err) {
		t.Fatalf("expected untracked file to be stashed, stat err = %v", err)
	}
}

func TestApplyStashRestoresWorkspaceAndKeepsStash(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	appRoot := filepath.Join(repo, "apps", "chat")
	target := filepath.Join(".repomind", "modules", "sd-generation.md")
	runGit(t, repo, "init")
	writeFile(t, repo, filepath.Join("apps", "chat", "README.md"), "initial\n")
	runGit(t, repo, "add", "--", ".")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	writeFile(t, repo, target, "generated\n")
	stashed, err := StashChanges(ctx, appRoot, []string{filepath.ToSlash(target)}, "stash generated metadata")
	if err != nil {
		t.Fatalf("StashChanges: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, target)); !os.IsNotExist(err) {
		t.Fatalf("expected stashed file to be removed, stat err = %v", err)
	}
	applied, err := ApplyStash(ctx, appRoot, "stash@{0}")
	if err != nil {
		t.Fatalf("ApplyStash: %v", err)
	}
	if applied["ref"] != "stash@{0}" {
		t.Fatalf("ApplyStash ref = %#v, want stash@{0}", applied["ref"])
	}
	if got := strings.ReplaceAll(readFile(t, repo, target), "\r\n", "\n"); got != "generated\n" {
		t.Fatalf("applied file = %q, want generated", got)
	}
	stashes, err := Stashes(ctx, appRoot, 30)
	if err != nil {
		t.Fatalf("Stashes: %v", err)
	}
	data, _ := stashes["data"].([]StashEntry)
	if len(data) != 1 || strings.TrimSpace(stashed["hash"].(string)) == "" {
		t.Fatalf("stash should be kept after apply, stashes=%#v stashed=%#v", stashes, stashed)
	}
}

func TestHistoryMarksUnpushedCommitsAndFiles(t *testing.T) {
	repo := t.TempDir()
	remote := t.TempDir()
	ctx := context.Background()
	runGit(t, remote, "init", "--bare")
	runGit(t, repo, "init")
	writeFile(t, repo, "README.md", "initial\n")
	runGit(t, repo, "add", "--", ".")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	runGit(t, repo, "branch", "-M", "main")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")

	writeFile(t, repo, "README.md", "updated\n")
	runGit(t, repo, "add", "--", "README.md")
	head := runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "update readme")
	_ = head
	headHash := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	payload, err := History(ctx, repo, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if payload["ahead"] != 1 {
		t.Fatalf("ahead = %#v, want 1", payload["ahead"])
	}
	entries := payload["data"].([]HistoryEntry)
	if len(entries) < 1 {
		t.Fatalf("history entries empty: %#v", payload)
	}
	if entries[0].Hash != headHash || !entries[0].Unpushed {
		t.Fatalf("first history entry = %#v, want unpushed head %s", entries[0], headHash)
	}
	if len(entries[0].Files) != 1 || entries[0].Files[0]["path"] != "README.md" {
		t.Fatalf("history files = %#v, want README.md", entries[0].Files)
	}
}

func TestHistoryAtRefReadsBranchWithoutCheckout(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	runGit(t, repo, "init", "-b", "main")
	writeFile(t, repo, "README.md", "main\n")
	runGit(t, repo, "add", "--", "README.md")
	runGit(t, repo, "-c", "user.name=main-author", "-c", "user.email=main@example.com", "commit", "-m", "main commit")
	runGit(t, repo, "checkout", "-b", "feature")
	writeFile(t, repo, "README.md", "feature\n")
	runGit(t, repo, "add", "--", "README.md")
	runGit(t, repo, "-c", "user.name=feature-author", "-c", "user.email=feature@example.com", "commit", "-m", "feature commit")
	runGit(t, repo, "checkout", "main")

	payload, err := HistoryAtRef(ctx, repo, "feature", 10)
	if err != nil {
		t.Fatalf("HistoryAtRef: %v", err)
	}
	entries := payload["data"].([]HistoryEntry)
	if len(entries) == 0 || entries[0].Subject != "feature commit" || entries[0].Author != "feature-author" || entries[0].AuthoredAt == "" {
		t.Fatalf("feature history = %#v", entries)
	}
	if payload["selected_ref"] != "feature" {
		t.Fatalf("selected_ref = %#v, want feature", payload["selected_ref"])
	}
	if current := strings.TrimSpace(runGit(t, repo, "branch", "--show-current")); current != "main" {
		t.Fatalf("HistoryAtRef switched branch to %q", current)
	}
}

func TestRemoteHistoryAndContributorsIgnoreStaleLocalBranch(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	runGit(t, repo, "init", "-b", "main")
	writeFile(t, repo, "README.md", "local\n")
	runGit(t, repo, "add", "--", "README.md")
	runGit(t, repo, "-c", "user.name=Local", "-c", "user.email=local@example.com", "commit", "--date=2026-07-30T10:00:00Z", "-m", "local main")
	localSHA := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))

	writeFile(t, repo, "README.md", "remote\n")
	runGit(t, repo, "add", "--", "README.md")
	runGit(t, repo, "-c", "user.name=Remote", "-c", "user.email=remote@example.com", "commit", "--date=2026-07-31T10:00:00Z", "-m", "remote main")
	remoteSHA := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	runGit(t, repo, "update-ref", "refs/remotes/origin/main", remoteSHA)
	runGit(t, repo, "reset", "--hard", localSHA)

	localPayload, err := HistoryAtRef(ctx, repo, "main", 10)
	if err != nil {
		t.Fatalf("HistoryAtRef: %v", err)
	}
	if entries := localPayload["data"].([]HistoryEntry); len(entries) == 0 || entries[0].Hash != localSHA {
		t.Fatalf("local history = %#v, want stale local SHA %s", entries, localSHA)
	}

	remotePayload, err := HistoryAtRemoteRef(ctx, repo, "main", 10)
	if err != nil {
		t.Fatalf("HistoryAtRemoteRef: %v", err)
	}
	if entries := remotePayload["data"].([]HistoryEntry); len(entries) == 0 || entries[0].Hash != remoteSHA || entries[0].Author != "Remote" {
		t.Fatalf("remote history = %#v, want remote SHA %s", entries, remoteSHA)
	}

	contributorsPayload, err := RecentContributorsAtRemoteRef(ctx, repo, "main")
	if err != nil {
		t.Fatalf("RecentContributorsAtRemoteRef: %v", err)
	}
	contributors := contributorsPayload["data"].([]map[string]any)
	if len(contributors) != 2 || contributors[0]["name"] != "Local" || contributors[1]["name"] != "Remote" {
		t.Fatalf("remote contributors = %#v, want Local and Remote", contributors)
	}
	if got := strings.TrimSpace(runGit(t, repo, "rev-parse", "refs/heads/main")); got != localSHA {
		t.Fatalf("remote reads changed local main from %s to %s", localSHA, got)
	}
}

func TestRecentContributorsUsesSelectedRefHeadAsWindowEnd(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	runGit(t, repo, "init", "-b", "main")
	writeFile(t, repo, "README.md", "old\n")
	runGit(t, repo, "add", "--", "README.md")
	runGit(t, repo, "-c", "user.name=Old", "-c", "user.email=old@example.com", "commit", "--date=2026-07-13T11:59:59Z", "-m", "old")
	runGit(t, repo, "checkout", "-b", "feature")
	writeFile(t, repo, "README.md", "boundary\n")
	runGit(t, repo, "add", "--", "README.md")
	runGit(t, repo, "-c", "user.name=Boundary", "-c", "user.email=boundary@example.com", "commit", "--date=2026-07-13T12:00:00Z", "-m", "boundary")
	writeFile(t, repo, "README.md", "head\n")
	runGit(t, repo, "add", "--", "README.md")
	runGit(t, repo, "-c", "user.name=Head", "-c", "user.email=head@example.com", "commit", "--date=2026-07-20T12:00:00Z", "-m", "head")
	runGit(t, repo, "checkout", "main")

	payload, err := RecentContributors(ctx, repo, "feature")
	if err != nil {
		t.Fatalf("RecentContributors: %v", err)
	}
	contributors := payload["data"].([]map[string]any)
	if len(contributors) != 2 || contributors[0]["name"] != "Boundary" || contributors[1]["name"] != "Head" {
		t.Fatalf("contributors = %#v, want Boundary and Head", contributors)
	}
	if payload["selected_ref"] != "feature" {
		t.Fatalf("selected_ref = %#v, want feature", payload["selected_ref"])
	}
	if current := strings.TrimSpace(runGit(t, repo, "branch", "--show-current")); current != "main" {
		t.Fatalf("RecentContributors switched branch to %q", current)
	}
}

func TestCommitFileDiffReturnsCommitContent(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()
	runGit(t, repo, "init")
	writeFile(t, repo, "README.md", "initial\n")
	runGit(t, repo, "add", "--", ".")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	writeFile(t, repo, "README.md", "updated\n")
	runGit(t, repo, "add", "--", "README.md")
	runGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "update readme")
	headHash := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	payload, err := CommitFileDiff(ctx, repo, headHash, "README.md")
	if err != nil {
		t.Fatalf("CommitFileDiff: %v", err)
	}
	before, _ := payload["before"].(*string)
	after, _ := payload["after"].(*string)
	raw, _ := payload["raw"].(*string)
	if before == nil || *before != "initial\n" {
		t.Fatalf("before = %#v, want initial", before)
	}
	if after == nil || *after != "updated\n" {
		t.Fatalf("after = %#v, want updated", after)
	}
	if raw == nil || !strings.Contains(*raw, "-initial") || !strings.Contains(*raw, "+updated") {
		t.Fatalf("raw = %#v, want commit diff", raw)
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
