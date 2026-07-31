package codevisualizer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateDataUsesBundledValidator(t *testing.T) {
	if _, _, err := pythonCommand(); err != nil {
		t.Skip(err)
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "master")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	appDir := filepath.Join(repo, "apps", "demo")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(appDir, "demo.go"), "package demo\n\nfunc Value() int { return 1 }\n")
	runGit(t, repo, "add", "apps/demo/demo.go")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(appDir, "demo.go"), "package demo\n\nfunc Value() int { return 2 }\n")
	runGit(t, repo, "add", "apps/demo/demo.go")
	runGit(t, repo, "commit", "-qm", "change value")
	head := runGit(t, repo, "rev-parse", "HEAD")

	walkthrough := map[string]any{
		"version": 2,
		"title":   "Value 变更",
		"summary": "调整返回值。",
		"comparison": map[string]any{
			"mode": "branch_compare", "base_ref": base, "head_ref": head, "strategy": "direct",
		},
		"flows": []any{},
		"changes": []any{map[string]any{
			"file": "apps/demo/demo.go", "purpose": "调整返回值。", "implementation": "修改 Value。",
			"units": []any{map[string]any{
				"id": "demo.value", "kind": "function", "symbol": "Demo.Value", "title": "更新返回值",
				"old_range": []int{3, 3}, "new_range": []int{3, 3},
				"meaning": "返回新的固定值。", "reason": "匹配新行为。", "impact": "调用方得到新值。",
			}},
		}},
		"database_changes": []any{map[string]any{
			"对象": "demo_values", "变更": "修改", "code_targets": []string{"demo.value"},
		}},
		"config_changes": []any{}, "api_changes": []any{},
		"log_points": []any{map[string]any{
			"事件": "value_changed", "级别": "INFO", "code_targets": []string{"demo.value"},
		}},
	}
	input, err := json.Marshal(walkthrough)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := GenerateData(ctx, appDir, input)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Title           string `json:"title"`
		DatabaseChanges []any  `json:"database_changes"`
		LogPoints       []any  `json:"log_points"`
		Files           []struct {
			Rows []struct {
				NewLine     *int     `json:"new_line"`
				NoteUnitIDs []string `json:"note_unit_ids"`
			} `json:"rows"`
		} `json:"files"`
		Units []struct {
			ID string `json:"id"`
		} `json:"units"`
	}
	if err := json.Unmarshal(output, &view); err != nil {
		t.Fatal(err)
	}
	if view.Title != "Value 变更" || len(view.Files) != 1 {
		t.Fatalf("unexpected view model: %#v", view)
	}
	if len(view.DatabaseChanges) != 0 || len(view.LogPoints) != 0 {
		t.Fatalf("unsupported model impacts were not discarded: database=%#v logs=%#v", view.DatabaseChanges, view.LogPoints)
	}
	if len(view.Units) != 1 || !strings.HasPrefix(view.Units[0].ID, "git-change.") {
		t.Fatalf("unit ID was not derived from Git: %#v", view.Units)
	}
	anchored := false
	for _, row := range view.Files[0].Rows {
		if row.NewLine != nil && *row.NewLine == 3 && len(row.NoteUnitIDs) == 1 && row.NoteUnitIDs[0] == view.Units[0].ID {
			anchored = true
		}
	}
	if !anchored {
		t.Fatal("AI note is not anchored to the changed source line")
	}
}

func TestExtractJSONAndLockComparison(t *testing.T) {
	input, err := ExtractJSON("```json\n{\"version\":2,\"comparison\":{\"base_ref\":\"wrong\"}}\n```")
	if err != nil {
		t.Fatal(err)
	}
	locked := map[string]any{"mode": "branch_compare", "base_ref": "main", "head_ref": "feature", "strategy": "merge_base"}
	input, err = WithComparison(input, locked)
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := Comparison(input)
	if err != nil {
		t.Fatal(err)
	}
	if comparison["base_ref"] != "main" || comparison["head_ref"] != "feature" {
		t.Fatalf("comparison was not locked: %#v", comparison)
	}
}

