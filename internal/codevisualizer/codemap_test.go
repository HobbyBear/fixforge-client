package codevisualizer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAttachCodeMapBuildsNumberedServicePath(t *testing.T) {
	raw := []byte(`{
		"title":"消息发送","summary":"接收后保存。","comparison":{"fingerprint":"abc"},
		"files":[{"index":0,"display_file":"apps/chat/controllers/message.go","new_file":"apps/chat/controllers/message.go","rows":[]}],
		"units":[
			{"id":"u1","file_index":0,"kind":"function","symbol":"Send","title":"接收消息","new_range":[10,20]},
			{"id":"u2","file_index":0,"kind":"function","symbol":"Save","title":"保存消息","new_range":[30,40]}
		],
		"flows":[{"title":"主流程","description":"消息保存","steps":[{"unit_id":"u1","label":"进入"},{"unit_id":"u2","label":"落库"}]}],
		"database_changes":[]
	}`)
	result, err := AttachCodeMap(raw)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(result, &view); err != nil {
		t.Fatal(err)
	}
	codeMap := view["code_map"].(map[string]any)
	edges := codeMap["edges"].([]any)
	if len(edges) != 2 || edges[0].(map[string]any)["number"] != "1" || edges[1].(map[string]any)["number"] != "2" {
		t.Fatalf("edges = %#v", edges)
	}
	nodes := codeMap["nodes"].([]any)
	if nodes[0].(map[string]any)["service"] != "apps/chat" {
		t.Fatalf("nodes = %#v", nodes)
	}
}

func TestAttachGoRepositoryContextAddsUnchangedCalleeAndStruct(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	path := filepath.Join(repo, "apps", "chat", "message.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "package chat\n\ntype Payload struct { ID string }\n\nfunc Entry() { helper() }\n\nfunc helper() {}\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, path, "package chat\n\ntype Payload struct { ID string }\n\nfunc Entry() {\n\thelper()\n}\n\nfunc helper() {}\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "change entry")
	head := runGit(t, repo, "rev-parse", "HEAD")
	raw := []byte(`{"title":"消息","summary":"发送","comparison":{"mode":"branch_compare","base_ref":"main","head_ref":"main","base_sha":"` + base + `","head_sha":"` + head + `","strategy":"direct"},"files":[{"index":0,"display_file":"apps/chat/message.go","new_file":"apps/chat/message.go","rows":[]}],"units":[{"id":"u1","file_index":0,"kind":"function","symbol":"chat.Entry","title":"入口","new_range":[5,7]}],"flows":[{"title":"主流程","steps":[{"unit_id":"u1"}]}],"database_changes":[]}`)
	withMap, err := AttachCodeMap(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := attachGoRepositoryContext(context.Background(), repo, withMap)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(result, &view); err != nil {
		t.Fatal(err)
	}
	codeMap := view["code_map"].(map[string]any)
	contextNodes := 0
	for _, rawNode := range codeMap["nodes"].([]any) {
		node := rawNode.(map[string]any)
		if node["change"] == "context" && node["label"] == "chat.helper" {
			contextNodes++
		}
	}
	if contextNodes != 1 {
		t.Fatalf("context helper nodes = %d, code map = %#v", contextNodes, codeMap)
	}
	foundASTEdge := false
	for _, rawEdge := range codeMap["edges"].([]any) {
		if rawEdge.(map[string]any)["evidence_kind"] == "go_ast_call" {
			foundASTEdge = true
		}
	}
	if !foundASTEdge {
		t.Fatalf("missing go_ast_call edge: %#v", codeMap["edges"])
	}
	if len(codeMap["data_structures"].([]any)) == 0 {
		t.Fatal("expected Payload structure in relevant context")
	}
}
