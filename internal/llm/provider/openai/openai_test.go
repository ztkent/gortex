package openai

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
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := New(llm.RemoteConfig{Model: "gpt-x"}); err == nil {
		t.Fatal("expected error when API key env is unset")
	}
}

func TestComplete_StructuredUsesJSONSchema(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer test-key" {
			t.Errorf("authorization=%q", r.Header.Get("authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"terms\":[\"jwt\"]}"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	p, err := New(llm.RemoteConfig{Model: "gpt-x", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

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
	rf, ok := gotBody["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing/invalid: %v", gotBody["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type=%v want json_schema", rf["type"])
	}
	js := rf["json_schema"].(map[string]any)
	if js["strict"] != true {
		t.Errorf("list shapes should request strict json_schema, got strict=%v", js["strict"])
	}
}

func TestComplete_ToolCallShapeIsNonStrict(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"tool\":\"x\",\"args\":{}}"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("OPENAI_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer p.Close()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
		Shape:    llm.ShapeToolCall,
		Tools:    []llm.ToolSpec{{Name: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rf := gotBody["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if js["strict"] != false {
		t.Errorf("tool-call shape must be non-strict (args is open-ended), got strict=%v", js["strict"])
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

	t.Setenv("OPENAI_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer p.Close()

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
}

func TestComplete_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_api_key","message":"bad key"}}`)
	}))
	defer srv.Close()

	t.Setenv("OPENAI_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestName(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m"})
	if p.Name() != "openai" {
		t.Errorf("Name()=%q", p.Name())
	}
}
