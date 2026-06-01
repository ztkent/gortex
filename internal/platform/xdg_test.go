package platform

import (
	"path/filepath"
	"testing"
)

// clearXDG unsets every XDG base-directory variable so a test starts
// from a known clean slate; t.Setenv restores the prior value at the
// end of the test.
func clearXDG(t *testing.T) {
	t.Helper()
	for _, v := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(v, "")
	}
}

// TestHome verifies the unified per-user directory is $HOME/.gortex.
func TestHome(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got, want := Home(), filepath.Join(home, ".gortex"); got != want {
		t.Errorf("Home() = %s, want %s", got, want)
	}
}

// TestConfigDir_HonorsXDGConfigHome verifies an absolute $XDG_CONFIG_HOME
// relocates config to the standard XDG location.
func TestConfigDir_HonorsXDGConfigHome(t *testing.T) {
	clearXDG(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	want := filepath.Join(xdg, "gortex")
	if got := ConfigDir(); got != want {
		t.Errorf("ConfigDir() = %s, want %s", got, want)
	}
}

// TestConfigDir_UnsetFallback verifies the env-unset default is the
// unified $HOME/.gortex directory.
func TestConfigDir_UnsetFallback(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, ".gortex")
	if got := ConfigDir(); got != want {
		t.Errorf("ConfigDir() = %s, want %s (unified default)", got, want)
	}
}

// TestDataDir_HonorsXDGDataHome verifies an absolute $XDG_DATA_HOME
// relocates data to the standard XDG location.
func TestDataDir_HonorsXDGDataHome(t *testing.T) {
	clearXDG(t)
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	want := filepath.Join(xdg, "gortex")
	if got := DataDir(); got != want {
		t.Errorf("DataDir() = %s, want %s", got, want)
	}
}

// TestDataDir_UnsetFallback verifies the env-unset default collapses
// into the unified $HOME/.gortex directory.
func TestDataDir_UnsetFallback(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, ".gortex")
	if got := DataDir(); got != want {
		t.Errorf("DataDir() = %s, want %s (unified default)", got, want)
	}
}

// TestCacheDir_HonorsXDGCacheHome verifies an absolute $XDG_CACHE_HOME
// relocates cache to the standard XDG location.
func TestCacheDir_HonorsXDGCacheHome(t *testing.T) {
	clearXDG(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	want := filepath.Join(xdg, "gortex")
	if got := CacheDir(); got != want {
		t.Errorf("CacheDir() = %s, want %s", got, want)
	}
}

// TestCacheDir_UnsetFallback verifies the env-unset default is the
// cache/ sub-directory inside the unified ~/.gortex tree.
func TestCacheDir_UnsetFallback(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, ".gortex", "cache")
	if got := CacheDir(); got != want {
		t.Errorf("CacheDir() = %s, want %s (unified default)", got, want)
	}
}

// TestOSCacheDir_ConvergesWithCacheDir verifies OSCacheDir now resolves
// to the same place as CacheDir under both an XDG override and the
// unified default.
func TestOSCacheDir_ConvergesWithCacheDir(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := OSCacheDir(), CacheDir(); got != want {
		t.Errorf("OSCacheDir() = %s, want %s (must converge with CacheDir)", got, want)
	}
	if got, want := OSCacheDir(), filepath.Join(home, ".gortex", "cache"); got != want {
		t.Errorf("OSCacheDir() unified = %s, want %s", got, want)
	}

	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)
	if got, want := OSCacheDir(), filepath.Join(xdg, "gortex"); got != want {
		t.Errorf("OSCacheDir() with XDG_CACHE_HOME = %s, want %s", got, want)
	}
}

// TestPurposeDirs_UnsetFallback verifies the store / models / memories
// sub-directories hang off the unified ~/.gortex tree by default.
func TestPurposeDirs_UnsetFallback(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name string
		got  func() string
		want string
	}{
		{"store", StoreDir, filepath.Join(home, ".gortex", "store")},
		{"models", ModelsDir, filepath.Join(home, ".gortex", "models")},
		{"memories", MemoriesDir, filepath.Join(home, ".gortex", "memories")},
	}
	for _, tc := range cases {
		if got := tc.got(); got != tc.want {
			t.Errorf("%sDir() = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestPurposeDirs_HonorXDGDataHome verifies the purpose sub-directories
// follow an absolute $XDG_DATA_HOME into the standard XDG layout.
func TestPurposeDirs_HonorXDGDataHome(t *testing.T) {
	clearXDG(t)
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	cases := []struct {
		name string
		got  func() string
		want string
	}{
		{"store", StoreDir, filepath.Join(xdg, "gortex", "store")},
		{"models", ModelsDir, filepath.Join(xdg, "gortex", "models")},
		{"memories", MemoriesDir, filepath.Join(xdg, "gortex", "memories")},
	}
	for _, tc := range cases {
		if got := tc.got(); got != tc.want {
			t.Errorf("%sDir() = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestNonAbsoluteXDGIgnored verifies a relative XDG_*_HOME value is
// ignored, as the XDG Base Directory specification mandates — the
// resolver falls back to the unified $HOME/.gortex default instead.
func TestNonAbsoluteXDGIgnored(t *testing.T) {
	clearXDG(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name   string
		envVar string
		relVal string
		got    func() string
		want   string
	}{
		{"config", "XDG_CONFIG_HOME", "relative/config", ConfigDir, filepath.Join(home, ".gortex")},
		{"data", "XDG_DATA_HOME", "relative/data", DataDir, filepath.Join(home, ".gortex")},
		{"cache", "XDG_CACHE_HOME", "relative/cache", CacheDir, filepath.Join(home, ".gortex", "cache")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.envVar, tc.relVal)
			if got := tc.got(); got != tc.want {
				t.Errorf("%s with relative %s=%q: got %s, want %s (relative value must be ignored)",
					tc.name, tc.envVar, tc.relVal, got, tc.want)
			}
		})
	}
}
