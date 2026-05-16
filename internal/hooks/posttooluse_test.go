package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// stubBridge starts a httptest server that answers the three
// /api/graph/* endpoints the PostToolUse handler consults:
//
//   - /api/graph/file?path=...        → {"nodes": [...]} with len-1 == file
//                                       node + symbolCount entries
//   - /api/graph/symbol-at?path=...&line=N → {id, name, kind} when (path,N)
//                                       is in `enclosing` (else 404)
//   - /api/graph/file-callers?path=...   → {"count": N} when path is in
//                                       `callers` (else {"count": 0})
//
// Returns the port the server is listening on plus a cleanup func.
func stubBridge(
	t *testing.T,
	indexed map[string]int, // path → symbol count
	enclosing map[string]struct{ ID, Name, Kind string }, // "path:line" → symbol
	callers map[string]int, // path → external-caller count
) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/graph/file", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		count, ok := indexed[p]
		if !ok {
			_, _ = io.WriteString(w, `{"nodes":[]}`)
			return
		}
		nodes := make([]any, 0, count+1)
		nodes = append(nodes, map[string]any{"id": p, "kind": "file"})
		for i := range count {
			nodes = append(nodes, map[string]any{"id": fmt.Sprintf("%s::Sym%d", p, i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": nodes})
	})
	mux.HandleFunc("/api/graph/symbol-at", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		line := r.URL.Query().Get("line")
		key := p + ":" + line
		sym, ok := enclosing[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(sym)
	})
	mux.HandleFunc("/api/graph/file-callers", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		_ = json.NewEncoder(w).Encode(map[string]int{"count": callers[p]})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	return port
}

// ---------------------------------------------------------------------------
// parseGrepHits / parseGlobPaths — pure parsing
// ---------------------------------------------------------------------------

func TestParseGrepHits(t *testing.T) {
	body := `pkg/foo.go:42:func Bar() {
pkg/foo.go:42:    duplicate line  // dedup by path:line
pkg/baz.go:7:type Quux struct{}
Found 2 matches`
	hits := parseGrepHits(body)
	if len(hits) != 2 {
		t.Fatalf("expected 2 unique hits, got %d (%+v)", len(hits), hits)
	}
	if hits[0].path != "pkg/foo.go" || hits[0].line != 42 {
		t.Errorf("hits[0] = %+v", hits[0])
	}
	if hits[1].path != "pkg/baz.go" || hits[1].line != 7 {
		t.Errorf("hits[1] = %+v", hits[1])
	}
}

func TestParseGlobPaths(t *testing.T) {
	body := `Found 3 files
src/main.go
src/helper.go
(no further matches)
internal/util/x.go
`
	paths := parseGlobPaths(body)
	want := []string{"src/main.go", "src/helper.go", "internal/util/x.go"}
	if len(paths) != len(want) {
		t.Fatalf("got %d paths, want %d (%+v)", len(paths), len(want), paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// postGrep — graph context for ripgrep-style match output
// ---------------------------------------------------------------------------

func TestPostGrep_EnrichesWithEnclosingSymbols(t *testing.T) {
	port := stubBridge(t, nil,
		map[string]struct{ ID, Name, Kind string }{
			"pkg/foo.go:42": {ID: "pkg/foo.go::Bar", Name: "Bar", Kind: "function"},
			"pkg/baz.go:7":  {ID: "pkg/baz.go::Quux", Name: "Quux", Kind: "type"},
		}, nil)

	input := postHookInput{
		ToolName: "Grep",
		ToolResponse: "pkg/foo.go:42:func Bar() {\n" +
			"pkg/baz.go:7:type Quux struct{}\n",
	}
	out := postGrep(input, port)
	if out == "" {
		t.Fatal("expected enrichment context, got empty")
	}
	if !strings.Contains(out, "function Bar") {
		t.Errorf("missing enclosing symbol for foo.go:42 in:\n%s", out)
	}
	if !strings.Contains(out, "type Quux") {
		t.Errorf("missing enclosing symbol for baz.go:7 in:\n%s", out)
	}
}

func TestPostGrep_EmptyWhenNoEnclosingSymbol(t *testing.T) {
	port := stubBridge(t, nil, nil, nil) // no enclosing for any hit
	input := postHookInput{
		ToolName:     "Grep",
		ToolResponse: "pkg/foo.go:42:something\n",
	}
	out := postGrep(input, port)
	if out != "" {
		t.Errorf("expected empty context when no symbols resolve, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// postGlob — file footprint summary
// ---------------------------------------------------------------------------

func TestPostGlob_RanksByIndexedSymbolCount(t *testing.T) {
	indexed := map[string]int{
		"src/big.go":   42,
		"src/small.go": 3,
	}
	port := stubBridge(t, indexed, nil, nil)

	input := postHookInput{
		ToolName:     "Glob",
		ToolResponse: "src/big.go\nsrc/small.go\nsrc/empty.go\n", // empty.go not indexed
	}
	out := postGlob(input, port)
	if out == "" {
		t.Fatal("expected enrichment, got empty")
	}
	// big.go (42 symbols) should appear before small.go (3 symbols).
	bigPos := strings.Index(out, "src/big.go")
	smallPos := strings.Index(out, "src/small.go")
	if bigPos < 0 || smallPos < 0 {
		t.Fatalf("expected both files in output:\n%s", out)
	}
	if bigPos >= smallPos {
		t.Errorf("expected larger file (big.go) ranked first, got:\n%s", out)
	}
	// Counts visible.
	if !strings.Contains(out, "42 symbol(s)") || !strings.Contains(out, "3 symbol(s)") {
		t.Errorf("expected symbol counts in output:\n%s", out)
	}
	// Unindexed file should NOT appear individually but should be in
	// the X/Y summary.
	if strings.Contains(out, "src/empty.go") {
		t.Errorf("unindexed file leaked into output:\n%s", out)
	}
}

func TestPostGlob_AllUnindexedReturnsEmpty(t *testing.T) {
	port := stubBridge(t, nil, nil, nil)
	input := postHookInput{
		ToolName:     "Glob",
		ToolResponse: "vendor/some.go\n",
	}
	out := postGlob(input, port)
	if out != "" {
		t.Errorf("expected empty context when nothing is indexed, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// postRead — file footprint
// ---------------------------------------------------------------------------

func TestPostRead_IncludesSymbolAndCallerCounts(t *testing.T) {
	indexed := map[string]int{"pkg/handler.go": 12}
	callers := map[string]int{"pkg/handler.go": 8}
	port := stubBridge(t, indexed, nil, callers)

	input := postHookInput{
		ToolName:  "Read",
		ToolInput: map[string]any{"file_path": "pkg/handler.go"},
	}
	out := postRead(input, port)
	if out == "" {
		t.Fatal("expected enrichment for indexed file, got empty")
	}
	if !strings.Contains(out, "12 indexed symbol(s)") {
		t.Errorf("missing symbol count in:\n%s", out)
	}
	if !strings.Contains(out, "8 unique external caller(s)") {
		t.Errorf("missing caller count in:\n%s", out)
	}
}

func TestPostRead_SkipsUnindexedFiles(t *testing.T) {
	port := stubBridge(t, nil, nil, nil)
	input := postHookInput{
		ToolName:  "Read",
		ToolInput: map[string]any{"file_path": "README.md"},
	}
	out := postRead(input, port)
	if out != "" {
		t.Errorf("expected empty for unindexed file, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// runPostToolUse — dispatcher integration
// ---------------------------------------------------------------------------

func TestRunPostToolUse_DispatchesByToolName(t *testing.T) {
	indexed := map[string]int{"pkg/handler.go": 5}
	port := stubBridge(t, indexed, nil, nil)

	payload := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"file_path":"pkg/handler.go"}}`)
	out := captureStdout(t, func() { runPostToolUse(payload, port) })
	if out == "" {
		t.Fatal("expected JSON output from dispatcher")
	}
	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, out)
	}
	if dec.HookSpecificOutput == nil {
		t.Fatal("missing hookSpecificOutput")
	}
	if dec.HookSpecificOutput.HookEventName != "PostToolUse" {
		t.Errorf("wrong event name: %q", dec.HookSpecificOutput.HookEventName)
	}
	if !strings.Contains(dec.HookSpecificOutput.AdditionalContext, "5 indexed symbol(s)") {
		t.Errorf("missing expected context:\n%s", dec.HookSpecificOutput.AdditionalContext)
	}
}

func TestRunPostToolUse_SilentForUnsupportedTool(t *testing.T) {
	port := stubBridge(t, nil, nil, nil)
	payload := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"echo hi"}}`)
	out := captureStdout(t, func() { runPostToolUse(payload, port) })
	if out != "" {
		t.Errorf("expected silent no-op for Bash, got:\n%s", out)
	}
}

func TestRunPostToolUse_SilentOnNonPostToolUseEvent(t *testing.T) {
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"x.go"}}`)
	out := captureStdout(t, func() { runPostToolUse(payload, 0) })
	if out != "" {
		t.Errorf("expected silent no-op for non-PostToolUse event, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Mode plumbing — Run() should route PostToolUse through runPostToolUse
// ---------------------------------------------------------------------------

func TestDispatchRoutesPostToolUseEvent(t *testing.T) {
	indexed := map[string]int{"pkg/x.go": 3}
	port := stubBridge(t, indexed, nil, nil)

	data := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"file_path":"pkg/x.go"}}`)
	withStdin(t, data, func() {
		out := captureStdout(t, func() { Run(port, ModeEnrich) })
		if out == "" {
			t.Fatal("dispatcher dropped PostToolUse silently")
		}
		if !strings.Contains(out, "3 indexed symbol(s)") {
			t.Errorf("dispatcher routed to wrong handler:\n%s", out)
		}
	})
}
