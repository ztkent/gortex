package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// newMultiLangRegistry registers Go + TypeScript for tests that exercise
// cross-language contracts (e.g. TS consumer → Go provider).
func newMultiLangRegistry() *parser.Registry {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	return reg
}

// writeSharedWorkspaceYAML drops a `.gortex.yaml` into dir declaring
// `workspace: shared-test` and `project: shared-test`. The cross-
// repo bridge tests in this file exercise the boundary's positive
// case: two distinct repos in the SAME (workspace, project) must
// produce CrossRepo provider/consumer pairs. The defaults
// (workspace = repo-name, project = repo-name) put each repo in its
// own bucket; without explicit shared slugs the matcher (correctly)
// declines to pair them. The shared-project declaration is the
// explicit "yes, these repos are one logical service" opt-in.
func writeSharedWorkspaceYAML(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"),
		[]byte("workspace: shared-test\nproject: shared-test\n"), 0o644))
}

// setupHTTPProviderRepo writes a Go file declaring a Gin route that binds
// GET /api/users to a handler function. After indexing, HTTPExtractor
// produces a provider contract with SymbolID pointing at the enclosing
// function (setupRoutes).
func setupHTTPProviderRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "github.com/gin-gonic/gin"

func setupRoutes(r *gin.Engine) {
	r.GET("/api/users", listUsers)
}

func listUsers() {}
`), 0o644))
	return dir
}

// setupHTTPConsumerRepo writes a Go file with an http.Get call to the same
// path. HTTPExtractor produces a consumer contract with SymbolID pointing
// at fetchUsers. After ReconcileContractEdges, fetchUsers --matches-->
// setupRoutes should exist in the graph.
func setupHTTPConsumerRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "client.go"), []byte(`package main

import "net/http"

