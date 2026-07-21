package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const localMCPBridgeAddr = "127.0.0.1:18421"

type localMCPBridge struct {
	cfg      *Config
	logger   *slog.Logger
	listener net.Listener
	server   *http.Server
	client   *http.Client

	mu     sync.Mutex
	leases map[int64]localMCPLease
}

type localMCPLease struct {
	MCPURL       string
	SessionToken string
	ExpiresAt    time.Time
}

func NewLocalMCPBridge(cfg *Config, logger *slog.Logger) (*localMCPBridge, error) {
	if cfg == nil {
		return nil, errors.New("runner configuration is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg.Normalize()
	if strings.TrimSpace(cfg.ServerURL) == "" || strings.TrimSpace(cfg.RunnerToken) == "" {
		return nil, errors.New("server URL and runner token are required for the local MCP bridge")
	}
	return &localMCPBridge{cfg: cfg, logger: logger, client: &http.Client{Timeout: 30 * time.Second}, leases: make(map[int64]localMCPLease)}, nil
}

func (b *localMCPBridge) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", localMCPBridgeAddr)
	if err != nil {
		return fmt.Errorf("start local MCP bridge on %s: %w", localMCPBridgeAddr, err)
	}
	b.listener = listener
	b.server = &http.Server{Handler: http.HandlerFunc(b.handle), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 65 * time.Second}
	go func() {
		if err := b.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.logger.Error("local MCP bridge stopped", "error", err)
		}
	}()
	go func() { <-ctx.Done(); _ = b.Close(context.Background()) }()
	b.logger.Info("local MCP bridge started", "addr", localMCPBridgeAddr)
	return nil
}

func (b *localMCPBridge) Close(ctx context.Context) error {
	if b.server == nil {
		return nil
	}
	return b.server.Shutdown(ctx)
}

func (b *localMCPBridge) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	projectID, ok := localMCPProjectID(r.URL.Path)
	if !ok || !b.hasProject(projectID) {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err != nil {
		http.Error(w, "MCP request is too large", http.StatusRequestEntityTooLarge)
		return
	}
	lease, err := b.lease(r.Context(), projectID)
	if err != nil {
		http.Error(w, "could not authorize local MCP request: "+err.Error(), http.StatusBadGateway)
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, lease.MCPURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "invalid MCP endpoint", http.StatusBadGateway)
		return
	}
	request.Header.Set("Authorization", "Bearer "+lease.SessionToken)
	request.Header.Set("Content-Type", "application/json")
	if sessionID := r.Header.Get("Mcp-Session-Id"); sessionID != "" {
		request.Header.Set("Mcp-Session-Id", sessionID)
	}
	response, err := b.client.Do(request)
	if err != nil {
		http.Error(w, "cloud MCP request failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	if sessionID := response.Header.Get("Mcp-Session-Id"); sessionID != "" {
		w.Header().Set("Mcp-Session-Id", sessionID)
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, 2*1024*1024))
}

