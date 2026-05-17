package resolver

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// fakeLSPHelper is a deterministic mock implementing LSPHelper for
// tests. exts narrows which file paths it claims; defs is the
// canonical mapping from (callerPath, line, name) → (defPath, line).
type fakeLSPHelper struct {
	exts   []string
	defs   map[lspKey]lspAnswer
	calls  int
	mu     sync.Mutex
	hangCh chan struct{} // optional: when non-nil, blocks Definition until closed (timeout testing)
}

type lspKey struct {
	path string
	line int
	name string
}

type lspAnswer struct {
	defPath string
	defLine int
}

func (f *fakeLSPHelper) SupportsPath(relPath string) bool {
	if len(f.exts) == 0 {
		return true
	}
	for _, e := range f.exts {
		if hasSuffix(relPath, e) {
			return true
		}
	}
	return false
}

func (f *fakeLSPHelper) Definition(relPath string, line int, name string) (string, int, bool) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.hangCh != nil {
		<-f.hangCh
	}
	a, ok := f.defs[lspKey{path: relPath, line: line, name: name}]
	if !ok {
		return "", 0, false
	}
	return a.defPath, a.defLine, true
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// TestLSPHotPath_BarrelReExport — the canonical case the heuristic
// loses: a method called by selector through a barrel re-export. The
// AST resolver can find a same-named target anywhere in the repo
// (potentially the wrong one); the LSP definition lookup pins the
// edge to the precise re-exported declaration.
func TestLSPHotPath_BarrelReExport(t *testing.T) {
	g := graph.New()
	// Files
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/real.ts", Kind: graph.KindFile, Name: "real.ts", FilePath: "src/real.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/decoy.ts", Kind: graph.KindFile, Name: "decoy.ts", FilePath: "src/decoy.ts", Language: "typescript"})

	// Decoy: same name in another file — the heuristic resolver
	// would pick this first because filterByReachability and same-
	// dir bias both fail.
	g.AddNode(&graph.Node{
		ID: "src/decoy.ts::doWork", Kind: graph.KindFunction, Name: "doWork",
		FilePath: "src/decoy.ts", StartLine: 12, EndLine: 14, Language: "typescript",
	})
	// Real definition the LSP will report.
	g.AddNode(&graph.Node{
		ID: "src/real.ts::doWork", Kind: graph.KindFunction, Name: "doWork",
		FilePath: "src/real.ts", StartLine: 7, EndLine: 9, Language: "typescript",
	})
	// Caller
	g.AddNode(&graph.Node{
		ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt",
		FilePath: "src/caller.ts", StartLine: 3, EndLine: 5, Language: "typescript",
	})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::doWork",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 4,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 4, name: "doWork"}: {defPath: "src/real.ts", defLine: 7},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/real.ts::doWork", callEdge.To, "edge must bind to LSP-reported definition, not the decoy")
	assert.Equal(t, graph.OriginLSPResolved, callEdge.Origin)
	require.NotNil(t, callEdge.Meta)
	assert.Equal(t, "lsp", callEdge.Meta["resolved_by"])
	assert.Equal(t, 1, helper.calls)
}

// TestLSPHotPath_FallthroughOnMiss — when the LSP returns no answer,
// the heuristic cascade still runs. The edge gets resolved by the
// AST resolver and its Origin reflects the AST tier (NOT lsp_*).
func TestLSPHotPath_FallthroughOnMiss(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/a.ts", Kind: graph.KindFile, Name: "a.ts", FilePath: "src/a.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/b.ts", Kind: graph.KindFile, Name: "b.ts", FilePath: "src/b.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/a.ts::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "src/a.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "src/b.ts::theTarget", Kind: graph.KindFunction, Name: "theTarget",
		FilePath: "src/b.ts", StartLine: 4, EndLine: 6, Language: "typescript",
	})

	callEdge := &graph.Edge{
		From: "src/a.ts::caller", To: "unresolved::theTarget",
		Kind: graph.EdgeCalls, FilePath: "src/a.ts", Line: 2,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{}, // empty — every call misses
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved, "heuristic cascade should still resolve")
	assert.Equal(t, "src/b.ts::theTarget", callEdge.To)
	assert.NotEqual(t, graph.OriginLSPResolved, callEdge.Origin, "miss → heuristic tier, not lsp_resolved")
	if callEdge.Meta != nil {
		assert.NotEqual(t, "lsp", callEdge.Meta["resolved_by"])
	}
}

