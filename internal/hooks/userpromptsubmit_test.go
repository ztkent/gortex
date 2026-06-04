package hooks

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPromptQuery(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"empty", "", ""},
		{"whitespace", "   \n  ", ""},
		{"slash command", "/clear", ""},
		{"short ack", "ok", ""},
		{"two-letter word", "go", ""},
		{"normal task", "fix the auth bug", "fix the auth bug"},
		{"collapses whitespace", "fix\n\n  the   bug", "fix the bug"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, promptQuery(tc.prompt))
		})
	}
}

func TestPromptQueryTruncatesLongPrompts(t *testing.T) {
	long := strings.Repeat("token ", 100) // ~600 chars
	got := promptQuery(long)
	require.LessOrEqual(t, len([]rune(got)), 240)
	require.NotEmpty(t, got)
}

func TestBuildPromptInjection(t *testing.T) {
	require.Equal(t, "", buildPromptInjection(nil), "no hits → no block")

	hits := []grepSymbolHit{
		{Name: "ValidateToken", Kind: "function", FilePath: "internal/auth/token.go", Line: 42},
		{Name: "AuthMiddleware", Kind: "type", FilePath: "internal/auth/mw.go"}, // no line
	}
	block := buildPromptInjection(hits)
	require.Contains(t, block, "relevant indexed symbols")
	require.Contains(t, block, "`ValidateToken` (function) — internal/auth/token.go:42")
	require.Contains(t, block, "`AuthMiddleware` (type) — internal/auth/mw.go")
	require.Contains(t, block, "smart_context")
}

func TestBuildPromptInjectionCapsHits(t *testing.T) {
	var hits []grepSymbolHit
	for i := 0; i < 20; i++ {
		hits = append(hits, grepSymbolHit{Name: "Sym", Kind: "function", FilePath: "f.go", Line: i + 1})
	}
	block := buildPromptInjection(hits)
	require.Equal(t, maxInjectedHits, strings.Count(block, "- `Sym`"), "injection is capped")
}

// TestRunUserPromptSubmitTrivialPromptIsNoOp confirms a trivial prompt never
// even probes the daemon (and so produces no output).
func TestRunUserPromptSubmitTrivialPromptIsNoOp(t *testing.T) {
	// A slash command produces an empty query → early return, no panic, no
	// daemon dial. We assert it doesn't panic and returns cleanly.
	runUserPromptSubmit([]byte(`{"hook_event_name":"UserPromptSubmit","prompt":"/clear"}`))
	runUserPromptSubmit([]byte(`{"hook_event_name":"UserPromptSubmit","prompt":"ok"}`))
	// Wrong event name is also a no-op.
	runUserPromptSubmit([]byte(`{"hook_event_name":"SessionStart","prompt":"do something big"}`))
}
