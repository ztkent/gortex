package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeFramework(t *testing.T, srv *Server, kind string, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = kind
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %+v", res.Content)
	}
	textBlock := res.Content[0].(mcplib.TextContent)
	var out map[string]any
	if err := json.Unmarshal([]byte(textBlock.Text), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, textBlock.Text)
	}
	return out
}

func addContractNode(g *graph.Graph, id, ctype string, meta map[string]any) {
	full := map[string]any{"type": ctype, "role": "provider"}
	for k, v := range meta {
		full[k] = v
	}
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindContract, Name: id, Language: "contract", Meta: full,
	})
}

func addHandlesRouteEdge(g *graph.Graph, from, to, file string, line int) {
	g.AddEdge(&graph.Edge{
		From: from, To: to, Kind: graph.EdgeHandlesRoute,
		FilePath: file, Line: line,
	})
}

func TestAnalyzeRoutes_BasicHTTPListing(t *testing.T) {
	srv, _ := setupTestServer(t)
	addContractNode(srv.graph, "http::GET::/v1/users", "http", map[string]any{"method": "GET", "path": "/v1/users"})
	addContractNode(srv.graph, "http::POST::/v1/users", "http", map[string]any{"method": "POST", "path": "/v1/users"})
	addHandlesRouteEdge(srv.graph, "handlers/users.go::List", "http::GET::/v1/users", "handlers/users.go", 12)
	addHandlesRouteEdge(srv.graph, "handlers/users.go::Create", "http::POST::/v1/users", "handlers/users.go", 24)

	out := callAnalyzeFramework(t, srv, "routes", map[string]any{})
	rows, _ := out["routes"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 route rows, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["method"] != "GET" || first["path"] != "/v1/users" {
		t.Errorf("expected GET /v1/users first, got %v %v", first["method"], first["path"])
	}
}

func TestAnalyzeRoutes_FilterByMethod(t *testing.T) {
	srv, _ := setupTestServer(t)
	addContractNode(srv.graph, "http::GET::/x", "http", map[string]any{"method": "GET", "path": "/x"})
	addContractNode(srv.graph, "http::POST::/y", "http", map[string]any{"method": "POST", "path": "/y"})
	addHandlesRouteEdge(srv.graph, "h.go::A", "http::GET::/x", "h.go", 1)
	addHandlesRouteEdge(srv.graph, "h.go::B", "http::POST::/y", "h.go", 5)

	out := callAnalyzeFramework(t, srv, "routes", map[string]any{"method": "POST"})
	rows, _ := out["routes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 route after method=POST filter, got %d", len(rows))
	}
}

func TestAnalyzeRoutes_FilterByKind(t *testing.T) {
	srv, _ := setupTestServer(t)
	addContractNode(srv.graph, "http::GET::/x", "http", map[string]any{"method": "GET", "path": "/x"})
	addContractNode(srv.graph, "grpc::Foo/Bar", "grpc", map[string]any{"service": "Foo", "method": "Bar"})
	addHandlesRouteEdge(srv.graph, "h.go::A", "http::GET::/x", "h.go", 1)
	addHandlesRouteEdge(srv.graph, "g.go::B", "grpc::Foo/Bar", "g.go", 1)

	out := callAnalyzeFramework(t, srv, "routes", map[string]any{"type": "grpc"})
	rows, _ := out["routes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 grpc route, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["kind"] != "grpc" {
		t.Errorf("filter leaked: %v", first["kind"])
	}
}

func addModelTableEdge(g *graph.Graph, from, to, orm, table, derivation string) {
	g.AddNode(&graph.Node{ID: to, Kind: graph.KindTable, Name: table, Language: "go", Meta: map[string]any{"dialect": "orm"}})
	g.AddEdge(&graph.Edge{
		From: from, To: to, Kind: graph.EdgeModelsTable,
		Meta: map[string]any{"orm": orm, "table_name": table, "derivation": derivation},
	})
}

func TestAnalyzeModels_GroupsByORM(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.graph.AddNode(&graph.Node{ID: "model.go::User", Kind: graph.KindType, Name: "User", Language: "go"})
	srv.graph.AddNode(&graph.Node{ID: "models/order.py::Order", Kind: graph.KindType, Name: "Order", Language: "python"})
	addModelTableEdge(srv.graph, "model.go::User", "db::orm::users", "gorm", "users", "convention")
	addModelTableEdge(srv.graph, "models/order.py::Order", "db::orm::orders", "sqlalchemy", "orders", "override")

	out := callAnalyzeFramework(t, srv, "models", map[string]any{})
	rows, _ := out["models"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 models, got %d", len(rows))
	}
}

