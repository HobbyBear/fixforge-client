package codevisualizer

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var structDeclarationPattern = regexp.MustCompile(`\btype\s+([A-Za-z_][A-Za-z0-9_]*)\s+struct\b`)

// AttachCodeMap adds a deterministic, clickable architecture map to the
// validated visualization. The model may explain and order validated units,
// while node IDs, files, ranges and allowed edges remain owned by FixForge.
func AttachCodeMap(raw []byte) ([]byte, error) {
	var view map[string]any
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("decode visualization for code map: %w", err)
	}
	view["code_map"] = buildCodeMap(view)
	return json.Marshal(view)
}

func buildCodeMap(view map[string]any) map[string]any {
	files := map[int]map[string]any{}
	for fallback, value := range codeMapSlice(view["files"]) {
		file := codeMapObject(value)
		index := codeMapInt(file["index"], fallback)
		files[index] = file
	}

	nodes := make([]any, 0)
	nodeByID := map[string]map[string]any{}
	services := map[string]map[string]any{}
	for _, value := range codeMapSlice(view["units"]) {
		unit := codeMapObject(value)
		id := strings.TrimSpace(fmt.Sprint(unit["id"]))
		if id == "" {
			continue
		}
		fileIndex := codeMapInt(unit["file_index"], -1)
		file := files[fileIndex]
		path := codeMapFirst(file["new_file"], file["old_file"], unit["display_file"], file["display_file"])
		service, module := codeMapBoundary(path)
		line, endLine := codeMapRange(unit["new_range"])
		if line == 0 {
			line, endLine = codeMapRange(unit["old_range"])
		}
		node := map[string]any{
			"id":         id,
			"unit_id":    id,
			"kind":       strings.TrimSpace(fmt.Sprint(unit["kind"])),
			"label":      codeMapFirst(unit["symbol"], unit["title"], filepath.Base(path)),
			"title":      codeMapFirst(unit["title"], unit["symbol"], "代码变更"),
			"service":    service,
			"module":     module,
			"file":       path,
			"line":       line,
			"end_line":   endLine,
			"meaning":    strings.TrimSpace(fmt.Sprint(unit["meaning"])),
			"reason":     strings.TrimSpace(fmt.Sprint(unit["reason"])),
			"impact":     strings.TrimSpace(fmt.Sprint(unit["impact"])),
			"change":     "changed",
			"evidence":   "validated_git_unit",
			"file_index": fileIndex,
		}
		nodes = append(nodes, node)
		nodeByID[id] = node
		if _, exists := services[service]; !exists {
			services[service] = map[string]any{"id": codeMapSlug(service), "label": service, "summary": "本次功能链路中的代码边界"}
		}
	}

	edges := make([]any, 0)
	scenarios := make([]any, 0)
	usedEdges := map[string]bool{}
	for flowIndex, value := range codeMapSlice(view["flows"]) {
		flow := codeMapObject(value)
		scenarioID := fmt.Sprintf("flow-%d", flowIndex+1)
		startID := "start:" + scenarioID
		startNode := map[string]any{
			"id": startID, "kind": "entry", "label": "从这里开始", "title": codeMapFirst(flow["title"], "阅读入口"),
			"service": "阅读入口", "module": "本次改动", "meaning": codeMapFirst(flow["description"], view["summary"]),
			"change": "context", "evidence": "reading_entry",
		}
		nodes = append(nodes, startNode)
		nodeByID[startID] = startNode
		services["阅读入口"] = map[string]any{"id": "reading-entry", "label": "阅读入口", "summary": "当前场景的起点"}
		nodeIDs := []string{startID}
		edgeIDs := make([]string, 0)
		previous := startID
		for stepIndex, stepValue := range codeMapSlice(flow["steps"]) {
			step := codeMapObject(stepValue)
			target := strings.TrimSpace(fmt.Sprint(step["unit_id"]))
			if nodeByID[target] == nil {
				continue
			}
			nodeIDs = append(nodeIDs, target)
			edgeID := fmt.Sprintf("%s:step-%d", scenarioID, stepIndex+1)
			if usedEdges[edgeID] {
				continue
			}
			usedEdges[edgeID] = true
			label := codeMapFirst(step["label"], nodeByID[target]["title"])
			edge := map[string]any{
				"id": edgeID, "scenario_id": scenarioID, "source": previous, "target": target,
				"order": stepIndex + 1, "number": fmt.Sprint(stepIndex + 1), "label": label,
				"meaning":       codeMapFirst(step["explanation"], nodeByID[target]["meaning"]),
				"kind":          codeMapEdgeKind(label + " " + strings.TrimSpace(fmt.Sprint(step["explanation"]))),
				"evidence_kind": "source_backed_walkthrough", "confidence": "medium",
			}
			edges = append(edges, edge)
			edgeIDs = append(edgeIDs, edgeID)
			previous = target
		}
		if len(edgeIDs) > 0 {
			scenarios = append(scenarios, map[string]any{
				"id": scenarioID, "title": codeMapFirst(flow["title"], fmt.Sprintf("实现路径 %d", flowIndex+1)),
				"description": codeMapFirst(flow["description"], view["summary"]),
				"node_ids":    codeMapUnique(nodeIDs), "edge_ids": edgeIDs,
			})
		}
	}

	if len(scenarios) == 0 && len(nodes) > 0 {
		scenarioID := "changed-units"
		startID := "start:" + scenarioID
		start := map[string]any{
			"id": startID, "kind": "entry", "label": "从这里开始", "title": "改动覆盖路径",
			"service": "阅读入口", "module": "本次改动", "meaning": codeMapFirst(view["summary"], "按 Git 变更单元阅读。"),
			"change": "context", "evidence": "reading_entry",
		}
		nodes = append(nodes, start)
		services["阅读入口"] = map[string]any{"id": "reading-entry", "label": "阅读入口", "summary": "当前场景的起点"}
		nodeIDs := []string{startID}
		edgeIDs := []string{}
		previous := startID
		for index, value := range nodes[:len(nodes)-1] {
			node := codeMapObject(value)
			target := strings.TrimSpace(fmt.Sprint(node["id"]))
			edgeID := fmt.Sprintf("%s:step-%d", scenarioID, index+1)
			edges = append(edges, map[string]any{
				"id": edgeID, "scenario_id": scenarioID, "source": previous, "target": target,
				"order": index + 1, "number": fmt.Sprint(index + 1), "label": node["title"], "meaning": node["meaning"],
				"kind": "coverage", "evidence_kind": "git_coverage_order", "confidence": "low",
			})
			nodeIDs = append(nodeIDs, target)
			edgeIDs = append(edgeIDs, edgeID)
			previous = target
		}
		scenarios = append(scenarios, map[string]any{
			"id": scenarioID, "title": "改动覆盖路径", "description": "未发现足够证据形成业务调用链，当前仅按 Git 变更单元提供阅读入口。",
			"node_ids": nodeIDs, "edge_ids": edgeIDs,
		})
	}

	serviceList := make([]any, 0, len(services))
	serviceNames := make([]string, 0, len(services))
	for name := range services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	for _, name := range serviceNames {
		serviceList = append(serviceList, services[name])
	}

	return map[string]any{
		"version": 1,
		"scope": map[string]any{
			"title":        view["title"],
			"summary":      view["summary"],
			"comparison":   view["comparison"],
			"changed_only": true,
		},
		"services":        serviceList,
		"nodes":           nodes,
		"edges":           edges,
		"scenarios":       scenarios,
		"data_structures": codeMapStructures(files),
		"tables":          codeMapSlice(view["database_changes"]),
	}
}

