package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Stop-intent marker.
//
// `gortex daemon stop` is a user's explicit "stay down" signal. But the
// daemon is auto-startable: any `gortex mcp` proxy (the process an editor /
// AI agent keeps alive) re-runs the autostart path on its next launch and
// would immediately respawn the daemon the user just stopped. The stop-intent
// marker records that intent so the autostart single-flight (ensureDaemonReady)
// declines to resurrect a deliberately-stopped daemon.
//
// Unlike the spawn-fail marker, this one is sticky — it has no TTL and is
// cleared only by an explicit `gortex daemon start` / `restart`. Suppressing
// autostart does not break `gortex mcp`: a suppressed proxy falls back to the
// embedded in-process server exactly as it does under GORTEX_AUTOSTART=0.

// StopIntentMarkerPath returns the sentinel file recording an explicit
// `daemon stop`. Co-located with the socket / PID / spawn-lock under the
// per-user state dir so it shares the daemon's lifecycle directory.
func StopIntentMarkerPath() string {
	if dir, ok := stateDir(); ok {
		return filepath.Join(dir, "daemon.stopped")
	}
	return filepath.Join(os.TempDir(), "gortex-daemon.stopped")
}

// MarkStopIntent records that the user explicitly stopped the daemon, so the
// autostart path will not respawn it until an explicit start clears the mark.
// The marker content is the stamp time (nanos) for debuggability; only its
// presence is load-bearing.
func MarkStopIntent() error {
	path := StopIntentMarkerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600)
}

// ClearStopIntent removes the stop-intent marker, re-enabling autostart. An
// explicit `daemon start` / `restart` is the user (or supervisor) asking for a
// running daemon, which supersedes a prior stop. Absent marker is a no-op.
func ClearStopIntent() {
	_ = os.Remove(StopIntentMarkerPath())
}

// StopIntentActive reports whether the user has an outstanding `daemon stop`
// that no subsequent start has cleared.
func StopIntentActive() bool {
	_, err := os.Stat(StopIntentMarkerPath())
	return err == nil
}
