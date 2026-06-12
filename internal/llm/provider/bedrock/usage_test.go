package bedrock

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// TestComplete_DecodesUsageBlock asserts the Converse API usage block —
// including the cache read/write split — is decoded onto the response.
func TestComplete_DecodesUsageBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},
			"usage":{"inputTokens":1234,"outputTokens":456,
			         "cacheReadInputTokens":2000,"cacheWriteInputTokens":300}
		}`)
	}))
	defer srv.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")
	p, err := New(llm.BedrockConfig{ModelID: "amazon.nova-pro-v1:0", BaseURL: srv.URL})
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

func TestComplete_NoUsageBlockIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}}}`)
	}))
	defer srv.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")
	p, _ := New(llm.BedrockConfig{ModelID: "amazon.nova-pro-v1:0", BaseURL: srv.URL})
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
