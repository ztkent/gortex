package rerank

import (
	"strings"
	"unicode"
)

// Hybrid retrieval blend weights for the BM25 ↔ semantic mix used by
// the alpha-fusion path in internal/search/hybrid.go. Lower α leans
// toward BM25 (identifier-style queries where exact tokens dominate);
// higher α gives semantic search more weight (natural-language
// concept queries where wording varies).
//
// The values mirror the empirical sweet spot found by hybrid-search
// benchmarks (HyDE / RAGAS papers): symbol queries benefit from a
// strong BM25 prior; NL queries benefit from a near-balanced blend.
const (
	// AlphaSymbol weights BM25 vs semantic for identifier-shaped
	// queries (CamelCase, snake_case, namespaced, all-caps). The
	// blend favors BM25 because exact-token matches are the most
	// reliable signal for code identifiers.
	AlphaSymbol = 0.3
	// AlphaNL weights BM25 vs semantic for natural-language queries
	// ("validate user token", "auth middleware"). Both channels
	// contribute roughly equally — semantic catches synonymous
	// wording, BM25 catches literal keywords.
	AlphaNL = 0.5
)

// IsSymbolQuery returns true when the query looks like a code
// identifier rather than a natural-language description. The
// classification drives:
//
//   - the definition-keyword bias signal (only fires for symbol
//     queries — boosting `class Foo` for the query "Foo")
//   - the auto-adaptive α blend in hybrid retrieval (symbol queries
//     get AlphaSymbol, NL queries get AlphaNL)
//
// Heuristic: the query is a single token (no whitespace) that carries
// at least one structural marker — CamelCase, snake_case, dotted /
// double-colon / slash namespace qualifier, or an all-uppercase
// shape. A multi-word query (containing spaces) is always treated as
// NL even if individual tokens look identifier-shaped — the user is
// describing intent, not naming a symbol. Empty or whitespace-only
// queries return false.
func IsSymbolQuery(query string) bool {
	q := strings.TrimSpace(query)
	if q == "" {
		return false
	}
	// Multi-token queries are natural-language.
	for _, r := range q {
		if unicode.IsSpace(r) {
			return false
		}
	}
	// Namespace qualifiers: pkg.Type, Module::Symbol, dir/file::Sym,
	// path/to/foo.go. Any of these is a strong symbol indicator.
	for _, sep := range []string{"::", ".", "/", "\\"} {
		if strings.Contains(q, sep) && hasIdentifierChar(q) {
			return true
		}
	}
	// Snake-case identifier: at least one underscore between letters
	// or digits and no whitespace already established above.
	if strings.Contains(q, "_") && hasIdentifierChar(q) {
		return true
	}
	// CamelCase / PascalCase: lowercase→uppercase transition or
	// uppercase→lowercase after another uppercase (e.g. HTTPServer).
	var prev rune
	for i, r := range q {
		if i > 0 {
			if unicode.IsUpper(r) && unicode.IsLower(prev) {
				return true
			}
			if unicode.IsLower(r) && unicode.IsUpper(prev) {
				return true
			}
		}
		prev = r
	}
	// All-uppercase token of length >= 2 (e.g. URL, JWT, API). Single
	// uppercase letter is too ambiguous to flag.
	if len(q) >= 2 {
		allUpper := true
		hasLetter := false
		for _, r := range q {
			if unicode.IsLetter(r) {
				hasLetter = true
				if !unicode.IsUpper(r) {
					allUpper = false
					break
				}
			}
		}
		if hasLetter && allUpper {
			return true
		}
	}
	return false
}

// hasIdentifierChar reports whether the string contains at least one
// letter or digit. Used by IsSymbolQuery to avoid classifying punct-
// only strings ("::", ".", "/") as symbol queries.
func hasIdentifierChar(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// AlphaFor returns the recommended α blend value for the query. Use
// in hybrid retrieval to choose between AlphaSymbol (BM25-heavy) and
// AlphaNL (balanced). The α blend semantics are
// `final = α × text_score + (1-α) × vector_score` — lower α gives
// BM25 less weight relative to semantic. We flip that convention so
// symbol queries (BM25-favored) carry the SMALLER α and NL queries
// carry the LARGER α; the alphaFuse implementation in hybrid.go uses
// `final = (1-α) × text_rrf + α × vector_rrf` to match.
func AlphaFor(query string) float64 {
	if IsSymbolQuery(query) {
		return AlphaSymbol
	}
	return AlphaNL
}
