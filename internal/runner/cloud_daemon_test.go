package runner

import (
	"strings"
	"testing"
)

func TestBuildTmuxExecutorScriptFeedsPromptFromFile(t *testing.T) {
	script := buildTmuxExecutorScript("claude", []string{"-p"}, "/tmp/terminal.log", "/tmp/executor.exit", "/tmp/prompt.txt")

	if !strings.Contains(script, `< "/tmp/prompt.txt"`) {
		t.Fatalf("expected script to read prompt from file, got %q", script)
	}
	if strings.Contains(script, "<prompt>") {
		t.Fatalf("script should not embed prompt placeholder, got %q", script)
	}
}

func TestNormalizeExecutorArgsAddsCodexStdinPrompt(t *testing.T) {
	args := normalizeExecutorArgs("codex", ExecutorConfig{Command: "codex", Args: []string{"exec"}})

	if !hasArg(args, "-") {
		t.Fatalf("expected codex args to include stdin prompt marker, got %#v", args)
	}
	if !hasArg(args, codexBypassSandboxFlag) {
		t.Fatalf("expected codex args to bypass sandbox on local runner, got %#v", args)
	}
}

func TestNormalizeExecutorArgsBypassesClaudePermissions(t *testing.T) {
	args := normalizeExecutorArgs("claude", ExecutorConfig{
		Command: "claude",
		Args:    []string{"-p", "--permission-mode", "default"},
	})

	if !hasArg(args, claudeDangerouslySkipPermissionsFlag) {
		t.Fatalf("expected claude args to skip permissions on local runner, got %#v", args)
	}
	if !hasClaudeBypassPermissionMode(args) {
		t.Fatalf("expected claude args to use bypass permission mode, got %#v", args)
	}
	for i, arg := range args {
		if arg == claudePermissionModeFlag && i+1 < len(args) && args[i+1] == "default" {
			t.Fatalf("expected old claude permission mode to be removed, got %#v", args)
		}
	}
}

func TestCodexExecArgsBypassSandboxAndDropSandboxFlags(t *testing.T) {
	args := codexExecArgs(ExecutorConfig{
		Command: "codex",
		Args:    []string{"exec", "--sandbox", "read-only", "--full-auto"},
	}, "/tmp/last-message.txt")

	if hasArg(args, "--sandbox") || hasArg(args, "--full-auto") {
		t.Fatalf("expected sandbox flags to be removed, got %#v", args)
	}
	if !hasArg(args, codexBypassSandboxFlag) {
		t.Fatalf("expected codex args to bypass sandbox, got %#v", args)
	}
}

func TestParseCodexExecJSONEventExtractsNestedReasoning(t *testing.T) {
	evt := parseCodexExecJSONEvent(`{"type":"response_item","payload":{"type":"reasoning","summary":[{"text":"checking files"}]}}`)

	if evt.EventType != "reasoning_delta" {
		t.Fatalf("expected reasoning_delta, got %#v", evt)
	}
	if evt.Text != "checking files" {
		t.Fatalf("unexpected reasoning text: %q", evt.Text)
	}
}

func TestParseCodexExecJSONEventExtractsThinkingDelta(t *testing.T) {
	evt := parseCodexExecJSONEvent(`{"type":"stream_event","delta":{"type":"thinking_delta","thinking":"looking at logs"}}`)

	if evt.EventType != "reasoning_delta" {
		t.Fatalf("expected reasoning_delta, got %#v", evt)
	}
	if evt.Text != "looking at logs" {
		t.Fatalf("unexpected thinking text: %q", evt.Text)
	}
}

func TestParseCodexExecJSONEventKeepsOutputTextAsResponse(t *testing.T) {
	evt := parseCodexExecJSONEvent(`{"type":"response_item","payload":{"type":"message","content":[{"type":"output_text","text":"final answer"}]}}`)

	if evt.EventType != "response" {
		t.Fatalf("expected response, got %#v", evt)
	}
	if evt.Text != "final answer" {
		t.Fatalf("unexpected response text: %q", evt.Text)
	}
}

func TestParseCodexExecJSONEventExtractsToolResult(t *testing.T) {
	evt := parseCodexExecJSONEvent(`{"type":"tool_result","tool_call_id":"call_1","content":"file contents"}`)

	if evt.EventType != "tool_result" {
		t.Fatalf("expected tool_result, got %#v", evt)
	}
	if evt.ToolOutput != "file contents" {
		t.Fatalf("unexpected tool output: %q", evt.ToolOutput)
	}
}

func TestCodexToolCallThinkingMatchesClaudeStyle(t *testing.T) {
	text := codexToolCallThinking(codexExecParsedEvent{
		ToolName:  "shell",
		ToolInput: `/usr/bin/zsh -lc "sed -n '18,199p' service/vip.go"`,
	})

	if !strings.HasPrefix(text, "> 使用工具: shell ") {
		t.Fatalf("expected claude-style tool thinking, got %q", text)
	}
	if !strings.Contains(text, "service/vip.go") {
		t.Fatalf("expected command preview in thinking, got %q", text)
	}
}