func fetchUsers() {
	http.Get("http://api.example.com/api/users")
}
`), 0o644))
	return dir
}

// TestReconcileContractEdges_BridgesConsumerToProvider is the north-star
// test for cross-service request tracing. After indexing a provider and a
// consumer in two separate tracked repos, get_call_chain from the consumer
// function must traverse into the provider's handler region. Without the
// matcher's output persisted as EdgeMatches, the BFS stops at the
// consumer-side HTTP call — nothing bridges the service boundary.
func TestReconcileContractEdges_BridgesConsumerToProvider(t *testing.T) {
	providerRoot := setupHTTPProviderRepo(t, "provider-svc")
	consumerRoot := setupHTTPConsumerRepo(t, "consumer-svc")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRoot, Name: "provider-svc"},
			{Path: consumerRoot, Name: "consumer-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())

	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err, "track %s", entry.Name)
	}

	// EdgeMatches must land on the handler function (listUsers), not on
	// the registration helper (setupRoutes). The HTTP provider extractor
	// captures the handler identifier from `r.GET("/path", handler)`
	// patterns — T1.3 — so "trace a request" lands on business logic.
	consumerSym := "consumer-svc/client.go::fetchUsers"
	providerSym := "provider-svc/main.go::listUsers"

	var matchEdge *graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeMatches {
			continue
		}
		if e.From == consumerSym && e.To == providerSym {
			matchEdge = e
			break
		}
	}
	require.NotNil(t, matchEdge,
		"expected EdgeMatches %s → %s; present match edges were: %v",
		consumerSym, providerSym, collectMatchEdges(g))
	assert.True(t, matchEdge.CrossRepo,
		"consumer and provider live in different repos — CrossRepo flag must be set")

	// Positive end-to-end: walking forward from the consumer symbol reaches
	// the provider symbol. This is what "trace a request through product"
	// relies on.
	eng := query.NewEngine(g)
	chain := eng.GetCallChain(consumerSym, query.QueryOptions{Depth: 4, Limit: 50, Detail: "brief"})
	require.NotNil(t, chain)
	reached := false
	for _, n := range chain.Nodes {
		if n.ID == providerSym {
			reached = true
			break
		}
	}
	assert.True(t, reached,
		"get_call_chain(%s) did not reach %s; chain nodes: %v",
		consumerSym, providerSym, nodeIDs(chain.Nodes))
}

// setupTSConsumerRepo writes a TypeScript file that builds its request URL
// from a template literal (${API_URL}/path) — the dominant pattern in the
// web/extension/mobile consumers in the tuck audit. T1.1 normalization
// must strip the base-URL placeholder so the consumer contract ID matches
// the provider's /v1/users.
func setupTSConsumerRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"`+name+`","version":"0.0.0"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "client.ts"), []byte(
		"const API_URL = \"https://api.example.com\";\n"+
			"export async function fetchUsers() {\n"+
			"  return fetch(`${API_URL}/api/users`);\n"+
			"}\n",
	), 0o644))
	return dir
}

// TestReconcileContractEdges_TemplateLiteralConsumer is T1.1's cross-repo
// integration guard. A TypeScript consumer constructs the request URL
// from a template literal whose base is an interpolated constant; the Go
// provider declares "/api/users" verbatim. Without NormalizeHTTPPath's
// template-literal stripping, the consumer's contract ID carries the
// placeholder and never matches the provider, so no EdgeMatches forms
// and get_call_chain stops at the fetch call site.
func TestReconcileContractEdges_TemplateLiteralConsumer(t *testing.T) {
	providerRoot := setupHTTPProviderRepo(t, "provider-svc")
	consumerRoot := setupTSConsumerRepo(t, "web-ui")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRoot, Name: "provider-svc"},
			{Path: consumerRoot, Name: "web-ui"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newMultiLangRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err, "track %s", entry.Name)
	}

	consumerSym := "web-ui/src/client.ts::fetchUsers"
	providerSym := "provider-svc/main.go::listUsers"

	var matchEdge *graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeMatches && e.From == consumerSym && e.To == providerSym {
			matchEdge = e
			break
		}
	}
	require.NotNil(t, matchEdge,
		"expected EdgeMatches %s → %s after template-literal normalization; present match edges were: %v",
		consumerSym, providerSym, collectMatchEdges(g))
	assert.True(t, matchEdge.CrossRepo, "consumer and provider live in different repos")

	eng := query.NewEngine(g)
	chain := eng.GetCallChain(consumerSym, query.QueryOptions{Depth: 4, Limit: 50, Detail: "brief"})
	reached := false
	for _, n := range chain.Nodes {
		if n.ID == providerSym {
			reached = true
			break
		}
	}
	assert.True(t, reached,
		"get_call_chain(%s) must reach %s across service boundary; chain was: %v",
		consumerSym, providerSym, nodeIDs(chain.Nodes))
}

// setupDartConsumerRepo writes a Flutter-shape api-client file with dio
// calls to clean absolute paths and Dart's bare-$id interpolation for the
// path param — the pattern tuck_app's TuckApiClient uses. T2.1 recognizes
// these as consumer contracts; NormalizeHTTPPath collapses $id to {id}.
func setupDartConsumerRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "lib", "core"), 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pubspec.yaml"),
		[]byte("name: "+name+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", "core", "api_client.dart"), []byte(
		"class TuckApiClient {\n"+
			"  final Dio _dio;\n"+
			"  TuckApiClient(this._dio);\n"+
			"\n"+
			"  Future<void> fetchUsers() async {\n"+
			"    await _dio.get('/api/users');\n"+
			"  }\n"+
			"}\n",
	), 0o644))
	return dir
}

// TestReconcileContractEdges_DartConsumer is the cross-language guard for
// T2.1 — a Flutter app's dio-based API client bridges to the Go provider.
// Without Dart patterns in the extractor the consumer would never produce
// a contract, the matcher would never pair, and get_call_chain would stop
// at TuckApiClient.fetchUsers instead of reaching the provider handler.
func TestReconcileContractEdges_DartConsumer(t *testing.T) {
	providerRoot := setupHTTPProviderRepo(t, "provider-svc")
	consumerRoot := setupDartConsumerRepo(t, "mobile-app")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRoot, Name: "provider-svc"},
			{Path: consumerRoot, Name: "mobile-app"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newMultiLangRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err, "track %s", entry.Name)
	}

	// The Dart extractor names methods by their short name, so the enclosing
	// symbol of the dio.get call is TuckApiClient.fetchUsers — the method.
	// The exact Dart symbol ID format depends on the Dart tree-sitter
	// extractor, so accept any consumer ID in the mobile-app repo whose
	// name ends in "fetchUsers".
	providerSym := "provider-svc/main.go::listUsers"

	var matchEdge *graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeMatches {
			continue
		}
		if e.To != providerSym {
			continue
		}
		n := g.GetNode(e.From)
		if n != nil && n.Name == "fetchUsers" && n.RepoPrefix == "mobile-app" {
			matchEdge = e
			break
		}
	}
	require.NotNil(t, matchEdge,
		"expected EdgeMatches from Dart fetchUsers to %s; present match edges: %v",
		providerSym, collectMatchEdges(g))
	assert.True(t, matchEdge.CrossRepo,
		"consumer (Dart) and provider (Go) live in different repos")

	eng := query.NewEngine(g)
	chain := eng.GetCallChain(matchEdge.From, query.QueryOptions{Depth: 4, Limit: 50, Detail: "brief"})
	reached := false
	for _, n := range chain.Nodes {
		if n.ID == providerSym {
			reached = true
			break
		}
	}
	assert.True(t, reached,
		"get_call_chain(%s) must reach %s; chain was: %v",
		matchEdge.From, providerSym, nodeIDs(chain.Nodes))
}

// setupTSWrapperRepo mirrors tuck's web/lib/api.ts shape: a private
// doFetch that calls fetch(`${API_URL}${path}`), a private request
// wrapper that forwards its path parameter to doFetch, and several
// exported per-endpoint functions that each call request with a
// literal path (some plain, some template-literal with params, some
// carrying a method: in the options). The wrapper chain has depth 2 —
// doFetch is the initial wrapper, request is discovered in the
// second BFS pass, and the exported functions are where inline
// contracts finally land.
func setupTSWrapperRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "lib"), 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"`+name+`","version":"0.0.0"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", "api.ts"), []byte(
		"const API_URL = \"https://api.example.com\";\n"+
			"\n"+
			"async function doFetch(path: string, token: string, options: any = {}) {\n"+
			"  return fetch(`${API_URL}${path}`, { ...options, headers: { Authorization: `Bearer ${token}` } });\n"+
			"}\n"+
			"\n"+
			"async function request<T>(path: string, getToken: () => Promise<string>, options: any = {}): Promise<T> {\n"+
			"  const token = await getToken();\n"+
			"  const res = await doFetch(path, token, options);\n"+
			"  return res.json();\n"+
			"}\n"+
			"\n"+
			"export async function fetchUsers(getToken: () => Promise<string>) {\n"+
			"  return request<any>('/api/users', getToken);\n"+
			"}\n"+
			"\n"+
			"export async function fetchUser(getToken: () => Promise<string>, id: string) {\n"+
			"  return request<any>(`/api/users/${id}`, getToken);\n"+
			"}\n"+
			"\n"+
			"export async function createUser(getToken: () => Promise<string>, data: any) {\n"+
			"  return request<any>('/api/users', getToken, { method: 'POST', body: JSON.stringify(data) });\n"+
			"}\n"+
			"\n"+
			"export async function deleteUser(getToken: () => Promise<string>, id: string) {\n"+
			"  return request<void>(`/api/users/${id}`, getToken, { method: 'DELETE' });\n"+
			"}\n",
	), 0o644))
	return dir
}

// setupGoProviderRepo writes a handler for every endpoint the TS
// wrapper repo calls. listUsers, getUser, createUser, deleteUser.
// Gin-style route declarations so T1.3 handler resolution can pick
// the method-level handler as the match target.
func setupGoProviderRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "github.com/gin-gonic/gin"

func setupRoutes(r *gin.Engine) {
	r.GET("/api/users", listUsers)
	r.GET("/api/users/:id", getUser)
	r.POST("/api/users", createUser)
	r.DELETE("/api/users/:id", deleteUser)
}

func listUsers()   {}
func getUser()     {}
func createUser()  {}
func deleteUser()  {}
`), 0o644))
	return dir
}

