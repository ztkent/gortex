package azure

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/llm"
)

func TestNew_RequiresDeployment(t *testing.T) {
	t.Setenv("AZURE_OPENAI_API_KEY", "k")
	if _, err := New(llm.AzureConfig{Endpoint: "https://x.openai.azure.com"}); err == nil {
		t.Fatal("expected an error when deployment is empty")
	}
}

func TestNew_RequiresEndpoint(t *testing.T) {
	t.Setenv("AZURE_OPENAI_API_KEY", "k")
	t.Setenv("AZURE_OPENAI_ENDPOINT", "")
	if _, err := New(llm.AzureConfig{Deployment: "gpt4o", EndpointEnv: "AZURE_OPENAI_ENDPOINT"}); err == nil {
		t.Fatal("expected an error when no endpoint is configured or in env")
	}
}

func TestNew_RequiresKey(t *testing.T) {
	t.Setenv("AZURE_OPENAI_API_KEY", "")
	if _, err := New(llm.AzureConfig{Deployment: "gpt4o", Endpoint: "https://x.openai.azure.com", APIKeyEnv: "AZURE_OPENAI_API_KEY"}); err == nil {
		t.Fatal("expected an error when the API key env is unset")
	}
}

// TestComplete_AddressesDeploymentWithApiKeyHeader proves the Azure
// auth model: deployment folded into the path, api-version in the
// query string, and the key in an api-key header (not Bearer).
func TestComplete_AddressesDeploymentWithApiKeyHeader(t *testing.T) {
	var gotPath, gotQuery, gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("api-key")
		gotAuth = r.Header.Get("authorization")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hello from azure"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("AZURE_OPENAI_API_KEY", "secret-key")
	p, err := New(llm.AzureConfig{
		Endpoint:   srv.URL,
		Deployment: "my-gpt4o",
		APIVersion: "2024-10-21",
		APIKeyEnv:  "AZURE_OPENAI_API_KEY",
	})
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
	if resp.Text != "hello from azure" {
		t.Errorf("text=%q", resp.Text)
	}
	if gotPath != "/openai/deployments/my-gpt4o/chat/completions" {
		t.Errorf("path=%q want the deployment folded into the path", gotPath)
	}
	if gotQuery != "api-version=2024-10-21" {
		t.Errorf("query=%q want api-version", gotQuery)
	}
	if gotKey != "secret-key" {
		t.Errorf("api-key header=%q", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("authorization header should be empty for Azure key auth, got %q", gotAuth)
	}
}

// TestComplete_EndpointFromEnv proves the endpoint is read from the
// configured env var when not set in config.
func TestComplete_EndpointFromEnv(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("AZURE_OPENAI_API_KEY", "k")
	t.Setenv("MY_AZURE_ENDPOINT", srv.URL)
	p, err := New(llm.AzureConfig{
		Deployment:  "d",
		EndpointEnv: "MY_AZURE_ENDPOINT",
		APIKeyEnv:   "AZURE_OPENAI_API_KEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Error("expected the request to reach the env-configured endpoint")
	}
}

func TestName(t *testing.T) {
	t.Setenv("AZURE_OPENAI_API_KEY", "k")
	p, err := New(llm.AzureConfig{Deployment: "d", Endpoint: "https://x.openai.azure.com", APIKeyEnv: "AZURE_OPENAI_API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "azure" {
		t.Errorf("Name()=%q want azure", p.Name())
	}
}

// TestComplete_StructuredUsesJSONSchema proves Azure inherits the
// native json_schema structured-output path from the shared client.
func TestComplete_StructuredUsesJSONSchema(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"terms\":[\"jwt\"]}"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("AZURE_OPENAI_API_KEY", "k")
	p, _ := New(llm.AzureConfig{Deployment: "d", Endpoint: srv.URL, APIKeyEnv: "AZURE_OPENAI_API_KEY"})
	defer func() { _ = p.Close() }()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "auth"}},
		Shape:    llm.ShapeExpandTerms,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"response_format"`) || !strings.Contains(gotBody, "json_schema") {
		t.Errorf("expected a json_schema response_format in the request body, got %s", gotBody)
	}
}
