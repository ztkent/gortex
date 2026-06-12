package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestOverlayManager_RegisterAndFiles is the happy-path round trip:
// register a session, push two overlays, list them via SnapshotFor.
// The snapshot must be path-sorted (the apply pass relies on stable
// ordering for reproducible drift errors) and must not alias the
// manager's internal map.
func TestOverlayManager_RegisterAndFiles(t *testing.T) {
	m := NewOverlayManager(time.Minute)
	id := m.Register("ws")
	require.NotEmpty(t, id)

	require.NoError(t, m.Push(id, OverlayFile{Path: "b.go", Content: "package b"}, nil))
	require.NoError(t, m.Push(id, OverlayFile{Path: "a.go", Content: "package a"}, nil))

	ws, files, err := m.SnapshotFor(id)
	require.NoError(t, err)
	require.Equal(t, "ws", ws)
	require.Len(t, files, 2)
	require.Equal(t, "a.go", files[0].Path, "snapshot must be path-sorted")
	require.Equal(t, "b.go", files[1].Path)

	// Snapshot must not alias the internal map.
	files[0].Content = "mutated"
	_, again, _ := m.SnapshotFor(id)
	require.Equal(t, "package a", again[0].Content, "SnapshotFor must return a deep copy")
}

// TestOverlayManager_RegisterWithID_Idempotent verifies the MCP-side
// register flow: calling RegisterWithID twice for the same (sessionID,
// workspaceID) tuple is a no-op, but a workspace mismatch is rejected
// with ErrSessionExists. Without this contract the MCP overlay_register
// tool would have to teach every editor extension to track register
// state across reconnects.
func TestOverlayManager_RegisterWithID_Idempotent(t *testing.T) {
	m := NewOverlayManager(time.Minute)
	require.NoError(t, m.RegisterWithID("sess-1", "ws-a"))
	require.NoError(t, m.RegisterWithID("sess-1", "ws-a"), "idempotent re-register must succeed")

	err := m.RegisterWithID("sess-1", "ws-b")
	require.ErrorIs(t, err, ErrSessionExists, "workspace mismatch must surface ErrSessionExists")

	// Empty session ID is a programming error.
	require.Error(t, m.RegisterWithID("", "ws-a"))
}

// TestOverlayManager_HasAndFileCount covers the dispatcher's fast-path
// gating: tools/call middleware bails before any apply work when
// Has==false or FileCount==0.
func TestOverlayManager_HasAndFileCount(t *testing.T) {
	m := NewOverlayManager(time.Minute)
	require.False(t, m.Has("unknown"))
	require.Zero(t, m.FileCount("unknown"))

	id := m.Register("ws")
	require.True(t, m.Has(id))
	require.Zero(t, m.FileCount(id), "freshly registered session has no files")

	require.NoError(t, m.Push(id, OverlayFile{Path: "x.go", Content: "x"}, nil))
	require.Equal(t, 1, m.FileCount(id))

	m.Drop(id)
	require.False(t, m.Has(id))
	require.Zero(t, m.FileCount(id))
}

// TestOverlayManager_DriftCheck verifies that Push surfaces a drift
// error when the supplied BaseSHA disagrees with the on-disk SHA
// reported by the callback. Without drift detection two clients
// pushing stale overlays against the same path would silently corrupt
// each other's query results.
func TestOverlayManager_DriftCheck(t *testing.T) {
	m := NewOverlayManager(time.Minute)
	id := m.Register("ws")

	// BaseSHA matches: push succeeds.
	require.NoError(t, m.Push(id,
		OverlayFile{Path: "x.go", Content: "x", BaseSHA: "abc"},
		func(path, sha string) bool { return sha == "abc" },
	))

	// BaseSHA mismatches: ErrOverlayDrift.
	err := m.Push(id,
		OverlayFile{Path: "x.go", Content: "x", BaseSHA: "stale"},
		func(path, sha string) bool { return sha == "abc" },
	)
	require.True(t, errors.Is(err, ErrOverlayDrift))
}

// TestOverlayManager_SweepIdleHonoursTTL ensures that sessions older
// than IdleTTL are reaped. A crashed editor extension leaving overlays
// in the daemon would otherwise pin memory until restart.
func TestOverlayManager_SweepIdleHonoursTTL(t *testing.T) {
	m := NewOverlayManager(20 * time.Millisecond)
	id := m.Register("ws")
	require.True(t, m.Has(id))

	time.Sleep(40 * time.Millisecond)
	dropped := m.SweepIdle()
	require.Equal(t, 1, dropped, "session past idleTTL must be reaped")
	require.False(t, m.Has(id))
}

func TestOverlayManager_StartJanitorSweepsAndStops(t *testing.T) {
	m := NewOverlayManager(20 * time.Millisecond)
	id := m.Register("ws")
	require.True(t, m.Has(id))

	swept := make(chan int, 1)
	stop := m.StartJanitor(10*time.Millisecond, func(dropped int) {
		select {
		case swept <- dropped:
		default:
		}
	})
	defer stop()

	select {
	case dropped := <-swept:
		require.Equal(t, 1, dropped, "janitor must reap the idle session")
	case <-time.After(2 * time.Second):
		t.Fatal("janitor never swept the idle session")
	}
	require.False(t, m.Has(id))

	// stop is idempotent and terminates the goroutine.
	stop()
	stop()
}

