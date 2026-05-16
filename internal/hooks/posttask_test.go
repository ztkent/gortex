package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRunPostTask_RejectsWrongEvent(t *testing.T) {
	data := []byte(`{"hook_event_name":"PreToolUse"}`)
	out := captureStdout(t, func() { runPostTask(data, 0) })
	if out != "" {
		t.Errorf("expected silent no-op for wrong event, got: %q", out)
	}
}

func TestRunPostTask_StopHookActive_Skips(t *testing.T) {
	// stop_hook_active=true means we're already inside a Stop-hook loop;
	// firing again would recurse.
	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":true}`)
	out := captureStdout(t, func() { runPostTask(data, 1) })
	if out != "" {
		t.Errorf("expected no output when stop_hook_active, got: %q", out)
	}
}

func TestRunPostTask_NoBridge(t *testing.T) {
	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, 1) })
	if out != "" {
		t.Errorf("expected no output when bridge unreachable, got: %q", out)
	}
}

func TestRunPostTask_NoChanges_Silent(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":[],"changed_symbols":[],"risk":"NONE","summary":"no indexed symbols affected"}`,
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })
	if out != "" {
		t.Errorf("expected silent no-op when nothing changed, got: %q", out)
	}
}

func TestRunPostTask_RendersDiagnostics(t *testing.T) {
	changedJSON := `{
		"changed_files":["internal/foo.go","internal/bar.go"],
		"changed_symbols":[
			{"id":"internal/foo.go::Foo","name":"Foo","kind":"function"},
			{"id":"internal/bar.go::Bar","name":"Bar","kind":"method"}
		],
		"risk":"MEDIUM",
		"summary":"2 symbols touched"
	}`
	srv := newFakeServer(map[string]string{
		"detect_changes":   changedJSON,
		"get_test_targets": "internal/foo_test.go::TestFoo\ninternal/bar_test.go::TestBar",
		"check_guards":     "boundary my-rule cross-layer import violates ui→db\n",
		"analyze":          "function Orphan internal/foo.go::Foo unused fan_in=0\n",
		"contracts":        "orphan provider http::GET::/api/ghost (no consumer matched)\n",
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, port) })

	if out == "" {
		t.Fatal("expected diagnostic output when changes are present")
	}
	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output not valid HookOutput JSON: %v\n%s", err, out)
	}
	if payload.HookSpecificOutput == nil || payload.HookSpecificOutput.HookEventName != "Stop" {
		t.Fatal("hookSpecificOutput missing or wrong event name")
	}

	ac := payload.HookSpecificOutput.AdditionalContext
	mustContain := []string{
		"Post-Task Diagnostics",
		"risk `MEDIUM`",
		"Tests to Run",
		"internal/foo_test.go::TestFoo",
		"Guard Violations",
		"boundary my-rule",
		"Potential Dead Code",
		"internal/foo.go::Foo", // only the changed-symbol intersection
		"API Contract Issues",
		"orphan provider",
	}
	for _, frag := range mustContain {
		if !strings.Contains(ac, frag) {
			t.Errorf("briefing missing %q\n---\n%s", frag, ac)
		}
	}
}

func TestRunPostTask_DeadCodeFiltersToChanged(t *testing.T) {
	// Dead-code results that don't overlap with changed symbols should
	// NOT be included — we only flag what the current task left orphaned.
	changedJSON := `{
		"changed_files":["foo.go"],
		"changed_symbols":[{"id":"foo.go::Foo","name":"Foo","kind":"function"}],
		"risk":"LOW","summary":"1 symbol"
	}`
	srv := newFakeServer(map[string]string{
		"detect_changes":   changedJSON,
		"get_test_targets": "",
		"check_guards":     "",
		"analyze":          "function SomethingElse unrelated/path.go fan_in=0\n",
		"contracts":        "no contract issues\n",
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if strings.Contains(out, "Potential Dead Code") {
		t.Errorf("dead-code section should be omitted when no overlap with changed symbols:\n%s", out)
	}
	if strings.Contains(out, "API Contract Issues") {
		t.Errorf("contract section should be omitted when clean:\n%s", out)
	}
}

func TestDispatch_RoutesStop(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runFromBytes(t, data, port) })
	if !strings.Contains(out, "Post-Task Diagnostics") {
		t.Errorf("Run did not route to PostTask handler:\n%s", out)
	}
}

// runFromBytes feeds raw bytes into Run() by temporarily swapping stdin.
func runFromBytes(t *testing.T, data []byte, port int) {
	t.Helper()
	withStdin(t, data, func() { Run(port, ModeDeny) })
}
