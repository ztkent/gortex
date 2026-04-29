package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"

	"github.com/zzet/gortex/internal/config"
)

// trackedPathMCPSetup builds a minimal Server + MultiIndexer with one
// repo tracked at `root`. Used by the not-tracked-guard tests so we can
// drive the dispatcher end-to-end without spinning up a real daemon.
func trackedPathMCPSetup(t *testing.T, root string) (*mcpDispatcher, *indexer.MultiIndexer) {
	t.Helper()
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	cm, err := config.NewConfigManager("")
	require.NoError(t, err)

	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	mi := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, zap.NewNop())

	// Register a repo the dispatcher will recognize. We bypass indexing
	// by stuffing metadata directly — the isCWDTracked check only reads
	// AllMetadata.
	if _, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{
		Path: root,
	}); err != nil {
		t.Fatalf("track test repo: %v", err)
	}

	eng := query.NewEngine(g)
	srv := gortexmcp.NewServer(eng, g, idx, nil, zap.NewNop(), nil, gortexmcp.MultiRepoOptions{
		MultiIndexer:  mi,
		ConfigManager: cm,
	})

	return newMCPDispatcher(srv, mi, zap.NewNop()), mi
}

func TestDispatcher_UntrackedCWD_ReturnsStructuredError(t *testing.T) {
	// Tracked root is a directory the test creates; untracked is a
	// sibling path the dispatcher shouldn't know about.
	tracked := t.TempDir()
	untracked := t.TempDir()

	d, _ := trackedPathMCPSetup(t, tracked)

	sess := &daemon.Session{ID: "sess_x", CWD: untracked}
	frame := []byte(`{"jsonrpc":"2.0","id":7,"method":"graph_stats","params":{}}`)

	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply, "untracked cwd must produce a reply, not silence")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))

	errObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "response must carry an error object: %v", parsed)

	// Machine-readable data for tool UIs.
	data, ok := errObj["data"].(map[string]any)
	require.True(t, ok, "error.data must be present for client-side handling")
	assert.Equal(t, "repo_not_tracked", data["error_code"])
	assert.Equal(t, untracked, data["path"])
	assert.Contains(t, data["suggestion"], "gortex track")

	// The response id must echo the inbound id so the client can pair
	// it with the in-flight request.
	assert.EqualValues(t, 7, parsed["id"])
}

