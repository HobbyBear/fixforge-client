package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var analysisSnapshotIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

// CreateAnalysisSnapshot materializes an analysis in a private Git repository.
// The repository has a detached HEAD and no remotes or branches, so it cannot be
// included in a normal push from either the source repository or the snapshot.
func CreateAnalysisSnapshot(ctx context.Context, sourceRoot, storeRoot, snapshotID string, comparison map[string]any) (map[string]any, error) {
	snapshotID = strings.TrimSpace(snapshotID)
	if !analysisSnapshotIDPattern.MatchString(snapshotID) {
		return nil, fmt.Errorf("invalid analysis snapshot id")
	}
	if err := ValidateAnalysisSnapshot(ctx, sourceRoot, comparison); err != nil {
		return nil, err
	}
	sourceRoot = gitCommandRoot(ctx, sourceRoot)
	root, err := AnalysisSnapshotRoot(storeRoot, snapshotID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(storeRoot, 0o700); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(root)
		}
	}()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if out, err := snapshotGit(ctx, root, "init", "--quiet"); err != nil {
		return nil, fmt.Errorf("initialize analysis snapshot: %w: %s", err, out)
	}
	objectsDir, err := sourceObjectsDir(ctx, sourceRoot)
	if err != nil {
		return nil, err
	}
	alternates := filepath.Join(root, ".git", "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(alternates), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(alternates, []byte(objectsDir+"\n"), 0o600); err != nil {
		return nil, err
	}

	sourceBaseSHA := strings.TrimSpace(fmt.Sprint(comparison["base_sha"]))
	sourceHeadSHA := strings.TrimSpace(fmt.Sprint(comparison["head_sha"]))
	if sourceBaseSHA == "" {
		return nil, fmt.Errorf("analysis comparison has no locked base SHA")
	}
	if sourceHeadSHA == "" {
		return nil, fmt.Errorf("analysis comparison has no locked head SHA")
	}
	sourceStrategy := strings.TrimSpace(fmt.Sprint(comparison["strategy"]))
	sourceCompareSHA := sourceBaseSHA
	if sourceStrategy == "merge_base" {
		out, mergeErr := gitOutput(ctx, sourceRoot, "merge-base", sourceBaseSHA, sourceHeadSHA)
		if mergeErr != nil || strings.TrimSpace(out) == "" {
			return nil, fmt.Errorf("cannot find merge base for analysis snapshot")
		}
		sourceCompareSHA = strings.TrimSpace(out)
	} else if lockedCompareSHA := codeAnalysisString(comparison["compare_sha"]); lockedCompareSHA != "" {
		sourceCompareSHA = lockedCompareSHA
	}
	checkoutSHA := sourceHeadSHA
	if strings.TrimSpace(fmt.Sprint(comparison["mode"])) == "working_tree" {
		checkoutSHA = sourceBaseSHA
	}
	if out, err := snapshotGit(ctx, root, "checkout", "--detach", "--quiet", checkoutSHA); err != nil {
		return nil, fmt.Errorf("checkout analysis snapshot: %w: %s", err, out)
	}

	result := cloneAnalysisComparison(comparison)
	headTree := ""
	sourceCommits := ""
	if strings.TrimSpace(fmt.Sprint(comparison["mode"])) == "working_tree" {
		patch, err := workingTreePatch(ctx, sourceRoot, comparison)
		if err != nil {
			return nil, err
		}
		if got, want := workingTreeDiffFingerprint(sourceBaseSHA, patch), codeAnalysisString(comparison["snapshot_fingerprint"]); want == "" || got != want {
			return nil, fmt.Errorf("local working tree changed while creating analysis snapshot")
		}
		cmd := exec.CommandContext(ctx, "git", "-C", root, "apply", "--binary", "--whitespace=nowarn", "-")
		cmd.Stdin = strings.NewReader(patch)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("apply working tree snapshot: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if out, err := snapshotGit(ctx, root, "add", "-A"); err != nil {
			return nil, fmt.Errorf("stage analysis snapshot: %w: %s", err, out)
		}
		headTree, err = snapshotGit(ctx, root, "write-tree")
		if err != nil {
			return nil, fmt.Errorf("write analysis snapshot tree: %w", err)
		}
		result["source_mode"] = "working_tree"
		result["mode"] = "branch_compare"
	} else {
		sourceCommits, _ = gitOutput(ctx, sourceRoot, "log", "--max-count=200", "--format=%H %s", sourceCompareSHA+".."+sourceHeadSHA)
		headTree, err = snapshotGit(ctx, root, "rev-parse", sourceHeadSHA+"^{tree}")
		if err != nil {
			return nil, fmt.Errorf("resolve analysis head tree: %w", err)
		}
	}
	baseTree, err := snapshotGit(ctx, root, "rev-parse", sourceCompareSHA+"^{tree}")
	if err != nil {
		return nil, fmt.Errorf("resolve analysis base tree: %w", err)
	}
	snapshotBaseSHA, err := createAnalysisSnapshotCommit(ctx, root, baseTree, "", "FixForge analysis base")
	if err != nil {
		return nil, err
	}
	snapshotHeadSHA, err := createAnalysisSnapshotCommit(ctx, root, headTree, snapshotBaseSHA, "FixForge analysis snapshot")
	if err != nil {
		return nil, err
	}
	if resetOut, err := snapshotGit(ctx, root, "reset", "--hard", "--quiet", snapshotHeadSHA); err != nil {
		return nil, fmt.Errorf("activate analysis snapshot: %w: %s", err, resetOut)
	}
	result["source_base_sha"] = sourceBaseSHA
	result["source_compare_sha"] = sourceCompareSHA
	result["source_head_sha"] = sourceHeadSHA
	result["source_strategy"] = sourceStrategy
	result["source_commits"] = strings.TrimSpace(sourceCommits)
	result["base_sha"] = snapshotBaseSHA
	result["compare_sha"] = snapshotBaseSHA
	result["head_sha"] = snapshotHeadSHA
	result["snapshot_sha"] = snapshotHeadSHA
	result["strategy"] = "direct"
	result["snapshot_id"] = snapshotID
	result["snapshot_created_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := detachAnalysisSnapshotObjects(ctx, root, alternates, snapshotHeadSHA); err != nil {
		return nil, err
	}

	if err := ValidateAnalysisSnapshot(ctx, sourceRoot, comparison); err != nil {
		return nil, fmt.Errorf("source changed while creating analysis snapshot: %w", err)
	}
	if err := ValidateMaterializedAnalysisSnapshot(ctx, root, result); err != nil {
		return nil, err
	}
	cleanup = false
	return result, nil
}

