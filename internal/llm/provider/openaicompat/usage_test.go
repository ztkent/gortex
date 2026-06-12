package openaicompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// TestComplete_DecodesUsageBlock asserts the Chat Completions usage block —
// including the cached-prompt share — is decoded onto the response. The
// cached tokens are reported as CacheReadTokens; prompt_tokens stays the
// full input count.
func TestComplete_DecodesUsageBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"choices":[{"message":{"content":"hello"}}],
			"usage":{"prompt_tokens":1234,"completion_tokens":456,
			         "prompt_tokens_details":{"cached_tokens":2000}}
		}`)
	}))
	defer srv.Close()

	c := &Client{Tag: "openai", Model: "gpt-x", URL: srv.URL, HTTPClient: srv.Client()}
	resp, err := c.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := llm.TokenUsage{InputTokens: 1234, OutputTokens: 456, CacheReadTokens: 2000}
	if resp.Usage != want {
		t.Errorf("usage=%+v want %+v", resp.Usage, want)
	}
}

func TestComplete_NoUsageBlockIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hello"}}]}`)
	}))
	defer srv.Close()

	c := &Client{Tag: "openai", Model: "gpt-x", URL: srv.URL, HTTPClient: srv.Client()}
	resp, err := c.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Usage.IsZero() {
		t.Errorf("usage=%+v want zero", resp.Usage)
	}
}
