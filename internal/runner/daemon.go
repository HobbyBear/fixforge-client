package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/HobbyBear/fixforge-client/internal/demo"
	"github.com/HobbyBear/fixforge-client/internal/gitops"
	"github.com/HobbyBear/fixforge-client/internal/openspec"
	tbridge "github.com/HobbyBear/fixforge-client/internal/terminal"
)

type Daemon struct {
	cfg    *Config
	client *Client
	logger *slog.Logger

	mu          sync.Mutex
	busyWith    map[int64]bool
	outputSeq   atomic.Int64
	terminals   *tbridge.Registry
	terminalMu  sync.Mutex
	terminalAtt map[string]*tbridge.Attachment
	qaRunning   map[int64]context.CancelFunc // sessionID -> cancel
}

func NewDaemon(cfg *Config, logger *slog.Logger) *Daemon {
	return &Daemon{
		cfg:         cfg,
		logger:      logger,
		busyWith:    make(map[int64]bool),
		client:      NewClient(cfg.Server, cfg.RunnerToken, cfg.DeviceName, cfg.RunnerName, cfg.WorkspaceRoot, cfg.Projects, logger),
		terminals:   tbridge.NewRegistry(),
		terminalAtt: make(map[string]*tbridge.Attachment),
		qaRunning:   make(map[int64]context.CancelFunc),
	}
}

func (d *Daemon) Run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return d.RunContext(ctx)
}

func (d *Daemon) RunContext(ctx context.Context) error {
	d.logger.Info("fixforge-client starting",
		"server", d.cfg.Server, "runner_name", d.cfg.RunnerName, "device", d.cfg.DeviceName, "projects", len(d.cfg.Projects),
	)
	if d.cfg.RunnerToken == "" {
		return fmt.Errorf("runner not configured - set token in %s", DefaultConfigPath())
	}
	d.client.OnResourceRequest(d.handleResourceRequest)
	d.client.OnTerminalOpen(d.handleTerminalOpen)
	d.client.OnTerminalInput(d.handleTerminalInput)
	d.client.OnTerminalResize(d.handleTerminalResize)
	d.client.OnTerminalClose(d.handleTerminalClose)
	d.client.OnQAStop(d.handleQAStop)
	d.client.OnQARequest(d.handleQARequest)

	if err := d.client.Connect(ctx); err != nil {
		d.logger.Info("runner stopped", "reason", err)
	}
	return nil
}

func (d *Daemon) handleQARequest(ctx context.Context, req *QARequest) {
	if req == nil {
		return
	}
	req.RunnerReceivedAt = time.Now()
	d.logger.Info("[runner.qa.request]",
		"qa_id", req.ID,
		"session_id", req.SessionID,
		"project", req.ProjectName,
		"repo_app_path", req.RepoAppPath,
		"branch", strings.TrimSpace(req.Branch),
		"executor", strings.TrimSpace(req.Executor),
		"prompt_chars", len([]rune(req.Prompt)),
	)

	// Create cancellable context so QA stop can interrupt the run.
	qaCtx, qaCancel := context.WithCancel(ctx)
	defer qaCancel()
	if req.SessionID > 0 {
		d.mu.Lock()
		d.qaRunning[req.SessionID] = qaCancel
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			delete(d.qaRunning, req.SessionID)
			d.mu.Unlock()
		}()
	}

	project := d.findProject(req.ProjectName, req.RepoAppPath)
	if project == nil {
		d.sendQAError(req.ID, fmt.Sprintf("project %q (%s) not registered on this runner", req.ProjectName, req.RepoAppPath))
		return
	}
	root := project.LocalPath
	if req.RepoAppPath != "" {
		root = filepath.Join(root, req.RepoAppPath)
	}
	d.logger.Info("[runner.qa.workdir]",
		"qa_id", req.ID,
		"session_id", req.SessionID,
		"workdir", root,
		"branch", strings.TrimSpace(req.Branch),
	)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		d.sendQAError(req.ID, fmt.Sprintf("workdir not found: %s", root))
		return
	}
	branch := strings.TrimSpace(req.Branch)
	if branch != "" {
		if _, err := gitops.CheckoutBranch(qaCtx, root, branch); err != nil {
			d.sendQAError(req.ID, err.Error())
			return
		}
	}

	executor := strings.TrimSpace(req.Executor)
	if executor == "" {
		executor = "claude"
	}
	d.logger.Info("[runner.qa.executor]",
		"qa_id", req.ID,
		"session_id", req.SessionID,
		"executor", executor,
	)
	execCfg, command, ok := BuiltinExecutor(executor)
	if !ok {
		d.sendQAError(req.ID, fmt.Sprintf("executor %q is not supported on this runner", executor))
		return
	}
	if usesCodexExec(executor, command) {
		d.runCodexQAExec(qaCtx, req, root, execCfg, command)
		return
	}
	if usesClaudeExec(executor, command) {
		d.runClaudeQAExec(qaCtx, req, root, execCfg, command)
		return
	}
	d.sendQAError(req.ID, fmt.Sprintf("executor %q is not supported for QA; choose claude or codex", executor))
}