func TestGenerateDataUsesTrackedWorkingTreeFilesOnly(t *testing.T) {
	if _, _, err := pythonCommand(); err != nil {
		t.Skip(err)
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	writeTestFile(t, filepath.Join(repo, "README.md"), "base\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(repo, "local.go"), "package demo\n\nfunc LocalValue() int { return 1 }\n")
	runGit(t, repo, "add", "local.go")
	writeTestFile(t, filepath.Join(repo, "ignored.txt"), "untracked\n")
	statusBefore := runGit(t, repo, "status", "--porcelain=v1")

	walkthrough := map[string]any{
		"version": 2, "title": "本地未提交代码", "summary": "增加本地实现。", "flows": []any{},
		"comparison": map[string]any{
			"mode": "working_tree", "base_ref": base, "head_ref": "WORKTREE", "strategy": "working_tree",
		},
		"changes": []any{map[string]any{
			"old_file": nil, "new_file": "local.go", "purpose": "增加本地实现。", "implementation": "新增 LocalValue。",
			"units": []any{map[string]any{
				"id": "demo.local-value", "kind": "function", "symbol": "demo.LocalValue", "title": "新增本地值",
				"old_range": nil, "new_range": []int{1, 3},
				"meaning": "返回本地值。", "reason": "覆盖未提交代码审核。", "impact": "新增一个可调用函数。",
			}},
		}},
		"database_changes": []any{}, "config_changes": []any{}, "api_changes": []any{}, "log_points": []any{},
	}
	input, err := json.Marshal(walkthrough)
	if err != nil {
		t.Fatal(err)
	}
	output, err := GenerateData(context.Background(), repo, input)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Comparison map[string]any `json:"comparison"`
		Files      []struct {
			Path  string `json:"display_file"`
			Units []struct {
				Symbol string `json:"symbol"`
			} `json:"units"`
		} `json:"files"`
	}
	if err := json.Unmarshal(output, &view); err != nil {
		t.Fatal(err)
	}
	if view.Comparison["mode"] != "working_tree" || len(view.Files) != 1 || view.Files[0].Path != "local.go" {
		t.Fatalf("unexpected working tree visualization: %#v", view)
	}
	if len(view.Files[0].Units) != 1 || view.Files[0].Units[0].Symbol != "LocalValue" {
		t.Fatalf("working tree symbol was not normalized: %#v", view.Files[0].Units)
	}
	if got := runGit(t, repo, "status", "--porcelain=v1"); got != statusBefore {
		t.Fatalf("visualizer changed the working tree:\nbefore: %s\nafter: %s", statusBefore, got)
	}
}

func TestGenerateDataKeepsOnlySourceBackedDatabaseChanges(t *testing.T) {
	if _, _, err := pythonCommand(); err != nil {
		t.Skip(err)
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "master")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	path := filepath.Join(repo, "demo.go")
	writeTestFile(t, path, "package demo\n\nfunc Save(db interface { Table(string) any; Where(string, ...any) any }) {\n}\n")
	runGit(t, repo, "add", "demo.go")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, path, "package demo\n\nfunc Save(db interface { Table(string) any; Where(string, ...any) any }) {\n\tdb.Table(\"users\").Where(\"id = ?\", 1)\n}\n")
	runGit(t, repo, "add", "demo.go")
	runGit(t, repo, "commit", "-qm", "add database lookup")
	head := runGit(t, repo, "rev-parse", "HEAD")

	walkthrough := map[string]any{
		"version": 2, "title": "数据库变更", "summary": "增加查询。", "flows": []any{},
		"comparison": map[string]any{
			"mode": "branch_compare", "base_ref": base, "head_ref": head, "strategy": "direct",
		},
		"changes": []any{map[string]any{
			"file": "demo.go", "purpose": "增加查询。", "implementation": "调用 Where。",
			"units": []any{map[string]any{
				"id": "demo.save", "kind": "function", "symbol": "Save", "title": "查询记录",
				"old_range": []int{3, 4}, "new_range": []int{3, 5},
				"meaning": "按 ID 查询。", "reason": "读取目标记录。", "impact": "增加一次数据库查询。",
			}},
		}},
		"database_changes": []any{map[string]any{
			"对象": "hallucinated_table", "SQL": "DROP TABLE wrong", "发布影响": "关注查询负载。", "code_targets": []string{"demo.save"},
		}},
		"config_changes": []any{}, "api_changes": []any{}, "log_points": []any{},
	}
	input, err := json.Marshal(walkthrough)
	if err != nil {
		t.Fatal(err)
	}
	output, err := GenerateData(context.Background(), repo, input)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		DatabaseChanges []map[string]any `json:"database_changes"`
	}
	if err := json.Unmarshal(output, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.DatabaseChanges) != 1 {
		t.Fatalf("database changes = %#v", view.DatabaseChanges)
	}
	change := view.DatabaseChanges[0]
	if change["对象"] != "demo.go" || !strings.Contains(change["SQL"].(string), "SELECT") {
		t.Fatalf("source-backed database facts were overwritten: %#v", change)
	}
	tables, ok := change["表"].([]any)
	if !ok || len(tables) != 1 || tables[0] != "users" {
		t.Fatalf("database table summary = %#v", change["表"])
	}
	if change["发布影响"] != "关注查询负载。" {
		t.Fatalf("manual enrichment was not retained: %#v", change)
	}
}

