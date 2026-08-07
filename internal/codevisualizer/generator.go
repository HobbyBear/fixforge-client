package codevisualizer

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed assets/*
var assets embed.FS

const maxHTMLBytes = 24 << 20
const maxDataBytes = 24 << 20

// Instructions returns the exact code-change-visualizer workflow bundled with
// FixForge. Keeping the model contract and renderer together prevents drift.
func Instructions() string {
	skill, _ := assets.ReadFile("assets/SKILL.md")
	format, _ := assets.ReadFile("assets/input-format.md")
	return string(skill) + "\n\n" + string(format)
}

// RunnerInstructions keeps the agent focused on producing the renderer input.
// FixForge owns comparison locking and runs the validator after the response.
func RunnerInstructions() string {
	return `FixForge orchestration boundary:
- The complete code-change-visualizer instructions and input format are already included above. Do not read them, RepoMind, AGENTS.md, or the renderer source again.
- The comparison is already resolved and locked. Analyze only its Git diff and the minimum source context needed to explain the change.
- Git, not the model, owns changed-file discovery, hunk boundaries, unit IDs, old/new ranges, coverage, and overlap. Do not calculate or validate line ranges.
- For branch or commit comparisons, never treat the current working tree as the selected snapshot. Use the authoritative evidence in the prompt or read source with git show <locked-sha>:<path>.
- Do not run the visualizer or validator. FixForge will validate and render the returned object.
- Keep command output narrow. Do not read unrelated files or broad repository history.
- Focus the response on title, summary, file purpose/implementation, and semantic meaning/reason/impact. Omitted files or semantic entries will be filled from Git.
- For changes spanning multiple files or units, prefer 1-3 flows with 2-8 steps each, ordered only by source-backed call, data, or state transitions. Do not substitute file or hunk order for a semantic flow; leave flows empty when the evidence is insufficient.
- Finish with exactly one v2 JSON object. Progress text is not accepted as the result.`
}

// ExtractJSON accepts a plain JSON answer or a fenced model response and
// returns a compact, validated JSON object.
func ExtractJSON(raw string) ([]byte, error) {
	value := strings.TrimSpace(raw)
	if strings.HasPrefix(value, "```") {
		lines := strings.Split(value, "\n")
		if len(lines) >= 2 {
			lines = lines[1:]
			if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
				lines = lines[:len(lines)-1]
			}
			value = strings.TrimSpace(strings.Join(lines, "\n"))
		}
	}
	start := strings.IndexByte(value, '{')
	end := strings.LastIndexByte(value, '}')
	if start < 0 || end < start {
		return nil, fmt.Errorf("AI response does not contain a JSON object")
	}
	value = value[start : end+1]
	var data map[string]any
	if err := json.Unmarshal([]byte(value), &data); err != nil {
		return nil, fmt.Errorf("invalid walkthrough JSON: %w", err)
	}
	if version, ok := data["version"].(float64); !ok || int(version) != 2 {
		return nil, fmt.Errorf("walkthrough version must be 2")
	}
	return json.Marshal(data)
}

// FallbackWalkthrough keeps a locked Git comparison renderable when the model
// fails to return structured analysis. The validator derives files and units.
func FallbackWalkthrough(comparison map[string]any, title string) ([]byte, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "代码变更分析"
	}
	return json.Marshal(map[string]any{
		"version":    2,
		"title":      title,
		"summary":    "模型未返回有效的结构化说明，当前结果由锁定的 Git 差异生成。",
		"comparison": comparison,
		"flows":      []any{},
		"changes":    []any{},
	})
}

// WithComparison replaces the model's comparison with a server-selected,
// validated comparison. This prevents an analysis from silently changing the
// requested refs or commit range.
func WithComparison(input []byte, comparison map[string]any) ([]byte, error) {
	var data map[string]any
	if err := json.Unmarshal(input, &data); err != nil {
		return nil, err
	}
	data["comparison"] = comparison
	return json.Marshal(data)
}

// Comparison returns the normalized comparison object from a walkthrough.
func Comparison(input []byte) (map[string]any, error) {
	var data struct {
		Comparison map[string]any `json:"comparison"`
	}
	if err := json.Unmarshal(input, &data); err != nil {
		return nil, err
	}
	if len(data.Comparison) == 0 {
		return nil, fmt.Errorf("walkthrough comparison is required")
	}
	return data.Comparison, nil
}

// Generate runs the bundled upstream validator and renderer against the real
// repository. The generated document is fully offline and contains the exact
// visualizer CSS, JavaScript, font, review comments and export behavior.
func Generate(ctx context.Context, repoRoot string, input []byte) ([]byte, error) {
	return generate(ctx, repoRoot, input, "html")
}

// GenerateData validates the model walkthrough against Git and returns the
// structured view model consumed by the FixForge React analysis route.
func GenerateData(ctx context.Context, repoRoot string, input []byte) ([]byte, error) {
	rendered, err := generate(ctx, repoRoot, input, "json")
	if err != nil {
		return nil, err
	}
	rendered, err = AttachCodeMap(rendered)
	if err != nil {
		return nil, err
	}
	return attachGoRepositoryContext(ctx, repoRoot, rendered)
}

func generate(ctx context.Context, repoRoot string, input []byte, outputFormat string) ([]byte, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("walkthrough JSON is empty")
	}
	repoRoot, err := gitRepositoryRoot(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	input, err = normalizeUnitSymbols(ctx, repoRoot, input)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp(repoRoot, ".fixforge-code-visualizer-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	files := []struct {
		source string
		name   string
		mode   os.FileMode
	}{
		{"assets/generate_visualization.py", "generate_visualization.py", 0o700},
		{"assets/example-font.woff2", "example-font.woff2", 0o600},
	}
	for _, file := range files {
		content, readErr := assets.ReadFile(file.source)
		if readErr != nil {
			return nil, readErr
		}
		if writeErr := os.WriteFile(filepath.Join(tempDir, file.name), content, file.mode); writeErr != nil {
			return nil, writeErr
		}
	}
	inputPath := filepath.Join(tempDir, "walkthrough.json")
	outputName := "walkthrough.html"
	if outputFormat == "json" {
		outputName = "visualization.json"
	}
	outputPath := filepath.Join(tempDir, outputName)
	if err := os.WriteFile(inputPath, input, 0o600); err != nil {
		return nil, err
	}

	command, prefix, err := pythonCommand()
	if err != nil {
		return nil, err
	}
	args := append(prefix,
		filepath.Join(tempDir, "generate_visualization.py"),
		"--repo-root", repoRoot,
		"--change-dir", tempDir,
		"--input", inputPath,
		"--output", outputPath,
		"--output-format", outputFormat,
		"--font", filepath.Join(tempDir, "example-font.woff2"),
	)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("visualizer validation failed: %s", message)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("visualizer did not produce %s: %w", outputFormat, err)
	}
	limit := int64(maxHTMLBytes)
	if outputFormat == "json" {
		limit = maxDataBytes
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("visualizer %s exceeds %d bytes", outputFormat, limit)
	}
	result, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, err
	}
	if outputFormat == "json" && !json.Valid(result) {
		return nil, fmt.Errorf("visualizer returned invalid JSON")
	}
	return result, nil
}

func gitRepositoryRoot(ctx context.Context, workdir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", workdir, "rev-parse", "--show-toplevel")
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("resolve visualizer Git root: %s", message)
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", fmt.Errorf("resolve visualizer Git root: empty path")
	}
	return filepath.Clean(root), nil
}

func pythonCommand() (string, []string, error) {
	if configured := strings.TrimSpace(os.Getenv("FIXFORGE_PYTHON")); configured != "" {
		if path, err := exec.LookPath(configured); err == nil {
			return path, nil, nil
		}
	}
	for _, candidate := range []string{"python3", "python"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil, nil
		}
	}
	if runtime.GOOS == "windows" {
		if path, err := exec.LookPath("py"); err == nil {
			return path, []string{"-3"}, nil
		}
	}
	return "", nil, fmt.Errorf("Python 3 is required to generate code change visualizations; set FIXFORGE_PYTHON to its executable")
}
