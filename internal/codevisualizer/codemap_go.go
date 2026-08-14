package codevisualizer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxCodeMapGoFiles = 1200
	maxCodeMapGoBytes = 12 << 20
)

type goMapDefinition struct {
	ID        string
	Path      string
	Package   string
	Name      string
	Symbol    string
	Receiver  string
	Start     int
	End       int
	Calls     []goMapCall
	Structs   []goMapStruct
	ChangedID string
}

type goMapCall struct {
	Name       string
	Package    string
	Line       int
	Confidence string
}

type goMapStruct struct {
	Name   string
	Line   int
	Fields []map[string]any
}

func attachGoRepositoryContext(ctx context.Context, repoRoot string, raw []byte) ([]byte, error) {
	var view map[string]any
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, err
	}
	codeMap := codeMapObject(view["code_map"])
	if len(codeMap) == 0 {
		return raw, nil
	}
	comparison := codeMapObject(view["comparison"])
	root, err := gitRepositoryRoot(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	paths, err := goMapSourcePaths(ctx, root, comparison, codeMapSlice(view["files"]))
	if err != nil || len(paths) == 0 {
		return raw, nil
	}
	definitions, err := parseGoMapDefinitions(ctx, root, comparison, paths)
	if err != nil || len(definitions) == 0 {
		return raw, nil
	}
	enrichCodeMapWithDefinitions(codeMap, definitions)
	view["code_map"] = codeMap
	return json.Marshal(view)
}