func localMCPProjectID(path string) (int64, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[0] != "mcp" {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	return id, err == nil && id > 0
}

func (b *localMCPBridge) hasProject(projectID int64) bool {
	for _, project := range b.cfg.Projects {
		id, err := strconv.ParseInt(strings.TrimSpace(project.ProjectID), 10, 64)
		if err == nil && id == projectID {
			return true
		}
	}
	return false
}

func (b *localMCPBridge) lease(ctx context.Context, projectID int64) (localMCPLease, error) {
	b.mu.Lock()
	lease, found := b.leases[projectID]
	b.mu.Unlock()
	if found && lease.ExpiresAt.After(time.Now().UTC().Add(2*time.Minute)) {
		return lease, nil
	}
	lease, err := requestLocalMCPLease(ctx, b.client, b.cfg, projectID)
	if err != nil {
		return localMCPLease{}, err
	}
	b.mu.Lock()
	b.leases[projectID] = lease
	b.mu.Unlock()
	return lease, nil
}

// RegisterLocalMCPConnections creates or refreshes each configured local
// connection before a CLI invokes a tool, so a project manager can grant write
// access in the UI before the first model request. The returned lease is never
// persisted or exposed to a project config.
func RegisterLocalMCPConnections(ctx context.Context, cfg *Config) error {
	if cfg == nil {
		return errors.New("runner configuration is required")
	}
	cfg.Normalize()
	client := &http.Client{Timeout: 15 * time.Second}
	var problems []string
	for _, project := range cfg.Projects {
		projectID, err := strconv.ParseInt(strings.TrimSpace(project.ProjectID), 10, 64)
		if err != nil || projectID <= 0 {
			continue
		}
		if _, err := requestLocalMCPLease(ctx, client, cfg, projectID); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", project.Name, err))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func requestLocalMCPLease(ctx context.Context, client *http.Client, cfg *Config, projectID int64) (localMCPLease, error) {
	endpoint := strings.TrimRight(cfg.ServerURL, "/") + "/api/runner/mcp-lease"
	payload, err := json.Marshal(map[string]any{"projectId": projectID, "installationId": cfg.InstallationID, "runnerName": cfg.RunnerName})
	if err != nil {
		return localMCPLease{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return localMCPLease{}, err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.RunnerToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return localMCPLease{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return localMCPLease{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return localMCPLease{}, fmt.Errorf("lease request failed with status %d", response.StatusCode)
	}
	var decoded struct {
		MCPURL       string    `json:"mcpUrl"`
		SessionToken string    `json:"sessionToken"`
		ExpiresAt    time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return localMCPLease{}, fmt.Errorf("invalid lease response: %w", err)
	}
	if strings.TrimSpace(decoded.MCPURL) == "" || strings.TrimSpace(decoded.SessionToken) == "" || !decoded.ExpiresAt.After(time.Now().UTC()) {
		return localMCPLease{}, errors.New("lease response is incomplete")
	}
	return localMCPLease{MCPURL: decoded.MCPURL, SessionToken: decoded.SessionToken, ExpiresAt: decoded.ExpiresAt}, nil
}

// SyncLocalMCPProjectConfigs writes only managed local entries. It refuses to
// modify Git-tracked config files or a user-owned FixForge server entry.
func SyncLocalMCPProjectConfigs(cfg *Config) error {
	if cfg == nil {
		return errors.New("runner configuration is required")
	}
	var problems []string
	for _, project := range cfg.Projects {
		project.Normalize()
		projectID, err := strconv.ParseInt(strings.TrimSpace(project.ProjectID), 10, 64)
		if err != nil || projectID <= 0 {
			problems = append(problems, fmt.Sprintf("%s: projectId must be the numeric FixForge project ID", project.Name))
			continue
		}
		root := project.LocalPath
		if project.RepoAppPath != "" {
			root = filepath.Join(root, project.RepoAppPath)
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			problems = append(problems, fmt.Sprintf("%s: project directory is unavailable", project.Name))
			continue
		}
		endpoint := fmt.Sprintf("http://%s/mcp/%d", localMCPBridgeAddr, projectID)
		if err := syncCodexMCPConfig(root, endpoint); err != nil {
			problems = append(problems, fmt.Sprintf("%s Codex: %v", project.Name, err))
		}
		if err := syncClaudeMCPConfig(root, endpoint); err != nil {
			problems = append(problems, fmt.Sprintf("%s Claude: %v", project.Name, err))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func syncCodexMCPConfig(root, endpoint string) error {
	path := filepath.Join(root, ".codex", "config.toml")
	if isTrackedProjectFile(root, ".codex/config.toml") {
		return errors.New("refusing to modify Git-tracked .codex/config.toml")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text := strings.ReplaceAll(string(current), "\r\n", "\n")
	const start = "# FixForge local MCP start"
	const end = "# FixForge local MCP end"
	if before, _, found := strings.Cut(text, start); found {
		_, after, hasEnd := strings.Cut(text, end)
		if !hasEnd {
			return errors.New("managed section is incomplete")
		}
		text = before + after
	} else if strings.Contains(text, "[mcp_servers.fixforge]") {
		return errors.New("an existing user-owned fixforge MCP server is configured")
	}
	block := fmt.Sprintf("%s\n[mcp_servers.fixforge]\nurl = %q\ndefault_tools_approval_mode = \"writes\"\n%s\n", start, endpoint, end)
	text = strings.TrimRight(text, "\n")
	if text != "" {
		text += "\n\n"
	}
	return os.WriteFile(path, []byte(text+block), 0o600)
}

func syncClaudeMCPConfig(root, endpoint string) error {
	path := filepath.Join(root, ".mcp.json")
	if isTrackedProjectFile(root, ".mcp.json") {
		return errors.New("refusing to modify Git-tracked .mcp.json")
	}
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	config := map[string]any{}
	if len(bytes.TrimSpace(current)) > 0 && json.Unmarshal(current, &config) != nil {
		return errors.New("existing .mcp.json is invalid JSON")
	}
	servers, _ := config["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		config["mcpServers"] = servers
	}
	if existing, found := servers["fixforge"].(map[string]any); found && !isManagedClaudeConfig(existing) {
		return errors.New("an existing user-owned fixforge MCP server is configured")
	}
	servers["fixforge"] = map[string]any{"type": "http", "url": endpoint}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

func isManagedClaudeConfig(config map[string]any) bool {
	return config["type"] == "http" && strings.HasPrefix(fmt.Sprint(config["url"]), "http://"+localMCPBridgeAddr+"/mcp/")
}

func isTrackedProjectFile(root, relative string) bool {
	command := exec.Command("git", "-C", root, "ls-files", "--error-unmatch", "--", relative)
	return command.Run() == nil
}
