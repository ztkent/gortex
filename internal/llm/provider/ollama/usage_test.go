package ollama

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

// TestComplete_UsageStaysZero confirms ollama — a separate impl that does
// not decode a usage block — reports zero usage and does not error, even
// when the response carries token counts. This is the graceful-zero
// contract for the not-yet-decoded HTTP providers.
func TestComplete_UsageStaysZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"hi there"},
			"prompt_eval_count":1234,"eval_count":456}`)
	}))
	defer srv.Close()

	p, err := New(llm.OllamaConfig{Model: "qwen", Host: srv.URL})
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
	if resp.Text != "hi there" {
		t.Errorf("text=%q want 'hi there'", resp.Text)
	}
	if !resp.Usage.IsZero() {
		t.Errorf("usage=%+v want zero (ollama does not decode usage)", resp.Usage)
	}
}