func TestAnalyzeModels_FilterByORM(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.graph.AddNode(&graph.Node{ID: "a.go::A", Kind: graph.KindType, Name: "A", Language: "go"})
	srv.graph.AddNode(&graph.Node{ID: "b.py::B", Kind: graph.KindType, Name: "B", Language: "python"})
	addModelTableEdge(srv.graph, "a.go::A", "db::orm::as", "gorm", "as", "convention")
	addModelTableEdge(srv.graph, "b.py::B", "db::orm::bs", "sqlalchemy", "bs", "convention")

	out := callAnalyzeFramework(t, srv, "models", map[string]any{"orm": "sqlalchemy"})
	rows, _ := out["models"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 sqlalchemy model, got %d", len(rows))
	}
}

func TestAnalyzeModels_FilterByTableSubstring(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.graph.AddNode(&graph.Node{ID: "a.go::A", Kind: graph.KindType, Name: "A", Language: "go"})
	srv.graph.AddNode(&graph.Node{ID: "b.go::B", Kind: graph.KindType, Name: "B", Language: "go"})
	addModelTableEdge(srv.graph, "a.go::A", "db::orm::orders", "gorm", "orders", "convention")
	addModelTableEdge(srv.graph, "b.go::B", "db::orm::users", "gorm", "users", "convention")

	out := callAnalyzeFramework(t, srv, "models", map[string]any{"table": "ord"})
	rows, _ := out["models"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 model after table=ord, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["table"] != "orders" {
		t.Errorf("filter leaked: %v", first["table"])
	}
}

func addRendersChildEdge(g *graph.Graph, from, to, name string, line int) {
	g.AddEdge(&graph.Edge{
		From: from, To: to, Kind: graph.EdgeRendersChild,
		Line: line,
		Meta: map[string]any{"child_name": name},
	})
}

func TestAnalyzeComponents_RollupSortsByTotal(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.graph.AddNode(&graph.Node{ID: "src/Page.tsx::Page", Kind: graph.KindFunction, Name: "Page", Language: "typescript"})
	srv.graph.AddNode(&graph.Node{ID: "src/Card.tsx::Card", Kind: graph.KindFunction, Name: "Card", Language: "typescript"})
	srv.graph.AddNode(&graph.Node{ID: "src/Button.tsx::Button", Kind: graph.KindFunction, Name: "Button", Language: "typescript"})
	addRendersChildEdge(srv.graph, "src/Page.tsx::Page", "src/Card.tsx::Card", "Card", 5)
	addRendersChildEdge(srv.graph, "src/Page.tsx::Page", "src/Button.tsx::Button", "Button", 6)
	addRendersChildEdge(srv.graph, "src/Card.tsx::Card", "src/Button.tsx::Button", "Button", 4)

	out := callAnalyzeFramework(t, srv, "components", map[string]any{})
	rows, _ := out["components"].([]any)
	if len(rows) < 3 {
		t.Fatalf("expected ≥3 component rows, got %d", len(rows))
	}
	// Button has fan-in 2 + fan-out 0 = 2; Card has 1 + 1 = 2; Page 0 + 2 = 2.
	// Each row contributes its own sum, sort ties break by name. Just
	// verify Button is present with correct fan_in.
	for _, r := range rows {
		row := r.(map[string]any)
		if row["name"] == "Button" {
			if int(row["fan_in"].(float64)) != 2 {
				t.Errorf("Button fan_in=%v, expected 2", row["fan_in"])
			}
			return
		}
	}
	t.Fatalf("expected Button in rollup")
}

func TestAnalyzeComponents_PerComponentChildren(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.graph.AddNode(&graph.Node{ID: "src/Page.tsx::Page", Kind: graph.KindFunction, Name: "Page", Language: "typescript"})
	addRendersChildEdge(srv.graph, "src/Page.tsx::Page", "src/Card.tsx::Card", "Card", 5)
	addRendersChildEdge(srv.graph, "src/Page.tsx::Page", "unresolved::Header", "Header", 7)

	out := callAnalyzeFramework(t, srv, "components", map[string]any{
		"id": "src/Page.tsx::Page",
	})
	if out["parent"] != "src/Page.tsx::Page" {
		t.Errorf("expected parent=Page, got %v", out["parent"])
	}
	rows, _ := out["children"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 children, got %d", len(rows))
	}
	for _, r := range rows {
		row := r.(map[string]any)
		if row["name"] == "Header" && row["resolved"] != false {
			t.Errorf("Header is unresolved::, expected resolved=false, got %v", row["resolved"])
		}
		if row["name"] == "Card" && row["resolved"] != true {
			t.Errorf("Card has real target, expected resolved=true")
		}
	}
}

func TestAnalyzeComponents_EmptyOnNoEdges(t *testing.T) {
	srv, _ := setupTestServer(t)
	out := callAnalyzeFramework(t, srv, "components", map[string]any{})
	rows, _ := out["components"].([]any)
	if len(rows) != 0 {
		t.Fatalf("expected 0 components, got %d", len(rows))
	}
}

func addDbtModelNode(g *graph.Graph, id, name, framework, resourceType, materialized string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindTable, Name: name, Language: "sql",
		FilePath: name + ".sql", StartLine: 1,
		Meta: map[string]any{
			"framework": framework, "resource_type": resourceType,
			"materialized": materialized,
		},
	})
}

