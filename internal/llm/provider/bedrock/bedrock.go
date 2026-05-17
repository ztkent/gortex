// Package bedrock is the hosted AWS Bedrock Converse API llm.Provider.
//
// It is pure Go — available in every build, no AWS SDK dependency.
// Requests are SigV4-signed against bedrock-runtime.<region>.amazonaws.com.
// Structured output is obtained by declaring a single forced tool
// (analogous to the Anthropic forced-tool pattern) — the model's
// toolUse block carries the structured JSON, which is marshaled back
// to text. The agent tool-loop uses the *emulated* protocol — tool
// calls and results travel as plain text turns — so a single
// llm.Message shape works across all providers.
package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/llm"
)

// respondToolName is the synthetic tool that forces structured
// output. Pinned via toolChoice so the model must invoke it.
const respondToolName = "respond"

// Provider implements llm.Provider against bedrock-runtime.<region>.amazonaws.com.
type Provider struct {
	modelID      string
	region       string
	accessKey    string
	secretKey    string
	sessionToken string
	baseURL      string
	client       *http.Client
	now          func() time.Time // overridable for tests
}

var _ llm.Provider = (*Provider)(nil)

// New constructs the Bedrock provider. Credentials are read from the
// env vars named by cfg.AccessKeyEnv / cfg.SecretKeyEnv (defaults
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY); a missing key id or
// secret is a hard error. The session token env var
// (default AWS_SESSION_TOKEN) is optional — set only when using
// STS-issued credentials.
func New(cfg llm.BedrockConfig) (llm.Provider, error) {
	if strings.TrimSpace(cfg.ModelID) == "" {
		return nil, errors.New("bedrock: llm.bedrock.model_id is empty")
	}
	accessEnv := strings.TrimSpace(cfg.AccessKeyEnv)
	if accessEnv == "" {
		accessEnv = "AWS_ACCESS_KEY_ID"
	}
	secretEnv := strings.TrimSpace(cfg.SecretKeyEnv)
	if secretEnv == "" {
		secretEnv = "AWS_SECRET_ACCESS_KEY"
	}
	sessionEnv := strings.TrimSpace(cfg.SessionTokenEnv)
	if sessionEnv == "" {
		sessionEnv = "AWS_SESSION_TOKEN"
	}
	access := strings.TrimSpace(os.Getenv(accessEnv))
	secret := strings.TrimSpace(os.Getenv(secretEnv))
	if access == "" || secret == "" {
		return nil, fmt.Errorf("bedrock: AWS credentials missing — set %s and %s", accessEnv, secretEnv)
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "us-east-1"
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://bedrock-runtime." + region + ".amazonaws.com"
	}
	return &Provider{
		modelID:      cfg.ModelID,
		region:       region,
		accessKey:    access,
		secretKey:    secret,
		sessionToken: strings.TrimSpace(os.Getenv(sessionEnv)),
		baseURL:      base,
		client:       &http.Client{Timeout: 120 * time.Second},
		now:          time.Now,
	}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "bedrock" }

// Close releases idle HTTP connections.
func (p *Provider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}

// wire types for the Converse API.
type apiTextBlock struct {
	Text string `json:"text,omitempty"`
}

type apiContentBlock struct {
	Text    string       `json:"text,omitempty"`
	ToolUse *apiToolUse  `json:"toolUse,omitempty"`
}

type apiToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type apiMessage struct {
	Role    string            `json:"role"`
	Content []apiContentBlock `json:"content"`
}

type apiInferenceConfig struct {
	MaxTokens int `json:"maxTokens,omitempty"`
}

type apiToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

type apiTool struct {
	ToolSpec apiToolSpec `json:"toolSpec"`
}

type apiToolConfig struct {
	Tools      []apiTool      `json:"tools"`
	ToolChoice map[string]any `json:"toolChoice,omitempty"`
}