// TestLSPHotPath_ExtensionGate — the helper short-circuits on
// SupportsPath, so a Go-file edge doesn't trigger any LSP call when
// the helper claims only TS extensions. The heuristic resolver
// produces the answer.
func TestLSPHotPath_ExtensionGate(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "b.go::target", Kind: graph.KindFunction, Name: "target",
		FilePath: "b.go", StartLine: 3, EndLine: 5, Language: "go",
	})

	callEdge := &graph.Edge{
		From: "a.go::caller", To: "unresolved::target",
		Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts", ".tsx"},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "b.go::target", callEdge.To)
	assert.Equal(t, 0, helper.calls, "helper must NOT be called for non-claimed extensions")
}

// TestLSPHotPath_KindGate — the LSP helper returns a file-node
// location, but the edge is a `calls` edge that must land on a
// function/method/closure. lspKindAcceptableFor rejects the bind
// and the heuristic falls through.
func TestLSPHotPath_KindGate(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/util.ts", Kind: graph.KindFile, Name: "util.ts", FilePath: "src/util.ts", Language: "typescript", StartLine: 1})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "src/util.ts::reallyDoIt", Kind: graph.KindFunction, Name: "reallyDoIt",
		FilePath: "src/util.ts", StartLine: 5, EndLine: 7, Language: "typescript",
	})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::reallyDoIt",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 2,
	}
	g.AddEdge(callEdge)

	// LSP points the edge at the file node, not the function node —
	// the kind-gate should reject.
	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 2, name: "reallyDoIt"}: {defPath: "src/util.ts", defLine: 1},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	// Heuristic picked the function node, not the file.
	assert.Equal(t, "src/util.ts::reallyDoIt", callEdge.To)
	// Origin must NOT be lsp_resolved — gate rejected the LSP answer.
	assert.NotEqual(t, graph.OriginLSPResolved, callEdge.Origin)
}

// TestLSPHotPath_MethodSelector — the resolver receives an unresolved
// `*.Name` selector target. tryResolveViaLSP strips the prefix and
// asks the helper for `Name`. On a hit, the method edge binds to the
// LSP-reported target across files.
func TestLSPHotPath_MethodSelector(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/svc.ts", Kind: graph.KindFile, Name: "svc.ts", FilePath: "src/svc.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/decoy.ts", Kind: graph.KindFile, Name: "decoy.ts", FilePath: "src/decoy.ts", Language: "typescript"})

	g.AddNode(&graph.Node{
		ID: "src/svc.ts::Service.handle", Kind: graph.KindMethod, Name: "handle",
		FilePath: "src/svc.ts", StartLine: 10, EndLine: 12, Language: "typescript",
	})
	g.AddNode(&graph.Node{
		ID: "src/decoy.ts::Other.handle", Kind: graph.KindMethod, Name: "handle",
		FilePath: "src/decoy.ts", StartLine: 5, EndLine: 7, Language: "typescript",
	})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript"})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::*.handle",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 9,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 9, name: "handle"}: {defPath: "src/svc.ts", defLine: 10},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/svc.ts::Service.handle", callEdge.To)
	assert.Equal(t, graph.OriginLSPResolved, callEdge.Origin)
	assert.Equal(t, "lsp", callEdge.Meta["resolved_by"])
}

