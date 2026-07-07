package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type CloudDaemon struct {
	cfg    *Config
	client *CloudClient
	logger *slog.Logger

	sem     chan struct{}
	mu      sync.Mutex
	running map[string]Runtime
	seq     atomic.Int64
}

type Runtime struct {
	RunID     string    `json:"runId"`
	TaskID    string    `json:"taskId"`
	Stage     string    `json:"stage"`
	Status    string    `json:"status"`
	Worktree  string    `json:"worktree"`
	Branch    string    `json:"branch"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

func NewCloudDaemon(cfg *Config, logger *slog.Logger) *CloudDaemon {
	cfg.Normalize()
	return &CloudDaemon{
		cfg:     cfg,
		client:  NewCloudClient(cfg.ServerURL, cfg.Token),
		logger:  logger,
		sem:     make(chan struct{}, cfg.MaxConcurrentRuns),
		running: map[string]Runtime{},
	}
}

func (d *CloudDaemon) Run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return d.RunContext(ctx)
}

func (d *CloudDaemon) RunContext(ctx context.Context) error {
	if d.cfg.Token == "" {
		return fmt.Errorf("runner not configured - set token in %s", DefaultConfigPath())
	}
	if d.cfg.ServerURL == "" {
		return fmt.Errorf("serverUrl is required")
	}
	if err := d.client.Register(ctx, d.cfg); err != nil {
		return fmt.Errorf("register runner: %w", err)
	}
	d.logger.Info("runner registered", "runner_id", d.cfg.RunnerID, "server", d.cfg.ServerURL)

	heartbeat := time.NewTicker(15 * time.Second)
	poll := time.NewTicker(3 * time.Second)
	defer heartbeat.Stop()
	defer poll.Stop()
	_ = d.sendHeartbeat(ctx)
	for {
		select {
		case <-ctx.Done():
			_ = d.sendHeartbeat(context.Background())
			return nil
		case <-heartbeat.C:
			_ = d.sendHeartbeat(ctx)
		case <-poll.C:
			d.pollOnce(ctx)
		}
	}
}

func (d *CloudDaemon) pollOnce(ctx context.Context) {
	if len(d.sem) >= cap(d.sem) {
		return
	}
	runs, err := d.client.PollRuns(ctx, d.cfg.RunnerID)
	if err != nil {
		d.logger.Warn("poll runs failed", "error", err)
		return
	}
	for _, run := range runs {
		if len(d.sem) >= cap(d.sem) {
			return
		}
		if d.isRunning(run.RunID) {
			continue
		}
		claimed, err := d.client.ClaimRun(ctx, run.RunID, d.cfg.RunnerID)
		if err != nil {
			d.logger.Warn("claim run failed", "run_id", run.RunID, "error", err)
			continue
		}
		d.sem <- struct{}{}
		go func(r CloudRun) {
			defer func() { <-d.sem }()
			d.executeRun(context.Background(), r)
		}(*claimed)
	}
}

func (d *CloudDaemon) executeRun(ctx context.Context, run CloudRun) {
	project := d.findProject(run.ProjectID)
	if project == nil {
		_ = d.client.SendEvent(ctx, run.RunID, "status", "failed", "project not configured: "+run.ProjectID)
		return
	}
	executor := run.Executor
	if strings.TrimSpace(executor) == "" {
		executor = "claude"
	}
	runDir := filepath.Join(project.LocalPath, ".fixforge", "runs", safePathName(run.RunID))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		_ = d.client.SendEvent(ctx, run.RunID, "status", "failed", "create run dir: "+err.Error())
		return
	}
	d.writeRunJSON(runDir, run, Runtime{RunID: run.RunID, TaskID: run.TaskID, Stage: run.Stage, Status: "preparing", StartedAt: time.Now()})

	_ = d.client.SendEvent(ctx, run.RunID, "status", "preparing", "preparing local workspace")
	gitOps := NewGitOps(project.LocalPath, d.logger)
	workspace := project.LocalPath
	branch := gitOps.CurrentBranch()
	if branch == "" {
		branch = strings.TrimSpace(run.FeatureBranch)
	}
	if branch == "" {
		branch = strings.TrimSpace(run.BaseBranch)
	}
	workdir := workspace
	if project.RepoAppPath != "" {
		workdir = filepath.Join(workspace, project.RepoAppPath)
	}
	if info, err := os.Stat(workdir); err != nil || !info.IsDir() {
		msg := "workspace directory not found: " + workdir
		if err != nil {
			msg = msg + ": " + err.Error()
		}
		_ = d.client.SendEvent(ctx, run.RunID, "status", "failed", msg)
		return
	}
	rt := Runtime{RunID: run.RunID, TaskID: run.TaskID, Stage: run.Stage, Status: "running", Worktree: workspace, Branch: branch, StartedAt: time.Now()}
	d.setRuntime(run.RunID, rt)
	defer d.clearRuntime(run.RunID)
	d.writeRunJSON(runDir, run, rt)

	_ = d.client.SendRuntime(ctx, run.RunID, "running", "executor started", workspace, branch)
	execErr := d.runExecutor(ctx, runDir, workdir, executor, run.Prompt, run.RunID, &rt)
	if execErr != nil {
		_ = d.client.SendEvent(ctx, run.RunID, "status", "failed", execErr.Error())
	}

	testResult := ""
	diffStat, _ := gitOps.DiffStat(workspace)
	changedFiles := gitOps.ChangedFiles(workspace)
	diffSummary := gitOps.DiffSummary(workspace)
	commitHash := ""
	status := "completed"
	risk := ""
	if execErr != nil {
		status = "failed"
		risk = execErr.Error()
	}
	result := RunResult{
		RunID:        run.RunID,
		Status:       status,
		Summary:      "run " + status,
		ChangedFiles: changedFiles,
		DiffStat:     diffStat,
		DiffSummary:  diffSummary,
		TestResult:   testResult,
		Risk:         risk,
		CommitHash:   commitHash,
	}
	d.writeJSONFile(filepath.Join(runDir, "result.json"), result)
	if err := d.client.SendResult(ctx, result); err != nil {
		d.logger.Warn("upload result failed", "run_id", run.RunID, "error", err)
	}
}

func (d *CloudDaemon) runExecutor(ctx context.Context, runDir, workdir, executor, prompt, runID string, rt *Runtime) error {
	cfg, command, ok := BuiltinExecutor(executor)
	if !ok || command == "" {
		return fmt.Errorf("unknown executor: %s", executor)
	}
	args := normalizeExecutorArgs(executor, cfg)
	displayArgs := append([]string{}, args...)
	displayArgs = append(displayArgs, "<prompt>")
	d.writeRunEvent(ctx, runDir, runID, "executor_start", "starting "+executor+" executor")
	d.writeRunLogLine(ctx, runDir, runID, "stdout", fmt.Sprintf("[fixforge] executor: %s %s", command, strings.Join(displayArgs, " ")))
	d.writeRunLogLine(ctx, runDir, runID, "stdout", "[fixforge] workdir: "+workdir)
	promptFile := filepath.Join(runDir, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		d.writeRunEvent(ctx, runDir, runID, "executor_error", "write prompt failed: "+err.Error())
		return fmt.Errorf("write prompt file: %w", err)
	}
	d.writeRunLogLine(ctx, runDir, runID, "stdout", "[fixforge] prompt: "+promptFile)

	socketPath, tmuxSession := runTerminalNames(runID)
	terminalLog := filepath.Join(runDir, "terminal.log")
	exitFile := filepath.Join(runDir, "executor.exit")
	_ = os.Remove(exitFile)
	_ = os.WriteFile(terminalLog, nil, 0o644)
	script := buildTmuxExecutorScript(command, args, terminalLog, exitFile, promptFile)
	cmd := exec.CommandContext(ctx, "tmux", "-S", socketPath, "new-session", "-d", "-s", tmuxSession, "-c", workdir, "bash", "-lc", script)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	if out, err := cmd.CombinedOutput(); err != nil {
		d.writeRunEvent(ctx, runDir, runID, "executor_error", "start executor failed: "+err.Error())
		return fmt.Errorf("tmux new-session failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	_ = exec.CommandContext(ctx, "tmux", "-S", socketPath, "set-option", "-t", tmuxSession, "mouse", "on").Run()
	_ = exec.CommandContext(ctx, "tmux", "-S", socketPath, "set-option", "-t", tmuxSession, "status", "off").Run()
	if out, err := exec.CommandContext(ctx, "tmux", "-S", socketPath, "display-message", "-p", "-t", tmuxSession, "#{pane_pid}").Output(); err == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(out))); parseErr == nil {
			rt.PID = pid
		}
	}
	d.setRuntime(runID, *rt)
	d.writeRunEvent(ctx, runDir, runID, "executor_pid", fmt.Sprintf("%s started pid=%d", executor, rt.PID))

	tailDone := make(chan struct{})
	go d.tailRunTerminalLog(ctx, runDir, runID, terminalLog, tailDone)
	err := d.waitRunTerminal(ctx, socketPath, tmuxSession, exitFile)
	close(tailDone)
	if err != nil {
		d.writeRunEvent(ctx, runDir, runID, "executor_exit", "executor failed: "+err.Error())
		d.writeRunLogLine(ctx, runDir, runID, "stderr", "[fixforge] executor failed: "+err.Error())
	} else {
		d.writeRunEvent(ctx, runDir, runID, "executor_exit", "executor completed")
		d.writeRunLogLine(ctx, runDir, runID, "stdout", "[fixforge] executor completed")
	}
	return err
}

func buildTmuxExecutorScript(command string, args []string, terminalLog, exitFile string, promptFile ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(command))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	var cmdLine string
	if len(promptFile) > 0 && promptFile[0] != "" {
		// Feed prompt from a file via stdin redirection. This avoids
		// ARG_MAX limits and shell quoting issues with long prompts.
		cmdLine = fmt.Sprintf("%s < %s 2>&1 | tee -a %s", strings.Join(parts, " "), shellQuote(promptFile[0]), shellQuote(terminalLog))
	} else {
		cmdLine = fmt.Sprintf("%s 2>&1 | tee -a %s", strings.Join(parts, " "), shellQuote(terminalLog))
	}
	return strings.Join([]string{
		"set -o pipefail",
		cmdLine,
		"code=${PIPESTATUS[0]}",
		fmt.Sprintf("printf '%%s\\n' \"$code\" > %s", shellQuote(exitFile)),
		// Keep tmux alive after the executor finishes so the user can
		// attach and interact with the session (e.g. run Claude/Codex
		// again, inspect output, etc.).
		"echo ''",
		"echo '━━━ Session finished (code '${code}') ━━━'",
		"exec \"${SHELL:-bash}\" --norc",
	}, "; ")
}

func (d *CloudDaemon) waitRunTerminal(ctx context.Context, socketPath, tmuxSession, exitFile string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = exec.CommandContext(context.Background(), "tmux", "-S", socketPath, "kill-session", "-t", tmuxSession).Run()
			return ctx.Err()
		case <-ticker.C:
			if _, err := os.Stat(exitFile); err == nil {
				raw, _ := os.ReadFile(exitFile)
				code, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
				if code != 0 {
					return fmt.Errorf("executor exited with code %d", code)
				}
				return nil
			}
			if err := exec.CommandContext(ctx, "tmux", "-S", socketPath, "has-session", "-t", tmuxSession).Run(); err != nil {
				return fmt.Errorf("executor terminal closed before exit status was written")
			}
		}
	}
}

func (d *CloudDaemon) tailRunTerminalLog(ctx context.Context, runDir, runID, path string, done <-chan struct{}) {
	var offset int64
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	readNew := func() {
		file, err := os.Open(path)
		if err != nil {
			return
		}
		defer file.Close()
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			d.writeRunLogLine(ctx, runDir, runID, "stdout", scanner.Text())
		}
		if pos, err := file.Seek(0, io.SeekCurrent); err == nil {
			offset = pos
		}
	}
	for {
		select {
		case <-done:
			readNew()
			return
		case <-ticker.C:
			readNew()
		}
	}
}

func normalizeExecutorArgs(executor string, cfg ExecutorConfig) []string {
	args := append([]string{}, cfg.Args...)
	if isClaudeExecutor(executor, cfg.Command) {
		args = forceClaudeNoSandbox(args)
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
		return args
	}
	if usesCodexExec(executor, cfg.Command) {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			args = append([]string{"exec"}, args...)
		}
		args = forceCodexNoSandbox(args)
		if !hasArg(args, "-") {
			args = append(args, "-")
		}
	}
	return args
}

func isClaudeExecutor(executor, command string) bool {
	name := strings.ToLower(executor)
	cmd := strings.ToLower(filepath.Base(command))
	return name == "claude" || strings.Contains(cmd, "claude")
}

func hasArg(args []string, needle string) bool {
	for _, arg := range args {
		if arg == needle {
			return true
		}
	}
	return false
}

func (d *CloudDaemon) pipeOutput(ctx context.Context, wg *sync.WaitGroup, runDir, runID, stream string, r io.Reader) {
	defer wg.Done()
	path := filepath.Join(runDir, stream+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		d.logger.Warn("open run log failed", "path", path, "error", err)
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		_, _ = file.WriteString(line + "\n")
		seq := d.seq.Add(1)
		if err := d.client.SendLog(ctx, runID, seq, stream, line+"\n"); err != nil {
			d.logger.Warn("upload log failed", "run_id", runID, "error", err)
		}
	}
}

func (d *CloudDaemon) writeRunLogLine(ctx context.Context, runDir, runID, stream, line string) {
	path := filepath.Join(runDir, stream+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		_, _ = file.WriteString(line + "\n")
		_ = file.Close()
	} else {
		d.logger.Warn("open run log failed", "path", path, "error", err)
	}
	seq := d.seq.Add(1)
	if err := d.client.SendLog(ctx, runID, seq, stream, line+"\n"); err != nil {
		d.logger.Warn("upload log failed", "run_id", runID, "error", err)
	}
}

func (d *CloudDaemon) writeRunEvent(ctx context.Context, runDir, runID, eventType, message string) {
	event := map[string]any{
		"runId":     runID,
		"type":      eventType,
		"message":   message,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	}
	path := filepath.Join(runDir, "events.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		if raw, mErr := json.Marshal(event); mErr == nil {
			_, _ = file.Write(append(raw, '\n'))
		}
		_ = file.Close()
	} else {
		d.logger.Warn("open run events failed", "path", path, "error", err)
	}
	_ = d.client.SendEvent(ctx, runID, "status", "running", message)
}

func (d *CloudDaemon) sendHeartbeat(ctx context.Context) error {
	status := "online"
	current := d.runningCount()
	if current > 0 {
		status = "busy"
	}
	return d.client.Heartbeat(ctx, d.cfg.RunnerID, status, current)
}

func (d *CloudDaemon) findProject(projectID string) *ProjectConfig {
	for i := range d.cfg.Projects {
		p := &d.cfg.Projects[i]
		if p.ProjectID == projectID || p.Name == projectID {
			return p
		}
	}
	return nil
}

func (d *CloudDaemon) isRunning(runID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.running[runID]
	return ok
}

func (d *CloudDaemon) setRuntime(runID string, rt Runtime) {
	d.mu.Lock()
	d.running[runID] = rt
	d.mu.Unlock()
}

func (d *CloudDaemon) clearRuntime(runID string) {
	d.mu.Lock()
	delete(d.running, runID)
	d.mu.Unlock()
}

func (d *CloudDaemon) runningCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.running)
}

func (d *CloudDaemon) writeRunJSON(runDir string, run CloudRun, rt Runtime) {
	d.writeJSONFile(filepath.Join(runDir, "run.json"), map[string]any{"run": run, "runtime": rt})
}

func (d *CloudDaemon) writeJSONFile(path string, v any) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		d.logger.Warn("marshal json failed", "path", path, "error", err)
		return
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		d.logger.Warn("write json failed", "path", path, "error", err)
	}
}
