// Package gemini is the hosted Google Gemini generateContent
// llm.Provider.
//
// It is pure Go — available in every build. Structured output uses
// Gemini's `responseSchema` + `responseMimeType: application/json` on
// the generationConfig: the model is constrained to emit JSON
// conforming to the requested JSONShape. The agent tool-loop uses the
// emulated protocol — tool calls and results travel as plain text
// turns — so a single llm.Message shape works across providers.
package gemini

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

// Provider implements llm.Provider against generativelanguage.googleapis.com.
type Provider struct {
	model   string
	apiKey  string
	baseURL string
	client  *http.Client
}

var _ llm.Provider = (*Provider)(nil)

// New constructs the Gemini provider. The API key is read from the
// env var named by cfg.APIKeyEnv (default GEMINI_API_KEY); an unset
// key is a hard error so misconfiguration surfaces at startup.
func New(cfg llm.RemoteConfig) (llm.Provider, error) {
	keyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = "GEMINI_API_KEY"
	}
	key := strings.TrimSpace(os.Getenv(keyEnv))
	if key == "" {
		return nil, fmt.Errorf("gemini: API key env %q is not set", keyEnv)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("gemini: llm.gemini.model is empty")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	return &Provider{
		model:   cfg.Model,
		apiKey:  key,
		baseURL: base,
		client:  &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "gemini" }

// Close releases idle HTTP connections.
func (p *Provider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

// wire types for the generateContent endpoint.
type apiPart struct {
	Text string `json:"text"`
}

type apiContent struct {
	Role  string    `json:"role,omitempty"`
	Parts []apiPart `json:"parts"`
}

type apiGenerationConfig struct {
	MaxOutputTokens  int            `json:"maxOutputTokens,omitempty"`
	ResponseMimeType string         `json:"responseMimeType,omitempty"`
	ResponseSchema   map[string]any `json:"responseSchema,omitempty"`
}

type apiRequest struct {
	SystemInstruction *apiContent          `json:"system_instruction,omitempty"`
	Contents          []apiContent         `json:"contents"`
	GenerationConfig  *apiGenerationConfig `json:"generationConfig,omitempty"`
}

type apiResponse struct {
	Candidates []struct {
		Content apiContent `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	system, contents := splitMessages(req.Messages)
	body := apiRequest{Contents: contents}
	if system != "" {
		body.SystemInstruction = &apiContent{Parts: []apiPart{{Text: system}}}
	}
	if cfg := buildGenerationConfig(req); cfg != nil {
		body.GenerationConfig = cfg
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("gemini: marshal request: %w", err)
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", p.baseURL, p.model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("gemini: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-goog-api-key", p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("gemini: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("gemini: read response: %w", err)
	}

	var parsed apiResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("gemini: decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return llm.CompletionResponse{}, fmt.Errorf("gemini: API error (status %d): %s: %s", resp.StatusCode, parsed.Error.Status, parsed.Error.Message)
		}
		return llm.CompletionResponse{}, fmt.Errorf("gemini: API error (status %d): %s", resp.StatusCode, snippet(payload))
	}
	if len(parsed.Candidates) == 0 {
		return llm.CompletionResponse{}, errors.New("gemini: response carried no candidates")
	}
	var b strings.Builder
	for _, part := range parsed.Candidates[0].Content.Parts {
		b.WriteString(part.Text)
	}
	return llm.CompletionResponse{Text: strings.TrimSpace(b.String())}, nil
}

// splitMessages pulls every RoleSystem message into the top-level
// `system_instruction` (Gemini carries system separately from the
// `contents` array) and maps the rest onto user/model turns. Tool
// observations become user turns (emulated tool-call protocol).
func splitMessages(in []llm.Message) (system string, contents []apiContent) {
	var sys []string
	for _, m := range in {
		switch m.Role {
		case llm.RoleSystem:
			if s := strings.TrimSpace(m.Content); s != "" {
				sys = append(sys, s)
			}
		case llm.RoleAssistant:
			contents = append(contents, apiContent{Role: "model", Parts: []apiPart{{Text: m.Content}}})
		case llm.RoleTool:
			contents = append(contents, apiContent{Role: "user", Parts: []apiPart{{Text: renderToolResult(m)}}})
		default:
			contents = append(contents, apiContent{Role: "user", Parts: []apiPart{{Text: m.Content}}})
		}
	}
	return strings.Join(sys, "\n\n"), contents
}

func renderToolResult(m llm.Message) string {
	if m.ToolName != "" {
		return "Tool result (" + m.ToolName + "):\n" + m.Content
	}
	return "Tool result:\n" + m.Content
}

// buildGenerationConfig assembles the `generationConfig` block. When
// the request is structured it sets responseMimeType:application/json
// and attaches a Gemini-compatible (additionalProperties-stripped)
// JSON schema as responseSchema. Returns nil when nothing needs
// setting — a freeform request without a token cap.
func buildGenerationConfig(req llm.CompletionRequest) *apiGenerationConfig {
	cfg := &apiGenerationConfig{}
	any := false
	if req.MaxTokens > 0 {
		cfg.MaxOutputTokens = req.MaxTokens
		any = true
	}
	if schema := llm.JSONSchemaFor(req.Shape, req.Tools); schema != nil {
		cfg.ResponseMimeType = "application/json"
		cfg.ResponseSchema = sanitiseSchema(schema)
		any = true
	}
	if !any {
		return nil
	}
	return cfg
}

// sanitiseSchema returns a deep copy of m with every
// `additionalProperties` key stripped recursively. Gemini's
// responseSchema is a subset of OpenAPI 3.0 that does not accept
// `additionalProperties`; leaving it in causes a 400 INVALID_ARGUMENT.
func sanitiseSchema(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "additionalProperties" {
			continue
		}
		switch vv := v.(type) {
		case map[string]any:
			out[k] = sanitiseSchema(vv)
		case []any:
			arr := make([]any, len(vv))
			for i, elt := range vv {
				if em, ok := elt.(map[string]any); ok {
					arr[i] = sanitiseSchema(em)
				} else {
					arr[i] = elt
				}
			}
			out[k] = arr
		default:
			out[k] = v
		}
	}
	return out
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