// handleQAStop is called when the server sends a QA stop signal.
func (d *Daemon) handleQAStop(msg *QAStop) {
	if msg == nil || msg.SessionID <= 0 {
		return
	}
	d.mu.Lock()
	cancel, ok := d.qaRunning[msg.SessionID]
	d.mu.Unlock()
	if ok && cancel != nil {
		d.logger.Info("qa: stopping by server request", "session_id", msg.SessionID)
		cancel()
	}
}

func (d *Daemon) sendQAError(id, message string) {
	_ = d.client.SendQAEvent(QAEvent{ID: id, EventType: "error", Error: message})
}

func (d *Daemon) handleTerminalOpen(msg *TerminalMessage) {
	project := d.findProject(msg.ProjectName, msg.RepoAppPath)
	if project == nil {
		d.sendTerminalClosed(msg.ID, tbridge.CloseTerminalNotFound, fmt.Sprintf("project %q (%s) not registered on this runner", msg.ProjectName, msg.RepoAppPath))
		return
	}
	root := project.LocalPath
	if msg.RepoAppPath != "" {
		root = filepath.Join(root, msg.RepoAppPath)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		d.sendTerminalClosed(msg.ID, tbridge.CloseTerminalNotFound, fmt.Sprintf("workdir not found: %s", root))
		return
	}
	if strings.TrimSpace(msg.RunID) != "" {
		d.attachRunTerminal(msg, root)
		return
	}
	term, err := d.terminals.Ensure(msg.SessionID, root, "bash", "main")
	if err != nil {
		d.sendTerminalClosed(msg.ID, tbridge.CloseInternalError, err.Error())
		return
	}
	att, err := term.Attach(msg.Cols, msg.Rows, func(data []byte) {
		_ = d.client.SendTerminalOutput(TerminalMessage{ID: msg.ID, Data: base64.StdEncoding.EncodeToString(data)})
	}, func(code int, reason string) {
		d.sendTerminalClosed(msg.ID, code, reason)
		d.terminalMu.Lock()
		delete(d.terminalAtt, msg.ID)
		d.terminalMu.Unlock()
	})
	if err != nil {
		d.sendTerminalClosed(msg.ID, tbridge.CloseInternalError, err.Error())
		return
	}
	d.terminalMu.Lock()
	if old := d.terminalAtt[msg.ID]; old != nil {
		old.Close()
	}
	d.terminalAtt[msg.ID] = att
	d.terminalMu.Unlock()
}

func (d *Daemon) attachRunTerminal(msg *TerminalMessage, workdir string) {
	socketPath, tmuxSession := runTerminalNames(msg.RunID)
	att, err := tbridge.AttachExisting(socketPath, tmuxSession, workdir, msg.Cols, msg.Rows, func(data []byte) {
		_ = d.client.SendTerminalOutput(TerminalMessage{ID: msg.ID, Data: base64.StdEncoding.EncodeToString(data)})
	}, func(code int, reason string) {
		d.sendTerminalClosed(msg.ID, code, reason)
		d.terminalMu.Lock()
		delete(d.terminalAtt, msg.ID)
		d.terminalMu.Unlock()
	})
	if err != nil {
		d.sendTerminalClosed(msg.ID, tbridge.CloseTerminalNotFound, fmt.Sprintf("run terminal not found for %s: %s", msg.RunID, err.Error()))
		return
	}
	d.terminalMu.Lock()
	if old := d.terminalAtt[msg.ID]; old != nil {
		old.Close()
	}
	d.terminalAtt[msg.ID] = att
	d.terminalMu.Unlock()
}

