package runner

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const runnerConfigDirName = ".fixforge"
const runnerConfigFileName = "runner.json"

// Config represents the runner's local configuration (runner.json).
type Config struct {
	Server            string          `json:"server"`
	ServerURL         string          `json:"serverUrl"`
	RunnerToken       string          `json:"runner_token"`
	Token             string          `json:"token"`
	RunnerID          string          `json:"runnerId"`
	RunnerName        string          `json:"runnerName"`
	DeviceName        string          `json:"device_name"`
	WorkspaceRoot     string          `json:"workspaceRoot"`
	WorkspaceRootOld  string          `json:"workspace_root"`
	MaxConcurrentRuns int             `json:"maxConcurrentRuns"`
	Projects          []ProjectConfig `json:"projects"`
}

// ProjectConfig holds the local configuration for a single project.
type ProjectConfig struct {
	ProjectID string `json:"projectId"`
	RepoURL   string `json:"repoUrl"`

	// Name is the project name (matching the server-side project name).
	Name string `json:"name"`

	// RepoAppPath is the sub-path within the repo (e.g., "apps/chat").
	RepoAppPath    string `json:"repo_app_path"`
	RepoAppPathNew string `json:"repoAppPath"`

	// LocalPath is the absolute path to the local git clone.
	LocalPath    string `json:"path"`
	LocalPathNew string `json:"localPath"`
	LocalPathOld string `json:"local_path"`
}

type ExecutorConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// BuiltinExecutor returns the runner command for a web-selected execution mode.
// runner.json no longer carries executor command configuration.
func BuiltinExecutor(executor string) (ExecutorConfig, string, bool) {
	name := strings.ToLower(strings.TrimSpace(executor))
	switch name {
	case "", "claude":
		cfg := ExecutorConfig{Command: "claude", Args: []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}}
		return cfg, cfg.Command, true
	case "codex":
		cfg := ExecutorConfig{Command: "codex", Args: []string{"exec"}}
		return cfg, cfg.Command, true
	default:
		return ExecutorConfig{}, "", false
	}
}

// DefaultConfigPath returns the per-user default path for runner.json.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return filepath.Join(home, runnerConfigDirName, runnerConfigFileName)
	}
	exe, err := os.Executable()
	if err != nil {
		return runnerConfigFileName
	}
	return filepath.Join(filepath.Dir(exe), runnerConfigFileName)
}

// LegacyConfigPath returns the old executable-adjacent runner.json path.
func LegacyConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return runnerConfigFileName
	}
	return filepath.Join(filepath.Dir(exe), runnerConfigFileName)
}

// LoadConfig reads and parses the runner.json config file.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found at %s — run 'fixforge-client connect ...' first", path)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.Normalize()
	return &cfg, nil
}

func (c *Config) Normalize() {
	if c.ServerURL == "" {
		c.ServerURL = c.Server
	}
	if c.Server == "" {
		c.Server = c.ServerURL
	}
	c.Server = normalizeLoopbackHost(c.Server)
	c.ServerURL = normalizeLoopbackHost(c.ServerURL)
	if c.Token == "" {
		c.Token = c.RunnerToken
	}
	if c.RunnerToken == "" {
		c.RunnerToken = c.Token
	}
	if c.RunnerName == "" {
		c.RunnerName = c.DeviceName
	}
	if c.DeviceName == "" {
		c.DeviceName = c.RunnerName
	}
	if c.RunnerID == "" {
		c.RunnerID = defaultRunnerID()
	}
	if c.WorkspaceRoot == "" {
		c.WorkspaceRoot = c.WorkspaceRootOld
	}
	if c.WorkspaceRootOld == "" {
		c.WorkspaceRootOld = c.WorkspaceRoot
	}
	if c.RunnerName == "" {
		c.RunnerName = DefaultDeviceName()
		c.DeviceName = c.RunnerName
	}
	if c.MaxConcurrentRuns <= 0 {
		c.MaxConcurrentRuns = 1
	}
	for i := range c.Projects {
		p := &c.Projects[i]
		if p.ProjectID == "" {
			p.ProjectID = p.Name
		}
		if p.Name == "" {
			p.Name = p.ProjectID
		}
		if p.RepoAppPath == "" {
			p.RepoAppPath = p.RepoAppPathNew
		}
		if p.RepoAppPathNew == "" {
			p.RepoAppPathNew = p.RepoAppPath
		}
		if p.LocalPath == "" {
			p.LocalPath = p.LocalPathOld
		}
		if p.LocalPathNew != "" {
			p.LocalPath = p.LocalPathNew
		}
		if p.LocalPathNew == "" {
			p.LocalPathNew = p.LocalPath
		}
	}
}

func normalizeLoopbackHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "::1" && host != "[::1]" {
		return raw
	}
	port := u.Port()
	if port != "" {
		u.Host = net.JoinHostPort("localhost", port)
	} else {
		u.Host = "localhost"
	}
	return u.String()
}

func (c *Config) ProjectIDs() []string {
	out := make([]string, 0, len(c.Projects)*2)
	seen := make(map[string]bool, len(c.Projects)*2)
	for _, p := range c.Projects {
		p.Normalize()
		if p.ProjectID != "" {
			key := strings.TrimSpace(p.ProjectID)
			if key != "" && !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
		if p.Name != "" {
			key := strings.TrimSpace(p.Name)
			if key != "" && !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	return out
}

func (c *Config) ExecutorNames() []string {
	return []string{"claude", "codex"}
}

func defaultRunnerID() string {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "runner"
	}
	return fmt.Sprintf("%s-%s", hostname, runtime.GOOS)
}

// SaveConfig writes the config to runner.json.
func SaveConfig(path string, cfg *Config) error {
	if path == "" {
		path = DefaultConfigPath()
	}
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	cfg.Normalize()
	data, err := json.MarshalIndent(minimalRunnerConfig{
		RunnerName: cfg.RunnerName,
		ServerURL:  cfg.ServerURL,
		Token:      cfg.Token,
		Projects:   minimalProjectConfigs(cfg.Projects),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

type minimalRunnerConfig struct {
	RunnerName string                 `json:"runnerName,omitempty"`
	ServerURL  string                 `json:"serverUrl"`
	Token      string                 `json:"token"`
	Projects   []minimalProjectConfig `json:"projects"`
}

type minimalProjectConfig struct {
	ProjectID   string `json:"projectId"`
	RepoURL     string `json:"repoUrl,omitempty"`
	RepoAppPath string `json:"repo_app_path,omitempty"`
	LocalPath   string `json:"localPath"`
}

func minimalProjectConfigs(projects []ProjectConfig) []minimalProjectConfig {
	out := make([]minimalProjectConfig, 0, len(projects))
	for _, project := range projects {
		project.Normalize()
		out = append(out, minimalProjectConfig{
			ProjectID:   project.ProjectID,
			RepoURL:     project.RepoURL,
			RepoAppPath: project.RepoAppPath,
			LocalPath:   project.LocalPath,
		})
	}
	return out
}

func (p *ProjectConfig) Normalize() {
	if p == nil {
		return
	}
	if p.ProjectID == "" {
		p.ProjectID = p.Name
	}
	if p.Name == "" {
		p.Name = p.ProjectID
	}
	if p.RepoAppPath == "" {
		p.RepoAppPath = p.RepoAppPathNew
	}
	if p.RepoAppPathNew == "" {
		p.RepoAppPathNew = p.RepoAppPath
	}
	if p.LocalPath == "" {
		p.LocalPath = p.LocalPathOld
	}
	if p.LocalPathNew != "" {
		p.LocalPath = p.LocalPathNew
	}
	if p.LocalPathNew == "" {
		p.LocalPathNew = p.LocalPath
	}
}

// DefaultDeviceName returns a default device name based on hostname.
func DefaultDeviceName() string {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	return hostname
}
