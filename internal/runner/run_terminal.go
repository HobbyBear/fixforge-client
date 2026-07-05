package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var runTerminalNameRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func runTerminalNames(runID string) (socketPath, tmuxSession string) {
	safe := safeRunTerminalName(runID)
	return filepath.Join(os.TempDir(), fmt.Sprintf("fixforge-run-%s.sock", safe)), "ff_run_" + safe
}

func safeRunTerminalName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "unknown"
	}
	value = runTerminalNameRe.ReplaceAllString(value, "_")
	if _, err := strconv.Atoi(value[:1]); err == nil {
		value = "r_" + value
	}
	if len(value) > 80 {
		value = value[:80]
	}
	return value
}

func shellQuote(value string) string {
	return strconv.Quote(value)
}
