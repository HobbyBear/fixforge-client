package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type codexExecParsedEvent struct {
	EventType  string
	Text       string
	ToolName   string
	ToolID     string
	ToolInput  string
	ToolOutput string
	Terminal   bool
	Failed     bool
}

func usesCodexExec(executor, command string) bool {
	name := strings.ToLower(strings.TrimSpace(executor))
	cmdBase := strings.ToLower(filepath.Base(strings.TrimSpace(command)))
	return strings.Contains(name, "codex") || strings.Contains(cmdBase, "codex")
}

func (d *Daemon) runCodexQAExec(ctx context.Context, req *QARequest, root string, cfg ExecutorConfig, command string) {
	tmpDir, err := os.MkdirTemp("", "fixforge-codex-qa-*")
	if err != nil {
		d.sendQAError(req.ID, err.Error())
		return
	}
	defer os.RemoveAll(tmpDir)

	lastMessagePath := filepath.Join(tmpDir, "last-message.txt")
	args := codexExecArgs(cfg, lastMessagePath)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(req.Prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		d.sendQAError(req.ID, err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		d.sendQAError(req.ID, err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		d.sendQAError(req.ID, fmt.Sprintf("start codex: %v", err))
		return
	}
	execStartedAt := time.Now()
	latency := newRunnerQALatencyTracker(req, runnerQASourceClientRunner, "codex", "since_exec_start", execStartedAt)
	latency.logExecStart(command, root, len([]rune(req.Prompt)))
	terminalOutput := newQATerminalOutputEmitter(d.client, req)
	defer terminalOutput.Close()

	var accumAnswer, accumThinking strings.Builder
	var rawOutput strings.Builder
	events := make(chan codexExecParsedEvent, 64)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanCodexJSONL(stdout, func(raw string, evt codexExecParsedEvent) {
			terminalOutput.EmitLine(raw)
			rawOutput.WriteString(raw)
			rawOutput.WriteByte('\n')
			events <- evt
		})
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			terminalOutput.EmitLine(line)
			if strings.TrimSpace(line) == "" {
				continue
			}
			if isIgnorableCodexStderr(line) {
				d.logger.Debug("suppressed codex stderr", "line", line)
				continue
			}
			events <- codexExecParsedEvent{EventType: "thinking", Text: line + "\n"}
		}
	}()

	waitCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		wg.Wait()
		close(events)
		waitCh <- err
	}()

	reasoningOpen := false
	turnBoundarySeen := false
	failedTurn := false
	responseEvents := 0
	thinkingEvents := 0
	reasoningEvents := 0
	seenToolCalls := map[string]bool{}
	sendResponse := func(text string) bool {
		latency.logFirstVisible("response", text)
		latency.logFirstResponse(text)
		responseEvents++
		logRunnerChunk(runnerQASourceClientRunner, "codex", req.SessionID, req.ID, "response", responseEvents, text)
		return streamRunnerTextChunks(ctx, text, func(chunk string) {
			_ = d.client.SendQAEvent(QAEvent{ID: req.ID, EventType: "response", Chunk: chunk})
			accumAnswer.WriteString(chunk)
		})
	}
	sendThinking := func(text string) bool {
		latency.logFirstVisible("thinking", text)
		thinkingEvents++
		logRunnerChunk(runnerQASourceClientRunner, "codex", req.SessionID, req.ID, "thinking", thinkingEvents, text)
		return streamRunnerTextChunks(ctx, text, func(chunk string) {
			_ = d.client.SendQAEvent(QAEvent{ID: req.ID, EventType: "thinking", Chunk: chunk})
			accumThinking.WriteString(chunk)
		})
	}
	sendReasoning := func(text string) bool {
		latency.logFirstVisible("reasoning_delta", text)
		reasoningEvents++
		logRunnerChunk(runnerQASourceClientRunner, "codex", req.SessionID, req.ID, "reasoning_delta", reasoningEvents, text)
		return streamRunnerTextChunks(ctx, text, func(chunk string) {
			_ = d.client.SendQAEvent(QAEvent{ID: req.ID, EventType: "reasoning_delta", Text: chunk})
			accumThinking.WriteString(chunk)
		})
	}
	for evt := range events {
		switch evt.EventType {
		case "reasoning_delta":
			if !reasoningOpen {
				reasoningOpen = true
				_ = d.client.SendQAEvent(QAEvent{ID: req.ID, EventType: "reasoning_start"})
			}
			if !sendReasoning(evt.Text) {
				return
			}
		case "thinking":
			if !sendThinking(evt.Text) {
				return
			}
		case "response":
			if !sendResponse(evt.Text) {
				return
			}
		case "tool_call":
			signature := codexToolSignature(evt)
			if signature != "" && seenToolCalls[signature] {
				continue
			}
			if signature != "" {
				seenToolCalls[signature] = true
			}
			if !sendThinking(codexToolCallThinking(evt)) {
				return
			}
			_ = d.client.SendQAEvent(QAEvent{
				ID: req.ID, EventType: "tool_call",
				ToolName: evt.ToolName, ToolCallID: evt.ToolID, ToolInput: evt.ToolInput,
			})
		case "tool_result":
			if !sendThinking(codexToolResultThinking(evt)) {
				return
			}
			_ = d.client.SendQAEvent(QAEvent{
				ID: req.ID, EventType: "tool_result",
				ToolCallID: evt.ToolID, ToolOutput: evt.ToolOutput,
			})
		case "turn_done":
			turnBoundarySeen = true
			failedTurn = failedTurn || evt.Failed
		}
	}
	waitErr := <-waitCh

	if finalBytes, readErr := os.ReadFile(lastMessagePath); readErr == nil {
		final := strings.TrimSpace(string(finalBytes))
		if final != "" && final != strings.TrimSpace(accumAnswer.String()) {
			current := accumAnswer.String()
			switch {
			case current == "":
				if !sendResponse(final) {
					return
				}
			case strings.HasPrefix(final, current):
				if !sendResponse(final[len(current):]) {
					return
				}
			default:
				accumAnswer.Reset()
				accumAnswer.WriteString(final)
			}
		}
	}
	answer := strings.TrimSpace(accumAnswer.String())
	thinking := strings.TrimSpace(accumThinking.String())
	if waitErr != nil || failedTurn {
		msg := strings.TrimSpace(waitErrString(waitErr))
		if msg == "" {
			msg = "codex turn failed"
		}
		if answer != "" || thinking != "" {
			_ = d.client.SendQAEvent(QAEvent{
				ID: req.ID, EventType: "done",
				Answer: answer, Thinking: thinking, RawOutput: rawOutput.String(),
			})
			return
		}
		d.sendQAError(req.ID, msg)
		return
	}
	if answer == "" && thinking == "" {
		d.sendQAError(req.ID, "codex returned empty answer")
		return
	}
	_ = d.client.SendQAEvent(QAEvent{
		ID: req.ID, EventType: "done",
		Answer: answer, Thinking: thinking,
		RawOutput: fmt.Sprintf("codex exec json turn_boundary=%t", turnBoundarySeen),
	})
}

func waitErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func isIgnorableCodexStderr(line string) bool {
	line = strings.ToLower(strings.TrimSpace(line))
	return strings.Contains(line, "codex_core::session") &&
		strings.Contains(line, "failed to record rollout items") &&
		strings.Contains(line, "thread") &&
		strings.Contains(line, "not found")
}

func codexExecArgs(cfg ExecutorConfig, lastMessagePath string) []string {
	args := append([]string(nil), cfg.Args...)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		args = append([]string{"exec"}, args...)
	}
	args = forceCodexNoSandbox(args)
	if !hasArg(args, "--json") {
		args = append(args, "--json")
	}
	if !hasArg(args, "--output-last-message") && !hasArg(args, "-o") {
		args = append(args, "--output-last-message", lastMessagePath)
	}
	hasPrompt := false
	for _, arg := range args {
		if arg == "-" {
			hasPrompt = true
			break
		}
	}
	if !hasPrompt {
		args = append(args, "-")
	}
	return args
}

const codexBypassSandboxFlag = "--dangerously-bypass-approvals-and-sandbox"

func forceCodexNoSandbox(args []string) []string {
	out := make([]string, 0, len(args)+1)
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		switch {
		case arg == "--sandbox" || arg == "-s":
			skipNext = true
			continue
		case strings.HasPrefix(arg, "--sandbox="):
			continue
		case arg == "--full-auto":
			continue
		}
		out = append(out, arg)
	}
	if !hasArg(out, codexBypassSandboxFlag) {
		out = append(out, codexBypassSandboxFlag)
	}
	return out
}

