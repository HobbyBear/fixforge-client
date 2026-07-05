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
}