func detachAnalysisSnapshotObjects(ctx context.Context, root, alternates, headSHA string) error {
	packDir := filepath.Join(root, ".git", "objects", "pack")
	if err := os.MkdirAll(packDir, 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "pack-objects", "--revs", filepath.Join(packDir, "pack"))
	cmd.Stdin = strings.NewReader(headSHA + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy analysis snapshot objects: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Remove(alternates); err != nil {
		return fmt.Errorf("detach analysis snapshot from source repository: %w", err)
	}
	if _, err := snapshotGit(ctx, root, "cat-file", "-e", headSHA+"^{commit}"); err != nil {
		return fmt.Errorf("validate detached analysis snapshot objects: %w", err)
	}
	return nil
}

func createAnalysisSnapshotCommit(ctx context.Context, root, treeSHA, parentSHA, message string) (string, error) {
	args := []string{"-C", root, "commit-tree", strings.TrimSpace(treeSHA)}
	if strings.TrimSpace(parentSHA) != "" {
		args = append(args, "-p", strings.TrimSpace(parentSHA))
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = strings.NewReader(message + "\n")
	now := time.Now().UTC().Format(time.RFC3339)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=FixForge Snapshot", "GIT_AUTHOR_EMAIL=snapshot@localhost",
		"GIT_COMMITTER_NAME=FixForge Snapshot", "GIT_COMMITTER_EMAIL=snapshot@localhost",
		"GIT_AUTHOR_DATE="+now, "GIT_COMMITTER_DATE="+now,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("commit analysis snapshot: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func AnalysisSnapshotRoot(storeRoot, snapshotID string) (string, error) {
	if !analysisSnapshotIDPattern.MatchString(strings.TrimSpace(snapshotID)) {
		return "", fmt.Errorf("invalid analysis snapshot id")
	}
	storeRoot, err := filepath.Abs(storeRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(storeRoot, snapshotID), nil
}

func ResolveAnalysisSnapshotRoot(storeRoot string, comparison map[string]any) (string, error) {
	id := strings.TrimSpace(fmt.Sprint(comparison["snapshot_id"]))
	if id == "" {
		return "", fmt.Errorf("analysis comparison has no snapshot id")
	}
	root, err := AnalysisSnapshotRoot(storeRoot, id)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return "", fmt.Errorf("analysis snapshot %q is unavailable on this device", id)
	}
	return root, nil
}

func ValidateMaterializedAnalysisSnapshot(ctx context.Context, root string, comparison map[string]any) error {
	want := strings.TrimSpace(fmt.Sprint(comparison["snapshot_sha"]))
	if want == "" {
		return fmt.Errorf("analysis snapshot has no locked SHA")
	}
	got, err := snapshotGit(ctx, root, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(got) != want {
		return fmt.Errorf("analysis snapshot HEAD changed")
	}
	branch, err := snapshotGit(ctx, root, "branch", "--show-current")
	if err != nil || strings.TrimSpace(branch) != "" {
		return fmt.Errorf("analysis snapshot must use detached HEAD")
	}
	remotes, err := snapshotGit(ctx, root, "remote")
	if err != nil || strings.TrimSpace(remotes) != "" {
		return fmt.Errorf("analysis snapshot must not configure a remote")
	}
	refs, err := snapshotGit(ctx, root, "show-ref")
	if err == nil || strings.TrimSpace(refs) != "" {
		return fmt.Errorf("analysis snapshot must not create a branch or ref")
	}
	status, err := snapshotGit(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || strings.TrimSpace(status) != "" {
		return fmt.Errorf("analysis snapshot working tree changed")
	}
	return nil
}

func sourceObjectsDir(ctx context.Context, sourceRoot string) (string, error) {
	commonDir, err := gitOutput(ctx, sourceRoot, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	commonDir = strings.TrimSpace(commonDir)
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(sourceRoot, commonDir)
	}
	return filepath.Clean(filepath.Join(commonDir, "objects")), nil
}

func workingTreePatch(ctx context.Context, sourceRoot string, comparison map[string]any) (string, error) {
	baseSHA := strings.TrimSpace(fmt.Sprint(comparison["base_sha"]))
	args := []string{"diff", "--binary", "--find-renames", baseSHA, "--"}
	for _, path := range stringsFromAny(comparison["changed_paths"]) {
		args = append(args, path)
	}
	return gitOutput(ctx, sourceRoot, args...)
}

func cloneAnalysisComparison(comparison map[string]any) map[string]any {
	raw, _ := json.Marshal(comparison)
	result := map[string]any{}
	_ = json.Unmarshal(raw, &result)
	return result
}

func snapshotGit(ctx context.Context, root string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
