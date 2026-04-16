package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// spinUpDaemon builds a daemon with the real realController +
// mcpDispatcher + indexer stack and starts it on a short socket path.
// Returns the socket and the first tracked repo root so tests can
// handshake with a cwd that will pass the untracked-cwd guard.
//
// Heavier than the unit-test fakes — this is what makes this file
// an integration test. Everything from handshake through MCP tool
// dispatch goes through the real production code paths.
func spinUpDaemon(t *testing.T) (socket, trackedRoot string) {
	t.Helper()
	_, socket, trackedRoot = spinUpDaemonWithConfig(t)
	return socket, trackedRoot
}

// spinUpDaemonWithConfig is spinUpDaemon plus the test-scoped global
// config path, which tests that assert persistence side effects need
// to read back.
func spinUpDaemonWithConfig(t *testing.T) (configPath, socket, trackedRoot string) {
	t.Helper()

	// Short base dir so the unix socket stays under the 104-char limit
	// on macOS even with test-name suffixes.
	dir, err := os.MkdirTemp("/tmp", "gxi")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket = filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	t.Setenv("GORTEX_DAEMON_SNAPSHOT", filepath.Join(dir, "snap.gob.gz"))

	// Stage a tiny repo the daemon can track.
	trackedRoot = filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(trackedRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(trackedRoot, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644))

	// Build the production-style state in-process.
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	// Test-scoped global config path so a Track that persists doesn't
	// touch the developer's real ~/.config/gortex/config.yaml.
	configPath = filepath.Join(dir, "config.yaml")

	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	cm, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	mi := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, zap.NewNop())

	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: trackedRoot})
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := gortexmcp.NewServer(eng, g, idx, nil, zap.NewNop(), nil, gortexmcp.MultiRepoOptions{
		MultiIndexer:  mi,
		ConfigManager: cm,
	})

	d := daemon.New(socket, "test", zap.NewNop())
	d.Controller = &realController{
		graph:         g,
		multiIndexer:  mi,
		configManager: cm,
		logger:        zap.NewNop(),
	}
	d.MCPDispatcher = newMCPDispatcher(srv, mi, zap.NewNop())

	require.NoError(t, d.Listen())
	go func() { _ = d.Serve() }()
	t.Cleanup(func() { _ = d.Shutdown() })

	require.Eventually(t, func() bool { return daemon.IsRunningAt(socket) },
		2*time.Second, 10*time.Millisecond)
	return configPath, socket, trackedRoot
}

// TestDaemon_EndToEnd_GraphStatsOverMCPProxy confirms that the whole
// stack — proxy handshake → session assignment → MCP frame forwarding →
// HandleMessage dispatch → tool handler → response marshaling — works
// with production components. If this test regresses, something
// fundamental is broken even though the unit tests still pass.
func TestDaemon_EndToEnd_GraphStatsOverMCPProxy(t *testing.T) {
	socket, trackedRoot := spinUpDaemon(t)

	client, err := daemon.DialTo(socket, daemon.Handshake{
		Mode:       daemon.ModeMCP,
		CWD:        trackedRoot,
		ClientName: "integration-test",
	})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	require.NotEmpty(t, client.Ack.SessionID)

	// Send an MCP `initialize` frame first — mcp-go requires it before
	// tool calls. Mirrors what a real MCP client does on connect.
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"integration","version":"0"}}}`)
	require.NoError(t, client.WriteMCPFrame(initFrame))
	initReply, err := client.ReadMCPFrame()
	require.NoError(t, err)
	require.Contains(t, string(initReply), `"result"`,
		"initialize must succeed: %s", string(initReply))

	// Now call a real tool. graph_stats needs no args and always returns
	// something for an indexed graph.
	toolFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`)
	require.NoError(t, client.WriteMCPFrame(toolFrame))
	toolReply, err := client.ReadMCPFrame()
	require.NoError(t, err)

	var resp struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(toolReply, &resp))
	require.Nil(t, resp.Error, "graph_stats must not error: %s", string(toolReply))

	// Tool responses from mark3labs/mcp-go come back as a content array
	// with a JSON text blob inside.
	content, ok := resp.Result["content"].([]any)
	require.True(t, ok, "result.content must be present: %+v", resp.Result)
	require.NotEmpty(t, content)
	text, ok := content[0].(map[string]any)["text"].(string)
	require.True(t, ok)

	var stats map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &stats))
	assert.Greater(t, int(stats["total_nodes"].(float64)), 0,
		"indexed repo should contribute nodes: %v", stats)
}

