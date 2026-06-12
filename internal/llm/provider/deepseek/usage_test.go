package deepseek

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// TestComplete_UsageStaysZero confirms deepseek — a separate impl that
// does NOT route through openaicompat and does not decode a usage block —
// reports zero usage even when the response carries one, and does not
// error. This is the "graceful zero usage" contract.
func TestComplete_UsageStaysZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"choices":[{"message":{"content":"hello"}}],
			"usage":{"prompt_tokens":1234,"completion_tokens":456}
		}`)
	}))
	defer srv.Close()

	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	p, err := New(llm.RemoteConfig{Model: "deepseek-chat", BaseURL: srv.URL})
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
	if resp.Text != "hello" {
		t.Errorf("text=%q want hello", resp.Text)
	}
	if !resp.Usage.IsZero() {
		t.Errorf("usage=%+v want zero (deepseek does not decode usage)", resp.Usage)
	}
}
