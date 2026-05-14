package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

func TestNew_MissingModel(t *testing.T) {
	if _, err := New(llm.OllamaConfig{}); err == nil {
		t.Fatal("expected error when model is unset")
	}
}

func TestNew_DefaultsHost(t *testing.T) {
	p, err := New(llm.OllamaConfig{Model: "qwen"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Name() != "ollama" {
		t.Errorf("Name()=%q", p.Name())
	}
}

func TestComplete_StructuredSendsFormatSchema(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path=%q want /api/chat", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"{\"keep\":[\"a\"]}"}}`)
	}))
	defer srv.Close()

	p, err := New(llm.OllamaConfig{Model: "qwen2.5-coder:7b", Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "verify"}},
		Shape:     llm.ShapeVerifyKeep,
		MaxTokens: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"keep":["a"]}` {
		t.Errorf("text=%q", resp.Text)
	}
	if gotBody["format"] == nil {
		t.Error("structured request must send a `format` schema")
	}
	if gotBody["stream"] != false {
		t.Errorf("stream=%v want false", gotBody["stream"])
	}
	opts, _ := gotBody["options"].(map[string]any)
	if opts == nil || opts["num_predict"] == nil {
		t.Errorf("options.num_predict missing: %v", gotBody["options"])
	}
}

func TestComplete_FreeformNoFormat(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"hi there"}}`)
	}))
	defer srv.Close()

	p, _ := New(llm.OllamaConfig{Model: "m", Host: srv.URL})
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Shape:    llm.ShapeFreeform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hi there" {
		t.Errorf("text=%q", resp.Text)
	}
	if _, ok := gotBody["format"]; ok {
		t.Error("freeform request must not send a `format` field")
	}
}

func TestComplete_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()

	p, _ := New(llm.OllamaConfig{Model: "m", Host: srv.URL})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestComplete_InlineErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 OK but an error payload — Ollama does this for some failures.
		_, _ = io.WriteString(w, `{"error":"something went wrong"}`)
	}))
	defer srv.Close()

	p, _ := New(llm.OllamaConfig{Model: "m", Host: srv.URL})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected an error when the response carries an inline error field")
	}
}