func (d *Daemon) handleTerminalInput(msg *TerminalMessage) {
	data, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		return
	}
	d.terminalMu.Lock()
	att := d.terminalAtt[msg.ID]
	d.terminalMu.Unlock()
	if att != nil {
		_ = att.Write(data)
	}
}

func (d *Daemon) handleTerminalResize(msg *TerminalMessage) {
	d.terminalMu.Lock()
	att := d.terminalAtt[msg.ID]
	d.terminalMu.Unlock()
	if att != nil {
		_ = att.Resize(msg.Cols, msg.Rows)
	}
}

func (d *Daemon) handleTerminalClose(msg *TerminalMessage) {
	d.terminalMu.Lock()
	att := d.terminalAtt[msg.ID]
	delete(d.terminalAtt, msg.ID)
	d.terminalMu.Unlock()
	if att != nil {
		att.Close()
	}
}

func (d *Daemon) sendTerminalClosed(id string, code int, reason string) {
	_ = d.client.SendTerminalClosed(TerminalMessage{ID: id, Code: code, Reason: reason})
}

func (d *Daemon) handleResourceRequest(ctx context.Context, req *ResourceRequest) *ResourceResponse {
	project := d.findProject(req.ProjectName, req.RepoAppPath)
	if project == nil {
		return resourceError(req.ID, fmt.Sprintf("project %q (%s) not registered on this runner", req.ProjectName, req.RepoAppPath))
	}
	root := project.LocalPath
	if req.RepoAppPath != "" {
		root = filepath.Join(root, req.RepoAppPath)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return resourceError(req.ID, fmt.Sprintf("workdir not found: %s", root))
	}
	started := time.Now()
	if shouldLogResourceOperation(req.Operation) {
		d.logger.Info("resource operation start",
			"id", req.ID,
			"operation", req.Operation,
			"project", project.Name,
			"project_id", project.ProjectID,
			"repo_app_path", req.RepoAppPath,
			"root", root,
			"path", req.Path,
			"files_count", len(req.Files),
			"files", req.Files,
			"branch", req.Branch,
			"target_branch", req.TargetBranch,
			"ref", req.Ref,
			"hash", req.Hash,
		)
	}
	var resp *ResourceResponse
	defer func() {
		if shouldLogResourceOperation(req.Operation) && resp != nil {
			attrs := []any{
				"id", req.ID,
				"operation", req.Operation,
				"root", root,
				"ok", resp.OK,
				"error", resp.Error,
				"payload_bytes", len(resp.Payload),
				"duration_ms", time.Since(started).Milliseconds(),
			}
			attrs = append(attrs, resourcePayloadLogAttrs(req.Operation, resp.Payload)...)
			d.logger.Info("resource operation done", attrs...)
		}
	}()
	switch req.Operation {
	case "list":
		resp = resourcePayload(req.ID, d.resourceList(root, req.Path))
		return resp
	case "read":
		resp = resourcePayload(req.ID, d.resourceRead(ctx, root, req.Path))
		return resp
	case "write":
		resp = resourcePayload(req.ID, d.resourceWrite(ctx, root, req.Path, req.Content, req.Encoding))
		return resp
	case "branches":
		resp = resourcePayload(req.ID, d.resourceBranches(ctx, root))
		return resp
	case "checkout_branch":
		payload, err := gitops.CheckoutBranch(ctx, root, req.Branch)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_status":
		payload, err := gitops.Status(ctx, root)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_history":
		payload, err := gitops.History(ctx, root, 30)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_stashes":
		payload, err := gitops.Stashes(ctx, root, 30)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_create_branch":
		payload, err := gitops.CreateBranch(ctx, root, req.Branch)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "changes":
		resp = resourcePayload(req.ID, d.resourceChanges(ctx, root))
		return resp
	case "diff":
		resp = resourcePayload(req.ID, d.resourceDiff(ctx, root, req.Path))
		return resp
	case "git_commit":
		payload, err := gitops.CommitFiles(ctx, root, req.Files, req.Message)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_add":
		payload, err := gitops.AddFiles(ctx, root, req.Files)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_restore":
		payload, err := gitops.RestoreFiles(ctx, root, req.Files)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_delete":
		payload, err := gitops.DeleteFiles(ctx, root, req.Files)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_pull":
		payload, err := gitops.PullBranch(ctx, root, req.Branch)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_push":
		payload, err := gitops.PushBranch(ctx, root, req.Branch)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_merge":
		var payload map[string]any
		var err error
		if strings.TrimSpace(req.TargetBranch) != "" {
			payload, err = gitops.MergeToBranch(ctx, root, req.TargetBranch)
		} else {
			payload, err = gitops.MergeBranch(ctx, root, req.Branch)
		}
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_stash":
		payload, err := gitops.StashChanges(ctx, root, req.Files, req.Message)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_stash_apply":
		payload, err := gitops.ApplyStash(ctx, root, req.Ref)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "git_commit_file_diff":
		payload, err := gitops.CommitFileDiff(ctx, root, req.Hash, req.Path)
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "openspec":
		payload, err := openspec.RunResourceOperation(root, openspec.Operation{
			Operation:    req.OpenSpecOperation,
			Change:       req.Change,
			WorkflowMode: req.WorkflowMode,
		})
		resp = resourceResult(req.ID, payload, err)
		return resp
	case "shell":
		resp = resourcePayload(req.ID, d.resourceShell(ctx, root, req.Command, req.Timeout))
		return resp
	default:
		resp = resourceError(req.ID, "unknown resource operation: "+req.Operation)
		return resp
	}
}

