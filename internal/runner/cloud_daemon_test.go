package runner

import (
	"fmt"
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
	}, "/tmp/last-message.txt", false)

	if hasArg(args, "--sandbox") || hasArg(args, "--full-auto") {
		t.Fatalf("expected sandbox flags to be removed, got %#v", args)
	}
	if !hasArg(args, codexBypassSandboxFlag) {
		t.Fatalf("expected codex args to bypass sandbox, got %#v", args)
	}
}

func TestCodexExecArgsUseIsolatedMode(t *testing.T) {
	args := codexExecArgs(ExecutorConfig{Command: "codex"}, "/tmp/last-message.txt", true)

	for _, expected := range []string{"--skip-git-repo-check", "--ephemeral"} {
		if !hasArg(args, expected) {
			t.Fatalf("expected codex args to contain %q, got %#v", expected, args)
		}
	}
	if hasArg(args, "--output-schema") {
		t.Fatalf("custom providers may not support output schemas, got %#v", args)
	}
	if hasArg(args, codexBypassSandboxFlag) || !hasArgValue(args, "--sandbox", "read-only") {
		t.Fatalf("isolated Codex must use the read-only sandbox, got %#v", args)
	}
}

func TestClaudeExecArgsUseReadOnlyToolsForIsolatedAnalysis(t *testing.T) {
	args := claudeExecArgs(ExecutorConfig{Command: "claude", Args: []string{"-p", "--dangerously-skip-permissions"}}, true)
	if hasArg(args, claudeDangerouslySkipPermissionsFlag) || !hasArgValue(args, claudePermissionModeFlag, "dontAsk") {
		t.Fatalf("isolated Claude permissions = %#v", args)
	}
	if !hasArgValue(args, "--tools", "Read,Grep,Glob,Bash") || !hasArgValueContaining(args, "--allowed-tools", "Bash(git -C * diff *)") {
		t.Fatalf("isolated Claude tools = %#v", args)
	}
}

func hasArgValue(args []string, key, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == key && args[index+1] == value {
			return true
		}
	}
	return false
}

func hasArgValueContaining(args []string, key, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == key && strings.Contains(args[index+1], value) {
			return true
		}
	}
	return false
}

func TestIsolatedQAExecutionPromptNamesTargetWithoutProjectDiscovery(t *testing.T) {
	prompt := isolatedQAExecutionPrompt("/work/repo", "analyze changes")

	for _, expected := range []string{"/work/repo", "git -C", "Do not discover", "analyze changes"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("expected isolated prompt to contain %q, got %q", expected, prompt)
		}
	}
}

func TestCodexFinalAnswerRejectsProgressWithoutStructuredResult(t *testing.T) {
	_, err := codexFinalAnswer(true, "", "I am inspecting the diff")
	if err == nil || !strings.Contains(err.Error(), "without a final structured response") {
		t.Fatalf("expected missing final response error, got %v", err)
	}
}

func TestCodexFinalAnswerUsesStructuredResultInsteadOfProgress(t *testing.T) {
	answer, err := codexFinalAnswer(true, `{"version":2}`, "I am inspecting the diff")
	if err != nil {
		t.Fatal(err)
	}
	if answer != `{"version":2}` {
		t.Fatalf("expected final JSON answer, got %q", answer)
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

func TestParseCodexExecJSONEventExtractsExecutorError(t *testing.T) {
	evt := parseCodexExecJSONEvent(`{"type":"error","message":"unexpected status 502 Bad Gateway"}`)

	if evt.EventType != "executor_error" {
		t.Fatalf("expected executor_error, got %#v", evt)
	}
	if evt.Text != "unexpected status 502 Bad Gateway" {
		t.Fatalf("unexpected executor error: %q", evt.Text)
	}
}

func TestParseCodexExecJSONEventExtractsTurnFailureError(t *testing.T) {
	evt := parseCodexExecJSONEvent(`{"type":"turn.failed","error":{"message":"upstream request failed"}}`)

	if evt.EventType != "turn_done" || !evt.Failed {
		t.Fatalf("expected failed turn, got %#v", evt)
	}
	if evt.Text != "upstream request failed" {
		t.Fatalf("unexpected turn failure error: %q", evt.Text)
	}
}

func TestCodexFailureMessagePrefersExecutorDetail(t *testing.T) {
	got := codexFailureMessage("unexpected status 502 Bad Gateway", fmt.Errorf("exit status 1"))
	if got != "unexpected status 502 Bad Gateway" {
		t.Fatalf("expected executor detail, got %q", got)
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

func TestCodexToolCallThinkingShowsFullFixforgeQuerySQL(t *testing.T) {
	sqlText := "SELECT user_id, action, event_time FROM dwd_spock_action_1h WHERE ds = '2026-07-20' AND action IN ('open', 'purchase') ORDER BY event_time DESC"
	text := codexToolCallThinking(codexExecParsedEvent{
		ToolName:  "mcp__fixforge__prod_maxcompute__query",
		ToolInput: `{"sql":"` + sqlText + `"}`,
	})

	if !strings.Contains(text, "```sql\n"+sqlText+"\n```") {
		t.Fatalf("query SQL was abbreviated or missing: %q", text)
	}
}
