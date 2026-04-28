package hooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withEditBlocking flips GORTEX_HOOK_BLOCK_EDIT for one test, and
// restores the previous value (including unset) on cleanup. Avoids
// leaking state between tests run in parallel via t.Cleanup.
func withEditBlocking(t *testing.T, on bool) {
	t.Helper()
	t.Setenv(editBlockingEnvVar, map[bool]string{true: "1", false: ""}[on])
}

// fakeIndexedBridge stands up an HTTP server matching the bridge's
// /api/graph/file shape so queryFileIndexed treats the named file as
// indexed.
func fakeIndexedBridge(t *testing.T, indexedPaths map[string]bool) (port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graph/file" {
			http.NotFound(w, r)
			return
		}
		path := r.URL.Query().Get("path")
		if !indexedPaths[path] {
			_, _ = w.Write([]byte(`{"nodes":[]}`))
			return
		}
		// Emit two nodes so queryFileIndexed sees ≥2 (file node + one).
		_, _ = w.Write([]byte(`{"nodes":[{"id":"file"},{"id":"sym"}]}`))
	}))
	t.Cleanup(srv.Close)
	return portFromURL(t, srv.URL)
}

func TestEnrichEdit_Disabled_NoOp(t *testing.T) {
	withEditBlocking(t, false)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/foo.go": true})

	result := enrichEdit(map[string]any{"file_path": "/repo/foo.go"}, port)
	if result.deny || result.context != "" {
		t.Errorf("disabled ⇒ silent; got deny=%v ctx=%q", result.deny, result.context)
	}
}

func TestEnrichEdit_NonSource_PassThrough(t *testing.T) {
	withEditBlocking(t, true)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/README.md": true})

	result := enrichEdit(map[string]any{"file_path": "/repo/README.md"}, port)
	if result.deny || result.context != "" {
		t.Errorf("non-source ⇒ pass through; got deny=%v ctx=%q", result.deny, result.context)
	}
}

func TestEnrichEdit_NotIndexed_PassThrough(t *testing.T) {
	withEditBlocking(t, true)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/other.go": true})

	result := enrichEdit(map[string]any{"file_path": "/repo/foo.go"}, port)
	if result.deny || result.context != "" {
		t.Errorf("unindexed source ⇒ pass through; got deny=%v ctx=%q", result.deny, result.context)
	}
}

func TestEnrichEdit_IndexedSource_Denies(t *testing.T) {
	withEditBlocking(t, true)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/foo.go": true})

	result := enrichEdit(map[string]any{"file_path": "/repo/foo.go"}, port)
	if !result.deny {
		t.Fatal("expected deny for Edit on indexed source")
	}
	for _, want := range []string{"BLOCKED", "edit_symbol", "edit_file", "rename_symbol"} {
		if !strings.Contains(result.reason, want) {
			t.Errorf("reason missing %q:\n%s", want, result.reason)
		}
	}
}

func TestEnrichWrite_IndexedSource_Denies(t *testing.T) {
	withEditBlocking(t, true)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/server.go": true})

	result := enrichWrite(map[string]any{"file_path": "/repo/server.go"}, port)
	if !result.deny {
		t.Fatal("expected deny for Write on indexed source")
	}
	if !strings.Contains(result.reason, "write_file") {
		t.Errorf("reason should mention write_file, got:\n%s", result.reason)
	}
}

func TestEnrichWrite_NewFile_PassThrough(t *testing.T) {
	withEditBlocking(t, true)
	port := fakeIndexedBridge(t, map[string]bool{}) // nothing indexed

	result := enrichWrite(map[string]any{"file_path": "/repo/new.go"}, port)
	if result.deny || result.context != "" {
		t.Errorf("new file ⇒ pass through; got deny=%v ctx=%q", result.deny, result.context)
	}
}

func TestEnrichEdit_DispatchedFromEnrich(t *testing.T) {
	withEditBlocking(t, true)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/x.go": true})

	input := HookInput{ToolName: "Edit", ToolInput: map[string]any{"file_path": "/repo/x.go"}}
	result := enrich(input, port)
	if !result.deny {
		t.Errorf("dispatcher must route Edit to enrichEdit; got deny=%v", result.deny)
	}
}

func TestEditBlockingEnabled_Variants(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"no", false},
		{"off", false},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"on", true},
		{"anything-else", true},
	}
	for _, c := range cases {
		t.Setenv(editBlockingEnvVar, c.val)
		got := editBlockingEnabled()
		if got != c.want {
			t.Errorf("editBlockingEnabled(%q) = %v, want %v", c.val, got, c.want)
		}
	}
}

func TestEnrichEdit_ReturnsValidJSONWhenWrappedByDispatcher(t *testing.T) {
	// Sanity: runPreToolUse must produce well-formed deny JSON when
	// the underlying enrichEdit denies. Catches future regressions
	// where a struct tag drift breaks the wire format.
	withEditBlocking(t, true)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/repo/handler.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port) })
	if out == "" {
		t.Fatal("expected JSON output for deny path")
	}
	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if dec.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected permissionDecision=deny, got %q", dec.HookSpecificOutput.PermissionDecision)
	}
}
