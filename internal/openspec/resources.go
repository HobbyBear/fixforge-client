package openspec

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Operation struct {
	Operation    string
	Change       string
	WorkflowMode string
}

type Document struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	Size     int64  `json:"size,omitempty"`
	Modified string `json:"modified_at,omitempty"`
}

type WorkflowMode struct {
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	Templates []Document `json:"templates,omitempty"`
}

type Change struct {
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	Archived  bool       `json:"archived,omitempty"`
	Updated   string     `json:"updated_at,omitempty"`
	Documents []Document `json:"documents,omitempty"`
}

var safeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func RunResourceOperation(root string, op Operation) (map[string]any, error) {
	switch strings.TrimSpace(op.Operation) {
	case "list":
		return List(root)
	case "create":
		return CreateChange(root, op.Change, op.WorkflowMode)
	case "archive":
		return ArchiveChange(root, op.Change)
	case "delete":
		return DeleteChange(root, op.Change)
	default:
		return nil, fmt.Errorf("unsupported openspec operation")
	}
}

func List(root string) (map[string]any, error) {
	openspecRoot := filepath.Join(root, "openspec")
	info, err := os.Stat(openspecRoot)
	if err != nil || !info.IsDir() {
		return map[string]any{
			"object":    "openspec.workspace",
			"available": false,
			"root":      "openspec",
			"workflows": []WorkflowMode{},
			"changes":   []Change{},
			"archived":  []Change{},
		}, nil
	}
	workflows := listWorkflowModes(openspecRoot)
	changes := listChanges(filepath.Join(openspecRoot, "changes"), false)
	archived := listChanges(filepath.Join(openspecRoot, "changes", "archive"), true)
	return map[string]any{
		"object":    "openspec.workspace",
		"available": true,
		"root":      "openspec",
		"workflows": workflows,
		"changes":   changes,
		"archived":  archived,
	}, nil
}

func CreateChange(root, changeName, workflowMode string) (map[string]any, error) {
	changeName, err := cleanOpenSpecName(changeName)
	if err != nil {
		return nil, err
	}
	openspecRoot := filepath.Join(root, "openspec")
	if info, err := os.Stat(openspecRoot); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("openspec directory not found")
	}
	changeDir := filepath.Join(openspecRoot, "changes", changeName)
	if _, err := os.Stat(changeDir); err == nil {
		return nil, fmt.Errorf("change already exists")
	}
	if err := os.MkdirAll(changeDir, 0o755); err != nil {
		return nil, err
	}
	copied := 0
	modeName, err := cleanOptionalOpenSpecName(workflowMode)
	if err != nil {
		return nil, err
	}
	if modeName != "" {
		templateDir := filepath.Join(openspecRoot, "schemas", modeName, "templates")
		if info, err := os.Stat(templateDir); err == nil && info.IsDir() {
			n, err := copyTemplateFiles(templateDir, changeDir)
			if err != nil {
				return nil, err
			}
			copied = n
		}
	}
	if copied == 0 {
		for _, item := range defaultTemplateFiles(changeName) {
			target := filepath.Join(changeDir, item.name)
			if err := os.WriteFile(target, []byte(item.content), 0o644); err != nil {
				return nil, err
			}
		}
	}
	list, _ := List(root)
	return map[string]any{
		"ok":            true,
		"change":        changeName,
		"path":          filepath.ToSlash(filepath.Join("openspec", "changes", changeName)),
		"workflow_mode": modeName,
		"workspace":     list,
	}, nil
}

func ArchiveChange(root, changeName string) (map[string]any, error) {
	changeName, err := cleanOpenSpecName(changeName)
	if err != nil {
		return nil, err
	}
	if changeName == "archive" {
		return nil, fmt.Errorf("archive is a reserved change name")
	}
	openspecRoot := filepath.Join(root, "openspec")
	source := filepath.Join(openspecRoot, "changes", changeName)
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("change not found")
	}
	archiveRoot := filepath.Join(openspecRoot, "changes", "archive")
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		return nil, err
	}
	base := time.Now().Format("2006-01-02") + "-" + changeName
	target := filepath.Join(archiveRoot, base)
	for i := 2; ; i++ {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			break
		}
		target = filepath.Join(archiveRoot, fmt.Sprintf("%s-%d", base, i))
	}
	if err := os.Rename(source, target); err != nil {
		return nil, err
	}
	list, _ := List(root)
	return map[string]any{
		"ok":        true,
		"change":    changeName,
		"archived":  filepath.ToSlash(targetPathFromOpenSpecRoot(openspecRoot, target)),
		"workspace": list,
	}, nil
}

