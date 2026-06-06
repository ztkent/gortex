package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// hermeticProxyEnv points the roster + daemon socket at a temp dir so
// the toggle commands neither touch the user's real roster nor dial the
// live development daemon.
func hermeticProxyEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.toml")
	t.Setenv("GORTEX_DAEMON_SERVERS", path)
	// A socket path that cannot exist => IsRunning() is false, so the
	// best-effort live-reload dial is skipped.
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(dir, "nonexistent.sock"))
	return path
}

// TestProxyToggle_OffThenOnPersists asserts `proxy off` writes
// enabled=false (surviving a reload) and `proxy on` clears the key (back
// to default-on).
func TestProxyToggle_OffThenOnPersists(t *testing.T) {
	path := hermeticProxyEnv(t)
	roster := "[[server]]\nslug = \"r2\"\nurl = \"https://r2.example:4747\"\n"
	if err := os.WriteFile(path, []byte(roster), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runProxyToggle("r2", false); err != nil {
		t.Fatalf("proxy off: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "enabled = false") {
		t.Fatalf("proxy off should write enabled = false, got:\n%s", data)
	}
	cfg, err := daemon.LoadServersConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server[0].IsEnabled() {
		t.Fatal("a disabled remote must survive a reload as disabled")
	}

	if err := runProxyToggle("r2", true); err != nil {
		t.Fatalf("proxy on: %v", err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "enabled") {
		t.Fatalf("proxy on should clear the enabled key, got:\n%s", data)
	}
	cfg, _ = daemon.LoadServersConfig(path)
	if !cfg.Server[0].IsEnabled() {
		t.Fatal("re-enabled remote must reload as enabled")
	}
}

// TestProxyToggle_UnknownSlug asserts toggling a remote not in the roster
// is a clear error, not a silent no-op.
func TestProxyToggle_UnknownSlug(t *testing.T) {
	path := hermeticProxyEnv(t)
	if err := os.WriteFile(path, []byte("[[server]]\nslug = \"r2\"\nurl = \"https://r2.example:4747\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runProxyToggle("nope", false); err == nil {
		t.Fatal("toggling an unknown slug must error")
	}
}

// TestProxyAdd_PositionalURL asserts `proxy add <slug> <url>` persists the
// remote with the positional URL.
func TestProxyAdd_PositionalURL(t *testing.T) {
	path := hermeticProxyEnv(t)
	daemonServerAddDefault = false
	daemonServerAddAuthToken = ""
	daemonServerAddAuthTokenEnv = ""
	daemonServerAddWorkspaces = nil
	daemonServerAddReadOnly = false
	t.Cleanup(func() { daemonServerAddURL = "" })

	if err := runProxyAdd(nil, []string{"r3", "https://r3.example:4747"}); err != nil {
		t.Fatalf("proxy add: %v", err)
	}
	cfg, err := daemon.LoadServersConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Server) != 1 || cfg.Server[0].Slug != "r3" || cfg.Server[0].URL != "https://r3.example:4747" {
		t.Fatalf("proxy add did not persist the remote: %+v", cfg.Server)
	}
}
