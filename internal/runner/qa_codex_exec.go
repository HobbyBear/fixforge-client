package runner

import (
	"bufio"
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
	EventType string
	Text      string
	ToolName  string
	ToolID    string
	ToolInput string
	Terminal  bool
	Failed    bool
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
	latency := newRunnerQALatencyTracker(req, "runner-codex", "codex", "since_exec_start", execStartedAt)
	latency.logExecStart(command, root, len([]rune(req.Prompt)))

	var accumAnswer, accumThinking strings.Builder
	var rawOutput strings.Builder
	events := make(chan codexExecParsedEvent, 64)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanCodexJSONL(stdout, func(raw string, evt codexExecParsedEvent) {
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
	sendResponse := func(text string) bool {
		latency.logFirstVisible("response", text)
		latency.logFirstResponse(text)
		responseEvents++
		logRunnerChunk("runner-codex", "response", responseEvents, text)
		return streamRunnerTextChunks(ctx, text, func(chunk string) {
			_ = d.client.SendQAEvent(QAEvent{ID: req.ID, EventType: "response", Chunk: chunk})
			accumAnswer.WriteString(chunk)
		})
	}
	sendThinking := func(text string) bool {
		latency.logFirstVisible("thinking", text)
		thinkingEvents++
		logRunnerChunk("runner-codex", "thinking", thinkingEvents, text)
		return streamRunnerTextChunks(ctx, text, func(chunk string) {
			_ = d.client.SendQAEvent(QAEvent{ID: req.ID, EventType: "thinking", Chunk: chunk})
			accumThinking.WriteString(chunk)
		})
	}
	sendReasoning := func(text string) bool {
		latency.logFirstVisible("reasoning_delta", text)
		reasoningEvents++
		logRunnerChunk("runner-codex", "reasoning_delta", reasoningEvents, text)
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
			_ = d.client.SendQAEvent(QAEvent{
				ID: req.ID, EventType: "tool_call",
				ToolName: evt.ToolName, ToolCallID: evt.ToolID, ToolInput: evt.ToolInput,
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

	if text := codexEventText(m); text != "" {
		if strings.Contains(strings.ToLower(name), "reason") {
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

func codexEventText(m map[string]any) string {
	for _, key := range []string{"message", "text", "delta", "content", "answer"} {
		if s, ok := m[key].(string); ok && s != "" {
			return s
		}
	}
	item, _ := m["item"].(map[string]any)
	if item != nil {
		for _, key := range []string{"message", "text", "content"} {
			if s, ok := item[key].(string); ok && s != "" {
				return s
			}
		}
		if parts, ok := item["content"].([]any); ok {
			var out []string
			for _, part := range parts {
				pm, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if s := firstString(pm, "text", "message", "content"); s != "" {
					out = append(out, s)
				}
			}
			return strings.Join(out, "")
		}
	}
	return ""
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