// TestInlineWrappers_TuckShape is the T2.4 north-star test. A TS
// wrapper chain (doFetch → request → exported fetch/create/delete
// functions) plus a Go provider with matching routes must produce
// one EdgeMatches per endpoint, not one meta-match behind an
// unresolvable wrapper contract.
//
// The test asserts three things that together define the feature
// working end-to-end:
//
//  1. A match edge exists from each exported TS function (fetchUsers,
//     fetchUser, createUser, deleteUser) to the corresponding Go
//     handler — proving wrapper inlining emits per-caller contracts
//     and that method inference distinguishes GET from POST/DELETE.
//  2. The edges have CrossRepo set, since consumer and provider live
//     in different repos.
//  3. get_call_chain from any exported function reaches its matched
//     handler across the bridge.
func TestInlineWrappers_TuckShape(t *testing.T) {
	providerRoot := setupGoProviderRepo(t, "provider-svc")
	consumerRoot := setupTSWrapperRepo(t, "web-ui")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRoot, Name: "provider-svc"},
			{Path: consumerRoot, Name: "web-ui"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newMultiLangRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err, "track %s", entry.Name)
	}

	type bridge struct{ consumer, provider string }
	wantBridges := []bridge{
		{"web-ui/lib/api.ts::fetchUsers", "provider-svc/main.go::listUsers"},
		{"web-ui/lib/api.ts::fetchUser", "provider-svc/main.go::getUser"},
		{"web-ui/lib/api.ts::createUser", "provider-svc/main.go::createUser"},
		{"web-ui/lib/api.ts::deleteUser", "provider-svc/main.go::deleteUser"},
	}

	have := make(map[string]*graph.Edge)
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeMatches {
			continue
		}
		have[e.From+"|"+e.To] = e
	}

	for _, b := range wantBridges {
		e, ok := have[b.consumer+"|"+b.provider]
		if !ok {
			t.Errorf("missing bridge %s → %s; have: %v", b.consumer, b.provider, matchEdgeSummaries(g))
			continue
		}
		assert.True(t, e.CrossRepo,
			"bridge %s → %s: CrossRepo must be set for TS→Go chain", b.consumer, b.provider)
	}

	// get_call_chain spot-check — pick the POST case, since method
	// inference is what distinguishes createUser → createUser (POST)
	// from fetchUsers → listUsers (GET).
	eng := query.NewEngine(g)
	chain := eng.GetCallChain("web-ui/lib/api.ts::createUser",
		query.QueryOptions{Depth: 4, Limit: 50, Detail: "brief"})
	reached := false
	for _, n := range chain.Nodes {
		if n.ID == "provider-svc/main.go::createUser" {
			reached = true
			break
		}
	}
	assert.True(t, reached,
		"get_call_chain(createUser[TS]) must reach createUser[Go] handler across the wrapper bridge; chain: %v",
		nodeIDs(chain.Nodes))
}

