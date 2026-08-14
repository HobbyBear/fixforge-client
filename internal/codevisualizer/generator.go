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

//go:embed assets/* assets/code-architecture-review/assets/* assets/code-architecture-review/references/*
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

// ArchitectureReviewInstructions returns the vendored code-architecture-review
// skill and its JSON contract. The copy is embedded in fixforge-client so a
// review does not depend on the operator's ~/.codex installation.
func ArchitectureReviewInstructions() string {
	skill, _ := assets.ReadFile("assets/code-architecture-review/SKILL.md")
	contract, _ := assets.ReadFile("assets/code-architecture-review/references/report-contract.md")
	return string(skill) + "\n\n--- report-contract.md ---\n\n" + string(contract)
}

// ArchitectureReviewRunnerInstructions narrows the skill to the immutable Git
// comparison selected by FixForge while preserving the skill's evidence rules.
func ArchitectureReviewRunnerInstructions() string {
	return `FixForge code-architecture-review execution boundary:
- Follow the code-architecture-review skill and report contract included above.
- The Git comparison in the request is already resolved and locked. Do not replace, reinterpret, fetch, checkout, merge, commit, push, or modify it.
- Run the RepoMind query steps from the skill when RepoMind is available in the repository, then inspect only the minimum source evidence for this comparison.
- The user's analysis focus is a hard scope constraint. For custom input, use it to select and order relevant entities inside the locked comparison, never to invent refs or relationships.
- Git and source/AST evidence own files, symbols, line numbers, calls, fields, and relationships. Mark insufficient evidence as unknown instead of manufacturing a complete path.
- Return exactly one JSON object with schema code-architecture-report/v1. Do not return Markdown, HTML, CSS, JavaScript, progress text, or the legacy code-change-walkthrough/v2 format.
- Put the locked comparison, including its fingerprint, in scope.comparison. The report must contain a real entrypoint, at least one core lane, and at least one source-backed data flow.`
}

// ExtractArchitectureReport extracts and minimally validates a skill report.
// Full reference validation is performed by the canonical renderer.
func ExtractArchitectureReport(raw string) ([]byte, error) {
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
		return nil, fmt.Errorf("AI response does not contain an architecture report JSON object")
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(value[start:end+1]), &data); err != nil {
		return nil, fmt.Errorf("invalid architecture report JSON: %w", err)
	}
	if strings.TrimSpace(fmt.Sprint(data["schema"])) != "code-architecture-report/v1" {
		return nil, fmt.Errorf("architecture report schema must be code-architecture-report/v1")
	}
	if _, ok := data["flow_map"].(map[string]any); !ok {
		return nil, fmt.Errorf("architecture report flow_map is required")
	}
	if _, ok := data["architecture_design"].(map[string]any); !ok {
		return nil, fmt.Errorf("architecture report architecture_design is required")
	}
	if err := validateArchitectureReportCompletion(data); err != nil {
		return nil, err
	}
	return json.Marshal(data)
}

func validateArchitectureReportCompletion(data map[string]any) error {
	codeMap, _ := data["code_map"].(map[string]any)
	if codeMap == nil {
		codeMap = data
	}
	sourceIDs := map[string]bool{}
	locatedSources := 0
	for _, rawNode := range codeMapSlice(codeMap["nodes"]) {
		node := codeMapObject(rawNode)
		id := strings.TrimSpace(fmt.Sprint(node["id"]))
		if id == "" {
			continue
		}
		sourceIDs[id] = true
		if strings.TrimSpace(fmt.Sprint(node["file"])) != "" && codeMapInt(node["line"], 0) > 0 {
			locatedSources++
		}
	}
	if locatedSources == 0 {
		return fmt.Errorf("architecture report requires at least one source node with file and line evidence")
	}

	design := codeMapObject(data["architecture_design"])
	lanes := codeMapSlice(design["lanes"])
	if len(lanes) == 0 {
		return fmt.Errorf("architecture report requires at least one module lane")
	}
	completeLane := false
	for _, rawLane := range lanes {
		lane := codeMapObject(rawLane)
		if strings.TrimSpace(fmt.Sprint(lane["id"])) == "" || len(codeMapSlice(lane["responsibilities"])) == 0 {
			continue
		}
		for _, sourceID := range codeMapSlice(lane["source_node_ids"]) {
			if sourceIDs[strings.TrimSpace(fmt.Sprint(sourceID))] {
				completeLane = true
				break
			}
		}
	}
	if !completeLane {
		return fmt.Errorf("architecture report requires a module lane with responsibilities and source evidence")
	}

	flow := codeMapObject(data["flow_map"])
	children := codeMapSlice(flow["children"])
	if strings.TrimSpace(fmt.Sprint(flow["id"])) == "" || len(children) == 0 {
		return fmt.Errorf("architecture report requires an entry flow with at least one business stage")
	}
	sourceBackedStage := false
	var visit func(map[string]any)
	visit = func(node map[string]any) {
		for _, rawSourceID := range codeMapSlice(node["source_node_ids"]) {
			if sourceIDs[strings.TrimSpace(fmt.Sprint(rawSourceID))] {
				sourceBackedStage = true
			}
		}
		for _, rawChild := range codeMapSlice(node["children"]) {
			visit(codeMapObject(rawChild))
		}
	}
	visit(flow)
	if !sourceBackedStage {
		return fmt.Errorf("architecture report requires at least one source-backed business flow stage")
	}
	return nil
}

