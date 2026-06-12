package daemon

import (
	"path/filepath"
	"testing"
)

func TestStopIntent_MarkClearRoundtrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if StopIntentActive() {
		t.Fatal("no marker should exist on a fresh state dir")
	}
	if err := MarkStopIntent(); err != nil {
		t.Fatalf("MarkStopIntent: %v", err)
	}
	if !StopIntentActive() {
		t.Fatal("marker must be active after MarkStopIntent")
	}
	// Idempotent: a second mark keeps it active, not an error.
	if err := MarkStopIntent(); err != nil {
		t.Fatalf("second MarkStopIntent: %v", err)
	}
	if !StopIntentActive() {
		t.Fatal("marker must still be active after a repeat mark")
	}
	ClearStopIntent()
	if StopIntentActive() {
		t.Fatal("marker must be gone after ClearStopIntent")
	}
	// Clearing an absent marker is a no-op, not a panic/error.
	ClearStopIntent()
	if StopIntentActive() {
		t.Fatal("clearing twice must leave the marker absent")
	}
}

func TestStopIntentMarkerPath_UnderStateDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	marker := StopIntentMarkerPath()
	if filepath.Base(marker) != "daemon.stopped" {
		t.Errorf("marker basename = %q, want daemon.stopped", filepath.Base(marker))
	}
	// Co-located with the rest of the daemon's runtime state.
	if filepath.Dir(marker) != filepath.Dir(SpawnLockPath()) {
		t.Error("stop-intent marker should be a sibling of the spawn lock")
	}
}