// setupGoTopicPublisherRepo writes a minimal Go package that calls
// p.Produce("user.created", ...) — Kafka producer idiom — from
// inside a named function. Pairs with setupGoTopicSubscriberRepo on
// the same broker (Kafka) so the matcher's broker-aware bucket
// keys see them as the same topic identity.
func setupGoTopicPublisherRepo(t *testing.T, name, topic string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pub.go"), []byte(
		"package main\n\nfunc publishEvent(p Producer) {\n\tp.Produce(\""+topic+"\", nil)\n}\n\ntype Producer interface{ Produce(topic string, v any) error }\n",
	), 0o644))
	return dir
}

// setupGoTopicSubscriberRepo mirrors the publisher: a Go function
// calling c.SubscribeTopics([]string{"user.created"}) — the
// confluent-kafka-go Consumer idiom — so the broker tag aligns
// with the publisher's Kafka contract.
func setupGoTopicSubscriberRepo(t *testing.T, name, topic string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub.go"), []byte(
		"package main\n\nfunc consumeEvent(c Consumer) {\n\tc.SubscribeTopics([]string{\""+topic+"\"}, nil)\n}\n\ntype Consumer interface{ SubscribeTopics(topics []string, rb any) error }\n",
	), 0o644))
	return dir
}

// TestReconcileContractEdges_TopicBridge exercises Phase A1 — publish
// and subscribe sites in two repos must bridge via EdgeMatches once
// both sides anchor SymbolID on their enclosing function.
func TestReconcileContractEdges_TopicBridge(t *testing.T) {
	pubRoot := setupGoTopicPublisherRepo(t, "producer-svc", "user.created")
	subRoot := setupGoTopicSubscriberRepo(t, "consumer-svc", "user.created")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: pubRoot, Name: "producer-svc"},
			{Path: subRoot, Name: "consumer-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	pub := "producer-svc/pub.go::publishEvent"
	sub := "consumer-svc/sub.go::consumeEvent"

	var matchEdge *graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeMatches && e.From == sub && e.To == pub {
			matchEdge = e
			break
		}
	}
	require.NotNil(t, matchEdge,
		"expected EdgeMatches %s → %s for topic bridge; have: %v",
		sub, pub, matchEdgeSummaries(g))
	assert.True(t, matchEdge.CrossRepo, "publisher and subscriber in different repos")
}

