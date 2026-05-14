//go:build llama

// Package local is the in-process llama.cpp llm.Provider. It wraps the
// CGO model/context from package llm and is the only provider that
// needs a `-tags llama` build; the non-llama build (stub.go) compiles
// a New that reports the provider as unavailable.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/llm"
)

// assistCtxSize is the KV-cache window for the short-call assist
// context (expand / rerank / verify). Sized for the heaviest user —
// verify with body + callers at ~3.5K tokens for 10 candidates.
const assistCtxSize = 4096

// defaultMaxTokens caps a Complete call whose request leaves
// MaxTokens unset.
const defaultMaxTokens = 512

// Provider is the local llama.cpp implementation of llm.Provider.
//
// It keeps two llama contexts behind separate mutexes: a small
// assistCtx for the structured search-assist shapes, and a full-size
// mainCtx for the agent tool-loop (ShapeToolCall) and freeform
// generation. Splitting them means a long agent run can't head-of-line
// block a hot-path assist call — at the llama.cpp level both share the
// model weights, each holds its own KV cache.
//
// Every Complete call is self-contained: it resets the context's KV
// cache and prefills the entire conversation passed in the request, so
// no cross-call state lives in a context. That makes per-call locking
// (rather than per-agent-run locking) correct even under concurrent
// callers.
type Provider struct {
	cfg  llm.LocalConfig
	tmpl chatTemplate

	loadOnce sync.Once
	loadErr  error
	model    *llm.Model

	assistMu  sync.Mutex
	assistCtx *llm.Context

	mainMu  sync.Mutex
	mainCtx *llm.Context
}

// compile-time assertion that *Provider satisfies the interface.
var _ llm.Provider = (*Provider)(nil)

// New constructs the local provider from its config sub-block. The
// model is NOT loaded here — that happens lazily on the first Complete
// call so daemon startup isn't slowed. New only validates that a model
// path is set and the file exists, and that the chat template is
// known, so misconfiguration surfaces immediately.
//
// Returns the llm.Provider interface (not the concrete *Provider) so
// the signature matches the non-llama stub and the provider factory
// can treat both builds uniformly.
func New(cfg llm.LocalConfig) (llm.Provider, error) {
	path := strings.TrimSpace(cfg.Model)
	if path == "" {
		return nil, errors.New("local: llm.local.model is empty")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("local: model file: %w", err)
	}
	tmpl, err := templateByName(cfg.Template)
	if err != nil {
		return nil, err
	}
	if cfg.Ctx <= 0 {
		cfg.Ctx = 4096
	}
	return &Provider{cfg: cfg, tmpl: tmpl}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "local" }

// ensureLoaded mmaps the model and allocates both contexts on first
// use. Idempotent; the stored loadErr is returned on every subsequent
// call once a load has failed.
func (p *Provider) ensureLoaded() error {
	p.loadOnce.Do(func() {
		m, err := llm.LoadModel(p.cfg.Model, p.cfg.GPULayers)
		if err != nil {
			p.loadErr = fmt.Errorf("local: load model: %w", err)
			return
		}
		assistCtx, err := m.NewContext(assistCtxSize, 0)
		if err != nil {
			m.Close()
			p.loadErr = fmt.Errorf("local: assist context: %w", err)
			return
		}
		mainCtx, err := m.NewContext(p.cfg.Ctx, 0)
		if err != nil {
			assistCtx.Close()
			m.Close()
			p.loadErr = fmt.Errorf("local: main context: %w", err)
			return
		}
		p.model = m
		p.assistCtx = assistCtx
		p.mainCtx = mainCtx
	})
	return p.loadErr
}

// Complete implements llm.Provider. It flattens the conversation
// through the chat template, installs the GBNF grammar implied by
// req.Shape, and runs greedy decoding with a JSON-complete early-stop.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	if err := ctx.Err(); err != nil {
		return llm.CompletionResponse{}, err
	}
	if err := p.ensureLoaded(); err != nil {
		return llm.CompletionResponse{}, err
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	prompt := p.tmpl.flatten(req.Messages)
	grammar := grammarForShape(req.Shape, req.Tools)
	structured := req.Shape != llm.ShapeFreeform

	llmCtx, mu := p.contextFor(req.Shape)
	mu.Lock()
	defer mu.Unlock()

	llmCtx.Reset()
	if err := llmCtx.SetGrammar(grammar); err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("local: install grammar: %w", err)
	}

	var buf strings.Builder
	_, err := llmCtx.Generate(prompt, maxTokens, func(piece string) bool {
		buf.WriteString(piece)
		// For a structured shape the grammar guarantees the output is
		// JSON; stop as soon as the top-level object closes and parses
		// instead of waiting on EOS. Freeform runs to EOS / maxTokens.
		if structured {
			return !jsonComplete(buf.String())
		}
		return true
	})
	if err != nil {
		return llm.CompletionResponse{}, err
	}
	return llm.CompletionResponse{Text: strings.TrimSpace(buf.String())}, nil
}

// contextFor routes a shape to its context + mutex. The structured
// search-assist shapes use the small assist context; the agent loop
// and freeform generation use the full-size main context.
func (p *Provider) contextFor(shape llm.JSONShape) (*llm.Context, *sync.Mutex) {
	switch shape {
	case llm.ShapeExpandTerms, llm.ShapeRerankOrder, llm.ShapeVerifyKeep:
		return p.assistCtx, &p.assistMu
	default:
		return p.mainCtx, &p.mainMu
	}
}

// Close releases the contexts and the model. Safe to call multiple
// times and before any Complete (when nothing was ever loaded).
func (p *Provider) Close() error {
	p.assistMu.Lock()
	if p.assistCtx != nil {
		p.assistCtx.Close()
		p.assistCtx = nil
	}
	p.assistMu.Unlock()

	p.mainMu.Lock()
	if p.mainCtx != nil {
		p.mainCtx.Close()
		p.mainCtx = nil
	}
	p.mainMu.Unlock()

	if p.model != nil {
		p.model.Close()
		p.model = nil
	}
	return nil
}

// jsonComplete reports whether s is a complete, parseable top-level
// JSON object — the early-stop predicate for grammar-constrained
// generation.
func jsonComplete(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return false
	}
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}
