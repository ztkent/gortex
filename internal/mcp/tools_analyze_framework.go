package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// handleAnalyzeRoutes surfaces the EdgeHandlesRoute graph layer:
// every (handler symbol, route contract) pair that the contracts
// pipeline detected as a real network route. Answers "which handler
// serves /v1/users/:id?" without making the agent walk EdgeProvides
// and filter by Meta["type"]="http" by hand.
//
// Filters:
//   - method: HTTP verb (GET/POST/...) or gRPC method (case-insensitive)
//   - path:   substring match on the contract's path / topic / channel
//   - type:   contract type — http / grpc / graphql / topic / ws.
//             Named `type` (not `kind`) because the analyze dispatcher
//             reserves `kind` for the analyzer name itself.
func (s *Server) handleAnalyzeRoutes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	methodFilter := strings.ToUpper(strings.TrimSpace(stringArg(args, "method")))
	pathFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "path")))
	kindFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "type")))

	type routeRow struct {
		Handler string `json:"handler"`
		Route   string `json:"route"`
		Method  string `json:"method,omitempty"`
		Path    string `json:"path,omitempty"`
		Kind    string `json:"kind"`
		File    string `json:"file"`
		Line    int    `json:"line"`
	}
	var rows []*routeRow
	for _, e := range s.graph.AllEdges() {
		if e.Kind != graph.EdgeHandlesRoute {
			continue
		}
		contractNode := s.graph.GetNode(e.To)
		if contractNode == nil {
			continue
		}
		ctype, _ := contractNode.Meta["type"].(string)
		if kindFilter != "" && ctype != kindFilter {
			continue
		}
		method, path := routeMethodAndPath(contractNode)
		if methodFilter != "" && strings.ToUpper(method) != methodFilter {
			continue
		}
		if pathFilter != "" && !strings.Contains(strings.ToLower(path), pathFilter) {
			continue
		}
		rows = append(rows, &routeRow{
			Handler: e.From,
			Route:   e.To,
			Method:  method,
			Path:    path,
			Kind:    ctype,
			File:    e.FilePath,
			Line:    e.Line,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Kind != rows[j].Kind {
			return rows[i].Kind < rows[j].Kind
		}
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		if rows[i].Method != rows[j].Method {
			return rows[i].Method < rows[j].Method
		}
		return rows[i].Handler < rows[j].Handler
	})
	if s.isGCX(ctx, req) {
		items := make([]routeItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, routeItem(*r))
		}
		return gcxResponse(encodeAnalyze("routes", items))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s %-6s %s  →  %s  (%s:%d)\n", r.Kind, r.Method, r.Path, r.Handler, r.File, r.Line)
		}
		if len(rows) == 0 {
			b.WriteString("no routes\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"routes": rows,
		"total":  len(rows),
	})
}

// routeMethodAndPath pulls the most useful pair of fields out of a
// KindContract node's Meta. HTTP and WS routes use Meta["method"] +
// Meta["path"]; gRPC uses Meta["service"] + Meta["method"]; topic uses
// Meta["topic"]; GraphQL uses Meta["operation"] + Meta["field"].
func routeMethodAndPath(n *graph.Node) (string, string) {
	if n == nil {
		return "", ""
	}
	meta := n.Meta
	method, _ := meta["method"].(string)
	path, _ := meta["path"].(string)
	if path != "" || method != "" {
		return method, path
	}
	if topic, ok := meta["topic"].(string); ok && topic != "" {
		return "", topic
	}
	if op, ok := meta["operation"].(string); ok && op != "" {
		field, _ := meta["field"].(string)
		return op, field
	}
	if svc, ok := meta["service"].(string); ok && svc != "" {
		return method, svc
	}
	return method, path
}

