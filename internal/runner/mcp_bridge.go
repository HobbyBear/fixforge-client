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
	"time"
)

const localMCPBridgeAddr = "127.0.0.1:18421"

const (
	localMCPProjectIDHeader    = "X-FixForge-Project-ID"
	localMCPInstallationHeader = "X-FixForge-Installation-ID"
	localMCPRunnerNameHeader   = "X-FixForge-Runner-Name"
)

type localMCPBridge struct {
	cfg      *Config
	logger   *slog.Logger
	listener net.Listener
	server   *http.Server
	client   *http.Client

	cancel context.CancelFunc
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
	return &localMCPBridge{cfg: cfg, logger: logger, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (b *localMCPBridge) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", localMCPBridgeAddr)
	if err != nil {
		return fmt.Errorf("start local MCP bridge on %s: %w", localMCPBridgeAddr, err)
	}
	b.listener = listener
	serveCtx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	// Streamable HTTP clients may keep a GET response open for server
	// notifications, so the loopback bridge cannot impose a response deadline.
	b.server = &http.Server{
		Handler: http.HandlerFunc(b.handle), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return serveCtx },
	}
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
	if b.cancel != nil {
		b.cancel()
	}
	if b.server == nil {
		return nil
	}
	return b.server.Shutdown(ctx)
}

func (b *localMCPBridge) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	projectID, ok := localMCPProjectID(r.URL.Path)
	if !ok || !b.hasProject(projectID) {
		http.NotFound(w, r)
		return
	}
	var body io.Reader
	if r.Method == http.MethodPost {
		value, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
		if err != nil {
			http.Error(w, "MCP request is too large", http.StatusRequestEntityTooLarge)
			return
		}
		body = bytes.NewReader(value)
	}
	endpoint := strings.TrimRight(b.cfg.ServerURL, "/") + "/mcp"
	request, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint, body)
	if err != nil {
		http.Error(w, "invalid MCP endpoint", http.StatusBadGateway)
		return
	}
	request.Header.Set("Authorization", "Bearer "+b.cfg.RunnerToken)
	request.Header.Set(localMCPProjectIDHeader, strconv.FormatInt(projectID, 10))
	request.Header.Set(localMCPInstallationHeader, b.cfg.InstallationID)
	request.Header.Set(localMCPRunnerNameHeader, b.cfg.RunnerName)
	if r.Method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, header := range []string{"Accept", "MCP-Protocol-Version", "Mcp-Session-Id", "Last-Event-ID"} {
		if value := r.Header.Get(header); value != "" {
			request.Header.Set(header, value)
		}
	}
	client := b.client
	if r.Method == http.MethodGet {
		streamClient := *b.client
		streamClient.Timeout = 0
		client = &streamClient
	}
	response, err := client.Do(request)
	if err != nil {
		http.Error(w, "cloud MCP request failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for _, header := range []string{"Content-Type", "Cache-Control", "Mcp-Session-Id", "X-Accel-Buffering"} {
		if value := response.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if r.Method == http.MethodGet && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		copyMCPEventStream(w, response.Body)
		return
	}
	_, _ = io.Copy(w, io.LimitReader(response.Body, 2*1024*1024))
}

func copyMCPEventStream(w http.ResponseWriter, source io.Reader) {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 4096)
	for {
		read, err := source.Read(buffer)
		if read > 0 {
			if _, writeErr := w.Write(buffer[:read]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
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

// RegisterLocalMCPConnections creates or refreshes each configured local
// connection before a CLI invokes a tool, so a project manager can grant write
// access in the UI before the first model request. It does not create an MCP
// session or treat an explicitly revoked connection as a startup failure.
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
		if err := requestLocalMCPConnectionRegistration(ctx, client, cfg, projectID); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", project.Name, err))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func requestLocalMCPConnectionRegistration(ctx context.Context, client *http.Client, cfg *Config, projectID int64) error {
	endpoint := strings.TrimRight(cfg.ServerURL, "/") + "/api/runner/mcp-connections/register"
	payload, err := json.Marshal(map[string]any{"projectId": projectID, "installationId": cfg.InstallationID, "runnerName": cfg.RunnerName})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.RunnerToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("connection registration failed with status %d", response.StatusCode)
	}
	return nil
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
	block := fmt.Sprintf("%s\n[mcp_servers.fixforge]\nurl = %q\n%s\n", start, endpoint, end)
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
