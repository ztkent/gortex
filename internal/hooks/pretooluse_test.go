package hooks

import (
	"strings"
	"testing"
)

func withDaemonReachable(t *testing.T, reachable bool) {
	t.Helper()
	prev := daemonReachableFn
	daemonReachableFn = func() bool { return reachable }
	t.Cleanup(func() { daemonReachableFn = prev })
}

func TestEnrichGlob_GreedySourcePattern_DaemonUp_Denies(t *testing.T) {
	withDaemonReachable(t, true)
	result := enrichGlob(map[string]any{"pattern": "**/*.go"})
	if !result.deny {
		t.Fatal("expected deny for greedy source glob with daemon up")
	}
	if !strings.Contains(result.reason, "BLOCKED") {
		t.Errorf("expected BLOCKED in reason, got: %s", result.reason)
	}
	if !strings.Contains(result.reason, "get_repo_outline") {
		t.Errorf("expected get_repo_outline in reason, got: %s", result.reason)
	}
}

func TestEnrichGlob_GreedySourcePattern_DaemonDown_Soft(t *testing.T) {
	withDaemonReachable(t, false)
	result := enrichGlob(map[string]any{"pattern": "**/*.go"})
	if result.deny {
		t.Fatal("daemon down ⇒ no enforcement; expected soft guidance")
	}
	if result.context == "" {
		t.Fatal("expected soft guidance text, got empty")
	}
	if !strings.Contains(result.context, "search_symbols") {
		t.Error("expected guidance to mention search_symbols")
	}
}

func TestEnrichGlob_NamedSourcePattern_NeverDenies(t *testing.T) {
	withDaemonReachable(t, true)
	cases := []string{
		"**/handler*.go",
		"*test*.ts",
		"**/Server.go",
		"src/**/component_*.tsx",
	}
	for _, p := range cases {
		result := enrichGlob(map[string]any{"pattern": p})
		if result.deny {
			t.Errorf("name-based pattern %q should not deny, got reason: %s", p, result.reason)
		}
		if result.context == "" {
			t.Errorf("name-based pattern %q should have soft guidance", p)
		}
	}
}

func TestEnrichGlob_NonSourcePattern(t *testing.T) {
	withDaemonReachable(t, true)
	result := enrichGlob(map[string]any{"pattern": "**/*.json"})
	if result.context != "" || result.deny {
		t.Errorf("expected empty for non-source glob, got: ctx=%q deny=%v", result.context, result.deny)
	}
}

func TestEnrichGlob_EmptyPattern(t *testing.T) {
	result := enrichGlob(map[string]any{"pattern": ""})
	if result.context != "" || result.deny {
		t.Errorf("expected empty for empty pattern, got: ctx=%q deny=%v", result.context, result.deny)
	}
}

func TestIsGreedySourceGlob(t *testing.T) {
	cases := []struct {
		pattern string
		want    bool
	}{
		{"*.go", true},
		{"**/*.go", true},
		{"src/**/*.tsx", true},
		{"**/handler.go", false},
		{"**/handler*.go", false},
		{"*test*.ts", false},
		{"foo.go", false}, // not a wildcard
		{"", false},
		{"**/*", false}, // no extension
	}
	for _, c := range cases {
		got := isGreedySourceGlob(c.pattern)
		if got != c.want {
			t.Errorf("isGreedySourceGlob(%q) = %v, want %v", c.pattern, got, c.want)
		}
	}
}

func TestEnrichRead_NonIndexed_Guidance(t *testing.T) {
	// Port 0 means bridge won't respond — file is not indexed.
	// Should return advisory guidance (not deny).
	result := enrichRead(map[string]any{"file_path": "/tmp/foo.go"}, 0)
	if result.context == "" {
		t.Fatal("expected guidance for unindexed source file, got empty")
	}
	if result.deny {
		t.Error("should not deny when file is not indexed")
	}
	if !strings.Contains(result.context, "get_symbol_source") {
		t.Error("expected guidance to mention get_symbol_source")
	}
	if !strings.Contains(result.context, "get_editing_context") {
		t.Error("expected guidance to mention get_editing_context")
	}
}

func TestEnrichRead_NonSourceFile(t *testing.T) {
	result := enrichRead(map[string]any{"file_path": "/tmp/config.json"}, 0)
	if result.context != "" || result.deny {
		t.Errorf("expected pass-through for non-source file, got: context=%q deny=%v", result.context, result.deny)
	}
}

