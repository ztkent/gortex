package svc

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/zzet/gortex/internal/llm"
)

// Token caps per assist call. Expansion emits at most a small JSON
// list; rerank emits one id per candidate; verify emits one id per
// surviving candidate. Handed to the provider as CompletionRequest
// .MaxTokens — the provider applies its own structured-output
// early-stop on top.
const (
	expandMaxTokens = 192
	rerankMaxTokens = 512
	verifyMaxTokens = 512
)

// ExpandQuery turns a natural-language search query into a small set
// of related identifier-style terms via one structured completion.
// Result is cached by query string. Empty / blank input returns an
// empty result without touching the provider.
//
// The caller is expected to OR the returned terms with the original
// query and rerank by combined BM25 score.
func (s *Service) ExpandQuery(ctx context.Context, query string) (*llm.ExpandResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return &llm.ExpandResult{Original: query}, nil
	}
	if cached, ok := s.expandCache.Get(query); ok {
		return &llm.ExpandResult{Original: query, Terms: cached, Cached: true}, nil
	}
	if s.provider == nil {
		return nil, errServiceUnavailable
	}

	resp, err := s.provider.Complete(ctx, llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: llm.ExpandSystemPrompt(s.profile)},
			{Role: llm.RoleUser, Content: "Query: " + query},
		},
		MaxTokens: expandMaxTokens,
		Shape:     llm.ShapeExpandTerms,
	})
	if err != nil {
		return nil, err
	}

	terms := parseStringList(resp.Text, "terms")
	terms = dedupeFilter(terms, query)
	// Even an empty result is worth caching — re-issuing the prompt
	// won't change a model that consistently emits nothing useful.
	s.expandCache.Set(query, terms)
	return &llm.ExpandResult{Original: query, Terms: terms}, nil
}

// RerankSymbols asks the provider to reorder a candidate set by
// relevance to the query. IDs the model drops are appended at the
// tail in original input order so the caller never loses a candidate.
// Empty input returns an empty order without touching the provider.
//
// The cache key includes the candidate ID set so two callers passing
// the same query against different candidate pools each get their own
// cache entry; ordering of input candidates does not affect the key.
func (s *Service) RerankSymbols(ctx context.Context, query string, cands []llm.RerankCandidate) (*llm.RerankResult, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(cands) == 0 {
		return &llm.RerankResult{Order: candIDs(cands)}, nil
	}

	key := rerankCacheKey(query, cands)
	if cached, ok := s.rerankCache.Get(key); ok {
		return &llm.RerankResult{Order: cached, Cached: true}, nil
	}
	if s.provider == nil {
		return nil, errServiceUnavailable
	}

	resp, err := s.provider.Complete(ctx, llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: llm.RerankSystemPrompt(s.profile)},
			{Role: llm.RoleUser, Content: buildRerankUser(query, cands)},
		},
		MaxTokens: rerankMaxTokens,
		Shape:     llm.ShapeRerankOrder,
	})
	if err != nil {
		// Surface the error but keep input order intact so the caller
		// can still return *something* — search-assist must never
		// degrade below baseline BM25 quality.
		return &llm.RerankResult{Order: candIDs(cands)}, err
	}

	rawOrder := parseStringList(resp.Text, "order")
	order := filterToInputAppend(rawOrder, cands)
	s.rerankCache.Set(key, order)
	return &llm.RerankResult{Order: order}, nil
}

// VerifyRelevance reads each candidate's code body and returns only
// the IDs the model judges genuinely related to the query — an empty
// list means "no candidate's code actually does what was asked",
// which is a load-bearing honest-negative signal the caller should
// preserve rather than fall back to BM25 noise.
//
// The cache key includes (query, sorted IDs, body hash) so a
// re-indexed codebase doesn't return stale verifications. Empty input
// short-circuits without touching the provider.
//
// On any inference or parse failure, returns the input order
// unchanged with the error — the caller should treat that as "could
// not verify" rather than "nothing matched".
func (s *Service) VerifyRelevance(ctx context.Context, query string, cands []llm.VerifyCandidate) (*llm.VerifyResult, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(cands) == 0 {
		return &llm.VerifyResult{Keep: verifyIDs(cands)}, nil
	}

	key := verifyCacheKey(query, cands)
	if cached, ok := s.verifyCache.Get(key); ok {
		return &llm.VerifyResult{Keep: cached, Cached: true}, nil
	}
	if s.provider == nil {
		return nil, errServiceUnavailable
	}

	resp, err := s.provider.Complete(ctx, llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: llm.VerifySystemPrompt(s.profile)},
			{Role: llm.RoleUser, Content: buildVerifyUser(query, cands)},
		},
		MaxTokens: verifyMaxTokens,
		Shape:     llm.ShapeVerifyKeep,
	})
	if err != nil {
		// On failure, surface the error and keep all input candidates
		// — better to over-include than to silently drop them.
		return &llm.VerifyResult{Keep: verifyIDs(cands)}, err
	}

	rawKeep := parseStringList(resp.Text, "keep")
	keep := filterKeepToInput(rawKeep, cands)
	s.verifyCache.Set(key, keep)
	return &llm.VerifyResult{Keep: keep}, nil
}

