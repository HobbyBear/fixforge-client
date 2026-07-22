package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type localMCPProjectReference struct {
	ProjectID   string `json:"projectId"`
	Name        string `json:"name"`
	RepoURL     string `json:"repoUrl"`
	RepoAppPath string `json:"repoAppPath"`
}

type localMCPProjectResolution struct {
	Index     int    `json:"index"`
	ProjectID int64  `json:"projectId"`
	Name      string `json:"name"`
	Resolved  bool   `json:"resolved"`
	Error     string `json:"error"`
}

// ResolveLocalMCPProjectIDs replaces legacy project-name identifiers with the
// numeric IDs visible to the runner token and persists successful migrations.
func ResolveLocalMCPProjectIDs(ctx context.Context, cfg *Config) (bool, error) {
	if cfg == nil {
		return false, errors.New("runner configuration is required")
	}
	cfg.Normalize()
	if len(cfg.Projects) == 0 {
		return false, nil
	}
	references := make([]localMCPProjectReference, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		project.Normalize()
		references = append(references, localMCPProjectReference{
			ProjectID: project.ProjectID, Name: project.Name, RepoURL: project.RepoURL, RepoAppPath: project.RepoAppPath,
		})
	}
	payload, err := json.Marshal(map[string]any{"projects": references})
	if err != nil {
		return false, err
	}
	endpoint := strings.TrimRight(cfg.ServerURL, "/") + "/api/runner/mcp-projects/resolve"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.RunnerToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return false, fmt.Errorf("resolve projects: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 256*1024))
	if err != nil {
		return false, fmt.Errorf("read project resolution response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("project resolution request failed with status %d", response.StatusCode)
	}
	var decoded struct {
		Projects []localMCPProjectResolution `json:"projects"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return false, fmt.Errorf("invalid project resolution response: %w", err)
	}

	changed := false
	seen := make(map[int]bool, len(decoded.Projects))
	var problems []string
	for _, resolution := range decoded.Projects {
		if resolution.Index < 0 || resolution.Index >= len(cfg.Projects) || seen[resolution.Index] {
			problems = append(problems, "server returned an invalid project resolution index")
			continue
		}
		seen[resolution.Index] = true
		project := &cfg.Projects[resolution.Index]
		if !resolution.Resolved || resolution.ProjectID <= 0 {
			message := strings.TrimSpace(resolution.Error)
			if message == "" {
				message = "project could not be resolved"
			}
			problems = append(problems, fmt.Sprintf("%s: %s", project.Name, message))
			continue
		}
		projectID := strconv.FormatInt(resolution.ProjectID, 10)
		if project.ProjectID != projectID {
			project.ProjectID = projectID
			changed = true
		}
		if name := strings.TrimSpace(resolution.Name); name != "" && project.Name != name {
			project.Name = name
			changed = true
		}
	}
	for index, project := range cfg.Projects {
		if !seen[index] {
			problems = append(problems, fmt.Sprintf("%s: server omitted project resolution", project.Name))
		}
	}
	if changed && cfg.configPath != "" {
		if err := SaveConfig(cfg.configPath, cfg); err != nil {
			problems = append(problems, "persist migrated runner configuration: "+err.Error())
		}
	}
	if len(problems) > 0 {
		return changed, errors.New(strings.Join(problems, "; "))
	}
	return changed, nil
}