type apiRequest struct {
	Messages        []apiMessage        `json:"messages"`
	System          []apiTextBlock      `json:"system,omitempty"`
	InferenceConfig *apiInferenceConfig `json:"inferenceConfig,omitempty"`
	ToolConfig      *apiToolConfig      `json:"toolConfig,omitempty"`
}

type apiResponse struct {
	Output struct {
		Message apiMessage `json:"message"`
	} `json:"output"`
	Message string `json:"message"` // AWS error body shape
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	system, msgs := splitMessages(req.Messages)
	body := apiRequest{Messages: msgs}
	if len(system) > 0 {
		body.System = []apiTextBlock{{Text: strings.Join(system, "\n\n")}}
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	body.InferenceConfig = &apiInferenceConfig{MaxTokens: maxTokens}
	structured := req.Shape != llm.ShapeFreeform
	if structured {
		body.ToolConfig = &apiToolConfig{
			Tools: []apiTool{{
				ToolSpec: apiToolSpec{
					Name:        respondToolName,
					Description: "Return your response as the structured arguments of this tool.",
					InputSchema: map[string]any{"json": llm.JSONSchemaFor(req.Shape, req.Tools)},
				},
			}},
			ToolChoice: map[string]any{"tool": map[string]any{"name": respondToolName}},
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("bedrock: marshal request: %w", err)
	}
	// `url.PathEscape` leaves `:` alone (it's a legal pchar per
	// RFC 3986); AWS Bedrock model IDs contain `:` (e.g.
	// "anthropic.claude-sonnet-4-20250514-v1:0") which must be
	// percent-encoded on the wire — otherwise the path the server
	// sees differs from what we signed.
	escaped := strings.ReplaceAll(url.PathEscape(p.modelID), ":", "%3A")
	endpoint := p.baseURL + "/model/" + escaped + "/converse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("bedrock: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	sign(httpReq, raw, sigV4Creds{
		AccessKey:    p.accessKey,
		SecretKey:    p.secretKey,
		SessionToken: p.sessionToken,
		Region:       p.region,
		Service:      "bedrock",
	}, p.now())

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("bedrock: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("bedrock: read response: %w", err)
	}

	var parsed apiResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("bedrock: decode response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Message != "" {
			return llm.CompletionResponse{}, fmt.Errorf("bedrock: API error (status %d): %s", resp.StatusCode, parsed.Message)
		}
		return llm.CompletionResponse{}, fmt.Errorf("bedrock: API error (status %d): %s", resp.StatusCode, snippet(payload))
	}

	text, err := extractText(parsed.Output.Message.Content, structured)
	if err != nil {
		return llm.CompletionResponse{}, err
	}
	return llm.CompletionResponse{Text: text}, nil
}

// splitMessages pulls every RoleSystem message into the top-level
// `system` array (Bedrock carries system separately from `messages`)
// and maps the rest onto user/assistant turns. Tool observations
// become user turns (emulated tool-call protocol).
func splitMessages(in []llm.Message) (system []string, msgs []apiMessage) {
	for _, m := range in {
		switch m.Role {
		case llm.RoleSystem:
			if s := strings.TrimSpace(m.Content); s != "" {
				system = append(system, s)
			}
		case llm.RoleAssistant:
			msgs = append(msgs, apiMessage{Role: "assistant", Content: []apiContentBlock{{Text: m.Content}}})
		case llm.RoleTool:
			msgs = append(msgs, apiMessage{Role: "user", Content: []apiContentBlock{{Text: renderToolResult(m)}}})
		default:
			msgs = append(msgs, apiMessage{Role: "user", Content: []apiContentBlock{{Text: m.Content}}})
		}
	}
	return system, msgs
}

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
			if b.ToolUse != nil && b.ToolUse.Name == respondToolName && len(b.ToolUse.Input) > 0 {
				return strings.TrimSpace(string(b.ToolUse.Input)), nil
			}
		}
		return "", errors.New("bedrock: response carried no forced-tool output")
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Text != "" {
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
