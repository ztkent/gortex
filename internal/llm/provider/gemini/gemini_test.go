package gemini

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
	t.Setenv("GEMINI_API_KEY", "")
	if _, err := New(llm.RemoteConfig{Model: "gemini-2.5-pro"}); err == nil {
		t.Fatal("expected error when API key env is unset")
	}
}

func TestNew_MissingModel(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "k")
	if _, err := New(llm.RemoteConfig{}); err == nil {
		t.Fatal("expected error when model is unset")
	}
}

func TestComplete_StructuredUsesResponseSchema(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("x-goog-api-key=%q", r.Header.Get("x-goog-api-key"))
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"{\"terms\":[\"jwt\"]}"}],"role":"model"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_KEY", "test-key")
	p, err := New(llm.RemoteConfig{Model: "gemini-2.5-pro", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "you expand queries"},
			{Role: llm.RoleUser, Content: "Query: auth"},
		},
		Shape:     llm.ShapeExpandTerms,
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"terms":["jwt"]}` {
		t.Errorf("text=%q", resp.Text)
	}
	if !strings.Contains(gotPath, "gemini-2.5-pro:generateContent") {
		t.Errorf("path=%q does not embed model + action", gotPath)
	}
	sys, _ := gotBody["system_instruction"].(map[string]any)
	if sys == nil {
		t.Fatal("system_instruction missing")
	}
	gen, _ := gotBody["generationConfig"].(map[string]any)
	if gen == nil {
		t.Fatal("generationConfig missing")
	}
	if gen["responseMimeType"] != "application/json" {
		t.Errorf("responseMimeType=%v want application/json", gen["responseMimeType"])
	}
	schema, _ := gen["responseSchema"].(map[string]any)
	if schema == nil {
		t.Fatal("responseSchema missing on structured request")
	}
	if _, ok := schema["additionalProperties"]; ok {
		t.Error("responseSchema must not carry additionalProperties — Gemini rejects it")
	}
}

func TestComplete_FreeformNoSchema(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer func() { _ = p.Close() }()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Shape:    llm.ShapeFreeform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hi" {
		t.Errorf("text=%q", resp.Text)
	}
	if gen, ok := gotBody["generationConfig"].(map[string]any); ok {
		if gen["responseSchema"] != nil {
			t.Errorf("freeform request must not set responseSchema, got %v", gen["responseSchema"])
		}
	}
}

func TestComplete_ToolResultBecomesUserTurn(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_KEY", "k")
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
	contents := gotBody["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents=%d want 3", len(contents))
	}
	asst := contents[1].(map[string]any)
	if asst["role"] != "model" {
		t.Errorf("assistant role=%v want model (Gemini uses 'model' for assistant turns)", asst["role"])
	}
	tool := contents[2].(map[string]any)
	if tool["role"] != "user" {
		t.Errorf("tool result role=%v want user (emulated protocol)", tool["role"])
	}
}

func TestComplete_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"bad model","status":"INVALID_ARGUMENT"}}`)
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m", BaseURL: srv.URL})
	defer func() { _ = p.Close() }()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestName(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "k")
	p, _ := New(llm.RemoteConfig{Model: "m"})
	if p.Name() != "gemini" {
		t.Errorf("Name()=%q", p.Name())
	}
}

func TestSanitiseSchema_StripsAdditionalProperties(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"terms": map[string]any{
				"type":                 "array",
				"items":                map[string]any{"type": "string"},
				"additionalProperties": false,
			},
		},
		"additionalProperties": false,
	}
	out := sanitiseSchema(in)
	if _, ok := out["additionalProperties"]; ok {
		t.Error("top-level additionalProperties not stripped")
	}
	props := out["properties"].(map[string]any)
	terms := props["terms"].(map[string]any)
	if _, ok := terms["additionalProperties"]; ok {
		t.Error("nested additionalProperties not stripped")
	}
}