// TestLSPHotPath_MethodValueReadPromotesToReferences — when the LSP
// helper binds an EdgeReads to a KindMethod (the `mux.HandleFunc("/p",
// h.foo)` shape where h.foo is passed as a method value), the kind
// must be promoted to EdgeReferences. The heuristic cascade already
// does this in resolver.go's `*. + Reads/Writes` case; the LSP hot
// path used to short-circuit before that branch ran and silently
// leave the kind as EdgeReads — which GetCallers/FindUsages drop
// (they only follow Calls/Matches/References). Every HTTP handler in
// every router-style codebase looked like dead code as a result.
func TestLSPHotPath_MethodValueReadPromotesToReferences(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/routes.go", Kind: graph.KindFile, Name: "routes.go", FilePath: "src/routes.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "src/handler.go", Kind: graph.KindFile, Name: "handler.go", FilePath: "src/handler.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "src/routes.go::RegisterRoutes", Kind: graph.KindFunction, Name: "RegisterRoutes", FilePath: "src/routes.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "src/handler.go::Handler.HandleHealth", Kind: graph.KindMethod, Name: "HandleHealth",
		FilePath: "src/handler.go", StartLine: 42, EndLine: 45, Language: "go",
	})

	readEdge := &graph.Edge{
		From: "src/routes.go::RegisterRoutes", To: "unresolved::*.HandleHealth",
		Kind: graph.EdgeReads, FilePath: "src/routes.go", Line: 10,
	}
	g.AddEdge(readEdge)

	helper := &fakeLSPHelper{
		exts: []string{".go"},
		defs: map[lspKey]lspAnswer{
			{path: "src/routes.go", line: 10, name: "HandleHealth"}: {defPath: "src/handler.go", defLine: 42},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/handler.go::Handler.HandleHealth", readEdge.To)
	assert.Equal(t, graph.OriginLSPResolved, readEdge.Origin)
	assert.Equal(t, graph.EdgeReferences, readEdge.Kind,
		"LSP-bound EdgeReads on a KindMethod must be promoted so get_callers surfaces it")
}

// TestLSPHotPath_FunctionValueReadPromotesToReferences — companion
// to the method-value test above. The cobra/CLI pattern
// `&cobra.Command{RunE: runClean}` emits EdgeReads with To=
// "unresolved::runClean", and the LSP helper happily binds it to
// the runClean function. Without promotion the wire-up site is
// invisible to get_callers, so every cobra subcommand looked dead.
func TestLSPHotPath_FunctionValueReadPromotesToReferences(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "cmd/x/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "cmd/x/main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "cmd/x/clean.go", Kind: graph.KindFile, Name: "clean.go", FilePath: "cmd/x/clean.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "cmd/x/main.go::init", Kind: graph.KindFunction, Name: "init", FilePath: "cmd/x/main.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "cmd/x/clean.go::runClean", Kind: graph.KindFunction, Name: "runClean",
		FilePath: "cmd/x/clean.go", StartLine: 20, EndLine: 30, Language: "go",
	})

	readEdge := &graph.Edge{
		From: "cmd/x/main.go::init", To: "unresolved::runClean",
		Kind: graph.EdgeReads, FilePath: "cmd/x/main.go", Line: 13,
	}
	g.AddEdge(readEdge)

	helper := &fakeLSPHelper{
		exts: []string{".go"},
		defs: map[lspKey]lspAnswer{
			{path: "cmd/x/main.go", line: 13, name: "runClean"}: {defPath: "cmd/x/clean.go", defLine: 20},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "cmd/x/clean.go::runClean", readEdge.To)
	assert.Equal(t, graph.OriginLSPResolved, readEdge.Origin)
	assert.Equal(t, graph.EdgeReferences, readEdge.Kind,
		"LSP-bound EdgeReads on a KindFunction must promote to References")
}

// TestLSPHotPath_NilHelper — when no helper is installed, the
// resolver runs heuristic-only as in the pre-N5 world.
func TestLSPHotPath_NilHelper(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.ts", Kind: graph.KindFile, Name: "a.ts", FilePath: "a.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "b.ts", Kind: graph.KindFile, Name: "b.ts", FilePath: "b.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "a.ts::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "a.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "b.ts::tgt", Kind: graph.KindFunction, Name: "tgt",
		FilePath: "b.ts", StartLine: 1, EndLine: 3, Language: "typescript",
	})

	callEdge := &graph.Edge{
		From: "a.ts::caller", To: "unresolved::tgt",
		Kind: graph.EdgeCalls, FilePath: "a.ts", Line: 1,
	}
	g.AddEdge(callEdge)

	r := New(g)
	// no helper installed
	stats := r.ResolveAll()
	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "b.ts::tgt", callEdge.To)
}

