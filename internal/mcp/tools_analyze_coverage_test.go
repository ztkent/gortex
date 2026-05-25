package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func TestAnalyzeCoverage_StampsMeta(t *testing.T) {
	srv, dir := setupTestServer(t)

	// Make the indexed repo a Go module so ReadModulePath returns
	// something useful for the prefix-strip path.
	_ = os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.test/repo\n\ngo 1.22\n"), 0o644)

	// Synthetic cover profile: covers `main` (line 7-9), uncovered
	// segment for `helper` (line 11). Line numbers match the
	// setupTestServer fixture in server_test.go — after the fmt
	// import was dropped to keep external-call attribution clean,
	// the function bodies shifted up by 2 lines. The file path is
	// the module-qualified form Go's cover tool emits.
	profile := []byte(`mode: set
example.test/repo/main.go:7.13,9.2 1 1
example.test/repo/main.go:11.13,11.16 1 0
`)
	profilePath := filepath.Join(dir, "cover.out")
	if err := os.WriteFile(profilePath, profile, 0o644); err != nil {
		t.Fatal(err)
	}

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{
		"kind":    "coverage",
		"profile": "cover.out",
	}
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

	enriched, _ := out["enriched"].(float64)
	if enriched < 1 {
		t.Errorf("expected at least 1 enriched node, got %v\n%s", enriched, textBlock.Text)
	}
	if mp, _ := out["module_path"].(string); mp != "example.test/repo" {
		t.Errorf("module_path = %q, want example.test/repo", mp)
	}

	// Spot-check the function node got coverage_pct.
	hasCovered, hasUncovered := false, false
	for _, n := range srv.graph.AllNodes() {
		if n.Kind != graph.KindFunction {
			continue
		}
		pct, ok := n.Meta["coverage_pct"].(float64)
		if !ok {
			continue
		}
		switch n.Name {
		case "main":
			if pct == 100 {
				hasCovered = true
			}
		case "helper":
			if pct == 0 {
				hasUncovered = true
			}
		}
	}
	if !hasCovered {
		t.Error("main should have coverage_pct = 100")
	}
	if !hasUncovered {
		t.Error("helper should have coverage_pct = 0")
	}
}

func TestAnalyzeCoverage_RejectsMissingProfile(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "coverage"}
	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected error result for missing profile arg")
	}
}
