package main

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

func restoreServiceSeams() {
	serviceActive = defaultServiceActive
	serviceStop = defaultServiceStop
	serviceRestart = defaultServiceRestart
}

func TestRunDaemonStop_SupervisedRoutesToServiceStop(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	defer restoreServiceSeams()

	var stopCalls int32
	serviceActive = func() bool { return true }
	serviceStop = func(io.Writer) error { atomic.AddInt32(&stopCalls, 1); return nil }
	serviceRestart = func(io.Writer) error { t.Fatal("restart must not be called by stop"); return nil }

	if err := runDaemonStop(&cobra.Command{}, nil); err != nil {
		t.Fatalf("runDaemonStop: %v", err)
	}
	if got := atomic.LoadInt32(&stopCalls); got != 1 {
		t.Fatalf("serviceStop calls = %d, want 1 (supervised stop must route through the supervisor)", got)
	}
	// The stay-down mark is still recorded so a stray `gortex mcp` proxy
	// doesn't autostart a second, unsupervised daemon either.
	if !daemon.StopIntentActive() {
		t.Fatal("supervised stop must still record stop-intent")
	}
}

func TestRunDaemonRestart_SupervisedRoutesToServiceRestart(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	defer restoreServiceSeams()

	var restartCalls int32
	serviceActive = func() bool { return true }
	serviceRestart = func(io.Writer) error { atomic.AddInt32(&restartCalls, 1); return nil }
	serviceStop = func(io.Writer) error { t.Fatal("restart must not stop the supervisor"); return nil }

	if err := runDaemonRestart(&cobra.Command{}, nil); err != nil {
		t.Fatalf("runDaemonRestart: %v", err)
	}
	if got := atomic.LoadInt32(&restartCalls); got != 1 {
		t.Fatalf("serviceRestart calls = %d, want 1 (supervised restart must bounce via the supervisor)", got)
	}
}

func TestDefaultServiceActive_NoUnitFileIsInactive(t *testing.T) {
	// Point HOME (and cache) at an empty dir so no unit file exists; detection
	// must short-circuit to false without shelling out to the supervisor.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	if defaultServiceActive() {
		t.Fatal("no installed unit file => service must be reported inactive")
	}
}

func TestLaunchdTemplate_RestartsOnlyOnFailure(t *testing.T) {
	// KeepAlive must be the SuccessfulExit=false policy, not bare <true/>, so a
	// clean `daemon stop` is not resurrected on macOS.
	if !strings.Contains(launchdPlistTemplate, "<key>SuccessfulExit</key>") {
		t.Error("launchd plist must use the KeepAlive SuccessfulExit policy")
	}
	if strings.Contains(launchdPlistTemplate, "<key>KeepAlive</key>\n    <true/>") {
		t.Error("launchd plist must not use unconditional KeepAlive=true")
	}
}

func TestSystemdTemplate_RestartsOnlyOnFailure(t *testing.T) {
	if !strings.Contains(systemdUnitTemplate, "Restart=on-failure") {
		t.Error("systemd unit must restart only on failure so a clean stop sticks")
	}
}