func TestGenerateDataSummarizesNewDatabaseTableAndFields(t *testing.T) {
	if _, _, err := pythonCommand(); err != nil {
		t.Skip(err)
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "master")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	path := filepath.Join(repo, "migrations", "001_preferences.sql")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "-- preferences schema\n")
	runGit(t, repo, "add", "migrations/001_preferences.sql")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, path, "-- preferences schema\nCREATE TABLE preferences (\n  id BIGINT PRIMARY KEY,\n  config_key VARCHAR(128),\n  config_value TEXT\n);\n")
	runGit(t, repo, "add", "migrations/001_preferences.sql")
	runGit(t, repo, "commit", "-qm", "add preferences table")
	head := runGit(t, repo, "rev-parse", "HEAD")

	walkthrough := map[string]any{
		"version": 2, "title": "配置表", "summary": "增加配置表。", "flows": []any{},
		"comparison": map[string]any{"mode": "branch_compare", "base_ref": base, "head_ref": head, "strategy": "direct"},
		"changes": []any{map[string]any{
			"file": "migrations/001_preferences.sql", "purpose": "增加配置表。", "implementation": "创建 preferences 表。",
			"units": []any{map[string]any{
				"id": "database.preferences", "kind": "block", "symbol": "create-preferences", "title": "创建配置表",
				"old_range": []int{1, 1}, "new_range": []int{2, 6},
				"meaning": "保存动态配置。", "reason": "支持配置持久化。", "impact": "部署时执行迁移。",
			}},
		}},
		"database_changes": []any{}, "config_changes": []any{}, "api_changes": []any{}, "log_points": []any{},
	}
	input, err := json.Marshal(walkthrough)
	if err != nil {
		t.Fatal(err)
	}
	output, err := GenerateData(context.Background(), repo, input)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		DatabaseChanges []map[string]any `json:"database_changes"`
	}
	if err := json.Unmarshal(output, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.DatabaseChanges) != 1 {
		t.Fatalf("database changes = %#v", view.DatabaseChanges)
	}
	change := view.DatabaseChanges[0]
	if change["类型"] != "新建表" {
		t.Fatalf("database type = %#v", change)
	}
	if got := stringValues(change["表"]); !equalStrings(got, []string{"preferences"}) {
		t.Fatalf("tables = %#v", got)
	}
	if got := stringValues(change["字段"]); !equalStrings(got, []string{"id", "config_key", "config_value"}) {
		t.Fatalf("fields = %#v", got)
	}
}