func TestDispatcher_TrackedCWD_Passes(t *testing.T) {
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	sess := &daemon.Session{ID: "sess_y", CWD: tracked}
	// The method string doesn't matter for this test — we're proving
	// the dispatcher passes the frame through to MCPServer instead of
	// short-circuiting on the tracked-cwd guard. Whatever mcp-go does
	// with the method (including "method not found" for bogus ones) is
	// evidence that our guard let it through.
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"graph_stats","params":{}}`)

	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))

	// The response may carry an mcp-go protocol error (method name isn't
	// the right shape — real tool calls go through tools/call), but it
	// must NOT carry OUR "repo_not_tracked" sentinel.
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			assert.NotEqual(t, "repo_not_tracked", data["error_code"],
				"tracked cwd wrongly rejected by guard: %v", parsed)
		}
	}
}

func TestDispatcher_SubdirectoryOfTrackedRoot_Passes(t *testing.T) {
	tracked := t.TempDir()
	// A nested path inside a tracked root also counts as tracked — an
	// agent opened in repo/internal/auth is still "in" the repo.
	nested := filepath.Join(tracked, "internal", "auth")

	d, _ := trackedPathMCPSetup(t, tracked)

	assert.True(t, d.isCWDTracked(nested),
		"subdirectory of tracked root must be recognized as tracked")
	assert.True(t, d.isCWDTracked(tracked),
		"tracked root itself must be recognized as tracked")
	assert.False(t, d.isCWDTracked(filepath.Dir(tracked)),
		"parent of tracked root must NOT be recognized as tracked")
}

func TestDispatcher_NilMultiIndexer_AllowsEverything(t *testing.T) {
	// Single-repo mode has no multi-indexer. The guard must not reject
	// in that case — otherwise we'd break the embedded stdio path.
	d := newMCPDispatcher(nil, nil, zap.NewNop())
	assert.True(t, d.isCWDTracked("/anywhere"))
	assert.True(t, d.isCWDTracked(""))
}

// TestDispatcher_RemoteRoutableCWD_Passes covers the multi-server
// case: a cwd that is NOT in any locally tracked repo but DOES
// resolve to a workspace declared by a server in the roster must
// pass the cwd guard so the request reaches tryProxyToolCall.
//
// Without this, the four-step priority chain in RouteForCwd is
// dead code from the MCP dispatcher's perspective: any user who
// `cd`s into a remote-served repo gets a repo_not_tracked error
// even though the daemon could have proxied happily.
func TestDispatcher_RemoteRoutableCWD_Passes(t *testing.T) {
	// Local daemon tracks nothing.
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	// A repo on disk that the daemon does NOT track but whose
	// .gortex.yaml declares a workspace claimed by a roster entry.
	remote := t.TempDir()
	require.NoError(t,
		writeFile(filepath.Join(remote, ".gortex.yaml"), "workspace: remote-ws\n"))

	cfg := &daemon.ServersConfig{
		Server: []daemon.ServerEntry{
			{Slug: "self", URL: "unix:///tmp/never-dialed.sock", Default: true},
			{Slug: "remote", URL: "https://remote.example.com", Workspaces: []string{"remote-ws"}},
		},
	}
	require.NoError(t, cfg.Validate())
	router := daemon.NewRouter(daemon.RouterConfig{
		Servers:   cfg,
		Rosters:   daemon.NewWorkspaceRosterCache(0),
		LocalSlug: "self",
		Logger:    zap.NewNop(),
	})
	d.SetRouter(router)

	// Sanity: cwdReachable agrees the remote-routable cwd passes.
	assert.True(t, d.cwdReachable(remote),
		"cwd inside remote-served repo must be reachable")
	assert.False(t, d.cwdReachable(filepath.Join(t.TempDir(), "nowhere")),
		"cwd with no .gortex.yaml + no roster match must be rejected")

	// End-to-end: Dispatch must NOT short-circuit with repo_not_tracked
	// for the remote-routable cwd. We don't need the proxy to actually
	// succeed (the URL is unreachable in the test) — proving the guard
	// passed is enough; tryProxyToolCall's failure path falls through
	// to the local executor which returns a normal mcp-go reply.
	sess := &daemon.Session{ID: "sess_remote", CWD: remote}
	frame := []byte(`{"jsonrpc":"2.0","id":3,"method":"graph_stats","params":{}}`)
	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			assert.NotEqual(t, "repo_not_tracked", data["error_code"],
				"remote-routable cwd wrongly rejected by guard: %v", parsed)
		}
	}
}

// TestDispatcher_LocalWorkspaceUmbrellaCWD_Passes covers the case
// where the cwd is the umbrella directory of a multi-repo workspace:
// it has a `.gortex.yaml` declaring `workspace: <slug>` but is NOT
// itself a tracked repo, AND no server in servers.toml claims that
// workspace. The repos belonging to the workspace are tracked
// individually as siblings/subdirectories.
//
// Without this, agents started in the umbrella get a `repo_not_tracked`
// error even though RouteForCwd resolves Source="config-yaml" from
// the .gortex.yaml — making local-only workspace declarations useless
// from the dispatcher's perspective. The contract is: any "config-yaml"
// resolution is reachable, regardless of whether a server claimed it.
func TestDispatcher_LocalWorkspaceUmbrellaCWD_Passes(t *testing.T) {
	// Local daemon tracks something unrelated — proves the guard does
	// not rely on isCWDTracked seeing the umbrella itself.
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	// Umbrella directory with .gortex.yaml::workspace = "vio". The
	// umbrella itself is NOT tracked. No server in servers.toml claims
	// "vio" — only some other workspace.
	umbrella := t.TempDir()
	require.NoError(t,
		writeFile(filepath.Join(umbrella, ".gortex.yaml"), "workspace: vio\n"))

	cfg := &daemon.ServersConfig{
		Server: []daemon.ServerEntry{
			{Slug: "self", URL: "unix:///tmp/never.sock", Workspaces: []string{"gortex"}, Default: true},
		},
	}
	require.NoError(t, cfg.Validate())
	router := daemon.NewRouter(daemon.RouterConfig{
		Servers:   cfg,
		Rosters:   daemon.NewWorkspaceRosterCache(0),
		LocalSlug: "self",
		Logger:    zap.NewNop(),
	})
	d.SetRouter(router)

	assert.True(t, d.cwdReachable(umbrella),
		"workspace-umbrella cwd must be reachable when .gortex.yaml declares a workspace, even when no server claims it")

	sess := &daemon.Session{ID: "sess_umbrella", CWD: umbrella}
	frame := []byte(`{"jsonrpc":"2.0","id":9,"method":"graph_stats","params":{}}`)
	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			assert.NotEqual(t, "repo_not_tracked", data["error_code"],
				"workspace-umbrella cwd wrongly rejected by guard: %v", parsed)
		}
	}
}

// TestDispatcher_UnreachableCWD_StillRejected guards against the
// fix becoming too permissive. A cwd that's neither locally tracked
// nor matches any workspace declared in the roster (and has no
// .gortex.yaml) must still get the repo_not_tracked error.
func TestDispatcher_UnreachableCWD_StillRejected(t *testing.T) {
	tracked := t.TempDir()
	d, _ := trackedPathMCPSetup(t, tracked)

	cfg := &daemon.ServersConfig{
		Server: []daemon.ServerEntry{
			{Slug: "self", URL: "unix:///tmp/never.sock", Default: true},
		},
	}
	require.NoError(t, cfg.Validate())
	router := daemon.NewRouter(daemon.RouterConfig{
		Servers:   cfg,
		Rosters:   daemon.NewWorkspaceRosterCache(0),
		LocalSlug: "self",
		Logger:    zap.NewNop(),
	})
	d.SetRouter(router)

	stranger := t.TempDir() // no .gortex.yaml, not tracked, no roster match
	assert.False(t, d.cwdReachable(stranger))

	sess := &daemon.Session{ID: "sess_stranger", CWD: stranger}
	frame := []byte(`{"jsonrpc":"2.0","id":4,"method":"graph_stats","params":{}}`)
	reply, err := d.Dispatch(context.Background(), sess, frame)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	errObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "stranger cwd must still be rejected: %v", parsed)
	data, ok := errObj["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "repo_not_tracked", data["error_code"])
}

// writeFile is a tiny helper to keep test setup readable.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