func TestEnrichRead_NarrowRead_Allowed(t *testing.T) {
	// A read with offset+limit is narrow (for editing) — should always pass through.
	result := enrichRead(map[string]any{
		"file_path": "/tmp/foo.go",
		"offset":    float64(100),
		"limit":     float64(20),
	}, 0)
	if result.deny {
		t.Error("narrow read (offset+limit) should not be denied")
	}
	if result.context != "" {
		t.Error("narrow read should not produce guidance")
	}
}

func TestEnrichRead_OffsetOnly_Allowed(t *testing.T) {
	result := enrichRead(map[string]any{
		"file_path": "/tmp/foo.go",
		"offset":    float64(50),
	}, 0)
	if result.deny {
		t.Error("read with offset only should not be denied")
	}
}

func TestEnrichRead_SmallLimit_Allowed(t *testing.T) {
	result := enrichRead(map[string]any{
		"file_path": "/tmp/foo.go",
		"limit":     float64(30),
	}, 0)
	if result.deny {
		t.Error("read with small limit should not be denied")
	}
}

func TestEnrichRead_LargeLimit_NotNarrow(t *testing.T) {
	// Limit > 50 without offset is a whole-file read — should get guidance.
	result := enrichRead(map[string]any{
		"file_path": "/tmp/foo.go",
		"limit":     float64(500),
	}, 0)
	// Not indexed (port 0), so advisory only.
	if result.deny {
		t.Error("should not deny unindexed file")
	}
	if result.context == "" {
		t.Error("expected advisory guidance for large-limit read of source file")
	}
}

func TestEnrichGrep_Guidance(t *testing.T) {
	// No daemon reachable → should return soft guidance, not deny.
	stubProbe(t, nil, errDaemonUnreachable)
	result := enrichGrep(map[string]any{"pattern": "handleFindUsages"}, 0)
	if result.context == "" {
		t.Fatal("expected guidance for grep, got empty")
	}
	if result.deny {
		t.Error("grep should never be denied")
	}
	if !strings.Contains(result.context, "search_symbols") {
		t.Error("expected guidance to mention search_symbols")
	}
	if !strings.Contains(result.context, "find_usages") {
		t.Error("expected guidance to mention find_usages")
	}
}

func TestEnrichGrep_ShortPattern(t *testing.T) {
	result := enrichGrep(map[string]any{"pattern": "ab"}, 0)
	if result.context != "" {
		t.Errorf("expected empty for short pattern, got: %s", result.context)
	}
}

func TestIsNarrowRead(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]any
		want  bool
	}{
		{"offset+limit", map[string]any{"offset": float64(10), "limit": float64(20)}, true},
		{"offset only", map[string]any{"offset": float64(10)}, true},
		{"small limit only", map[string]any{"limit": float64(30)}, true},
		{"large limit only", map[string]any{"limit": float64(500)}, false},
		{"no offset no limit", map[string]any{}, false},
		{"nil values", map[string]any{"file_path": "foo.go"}, false},
	}
	for _, tt := range tests {
		got := isNarrowRead(tt.input)
		if got != tt.want {
			t.Errorf("isNarrowRead(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestEnrich_DispatchesCorrectly(t *testing.T) {
	tests := []struct {
		tool    string
		input   map[string]any
		wantNon bool // expect non-empty context or deny
	}{
		{"Read", map[string]any{"file_path": "/tmp/foo.go"}, true},
		{"Grep", map[string]any{"pattern": "handleFoo"}, true},
		{"Glob", map[string]any{"pattern": "**/*.ts"}, true},
		{"Glob", map[string]any{"pattern": "**/*.json"}, false},
		{"Write", map[string]any{}, false},
	}
	for _, tt := range tests {
		result := enrich(HookInput{
			HookEventName: "PreToolUse",
			ToolName:      tt.tool,
			ToolInput:     tt.input,
		}, 0)
		hasOutput := result.context != "" || result.deny
		if tt.wantNon && !hasOutput {
			t.Errorf("enrich(%s) returned no output, expected non-empty", tt.tool)
		}
		if !tt.wantNon && hasOutput {
			t.Errorf("enrich(%s) returned output, expected empty", tt.tool)
		}
	}
}