func TestNormalizeUnitSymbolsDowngradesMissingDeclaration(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "master")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	path := filepath.Join(repo, "demo.go")
	writeTestFile(t, path, "package demo\n\nfunc Removed() int { return 1 }\n")
	runGit(t, repo, "add", "demo.go")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, path, "package demo\n\nfunc Added() int { return 2 }\n")
	runGit(t, repo, "add", "demo.go")
	runGit(t, repo, "commit", "-qm", "replace function")
	head := runGit(t, repo, "rev-parse", "HEAD")

	walkthrough := map[string]any{
		"comparison": map[string]any{
			"base_ref": base, "head_ref": head, "strategy": "direct",
		},
		"changes": []any{map[string]any{
			"file": "demo.go",
			"units": []any{map[string]any{
				"kind": "function", "symbol": "Removed",
			}},
		}},
	}
	input, err := json.Marshal(walkthrough)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeUnitSymbols(context.Background(), repo, input)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(normalized, &result); err != nil {
		t.Fatal(err)
	}
	change := result["changes"].([]any)[0].(map[string]any)
	unit := change["units"].([]any)[0].(map[string]any)
	if unit["kind"] != "block" || unit["symbol"] != "Removed" {
		t.Fatalf("missing declaration was not preserved as a block: %#v", unit)
	}
}

func TestGenerateDataPrunesSemanticUnitsWithoutChangedLines(t *testing.T) {
	if _, _, err := pythonCommand(); err != nil {
		t.Skip(err)
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "master")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	path := filepath.Join(repo, "character.md")
	writeTestFile(t, path, "# Character\n\nOld behavior.\n\n## Conflict notes\nNo conflict.\n")
	runGit(t, repo, "add", "character.md")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, path, "# Character\n\nNew behavior.\n\n## Conflict notes\nNo conflict.\n")
	runGit(t, repo, "add", "character.md")
	runGit(t, repo, "commit", "-qm", "update character")
	head := runGit(t, repo, "rev-parse", "HEAD")

	walkthrough := map[string]any{
		"version": 2, "title": "角色文档", "summary": "更新角色行为。",
		"comparison": map[string]any{"mode": "branch_compare", "base_ref": base, "head_ref": head, "strategy": "direct"},
		"flows": []any{map[string]any{
			"title": "角色文档更新", "description": "更新行为并保留冲突说明。",
			"steps": []any{
				map[string]any{"label": "更新行为", "explanation": "修改行为描述。", "unit_id": "character-doc.behavior"},
				map[string]any{"label": "解决冲突", "explanation": "冲突说明没有代码变化。", "unit_id": "character-doc.resolve-conflict"},
			},
		}},
		"changes": []any{map[string]any{
			"file": "character.md", "purpose": "更新角色行为。", "implementation": "替换行为描述。",
			"units": []any{
				map[string]any{
					"id": "character-doc.behavior", "kind": "block", "symbol": "behavior", "title": "更新行为",
					"old_range": []int{3, 3}, "new_range": []int{3, 3},
					"meaning": "描述新行为。", "reason": "同步当前实现。", "impact": "读者看到新行为。",
				},
				map[string]any{
					"id": "character-doc.resolve-conflict", "kind": "block", "symbol": "resolve-conflict", "title": "解决冲突",
					"old_range": []int{5, 6}, "new_range": []int{5, 6},
					"meaning": "记录冲突结论。", "reason": "解释合并背景。", "impact": "没有实际差异。",
				},
			},
		}},
		"database_changes": []any{}, "config_changes": []any{},
		"api_changes": []any{map[string]any{"接口": "文档", "code_targets": []string{"character-doc.resolve-conflict"}}},
		"log_points":  []any{},
	}
	input, err := json.Marshal(walkthrough)
	if err != nil {
		t.Fatal(err)
	}
	output, err := GenerateData(context.Background(), repo, input)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Units []struct {
			ID string `json:"id"`
		} `json:"units"`
		Flows []struct {
			Steps []struct {
				UnitID string `json:"unit_id"`
			} `json:"steps"`
		} `json:"flows"`
		APIChanges []any `json:"api_changes"`
	}
	if err := json.Unmarshal(output, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Units) != 1 || !strings.HasPrefix(view.Units[0].ID, "git-change.") {
		t.Fatalf("context-only unit was not pruned: %#v", view.Units)
	}
	if len(view.Flows) != 1 || len(view.Flows[0].Steps) != 1 || view.Flows[0].Steps[0].UnitID != view.Units[0].ID {
		t.Fatalf("pruned flow reference was retained: %#v", view.Flows)
	}
	if len(view.APIChanges) != 0 {
		t.Fatalf("detail item backed only by a pruned unit was retained: %#v", view.APIChanges)
	}
}