// setupGoEnvConsumerRepo calls os.Getenv from a named function.
func setupGoEnvConsumerRepo(t *testing.T, name, envVar string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.go"), []byte(
		"package main\n\nimport \"os\"\n\nfunc loadDatabase() string {\n\treturn os.Getenv(\""+envVar+"\")\n}\n",
	), 0o644))
	return dir
}

// TestEnvConsumer_SymbolIDSet covers the env-var pillar of Phase A4:
// bridges are not expected (config files aren't functions) but the
// consumer contract MUST carry a SymbolID so get_dependencies can
// trace handler → env::VAR. The test asserts the os.Getenv call is
// anchored on loadDatabase.
func TestEnvConsumer_SymbolIDSet(t *testing.T) {
	repo := setupGoEnvConsumerRepo(t, "svc", "DATABASE_URL")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: repo, Name: "svc"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	// Single-repo mode skips RepoPrefix, so the SymbolID is unprefixed.
	// The point is the anchor is set at all — that's what T1.2's bridge
	// emission check requires.
	want := "config.go::loadDatabase"
	var found bool
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeConsumes && e.From == want && e.To == "env::DATABASE_URL" {
			found = true
			break
		}
	}
	assert.True(t, found,
		"expected EdgeConsumes %s → env::DATABASE_URL (SymbolID must anchor on loadDatabase)", want)
}

