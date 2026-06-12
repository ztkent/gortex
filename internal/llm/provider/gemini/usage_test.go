package gemini

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// TestComplete_DecodesUsageMetadata asserts the generateContent
// usageMetadata block is decoded onto the response, with the cached
// share reported as CacheReadTokens.
func TestComplete_DecodesUsageMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"candidates":[{"content":{"parts":[{"text":"hello"}]}}],
			"usageMetadata":{"promptTokenCount":1234,"candidatesTokenCount":456,
			                 "cachedContentTokenCount":2000}
		}`)
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_KEY", "test-key")
	p, err := New(llm.RemoteConfig{Model: "gemini-2.5-pro", BaseURL: srv.URL})
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
	want := llm.TokenUsage{InputTokens: 1234, OutputTokens: 456, CacheReadTokens: 2000}
	if resp.Usage != want {
		t.Errorf("usage=%+v want %+v", resp.Usage, want)
	}
}

func TestComplete_NoUsageMetadataIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`)
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_KEY", "test-key")
	p, _ := New(llm.RemoteConfig{Model: "gemini-2.5-pro", BaseURL: srv.URL})
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
