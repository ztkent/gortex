package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fixedClockRegistry() (*agentRegistry, *time.Time) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := newAgentRegistry()
	r.now = func() time.Time { return clock }
	return r, &clock
}

func TestAgentRegistry_RegisterAndList(t *testing.T) {
	r, _ := fixedClockRegistry()
	r.register("a1", "alice", "pkg/x.go::Foo", "editing", "sess-a", nil)
	r.register("b1", "bob", "", "reviewing", "sess-b", nil)

	agents := r.list()
	require.Len(t, agents, 2)
	require.Equal(t, "alice", agents[0].Name) // sorted by name
	require.Equal(t, "pkg/x.go::Foo", agents[0].Cursor)
	require.Equal(t, "bob", agents[1].Name)
}

func TestAgentRegistry_HeartbeatUnknown(t *testing.T) {
	r, _ := fixedClockRegistry()
	_, ok := r.heartbeat("ghost", "", "")
	require.False(t, ok, "heartbeat for an unregistered agent must fail")

	r.register("a1", "alice", "", "", "", nil)
	a, ok := r.heartbeat("a1", "pkg/y.go::Bar", "editing")
	require.True(t, ok)
	require.Equal(t, "pkg/y.go::Bar", a.Cursor)
}

func TestAgentRegistry_LockConflict(t *testing.T) {
	r, _ := fixedClockRegistry()
	r.register("a1", "alice", "", "", "", nil)
	r.register("b1", "bob", "", "", "", nil)

	locked, conflicts, ok := r.lock("a1", []string{"internal/x.go", "internal/y.go"})
	require.True(t, ok)
	require.ElementsMatch(t, []string{"internal/x.go", "internal/y.go"}, locked)
	require.Empty(t, conflicts)

	// bob tries to lock one already-held + one free path.
	locked, conflicts, ok = r.lock("b1", []string{"internal/x.go", "internal/z.go"})
	require.True(t, ok)
	require.Equal(t, []string{"internal/z.go"}, locked)
	require.Len(t, conflicts, 1)
	require.Equal(t, "internal/x.go", conflicts[0].Path)
	require.Equal(t, "a1", conflicts[0].HeldBy)
}

func TestAgentRegistry_Unlock(t *testing.T) {
	r, _ := fixedClockRegistry()
	r.register("a1", "alice", "", "", "", nil)
	r.lock("a1", []string{"a.go", "b.go", "c.go"})

	a, ok := r.unlock("a1", []string{"b.go"})
	require.True(t, ok)
	require.ElementsMatch(t, []string{"a.go", "c.go"}, a.LockedPaths)

	a, _ = r.unlock("a1", nil) // unlock all
	require.Empty(t, a.LockedPaths)
}

func TestAgentRegistry_TTLPrune(t *testing.T) {
	r, clock := fixedClockRegistry()
	r.register("a1", "alice", "", "", "", nil)
	require.Len(t, r.list(), 1)

	*clock = clock.Add(r.ttl + time.Second) // past the TTL
	require.Empty(t, r.list(), "a stale agent must be pruned")
}

// TestAgentRegistryTool_EndToEnd drives the MCP dispatcher: two agents
// register, list, and contend for the same path.
func TestAgentRegistryTool_EndToEnd(t *testing.T) {
	srv, _ := setupTestServer(t)

	reg := callEditHandlerJSON(t, srv.handleAgentRegistry, map[string]any{
		"action": "register", "id": "alice", "name": "alice", "cursor": "main.go::helper",
	})
	require.Equal(t, "registered", reg["status"])
	agent := reg["agent"].(map[string]any)
	require.Equal(t, "agent", agent["kind"])
	require.Equal(t, "agent::alice", agent["id"])

	callEditHandlerJSON(t, srv.handleAgentRegistry, map[string]any{"action": "register", "id": "bob", "name": "bob"})

	list := callEditHandlerJSON(t, srv.handleAgentRegistry, map[string]any{"action": "list"})
	require.EqualValues(t, 2, list["total"])

	lock := callEditHandlerJSON(t, srv.handleAgentRegistry, map[string]any{"action": "lock", "id": "alice", "paths": "main.go"})
	require.Empty(t, lock["conflicts"])

	lock = callEditHandlerJSON(t, srv.handleAgentRegistry, map[string]any{"action": "lock", "id": "bob", "paths": "main.go"})
	conflicts := lock["conflicts"].([]any)
	require.Len(t, conflicts, 1)
	require.Equal(t, "alice", conflicts[0].(map[string]any)["held_by"])
}
