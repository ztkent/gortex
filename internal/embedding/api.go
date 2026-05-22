package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRetryAfterWait caps how long the API provider will sleep on an
// HTTP 429 Retry-After hint. A hostile or mis-set header should not be
// able to stall indexing indefinitely; past this the request just
// fails and the caller aborts to text-only search.
const maxRetryAfterWait = 60 * time.Second

// APIProvider calls an external embedding API (Ollama or OpenAI-compatible).
type APIProvider struct {
	url    string
	model  string
	client *http.Client
	dims   int
	format apiFormat
}

type apiFormat int

const (
	formatOllama apiFormat = iota
	formatOpenAI
)

// NewAPIProvider creates a provider that calls an external embedding API.
// Auto-detects Ollama vs OpenAI format from the URL.
func NewAPIProvider(url, model string) *APIProvider {
	format := formatOpenAI
	if strings.Contains(url, "11434") || strings.Contains(url, "/api/") {
		format = formatOllama
	}

	if model == "" {
		if format == formatOllama {
			model = "nomic-embed-text"
		} else {
			model = "text-embedding-3-small"
		}
	}

	return &APIProvider{
		url:    strings.TrimRight(url, "/"),
		model:  model,
		client: &http.Client{Timeout: 30 * time.Second},
		format: format,
	}
}

func (p *APIProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embedding API returned no results")
	}
	return vecs[0], nil
}

func (p *APIProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if p.format == formatOllama {
		return p.embedOllama(ctx, texts)
	}
	return p.embedOpenAI(ctx, texts)
}

func (p *APIProvider) Dimensions() int { return p.dims }
func (p *APIProvider) Close() error    { return nil }

// Concurrent reports that this provider is safe — and worth — calling
// from several goroutines at once. An external HTTP embedding endpoint
// gains from overlapped round-trips; the indexer's embedding pool uses
// this to decide whether to parallelise.
func (p *APIProvider) Concurrent() bool { return true }

// doRequest issues req via the provider's HTTP client and returns the
// response. On an HTTP 429 it honours a Retry-After header (delta-
// seconds form) and retries once after sleeping — capped at
// maxRetryAfterWait so a bad header cannot stall indexing. The caller
// owns closing the returned body.
func (p *APIProvider) doRequest(ctx context.Context, req *http.Request, body []byte) (*http.Response, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	// Rate limited — read the hint, drain and close this response,
	// then retry exactly once.
	wait := parseRetryAfter(resp.Header.Get("Retry-After"))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	if wait <= 0 {
		// No usable hint — let the caller surface the 429 by re-issuing
		// once without a sleep would just hammer the API, so fall back
		// to a short fixed backoff.
		wait = time.Second
	}
	if wait > maxRetryAfterWait {
		wait = maxRetryAfterWait
	}
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// Rebuild the request — a *http.Request body is single-use.
	retry, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	retry.Header = req.Header.Clone()
	return p.client.Do(retry)
}

// parseRetryAfter parses the delta-seconds form of an HTTP Retry-After
// header ("Retry-After: 12"). The HTTP-date form is not handled —
// embedding APIs use delta-seconds in practice — and returns 0, which
// the caller treats as "no usable hint".
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// --- Ollama API ---

type ollamaRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"` // string or []string
}

type ollamaResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

func (p *APIProvider) embedOllama(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := ollamaRequest{
		Model: p.model,
		Input: texts,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := p.url + "/api/embed"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.doRequest(ctx, req, body)
	if err != nil {
		return nil, fmt.Errorf("API call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(result.Embeddings) > 0 && p.dims == 0 {
		p.dims = len(result.Embeddings[0])
	}

	return result.Embeddings, nil
}

// --- OpenAI API ---

type openAIRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openAIResponse struct {
	Data []openAIEmbedding `json:"data"`
}

type openAIEmbedding struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

func (p *APIProvider) embedOpenAI(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := openAIRequest{
		Model: p.model,
		Input: texts,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := p.url + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.doRequest(ctx, req, body)
	if err != nil {
		return nil, fmt.Errorf("API call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	vecs := make([][]float32, len(result.Data))
	for _, d := range result.Data {
		vecs[d.Index] = d.Embedding
	}

	if len(vecs) > 0 && p.dims == 0 && len(vecs[0]) > 0 {
		p.dims = len(vecs[0])
	}

	return vecs, nil
}
