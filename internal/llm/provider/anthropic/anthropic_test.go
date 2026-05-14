package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

func TestNew_MissingKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := New(llm.RemoteConfig{Model: "claude-x"}); err == nil {
		t.Fatal("expected error when API key env is unset")
	}
}

func TestNew_MissingModel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	if _, err := New(llm.RemoteConfig{}); err == nil {
		t.Fatal("expected error when model is unset")
	}
}

func TestComplete_StructuredUsesForcedTool(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path=%q want /v1/messages", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key=%q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version header")
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"tool_use","name":"respond","input":{"terms":["bcrypt","argon2"]}}]}`)
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	p, err := New(llm.RemoteConfig{Model: "claude-x", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "you expand queries"},
			{Role: llm.RoleUser, Content: "Query: hashing"},
		},
		Shape:     llm.ShapeExpandTerms,
		MaxTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"terms":["bcrypt","argon2"]}` {
		t.Errorf("text=%q", resp.Text)
	}
	if gotBody["system"] != "you expand queries" {
		t.Errorf("system=%v — system message should be hoisted to the top-level field", gotBody["system"])
	}
	if gotBody["tool_choice"] == nil {
		t.Error("structured request must force a tool_choice")
	}
	if tools, _ := gotBody["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools=%v want exactly the respond tool", gotBody["tools"])
	}
	if msgs, _ := gotBody["messages"].([]any); len(msgs) != 1 {
		t.Errorf("messages=%v — system should be extracted, leaving just the user turn", gotBody["messages"])
	}
}

func TestComplete_FreeformNoTools(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"hello world"}]}`)
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Shape:    llm.ShapeFreeform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hello world" {
		t.Errorf("text=%q want 'hello world'", resp.Text)
	}
	if _, ok := gotBody["tools"]; ok {
		t.Error("freeform request must not send tools")
	}
}

func TestComplete_ToolResultBecomesUserTurn(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}]}`)
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer p.Close()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "q"},
			{Role: llm.RoleAssistant, Content: `{"tool":"x","args":{}}`},
			{Role: llm.RoleTool, Content: `{"result":1}`, ToolName: "x"},
		},
		Shape: llm.ShapeFreeform,
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages=%d want 3", len(msgs))
	}
	last := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Errorf("tool result role=%v want user (emulated protocol)", last["role"])
	}
}

func TestComplete_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad model"}}`)
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer p.Close()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestName(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m"})
	if p.Name() != "anthropic" {
		t.Errorf("Name()=%q", p.Name())
	}
}
