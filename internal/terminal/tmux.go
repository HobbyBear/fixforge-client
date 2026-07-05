package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

const (
	CloseTerminalNotFound = 4404
	CloseTerminalDetached = 4405
	CloseInternalError    = 4500
)

type Resource struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	Name       string `json:"name"`
	SessionKey string `json:"session_key"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

type Registry struct {
	mu        sync.Mutex
	terminals map[string]*Terminal
}

type Terminal struct {
	id          string
	name        string
	sessionKey  string
	sessionID   int64
	workDir     string
	socketPath  string
	tmuxSession string
	createdAt   time.Time
}

type Attachment struct {
	term *Terminal
	cmd  *exec.Cmd
	file *os.File
	once sync.Once
	done chan struct{}
}

type ResizeMessage struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

var tmuxNameRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func NewRegistry() *Registry {
	return &Registry{terminals: map[string]*Terminal{}}
}

func (r *Registry) List(sessionID int64, workDir string) []Resource {
	term, _ := r.Ensure(sessionID, workDir, "bash", "main")
	if term == nil {
		return nil
	}
	return []Resource{term.Resource()}
}

func (r *Registry) Ensure(sessionID int64, workDir, name, sessionKey string) (*Terminal, error) {
	if name == "" {
		name = "bash"
	}
	if sessionKey == "" {
		sessionKey = "main"
	}
	id := fmt.Sprintf("terminal_%s_%s", sanitize(name), sanitize(sessionKey))
	key := fmt.Sprintf("%d:%s", sessionID, id)

	r.mu.Lock()
	existing := r.terminals[key]
	r.mu.Unlock()
	if existing != nil {
		if existing.Alive(context.Background()) {
			return existing, nil
		}
		r.mu.Lock()
		delete(r.terminals, key)
		r.mu.Unlock()
	}

	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("terminal workdir not found: %s", workDir)
	}
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("fixforge-tmux-%d.sock", sessionID))
	tmuxSession := sanitize(fmt.Sprintf("ff_%d_%s_%s", sessionID, name, sessionKey))
	term := &Terminal{
		id:          id,
		name:        name,
		sessionKey:  sessionKey,
		sessionID:   sessionID,
		workDir:     workDir,
		socketPath:  socketPath,
		tmuxSession: tmuxSession,
		createdAt:   time.Now(),
	}
	if err := term.launch(context.Background()); err != nil {
		return nil, err
	}

	r.mu.Lock()
	if racer := r.terminals[key]; racer != nil && racer.Alive(context.Background()) {
		r.mu.Unlock()
		return racer, nil
	}
	r.terminals[key] = term
	r.mu.Unlock()
	return term, nil
}

func (r *Registry) Get(sessionID int64, terminalID string) *Terminal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminals[fmt.Sprintf("%d:%s", sessionID, terminalID)]
}

func (t *Terminal) Resource() Resource {
	status := "running"
	if !t.Alive(context.Background()) {
		status = "closed"
	}
	return Resource{
		ID: t.id, Object: "session.terminal", Name: t.name,
		SessionKey: t.sessionKey, Status: status, CreatedAt: t.createdAt.Format(time.RFC3339),
	}
}

func (t *Terminal) Attach(cols, rows uint16, output func([]byte), closed func(code int, reason string)) (*Attachment, error) {
	if cols == 0 {
		cols = 100
	}
	if rows == 0 {
		rows = 30
	}
	if !t.Alive(context.Background()) {
		if err := t.launch(context.Background()); err != nil {
			return nil, err
		}
	}
	cmd := exec.Command("tmux", "-S", t.socketPath, "attach", "-t", t.tmuxSession)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Dir = t.workDir
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	att := &Attachment{term: t, cmd: cmd, file: file, done: make(chan struct{})}
	go att.readLoop(output, closed)
	return att, nil
}

func AttachExisting(socketPath, tmuxSession, workDir string, cols, rows uint16, output func([]byte), closed func(code int, reason string)) (*Attachment, error) {
	term := &Terminal{
		id:          tmuxSession,
		name:        tmuxSession,
		sessionKey:  tmuxSession,
		workDir:     workDir,
		socketPath:  socketPath,
		tmuxSession: tmuxSession,
		createdAt:   time.Now(),
	}
	if !term.Alive(context.Background()) {
		return nil, fmt.Errorf("terminal session not found")
	}
	if cols == 0 {
		cols = 100
	}
	if rows == 0 {
		rows = 30
	}
	cmd := exec.Command("tmux", "-S", term.socketPath, "attach", "-t", term.tmuxSession)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Dir = term.workDir
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	att := &Attachment{term: term, cmd: cmd, file: file, done: make(chan struct{})}
	go att.readLoop(output, closed)
	return att, nil
}

func (a *Attachment) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	_, err := a.file.Write(data)
	return err
}

func (a *Attachment) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	return pty.Setsize(a.file, &pty.Winsize{Cols: cols, Rows: rows})
}

func (a *Attachment) Close() {
	a.once.Do(func() {
		_ = a.file.Close()
		if a.cmd != nil && a.cmd.Process != nil {
			_ = a.cmd.Process.Kill()
		}
		<-a.done
	})
}

func (a *Attachment) readLoop(output func([]byte), closed func(code int, reason string)) {
	defer close(a.done)
	defer func() { _ = a.cmd.Wait() }()

	buf := make([]byte, 4096)
	for {
		n, err := a.file.Read(buf)
		if n > 0 && output != nil {
			chunk := append([]byte(nil), buf[:n]...)
			output(chunk)
		}
		if err != nil {
			code := CloseTerminalDetached
			reason := "terminal detached"
			if !a.term.Alive(context.Background()) {
				code = CloseTerminalNotFound
				reason = "terminal not found"
			}
			if !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.EOF) {
				reason = err.Error()
			}
			if closed != nil {
				closed(code, reason)
			}
			return
		}
	}
}

func ParseResize(data []byte) (uint16, uint16, bool) {
	var msg ResizeMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return 0, 0, false
	}
	if msg.Type != "resize" || msg.Cols == 0 || msg.Rows == 0 {
		return 0, 0, false
	}
	return msg.Cols, msg.Rows, true
}

func (t *Terminal) Alive(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "tmux", "-S", t.socketPath, "has-session", "-t", t.tmuxSession)
	return cmd.Run() == nil
}

func (t *Terminal) launch(ctx context.Context) error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux not found on PATH")
	}
	if t.Alive(ctx) {
		return nil
	}
	shellPath := defaultShell()
	args := []string{"-S", t.socketPath, "new-session", "-d", "-s", t.tmuxSession, "-c", t.workDir, shellPath, "-l"}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	_ = exec.CommandContext(ctx, "tmux", "-S", t.socketPath, "set-option", "-t", t.tmuxSession, "mouse", "on").Run()
	_ = exec.CommandContext(ctx, "tmux", "-S", t.socketPath, "set-option", "-t", t.tmuxSession, "status", "off").Run()
	return nil
}

func sanitize(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	value = tmuxNameRe.ReplaceAllString(value, "_")
	if _, err := strconv.Atoi(value[:1]); err == nil {
		value = "t_" + value
	}
	return value
}

func defaultShell() string {
	candidates := []string{os.Getenv("SHELL"), "/bin/zsh", "/usr/bin/zsh", "/bin/bash", "/usr/bin/bash", "/bin/sh"}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "sh"
}
