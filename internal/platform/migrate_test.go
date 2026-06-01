package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func seed(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateToUnifiedHome verifies the old split layout folds into the
// unified ~/.gortex tree, the stale socket is left behind, and a second
// run is a no-op that doesn't clobber.
func TestMigrateToUnifiedHome(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	seed(t, filepath.Join(home, ".config", "gortex", "config.yaml"), "cfg")
	seed(t, filepath.Join(home, ".cache", "gortex", "daemon-sqlite.gob.gz"), "snap")
	seed(t, filepath.Join(home, ".cache", "gortex", "models", "gte-small", "model.onnx"), "model")
	seed(t, filepath.Join(home, ".cache", "gortex", "daemon.sock"), "sock") // ephemeral — skipped
	seed(t, filepath.Join(home, ".gortex", "store.sqlite"), "db")
	seed(t, filepath.Join(home, ".gortex", "store.sqlite-wal"), "wal")
	seed(t, filepath.Join(home, ".gortex", "memories-cache", "global", "x.json"), "mem")

	MigrateToUnifiedHome(nil)

	want := map[string]string{
		filepath.Join(home, ".gortex", "config.yaml"):                       "cfg",
		filepath.Join(home, ".gortex", "cache", "daemon-sqlite.gob.gz"):     "snap",
		filepath.Join(home, ".gortex", "models", "gte-small", "model.onnx"): "model",
		filepath.Join(home, ".gortex", "store", "store.sqlite"):             "db",
		filepath.Join(home, ".gortex", "store", "store.sqlite-wal"):         "wal",
		filepath.Join(home, ".gortex", "memories", "global", "x.json"):      "mem",
	}
	for p, w := range want {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("expected migrated file %s: %v", p, err)
			continue
		}
		if string(got) != w {
			t.Errorf("%s = %q, want %q", p, got, w)
		}
	}

	// The stale socket must NOT be carried into the unified cache.
	if _, err := os.Lstat(filepath.Join(home, ".gortex", "cache", "daemon.sock")); err == nil {
		t.Errorf("daemon.sock should have been skipped, not migrated")
	}
	// The old flat store file must have moved (not left behind).
	if _, err := os.Lstat(filepath.Join(home, ".gortex", "store.sqlite")); err == nil {
		t.Errorf("old flat store.sqlite should have moved under store/")
	}

	// Idempotent: a second run neither errors nor clobbers.
	MigrateToUnifiedHome(nil)
	if got, _ := os.ReadFile(filepath.Join(home, ".gortex", "config.yaml")); string(got) != "cfg" {
		t.Errorf("config.yaml clobbered on second migration run")
	}
}

// TestMigrateToUnifiedHome_SkipsUnderXDG verifies an explicit XDG opt-in
// makes migration a no-op — the user chose the XDG layout.
func TestMigrateToUnifiedHome_SkipsUnderXDG(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	old := filepath.Join(home, ".config", "gortex", "config.yaml")
	seed(t, old, "cfg")

	MigrateToUnifiedHome(nil)

	if _, err := os.Lstat(filepath.Join(home, ".gortex", "config.yaml")); err == nil {
		t.Errorf("migration must be a no-op when an XDG override is set")
	}
	if _, err := os.Lstat(old); err != nil {
		t.Errorf("original config must be untouched under XDG: %v", err)
	}
}