func scanCodexJSONL(r io.Reader, emit func(raw string, evt codexExecParsedEvent)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		raw := scanner.Text()
		evt := parseCodexExecJSONEvent(raw)
		if evt.EventType == "" {
			continue
		}
		emit(raw, evt)
	}
}

func parseCodexExecJSONEvent(raw string) codexExecParsedEvent {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return codexExecParsedEvent{}
	}
	name := firstString(m, "type", "method", "event")
	switch normalizeCodexEventName(name) {
	case "turn_completed":
		return codexExecParsedEvent{EventType: "turn_done", Terminal: true, Failed: codexTurnFailed(m)}
	case "turn_failed":
		return codexExecParsedEvent{EventType: "turn_done", Terminal: true, Failed: true}
	}
	if codexTurnFailed(m) {
		return codexExecParsedEvent{EventType: "turn_done", Terminal: true, Failed: true}
	}

	if output := codexToolOutput(m); output != "" {
		return codexExecParsedEvent{
			EventType:  "tool_result",
			ToolID:     firstString(m, "id", "call_id", "tool_call_id"),
			ToolOutput: output,
		}
	}
	if text, reasoning := codexEventText(m, name); text != "" {
		if reasoning {
			return codexExecParsedEvent{EventType: "reasoning_delta", Text: text}
		}
		return codexExecParsedEvent{EventType: "response", Text: text}
	}
	if toolName := codexToolName(m); toolName != "" {
		return codexExecParsedEvent{
			EventType: "tool_call",
			ToolName:  toolName,
			ToolID:    firstString(m, "id", "call_id", "tool_call_id"),
			ToolInput: codexToolInput(m),
		}
	}
	return codexExecParsedEvent{}
}

func normalizeCodexEventName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, ".", "_")
	name = strings.ReplaceAll(name, "-", "_")
	return name
}

func codexTurnFailed(m map[string]any) bool {
	if status := strings.ToLower(firstString(m, "status")); status == "failed" || status == "errored" {
		return true
	}
	if turn, ok := m["turn"].(map[string]any); ok {
		status := strings.ToLower(firstString(turn, "status", "type"))
		if status == "failed" || status == "errored" {
			return true
		}
		if turn["error"] != nil {
			return true
		}
	}
	return m["error"] != nil
}

func codexEventText(m map[string]any, eventName string) (string, bool) {
	reasoning := codexLooksLikeReasoning(eventName)
	if text := codexTextFromValue(m, reasoning); text != "" {
		return text, reasoning || codexValueLooksLikeReasoning(m)
	}
	for _, key := range []string{"payload", "item", "delta", "message", "response"} {
		if nested, ok := m[key].(map[string]any); ok {
			nestedReasoning := reasoning || codexValueLooksLikeReasoning(nested)
			if text := codexTextFromValue(nested, nestedReasoning); text != "" {
				return text, nestedReasoning
			}
		}
	}
	return "", false
}

func codexTextFromValue(v any, reasoning bool) string {
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) == "" {
			return ""
		}
		return x
	case []any:
		var out []string
		for _, item := range x {
			if text := codexTextFromValue(item, reasoning || codexValueLooksLikeReasoning(item)); text != "" {
				out = append(out, text)
			}
		}
		return strings.Join(out, "")
	case map[string]any:
		localReasoning := reasoning || codexValueLooksLikeReasoning(x)
		textKeys := []string{"text", "message", "delta", "answer"}
		if localReasoning {
			textKeys = append([]string{"thinking", "reasoning", "analysis", "summary", "content"}, textKeys...)
		} else {
			textKeys = append(textKeys, "content")
		}
		for _, key := range textKeys {
			if key == "encrypted_content" {
				continue
			}
			if text := codexTextFromValue(x[key], localReasoning); text != "" {
				return text
			}
		}
		for _, key := range []string{"payload", "item", "delta", "message", "response"} {
			if text := codexTextFromValue(x[key], localReasoning); text != "" {
				return text
			}
		}
	}
	return ""
}

func codexValueLooksLikeReasoning(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range []string{"type", "event", "method", "phase", "name"} {
			if codexLooksLikeReasoning(firstString(x, key)) {
				return true
			}
		}
		if delta, ok := x["delta"].(map[string]any); ok && codexValueLooksLikeReasoning(delta) {
			return true
		}
		if item, ok := x["item"].(map[string]any); ok && codexValueLooksLikeReasoning(item) {
			return true
		}
		if payload, ok := x["payload"].(map[string]any); ok && codexValueLooksLikeReasoning(payload) {
			return true
		}
	}
	return false
}