// TestDaemon_EndToEnd_UntrackedCWDRejectedViaProxy verifies the
// structured error reaches the client end-to-end, not just the unit
// dispatcher. An agent opening in an untracked directory must see a
// usable error on every tool call, not a silent zero result.
func TestDaemon_EndToEnd_UntrackedCWDRejectedViaProxy(t *testing.T) {
	socket, trackedRoot := spinUpDaemon(t)
	untracked := filepath.Dir(trackedRoot) // parent of tracked root is NOT tracked

	client, err := daemon.DialTo(socket, daemon.Handshake{
		Mode: daemon.ModeMCP,
		CWD:  untracked,
	})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	// Skip initialize — the guard fires regardless of MCP state. That's
	// actually the right behavior: untracked cwds can't do anything
	// through the shared graph.
	frame := []byte(`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`)
	require.NoError(t, client.WriteMCPFrame(frame))
	reply, err := client.ReadMCPFrame()
	require.NoError(t, err)

	var resp struct {
		Error struct {
			Data map[string]any `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(reply, &resp))
	assert.Equal(t, "repo_not_tracked", resp.Error.Data["error_code"],
		"untracked cwd must produce structured error: %s", string(reply))
	assert.Equal(t, untracked, resp.Error.Data["path"])
}

// TestDaemon_EndToEnd_TrackAddsRepoLive proves track-while-running is
// instantly visible. We start the daemon with one tracked repo, track
// a second via the control socket, then verify a proxy dialing from
// the second repo's cwd is no longer rejected.
func TestDaemon_EndToEnd_TrackAddsRepoLive(t *testing.T) {
	socket, _ := spinUpDaemon(t)

	// Stage a second repo under a path the daemon doesn't know about.
	second := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(second, "lib.go"),
		[]byte("package main\nfunc Foo() {}\n"), 0o644))

	// Reject first — not tracked yet.
	client1, err := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeMCP, CWD: second})
	require.NoError(t, err)
	require.NoError(t, client1.WriteMCPFrame([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`)))
	rej, err := client1.ReadMCPFrame()
	require.NoError(t, err)
	require.Contains(t, string(rej), "repo_not_tracked",
		"proxy from untracked cwd must be rejected before track")
	_ = client1.Close()

	// Track via control surface.
	ctl, err := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	resp, err := ctl.Control(daemon.ControlTrack, daemon.TrackParams{Path: second})
	require.NoError(t, err)
	require.True(t, resp.OK, "track failed: %s %s", resp.ErrorCode, resp.ErrorMsg)
	_ = ctl.Close()

	// Now a proxy from the same cwd should pass the guard.
	client2, err := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeMCP, CWD: second})
	require.NoError(t, err)
	defer func() { _ = client2.Close() }()

	initFrame := []byte(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"i","version":"0"}}}`)
	require.NoError(t, client2.WriteMCPFrame(initFrame))
	_, _ = client2.ReadMCPFrame() // ack

	require.NoError(t, client2.WriteMCPFrame([]byte(
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`)))
	ok, err := client2.ReadMCPFrame()
	require.NoError(t, err)
	assert.NotContains(t, string(ok), "repo_not_tracked",
		"post-track proxy must not be rejected: %s", string(ok))
}

// TestDaemon_EndToEnd_TrackPersistsToConfig pins the contract that
// `gortex track <path>` against a *running* daemon survives a daemon
// restart. Regression for a bug where realController.Track updated the
// in-memory GlobalConfig via AddRepo but never flushed to disk, so the
// repo vanished on the next start.
func TestDaemon_EndToEnd_TrackPersistsToConfig(t *testing.T) {
	configPath, socket, _ := spinUpDaemonWithConfig(t)

	second := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(second, "lib.go"),
		[]byte("package main\nfunc Foo() {}\n"), 0o644))

	ctl, err := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	resp, err := ctl.Control(daemon.ControlTrack, daemon.TrackParams{Path: second})
	require.NoError(t, err)
	require.True(t, resp.OK, "track failed: %s %s", resp.ErrorCode, resp.ErrorMsg)
	_ = ctl.Close()

	// Re-read the config from disk as a fresh process would on restart.
	gc, err := config.LoadGlobal(configPath)
	require.NoError(t, err)

	absSecond, _ := filepath.Abs(second)
	var foundPaths []string
	for _, r := range gc.Repos {
		foundPaths = append(foundPaths, r.Path)
	}
	assert.Contains(t, foundPaths, absSecond,
		"tracked repo must be persisted to the global config; got %v", foundPaths)
}

// silence unused-import noise when gofmt reorders during edits.
var _ = fmt.Sprint
