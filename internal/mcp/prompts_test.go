package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// promptText concatenates the text content of a prompt result.
func promptText(res *mcp.GetPromptResult) string {
	var b strings.Builder
	for _, m := range res.Messages {
		if tc, ok := m.Content.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestPrompts_EmptyArgs proves every prompts/get handler survives a
// request with no `arguments` object (mcp-go leaves Params.Arguments
// nil) — no panic, a non-nil result with at least one message.
func TestPrompts_EmptyArgs(t *testing.T) {
	srv := newFullTestServer(t)
	ctx := context.Background()
	handlers := map[string]func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error){
		"pre_commit":     srv.handlePromptPreCommit,
		"orientation":    srv.handlePromptOrientation,
		"safe_to_change": srv.handlePromptSafeToChange,
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			res, err := h(ctx, mcp.GetPromptRequest{})
			if err != nil {
				t.Fatalf("empty-args %s returned a Go error: %v", name, err)
			}
			if res == nil || len(res.Messages) == 0 {
				t.Fatalf("empty-args %s returned an empty result", name)
			}
		})
	}
}

func TestPromptArg_NilAndWhitespace(t *testing.T) {
	if got := promptArg(mcp.GetPromptRequest{}, "x"); got != "" {
		t.Errorf("promptArg on a nil Arguments map = %q, want empty", got)
	}
	req := mcp.GetPromptRequest{}
	req.Params.Arguments = map[string]string{"ids": "  pkg/a.go::F  "}
	if got := promptArg(req, "ids"); got != "pkg/a.go::F" {
		t.Errorf("promptArg should trim surrounding whitespace, got %q", got)
	}
	if got := promptArg(req, "missing"); got != "" {
		t.Errorf("promptArg on a missing key = %q, want empty", got)
	}
}

func TestHandlePromptSafeToChange_EmptyArgsAsksForIDs(t *testing.T) {
	srv := newFullTestServer(t)
	res, err := srv.handlePromptSafeToChange(context.Background(), mcp.GetPromptRequest{})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || len(res.Messages) == 0 {
		t.Fatal("expected a graceful prompt-level error result")
	}
	if txt := promptText(res); !strings.Contains(txt, "ids argument is required") {
		t.Errorf("want 'ids argument is required', got: %q", txt)
	}
}

func TestHandlePromptPreCommit_InvalidScopeFallsBack(t *testing.T) {
	srv := newFullTestServer(t)
	req := mcp.GetPromptRequest{}
	req.Params.Arguments = map[string]string{"scope": "bogus-scope-xyz"}
	// A junk scope must never reach analysis.MapGitDiff verbatim — the
	// handler falls back to "all" and still produces a result.
	res, err := srv.handlePromptPreCommit(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || len(res.Messages) == 0 {
		t.Fatal("invalid scope produced an empty result")
	}
}