// buildVerifyUser formats the candidate list for the body-grounded
// verify prompt. Each candidate ships with its body and a compact
// callers block — the callers carry independent contextual signal
// that lets the model distinguish "same operation, different data"
// cases the body alone can't disambiguate. Bodies and signatures
// must be pre-truncated by the caller — this is a formatter, not the
// place to enforce length limits.
func buildVerifyUser(query string, cands []llm.VerifyCandidate) string {
	var b strings.Builder
	b.WriteString("Query: ")
	b.WriteString(query)
	b.WriteString("\n\nCandidates:\n")
	for _, c := range cands {
		b.WriteString(c.ID)
		b.WriteString(" | ")
		b.WriteString(c.Name)
		if sig := strings.TrimSpace(c.Signature); sig != "" {
			b.WriteString(" | ")
			if len(sig) > 160 {
				sig = sig[:160] + "…"
			}
			b.WriteString(sig)
		}
		b.WriteString("\nbody:\n")
		if body := strings.TrimSpace(c.Body); body != "" {
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteString("\n")
			}
		} else {
			b.WriteString("(no body — signature-only)\n")
		}
		if len(c.Callers) > 0 {
			b.WriteString("callers:\n")
			for _, cl := range c.Callers {
				b.WriteString("- ")
				b.WriteString(cl.Name)
				if sig := strings.TrimSpace(cl.Signature); sig != "" {
					b.WriteString(" | ")
					if len(sig) > 120 {
						sig = sig[:120] + "…"
					}
					b.WriteString(sig)
				}
				b.WriteString("\n")
			}
		} else {
			b.WriteString("callers: (none indexed)\n")
		}
		b.WriteString("---\n")
	}
	return b.String()
}