func TestGenerateDataDerivesCompleteUnitsFromGit(t *testing.T) {
	if _, _, err := pythonCommand(); err != nil {
		t.Skip(err)
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	auditPath := filepath.Join(repo, "audit.md")
	otherPath := filepath.Join(repo, "other.md")
	baseLines := make([]string, 30)
	for index := range baseLines {
		baseLines[index] = fmt.Sprintf("line %02d", index+1)
	}
	writeTestFile(t, auditPath, strings.Join(baseLines, "\n")+"\n")
	writeTestFile(t, otherPath, "old other\n")
	runGit(t, repo, "add", "audit.md", "other.md")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")

	headLines := append([]string(nil), baseLines...)
	for _, line := range []int{3, 4, 5, 25} {
		headLines[line-1] = fmt.Sprintf("changed %02d", line)
	}
	writeTestFile(t, auditPath, strings.Join(headLines, "\n")+"\n")
	writeTestFile(t, otherPath, "new other\n")
	runGit(t, repo, "add", "audit.md", "other.md")
	runGit(t, repo, "commit", "-qm", "change two files")
	head := runGit(t, repo, "rev-parse", "HEAD")

	walkthrough := map[string]any{
		"version": 2, "title": "审计文档", "summary": "更新审计说明。",
		"comparison": map[string]any{"mode": "branch_compare", "base_ref": base, "head_ref": head, "strategy": "direct"},
		"flows": []any{map[string]any{
			"title": "模型链路", "description": "包含一个有效引用和一个错误引用。",
			"steps": []any{
				map[string]any{"label": "主变更", "explanation": "模型只覆盖了第一行。", "unit_id": "audit.primary"},
				map[string]any{"label": "虚构变更", "explanation": "应被忽略。", "unit_id": "audit.unknown"},
			},
		}},
		"changes": []any{map[string]any{
			"file": "audit.md", "purpose": "更新审计规则。", "implementation": "调整两处说明。",
			"units": []any{
				map[string]any{
					"id": "audit.primary", "kind": "block", "symbol": "audit", "title": "更新规则",
					"old_range": []int{3, 3}, "new_range": []int{3, 3},
					"meaning": "更新审计规则。", "reason": "同步行为。", "impact": "影响审计说明。",
				},
				map[string]any{
					"id": "audit.overlap", "kind": "block", "symbol": "audit-overlap", "title": "重复范围",
					"old_range": []int{3, 3}, "new_range": []int{3, 3},
					"meaning": "重复说明。", "reason": "模型重复。", "impact": "不应产生重叠。",
				},
			},
		}},
		"database_changes": []any{}, "config_changes": []any{}, "api_changes": []any{}, "log_points": []any{},
	}
	input, err := json.Marshal(walkthrough)
	if err != nil {
		t.Fatal(err)
	}
	output, err := GenerateData(context.Background(), repo, input)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Files []struct {
			Path string `json:"display_file"`
			Rows []struct {
				Kind   string `json:"kind"`
				Code   string `json:"code"`
				UnitID string `json:"unit_id"`
			} `json:"rows"`
		} `json:"files"`
		Units []struct {
			ID       string `json:"id"`
			OldRange []int  `json:"old_range"`
			NewRange []int  `json:"new_range"`
		} `json:"units"`
		Flows []struct {
			Steps []struct {
				UnitID string `json:"unit_id"`
			} `json:"steps"`
		} `json:"flows"`
	}
	if err := json.Unmarshal(output, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Files) != 2 {
		t.Fatalf("Git-discovered files = %d, want 2: %#v", len(view.Files), view.Files)
	}
	for _, file := range view.Files {
		for _, row := range file.Rows {
			if (row.Kind == "add" || row.Kind == "del") && strings.TrimSpace(row.Code) != "" && row.UnitID == "" {
				t.Fatalf("changed line is not owned by a Git-derived unit: file=%s row=%#v", file.Path, row)
			}
		}
	}
	primaryID := ""
	for _, unit := range view.Units {
		if unit.ID == "audit.primary" || unit.ID == "audit.overlap" {
			t.Fatalf("model unit ID leaked into Git-derived output: %#v", view.Units)
		}
		if len(unit.OldRange) == 2 && unit.OldRange[0] == 3 {
			primaryID = unit.ID
			if len(unit.OldRange) != 2 || unit.OldRange[0] != 3 || unit.OldRange[1] != 5 || len(unit.NewRange) != 2 || unit.NewRange[1] != 5 {
				t.Fatalf("model range was not expanded to the real Git hunk: %#v", unit)
			}
		}
	}
	if primaryID == "" {
		t.Fatalf("model semantics were not mapped to the first Git hunk: %#v", view.Units)
	}
	if len(view.Flows) != 1 || len(view.Flows[0].Steps) != 1 || view.Flows[0].Steps[0].UnitID != primaryID {
		t.Fatalf("unknown flow references were not discarded: %#v", view.Flows)
	}
}