// handleAnalyzeModels surfaces the EdgeModelsTable graph layer: every
// model class that maps to a database table. Useful for "which model
// owns the orders table?" and "which tables does this codebase
// persist?" queries.
//
// Filters:
//   - orm:    orm flavour (gorm / sqlalchemy / django / activerecord / jpa / typeorm)
//   - table:  substring match on the table name
//   - model:  substring match on the model class name
func (s *Server) handleAnalyzeModels(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	ormFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "orm")))
	tableFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "table")))
	modelFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "model")))

	type modelRow struct {
		Model      string `json:"model"`
		Table      string `json:"table"`
		ORM        string `json:"orm"`
		Derivation string `json:"derivation,omitempty"`
		File       string `json:"file"`
		Line       int    `json:"line"`
	}
	var rows []*modelRow
	for _, e := range s.graph.AllEdges() {
		if e.Kind != graph.EdgeModelsTable {
			continue
		}
		modelNode := s.graph.GetNode(e.From)
		if modelNode == nil {
			continue
		}
		orm, _ := e.Meta["orm"].(string)
		if ormFilter != "" && strings.ToLower(orm) != ormFilter {
			continue
		}
		tableName, _ := e.Meta["table_name"].(string)
		if tableName == "" {
			tableNode := s.graph.GetNode(e.To)
			if tableNode != nil {
				tableName = tableNode.Name
			}
		}
		if tableFilter != "" && !strings.Contains(strings.ToLower(tableName), tableFilter) {
			continue
		}
		if modelFilter != "" && !strings.Contains(strings.ToLower(modelNode.Name), modelFilter) {
			continue
		}
		derivation, _ := e.Meta["derivation"].(string)
		rows = append(rows, &modelRow{
			Model:      modelNode.ID,
			Table:      tableName,
			ORM:        orm,
			Derivation: derivation,
			File:       e.FilePath,
			Line:       e.Line,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ORM != rows[j].ORM {
			return rows[i].ORM < rows[j].ORM
		}
		if rows[i].Table != rows[j].Table {
			return rows[i].Table < rows[j].Table
		}
		return rows[i].Model < rows[j].Model
	})
	if s.isGCX(ctx, req) {
		items := make([]modelItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, modelItem(*r))
		}
		return gcxResponse(encodeAnalyze("models", items))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s %-12s %s  →  %s  (%s:%d)\n", r.ORM, r.Derivation, r.Model, r.Table, r.File, r.Line)
		}
		if len(rows) == 0 {
			b.WriteString("no model→table edges\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"models": rows,
		"total":  len(rows),
	})
}

// handleAnalyzeComponents surfaces the EdgeRendersChild graph layer:
// the parent → child component dependency tree. Two views:
//
//   - rollup (no `id`): per-component fan-in / fan-out summary so the
//     agent sees which components are central (high fan-in =
//     widely-rendered shared component; high fan-out = composite
//     view).
//   - per-component (id=<symbol>): list of every child the component
//     renders with their resolved targets.
func (s *Server) handleAnalyzeComponents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	idFilter := strings.TrimSpace(stringArg(args, "id"))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))

	if idFilter != "" {
		return s.componentsForOne(ctx, req, idFilter)
	}
	return s.componentsRollup(ctx, req, nameFilter)
}

// componentsRollup groups EdgeRendersChild edges per parent + per
// child to produce a fan-in / fan-out leaderboard.
func (s *Server) componentsRollup(ctx context.Context, req mcp.CallToolRequest, nameFilter string) (*mcp.CallToolResult, error) {
	type compRow struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		FanIn   int    `json:"fan_in"`
		FanOut  int    `json:"fan_out"`
		File    string `json:"file,omitempty"`
	}
	stats := map[string]*compRow{}
	get := func(id string) *compRow {
		row, ok := stats[id]
		if ok {
			return row
		}
		name := id
		file := ""
		if n := s.graph.GetNode(id); n != nil {
			name = n.Name
			file = n.FilePath
		} else if i := strings.LastIndex(id, "::"); i >= 0 {
			name = id[i+2:]
		}
		row = &compRow{ID: id, Name: name, File: file}
		stats[id] = row
		return row
	}
	for _, e := range s.graph.AllEdges() {
		if e.Kind != graph.EdgeRendersChild {
			continue
		}
		parent := get(e.From)
		parent.FanOut++
		// Skip the child if it never resolved to a real node — leaving
		// it in the fan-in count would inflate uses-of-unresolved
		// references and pollute the rollup. Resolved targets show up
		// without the "unresolved::" prefix.
		if !strings.HasPrefix(e.To, "unresolved::") {
			child := get(e.To)
			child.FanIn++
		}
	}
	rows := make([]*compRow, 0, len(stats))
	for _, r := range stats {
		if nameFilter != "" && !strings.Contains(strings.ToLower(r.Name), nameFilter) {
			continue
		}
		if r.FanIn == 0 && r.FanOut == 0 {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		ai := rows[i].FanIn + rows[i].FanOut
		aj := rows[j].FanIn + rows[j].FanOut
		if ai != aj {
			return ai > aj
		}
		return rows[i].Name < rows[j].Name
	})
	if s.isGCX(ctx, req) {
		items := make([]componentRollupItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, componentRollupItem(*r))
		}
		return gcxResponse(encodeAnalyze("components", items))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d in / %-3d out  %-30s  (%s)\n", r.FanIn, r.FanOut, r.Name, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no renders_child edges\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"components": rows,
		"total":      len(rows),
	})
}

