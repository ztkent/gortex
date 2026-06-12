package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// TestComplete_DecodesUsageBlock asserts the Messages API usage block —
// including the cache read/write split — is decoded onto the response.
func TestComplete_DecodesUsageBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{
			"content":[{"type":"text","text":"hello"}],
			"usage":{"input_tokens":1234,"output_tokens":456,
			         "cache_read_input_tokens":2000,"cache_creation_input_tokens":300}
		}`)
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	p, err := New(llm.RemoteConfig{Model: "claude-x", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := llm.TokenUsage{InputTokens: 1234, OutputTokens: 456, CacheReadTokens: 2000, CacheWriteTokens: 300}
	if resp.Usage != want {
		t.Errorf("usage=%+v want %+v", resp.Usage, want)
	}
}

// TestComplete_NoUsageBlockIsZero asserts a response with no usage block
// yields zero usage and no error.
func TestComplete_NoUsageBlockIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"hello"}]}`)
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	p, _ := New(llm.RemoteConfig{Model: "claude-x", BaseURL: srv.URL})
	defer func() { _ = p.Close() }()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Usage.IsZero() {
		t.Errorf("usage=%+v want zero", resp.Usage)
	}
}
