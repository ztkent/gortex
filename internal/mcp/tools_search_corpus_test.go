package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// corpusTestServer indexes a repo containing a Go file and a Markdown
// doc, so the BM25 index holds both code symbols and prose-section
// nodes.
func corpusTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.go"),
		[]byte("package app\n\nfunc StartServer() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "GUIDE.md"),
		[]byte("# Guide\n\n## Deployment\n\n"+
			"To deploy the service push the container image to the registry "+
			"and apply the kubernetes manifest.\n\n"+
			"## Troubleshooting\n\nCheck the logs when a request times out.\n"), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	eng := query.NewEngine(g)
	eng.SetSearchProvider(idx.Search)
	return NewServer(eng, g, idx, nil, zap.NewNop(), nil)
}

func corpusSearch(t *testing.T, srv *Server, args map[string]any) []map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = args
	res, err := srv.handleSearchSymbols(context.Background(), req)
	require.NoError(t, err)
	require.Falsef(t, res.IsError, "search errored: %v", res.Content)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	var out []map[string]any
	if results, ok := resp["results"].([]any); ok {
		for _, r := range results {
			out = append(out, r.(map[string]any))
		}
	}
	return out
}

// TestSearchSymbols_CorpusDocs confirms corpus:"docs" returns prose
// section hits and corpus:"code" (the default) excludes them.
func TestSearchSymbols_CorpusDocs(t *testing.T) {
	srv := corpusTestServer(t)

	// A prose-only query under corpus:docs surfaces the Deployment
	// section -- "kubernetes" appears in no code symbol.
	docs := corpusSearch(t, srv, map[string]any{"query": "kubernetes deploy", "corpus": "docs"})
	require.NotEmpty(t, docs, "corpus:docs should return prose hits")
	foundDoc := false
	for _, d := range docs {
		require.Equal(t, "doc", d["kind"], "corpus:docs must return only doc-kind nodes")
		if name, _ := d["name"].(string); name != "" {
			foundDoc = true
		}
	}
	require.True(t, foundDoc)

	// The same query under corpus:code (the default) returns no doc
	// nodes.
	code := corpusSearch(t, srv, map[string]any{"query": "kubernetes deploy"})
	for _, c := range code {
		require.NotEqual(t, "doc", c["kind"], "corpus:code must exclude prose-section nodes")
	}
}

// TestSearchSymbols_CorpusAll confirms corpus:"all" admits both code
// and prose nodes.
func TestSearchSymbols_CorpusAll(t *testing.T) {
	srv := corpusTestServer(t)
	all := corpusSearch(t, srv, map[string]any{"query": "server deploy", "corpus": "all"})
	require.NotEmpty(t, all)
	// At minimum, the corpus:all path must not error and must be able
	// to return doc-kind nodes (it does not filter them out).
	kinds := map[string]bool{}
	for _, r := range all {
		kinds[r["kind"].(string)] = true
	}
	// "deploy" hits the Deployment prose section.
	require.True(t, kinds["doc"], "corpus:all should include the matching prose section")
}

// TestSearchSymbols_CorpusRanking is the BM25-ranking assertion: a
// prose query ranks the right README section first under corpus:docs.
func TestSearchSymbols_CorpusRanking(t *testing.T) {
	srv := corpusTestServer(t)
	// "logs request times out" is the Troubleshooting section's prose.
	docs := corpusSearch(t, srv, map[string]any{"query": "logs request timeout", "corpus": "docs"})
	require.NotEmpty(t, docs)
	top, _ := docs[0]["name"].(string)
	require.Contains(t, top, "Troubleshooting",
		"the prose query should rank the Troubleshooting section first; got %q", top)
	// A doc result surfaces a section snippet, not a signature.
	require.NotEmpty(t, docs[0]["section"], "a doc result should carry a section snippet")
}

// TestSearchSymbols_CorpusInvalid confirms an unrecognised corpus
// value is a clear error.
func TestSearchSymbols_CorpusInvalid(t *testing.T) {
	srv := corpusTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{"query": "x", "corpus": "bogus"}
	res, err := srv.handleSearchSymbols(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "an invalid corpus value should surface an error")
}