// componentsForOne returns every child component a single parent
// renders, with the resolved-target indicator per row.
func (s *Server) componentsForOne(ctx context.Context, req mcp.CallToolRequest, parentID string) (*mcp.CallToolResult, error) {
	type childRow struct {
		To       string `json:"to"`
		Name     string `json:"name"`
		Resolved bool   `json:"resolved"`
		File     string `json:"file,omitempty"`
		Line     int    `json:"line"`
	}
	var rows []*childRow
	for _, e := range s.graph.GetOutEdges(parentID) {
		if e.Kind != graph.EdgeRendersChild {
			continue
		}
		name, _ := e.Meta["child_name"].(string)
		if name == "" {
			if strings.HasPrefix(e.To, "unresolved::") {
				name = strings.TrimPrefix(e.To, "unresolved::")
			} else if n := s.graph.GetNode(e.To); n != nil {
				name = n.Name
			}
		}
		rows = append(rows, &childRow{
			To:       e.To,
			Name:     name,
			Resolved: !strings.HasPrefix(e.To, "unresolved::"),
			File:     e.FilePath,
			Line:     e.Line,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Line != rows[j].Line {
			return rows[i].Line < rows[j].Line
		}
		return rows[i].Name < rows[j].Name
	})
	if s.isGCX(ctx, req) {
		items := make([]componentChildItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, componentChildItem{
				To:       r.To,
				Name:     r.Name,
				Resolved: boolStr(r.Resolved),
				File:     r.File,
				Line:     r.Line,
			})
		}
		return gcxResponse(encodeAnalyze("components.children", items))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			marker := "✓"
			if !r.Resolved {
				marker = "?"
			}
			fmt.Fprintf(&b, "%s %s  (%s:%d)\n", marker, r.Name, r.File, r.Line)
		}
		if len(rows) == 0 {
			b.WriteString("no children\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"parent":   parentID,
		"children": rows,
		"total":    len(rows),
	})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// routeItem is the GCX1 row layout for the routes analyzer.
type routeItem struct {
	Handler string `gcx:"handler"`
	Route   string `gcx:"route"`
	Method  string `gcx:"method"`
	Path    string `gcx:"path"`
	Kind    string `gcx:"kind"`
	File    string `gcx:"file"`
	Line    int    `gcx:"line"`
}

// modelItem is the GCX1 row layout for the models analyzer.
type modelItem struct {
	Model      string `gcx:"model"`
	Table      string `gcx:"table"`
	ORM        string `gcx:"orm"`
	Derivation string `gcx:"derivation"`
	File       string `gcx:"file"`
	Line       int    `gcx:"line"`
}

// componentRollupItem is the GCX1 row layout for the components rollup.
type componentRollupItem struct {
	ID     string `gcx:"id"`
	Name   string `gcx:"name"`
	FanIn  int    `gcx:"fan_in"`
	FanOut int    `gcx:"fan_out"`
	File   string `gcx:"file"`
}

// componentChildItem is the GCX1 row layout for per-component children.
type componentChildItem struct {
	To       string `gcx:"to"`
	Name     string `gcx:"name"`
	Resolved string `gcx:"resolved"`
	File     string `gcx:"file"`
	Line     int    `gcx:"line"`
}