// setupGRPCProtoProviderRepo writes a repo containing:
//   - a .proto file declaring service Users with one RPC
//   - a Go implementation file with UsersServer.GetUser as the handler
//     and a pb.RegisterUsersServer registration site
//
// After indexing, the proto file produces a provider contract
// "grpc::Users::GetUser" whose SymbolID is initially empty.
// BindProviderSymbols should resolve it to UsersServer.GetUser.
func setupGRPCProtoProviderRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "proto"), 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "proto", "users.proto"), []byte(`syntax = "proto3";
package users;

service Users {
	rpc GetUser (GetUserRequest) returns (GetUserResponse);
}

message GetUserRequest { string id = 1; }
message GetUserResponse { string name = 1; }
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.go"), []byte(`package main

import "context"

type UsersServer struct{}

func (s *UsersServer) GetUser(ctx context.Context, req *GetUserRequest) (*GetUserResponse, error) {
	return &GetUserResponse{Name: "Alice"}, nil
}

type GetUserRequest struct{ Id string }
type GetUserResponse struct{ Name string }

func register(grpcServer interface{}) {
	pb.RegisterUsersServer(grpcServer, &UsersServer{})
}
`), 0o644))
	return dir
}

// setupGRPCGoConsumerRepo mirrors the real pattern: establish
// userClient via pb.NewUsersClient(conn), then invoke the RPC as a
// method call. The two-pass extractor emits grpc::Users::GetUser
// with SymbolID on the calling function.
func setupGRPCGoConsumerRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import (
	"context"
	"example.com/pb"
)

func callUsers(ctx context.Context, conn interface{}) {
	userClient := pb.NewUsersClient(conn)
	_, _ = userClient.GetUser(ctx, &pb.GetUserRequest{Id: "x"})
}
`), 0o644))
	return dir
}

// TestReconcileContractEdges_GRPCBridge is the end-to-end guarantee
// for gRPC: a .proto provider in repo A, a Go consumer in repo B
// using pb.NewClient + method calls, plus an implementation method
// in repo A matching the RPC by name on a *UsersServer receiver.
// The matcher must pair them, BindProviderSymbols must resolve the
// provider SymbolID to the handler, and ReconcileContractEdges must
// emit EdgeMatches from the consumer call site to the handler —
// enabling get_call_chain to cross the service boundary.
func TestReconcileContractEdges_GRPCBridge(t *testing.T) {
	providerRoot := setupGRPCProtoProviderRepo(t, "auth-service")
	consumerRoot := setupGRPCGoConsumerRepo(t, "client-svc")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRoot, Name: "auth-service"},
			{Path: consumerRoot, Name: "client-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newMultiLangRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	consumerSym := "client-svc/main.go::callUsers"
	providerSym := "auth-service/server.go::UsersServer.GetUser"

	var matchEdge *graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeMatches && e.From == consumerSym && e.To == providerSym {
			matchEdge = e
			break
		}
	}
	require.NotNil(t, matchEdge,
		"expected EdgeMatches %s → %s for gRPC bridge; present match edges: %v",
		consumerSym, providerSym, matchEdgeSummaries(g))
	assert.True(t, matchEdge.CrossRepo, "consumer and provider live in different repos")

	eng := query.NewEngine(g)
	chain := eng.GetCallChain(consumerSym, query.QueryOptions{Depth: 4, Limit: 50, Detail: "brief"})
	reached := false
	for _, n := range chain.Nodes {
		if n.ID == providerSym {
			reached = true
			break
		}
	}
	assert.True(t, reached,
		"get_call_chain(%s) must reach %s across the gRPC bridge; chain: %v",
		consumerSym, providerSym, nodeIDs(chain.Nodes))
}

// setupOpenAPIProviderRepo writes a minimal repo containing an
// OpenAPI 3.0 YAML spec plus a Go Gin implementation. After Phase C
// OpenAPI providers emit IDs in the http::METHOD::path shape so they
// match HTTP consumers from other repos; BindProviderSymbols then
// attaches the spec-declared provider to the Gin-registered handler
// so the bridge lands on real business logic.
func setupOpenAPIProviderRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "openapi.yaml"), []byte(`openapi: 3.0.0
info:
  title: API
  version: 1.0.0
paths:
  /v1/widgets:
    get:
      operationId: listWidgets
      responses:
        '200':
          description: ok
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "github.com/gin-gonic/gin"

func setupRoutes(r *gin.Engine) {
	r.GET("/v1/widgets", listWidgets)
}

func listWidgets() {}
`), 0o644))
	return dir
}

