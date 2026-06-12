package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

// The stop-intent contract is two-sided: `daemon stop` writes a sticky marker
// and an explicit `daemon start` clears it. If the clear ever regresses, a
// single `daemon stop` would suppress autostart forever — worse than the
// original bug — so guard both sides directly.

func TestRunDaemonStart_ClearsStopIntent(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	defer restoreSeams()

	if err := daemon.MarkStopIntent(); err != nil {
		t.Fatal(err)
	}
	// Short-circuit start at the "already running" guard, which sits right
	// after the clear — so no real serve loop is needed.
	isDaemonRunning = func() bool { return true }
	_ = runDaemonStart(&cobra.Command{}, nil)

	if daemon.StopIntentActive() {
		t.Fatal("an explicit `daemon start` must clear the stop-intent marker")
	}
}

func TestRunDaemonStart_AutostartChildDoesNotClearStopIntent(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("GORTEX_DAEMON_CHILD", "1")
	defer restoreSeams()

	if err := daemon.MarkStopIntent(); err != nil {
		t.Fatal(err)
	}
	isDaemonRunning = func() bool { return true }
	_ = runDaemonStart(&cobra.Command{}, nil)

	// The autostart-spawned child must not erase a stop-intent the user may
	// have written after the autostart decision (the TOCTOU window).
	if !daemon.StopIntentActive() {
		t.Fatal("autostart child must NOT clear the stop-intent marker")
	}
}

func TestRunDaemonStop_UnsupervisedRecordsStopIntent(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	defer restoreServiceSeams()

	// The reporter's path: no service supervisor installed.
	serviceActive = func() bool { return false }
	// Daemon already down → stop returns via the already-down path after
	// recording intent; no real daemon required.
	if err := runDaemonStop(&cobra.Command{}, nil); err != nil {
		t.Fatalf("runDaemonStop: %v", err)
	}
	if !daemon.StopIntentActive() {
		t.Fatal("an unsupervised `daemon stop` must record stop-intent")
	}
}
