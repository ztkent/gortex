// Package deepseek is the hosted DeepSeek Chat Completions
// llm.Provider.
//
// It is pure Go — available in every build. DeepSeek exposes an
// OpenAI-compatible /v1/chat/completions endpoint but supports only
// the JSON-object form of structured output (`response_format`:
// `{"type":"json_object"}`), not the strict `json_schema` form.
// Structured shapes therefore set JSON mode and inline the requested
// schema into the system prompt as instructions. The agent tool-loop
// uses the emulated protocol — tool calls and results travel as
// plain text turns — so a single llm.Message shape works across
// providers.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/llm"
)

// Provider implements llm.Provider against api.deepseek.com.
type Provider struct {
	model   string
	apiKey  string
	baseURL string
	client  *http.Client
}

var _ llm.Provider = (*Provider)(nil)

// New constructs the DeepSeek provider. The API key is read from the
// env var named by cfg.APIKeyEnv (default DEEPSEEK_API_KEY); an unset
// key is a hard error so misconfiguration surfaces at startup.
func New(cfg llm.RemoteConfig) (llm.Provider, error) {
	keyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = "DEEPSEEK_API_KEY"
	}
	key := strings.TrimSpace(os.Getenv(keyEnv))
	if key == "" {
		return nil, fmt.Errorf("deepseek: API key env %q is not set", keyEnv)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("deepseek: llm.deepseek.model is empty")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://api.deepseek.com"
	}
	return &Provider{
		model:   cfg.Model,
		apiKey:  key,
		baseURL: base,
		client:  &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "deepseek" }

// Close releases idle HTTP connections.
func (p *Provider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

// wire types for the chat completions endpoint.
type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiRequest struct {
	Model          string         `json:"model"`
	Messages       []apiMessage   `json:"messages"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	ResponseFormat map[string]any `json:"response_format,omitempty"`
}

type apiResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	body := apiRequest{
		Model:     p.model,
		Messages:  mapMessages(req.Messages, req.Shape, req.Tools),
		MaxTokens: req.MaxTokens,
	}
	if schema := llm.JSONSchemaFor(req.Shape, req.Tools); schema != nil {
		body.ResponseFormat = map[string]any{"type": "json_object"}
		_ = schema // schema content is folded into the system prompt by mapMessages
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("deepseek: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("deepseek: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("deepseek: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("deepseek: read response: %w", err)
	}

	var parsed apiResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("deepseek: decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return llm.CompletionResponse{}, fmt.Errorf("deepseek: API error (status %d): %s: %s", resp.StatusCode, parsed.Error.Type, parsed.Error.Message)
		}
		return llm.CompletionResponse{}, fmt.Errorf("deepseek: API error (status %d): %s", resp.StatusCode, snippet(payload))
	}
	if len(parsed.Choices) == 0 {
		return llm.CompletionResponse{}, errors.New("deepseek: response carried no choices")
	}
	return llm.CompletionResponse{Text: strings.TrimSpace(parsed.Choices[0].Message.Content)}, nil
}

// mapMessages flattens the provider-neutral conversation onto DeepSeek
// chat roles. Tool observations become user turns (emulated tool-call
// protocol). When the request is structured the JSON schema is folded
// into the system prompt as plain text — DeepSeek's JSON-mode only
// guarantees valid JSON, not schema conformance, so the prompt is
// where the shape gets communicated.
func mapMessages(in []llm.Message, shape llm.JSONShape, tools []llm.ToolSpec) []apiMessage {
	out := make([]apiMessage, 0, len(in)+1)
	schemaHint := schemaPrompt(shape, tools)
	if schemaHint != "" {
		// Inject the schema hint as an extra leading system turn so
		// it merges with any user-supplied system messages.
		out = append(out, apiMessage{Role: "system", Content: schemaHint})
	}
	for _, m := range in {
		switch m.Role {
		case llm.RoleSystem:
			out = append(out, apiMessage{Role: "system", Content: m.Content})
		case llm.RoleAssistant:
			out = append(out, apiMessage{Role: "assistant", Content: m.Content})
		case llm.RoleTool:
			out = append(out, apiMessage{Role: "user", Content: renderToolResult(m)})
		default:
			out = append(out, apiMessage{Role: "user", Content: m.Content})
		}
	}
	return out
}

func renderToolResult(m llm.Message) string {
	if m.ToolName != "" {
		return "Tool result (" + m.ToolName + "):\n" + m.Content
	}
	return "Tool result:\n" + m.Content
}

// schemaPrompt builds a small system instruction that names the JSON
// schema the model must follow. Returns "" for ShapeFreeform so the
// system messages array is left untouched.
func schemaPrompt(shape llm.JSONShape, tools []llm.ToolSpec) string {
	schema := llm.JSONSchemaFor(shape, tools)
	if schema == nil {
		return ""
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return ""
	}
	return "Respond with a single JSON object that conforms to this JSON schema. " +
		"Do not include any prose outside the JSON object.\n" +
		"Schema: " + string(encoded)
}

// snippet truncates a response body for inclusion in an error.
func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
