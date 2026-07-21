package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if !strings.Contains(string(codex), "http://127.0.0.1:18421/mcp/42") || strings.Contains(string(codex), "token") {
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

func TestRegisterLocalMCPConnectionsUsesLeaseWithoutPersistingIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runner/mcp-lease" || r.Header.Get("Authorization") != "Bearer runner-token" {
			t.Fatalf("unexpected lease request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = fmt.Fprint(w, `{"mcpUrl":"https://fixforge.example.com/mcp","sessionToken":"short-lived","expiresAt":"2099-01-01T00:00:00Z"}`)
	}))
	defer server.Close()
	if err := RegisterLocalMCPConnections(context.Background(), &Config{ServerURL: server.URL, RunnerToken: "runner-token", InstallationID: "install-1", RunnerName: "laptop", Projects: []ProjectConfig{{ProjectID: "42", Name: "demo"}}}); err != nil {
		t.Fatalf("RegisterLocalMCPConnections: %v", err)
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