func codexLooksLikeReasoning(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(value, "reason") ||
		strings.Contains(value, "thinking") ||
		strings.Contains(value, "analysis")
}

func codexToolName(m map[string]any) string {
	if s := firstString(m, "tool_name", "name"); s != "" {
		return s
	}
	if item, ok := m["item"].(map[string]any); ok {
		itemType := strings.ToLower(firstString(item, "type"))
		if strings.Contains(itemType, "tool") || strings.Contains(itemType, "command") {
			return firstString(item, "tool_name", "name", "command")
		}
	}
	return ""
}

func codexToolInput(m map[string]any) string {
	for _, key := range []string{"input", "arguments", "command"} {
		if v, ok := m[key]; ok {
			return jsonish(v)
		}
	}
	if item, ok := m["item"].(map[string]any); ok {
		for _, key := range []string{"input", "arguments", "command"} {
			if v, ok := item[key]; ok {
				return jsonish(v)
			}
		}
	}
	return ""
}

func codexToolOutput(m map[string]any) string {
	if !codexLooksLikeToolResult(m) {
		return ""
	}
	for _, key := range []string{"output", "result", "stdout", "stderr", "content"} {
		if v, ok := m[key]; ok {
			return jsonish(v)
		}
	}
	for _, key := range []string{"item", "payload", "message", "response"} {
		if nested, ok := m[key].(map[string]any); ok {
			for _, outKey := range []string{"output", "result", "stdout", "stderr", "content"} {
				if v, ok := nested[outKey]; ok {
					return jsonish(v)
				}
			}
		}
	}
	return ""
}

func codexLooksLikeToolResult(m map[string]any) bool {
	for _, key := range []string{"type", "event", "method", "phase", "name"} {
		value := strings.ToLower(firstString(m, key))
		if strings.Contains(value, "tool_result") ||
			strings.Contains(value, "tool_output") ||
			strings.Contains(value, "command_output") ||
			strings.Contains(value, "exec_output") {
			return true
		}
	}
	for _, key := range []string{"item", "payload", "message", "response"} {
		if nested, ok := m[key].(map[string]any); ok && codexLooksLikeToolResult(nested) {
			return true
		}
	}
	return false
}

func codexToolSignature(evt codexExecParsedEvent) string {
	parts := []string{
		strings.TrimSpace(evt.ToolID),
		strings.TrimSpace(evt.ToolName),
		strings.TrimSpace(evt.ToolInput),
	}
	return strings.Join(parts, "\x00")
}

func codexToolCallThinking(evt codexExecParsedEvent) string {
	name := strings.TrimSpace(evt.ToolName)
	if isFixforgeDataQueryTool(name) {
		if sqlText, ok := codexToolSQL(evt.ToolInput); ok {
			return fmt.Sprintf("> 使用工具: %s\n\n```sql\n%s\n```\n", name, sqlText)
		}
		return fmt.Sprintf("> 使用工具: %s\n\n```json\n%s\n```\n", name, formatCodexToolJSON(evt.ToolInput))
	}
	input := summarizePlainText(evt.ToolInput, 180)
	if name == "" && input == "" {
		return ""
	}
	if input == "" {
		return fmt.Sprintf("> 使用工具: %s\n", name)
	}
	if name == "" {
		return fmt.Sprintf("> 使用工具: %s\n", input)
	}
	return fmt.Sprintf("> 使用工具: %s %s\n", name, input)
}

func codexToolSQL(input string) (string, bool) {
	var arguments map[string]any
	if err := json.Unmarshal([]byte(input), &arguments); err != nil {
		return "", false
	}
	sqlText, found := arguments["sql"].(string)
	return strings.TrimSpace(sqlText), found && strings.TrimSpace(sqlText) != ""
}

func formatCodexToolJSON(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "{}"
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, []byte(trimmed), "", "  "); err == nil {
		return formatted.String()
	}
	return trimmed
}

func codexToolResultThinking(evt codexExecParsedEvent) string {
	output := summarizePlainText(evt.ToolOutput, 180)
	if output == "" {
		return ""
	}
	return fmt.Sprintf("> 工具返回: %s\n", output)
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok {
			return s
		}
		if nested, ok := m[key].(map[string]any); ok {
			if s := firstString(nested, "type", "status", "text", "message"); s != "" {
				return s
			}
		}
	}
	return ""
}

func jsonish(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