// buildRerankUser formats the candidate list for the rerank prompt.
// One line per candidate: "id | name | signature?". Truncates very
// long signatures so a single noisy entry can't blow the context.
func buildRerankUser(query string, cands []llm.RerankCandidate) string {
	var b strings.Builder
	b.WriteString("Query: ")
	b.WriteString(query)
	b.WriteString("\nCandidates:\n")
	for _, c := range cands {
		b.WriteString("- ")
		b.WriteString(c.ID)
		b.WriteString(" | ")
		b.WriteString(c.Name)
		if sig := strings.TrimSpace(c.Signature); sig != "" {
			b.WriteString(" | ")
			if len(sig) > 120 {
				sig = sig[:120] + "…"
			}
			b.WriteString(sig)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// parseStringList extracts a top-level JSON string array under the
// given key. Returns nil on any parse failure — the caller decides
// the fallback behaviour.
func parseStringList(raw, key string) []string {
	if raw == "" {
		return nil
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	var out []string
	if err := json.Unmarshal(v, &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// expansionStoplist is the conservative list of generic English nouns
// that the BM25 layer matches against thousands of unrelated symbols
// (e.g. `function`, `data`, `library`). These rarely carry useful
// search signal on their own and almost always inflate the candidate
// pool with noise. Members were chosen by inspecting real expansion
// outputs from Qwen2.5-Coder 3B against the gortex corpus — words
// that produced no relevant additional hits but many irrelevant ones.
//
// Borderline / domain-bearing words like `encryption`, `algorithm`,
// `security`, `key` are deliberately NOT here: they can be load-bearing
// in some codebases. Keep this list short — over-filtering throws
// away the only signal expansion has to offer.
var expansionStoplist = map[string]bool{
	"function": true, "functions": true, "method": true, "methods": true,
	"library": true, "libraries": true,
	"module": true, "modules": true, "package": true, "packages": true,
	"system": true, "systems": true,
	"service": true, "services": true,
	"code": true, "codes": true, "source": true,
	"data": true, "datum": true,
	"value": true, "values": true,
	"object": true, "objects": true, "item": true, "items": true,
	"thing": true, "things": true,
	"info": true, "information": true,
	"content": true, "contents": true,
	"stuff":   true,
	"general": true, "common": true, "basic": true, "simple": true, "main": true,
	"text": true,
	// Generic verbs/nouns that slip through with NL queries — observed
	// in the wild: "where is the rerank logic for search results" pulled
	// in "logic" as an expansion term, which broadens BM25 enormously
	// against any *_logic or logical_* identifier.
	"logic": true, "logical": true,
	"process": true, "processing": true,
	"handle": true, "handler": true, "handling": true,
	"flow": true, "flows": true,
	"action": true, "actions": true,
	"helper": true, "helpers": true,
	"util": true, "utils": true, "utility": true, "utilities": true,
}

// minExpansionTermLen rejects terms shorter than this. Sub-3 char
// fragments (`do`, `is`, `id`) generate huge BM25 hit lists and
// almost never carry useful signal. The threshold is conservative —
// short identifiers like `js`, `db`, `ui` get through.
const minExpansionTermLen = 3

// maxExpansionTerms caps the per-call expansion regardless of model
// output. Each extra term adds a BM25 sweep + candidate-pool growth,
// so trimming aggressively saves both latency and rerank prompt size.
const maxExpansionTerms = 5

// dedupeFilter trims, lowercases for comparison, and drops terms that
// are empty, duplicates, the original query, in expansionStoplist, or
// shorter than minExpansionTermLen. Preserves order of the surviving
// terms. The cap at maxExpansionTerms keeps the merged candidate pool
// bounded even when the model ignores the "2 to 5" prompt instruction.
func dedupeFilter(terms []string, query string) []string {
	queryLower := strings.ToLower(strings.TrimSpace(query))
	seen := map[string]bool{queryLower: true}
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		k := strings.ToLower(t)
		if seen[k] || expansionStoplist[k] {
			continue
		}
		if len(t) < minExpansionTermLen {
			continue
		}
		seen[k] = true
		out = append(out, t)
		if len(out) >= maxExpansionTerms {
			break
		}
	}
	return out
}

// candIDs extracts just the ID slice from a candidate list,
// preserving order. Returned for fallback paths so the caller still
// gets a valid (if unhelpful) ordering.
func candIDs(cands []llm.RerankCandidate) []string {
	if len(cands) == 0 {
		return nil
	}
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.ID
	}
	return out
}

// verifyIDs is the VerifyCandidate equivalent of candIDs — used on
// fallback paths where we want to preserve every input ID rather than
// drop them silently.
func verifyIDs(cands []llm.VerifyCandidate) []string {
	if len(cands) == 0 {
		return nil
	}
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.ID
	}
	return out
}

// filterKeepToInput is the VerifyResult equivalent of
// filterToInputAppend but with one critical difference: dropped IDs
// are NOT appended at the tail. An empty result IS the load-bearing
// honest-negative signal, so callers must see exactly what the model
// decided to keep.
//
// Hallucinated and duplicate IDs are still filtered defensively.
func filterKeepToInput(modelKeep []string, cands []llm.VerifyCandidate) []string {
	valid := make(map[string]bool, len(cands))
	for _, c := range cands {
		valid[c.ID] = true
	}
	used := make(map[string]bool, len(cands))
	out := make([]string, 0, len(modelKeep))
	for _, id := range modelKeep {
		if !valid[id] || used[id] {
			continue
		}
		used[id] = true
		out = append(out, id)
	}
	return out
}

// filterToInputAppend builds the final rerank order: every model ID
// that matches an input candidate, in model-supplied order, then any
// remaining input IDs in their original order. This makes the result
// a stable permutation of the input set even when the model drops or
// hallucinates entries.
func filterToInputAppend(modelOrder []string, cands []llm.RerankCandidate) []string {
	valid := make(map[string]bool, len(cands))
	for _, c := range cands {
		valid[c.ID] = true
	}
	used := make(map[string]bool, len(cands))
	out := make([]string, 0, len(cands))
	for _, id := range modelOrder {
		if !valid[id] || used[id] {
			continue
		}
		used[id] = true
		out = append(out, id)
	}
	for _, c := range cands {
		if !used[c.ID] {
			out = append(out, c.ID)
		}
	}
	return out
}