func TestOverlayManager_StartJanitorDisabledTTL(t *testing.T) {
	m := NewOverlayManager(0)
	stop := m.StartJanitor(time.Millisecond, func(int) {
		t.Error("janitor must not run when expiry is disabled")
	})
	stop()
	stop()
}

// TestOverlayManager_SnapshotForBumpsLastUsed proves the load-bearing
// "reads count as activity" guarantee: a session that only queries
// (no further Push) keeps its lease alive as long as the view-build
// path keeps calling SnapshotFor. Without this a long sequence of
// tool calls without intervening pushes would let the TTL trip the
// session in active use.
func TestOverlayManager_SnapshotForBumpsLastUsed(t *testing.T) {
	m := NewOverlayManager(60 * time.Millisecond)
	id := m.Register("ws")
	require.NoError(t, m.Push(id, OverlayFile{Path: "x.go", Content: "x"}, nil))

	// Two TTL/3 sleeps with a SnapshotFor in between — total span >
	// TTL — but the read bumps LastUsed so the session survives.
	time.Sleep(30 * time.Millisecond)
	_, _, err := m.SnapshotFor(id)
	require.NoError(t, err, "SnapshotFor must bump LastUsed")
	time.Sleep(30 * time.Millisecond)
	require.True(t, m.Has(id), "session must survive when reads bump LastUsed")

	// Now truly idle: skip the read, sleep past TTL, sweep — should
	// be reaped.
	time.Sleep(80 * time.Millisecond)
	dropped := m.SweepIdle()
	require.Equal(t, 1, dropped)
	require.False(t, m.Has(id))
}

// TestOverlayManager_TouchExtendsLease verifies the keepalive
// primitive: Touch refreshes LastUsed without altering files.
func TestOverlayManager_TouchExtendsLease(t *testing.T) {
	m := NewOverlayManager(40 * time.Millisecond)
	id := m.Register("ws")
	require.NoError(t, m.Push(id, OverlayFile{Path: "y.go", Content: "y"}, nil))

	time.Sleep(25 * time.Millisecond)
	require.NoError(t, m.Touch(id))
	time.Sleep(25 * time.Millisecond)
	require.True(t, m.Has(id), "Touch must keep the session alive past the original TTL window")

	// Touch on unknown session reports a structured error.
	require.ErrorIs(t, m.Touch("nope"), ErrSessionNotFound)
}

// TestOverlayManager_StatusForDoesNotBumpLastUsed makes sure the
// liveness-query path is read-only: polling overlay_list every
// second must NOT extend the lease (otherwise a misconfigured
// editor could keep a dropped session alive forever just by
// polling).
func TestOverlayManager_StatusForDoesNotBumpLastUsed(t *testing.T) {
	m := NewOverlayManager(40 * time.Millisecond)
	id := m.Register("ws")
	require.NoError(t, m.Push(id, OverlayFile{Path: "z.go", Content: "z"}, nil))

	// Poll status every 10ms; total elapsed > TTL. Without
	// LastUsed-bump on StatusFor, the sweeper still reaps.
	for i := 0; i < 6; i++ {
		_, err := m.StatusFor(id)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}
	dropped := m.SweepIdle()
	require.Equal(t, 1, dropped, "StatusFor must NOT bump LastUsed; sweep should reap the idle session")
	require.False(t, m.Has(id))
}

// TestOverlayManager_StatusForReportsExpiryMetadata: overlay_list
// surfaces this metadata so editor extensions can schedule
// keepalive proactively.
func TestOverlayManager_StatusForReportsExpiryMetadata(t *testing.T) {
	m := NewOverlayManager(2 * time.Minute)
	id := m.Register("ws-alpha")
	st, err := m.StatusFor(id)
	require.NoError(t, err)
	require.Equal(t, "ws-alpha", st.WorkspaceID)
	require.False(t, st.Created.IsZero())
	require.False(t, st.LastUsed.IsZero())
	require.InDelta(t, 0, st.IdleSeconds, 1.0, "newly-registered session has near-zero idle time")
	require.InDelta(t, 120, st.IdleTTLSeconds, 0.5)
	require.False(t, st.ExpiresAt.IsZero())
	require.True(t, st.ExpiresAt.After(time.Now()), "expires_at must be in the future")
}

// TestOverlayIdleTTLFromEnv covers the three branches of the
// configuration resolution: explicit override > env var > default.
// t.Setenv auto-restores the original value on test completion, so
// the test never leaks env state into sibling tests in the package.
func TestOverlayIdleTTLFromEnv(t *testing.T) {
	// Explicit non-zero override wins over env.
	t.Setenv("GORTEX_OVERLAY_IDLE_TTL", "1h")
	require.Equal(t, 7*time.Minute, OverlayIdleTTLFromEnv(7*time.Minute))

	// Env var (no override).
	t.Setenv("GORTEX_OVERLAY_IDLE_TTL", "45m")
	require.Equal(t, 45*time.Minute, OverlayIdleTTLFromEnv(0))

	// Garbage env: fall through to default (don't fail startup).
	t.Setenv("GORTEX_OVERLAY_IDLE_TTL", "garbage")
	require.Equal(t, DefaultOverlayIdleTTL, OverlayIdleTTLFromEnv(0))

	// Empty env (t.Setenv to "" is the documented way to model "unset"
	// in a test-scoped way): default.
	t.Setenv("GORTEX_OVERLAY_IDLE_TTL", "")
	require.Equal(t, DefaultOverlayIdleTTL, OverlayIdleTTLFromEnv(0))
}
