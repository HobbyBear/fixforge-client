package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLocalMCPProjectIDsPersistsPartialMigration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runner/mcp-projects/resolve" || r.Header.Get("Authorization") != "Bearer runner-token" {
			t.Fatalf("unexpected resolution request: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request struct {
			Projects []localMCPProjectReference `json:"projects"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Projects) != 2 || request.Projects[0].ProjectID != "chat" {
			t.Fatalf("projects = %#v", request.Projects)
		}
		_, _ = fmt.Fprint(w, `{"projects":[{"index":0,"projectId":42,"name":"chat","resolved":true},{"index":1,"resolved":false,"error":"no accessible FixForge project matches this configuration"}]}`)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "runner.json")
	cfg := &Config{
		ServerURL: server.URL, Token: "runner-token",
		Projects: []ProjectConfig{
			{ProjectID: "chat", Name: "chat", LocalPath: t.TempDir()},
			{ProjectID: "stale", Name: "stale", LocalPath: t.TempDir()},
		},
	}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ResolveLocalMCPProjectIDs(context.Background(), loaded)
	if !changed || err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed=%v error=%v", changed, err)
	}
	if loaded.Projects[0].ProjectID != "42" {
		t.Fatalf("resolved project = %#v", loaded.Projects[0])
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Projects[0].ProjectID != "42" || reloaded.Projects[1].ProjectID != "stale" {
		t.Fatalf("persisted projects = %#v", reloaded.Projects)
	}
}

func TestSyncLocalMCPProjectConfigsWritesLoopbackOnly(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{Projects: []ProjectConfig{{ProjectID: "42", Name: "demo", LocalPath: root}}}
	if err := SyncLocalMCPProjectConfigs(cfg); err != nil {
		t.Fatalf("SyncLocalMCPProjectConfigs: %v", err)
	}
	codex, err := os.ReadFile(filepath.Join(root, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(codex), "http://127.0.0.1:18421/mcp/42") || strings.Contains(string(codex), "token") || strings.Contains(string(codex), "default_tools_approval_mode") {
		t.Fatalf("unexpected Codex config: %s", codex)
	}
	claude, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(claude), "http://127.0.0.1:18421/mcp/42") || strings.Contains(string(claude), "sessionToken") {
		t.Fatalf("unexpected Claude config: %s", claude)
	}
}

func TestLocalMCPBridgeForwardsToolChangeEventStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer runner-token" || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Fatalf("unexpected MCP stream request: path=%s method=%s auth=%q accept=%q", r.URL.Path, r.Method, r.Header.Get("Authorization"), r.Header.Get("Accept"))
		}
		if r.Header.Get(localMCPProjectIDHeader) != "42" || r.Header.Get(localMCPInstallationHeader) != "install-1" || r.Header.Get(localMCPRunnerNameHeader) != "laptop" {
			t.Fatalf("unexpected runner identity headers: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n")
	}))
	defer server.Close()

	bridge, err := NewLocalMCPBridge(&Config{
		ServerURL: server.URL, RunnerToken: "runner-token", InstallationID: "install-1", RunnerName: "laptop",
		Projects: []ProjectConfig{{ProjectID: "42", Name: "demo"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/mcp/42", nil)
	req.Header.Set("Accept", "text/event-stream")
	recorder := httptest.NewRecorder()
	bridge.handle(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "notifications/tools/list_changed") {
		t.Fatalf("stream response = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestRegisterLocalMCPConnectionsDoesNotCreateLease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runner/mcp-connections/register" || r.Header.Get("Authorization") != "Bearer runner-token" {
			t.Fatalf("unexpected registration request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = fmt.Fprint(w, `{"connectionId":9,"status":"revoked"}`)
	}))
	defer server.Close()
	if err := RegisterLocalMCPConnections(context.Background(), &Config{ServerURL: server.URL, RunnerToken: "runner-token", InstallationID: "install-1", RunnerName: "laptop", Projects: []ProjectConfig{{ProjectID: "42", Name: "demo"}}}); err != nil {
		t.Fatalf("RegisterLocalMCPConnections: %v", err)
	}
}

func TestRegisterLocalMCPConnectionsSkipsUnresolvedProjectID(t *testing.T) {
	err := RegisterLocalMCPConnections(context.Background(), &Config{Projects: []ProjectConfig{{ProjectID: "fixforge", Name: "fixforge"}}})
	if err != nil {
		t.Fatalf("RegisterLocalMCPConnections() error = %v", err)
	}
}

func TestLocalMCPProjectIDOnlyAcceptsProjectScopedPath(t *testing.T) {
	for _, path := range []string{"/mcp/42", "mcp/42"} {
		if id, ok := localMCPProjectID(path); !ok || id != 42 {
			t.Fatalf("localMCPProjectID(%q) = %d, %v", path, id, ok)
		}
	}
	for _, path := range []string{"/mcp", "/mcp/42/other", "/mcp/nope", "/other/42"} {
		if _, ok := localMCPProjectID(path); ok {
			t.Fatalf("localMCPProjectID(%q) unexpectedly succeeded", path)
		}
	}
}
