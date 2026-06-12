package main

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

func restoreSeams() {
	isDaemonRunning = daemon.IsRunning
	spawnDaemon = spawnDetachedDaemon
	stopIntentActive = daemon.StopIntentActive
}

// isolateSpawnLock points the spawn lock + fail marker at a fresh temp
// dir per test so concurrent runs and prior fail-markers don't interfere.
func isolateSpawnLock(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	_ = os.Remove(daemon.SpawnFailMarkerPath())
	t.Cleanup(func() {
		_ = os.Remove(daemon.SpawnFailMarkerPath())
		_ = os.Remove(daemon.SpawnLockPath())
	})
}

func TestEnsureDaemon_AlreadyRunning(t *testing.T) {
	defer restoreSeams()
	var spawned int32
	isDaemonRunning = func() bool { return true }
	spawnDaemon = func() error { atomic.AddInt32(&spawned, 1); return nil }
	if d := ensureDaemonReady(true); d != daemonReady {
		t.Fatalf("want daemonReady, got %d", d)
	}
	if atomic.LoadInt32(&spawned) != 0 {
		t.Fatal("a live daemon must not be re-spawned (and no lock taken)")
	}
}

func TestEnsureDaemon_AutostartOff(t *testing.T) {
	defer restoreSeams()
	isDaemonRunning = func() bool { return false }
	spawnDaemon = func() error { t.Fatal("no spawn when autostart is off"); return nil }
	if d := ensureDaemonReady(false); d != daemonUnavailable {
		t.Fatalf("want daemonUnavailable, got %d", d)
	}
}

func TestEnsureDaemon_StopIntentSuppressesAutostart(t *testing.T) {
	defer restoreSeams()
	var spawned int32
	isDaemonRunning = func() bool { return false }
	stopIntentActive = func() bool { return true }
	spawnDaemon = func() error { atomic.AddInt32(&spawned, 1); return nil }
	// Autostart is on, but the user explicitly stopped the daemon: it must
	// stay down rather than be resurrected by the proxy's autostart path.
	if d := ensureDaemonReady(true); d != daemonUnavailable {
		t.Fatalf("stop-intent must suppress autostart => daemonUnavailable, got %d", d)
	}
	if atomic.LoadInt32(&spawned) != 0 {
		t.Fatal("a deliberately-stopped daemon must not be auto-respawned")
	}
}

func TestEnsureDaemon_RealStopIntentMarkerSuppresses(t *testing.T) {
	isolateSpawnLock(t) // points XDG_CACHE_HOME at a fresh temp dir
	defer restoreSeams()
	stopIntentActive = daemon.StopIntentActive // exercise the real FS-backed check
	var spawned int32
	isDaemonRunning = func() bool { return false }
	spawnDaemon = func() error { atomic.AddInt32(&spawned, 1); return nil }
	if err := daemon.MarkStopIntent(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.ClearStopIntent() })
	// End-to-end: the real marker write + real read must agree on the path and
	// suppress the spawn — not just the stubbed seam.
	if d := ensureDaemonReady(true); d != daemonUnavailable {
		t.Fatalf("a real stop-intent marker must suppress autostart, got %d", d)
	}
	if atomic.LoadInt32(&spawned) != 0 {
		t.Fatal("must not spawn while a real stop-intent marker is present")
	}
}

func TestEnsureDaemon_SingleFlight(t *testing.T) {
	isolateSpawnLock(t)
	defer restoreSeams()
	var running atomic.Bool
	var spawnCount atomic.Int32
	isDaemonRunning = func() bool { return running.Load() }
	spawnDaemon = func() error {
		spawnCount.Add(1)
		time.Sleep(30 * time.Millisecond) // simulate the spawn window
		running.Store(true)
		return nil
	}
	const K = 8
	var wg sync.WaitGroup
	results := make([]daemonDecision, K)
	for i := 0; i < K; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = ensureDaemonReady(true) }(i)
	}
	wg.Wait()
	if got := spawnCount.Load(); got != 1 {
		t.Fatalf("exactly one spawn across %d callers, got %d", K, got)
	}
	for i, r := range results {
		if r == daemonUnavailable {
			t.Fatalf("caller %d should not be unavailable when the spawn succeeded", i)
		}
	}
}

func TestEnsureDaemon_SpawnTimeout(t *testing.T) {
	isolateSpawnLock(t)
	defer restoreSeams()
	isDaemonRunning = func() bool { return false }
	spawnDaemon = func() error { return errors.New("spawn failed") }
	if d := ensureDaemonReady(true); d != daemonUnavailable {
		t.Fatalf("a failed spawn must yield daemonUnavailable, got %d", d)
	}
}

func TestEnsureDaemon_SpawnFailure_SingleAttempt(t *testing.T) {
	isolateSpawnLock(t)
	defer restoreSeams()
	var spawnCount atomic.Int32
	isDaemonRunning = func() bool { return false }
	spawnDaemon = func() error { spawnCount.Add(1); return errors.New("broken spawn") }
	const K = 8
	var wg sync.WaitGroup
	for i := 0; i < K; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = ensureDaemonReady(true) }()
	}
	wg.Wait()
	if got := spawnCount.Load(); got != 1 {
		t.Fatalf("a broken spawn must be attempted exactly once within the cooldown, got %d", got)
	}
}
