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

const runnerVisibleChunkRunes = 96

type claudeExecStreamState struct {
	answer         strings.Builder
	thinking       strings.Builder
	raw            strings.Builder
	lastText       string
	lastThinking   string
	finalResult    string
	responseEvents int
	thinkingEvents int
	reasoningOpen  bool
	latency        *runnerQALatencyTracker
}

func (d *Daemon) runClaudeQAExec(ctx context.Context, req *QARequest, root string, cfg ExecutorConfig, command string) {
	args := claudeExecArgs(cfg)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Env = os.Environ()

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
		d.sendQAError(req.ID, fmt.Sprintf("start claude: %v", err))
		return
	}
	execStartedAt := time.Now()
	latency := newRunnerQALatencyTracker(req, "runner-claude", "claude", "since_claude_start", execStartedAt)
	latency.logExecStart(command, root, len([]rune(req.Prompt)))

	state := &claudeExecStreamState{latency: latency}
	var mu sync.Mutex
	emitEvent := func(evt QAEvent) {
		evt.ID = req.ID
		_ = d.client.SendQAEvent(evt)
	}
	emitThinking := func(text string) {
		if text == "" {
			return
		}
		latency.logFirstVisible("thinking", text)
		mu.Lock()
		state.thinking.WriteString(text)
		mu.Unlock()
		streamRunnerTextChunks(ctx, text, func(chunk string) {
			emitEvent(QAEvent{EventType: "thinking", Chunk: chunk})
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanClaudeStreamJSON(stdout, func(raw string) {
			mu.Lock()
			state.raw.WriteString(raw)
			state.raw.WriteByte('\n')
			if err := state.processLine(ctx, raw, emitEvent); err != nil {
				mu.Unlock()
				emitThinking(fmt.Sprintf("> 流事件解析已跳过: %s\n", err.Error()))
				return
			}
			mu.Unlock()
		})
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				emitThinking("> " + line + "\n")
			}
		}
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	if ctx.Err() != nil {
		return
	}
	if !state.flushFinalResponse(ctx, emitEvent) {
		return
	}
	if ctx.Err() != nil {
		return
	}
	if waitErr != nil {
		result := state.result()
		if strings.TrimSpace(result.Answer) != "" || strings.TrimSpace(result.Thinking) != "" {
			_ = d.client.SendQAEvent(QAEvent{
				ID: req.ID, EventType: "done",
				Answer: result.Answer, Thinking: result.Thinking, RawOutput: result.RawOutput,
			})
			return
		}
		d.sendQAError(req.ID, fmt.Sprintf("claude failed: %v", waitErr))
		return
	}

	result := state.result()
	if strings.TrimSpace(result.Answer) == "" && strings.TrimSpace(result.Thinking) == "" {
		d.sendQAError(req.ID, "claude returned empty answer")
		return
	}
	_ = d.client.SendQAEvent(QAEvent{
		ID: req.ID, EventType: "done",
		Answer: result.Answer, Thinking: result.Thinking, RawOutput: result.RawOutput,
	})
}

func claudeExecArgs(cfg ExecutorConfig) []string {
	args := append([]string(nil), cfg.Args...)
	if len(args) == 0 {
		args = []string{"-p"}
	}
	if !hasArg(args, "-p") && !hasArg(args, "--print") {
		args = append([]string{"-p"}, args...)
	}
	if !hasArg(args, "--output-format") {
		args = append(args, "--output-format", "stream-json")
	}
	if !hasArg(args, "--verbose") {
		args = append(args, "--verbose")
	}
	if !hasArg(args, "--include-partial-messages") {
		args = append(args, "--include-partial-messages")
	}
	if !hasArg(args, "--allowed-tools") {
		args = append(args, "--allowed-tools", "Read,Grep,Glob,WebFetch,WebSearch,Skill,Agent,Task,Write(*),Edit(*),Bash(*)")
	}
	if !hasArg(args, "--tools") {
		args = append(args, "--tools", "Read,Write,Edit,Grep,Glob,Bash,WebFetch,WebSearch,Skill,Agent,Task")
	}
	return args
}

func scanClaudeStreamJSON(r io.Reader, emit func(raw string)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		raw := scanner.Text()
		if strings.TrimSpace(raw) != "" {
			emit(raw)
		}
	}
}

func (s *claudeExecStreamState) result() qaExecResult {
	full := strings.TrimSpace(s.answer.String())
	final := strings.TrimSpace(s.finalResult)
	answer := full
	if len(final) > len(full) {
		answer = final
	}
	return qaExecResult{
		Answer:    answer,
		Thinking:  strings.TrimSpace(s.thinking.String()),
		RawOutput: s.raw.String(),
	}
}

func (s *claudeExecStreamState) processLine(ctx context.Context, line string, emit func(QAEvent)) error {
	var event claudeStreamEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return err
	}
	switch event.Type {
	case "assistant":
		return s.processAssistant(ctx, event.Message, emit)
	case "result":
		if strings.TrimSpace(event.Result) != "" {
			s.finalResult = event.Result
		}
		if event.Error != "" {
			s.emitThinking(ctx, fmt.Sprintf("> 分析出错: %s\n", event.Error), emit)
		}
	case "system":
	}
	return nil
}

func (s *claudeExecStreamState) processAssistant(ctx context.Context, msg *claudeMessage, emit func(QAEvent)) error {
	if msg == nil || len(msg.Content) == 0 {
		return nil
	}
	var items []claudeContent
	if err := json.Unmarshal(msg.Content, &items); err == nil {
		for _, item := range items {
			s.processContent(ctx, item, emit)
		}
		return nil
	}
	var text string
	if err := json.Unmarshal(msg.Content, &text); err == nil {
		s.emitResponse(ctx, text, emit)
	}
	return nil
}