func TestGenerateDataReadsSelectedCommitWithoutCheckout(t *testing.T) {
	if _, _, err := pythonCommand(); err != nil {
		t.Skip(err)
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	writeTestFile(t, filepath.Join(repo, "README.md"), "base\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-qm", "base")
	base := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "checkout", "-qb", "feature")
	path := filepath.Join(repo, "value.txt")
	writeTestFile(t, path, "feature snapshot\n")
	runGit(t, repo, "add", "value.txt")
	runGit(t, repo, "commit", "-qm", "feature value")
	head := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "checkout", "-q", "main")

	walkthrough, err := FallbackWalkthrough(map[string]any{
		"mode": "branch_compare", "base_ref": base, "head_ref": head, "strategy": "direct",
	}, "目标提交快照")
	if err != nil {
		t.Fatal(err)
	}
	output, err := GenerateData(context.Background(), repo, walkthrough)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Summary string `json:"summary"`
		Files   []struct {
			Rows []struct {
				Kind string `json:"kind"`
				Code string `json:"code"`
			} `json:"rows"`
		} `json:"files"`
	}
	if err := json.Unmarshal(output, &view); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("feature-only file unexpectedly exists in the current checkout: %v", statErr)
	}
	foundFeature := false
	for _, row := range view.Files[0].Rows {
		if row.Kind == "add" && row.Code == "feature snapshot" {
			foundFeature = true
		}
	}
	if !foundFeature || !strings.Contains(view.Summary, "Git 差异") {
		t.Fatalf("selected commit snapshot was not rendered: %#v", view)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stringValues(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
