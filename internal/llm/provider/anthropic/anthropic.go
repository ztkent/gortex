// Package anthropic is the hosted Anthropic Messages API llm.Provider.
//
// It is pure Go — available in every build, no `-tags llama` needed.
// Structured output (the expand / rerank / verify shapes and the agent
// tool-call shape) is obtained by declaring a single forced tool whose
// input_schema is the requested JSONShape: the model's tool_use block
// carries the structured JSON, which is marshaled back to text. The
// agent tool-loop itself uses the *emulated* protocol — tool calls and
// results travel as plain text turns — so a single llm.Message shape
// works across all four providers.
package anthropic

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

// anthropicVersion is the API version header value the Messages API
// requires.
const anthropicVersion = "2023-06-01"

// respondToolName is the synthetic tool used to force structured
// output. The model is given exactly this one tool with tool_choice
// pinned to it; its input is our JSON payload.
const respondToolName = "respond"

// Provider implements llm.Provider against api.anthropic.com.
type Provider struct {
	model   string
	apiKey  string
	baseURL string
	client  *http.Client
}

var _ llm.Provider = (*Provider)(nil)

// New constructs the Anthropic provider. The API key is read from the
// env var named by cfg.APIKeyEnv (default ANTHROPIC_API_KEY) — an
// unset key is a hard error so misconfiguration surfaces at startup,
// not on the first query.
func New(cfg llm.RemoteConfig) (llm.Provider, error) {
	keyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = "ANTHROPIC_API_KEY"
	}
	key := strings.TrimSpace(os.Getenv(keyEnv))
	if key == "" {
		return nil, fmt.Errorf("anthropic: API key env %q is not set", keyEnv)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("anthropic: llm.anthropic.model is empty")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &Provider{
		model:   cfg.Model,
		apiKey:  key,
		baseURL: base,
		client:  &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "anthropic" }

// Close releases idle HTTP connections.
func (p *Provider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

// wire types for the Messages API request/response.
type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type apiRequest struct {
	Model      string         `json:"model"`
	MaxTokens  int            `json:"max_tokens"`
	System     string         `json:"system,omitempty"`
	Messages   []apiMessage   `json:"messages"`
	Tools      []apiTool      `json:"tools,omitempty"`
	ToolChoice map[string]any `json:"tool_choice,omitempty"`
}

type apiContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type apiResponse struct {
	Content []apiContentBlock `json:"content"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	system, msgs := splitMessages(req.Messages)
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}

	body := apiRequest{
		Model:     p.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  msgs,
	}
	structured := req.Shape != llm.ShapeFreeform
	if structured {
		body.Tools = []apiTool{{
			Name:        respondToolName,
			Description: "Return your response as the structured arguments of this tool.",
			InputSchema: llm.JSONSchemaFor(req.Shape, req.Tools),
		}}
		body.ToolChoice = map[string]any{"type": "tool", "name": respondToolName}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("anthropic: read response: %w", err)
	}

	var parsed apiResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("anthropic: decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return llm.CompletionResponse{}, fmt.Errorf("anthropic: API error (status %d): %s: %s", resp.StatusCode, parsed.Error.Type, parsed.Error.Message)
		}
		return llm.CompletionResponse{}, fmt.Errorf("anthropic: API error (status %d): %s", resp.StatusCode, snippet(payload))
	}

	text, err := extractText(parsed.Content, structured)
	if err != nil {
		return llm.CompletionResponse{}, err
	}
	return llm.CompletionResponse{Text: text}, nil
}

// splitMessages pulls every RoleSystem message into the top-level
// `system` string (Anthropic carries system separately from the
// messages array) and maps the rest onto user/assistant turns. Tool
// observations are rendered as user turns — the emulated tool-call
// protocol — which keeps the user/assistant alternation the API
// requires intact.
func splitMessages(in []llm.Message) (system string, msgs []apiMessage) {
	var sys []string
	for _, m := range in {
		switch m.Role {
		case llm.RoleSystem:
			if s := strings.TrimSpace(m.Content); s != "" {
				sys = append(sys, s)
			}
		case llm.RoleAssistant:
			msgs = append(msgs, apiMessage{Role: "assistant", Content: m.Content})
		case llm.RoleTool:
			msgs = append(msgs, apiMessage{Role: "user", Content: renderToolResult(m)})
		default: // RoleUser and anything unexpected
			msgs = append(msgs, apiMessage{Role: "user", Content: m.Content})
		}
	}
	return strings.Join(sys, "\n\n"), msgs
}

// renderToolResult formats a RoleTool message as a plain-text user
// turn for the emulated tool-call protocol.
func renderToolResult(m llm.Message) string {
	if m.ToolName != "" {
		return "Tool result (" + m.ToolName + "):\n" + m.Content
	}
	return "Tool result:\n" + m.Content
}

// extractText pulls the response text out of the content blocks. For a
// structured request it returns the forced tool's input JSON; for a
// freeform request it concatenates the text blocks.
func extractText(blocks []apiContentBlock, structured bool) (string, error) {
	if structured {
		for _, b := range blocks {
			if b.Type == "tool_use" && b.Name == respondToolName && len(b.Input) > 0 {
				return strings.TrimSpace(string(b.Input)), nil
			}
		}
		return "", errors.New("anthropic: response carried no forced-tool output")
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return strings.TrimSpace(b.String()), nil
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
