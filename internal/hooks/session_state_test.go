package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withSessionDir points the per-session state store at a fresh
// t.TempDir() for one test, so session files never touch the user's
// real cache directory.
func withSessionDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(hookSessionDirEnvVar, dir)
	return dir
}

func TestSessionState_RoundTrip(t *testing.T) {
	dir := withSessionDir(t)

	// A fresh session loads as the zero value.
	if got := loadSessionState("sess-1"); got.GraphConsulted {
		t.Fatalf("fresh session should be zero value, got %+v", got)
	}

	saveSessionState("sess-1", sessionState{GraphConsulted: true})

	got := loadSessionState("sess-1")
	if !got.GraphConsulted {
		t.Errorf("expected GraphConsulted=true after save, got %+v", got)
	}

	// The file lands inside the redirected directory.
	if _, err := os.Stat(filepath.Join(dir, "sess-1.json")); err != nil {
		t.Errorf("expected session file on disk: %v", err)
	}
}

func TestSessionState_DistinctSessions(t *testing.T) {
	withSessionDir(t)
	saveSessionState("a", sessionState{GraphConsulted: true})
	saveSessionState("b", sessionState{GraphConsulted: false})

	if !loadSessionState("a").GraphConsulted {
		t.Error("session a should have its own state")
	}
	if loadSessionState("b").GraphConsulted {
		t.Error("session b state must not bleed from session a")
	}
}

func TestSessionState_EmptySessionID_NoOp(t *testing.T) {
	withSessionDir(t)
	// Empty ID: save is a no-op, load returns zero — never an error.
	saveSessionState("", sessionState{GraphConsulted: true})
	if loadSessionState("").GraphConsulted {
		t.Error("empty session ID must not persist state")
	}
}

func TestSessionState_CorruptFile_DegradesToZero(t *testing.T) {
	dir := withSessionDir(t)
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadSessionState("broken"); got.GraphConsulted {
		t.Errorf("corrupt state file should degrade to zero value, got %+v", got)
	}
}

func TestSessionState_MissingDir_NoPanic(t *testing.T) {
	// Point at a path whose parent does not exist; save must create it
	// and not panic, load of a still-absent file returns zero.
	dir := filepath.Join(t.TempDir(), "nested", "deeper")
	t.Setenv(hookSessionDirEnvVar, dir)

	if got := loadSessionState("x"); got.GraphConsulted {
		t.Errorf("missing dir should load as zero value, got %+v", got)
	}
	saveSessionState("x", sessionState{GraphConsulted: true})
	if !loadSessionState("x").GraphConsulted {
		t.Error("save should create the nested directory and persist")
	}
}

func TestSanitizeSessionID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc123", "abc123"},
		{"a-b_c.d", "a-b_c.d"},
		{"  spaced  ", "spaced"},
		{"with/slash", "with_slash"},
		{"../traversal", ".._traversal"},
		{"a b c", "a_b_c"},
		{"", ""},
		{".", ""},
		{"..", ""},
		{"weird*chars$here", "weird_chars_here"},
	}
	for _, c := range cases {
		if got := sanitizeSessionID(c.in); got != c.want {
			t.Errorf("sanitizeSessionID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSessionStatePath_RejectsTraversal(t *testing.T) {
	dir := withSessionDir(t)
	// A traversal-shaped ID must resolve to a path that stays inside
	// the session directory — the separators are sanitized to '_', so
	// the path can never climb out.
	p := sessionStatePath("../../etc/passwd")
	if p == "" {
		t.Fatal("expected a non-empty sanitized path")
	}
	if !strings.HasPrefix(p, dir+string(os.PathSeparator)) {
		t.Errorf("sanitized path %q escaped the session dir %q", p, dir)
	}
	// The file segment must be a single component — no separators, no
	// ".." path element survives sanitization. (A literal ".." byte
	// pair is harmless once it can't act as a directory step.)
	segment := strings.TrimPrefix(p, dir+string(os.PathSeparator))
	if strings.ContainsRune(segment, os.PathSeparator) {
		t.Errorf("sanitized file segment %q must not contain a path separator", segment)
	}
	if filepath.Clean(p) != p {
		t.Errorf("sanitized path %q is not already clean — a traversal element survived", p)
	}
}
