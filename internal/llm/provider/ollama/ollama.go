// Package ollama is the Ollama daemon llm.Provider.
//
// It is pure Go — available in every build. Ollama runs models
// locally (or on a remote host) and exposes an OpenAI-ish /api/chat
// endpoint. Structured output uses Ollama's `format` field, which
// accepts a JSON schema directly. The agent tool-loop uses the
// emulated protocol — tool calls and results travel as plain text
// turns.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/llm"
)

// Provider implements llm.Provider against an Ollama daemon.
type Provider struct {
	model  string
	host   string
	client *http.Client
}

var _ llm.Provider = (*Provider)(nil)

// New constructs the Ollama provider. Unlike the hosted providers
// there is no API key; New only requires a model tag and a reachable
// host (default http://localhost:11434). Reachability is not probed
// here — that surfaces on the first Complete call.
func New(cfg llm.OllamaConfig) (llm.Provider, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("ollama: llm.ollama.model is empty")
	}
	host := strings.TrimRight(strings.TrimSpace(cfg.Host), "/")
	if host == "" {
		host = "http://localhost:11434"
	}
	return &Provider{
		model:  cfg.Model,
		host:   host,
		client: &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "ollama" }

// Close releases idle HTTP connections.
func (p *Provider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

// wire types for the /api/chat endpoint.
type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiRequest struct {
	Model    string          `json:"model"`
	Messages []apiMessage    `json:"messages"`
	Stream   bool            `json:"stream"`
	Format   json.RawMessage `json:"format,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
}

type apiResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Error string `json:"error"`
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	body := apiRequest{
		Model:    p.model,
		Messages: mapMessages(req.Messages),
		Stream:   false,
	}
	if schema := llm.JSONSchemaFor(req.Shape, req.Tools); schema != nil {
		// Ollama's `format` accepts a JSON schema verbatim.
		encoded, err := json.Marshal(schema)
		if err != nil {
			return llm.CompletionResponse{}, fmt.Errorf("ollama: marshal schema: %w", err)
		}
		body.Format = encoded
	}
	if req.MaxTokens > 0 {
		body.Options = map[string]any{"num_predict": req.MaxTokens}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("ollama: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.host+"/api/chat", bytes.NewReader(raw))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("ollama: read response: %w", err)
	}

	var parsed apiResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("ollama: decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != "" {
			return llm.CompletionResponse{}, fmt.Errorf("ollama: API error (status %d): %s", resp.StatusCode, parsed.Error)
		}
		return llm.CompletionResponse{}, fmt.Errorf("ollama: API error (status %d): %s", resp.StatusCode, snippet(payload))
	}
	if parsed.Error != "" {
		return llm.CompletionResponse{}, fmt.Errorf("ollama: %s", parsed.Error)
	}
	return llm.CompletionResponse{Text: strings.TrimSpace(parsed.Message.Content)}, nil
}

// mapMessages flattens the provider-neutral conversation onto Ollama
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

// snippet truncates a response body for inclusion in an error.
func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
