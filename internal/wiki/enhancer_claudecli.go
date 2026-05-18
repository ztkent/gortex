package wiki

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/llm"
)

// ClaudeCLIEnhancer wraps any llm.Provider (with the Claude CLI
// provider being the MVP target — but anything that satisfies the
// interface works) and runs one Complete call per section, with a
// content-addressable cache layered on top for determinism.
//
// The Enhance method is intentionally tolerant: if the provider
// errors, we return the original markdown so a flaky provider can't
// stop the wiki from being generated. The caller logs the error path.
type ClaudeCLIEnhancer struct {
	provider llm.Provider
	cache    *EnhanceCache
	maxTok   int
}

// NewClaudeCLIEnhancer constructs the enhancer. Pass nil cache to
// disable caching.
func NewClaudeCLIEnhancer(provider llm.Provider, cache *EnhanceCache) *ClaudeCLIEnhancer {
	return &ClaudeCLIEnhancer{
		provider: provider,
		cache:    cache,
		maxTok:   2048,
	}
}

// Enhance implements Enhancer.
func (e *ClaudeCLIEnhancer) Enhance(ctx context.Context, s EnhanceSection) (string, error) {
	if e == nil || e.provider == nil {
		return s.RawMarkdown, nil
	}

	// Cache lookup first. Determinism: identical inputs → identical
	// output without invoking the provider.
	var key string
	if e.cache != nil {
		key = e.cache.Key(s, e.provider.Name())
		if cached, hit, err := e.cache.Get(key); err == nil && hit {
			return cached, nil
		}
	}

	prompt := buildPrompt(s)
	req := llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: enhancerSystemPrompt},
			{Role: llm.RoleUser, Content: prompt},
		},
		MaxTokens: e.maxTok,
		Shape:     llm.ShapeFreeform,
	}
	resp, err := e.provider.Complete(ctx, req)
	if err != nil {
		// Provider failure → fall back to template content. The
		// wiki still ships.
		return s.RawMarkdown, fmt.Errorf("enhance %s: %w", s.Kind, err)
	}
	out := strings.TrimSpace(resp.Text)
	if out == "" {
		return s.RawMarkdown, nil
	}
	if e.cache != nil {
		_ = e.cache.Set(key, out)
	}
	return out, nil
}

const enhancerSystemPrompt = `You are a documentation writer for software-engineering reference docs. You receive a markdown page that was rendered from a code-intelligence graph. Your job is to make small, targeted prose improvements while preserving every table, code block, and link verbatim. You never invent symbols, files, or relationships. Return only the rewritten markdown with no surrounding commentary.`

func buildPrompt(s EnhanceSection) string {
	switch s.Kind {
	case "community":
		return fmt.Sprintf(promptCommunity, s.PageTitle, s.Context, s.RawMarkdown)
	case "process":
		return fmt.Sprintf(promptProcess, s.PageTitle, s.RawMarkdown)
	case "architecture":
		return fmt.Sprintf(promptArchitecture, s.PageTitle, s.RawMarkdown)
	default:
		return fmt.Sprintf("Improve the prose of this markdown page (%s). Keep tables, code, and links verbatim.\n\n%s", s.PageTitle, s.RawMarkdown)
	}
}