// setupHTTPConsumerForOpenAPI writes a tiny Go consumer that calls
// the same /v1/widgets endpoint via http.Get.
func setupHTTPConsumerForOpenAPI(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeSharedWorkspaceYAML(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "client.go"), []byte(`package main

import "net/http"

func fetchWidgets() {
	http.Get("http://api.example.com/v1/widgets")
}
`), 0o644))
	return dir
}

// TestReconcileContractEdges_OpenAPIBridge asserts Phase C: an
// OpenAPI-declared provider pairs with an HTTP consumer from another
// repo because both now produce http::GET::/v1/widgets IDs. The match
// may land on either provider (spec or Gin registration) — both
// point at the same handler once the matcher picks one; we accept
// whichever and verify the consumer→handler bridge is intact.
func TestReconcileContractEdges_OpenAPIBridge(t *testing.T) {
	providerRoot := setupOpenAPIProviderRepo(t, "api-svc")
	consumerRoot := setupHTTPConsumerForOpenAPI(t, "client-svc")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRoot, Name: "api-svc"},
			{Path: consumerRoot, Name: "client-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newMultiLangRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	consumerSym := "client-svc/client.go::fetchWidgets"
	// Match edge may target listWidgets directly (Gin handler-level
	// binding via T1.3) or it may go through the OpenAPI binding —
	// both land on the same handler method. Accept either.
	handlerSym := "api-svc/main.go::listWidgets"

	var matchEdge *graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeMatches && e.From == consumerSym && e.To == handlerSym {
			matchEdge = e
			break
		}
	}
	require.NotNil(t, matchEdge,
		"expected EdgeMatches %s → %s for OpenAPI bridge; present match edges: %v",
		consumerSym, handlerSym, matchEdgeSummaries(g))
	assert.True(t, matchEdge.CrossRepo, "consumer and provider live in different repos")

	eng := query.NewEngine(g)
	chain := eng.GetCallChain(consumerSym, query.QueryOptions{Depth: 4, Limit: 50, Detail: "brief"})
	reached := false
	for _, n := range chain.Nodes {
		if n.ID == handlerSym {
			reached = true
			break
		}
	}
	assert.True(t, reached,
		"get_call_chain(%s) must reach %s; chain: %v",
		consumerSym, handlerSym, nodeIDs(chain.Nodes))
}

// matchEdgeSummaries dumps all EdgeMatches as "from → to" strings for
// failure-message context when the expected bridges aren't present.
func matchEdgeSummaries(g *graph.Graph) []string {
	var out []string
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeMatches {
			out = append(out, e.From+" → "+e.To)
		}
	}
	return out
}

// TestReconcileContractEdges_PurgesStaleOnUntrack asserts that removing
// the consumer repo deletes its match edges — otherwise the graph would
// accumulate dangling edges pointing at symbols that no longer exist.
func TestReconcileContractEdges_PurgesStaleOnUntrack(t *testing.T) {
	providerRoot := setupHTTPProviderRepo(t, "provider-svc")
	consumerRoot := setupHTTPConsumerRepo(t, "consumer-svc")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: providerRoot, Name: "provider-svc"},
			{Path: consumerRoot, Name: "consumer-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	require.NotEmpty(t, collectMatchEdges(g), "setup precondition: at least one EdgeMatches must exist")

	mi.UntrackRepo("consumer-svc")

	remaining := collectMatchEdges(g)
	assert.Empty(t, remaining,
		"untracking the consumer must purge its match edges; found %d leftover: %v",
		len(remaining), remaining)
}

func collectMatchEdges(g *graph.Graph) []string {
	var out []string
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeMatches {
			out = append(out, e.From+" → "+e.To)
		}
	}
	return out
}

func nodeIDs(nodes []*graph.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}