func codeMapStructures(files map[int]map[string]any) []any {
	structures := make([]any, 0)
	seen := map[string]bool{}
	for fileIndex, file := range files {
		path := strings.TrimSpace(fmt.Sprint(file["display_file"]))
		for _, value := range codeMapSlice(file["rows"]) {
			row := codeMapObject(value)
			code := strings.TrimSpace(fmt.Sprint(row["code"]))
			match := structDeclarationPattern.FindStringSubmatch(code)
			if len(match) != 2 || seen[path+":"+match[1]] {
				continue
			}
			seen[path+":"+match[1]] = true
			line := codeMapInt(row["new_line"], codeMapInt(row["old_line"], 0))
			structures = append(structures, map[string]any{
				"id": codeMapSlug(path + ":" + match[1]), "name": match[1], "role": "本次改动涉及的数据结构",
				"file": path, "line": line, "file_index": fileIndex, "fields": []any{},
			})
		}
	}
	return structures
}

func codeMapBoundary(path string) (string, string) {
	path = filepath.ToSlash(path)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "未分类", "未分类"
	}
	service := parts[0]
	if len(parts) > 1 && (parts[0] == "apps" || parts[0] == "cmd" || parts[0] == "internal") {
		service = parts[0] + "/" + parts[1]
	} else if parts[0] == "web" {
		service = "Web Client"
	}
	moduleParts := parts
	if len(moduleParts) > 3 {
		moduleParts = moduleParts[:3]
	}
	if len(moduleParts) > 1 {
		moduleParts = moduleParts[:len(moduleParts)-1]
	}
	module := strings.Join(moduleParts, "/")
	if module == "" {
		module = service
	}
	return service, module
}

func codeMapEdgeKind(value string) string {
	value = strings.ToLower(value)
	switch {
	case strings.Contains(value, "redis") || strings.Contains(value, "topic") || strings.Contains(value, "publish") || strings.Contains(value, "subscribe") || strings.Contains(value, "消息"):
		return "async"
	case strings.Contains(value, "http") || strings.Contains(value, "rpc") || strings.Contains(value, "接口"):
		return "request"
	case strings.Contains(value, "sql") || strings.Contains(value, "database") || strings.Contains(value, "写表") || strings.Contains(value, "数据库"):
		return "storage"
	case strings.Contains(value, "stream") || strings.Contains(value, "流式") || strings.Contains(value, "websocket"):
		return "stream"
	default:
		return "call"
	}
}

func codeMapRange(value any) (int, int) {
	items := codeMapSlice(value)
	if len(items) < 2 {
		return 0, 0
	}
	return codeMapInt(items[0], 0), codeMapInt(items[1], 0)
}

func codeMapSlice(value any) []any {
	items, _ := value.([]any)
	return items
}

func codeMapObject(value any) map[string]any {
	item, _ := value.(map[string]any)
	if item == nil {
		return map[string]any{}
	}
	return item
}

func codeMapInt(value any, fallback int) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	default:
		return fallback
	}
}

func codeMapFirst(values ...any) string {
	for _, value := range values {
		if text := strings.TrimSpace(fmt.Sprint(value)); text != "" && text != "<nil>" {
			return text
		}
	}
	return ""
}

func codeMapSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = regexp.MustCompile(`[^a-z0-9\p{Han}]+`).ReplaceAllString(value, "-")
	return strings.Trim(value, "-")
}

func codeMapUnique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
