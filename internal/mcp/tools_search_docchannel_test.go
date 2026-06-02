package mcp

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// docChannelServer indexes MANY code symbols whose names share the
// query token "deploy", plus a single Markdown doc whose Deployment
// section is about deploying. The flood of code hits is what would
// crowd the lone prose section out of a tight primary fetch -- the
// docs retrieval channel must rescue it.
func docChannelServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()

	var b strings.Builder
	b.WriteString("package app\n\n")
	for i := 0; i < 40; i++ {
		// Every function name contains "deploy" so a "deploy" query
		// matches all of them on the code side.
		b.WriteString("func Deploy" + strconv.Itoa(i) + "Service() {}\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deploy.go"), []byte(b.String()), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "GUIDE.md"),
		[]byte("# Guide\n\n## Deployment\n\n"+
			"To deploy the service push the container image and apply the manifest.\n"), 0o644))

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

// TestSearchSymbols_DocChannelRescuesCrowdedProse is the core feature
// test: with a tight limit, a flood of "deploy"-named code symbols
// would crowd the single Deployment prose section out of the primary
// fetch. The docs retrieval channel over-fetches and merges the prose
// hit, so corpus:docs still returns it.
func TestSearchSymbols_DocChannelRescuesCrowdedProse(t *testing.T) {
	srv := docChannelServer(t)

	// A very tight limit. Under a pure post-filter over one
	// code-shaped fetch, the prose section (one doc among 40 code
	// hits) would never enter the candidate pool.
	docs := corpusSearch(t, srv, map[string]any{
		"query": "deploy", "corpus": "docs", "limit": float64(3),
	})
	require.NotEmpty(t, docs, "the docs channel should surface the prose section despite the code flood")
	foundDeployment := false
	for _, d := range docs {
		require.Equal(t, "doc", d["kind"], "corpus:docs must return only doc-kind nodes")
		if name, _ := d["name"].(string); strings.Contains(name, "Deployment") {
			foundDeployment = true
		}
	}
	require.True(t, foundDeployment, "the Deployment prose section must be retrieved via the docs channel")
}

// TestSearchSymbols_DocChannelAllIncludesProse confirms corpus:all
// also admits the prose section through the channel even under a tight
// limit dominated by code hits.
func TestSearchSymbols_DocChannelAllIncludesProse(t *testing.T) {
	srv := docChannelServer(t)
	all := corpusSearch(t, srv, map[string]any{
		"query": "deploy", "corpus": "all", "limit": float64(5),
	})
	require.NotEmpty(t, all)
	kinds := map[string]bool{}
	for _, r := range all {
		kinds[r["kind"].(string)] = true
	}
	require.True(t, kinds["doc"], "corpus:all should include the prose section via the docs channel")
	require.True(t, kinds["function"], "corpus:all should still include code hits")
}

// TestSearchSymbols_DocChannelInertForCode confirms the docs channel
// does NOT fire for the default code corpus -- a code-only query pays
// no extra fetch and returns no doc nodes.
func TestSearchSymbols_DocChannelInertForCode(t *testing.T) {
	srv := docChannelServer(t)
	code := corpusSearch(t, srv, map[string]any{"query": "deploy", "limit": float64(5)})
	require.NotEmpty(t, code)
	for _, c := range code {
		require.NotEqual(t, "doc", c["kind"], "corpus:code must never return prose-section nodes")
	}
}
