package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithSessionID_RoundTrip(t *testing.T) {
	ctx := WithSessionID(context.Background(), "sess_abc")
	assert.Equal(t, "sess_abc", SessionIDFromContext(ctx))
}

func TestWithSessionID_EmptyIsNoOp(t *testing.T) {
	// Empty session IDs shouldn't pollute the ctx tree with a useless
	// key — callers rely on SessionIDFromContext == "" as the signal
	// "no session, use default."
	base := context.Background()
	withEmpty := WithSessionID(base, "")
	assert.Equal(t, base, withEmpty, "empty session ID must return ctx unchanged")
	assert.Equal(t, "", SessionIDFromContext(withEmpty))
}

func TestSessionIDFromContext_BareContext(t *testing.T) {
	// A context with no session value attached should return "" rather
	// than panic — callers rely on "" as the "no session, use default"
	// signal, and the helper must be safe to call from any callsite.
	assert.Equal(t, "", SessionIDFromContext(context.TODO()))
}

func TestSessionMap_GetCreatesLazily(t *testing.T) {
	m := newSessionMap()
	s1 := m.get("a")
	s2 := m.get("a")
	assert.Same(t, s1, s2, "repeated get(same id) must return the same entry")

	s3 := m.get("b")
	assert.NotSame(t, s1, s3, "different ids must yield distinct entries")
}

func TestSessionMap_Release(t *testing.T) {
	m := newSessionMap()
	original := m.get("a")
	m.release("a")
	reborn := m.get("a")
	assert.NotSame(t, original, reborn,
		"release() must drop the entry so a subsequent get() yields a fresh one")
}

// TestServer_SessionFor_IsolatedState proves that two different session
// IDs get independent per-client activity state — the core guarantee
// that makes the daemon's shared *mcp.Server safe for concurrent
// proxies.
func TestServer_SessionFor_IsolatedState(t *testing.T) {
	srv, _ := setupTestServer(t)

	ctxA := WithSessionID(context.Background(), "session_A")
	ctxB := WithSessionID(context.Background(), "session_B")

	srv.sessionFor(ctxA).recordSymbol("main.go::Foo")
	srv.sessionFor(ctxB).recordSymbol("main.go::Bar")

	snapA := srv.sessionFor(ctxA).snapshot()
	snapB := srv.sessionFor(ctxB).snapshot()

	symsA, _ := snapA["viewed_symbols"].([]string)
	symsB, _ := snapB["viewed_symbols"].([]string)

	assert.Contains(t, symsA, "main.go::Foo", "session A must see its own symbol")
	assert.NotContains(t, symsA, "main.go::Bar", "session A must NOT see session B's symbol")

	assert.Contains(t, symsB, "main.go::Bar", "session B must see its own symbol")
	assert.NotContains(t, symsB, "main.go::Foo", "session B must NOT see session A's symbol")
}

// TestServer_SessionFor_NoIDFallsBackToShared confirms that embedded
// mode (no session ID in ctx) still hits the shared default state, so
// existing single-client behavior is preserved byte-for-byte.
func TestServer_SessionFor_NoIDFallsBackToShared(t *testing.T) {
	srv, _ := setupTestServer(t)

	// No WithSessionID → fallback to shared default.
	sess := srv.sessionFor(context.Background())
	assert.Same(t, srv.session, sess,
		"ctx without session ID must route to the shared default")
}

// TestServer_TokenStatsFor_IsolatedCounters proves per-session token
// savings stay separate. Client A's record() calls must not show up in
// client B's session-level snapshot — the token_savings field in
// graph_stats would otherwise merge them and mislead each client about
// its own efficiency.
func TestServer_TokenStatsFor_IsolatedCounters(t *testing.T) {
	srv, _ := setupTestServer(t)

	ctxA := WithSessionID(context.Background(), "session_A")
	ctxB := WithSessionID(context.Background(), "session_B")

	// A records 1000 saved / 500 returned; B records 300/100.
	srv.tokenStatsFor(ctxA).record(nil, 500, 1500) // returned=500, fullFile=1500 → saved=1000
	srv.tokenStatsFor(ctxB).record(nil, 100, 400)  // returned=100, fullFile=400 → saved=300

	snapA := srv.tokenStatsFor(ctxA).snapshot()
	snapB := srv.tokenStatsFor(ctxB).snapshot()

	assert.EqualValues(t, 1000, snapA["tokens_saved"], "session A isolated counter")
	assert.EqualValues(t, 300, snapB["tokens_saved"], "session B isolated counter")
	assert.EqualValues(t, 1, snapA["calls_counted"])
	assert.EqualValues(t, 1, snapB["calls_counted"])
}
