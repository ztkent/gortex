package semantic

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// mockProvider is a test provider that records calls.
type mockProvider struct {
	name       string
	languages  []string
	available  bool
	enrichFunc func(g *graph.Graph, root string) (*EnrichResult, error)
	closed     bool
}

func (m *mockProvider) Name() string        { return m.name }
func (m *mockProvider) Languages() []string { return m.languages }
func (m *mockProvider) Available() bool     { return m.available }
func (m *mockProvider) Close() error        { m.closed = true; return nil }

func (m *mockProvider) Enrich(g *graph.Graph, repoRoot string) (*EnrichResult, error) {
	if m.enrichFunc != nil {
		return m.enrichFunc(g, repoRoot)
	}
	return &EnrichResult{
		Provider:        m.name,
		Language:        m.languages[0],
		EdgesConfirmed:  5,
		EdgesAdded:      2,
		CoveragePercent: 95.0,
	}, nil
}

func (m *mockProvider) EnrichFile(g *graph.Graph, repoRoot, filePath string) (*EnrichResult, error) {
	return nil, nil
}

func TestManager_EnrichAll(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "test-go",
		languages: []string{"go"},
		available: true,
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})

	roots := map[string]string{"default": "/tmp/test"}
	results, err := mgr.EnrichAll(g, roots)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "test-go", results[0].Provider)
	assert.Equal(t, 5, results[0].EdgesConfirmed)
}

func TestManager_PrioritySelection(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "high-priority", Languages: []string{"go"}, Priority: 1, Enabled: true},
			{Name: "low-priority", Languages: []string{"go"}, Priority: 2, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)

	highCalled := false
	lowCalled := false

	mgr.RegisterProvider(&mockProvider{
		name:      "high-priority",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g *graph.Graph, root string) (*EnrichResult, error) {
			highCalled = true
			return &EnrichResult{Provider: "high-priority", Language: "go"}, nil
		},
	})
	mgr.RegisterProvider(&mockProvider{
		name:      "low-priority",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g *graph.Graph, root string) (*EnrichResult, error) {
			lowCalled = true
			return &EnrichResult{Provider: "low-priority", Language: "go"}, nil
		},
	})

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	_, err := mgr.EnrichAll(g, roots)
	require.NoError(t, err)

	assert.True(t, highCalled, "high-priority provider should run")
	assert.False(t, lowCalled, "low-priority provider should not run")
}

func TestManager_UnavailableProvider(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "unavailable", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "unavailable",
		languages: []string{"go"},
		available: false,
	})

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	results, err := mgr.EnrichAll(g, roots)
	require.NoError(t, err)
	assert.Len(t, results, 0)
}

func TestManager_Disabled(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{Enabled: false}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "test",
		languages: []string{"go"},
		available: true,
	})

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	results, err := mgr.EnrichAll(g, roots)
	require.NoError(t, err)
	assert.Nil(t, results)
}

func TestManager_Close(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{Enabled: true}

	mgr := NewManager(cfg, logger)
	p := &mockProvider{name: "test", languages: []string{"go"}, available: true}
	mgr.RegisterProvider(p)

	err := mgr.Close()
	require.NoError(t, err)
	assert.True(t, p.closed)
}

// fakeRouter implements LSPRouter for tests so we can validate the
// Manager↔Router contract without spawning real LSP subprocesses.
type fakeRouter struct {
	specs        []string
	available    map[string]bool
	providers    map[string]Provider
	closeCalls   int
	providerErrs map[string]error
	calls        []string // method-name trace for ordering assertions
}

func (f *fakeRouter) EnabledSpecNames() []string {
	f.calls = append(f.calls, "EnabledSpecNames")
	out := make([]string, len(f.specs))
	copy(out, f.specs)
	return out
}

func (f *fakeRouter) SpecAvailable(name string) bool {
	f.calls = append(f.calls, "SpecAvailable:"+name)
	return f.available[name]
}

func (f *fakeRouter) ProviderForSpec(name string) (Provider, error) {
	f.calls = append(f.calls, "ProviderForSpec:"+name)
	if err, ok := f.providerErrs[name]; ok {
		return nil, err
	}
	p, ok := f.providers[name]
	if !ok {
		return nil, assertionError("no provider for spec " + name)
	}
	return p, nil
}