// WithArchitectureComparison enforces the server-selected comparison after AI
// analysis so custom instructions cannot drift the saved report to another ref.
func WithArchitectureComparison(input []byte, comparison map[string]any, focus string) ([]byte, error) {
	var data map[string]any
	if err := json.Unmarshal(input, &data); err != nil {
		return nil, err
	}
	data["comparison"] = comparison
	data["review_skill"] = "code-architecture-review"
	data["review_renderer"] = "canonical-vendored"
	scope, _ := data["scope"].(map[string]any)
	if scope == nil {
		scope = map[string]any{}
	}
	scope["comparison"] = comparison
	data["scope"] = scope
	analysisFocus, _ := data["analysis_focus"].(map[string]any)
	if analysisFocus == nil {
		analysisFocus = map[string]any{"entity": "all", "policy": "related_to_changed", "include_ids": []any{}, "exclude_ids": []any{}}
	}
	if strings.TrimSpace(focus) != "" {
		analysisFocus["query"] = strings.TrimSpace(focus)
	}
	data["analysis_focus"] = analysisFocus
	return json.Marshal(data)
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
	// The saved HTML report and the React prototype intentionally share the
	// architecture workbench layout. Keep the v2 validator as the source of
	// truth, then render its enriched code map with the canonical template.
	data, err := GenerateData(ctx, repoRoot, input)
	if err != nil {
		return nil, err
	}
	return renderArchitectureHTML(ctx, repoRoot, data)
}

func renderArchitectureHTML(ctx context.Context, repoRoot string, data []byte) ([]byte, error) {
	tempDir, err := os.MkdirTemp(repoRoot, ".fixforge-code-architecture-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	for _, file := range []struct {
		source string
		name   string
		mode   os.FileMode
	}{
		{"assets/code-architecture-review/assets/architecture_report.py", "architecture_report.py", 0o700},
	} {
		content, readErr := assets.ReadFile(file.source)
		if readErr != nil {
			return nil, readErr
		}
		if writeErr := os.WriteFile(filepath.Join(tempDir, file.name), content, file.mode); writeErr != nil {
			return nil, writeErr
		}
	}
	inputPath := filepath.Join(tempDir, "report.json")
	outputPath := filepath.Join(tempDir, "report.html")
	if err := os.WriteFile(inputPath, data, 0o600); err != nil {
		return nil, err
	}
	command, prefix, err := pythonCommand()
	if err != nil {
		return nil, err
	}
	args := append(prefix, filepath.Join(tempDir, "architecture_report.py"), "--input", inputPath, "--output", outputPath)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("architecture renderer failed: %s", message)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("architecture renderer did not produce HTML: %w", err)
	}
	if info.Size() > maxHTMLBytes {
		return nil, fmt.Errorf("architecture report exceeds %d bytes", maxHTMLBytes)
	}
	return os.ReadFile(outputPath)
}

// GenerateArchitectureReport validates a code-architecture-review JSON object
// with the canonical vendored renderer and returns the self-contained HTML.
func GenerateArchitectureReport(ctx context.Context, repoRoot string, report []byte) ([]byte, error) {
	if len(report) == 0 || !json.Valid(report) {
		return nil, fmt.Errorf("architecture report JSON is empty or invalid")
	}
	if len(report) > maxDataBytes {
		return nil, fmt.Errorf("architecture report exceeds %d bytes", maxDataBytes)
	}
	if strings.TrimSpace(repoRoot) == "" {
		repoRoot = os.TempDir()
	}
	return renderArchitectureHTML(ctx, repoRoot, report)
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