// TestLSPHotPath_IdentifierFromTarget — covers the prefix stripping
// for the target shapes the resolver dispatches on.
func TestIdentifierFromTarget(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"foo", "foo"},
		{"*.handle", "handle"},
		{"extern::pkg/sub::Symbol", "Symbol"},
		{"extern::pkg::A::B", "B"},
		{"import::pkg/foo", ""},
		{"pyrel::foo", ""},
		{"grpc::Svc::Method", ""},
	}
	for _, c := range cases {
		got := identifierFromTarget(c.in)
		assert.Equalf(t, c.want, got, "input=%q", c.in)
	}
}

// TestLSPKindAcceptableFor covers the kind-gate rules.
func TestLSPKindAcceptableFor(t *testing.T) {
	cases := []struct {
		ek   graph.EdgeKind
		nk   graph.NodeKind
		want bool
	}{
		{graph.EdgeCalls, graph.KindFunction, true},
		{graph.EdgeCalls, graph.KindMethod, true},
		{graph.EdgeCalls, graph.KindFile, false},
		{graph.EdgeCalls, graph.KindImport, false},
		{graph.EdgeExtends, graph.KindType, true},
		{graph.EdgeExtends, graph.KindFunction, false},
		{graph.EdgeImplements, graph.KindInterface, true},
		{graph.EdgeImplements, graph.KindMethod, false},
		{graph.EdgeReads, graph.KindField, true},
		{graph.EdgeReads, graph.KindVariable, true},
		{graph.EdgeReads, graph.KindFile, false},
		{graph.EdgeReferences, graph.KindType, true},
		{graph.EdgeReferences, graph.KindFile, false},
	}
	for _, c := range cases {
		got := lspKindAcceptableFor(c.ek, c.nk)
		assert.Equalf(t, c.want, got, "edge=%s node=%s", c.ek, c.nk)
	}
}

// TestLSPHotPath_LSPIndexCaching — multiple edges resolving via LSP
// to the same definition should hit the lspIndex cache, not rescan
// the file each time.
func TestLSPHotPath_LSPIndexCaching(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/svc.ts", Kind: graph.KindFile, Name: "svc.ts", FilePath: "src/svc.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "src/svc.ts::theTarget", Kind: graph.KindFunction, Name: "theTarget",
		FilePath: "src/svc.ts", StartLine: 4, EndLine: 6, Language: "typescript",
	})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callerA", Kind: graph.KindFunction, Name: "callerA", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callerB", Kind: graph.KindFunction, Name: "callerB", FilePath: "src/caller.ts", Language: "typescript"})

	e1 := &graph.Edge{From: "src/caller.ts::callerA", To: "unresolved::theTarget", Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 3}
	e2 := &graph.Edge{From: "src/caller.ts::callerB", To: "unresolved::theTarget", Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 7}
	g.AddEdge(e1)
	g.AddEdge(e2)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 3, name: "theTarget"}: {defPath: "src/svc.ts", defLine: 4},
			{path: "src/caller.ts", line: 7, name: "theTarget"}: {defPath: "src/svc.ts", defLine: 4},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	require.Equal(t, 2, stats.Resolved)
	assert.Equal(t, "src/svc.ts::theTarget", e1.To)
	assert.Equal(t, "src/svc.ts::theTarget", e2.To)
	assert.Equal(t, graph.OriginLSPResolved, e1.Origin)
	assert.Equal(t, graph.OriginLSPResolved, e2.Origin)
}

// TestLSPHotPath_NoOpAfterFileMiss — when LSP returns a path that
// doesn't exist in the graph, the bind should fall through. This
// protects against off-by-one path mismatches.
func TestLSPHotPath_NoOpAfterFileMiss(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "src/caller.ts", Language: "typescript"})

	callEdge := &graph.Edge{
		From: "src/caller.ts::callIt", To: "unresolved::ghost",
		Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 2,
	}
	g.AddEdge(callEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/caller.ts", line: 2, name: "ghost"}: {defPath: "nonexistent/file.ts", defLine: 1},
		},
	}

	r := New(g)
	r.SetLSPHelper(helper)
	stats := r.ResolveAll()

	// LSP miss path triggered (no graph node at that file) — heuristic
	// has nothing to find either, so the edge is left unresolved.
	assert.Equal(t, 0, stats.Resolved)
	assert.Equal(t, 1, stats.Unresolved)
}