func (f *fakeRouter) Close() error {
	f.closeCalls++
	return nil
}

type assertionError string

func (e assertionError) Error() string { return string(e) }

// TestManager_LSPRouter_RoundTrip — SetLSPRouter / LSPRouter accessor
// pair returns the same instance.
func TestManager_LSPRouter_RoundTrip(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	r := &fakeRouter{}
	mgr.SetLSPRouter(r)
	assert.Same(t, r, mgr.LSPRouter())
}

// TestManager_EnrichAll_RoutesThroughLSPRouter — when a router is
// installed, EnrichAll asks it for each enabled spec and runs Enrich
// against the returned provider exactly once per repo root.
func TestManager_EnrichAll_RoutesThroughLSPRouter(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{Enabled: true}

	mgr := NewManager(cfg, logger)

	rsProvider := &mockProvider{
		name:      "lsp-rust-analyzer",
		languages: []string{"rust"},
		available: true,
	}
	r := &fakeRouter{
		specs:     []string{"rust-analyzer"},
		providers: map[string]Provider{"rust-analyzer": rsProvider},
	}
	mgr.SetLSPRouter(r)

	g := graph.New()
	roots := map[string]string{"repo-a": "/tmp/a", "repo-b": "/tmp/b"}
	results, err := mgr.EnrichAll(g, roots)
	require.NoError(t, err)
	// Two repos × one router-backed spec = two enrichment results.
	assert.Len(t, results, 2)
	assert.Equal(t, "lsp-rust-analyzer", results[0].Provider)
}

// TestManager_EnrichAll_SkipsCoveredLanguages — when an eager
// provider already covers the same language a router-backed spec
// serves, the router-backed enrichment is skipped (priority semantics
// preserved).
func TestManager_EnrichAll_SkipsCoveredLanguages(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "eager-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{name: "eager-go", languages: []string{"go"}, available: true})

	routerProvider := &mockProvider{name: "lsp-gopls", languages: []string{"go"}, available: true}
	r := &fakeRouter{
		specs:     []string{"gopls"},
		providers: map[string]Provider{"gopls": routerProvider},
	}
	mgr.SetLSPRouter(r)

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	results, err := mgr.EnrichAll(g, roots)
	require.NoError(t, err)
	// Eager provider runs (1 repo), router-backed gopls is skipped
	// because go is already covered.
	assert.Len(t, results, 1)
	assert.Equal(t, "eager-go", results[0].Provider)
}

// TestManager_HasProviders_RouterOnly — a router-only setup with at
// least one available spec returns true, and the check does NOT
// trigger ProviderForSpec (which would lazy-spawn a real LSP).
func TestManager_HasProviders_RouterOnly(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	r := &fakeRouter{
		specs:     []string{"rust-analyzer"},
		available: map[string]bool{"rust-analyzer": true},
	}
	mgr.SetLSPRouter(r)

	assert.True(t, mgr.HasProviders())
	for _, c := range r.calls {
		if c == "ProviderForSpec:rust-analyzer" {
			t.Fatalf("HasProviders should not lazy-spawn — calls: %v", r.calls)
		}
	}
}

// TestManager_HasProviders_NoneAvailable — router enabled but no spec
// available returns false (and again does not spawn).
func TestManager_HasProviders_NoneAvailable(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	r := &fakeRouter{
		specs:     []string{"rust-analyzer"},
		available: map[string]bool{"rust-analyzer": false},
	}
	mgr.SetLSPRouter(r)
	assert.False(t, mgr.HasProviders())
}

// TestManager_Close_ShutsDownRouter — Manager.Close cascades into
// LSPRouter.Close exactly once.
func TestManager_Close_ShutsDownRouter(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	r := &fakeRouter{}
	mgr.SetLSPRouter(r)
	require.NoError(t, mgr.Close())
	assert.Equal(t, 1, r.closeCalls)
}

func TestManager_Stats(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "test-go",
		languages: []string{"go"},
		available: true,
	})

	stats := mgr.Stats()
	require.Len(t, stats, 1)
	assert.Equal(t, "test-go", stats[0].Name)
	assert.Equal(t, "go", stats[0].Language)
	assert.Equal(t, "ready", stats[0].Status)
}