func resourcePayload(id string, payload any) *ResourceResponse {
	data, err := json.Marshal(payload)
	if err != nil {
		return resourceError(id, err.Error())
	}
	return &ResourceResponse{ID: id, OK: true, Payload: data}
}

func resourceResult(id string, payload any, err error) *ResourceResponse {
	if err != nil {
		return resourceError(id, err.Error())
	}
	return resourcePayload(id, payload)
}

func resourceError(id, message string) *ResourceResponse {
	return &ResourceResponse{ID: id, OK: false, Error: message}
}

func shouldLogResourceOperation(operation string) bool {
	switch operation {
	case "branches", "changes", "diff",
		"checkout_branch", "git_status", "git_history", "git_stashes", "git_create_branch",
		"git_commit", "git_add", "git_restore", "git_delete", "git_pull", "git_push", "git_merge", "git_stash", "git_stash_apply", "git_commit_file_diff":
		return true
	default:
		return false
	}
}

func resourcePayloadLogAttrs(operation string, payload json.RawMessage) []any {
	if len(payload) == 0 {
		return nil
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return []any{"payload_parse_error", err.Error()}
	}
	attrs := make([]any, 0, 12)
	if object, _ := body["object"].(string); object != "" {
		attrs = append(attrs, "object", object)
	}
	if branch, _ := body["current_branch"].(string); branch != "" {
		attrs = append(attrs, "current_branch", branch)
	}
	if branch, _ := body["branch"].(string); branch != "" {
		attrs = append(attrs, "branch", branch)
	}
	if hash, _ := body["hash"].(string); hash != "" {
		attrs = append(attrs, "hash", hash)
	}
	if files, ok := body["files"].([]any); ok {
		attrs = append(attrs, "result_files_count", len(files), "result_files", previewAnyStrings(files, 20))
	}
	if data, ok := body["data"].([]any); ok {
		attrs = append(attrs, "data_count", len(data))
		if operation == "changes" {
			staged, untracked := 0, 0
			paths := make([]string, 0, minInt(len(data), 20))
			for _, item := range data {
				entry, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if stagedValue, _ := entry["staged"].(bool); stagedValue {
					staged++
				}
				if status, _ := entry["status"].(string); status == "untracked" {
					untracked++
				}
				if len(paths) < 20 {
					if path, _ := entry["path"].(string); path != "" {
						paths = append(paths, path)
					}
				}
			}
			attrs = append(attrs, "staged_count", staged, "untracked_count", untracked, "paths", paths)
		}
	}
	if changedCount, ok := body["changed_count"].(float64); ok {
		attrs = append(attrs, "changed_count", int(changedCount))
	}
	return attrs
}