func (s *claudeExecStreamState) processContent(ctx context.Context, item claudeContent, emit func(QAEvent)) {
	switch item.Type {
	case "text":
		s.emitResponse(ctx, item.Text, emit)
	case "thinking":
		if !s.reasoningOpen {
			s.reasoningOpen = true
			emit(QAEvent{EventType: "reasoning_start"})
		}
		delta := s.emitThinking(ctx, item.Thinking, emit)
		if delta != "" {
			emit(QAEvent{EventType: "reasoning_delta", Text: delta})
		}
	case "tool_use":
		argsJSON := string(item.Input)
		if strings.TrimSpace(argsJSON) == "" {
			argsJSON = "{}"
		}
		s.emitThinking(ctx, fmt.Sprintf("> 使用工具: %s %s\n", item.Name, summarizeRawJSON(item.Input, 180)), emit)
		emit(QAEvent{EventType: "tool_call", ToolName: item.Name, ToolCallID: item.ID, ToolInput: argsJSON})
	case "tool_result":
		s.emitThinking(ctx, fmt.Sprintf("> 工具返回: %s\n", summarizePlainText(item.Content, 180)), emit)
		emit(QAEvent{EventType: "tool_result", ToolCallID: item.ToolUseID, ToolOutput: item.Content})
	}
}

func (s *claudeExecStreamState) emitResponse(ctx context.Context, text string, emit func(QAEvent)) bool {
	if text == "" {
		return true
	}
	chunk := text
	if s.lastText != "" && strings.HasPrefix(text, s.lastText) {
		chunk = text[len(s.lastText):]
	}
	s.lastText = text
	if chunk == "" {
		return true
	}
	s.responseEvents++
	logRunnerChunk("runner-claude", "response", s.responseEvents, chunk)
	return s.emitResponseDelta(ctx, chunk, emit)
}

func (s *claudeExecStreamState) emitResponseDelta(ctx context.Context, text string, emit func(QAEvent)) bool {
	s.latency.logFirstVisible("response", text)
	s.latency.logFirstResponse(text)
	return streamRunnerTextChunks(ctx, text, func(chunk string) {
		s.answer.WriteString(chunk)
		emit(QAEvent{EventType: "response", Chunk: chunk})
	})
}

func (s *claudeExecStreamState) flushFinalResponse(ctx context.Context, emit func(QAEvent)) bool {
	if strings.TrimSpace(s.finalResult) == "" {
		return true
	}
	accumulated := s.answer.String()
	if s.finalResult == accumulated || strings.TrimSpace(s.finalResult) == strings.TrimSpace(accumulated) {
		return true
	}
	if strings.HasPrefix(s.finalResult, accumulated) {
		return s.emitResponseDelta(ctx, s.finalResult[len(accumulated):], emit)
	}
	if strings.TrimSpace(accumulated) != "" {
		return true
	}
	return s.emitResponseDelta(ctx, s.finalResult, emit)
}

func (s *claudeExecStreamState) emitThinking(ctx context.Context, text string, emit func(QAEvent)) string {
	if text == "" {
		return ""
	}
	chunk := text
	if s.lastThinking != "" && strings.HasPrefix(text, s.lastThinking) {
		chunk = text[len(s.lastThinking):]
	}
	s.lastThinking = text
	if chunk == "" {
		return ""
	}
	s.thinkingEvents++
	logRunnerChunk("runner-claude", "thinking", s.thinkingEvents, chunk)
	s.latency.logFirstVisible("thinking", chunk)
	s.thinking.WriteString(chunk)
	streamRunnerTextChunks(ctx, chunk, func(part string) {
		emit(QAEvent{EventType: "thinking", Chunk: part})
	})
	return chunk
}

type qaExecResult struct {
	Answer    string
	Thinking  string
	RawOutput string
}

type claudeStreamEvent struct {
	Type    string         `json:"type"`
	Subtype string         `json:"subtype"`
	Message *claudeMessage `json:"message"`
	Result  string         `json:"result"`
	Error   string         `json:"error"`
	Status  string         `json:"status"`
}

type claudeMessage struct {
	Content json.RawMessage `json:"content"`
}

type claudeContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   string          `json:"content"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
}

func summarizeRawJSON(raw json.RawMessage, limit int) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(trimmed)); err != nil {
		return summarizePlainText(trimmed, limit)
	}
	return summarizePlainText(compact.String(), limit)
}

func summarizePlainText(s string, limit int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "..."
}

func usesClaudeExec(executor, command string) bool {
	name := strings.ToLower(strings.TrimSpace(executor))
	cmdBase := strings.ToLower(filepath.Base(strings.TrimSpace(command)))
	return name == "" || strings.Contains(name, "claude") || strings.Contains(cmdBase, "claude")
}

func streamRunnerTextChunks(ctx context.Context, text string, emit func(string)) bool {
	if text == "" {
		return true
	}
	runes := []rune(text)
	if len(runes) <= runnerVisibleChunkRunes {
		if ctx.Err() != nil {
			return false
		}
		emit(text)
		return true
	}
	for offset := 0; offset < len(runes); {
		if ctx.Err() != nil {
			return false
		}
		end := offset + runnerVisibleChunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		emit(string(runes[offset:end]))
		offset = end
	}
	return true
}

func logRunnerChunk(source, kind string, seq int, text string) {
	chars := len([]rune(text))
	if seq > 3 && chars <= runnerVisibleChunkRunes {
		return
	}
	fmt.Printf("[runner.qa.chunk] source=%s kind=%s upstream_seq=%d upstream_chars=%d upstream_bytes=%d split_chars=%d preview=%q\n",
		source, kind, seq, chars, len(text), runnerVisibleChunkRunes, summarizePlainText(text, 120))
}
