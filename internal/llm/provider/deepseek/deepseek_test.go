package deepseek

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

func TestNew_MissingKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	if _, err := New(llm.RemoteConfig{Model: "deepseek-chat"}); err == nil {
		t.Fatal("expected error when API key env is unset")
	}
}

func TestNew_MissingModel(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "k")
	if _, err := New(llm.RemoteConfig{}); err == nil {
		t.Fatal("expected error when model is unset")
	}
}

func TestComplete_StructuredUsesJSONObjectMode(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%q want /v1/chat/completions", r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer test-key" {
			t.Errorf("authorization=%q", r.Header.Get("authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"terms\":[\"jwt\"]}"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	p, err := New(llm.RemoteConfig{Model: "deepseek-chat", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "Query: auth"}},
		Shape:     llm.ShapeExpandTerms,
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"terms":["jwt"]}` {
		t.Errorf("text=%q", resp.Text)
	}
	rf, _ := gotBody["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "json_object" {
		t.Errorf("response_format=%v want type=json_object — DeepSeek does not support strict json_schema", gotBody["response_format"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("messages missing")
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(first["content"].(string), "JSON schema") {
		t.Errorf("first message=%v should be a system prompt carrying the schema hint", first)
	}
}

func TestComplete_FreeformNoResponseFormat(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"plain text"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("DEEPSEEK_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer func() { _ = p.Close() }()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Shape:    llm.ShapeFreeform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "plain text" {
		t.Errorf("text=%q", resp.Text)
	}
	if _, ok := gotBody["response_format"]; ok {
		t.Error("freeform request must not send response_format")
	}
	msgs := gotBody["messages"].([]any)
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "system" && strings.Contains(mm["content"].(string), "JSON schema") {
			t.Error("freeform request must not inject schema-hint system prompt")
		}
	}
}

func TestComplete_ToolResultBecomesUserTurn(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("DEEPSEEK_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer func() { _ = p.Close() }()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "q"},
			{Role: llm.RoleAssistant, Content: `{"tool":"x"}`},
			{Role: llm.RoleTool, Content: `{"result":1}`, ToolName: "x"},
		},
		Shape: llm.ShapeFreeform,
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := gotBody["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Errorf("tool result role=%v want user (emulated protocol)", last["role"])
	}
}

func TestComplete_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_api_key","message":"bad key"}}`)
	}))
	defer srv.Close()

	t.Setenv("DEEPSEEK_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer func() { _ = p.Close() }()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestName(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m"})
	if p.Name() != "deepseek" {
		t.Errorf("Name()=%q", p.Name())
	}
}
