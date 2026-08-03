package codevisualizer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func normalizeUnitSymbols(ctx context.Context, repoRoot string, input []byte) ([]byte, error) {
	var walkthrough map[string]any
	if err := json.Unmarshal(input, &walkthrough); err != nil {
		return nil, fmt.Errorf("invalid walkthrough JSON: %w", err)
	}
	comparison, _ := walkthrough["comparison"].(map[string]any)
	mode := strings.TrimSpace(fmt.Sprint(comparison["mode"]))
	headRef := comparisonRevision(comparison, "head_sha", "head_ref")
	compareRef := comparisonRevision(comparison, "base_sha", "base_ref")
	if strings.TrimSpace(fmt.Sprint(comparison["strategy"])) == "merge_base" {
		output, err := gitOutput(ctx, repoRoot, "merge-base", compareRef, headRef)
		if err != nil {
			return nil, fmt.Errorf("resolve walkthrough merge base: %w", err)
		}
		compareRef = strings.TrimSpace(output)
	}
	if headRef == "" || compareRef == "" {
		return input, nil
	}

	changes, _ := walkthrough["changes"].([]any)
	changed := false
	for _, rawChange := range changes {
		change, _ := rawChange.(map[string]any)
		newPath := walkthroughPath(change, "new_file", "file")
		oldPath := walkthroughPath(change, "old_file", "file")
		var newSource string
		var newErr error
		if mode == "working_tree" {
			newSource, newErr = sourceFromWorkingTree(repoRoot, newPath)
		} else {
			newSource, newErr = sourceAtRef(ctx, repoRoot, headRef, newPath)
		}
		oldSource, oldErr := sourceAtRef(ctx, repoRoot, compareRef, oldPath)
		sourceForSymbol := newSource
		if newErr != nil || sourceForSymbol == "" {
			sourceForSymbol = oldSource
		}
		if (newErr != nil || newSource == "") && (oldErr != nil || oldSource == "") {
			continue
		}
		units, _ := change["units"].([]any)
		for _, rawUnit := range units {
			unit, _ := rawUnit.(map[string]any)
			kind := strings.TrimSpace(fmt.Sprint(unit["kind"]))
			if kind != "method" && kind != "function" && kind != "class" {
				continue
			}
			symbol := strings.TrimSpace(fmt.Sprint(unit["symbol"]))
			if symbol == "" || strings.Contains(sourceForSymbol, symbol) {
				continue
			}
			normalized := false
			for _, candidate := range unqualifiedSymbols(symbol) {
				if strings.Contains(sourceForSymbol, candidate) {
					unit["symbol"] = candidate
					changed = true
					normalized = true
					break
				}
			}
			if !normalized {
				// The upstream validator checks declaration symbols only against the
				// new source for modified files. Deleted declarations and semantic
				// names therefore cannot truthfully remain declaration units.
				unit["kind"] = "block"
				changed = true
			}
		}
	}
	if !changed {
		return input, nil
	}
	return json.Marshal(walkthrough)
}

func comparisonRevision(comparison map[string]any, lockedKey, refKey string) string {
	locked := strings.TrimSpace(fmt.Sprint(comparison[lockedKey]))
	if locked != "" && locked != "<nil>" {
		return locked
	}
	return strings.TrimSpace(fmt.Sprint(comparison[refKey]))
}

func sourceFromWorkingTree(repoRoot, path string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid working tree source path %q", path)
	}
	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", err
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("working tree source path escapes repository: %q", path)
	}
	content, err := os.ReadFile(candidate)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func walkthroughPath(change map[string]any, keys ...string) string {
	for _, key := range keys {
		path := filepath.ToSlash(strings.TrimSpace(fmt.Sprint(change[key])))
		if path != "" && path != "<nil>" && path != "." && !filepath.IsAbs(path) && !strings.HasPrefix(path, "../") {
			return path
		}
	}
	return ""
}

func sourceAtRef(ctx context.Context, repoRoot, ref, path string) (string, error) {
	if ref == "" || path == "" {
		return "", fmt.Errorf("source ref and path are required")
	}
	return gitOutput(ctx, repoRoot, "show", ref+":"+path)
}

func unqualifiedSymbols(symbol string) []string {
	candidates := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, separator := range []string{"::", ".", "#", "/"} {
		if index := strings.LastIndex(symbol, separator); index >= 0 {
			candidate := strings.TrimSpace(symbol[index+len(separator):])
			candidate = strings.TrimSuffix(candidate, "()")
			if len(candidate) >= 2 && candidate != symbol && !seen[candidate] {
				seen[candidate] = true
				candidates = append(candidates, candidate)
			}
		}
	}
	return candidates
}

func gitOutput(ctx context.Context, repoRoot string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", repoRoot}, args...)
	output, err := exec.CommandContext(ctx, "git", cmdArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
