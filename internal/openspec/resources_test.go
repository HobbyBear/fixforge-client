package openspec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListMissingOpenSpec(t *testing.T) {
	payload, err := List(t.TempDir())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if payload["available"] != false {
		t.Fatalf("expected unavailable workspace, got %#v", payload["available"])
	}
}

func TestCreateChangeFromWorkflowTemplateAndArchive(t *testing.T) {
	root := t.TempDir()
	templateDir := filepath.Join(root, "openspec", "schemas", "requirement-spec", "templates")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "proposal.md"), []byte("# Proposal\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := CreateChange(root, "sample-change", "requirement-spec")
	if err != nil {
		t.Fatalf("CreateChange returned error: %v", err)
	}
	if created["change"] != "sample-change" {
		t.Fatalf("unexpected change name: %#v", created["change"])
	}
	if _, err := os.Stat(filepath.Join(root, "openspec", "changes", "sample-change", "proposal.md")); err != nil {
		t.Fatalf("expected copied proposal.md: %v", err)
	}

	listed, err := List(root)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	changes, ok := listed["changes"].([]Change)
	if !ok || len(changes) != 1 || changes[0].Name != "sample-change" || len(changes[0].Documents) != 1 {
		t.Fatalf("unexpected listed changes: %#v", listed["changes"])
	}

	archived, err := ArchiveChange(root, "sample-change")
	if err != nil {
		t.Fatalf("ArchiveChange returned error: %v", err)
	}
	archivePath, ok := archived["archived"].(string)
	if !ok || archivePath == "" {
		t.Fatalf("unexpected archive path: %#v", archived["archived"])
	}
	if _, err := os.Stat(filepath.Join(root, archivePath, "proposal.md")); err != nil {
		t.Fatalf("expected archived proposal.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "openspec", "changes", "sample-change")); !os.IsNotExist(err) {
		t.Fatalf("expected active change to move, stat err=%v", err)
	}
}

func TestCreateChangeRejectsUnsafeName(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "openspec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateChange(root, "../bad", ""); err == nil {
		t.Fatal("expected invalid openspec name error")
	}
}

func TestDeleteChangeRemovesActiveOrArchivedChange(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "openspec", "changes", "archive", "2026-07-05-old-change"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "openspec", "changes", "active-change"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "openspec", "changes", "active-change", "proposal.md"), []byte("# Proposal\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deleted, err := DeleteChange(root, "active-change")
	if err != nil {
		t.Fatalf("DeleteChange active returned error: %v", err)
	}
	if deleted["deleted"] != "openspec/changes/active-change" {
		t.Fatalf("unexpected active delete path: %#v", deleted["deleted"])
	}
	if _, err := os.Stat(filepath.Join(root, "openspec", "changes", "active-change")); !os.IsNotExist(err) {
		t.Fatalf("expected active change removed, stat err=%v", err)
	}

	deleted, err = DeleteChange(root, "2026-07-05-old-change")
	if err != nil {
		t.Fatalf("DeleteChange archived returned error: %v", err)
	}
	if deleted["deleted"] != "openspec/changes/archive/2026-07-05-old-change" {
		t.Fatalf("unexpected archived delete path: %#v", deleted["deleted"])
	}
	if _, err := os.Stat(filepath.Join(root, "openspec", "changes", "archive", "2026-07-05-old-change")); !os.IsNotExist(err) {
		t.Fatalf("expected archived change removed, stat err=%v", err)
	}
}
