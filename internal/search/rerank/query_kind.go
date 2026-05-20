package rerank

import (
	"strings"
	"unicode"
)

// QueryClass is the detected shape of a search query. The class tunes
// the bm25 ↔ semantic blend in two places: the per-signal weight
// scaling inside Pipeline.Rerank, and the α value of the hybrid
// alpha-fusion path in internal/search/hybrid.go. Identifier and path
// queries lean on exact-token (BM25) evidence; natural-language
// queries give the semantic channel its full weight.
type QueryClass int

const (
	// QueryClassUnknown is the zero value — "not yet classified". A
	// caller that leaves it unset lets Pipeline.Rerank auto-detect.
	QueryClassUnknown QueryClass = iota
	// QueryClassSymbol is a single identifier-shaped token: a symbol
	// or API name (validateToken, HTTPServer, pkg.Type). Exact-token
	// BM25 evidence dominates.
	QueryClassSymbol
	// QueryClassConcept is a natural-language description of intent
	// ("how does auth refresh", "validate user token"). The semantic
	// channel earns its keep here. This is the neutral baseline class.
	QueryClassConcept
	// QueryClassPath is a file-path-shaped query (internal/auth/token.go,
	// auth/handler). Path components are exact tokens and the semantic
	// channel is near-useless, so BM25 leans hardest of all classes.
	QueryClassPath
	// QueryClassSignature is a type- or function-signature fragment
	// ("func(ctx) error", "(string) bool"). Structural keywords carry
	// the signal: BM25-leaning but less extreme than a bare path.
	QueryClassSignature
)

// String returns the lowercase class name used by the search_symbols
// query_class argument and surfaced back on the response.
func (q QueryClass) String() string {
	switch q {
	case QueryClassSymbol:
		return "symbol"
	case QueryClassConcept:
		return "concept"
	case QueryClassPath:
		return "path"
	case QueryClassSignature:
		return "signature"
	default:
		return "unknown"
	}
}

// ParseQueryClass maps the search_symbols query_class argument to a
// QueryClass. "" and "auto" map to QueryClassUnknown — the signal to
// auto-detect. The bool is false for an unrecognised value so the
// caller can reject it with a clear error.
func ParseQueryClass(s string) (QueryClass, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return QueryClassUnknown, true
	case "symbol":
		return QueryClassSymbol, true
	case "concept":
		return QueryClassConcept, true
	case "path":
		return QueryClassPath, true
	case "signature":
		return QueryClassSignature, true
	default:
		return QueryClassUnknown, false
	}
}

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
	// AlphaPath weights BM25 vs semantic for file-path queries. The
	// most BM25-heavy blend: path components are exact tokens and the
	// semantic channel mostly contributes noise.
	AlphaPath = 0.15
	// AlphaSignature weights BM25 vs semantic for type/function-
	// signature fragments. BM25-leaning — structural keywords are
	// literal — but less extreme than a bare path.
	AlphaSignature = 0.35
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

// ClassifyQuery detects the QueryClass of a query with cheap
// structural heuristics — no LLM, no graph walk. Checks run in
// precedence order: signature markers (parentheses, arrows) are the
// most distinctive, then a path-separated single token, then the
// identifier-shape rubric IsSymbolQuery uses, and finally the
// natural-language default. An empty query classifies as concept.
func ClassifyQuery(query string) QueryClass {
	q := strings.TrimSpace(query)
	if q == "" {
		return QueryClassConcept
	}
	if looksLikeSignature(q) {
		return QueryClassSignature
	}
	if looksLikePath(q) {
		return QueryClassPath
	}
	if IsSymbolQuery(q) {
		return QueryClassSymbol
	}
	return QueryClassConcept
}

// looksLikeSignature reports whether the query carries an unambiguous
// type/function-signature marker — a parenthesis or a Go/JS-style
// return or lambda arrow. Natural-language queries virtually never do.
func looksLikeSignature(q string) bool {
	if strings.ContainsAny(q, "()") {
		return true
	}
	return strings.Contains(q, "->") || strings.Contains(q, "=>")
}

// looksLikePath reports whether the query is a single whitespace-free
// token carrying a directory separator — "internal/auth/token.go",
// "auth/handler". A "::" or bare "." qualifier is NOT a path: those
// stay in the symbol class.
func looksLikePath(q string) bool {
	if strings.ContainsAny(q, " \t\n") {
		return false
	}
	if !strings.ContainsAny(q, "/\\") {
		return false
	}
	return hasIdentifierChar(q)
}

// AlphaFor returns the recommended α blend value for the query. It
// classifies the query, then defers to AlphaForClass.
func AlphaFor(query string) float64 {
	return AlphaForClass(ClassifyQuery(query))
}

// AlphaForClass returns the α blend for a known class. The α-fusion
// formula in hybrid.go is `final = (1-α)·text_rrf + α·vector_rrf`, so
// a smaller α leans toward BM25. QueryClassUnknown falls back to the
// natural-language blend.
func AlphaForClass(c QueryClass) float64 {
	switch c {
	case QueryClassSymbol:
		return AlphaSymbol
	case QueryClassPath:
		return AlphaPath
	case QueryClassSignature:
		return AlphaSignature
	default: // QueryClassConcept, QueryClassUnknown
		return AlphaNL
	}
}

// classWeights holds the per-class scaling applied to the bm25 and
// semantic rerank signals. Every other signal scales by 1.0 — the
// per-class lever tunes only the text-vs-semantic balance and leaves
// the structural and session signals untouched.
type classWeights struct {
	bm25     float64
	semantic float64
}

// classWeightTable is the tuned per-class multiplier set. Concept is
// the neutral 1.0/1.0 baseline, so natural-language queries score
// exactly as they did before per-class weighting existed; the
// identifier, path, and signature classes push BM25 up and the
// semantic channel down by amounts that grow with how literal the
// query is.
var classWeightTable = map[QueryClass]classWeights{
	QueryClassConcept:   {bm25: 1.00, semantic: 1.00},
	QueryClassSymbol:    {bm25: 1.20, semantic: 0.65},
	QueryClassPath:      {bm25: 1.25, semantic: 0.45},
	QueryClassSignature: {bm25: 1.10, semantic: 0.80},
}

// ClassWeightMultiplier returns the factor applied to a signal's
// configured weight for a given query class. Only the bm25 and
// semantic signals are class-sensitive; every other signal — and an
// unknown class — returns 1.0.
func ClassWeightMultiplier(c QueryClass, signal string) float64 {
	cw, ok := classWeightTable[c]
	if !ok {
		return 1.0
	}
	switch signal {
	case SignalBM25:
		return cw.bm25
	case SignalSemantic:
		return cw.semantic
	default:
		return 1.0
	}
}
