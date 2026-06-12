package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gofrs/flock"

	"github.com/zzet/gortex/internal/daemon"
)

// daemonDecision is the resolved auto-start outcome.
type daemonDecision int

const (
	daemonReady       daemonDecision = iota // socket was already live
	daemonAutostarted                       // we (or a peer) brought it up; socket live
	daemonUnavailable                       // autostart off, or spawn failed/timed out
)

const (
	// spawnLockTimeout must outlast spawnDetachedDaemon's readiness wait
	// (~60s) so a loser blocked on the lock is waited out, not contended
	// into a second spawn.
	spawnLockTimeout = 65 * time.Second
	// spawnFailCooldown bounds serial retries of a broken spawn: within
	// the window at most one spawn attempt runs across contending callers.
	spawnFailCooldown = 5 * time.Second
)

// Injectable seams so the race/fallback/spawn-failure branches are
// testable without a real daemon.
var (
	isDaemonRunning  = daemon.IsRunning
	spawnDaemon      = spawnDetachedDaemon
	stopIntentActive = daemon.StopIntentActive
)

// resolveDaemonDecision probes the socket and, when auto-start is enabled
// and no daemon is up, single-flights a spawn. It never returns an error;
// an unrecoverable state collapses to daemonUnavailable so the caller
// falls back to the embedded server.
func resolveDaemonDecision() daemonDecision {
	return ensureDaemonReady(daemon.ParseAutostart())
}

// ensureDaemonReady is the lock-protected single-flight critical section,
// shared by `gortex mcp` and `gortex track`.
func ensureDaemonReady(autostart bool) daemonDecision {
	if isDaemonRunning() {
		return daemonReady
	}
	if !autostart {
		return daemonUnavailable
	}
	// Respect an explicit `daemon stop`: do not resurrect a daemon the user
	// deliberately stopped. The mark is cleared by `daemon start` / `restart`.
	// A suppressed `gortex mcp` falls back to the embedded server, so this
	// declines the background daemon without breaking the tool surface.
	if stopIntentActive() {
		return daemonUnavailable
	}

	lockPath := daemon.SpawnLockPath()
	_ = os.MkdirAll(filepath.Dir(lockPath), 0o700)
	lock := flock.New(lockPath)

	ctx, cancel := context.WithTimeout(context.Background(), spawnLockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil || !locked {
		// Couldn't acquire the lock in time. The winner may have brought
		// the socket up while we waited — re-probe before giving up.
		if isDaemonRunning() {
			return daemonReady
		}
		return daemonUnavailable
	}
	defer func() { _ = lock.Unlock() }()

	// Re-probe inside the lock: a peer may have won the race while we
	// blocked on the lock.
	if isDaemonRunning() {
		return daemonReady
	}
	// A recent failed spawn within the cooldown — skip our own attempt so
	// K callers don't serially retry a broken spawn.
	if spawnFailedRecently() {
		return daemonUnavailable
	}
	if err := spawnDaemon(); err != nil {
		stampSpawnFailure()
		return daemonUnavailable
	}
	return daemonAutostarted
}

func spawnFailedRecently() bool {
	info, err := os.Stat(daemon.SpawnFailMarkerPath())
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < spawnFailCooldown
}

func stampSpawnFailure() {
	path := daemon.SpawnFailMarkerPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600)
}
