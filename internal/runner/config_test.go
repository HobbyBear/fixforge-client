package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveConfigPreservesNumericProjectIDAndName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.json")
	cfg := &Config{
		ServerURL: "http://localhost:8991",
		Token:     "runner-token",
		Projects: []ProjectConfig{{
			ProjectID: "42",
			Name:      "fixforge",
			LocalPath: t.TempDir(),
		}},
	}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Projects []struct {
			ProjectID string `json:"projectId"`
			Name      string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Projects) != 1 || saved.Projects[0].ProjectID != "42" || saved.Projects[0].Name != "fixforge" {
		t.Fatalf("saved projects = %#v", saved.Projects)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Projects[0].ProjectID != "42" || loaded.Projects[0].Name != "fixforge" {
		t.Fatalf("loaded project = %#v", loaded.Projects[0])
	}
	if loaded.configPath != path {
		t.Fatalf("config path = %q, want %q", loaded.configPath, path)
	}
}
