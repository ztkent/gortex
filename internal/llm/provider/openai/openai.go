// Package openai is the hosted OpenAI Chat Completions llm.Provider.
//
// It is pure Go — available in every build. Structured output uses the
// Chat Completions `response_format` field: a strict json_schema for
// the fixed list shapes (expand / rerank / verify), and a non-strict
// json_schema for the agent tool-call shape whose `args` object is
// intentionally open-ended. The agent tool-loop uses the emulated
// protocol — tool calls and results travel as plain text turns.
package openai

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

// Provider implements llm.Provider against api.openai.com.
type Provider struct {
	model   string
	apiKey  string
	baseURL string
	client  *http.Client
}

var _ llm.Provider = (*Provider)(nil)

// New constructs the OpenAI provider. The API key is read from the env
// var named by cfg.APIKeyEnv (default OPENAI_API_KEY); an unset key is
// a hard error.
func New(cfg llm.RemoteConfig) (llm.Provider, error) {
	keyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = "OPENAI_API_KEY"
	}
	key := strings.TrimSpace(os.Getenv(keyEnv))
	if key == "" {
		return nil, fmt.Errorf("openai: API key env %q is not set", keyEnv)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("openai: llm.openai.model is empty")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://api.openai.com"
	}
	return &Provider{
		model:   cfg.Model,
		apiKey:  key,
		baseURL: base,
		client:  &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "openai" }

// Close releases idle HTTP connections.
func (p *Provider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

// wire types for the Chat Completions API.
type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiRequest struct {
	Model          string         `json:"model"`
	Messages       []apiMessage   `json:"messages"`
	MaxTokens      int            `json:"max_completion_tokens,omitempty"`
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
		Messages:  mapMessages(req.Messages),
		MaxTokens: req.MaxTokens,
	}
	if rf := responseFormat(req.Shape, req.Tools); rf != nil {
		body.ResponseFormat = rf
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("openai: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("openai: request failed: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("openai: read response: %w", err)
	}

	var parsed apiResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("openai: decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return llm.CompletionResponse{}, fmt.Errorf("openai: API error (status %d): %s: %s", resp.StatusCode, parsed.Error.Type, parsed.Error.Message)
		}
		return llm.CompletionResponse{}, fmt.Errorf("openai: API error (status %d): %s", resp.StatusCode, snippet(payload))
	}
	if len(parsed.Choices) == 0 {
		return llm.CompletionResponse{}, errors.New("openai: response carried no choices")
	}
	return llm.CompletionResponse{Text: strings.TrimSpace(parsed.Choices[0].Message.Content)}, nil
}

// mapMessages flattens the provider-neutral conversation onto OpenAI
// chat roles. Tool observations become user turns (emulated tool-call
// protocol).
func mapMessages(in []llm.Message) []apiMessage {
	out := make([]apiMessage, 0, len(in))
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

// responseFormat builds the `response_format` payload. The fixed list
// shapes get a strict json_schema (they are fully strict-compliant);
// the tool-call shape gets a non-strict json_schema because its `args`
// object is deliberately unconstrained, which strict mode forbids.
// ShapeFreeform returns nil — no constraint.
func responseFormat(shape llm.JSONShape, tools []llm.ToolSpec) map[string]any {
	schema := llm.JSONSchemaFor(shape, tools)
	if schema == nil {
		return nil
	}
	strict := shape != llm.ShapeToolCall
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   schemaName(shape),
			"schema": schema,
			"strict": strict,
		},
	}
}

func schemaName(shape llm.JSONShape) string {
	switch shape {
	case llm.ShapeExpandTerms:
		return "expand_terms"
	case llm.ShapeRerankOrder:
		return "rerank_order"
	case llm.ShapeVerifyKeep:
		return "verify_keep"
	case llm.ShapeToolCall:
		return "tool_call"
	default:
		return "response"
	}
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
