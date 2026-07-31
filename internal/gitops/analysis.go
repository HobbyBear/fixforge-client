package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const maxAnalysisDiffBytes = 2 << 20

type AnalysisSelection struct {
	Mode         string
	BaseRef      string
	HeadRef      string
	Strategy     string
	CommitHashes []string
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
		baseRef, _, err := resolveAnalysisRef(ctx, root, selection.BaseRef)
		if err != nil {
			return nil, fmt.Errorf("invalid base ref: %w", err)
		}
		headRef, _, err := resolveAnalysisRef(ctx, root, selection.HeadRef)
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

func resolveWorkingTreeSelection(ctx context.Context, root string) (map[string]any, error) {
	_, baseSHA, err := resolveAnalysisRef(ctx, root, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("working tree has no commit to use as a baseline: %w", err)
	}
	tracked, err := gitOutput(ctx, root, "diff", "--name-only", baseSHA, "--")
	if err != nil {
		return nil, fmt.Errorf("read working tree changes: %w: %s", err, strings.TrimSpace(tracked))
	}
	if strings.TrimSpace(tracked) == "" {
		return nil, fmt.Errorf("working tree has no tracked uncommitted changes")
	}
	return map[string]any{
		"mode":     "working_tree",
		"base_ref": baseSHA,
		"head_ref": "WORKTREE",
		"strategy": "working_tree",
	}, nil
}

func AnalysisContext(ctx context.Context, root string, comparison map[string]any) (string, error) {
	baseRef := strings.TrimSpace(fmt.Sprint(comparison["base_ref"]))
	headRef := strings.TrimSpace(fmt.Sprint(comparison["head_ref"]))
	strategy := strings.TrimSpace(fmt.Sprint(comparison["strategy"]))
	baseRef, baseSHA, err := resolveAnalysisRef(ctx, root, baseRef)
	if err != nil {
		return "", err
	}
	headRef, headSHA, err := resolveAnalysisRef(ctx, root, headRef)
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
