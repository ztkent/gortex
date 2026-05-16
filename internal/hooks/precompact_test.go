package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestRunPreCompact_RejectsWrongEvent(t *testing.T) {
	// Should be a no-op for PreToolUse payload.
	data := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read"}`)
	out := captureStdout(t, func() { runPreCompact(data, 0) })
	if out != "" {
		t.Errorf("expected silent no-op, got: %q", out)
	}
}

func TestRunPreCompact_NoBridge(t *testing.T) {
	// Port 1 is guaranteed to be closed. Hook must fail silently.
	data := []byte(`{"hook_event_name":"PreCompact","session_id":"s","trigger":"auto"}`)
	out := captureStdout(t, func() { runPreCompact(data, 1) })
	if out != "" {
		t.Errorf("expected no output when bridge unreachable, got: %q", out)
	}
}

func TestRunPreCompact_RendersBriefing(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"graph_stats": `{"total_nodes":4500,"total_edges":47000,"by_language":{"go":3000,"typescript":400,"markdown":500}}`,
		"get_symbol_history": "method Server.handleBatchEdit internal/mcp/tools_enhancements.go:1200 (edits=3, CHURNING)\n" +
			"function renderContextMarkdown internal/mcp/tools_enhancements.go:1790 (edits=1)",
		"analyze": "method Graph.AddNode internal/graph/graph.go fan_in=42 fan_out=3\n" +
			"function New internal/indexer/indexer.go fan_in=31 fan_out=5",
		"feedback": "pkg/server.go::HandleRequest useful=12 not_needed=1",
	})
	defer srv.Close()

	port := portFromURL(t, srv.URL)
	data := []byte(`{"hook_event_name":"PreCompact","session_id":"s","trigger":"auto"}`)
	out := captureStdout(t, func() { runPreCompact(data, port) })

	if out == "" {
		t.Fatal("expected output when bridge is reachable")
	}

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output is not valid HookOutput JSON: %v\n%s", err, out)
	}
	if payload.HookSpecificOutput == nil {
		t.Fatal("hookSpecificOutput missing")
	}
	if payload.HookSpecificOutput.HookEventName != "PreCompact" {
		t.Errorf("wrong hookEventName: %q", payload.HookSpecificOutput.HookEventName)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "Gortex PreCompact Snapshot") {
		t.Errorf("briefing missing header:\n%s", ac)
	}
	if !strings.Contains(ac, "4500 nodes, 47000 edges") {
		t.Errorf("briefing missing graph stats:\n%s", ac)
	}
	if !strings.Contains(ac, "handleBatchEdit") {
		t.Errorf("briefing missing recent modifications:\n%s", ac)
	}
	if !strings.Contains(ac, "Graph.AddNode") {
		t.Errorf("briefing missing hotspots:\n%s", ac)
	}
	if !strings.Contains(ac, "HandleRequest") {
		t.Errorf("briefing missing feedback ranking:\n%s", ac)
	}
}

func TestDispatch_RoutesPreCompact(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"graph_stats": `{"total_nodes":1,"total_edges":0,"by_language":{"go":1}}`,
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	data := []byte(`{"hook_event_name":"PreCompact","session_id":"s","trigger":"auto"}`)
	old := os.Stdin
	defer func() { os.Stdin = old }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.Write(data)
		_ = w.Close()
	}()
	os.Stdin = r

	out := captureStdout(t, func() { Run(port, ModeDeny) })
	if !strings.Contains(out, "Gortex PreCompact Snapshot") {
		t.Errorf("Run did not route to PreCompact handler:\n%s", out)
	}
}

func TestDispatch_UnknownEventSilent(t *testing.T) {
	data := []byte(`{"hook_event_name":"UserPromptSubmit"}`)
	old := os.Stdin
	defer func() { os.Stdin = old }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.Write(data)
		_ = w.Close()
	}()
	os.Stdin = r

	out := captureStdout(t, func() { Run(1, ModeDeny) })
	if out != "" {
		t.Errorf("expected silent no-op for unknown event, got: %q", out)
	}
}

// ---- helpers ----

// newFakeServer returns a test HTTP server that mimics /v1/tools/{name}
// responses. `toolResponses` maps tool name to the raw `text` field of the
// first content block.
func newFakeServer(toolResponses map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/tools/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/tools/")
		text, ok := toolResponses[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		resp := map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		}
		body, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

func portFromURL(t *testing.T, u string) int {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse url %q: %v", u, err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse port from %q: %v", u, err)
	}
	return port
}

// captureStdout runs fn with os.Stdout redirected to a pipe, returning what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()
	_ = w.Close()
	os.Stdout = old
	<-done
	return buf.String()
}

// withStdin runs fn with os.Stdin swapped to a pipe fed with data.
func withStdin(t *testing.T, data []byte, fn func()) {
	t.Helper()
	old := os.Stdin
	defer func() { os.Stdin = old }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.Write(data)
		_ = w.Close()
	}()
	os.Stdin = r
	fn()
}