func DeleteChange(root, changeName string) (map[string]any, error) {
	changeName, err := cleanOpenSpecName(changeName)
	if err != nil {
		return nil, err
	}
	if changeName == "archive" {
		return nil, fmt.Errorf("archive is a reserved change name")
	}
	openspecRoot := filepath.Join(root, "openspec")
	candidates := []struct {
		abs string
		rel string
	}{
		{
			abs: filepath.Join(openspecRoot, "changes", changeName),
			rel: filepath.Join("openspec", "changes", changeName),
		},
		{
			abs: filepath.Join(openspecRoot, "changes", "archive", changeName),
			rel: filepath.Join("openspec", "changes", "archive", changeName),
		},
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate.abs)
		if err != nil || !info.IsDir() {
			continue
		}
		if err := os.RemoveAll(candidate.abs); err != nil {
			return nil, err
		}
		list, _ := List(root)
		return map[string]any{
			"ok":        true,
			"change":    changeName,
			"deleted":   filepath.ToSlash(candidate.rel),
			"workspace": list,
		}, nil
	}
	return nil, fmt.Errorf("change not found")
}

func listWorkflowModes(openspecRoot string) []WorkflowMode {
	schemasRoot := filepath.Join(openspecRoot, "schemas")
	entries, err := os.ReadDir(schemasRoot)
	if err != nil {
		return []WorkflowMode{}
	}
	out := make([]WorkflowMode, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		modePath := filepath.ToSlash(filepath.Join("openspec", "schemas", entry.Name()))
		out = append(out, WorkflowMode{
			Name:      entry.Name(),
			Path:      modePath,
			Templates: listDocuments(filepath.Join(schemasRoot, entry.Name(), "templates"), filepath.Join("openspec", "schemas", entry.Name(), "templates"), 2),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func listChanges(changesRoot string, archived bool) []Change {
	entries, err := os.ReadDir(changesRoot)
	if err != nil {
		return []Change{}
	}
	out := make([]Change, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if !archived && entry.Name() == "archive" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		rel := filepath.Join("openspec", "changes", entry.Name())
		if archived {
			rel = filepath.Join("openspec", "changes", "archive", entry.Name())
		}
		abs := filepath.Join(changesRoot, entry.Name())
		out = append(out, Change{
			Name:      entry.Name(),
			Path:      filepath.ToSlash(rel),
			Archived:  archived,
			Updated:   info.ModTime().Format(time.RFC3339),
			Documents: listDocuments(abs, rel, 5),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func listDocuments(root, relRoot string, maxDepth int) []Document {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return []Document{}
	}
	var out []Document
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		depth := len(strings.Split(filepath.ToSlash(rel), "/"))
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" {
				return filepath.SkipDir
			}
			if depth > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if depth > maxDepth || !isOpenSpecDocument(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		docPath := filepath.ToSlash(filepath.Join(relRoot, rel))
		out = append(out, Document{
			Name:     d.Name(),
			Path:     docPath,
			Type:     strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."),
			Size:     info.Size(),
			Modified: info.ModTime().Format(time.RFC3339),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return documentRank(out[i]) < documentRank(out[j]) })
	return out
}

func documentRank(doc Document) string {
	name := strings.ToLower(doc.Name)
	prefix := "9"
	switch name {
	case "requirement.md":
		prefix = "0"
	case "proposal.md":
		prefix = "1"
	case "design.md":
		prefix = "2"
	case "tasks.md":
		prefix = "3"
	case "test-report.md":
		prefix = "4"
	}
	return prefix + ":" + doc.Path
}

func isOpenSpecDocument(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".sql", ".json", ".yaml", ".yml", ".txt":
		return true
	default:
		return false
	}
}

func copyTemplateFiles(srcDir, dstDir string) (int, error) {
	count := 0
	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == srcDir {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !isOpenSpecDocument(path) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := copyFile(path, target); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func defaultTemplateFiles(changeName string) []struct {
	name    string
	content string
} {
	title := strings.ReplaceAll(changeName, "-", " ")
	return []struct {
		name    string
		content string
	}{
		{"requirement.md", "# Requirement: " + title + "\n\n## Background\n\n## Goals\n\n## Non-Goals\n"},
		{"proposal.md", "# Proposal: " + title + "\n\n## Summary\n\n## Scope\n\n## Acceptance Criteria\n"},
		{"design.md", "# Design: " + title + "\n\n## Overview\n\n## Implementation\n\n## Risks\n"},
		{"tasks.md", "# Tasks: " + title + "\n\n- [ ] Confirm requirements\n- [ ] Implement changes\n- [ ] Run validation\n"},
	}
}

func cleanOpenSpecName(raw string) (string, error) {
	name, err := cleanOptionalOpenSpecName(raw)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("change is required")
	}
	return name, nil
}

func cleanOptionalOpenSpecName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", nil
	}
	if !safeNamePattern.MatchString(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid openspec name")
	}
	return name, nil
}

func targetPathFromOpenSpecRoot(openspecRoot, target string) string {
	rel, err := filepath.Rel(filepath.Dir(openspecRoot), target)
	if err != nil {
		return target
	}
	return rel
}