func previewAnyStrings(values []any, limit int) []string {
	if limit <= 0 || len(values) == 0 {
		return nil
	}
	out := make([]string, 0, minInt(len(values), limit))
	for _, value := range values {
		if len(out) >= limit {
			break
		}
		text := strings.TrimSpace(fmt.Sprint(value))
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (d *Daemon) resourceList(root, rel string) any {
	target, err := safeRunnerJoin(root, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	type entry struct {
		ID       string `json:"id"`
		Object   string `json:"object"`
		Path     string `json:"path"`
		Name     string `json:"name"`
		Type     string `json:"type"`
		Size     int64  `json:"size,omitempty"`
		Modified string `json:"modified_at,omitempty"`
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".git" || e.Name() == "node_modules" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		kind := "file"
		if e.IsDir() {
			kind = "directory"
		}
		p := filepath.ToSlash(filepath.Join(rel, e.Name()))
		out = append(out, entry{ID: p, Object: "session.environment.filesystem.entry", Path: p, Name: e.Name(), Type: kind, Size: info.Size(), Modified: info.ModTime().Format(time.RFC3339)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type == "directory"
		}
		return out[i].Path < out[j].Path
	})
	return map[string]any{"object": "list", "data": out}
}

func (d *Daemon) resourceRead(ctx context.Context, root, rel string) any {
	target, err := safeRunnerJoin(root, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	info, err := os.Stat(target)
	if err != nil {
		if resolved, _, resolveErr := gitops.ResolveWorktreePath(ctx, root, rel); resolveErr == nil {
			target = resolved
			info, err = os.Stat(target)
		}
	}
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	if info.IsDir() {
		return d.resourceList(root, rel)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	ct := mime.TypeByExtension(filepath.Ext(target))
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	return map[string]any{"object": "session.environment.filesystem.file_content", "path": filepath.ToSlash(rel), "content_type": ct, "encoding": "utf-8", "content": string(data), "bytes": len(data)}
}

func (d *Daemon) resourceWrite(ctx context.Context, root, rel, content, encoding string) any {
	target, err := safeRunnerJoin(root, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	if info, statErr := os.Stat(target); statErr != nil {
		if resolved, _, resolveErr := gitops.ResolveWorktreePath(ctx, root, rel); resolveErr == nil {
			target = resolved
			info, statErr = os.Stat(target)
		}
		if statErr == nil && info.IsDir() {
			return map[string]any{"error": "cannot write directory"}
		}
	} else if info.IsDir() {
		return map[string]any{"error": "cannot write directory"}
	}
	var data []byte
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "utf-8", "utf8":
		data = []byte(content)
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return map[string]any{"error": "invalid base64 content"}
		}
		data = decoded
	default:
		return map[string]any{"error": "unsupported encoding"}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return map[string]any{"error": err.Error()}
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"object": "session.environment.filesystem.file_content", "path": filepath.ToSlash(rel), "encoding": "utf-8", "content": string(data), "bytes": len(data), "saved": true}
}

func (d *Daemon) resourceChanges(ctx context.Context, root string) any {
	files, err := gitops.ChangedFiles(ctx, root)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"object": "list", "data": files}
}

func (d *Daemon) resourceBranches(ctx context.Context, root string) any {
	payload, err := gitops.Branches(ctx, root)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return payload
}

func (d *Daemon) resourceDiff(ctx context.Context, root, rel string) any {
	payload, err := gitops.FileDiff(ctx, root, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return payload
}

func (d *Daemon) resourceShell(ctx context.Context, root, command string, timeout int) any {
	command = strings.TrimSpace(command)
	if command == "" {
		return map[string]any{"error": "command is required"}
	}
	if timeout <= 0 || timeout > 120 {
		timeout = 30
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-lc", command)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = 1
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		}
	}
	return map[string]any{"object": "session.environment.shell_result", "command": command, "stdout": stdout.String(), "stderr": stderr.String(), "exit_code": exitCode, "ok": err == nil}
}

func safeRunnerJoin(root, rel string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rel = strings.TrimPrefix(filepath.Clean("/"+rel), string(filepath.Separator))
	target := filepath.Join(rootAbs, rel)
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if targetAbs != rootAbs && !strings.HasPrefix(targetAbs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return targetAbs, nil
}

// ─── Busy control ───

func (d *Daemon) trySetBusy(sessionID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.busyWith[sessionID] {
		return false
	}
	d.busyWith[sessionID] = true
	return true
}

func (d *Daemon) setIdle(sessionID int64) {
	d.mu.Lock()
	delete(d.busyWith, sessionID)
	allIdle := len(d.busyWith) == 0
	d.mu.Unlock()
	if allIdle {
		d.client.SendRunnerState("online")
	}
}

// ─── Helpers ───

func (d *Daemon) findProject(name, repoAppPath string) *ProjectConfig {
	for i := range d.cfg.Projects {
		p := &d.cfg.Projects[i]
		if p.Name == name && p.RepoAppPath == repoAppPath {
			return p
		}
	}
	return nil
}

// ─── CLI commands ───

func DoRun() error {
	cfgPath := DefaultConfigPath()
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	if cfg.Token == "" {
		return fmt.Errorf("runner not configured - set token in %s", cfgPath)
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if handled, err := maybeRunPlatformService(cfg, logger); handled {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return runDaemons(ctx, cfg, logger)
}

func runDaemons(ctx context.Context, cfg *Config, logger *slog.Logger) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() {
		errCh <- NewDaemon(cfg, logger).RunContext(runCtx)
	}()
	go func() {
		errCh <- NewCloudDaemon(cfg, logger).RunContext(runCtx)
	}()

	err := <-errCh
	cancel()
	if err == context.Canceled || err == context.DeadlineExceeded {
		return nil
	}
	return err
}

func DoConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	server := fs.String("server", "", "FixForge server URL")
	token := fs.String("token", "", "runner token")
	runnerName := fs.String("runner-name", "", "runner display name")
	projectID := fs.String("project-id", "", "project identifier used by CloudRun")
	projectName := fs.String("project-name", "", "project name")
	nameAlias := fs.String("name", "", "project name")
	repoURL := fs.String("repo-url", "", "project repository URL")
	repoAppPath := fs.String("repo-app-path", "", "repository sub-path")
	localPath := fs.String("local-path", "", "local repository path")
	installService := fs.Bool("install-service", false, "install and start the runner service after saving config")
	if err := fs.Parse(args); err != nil {
		return err
	}

	*server = strings.TrimSpace(*server)
	*token = strings.TrimSpace(*token)
	*runnerName = strings.TrimSpace(*runnerName)
	*projectID = strings.TrimSpace(*projectID)
	*projectName = strings.TrimSpace(*projectName)
	if *projectName == "" {
		*projectName = strings.TrimSpace(*nameAlias)
	}
	*repoURL = strings.TrimSpace(*repoURL)
	*repoAppPath = strings.Trim(strings.TrimSpace(*repoAppPath), "/")
	*localPath = strings.TrimSpace(*localPath)

	if *server == "" {
		return fmt.Errorf("--server is required")
	}
	if *token == "" {
		return fmt.Errorf("--token is required")
	}
	if *projectName == "" && *projectID == "" {
		return fmt.Errorf("--project-name or --project-id is required")
	}
	if *projectID == "" {
		*projectID = *projectName
	}
	if *projectName == "" {
		*projectName = *projectID
	}
	if *localPath == "" {
		if *repoURL == "" {
			return fmt.Errorf("--local-path is required when --repo-url is empty")
		}
		defaultPath, err := defaultProjectClonePath(*repoURL)
		if err != nil {
			return err
		}
		*localPath = defaultPath
	}
	expandedLocalPath, err := expandUserPath(*localPath)
	if err != nil {
		return err
	}
	absLocalPath, err := filepath.Abs(expandedLocalPath)
	if err != nil {
		return fmt.Errorf("resolve local path: %w", err)
	}
	info, err := os.Stat(absLocalPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat local path: %w", err)
		}
		if *repoURL == "" {
			return fmt.Errorf("local path not found: %s", absLocalPath)
		}
		if err := cloneRepository(*repoURL, absLocalPath); err != nil {
			return err
		}
		info, err = os.Stat(absLocalPath)
		if err != nil {
			return fmt.Errorf("stat cloned path: %w", err)
		}
	}
	if !info.IsDir() {
		return fmt.Errorf("local path is not a directory: %s", absLocalPath)
	}

	cfgPath := DefaultConfigPath()
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		if !strings.Contains(err.Error(), "config file not found") {
			return err
		}
		cfg = &Config{}
	}
	cfg.ServerURL = *server
	cfg.Server = *server
	cfg.Token = *token
	cfg.RunnerToken = *token
	if *runnerName != "" {
		cfg.RunnerName = *runnerName
		cfg.DeviceName = *runnerName
	}
	upsertRunnerProject(cfg, ProjectConfig{
		ProjectID:   *projectID,
		Name:        *projectName,
		RepoURL:     *repoURL,
		RepoAppPath: *repoAppPath,
		LocalPath:   absLocalPath,
	})
	if err := SaveConfig(cfgPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("Config saved: %s\n", cfgPath)
	fmt.Printf("Project connected: %s -> %s\n", *projectName, absLocalPath)
	if *installService {
		if err := DoServiceInstall(); err != nil {
			return err
		}
	}
	return nil
}

func defaultProjectClonePath(repoURL string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	dir := repoNameFromURL(repoURL)
	if dir == "" {
		dir = "fixforge-project"
	}
	if demo.IsRepoURL(repoURL) {
		dir = demo.CloneDir
	}
	return filepath.Join(home, dir), nil
}

func repoNameFromURL(repoURL string) string {
	text := strings.TrimSpace(repoURL)
	text = strings.TrimSuffix(strings.TrimRight(text, "/"), ".git")
	text = strings.TrimPrefix(text, "git@github.com:")
	text = strings.TrimPrefix(text, "ssh://")
	if idx := strings.LastIndex(text, "/"); idx >= 0 {
		text = text[idx+1:]
	}
	return strings.TrimSpace(text)
}

func cloneRepository(repoURL, localPath string) error {
	parent := filepath.Dir(localPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create clone parent: %w", err)
	}
	fmt.Printf("Local project not found, cloning %s into %s\n", repoURL, localPath)
	cmd := exec.Command("git", "clone", repoURL, localPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}
	return nil
}

func upsertRunnerProject(cfg *Config, project ProjectConfig) {
	project.Normalize()
	for i := range cfg.Projects {
		current := &cfg.Projects[i]
		current.Normalize()
		sameID := project.ProjectID != "" && current.ProjectID == project.ProjectID
		sameName := project.Name != "" && current.Name == project.Name && current.RepoAppPath == project.RepoAppPath
		sameRepo := project.RepoURL != "" && normalizeRunnerRepo(project.RepoURL) == normalizeRunnerRepo(current.RepoURL) && current.RepoAppPath == project.RepoAppPath
		if sameID || sameName || sameRepo {
			cfg.Projects[i] = project
			return
		}
	}
	cfg.Projects = append(cfg.Projects, project)
}

func normalizeRunnerRepo(value string) string {
	return strings.TrimSuffix(strings.TrimRight(strings.ToLower(strings.TrimSpace(value)), "/"), ".git")
}

func expandUserPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func DoAddProject() error {
	cfgPath := DefaultConfigPath()
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	var project ProjectConfig
	fmt.Print("Project name: ")
	fmt.Scanln(&project.Name)
	if project.Name == "" {
		return fmt.Errorf("project name is required")
	}
	fmt.Print("Repo sub-path (e.g. apps/chat, empty for root): ")
	fmt.Scanln(&project.RepoAppPath)
	fmt.Print("Local path: ")
	fmt.Scanln(&project.LocalPath)
	if project.LocalPath == "" {
		return fmt.Errorf("local path is required")
	}
	cfg.Projects = append(cfg.Projects, project)
	if err := SaveConfig(cfgPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("+ project '%s' added\n", project.Name)
	return nil
}

func DoStatus() error {
	cfgPath := DefaultConfigPath()
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	fmt.Println("=== FixForge Runner Status ===")
	fmt.Printf("Server:      %s\n", cfg.ServerURL)
	fmt.Printf("Runner ID:   %s\n", cfg.RunnerID)
	fmt.Printf("Device:      %s\n", cfg.RunnerName)
	fmt.Printf("Bound:       %v\n", cfg.Token != "")
	fmt.Println()
	if len(cfg.Projects) == 0 {
		fmt.Println("Projects: (none)")
	} else {
		fmt.Println("Registered projects:")
		for i, p := range cfg.Projects {
			fmt.Printf("  %d. %s\n", i+1, p.Name)
			if p.RepoAppPath != "" {
				fmt.Printf("     sub-path: %s\n", p.RepoAppPath)
			}
			fmt.Printf("     path:     %s\n", p.LocalPath)
		}
	}
	return nil
}

func DoPS() error {
	cfg, err := LoadConfig(DefaultConfigPath())
	if err != nil {
		return err
	}
	fmt.Println("TASK        RUN                         STAGE    STATUS")
	for _, item := range localRunInfos(cfg) {
		fmt.Printf("%-11s %-27s %-8s %s\n", item.TaskID, item.RunID, item.Stage, item.Status)
	}
	return nil
}

func DoOpen(taskID string) error {
	cfg, err := LoadConfig(DefaultConfigPath())
	if err != nil {
		return err
	}
	item, ok := latestLocalRun(cfg, taskID)
	if !ok {
		return fmt.Errorf("no local run found for %s", taskID)
	}
	fmt.Println("worktree:")
	fmt.Println(item.Worktree)
	fmt.Println()
	fmt.Println("branch:")
	fmt.Println(item.Branch)
	return nil
}

func DoAttach(taskID string) error {
	cfg, err := LoadConfig(DefaultConfigPath())
	if err != nil {
		return err
	}
	item, ok := latestLocalRun(cfg, taskID)
	if !ok {
		return fmt.Errorf("no local run found for %s", taskID)
	}
	fmt.Printf("run: %s\nworktree: %s\nbranch: %s\n\n", item.RunID, item.Worktree, item.Branch)
	for _, path := range []string{item.StdoutPath, item.StderrPath} {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			fmt.Printf("==> %s <==\n%s\n", path, string(data))
		}
	}
	return nil
}

type localRunInfo struct {
	TaskID     string
	RunID      string
	Stage      string
	Status     string
	Worktree   string
	Branch     string
	UpdatedAt  time.Time
	StdoutPath string
	StderrPath string
}

func localRunInfos(cfg *Config) []localRunInfo {
	var out []localRunInfo
	for _, p := range cfg.Projects {
		base := filepath.Join(p.LocalPath, ".fixforge", "runs")
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			runDir := filepath.Join(base, entry.Name())
			raw, err := os.ReadFile(filepath.Join(runDir, "run.json"))
			if err != nil {
				continue
			}
			var parsed struct {
				Runtime Runtime `json:"runtime"`
			}
			if err := json.Unmarshal(raw, &parsed); err != nil {
				continue
			}
			info := parsed.Runtime
			out = append(out, localRunInfo{
				TaskID:     info.TaskID,
				RunID:      info.RunID,
				Stage:      info.Stage,
				Status:     info.Status,
				Worktree:   info.Worktree,
				Branch:     info.Branch,
				UpdatedAt:  info.StartedAt,
				StdoutPath: filepath.Join(runDir, "stdout.log"),
				StderrPath: filepath.Join(runDir, "stderr.log"),
			})
		}
	}
	return out
}

func latestLocalRun(cfg *Config, taskID string) (localRunInfo, bool) {
	var latest localRunInfo
	ok := false
	for _, item := range localRunInfos(cfg) {
		if item.TaskID != taskID {
			continue
		}
		if !ok || item.UpdatedAt.After(latest.UpdatedAt) {
			latest = item
			ok = true
		}
	}
	return latest, ok
}
