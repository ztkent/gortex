package indexer

// Workspace-isolation acceptance suite. The five criteria below are
// the alpha-launch exit signal: all five must pass on a fresh local
// setup with two unrelated workspaces (`tuck` and `personal`), each
// defining `POST /api/auth/login`.
//
// The suite uses the same MultiIndexer + ConfigManager wiring real
// users go through, populated with fixture repos under `t.TempDir()`.
// No cloud, no daemon — purely local; all five criteria are
// mechanically testable on a single laptop.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// writeWorkspaceYAML drops a `.gortex.yaml` declaring workspace and
// optional project. Callers pass project="" for the
// "single-project repo, project = workspace" default.
func writeWorkspaceYAML(t *testing.T, dir, workspace, project string) {
	t.Helper()
	body := "workspace: " + workspace + "\n"
	if project != "" {
		body += "project: " + project + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte(body), 0o644))
}

// setupHTTPLoginProvider writes a Go file with a Gin handler for
// POST /api/auth/login.
func setupHTTPLoginProvider(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "github.com/gin-gonic/gin"

func setupRoutes(r *gin.Engine) {
	r.POST("/api/auth/login", login)
}

func login() {}
`), 0o644))
	return dir
}

// runMatcher returns the merged-registry MatchResult — the same shape
// `gortex contracts check` produces for the user.
func runMatcher(mi *MultiIndexer) contracts.MatchResult {
	return contracts.Match(mi.MergedContractRegistry())
}

// TestSpecLaunch_4_5_OrphansAreReportedPerWorkspace covers criterion 1:
// "gortex contracts check for tuck reports an orphan provider in tuck
// (consumer not present), and similarly for personal."
//
// Both workspaces ship a provider but no consumer; the matcher must
// report each workspace's provider as orphaned, and the orphan's
// WorkspaceID must be set so a `--workspace tuck` filter can isolate
// the right list.
func TestSpecLaunch_4_5_OrphansAreReportedPerWorkspace(t *testing.T) {
	tuckRoot := setupHTTPLoginProvider(t, "tuck-api")
	personalRoot := setupHTTPLoginProvider(t, "personal-app")
	writeWorkspaceYAML(t, tuckRoot, "tuck", "")
	writeWorkspaceYAML(t, personalRoot, "personal", "")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: tuckRoot, Name: "tuck-api"},
			{Path: personalRoot, Name: "personal-app"},
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

	result := runMatcher(mi)

	// No matches — there are no consumers anywhere.
	assert.Empty(t, result.Matched, "no consumers exist; matcher must produce zero matches")

	// Both providers must be orphaned, each carrying its own
	// workspace slug so a UI / CLI filter can isolate them.
	tuckSeen, personalSeen := false, false
	for _, op := range result.OrphanProviders {
		switch op.WorkspaceID {
		case "tuck":
			tuckSeen = true
		case "personal":
			personalSeen = true
		}
	}
	assert.True(t, tuckSeen, "tuck provider must appear as an orphan with WorkspaceID=tuck")
	assert.True(t, personalSeen, "personal provider must appear as an orphan with WorkspaceID=personal")
}

// TestSpecLaunch_4_5_NoCrossWorkspacePairs covers criterion 2:
// "the two services do not appear paired with each other in either
// contracts list or contracts check. Cross-workspace traversal on
// the matcher is impossible by construction."
//
// Both workspaces have BOTH a provider and a consumer for
// POST /api/auth/login. Without workspace bucketing the matcher
// would have produced 4 cross-workspace pairs. With boundary
// enforcement we expect exactly 2 pairs, one per workspace, neither
// crossing the workspace line.
func TestSpecLaunch_4_5_NoCrossWorkspacePairs(t *testing.T) {
	tuckRoot := setupHTTPLoginProvider(t, "tuck-api")
	tuckConsumerRoot := setupHTTPLoginConsumer(t, "tuck-app")
	personalRoot := setupHTTPLoginProvider(t, "personal-app")
	personalConsumerRoot := setupHTTPLoginConsumer(t, "personal-cli")

	for _, dir := range []string{tuckRoot, tuckConsumerRoot} {
		writeWorkspaceYAML(t, dir, "tuck", "tuck")
	}
	for _, dir := range []string{personalRoot, personalConsumerRoot} {
		writeWorkspaceYAML(t, dir, "personal", "personal")
	}

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: tuckRoot, Name: "tuck-api"},
			{Path: tuckConsumerRoot, Name: "tuck-app"},
			{Path: personalRoot, Name: "personal-app"},
			{Path: personalConsumerRoot, Name: "personal-cli"},
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

	result := runMatcher(mi)

	require.Len(t, result.Matched, 2,
		"expected exactly 2 matches (tuck↔tuck, personal↔personal); got %d", len(result.Matched))
	for _, m := range result.Matched {
		assert.Equal(t, m.Provider.WorkspaceID, m.Consumer.WorkspaceID,
			"matched pair must share a workspace; got provider=%s consumer=%s",
			m.Provider.WorkspaceID, m.Consumer.WorkspaceID)
	}
}

// TestSpecLaunch_4_5_FindUsagesScopedToWorkspace covers criterion 3:
// "find_usages on a tuck symbol returns hits only from tuck."
//
// Setup: tuck has a function `tuckSharedHelper` and a caller in the
// same workspace. We run FindUsagesScoped with WorkspaceID="tuck"
// and require every returned node belong to that workspace.
func TestSpecLaunch_4_5_FindUsagesScopedToWorkspace(t *testing.T) {
	tuckRoot := setupGoCallerCallee(t, "tuck-api")
	personalRoot := setupGoCallerCallee(t, "personal-app")
	writeWorkspaceYAML(t, tuckRoot, "tuck", "")
	writeWorkspaceYAML(t, personalRoot, "personal", "")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: tuckRoot, Name: "tuck-api"},
			{Path: personalRoot, Name: "personal-app"},
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

	eng := query.NewEngine(g)

	// Look up the tuck-side callee. Both repos use the same name —
	// criterion 3's whole point is that the lookup stays in the
	// tuck workspace despite the personal repo defining the same
	// symbol.
	tuckHelperID := "tuck-api/main.go::sharedHelper"
	require.NotNil(t, g.GetNode(tuckHelperID), "fixture: expected node %s", tuckHelperID)

	scoped := eng.FindUsagesScoped(tuckHelperID, query.QueryOptions{WorkspaceID: "tuck"})
	require.NotNil(t, scoped)
	for _, n := range scoped.Nodes {
		ws := n.WorkspaceID
		if ws == "" {
			ws = n.RepoPrefix
		}
		assert.Equal(t, "tuck", ws,
			"FindUsagesScoped(WorkspaceID=tuck) returned node %s in workspace %q",
			n.ID, ws)
	}
}

// TestSpecLaunch_4_5_CrossWorkspaceHappyPath covers criterion 4:
// "a workspace declaring cross_workspace_deps against gortex
// correctly resolves stubs into gortex when the user passes
// scope: workspace:gortex."
//
// We declare workspace `client-ws` with a cross_workspace_deps entry
// pointing at workspace `lib-ws`, then verify the resolver does NOT
// flag the cross-workspace edge as unresolved when the dep is
// declared. This is the inverse of TestCrossWorkspaceContractMatching:
// that test confirms the boundary blocks; this one confirms the
// declared exception lets the reference through.
//
// The full happy-path acceptance ("scope: workspace:gortex" resolves
// stubs into gortex) is covered by the resolver-level test in
// internal/resolver/cross_repo_test.go — this test pins the wire-up
// at the indexer level so a regression in the lookup plumbing
// surfaces here.
func TestSpecLaunch_4_5_CrossWorkspaceHappyPath(t *testing.T) {
	libRoot := setupGoLibraryRepo(t, "lib-svc", "lib-mod")
	clientRoot := setupGoLibraryConsumer(t, "client-svc", "lib-mod")

	writeWorkspaceYAML(t, libRoot, "lib-ws", "")
	require.NoError(t, os.WriteFile(filepath.Join(clientRoot, ".gortex.yaml"), []byte(
		"workspace: client-ws\n"+
			"cross_workspace_deps:\n"+
			"  - workspace: lib-ws\n"+
			"    modules: [\"example.com/lib-mod\"]\n"+
			"    mode: read-only\n",
	), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: libRoot, Name: "lib-svc"},
			{Path: clientRoot, Name: "client-svc"},
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

	// The wire-up itself is what we care about. The
	// crossWorkspaceLookup MultiIndexer method should return a
	// non-empty rule list for `client-ws`, mirroring the
	// `cross_workspace_deps` declaration in client-svc/.gortex.yaml.
	rules := mi.crossWorkspaceLookup()("client-ws")
	require.NotEmpty(t, rules,
		"client-ws declared cross_workspace_deps but the indexer's lookup returned no rules; "+
			"verify LoadWorkspaceConfig reads the file and CrossWorkspaceDeps survives Config.Validate")
	require.Equal(t, "lib-ws", rules[0].Workspace, "rule must target lib-ws")
	assert.Contains(t, rules[0].Modules, "example.com/lib-mod")

	// The same lookup with a non-declaring source must be empty.
	assert.Empty(t, mi.crossWorkspaceLookup()("lib-ws"),
		"lib-ws didn't declare any cross_workspace_deps; lookup must be empty")
}

// TestSpecLaunch_4_5_MonorepoProjectBoundary covers criterion 5:
// "in a monorepo declaring services/api and services/worker as
// separate projects, contracts in api and worker are extracted
// independently and not paired unless explicitly declared."
//
// Iteration 1 simplification: we model the monorepo as two repos in
// one shared workspace with distinct projects (since per-file
// projects[]-glob mapping lands later). The matcher's project
// boundary should still hold: api's provider and worker's consumer
// of the same identifier do NOT pair.
func TestSpecLaunch_4_5_MonorepoProjectBoundary(t *testing.T) {
	apiRoot := setupHTTPLoginProvider(t, "monorepo-api")
	workerRoot := setupHTTPLoginConsumer(t, "monorepo-worker")
	writeWorkspaceYAML(t, apiRoot, "monorepo", "api")
	writeWorkspaceYAML(t, workerRoot, "monorepo", "worker")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: apiRoot, Name: "monorepo-api"},
			{Path: workerRoot, Name: "monorepo-worker"},
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

	result := runMatcher(mi)

	// Same workspace ("monorepo") but different projects ("api" /
	// "worker") — Match must NOT pair. Both sides become orphans.
	for _, m := range result.Matched {
		// If there's any pair, both ends must share a project. The
		// fixture only has cross-project pairs, so any match would
		// be a violation.
		assert.Equal(t, m.Provider.ProjectID, m.Consumer.ProjectID,
			"cross-project pair must not be matched: provider.project=%s consumer.project=%s",
			m.Provider.ProjectID, m.Consumer.ProjectID)
	}

	// Both provider and consumer should appear as orphans because
	// they're in different projects.
	assert.NotEmpty(t, result.OrphanProviders, "api provider must be orphaned (worker is in a different project)")
	assert.NotEmpty(t, result.OrphanConsumers, "worker consumer must be orphaned (api is in a different project)")
}

// setupHTTPLoginConsumer writes a Go file with an http.Post to the
// login endpoint.
func setupHTTPLoginConsumer(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "client.go"), []byte(`package main

import (
	"net/http"
	"strings"
)

func doLogin() {
	http.Post("http://api.example.com/api/auth/login", "application/json", strings.NewReader(""))
}
`), 0o644))
	return dir
}

// setupGoCallerCallee writes a Go file with `caller` -> `sharedHelper`.
// Both functions live in the same file under the repo's root.
func setupGoCallerCallee(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

func sharedHelper() string { return "ok" }

func caller() string { return sharedHelper() }
`), 0o644))
	return dir
}

// setupGoLibraryRepo writes a Go module that exports a function the
// consumer will call. The module path is example.com/<modName>.
func setupGoLibraryRepo(t *testing.T, name, modName string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+modName+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib.go"), []byte(`package lib

func Greet(name string) string { return "hello " + name }
`), 0o644))
	return dir
}

// setupGoLibraryConsumer writes a Go file that imports the library
// module and calls Greet. The import path is example.com/<modName>.
func setupGoLibraryConsumer(t *testing.T, name, modName string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "example.com/`+modName+`"

func main() {
	_ = lib.Greet("world")
}
`), 0o644))
	return dir
}
