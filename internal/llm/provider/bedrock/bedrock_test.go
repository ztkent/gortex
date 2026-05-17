package bedrock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/llm"
)

func TestNew_MissingModelID(t *testing.T) {
	if _, err := New(llm.BedrockConfig{}); err == nil {
		t.Fatal("expected error when model_id is unset")
	}
}

func TestNew_MissingCreds(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if _, err := New(llm.BedrockConfig{ModelID: "anthropic.claude-sonnet-4-20250514-v1:0"}); err == nil {
		t.Fatal("expected error when AWS credentials are unset")
	}
}

func TestComplete_StructuredUsesForcedTool(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotDate, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use EscapedPath so the colon-encoded form is preserved —
		// r.URL.Path would decode `%3A` back to `:`.
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("authorization")
		gotDate = r.Header.Get("x-amz-date")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"toolUse":{"toolUseId":"t1","name":"respond","input":{"terms":["bcrypt"]}}}]}}}`)
	}))
	defer srv.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	p, err := New(llm.BedrockConfig{
		ModelID: "anthropic.claude-sonnet-4-20250514-v1:0",
		Region:  "us-east-1",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	// Pin time for reproducibility.
	p.(*Provider).now = func() time.Time { return time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC) }

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "you expand queries"},
			{Role: llm.RoleUser, Content: "Query: hashing"},
		},
		Shape:     llm.ShapeExpandTerms,
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"terms":["bcrypt"]}` {
		t.Errorf("text=%q", resp.Text)
	}
	if !strings.Contains(gotPath, "%3A0") {
		t.Errorf("path=%q must URL-encode the colon in the model id", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIATEST/") {
		t.Errorf("authorization=%q — missing or malformed SigV4 header", gotAuth)
	}
	if gotDate == "" {
		t.Error("x-amz-date header missing")
	}
	if sys, _ := gotBody["system"].([]any); len(sys) != 1 {
		t.Errorf("system=%v want one entry hoisted from the system message", gotBody["system"])
	}
	tc, _ := gotBody["toolConfig"].(map[string]any)
	if tc == nil {
		t.Fatal("toolConfig missing on structured request")
	}
	tools := tc["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("tools=%d want 1", len(tools))
	}
	if choice, _ := tc["toolChoice"].(map[string]any); choice == nil {
		t.Error("toolChoice missing")
	}
}

func TestComplete_FreeformNoToolConfig(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"hello world"}]}}}`)
	}))
	defer srv.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	t.Setenv("AWS_SESSION_TOKEN", "")
	p, _ := New(llm.BedrockConfig{ModelID: "amazon.nova-pro-v1:0", BaseURL: srv.URL})
	defer func() { _ = p.Close() }()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Shape:    llm.ShapeFreeform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hello world" {
		t.Errorf("text=%q", resp.Text)
	}
	if _, ok := gotBody["toolConfig"]; ok {
		t.Error("freeform request must not send toolConfig")
	}
}

func TestComplete_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"validation error"}`)
	}))
	defer srv.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	p, _ := New(llm.BedrockConfig{ModelID: "m", BaseURL: srv.URL})
	defer func() { _ = p.Close() }()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
	if !strings.Contains(err.Error(), "validation error") {
		t.Errorf("err=%v should surface AWS message", err)
	}
}

func TestComplete_SessionTokenIncludedInSignedHeaders(t *testing.T) {
	var gotAuth, gotSecToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		gotSecToken = r.Header.Get("x-amz-security-token")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}}}`)
	}))
	defer srv.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	t.Setenv("AWS_SESSION_TOKEN", "FwoGZ...session...")
	p, _ := New(llm.BedrockConfig{ModelID: "m", BaseURL: srv.URL})
	defer func() { _ = p.Close() }()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Shape:    llm.ShapeFreeform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotSecToken != "FwoGZ...session..." {
		t.Errorf("x-amz-security-token=%q — STS session token must be forwarded", gotSecToken)
	}
	if !strings.Contains(gotAuth, "x-amz-security-token") {
		t.Errorf("authorization=%q must list x-amz-security-token among SignedHeaders", gotAuth)
	}
}

func TestName(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	p, _ := New(llm.BedrockConfig{ModelID: "m"})
	if p.Name() != "bedrock" {
		t.Errorf("Name()=%q", p.Name())
	}
}