func addDbtColumn(g *graph.Graph, modelID, col string) {
	colID := modelID + "::" + col
	g.AddNode(&graph.Node{ID: colID, Kind: graph.KindColumn, Name: col, Language: "sql"})
	g.AddEdge(&graph.Edge{From: colID, To: modelID, Kind: graph.EdgeMemberOf})
}

func TestAnalyzeDbtModels_ListingAndCounts(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	addDbtModelNode(g, "dbt::model::stg_orders", "stg_orders", "dbt", "model", "view")
	addDbtModelNode(g, "dbt::model::dim_orders", "dim_orders", "dbt", "model", "table")
	addDbtModelNode(g, "dbt::source::raw.orders", "raw.orders", "dbt", "source", "")
	addDbtModelNode(g, "sqlmesh::model::sushi.customers", "customers", "sqlmesh", "model", "full")

	addDbtColumn(g, "dbt::model::stg_orders", "order_id")
	addDbtColumn(g, "dbt::model::stg_orders", "total")
	addDbtColumn(g, "dbt::model::dim_orders", "order_id")

	// Lineage: dim_orders depends on stg_orders depends on raw.orders.
	g.AddEdge(&graph.Edge{From: "dbt::model::dim_orders", To: "dbt::model::stg_orders", Kind: graph.EdgeDependsOn})
	g.AddEdge(&graph.Edge{From: "dbt::model::stg_orders", To: "dbt::source::raw.orders", Kind: graph.EdgeDependsOn})

	out := callAnalyzeFramework(t, srv, "dbt_models", map[string]any{})
	rows, _ := out["dbt_models"].([]any)
	if len(rows) != 4 {
		t.Fatalf("expected 4 dbt model rows, got %d", len(rows))
	}
	byName := map[string]map[string]any{}
	for _, r := range rows {
		m := r.(map[string]any)
		byName[m["name"].(string)] = m
	}
	stg := byName["stg_orders"]
	if stg["columns"].(float64) != 2 {
		t.Errorf("stg_orders columns = %v, want 2", stg["columns"])
	}
	if stg["upstream"].(float64) != 1 {
		t.Errorf("stg_orders upstream = %v, want 1", stg["upstream"])
	}
	if stg["downstream"].(float64) != 1 {
		t.Errorf("stg_orders downstream = %v, want 1", stg["downstream"])
	}
}

func TestAnalyzeDbtModels_FilterByFramework(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	addDbtModelNode(g, "dbt::model::a", "a", "dbt", "model", "view")
	addDbtModelNode(g, "sqlmesh::model::b", "b", "sqlmesh", "model", "full")

	out := callAnalyzeFramework(t, srv, "dbt_models", map[string]any{"framework": "sqlmesh"})
	rows, _ := out["dbt_models"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after framework=sqlmesh, got %d", len(rows))
	}
	if rows[0].(map[string]any)["name"] != "b" {
		t.Errorf("expected sqlmesh model b, got %v", rows[0])
	}
}

func TestAnalyzeDbtModels_FilterByType(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	addDbtModelNode(g, "dbt::model::a", "a", "dbt", "model", "view")
	addDbtModelNode(g, "dbt::source::raw.b", "raw.b", "dbt", "source", "")

	out := callAnalyzeFramework(t, srv, "dbt_models", map[string]any{"type": "source"})
	rows, _ := out["dbt_models"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 source row, got %d", len(rows))
	}
}