func goMapSourcePaths(ctx context.Context, root string, comparison map[string]any, files []any) ([]string, error) {
	prefixes := map[string]bool{}
	for _, value := range files {
		file := codeMapObject(value)
		path := codeMapFirst(file["new_file"], file["old_file"], file["display_file"])
		parts := strings.Split(filepath.ToSlash(path), "/")
		if len(parts) >= 2 && parts[0] == "apps" {
			prefixes[parts[0]+"/"+parts[1]+"/"] = true
		} else if len(parts) > 0 && parts[0] != "" {
			prefixes[parts[0]+"/"] = true
		}
	}
	mode := strings.TrimSpace(fmt.Sprint(comparison["mode"]))
	var output string
	var err error
	if mode == "working_tree" {
		output, err = gitOutput(ctx, root, "ls-files")
	} else {
		head := codeMapFirst(comparison["head_sha"], comparison["head_ref"])
		output, err = gitOutput(ctx, root, "ls-tree", "-r", "--name-only", head)
	}
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0)
	for _, path := range strings.Split(output, "\n") {
		path = filepath.ToSlash(strings.TrimSpace(path))
		if path == "" || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		if len(prefixes) > 0 {
			matched := false
			for prefix := range prefixes {
				if strings.HasPrefix(path, prefix) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		paths = append(paths, path)
		if len(paths) == maxCodeMapGoFiles {
			break
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func parseGoMapDefinitions(ctx context.Context, root string, comparison map[string]any, paths []string) ([]*goMapDefinition, error) {
	fset := token.NewFileSet()
	definitions := make([]*goMapDefinition, 0)
	totalBytes := 0
	for _, path := range paths {
		source, err := goMapSource(ctx, root, comparison, path)
		if err != nil {
			continue
		}
		totalBytes += len(source)
		if totalBytes > maxCodeMapGoBytes {
			break
		}
		parsed, err := parser.ParseFile(fset, path, source, 0)
		if err != nil {
			continue
		}
		imports := map[string]string{}
		for _, item := range parsed.Imports {
			importPath := strings.Trim(item.Path.Value, `"`)
			alias := filepath.Base(importPath)
			if item.Name != nil && item.Name.Name != "_" && item.Name.Name != "." {
				alias = item.Name.Name
			}
			imports[alias] = filepath.Base(importPath)
		}
		for _, decl := range parsed.Decls {
			switch typed := decl.(type) {
			case *ast.FuncDecl:
				receiver := goMapReceiver(typed)
				symbol := parsed.Name.Name + "." + typed.Name.Name
				if receiver != "" {
					symbol = parsed.Name.Name + "." + receiver + "." + typed.Name.Name
				}
				definition := &goMapDefinition{
					ID: goMapID(path + ":" + symbol), Path: path, Package: parsed.Name.Name, Name: typed.Name.Name,
					Symbol: symbol, Receiver: receiver, Start: fset.Position(typed.Pos()).Line, End: fset.Position(typed.End()).Line,
				}
				if typed.Body != nil {
					ast.Inspect(typed.Body, func(node ast.Node) bool {
						call, ok := node.(*ast.CallExpr)
						if !ok {
							return true
						}
						if ref, ok := goMapCallRef(fset, call, imports); ok {
							definition.Calls = append(definition.Calls, ref)
						}
						return true
					})
				}
				definitions = append(definitions, definition)
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					structure, ok := typeSpec.Type.(*ast.StructType)
					if !ok {
						continue
					}
					fields := make([]map[string]any, 0)
					for _, field := range structure.Fields.List {
						var buffer bytes.Buffer
						_ = printer.Fprint(&buffer, fset, field.Type)
						fieldType := buffer.String()
						for _, name := range field.Names {
							fields = append(fields, map[string]any{"name": name.Name, "type": fieldType})
						}
					}
					definitions = append(definitions, &goMapDefinition{
						ID: goMapID(path + ":type:" + typeSpec.Name.Name), Path: path, Package: parsed.Name.Name,
						Name: typeSpec.Name.Name, Symbol: parsed.Name.Name + "." + typeSpec.Name.Name,
						Start: fset.Position(typeSpec.Pos()).Line, End: fset.Position(typeSpec.End()).Line,
						Structs: []goMapStruct{{Name: typeSpec.Name.Name, Line: fset.Position(typeSpec.Pos()).Line, Fields: fields}},
					})
				}
			}
		}
	}
	return definitions, nil
}

func enrichCodeMapWithDefinitions(codeMap map[string]any, definitions []*goMapDefinition) {
	nodes := codeMapSlice(codeMap["nodes"])
	nodeByID := map[string]map[string]any{}
	for _, value := range nodes {
		node := codeMapObject(value)
		nodeByID[codeMapFirst(node["id"])] = node
	}
	definitionsByName := map[string][]*goMapDefinition{}
	definitionsByPackageName := map[string][]*goMapDefinition{}
	definitionByID := map[string]*goMapDefinition{}
	for _, definition := range definitions {
		definitionsByName[definition.Name] = append(definitionsByName[definition.Name], definition)
		definitionsByPackageName[definition.Package+"."+definition.Name] = append(definitionsByPackageName[definition.Package+"."+definition.Name], definition)
		definitionByID[definition.ID] = definition
	}
	seeds := map[string]bool{}
	for _, definition := range definitions {
		for id, node := range nodeByID {
			if codeMapFirst(node["file"]) != definition.Path {
				continue
			}
			line := codeMapInt(node["line"], 0)
			label := codeMapFirst(node["label"])
			if (line >= definition.Start && line <= definition.End) || strings.Contains(label, definition.Name) {
				definition.ChangedID = id
				seeds[definition.ID] = true
				break
			}
		}
	}
	type relation struct {
		from, to   string
		line       int
		confidence string
	}
	relations := make([]relation, 0)
	neighbors := map[string][]string{}
	for _, definition := range definitions {
		if len(definition.Structs) > 0 {
			continue
		}
		for _, call := range definition.Calls {
			candidates := definitionsByName[call.Name]
			if call.Package != "" {
				candidates = definitionsByPackageName[call.Package+"."+call.Name]
			}
			if len(candidates) != 1 || len(candidates[0].Structs) > 0 {
				continue
			}
			target := candidates[0]
			relations = append(relations, relation{from: definition.ID, to: target.ID, line: call.Line, confidence: call.Confidence})
			neighbors[definition.ID] = append(neighbors[definition.ID], target.ID)
			neighbors[target.ID] = append(neighbors[target.ID], definition.ID)
		}
	}
	included := map[string]bool{}
	frontier := make([]string, 0, len(seeds))
	for id := range seeds {
		included[id] = true
		frontier = append(frontier, id)
	}
	for depth := 0; depth < 2; depth++ {
		next := []string{}
		for _, id := range frontier {
			for _, neighbor := range neighbors[id] {
				if !included[neighbor] {
					included[neighbor] = true
					next = append(next, neighbor)
				}
			}
		}
		frontier = next
	}
	for id := range included {
		definition := definitionByID[id]
		if definition == nil || definition.ChangedID != "" || len(definition.Structs) > 0 {
			continue
		}
		service, module := codeMapBoundary(definition.Path)
		node := map[string]any{
			"id": id, "kind": "function", "label": definition.Symbol, "title": definition.Name,
			"service": service, "module": module, "file": definition.Path, "line": definition.Start, "end_line": definition.End,
			"meaning": "理解本次改动所需的未修改上下文方法。", "reason": "它与本次改动函数存在直接 Go AST 调用证据。",
			"impact": "本节点不属于本次改动。", "change": "context", "evidence": "go_ast_definition", "file_index": -1,
		}
		nodes = append(nodes, node)
		nodeByID[id] = node
	}
	edges := codeMapSlice(codeMap["edges"])
	for _, item := range relations {
		if !included[item.from] || !included[item.to] {
			continue
		}
		source := item.from
		target := item.to
		if definitionByID[source].ChangedID != "" {
			source = definitionByID[source].ChangedID
		}
		if definitionByID[target].ChangedID != "" {
			target = definitionByID[target].ChangedID
		}
		if source == target || nodeByID[source] == nil || nodeByID[target] == nil {
			continue
		}
		edges = append(edges, map[string]any{
			"id": "ast:" + goMapID(source+":"+target+fmt.Sprint(item.line)), "scenario_id": "context",
			"source": source, "target": target, "order": 0, "number": "", "label": "调用",
			"meaning": fmt.Sprintf("%s:%d 的直接函数调用。", definitionByID[item.from].Path, item.line),
			"kind":    "call", "evidence_kind": "go_ast_call", "confidence": item.confidence,
			"file": definitionByID[item.from].Path, "line": item.line,
		})
	}
	structures := codeMapSlice(codeMap["data_structures"])
	seenStructures := map[string]bool{}
	includedPaths := map[string]bool{}
	for id := range included {
		if definitionByID[id] != nil {
			includedPaths[definitionByID[id].Path] = true
		}
	}
	for _, node := range nodeByID {
		if codeMapFirst(node["change"]) == "changed" {
			includedPaths[codeMapFirst(node["file"])] = true
		}
	}
	for _, value := range structures {
		seenStructures[codeMapFirst(codeMapObject(value)["file"])+":"+codeMapFirst(codeMapObject(value)["name"])] = true
	}
	for _, definition := range definitions {
		if !includedPaths[definition.Path] {
			continue
		}
		for _, structure := range definition.Structs {
			key := definition.Path + ":" + structure.Name
			if seenStructures[key] {
				continue
			}
			seenStructures[key] = true
			structures = append(structures, map[string]any{
				"id": definition.ID, "name": structure.Name, "role": "调用链相关数据结构",
				"file": definition.Path, "line": structure.Line, "fields": structure.Fields,
			})
		}
	}
	codeMap["structure_relations"] = buildStructureRelations(structures)
	services := map[string]map[string]any{}
	for _, value := range nodes {
		node := codeMapObject(value)
		service := codeMapFirst(node["service"], "未分类")
		services[service] = map[string]any{"id": codeMapSlug(service), "label": service, "summary": "本次功能链路中的代码边界"}
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
	codeMap["nodes"] = nodes
	codeMap["edges"] = edges
	codeMap["services"] = serviceList
	codeMap["data_structures"] = structures
}

func buildStructureRelations(structures []any) []any {
	idsByName := map[string]string{}
	for _, value := range structures {
		item := codeMapObject(value)
		name := codeMapFirst(item["name"])
		id := codeMapFirst(item["id"])
		if name != "" && id != "" {
			idsByName[name] = id
		}
	}
	relations := make([]any, 0)
	seen := map[string]bool{}
	for _, value := range structures {
		item := codeMapObject(value)
		source := codeMapFirst(item["id"])
		name := codeMapFirst(item["name"])
		for _, fieldValue := range codeMapSlice(item["fields"]) {
			field := codeMapObject(fieldValue)
			fieldType := codeMapFirst(field["type"])
			for targetName, target := range idsByName {
				if targetName == name || target == source || !strings.Contains(fieldType, targetName) {
					continue
				}
				key := source + "->" + target
				if seen[key] {
					continue
				}
				seen[key] = true
				relations = append(relations, map[string]any{
					"id": "structure:" + codeMapSlug(key), "source": source, "target": target,
					"label": "字段引用", "kind": "contains", "meaning": fmt.Sprintf("%s 的字段类型引用 %s。", name, targetName),
					"evidence_kind": "ast_definition", "confidence": "high",
				})
			}
		}
	}
	return relations
}

func goMapSource(ctx context.Context, root string, comparison map[string]any, path string) ([]byte, error) {
	mode := strings.TrimSpace(fmt.Sprint(comparison["mode"]))
	if mode == "working_tree" {
		changed := map[string]bool{}
		for _, value := range codeMapSlice(comparison["changed_paths"]) {
			changed[filepath.ToSlash(strings.TrimSpace(fmt.Sprint(value)))] = true
		}
		if changed[path] {
			return os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		}
		base := codeMapFirst(comparison["base_sha"], comparison["base_ref"])
		value, err := gitOutput(ctx, root, "show", base+":"+path)
		return []byte(value), err
	}
	head := codeMapFirst(comparison["head_sha"], comparison["head_ref"])
	value, err := gitOutput(ctx, root, "show", head+":"+path)
	return []byte(value), err
}

func goMapReceiver(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return ""
	}
	value := decl.Recv.List[0].Type
	if star, ok := value.(*ast.StarExpr); ok {
		value = star.X
	}
	if name, ok := value.(*ast.Ident); ok {
		return name.Name
	}
	return ""
}

func goMapCallRef(fset *token.FileSet, call *ast.CallExpr, imports map[string]string) (goMapCall, bool) {
	line := fset.Position(call.Pos()).Line
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return goMapCall{Name: fun.Name, Line: line, Confidence: "high"}, true
	case *ast.SelectorExpr:
		if qualifier, ok := fun.X.(*ast.Ident); ok {
			if pkg := imports[qualifier.Name]; pkg != "" {
				return goMapCall{Name: fun.Sel.Name, Package: pkg, Line: line, Confidence: "high"}, true
			}
		}
		return goMapCall{Name: fun.Sel.Name, Line: line, Confidence: "medium"}, true
	default:
		return goMapCall{}, false
	}
}

func goMapID(value string) string {
	sum := sha1.Sum([]byte(value))
	return "ctx-" + hex.EncodeToString(sum[:8])
}
