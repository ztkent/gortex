package rerank

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// DefinitionBiasSignal rewards candidates that are the *definition*
// of the queried symbol, as opposed to one of its callers, an import,
// or a comment mention. Only fires for queries that look like an
// identifier (CamelCase, snake_case, namespaced, all-caps) — for
// natural-language queries it contributes 0 because there is no
// single symbol to "define".
//
// The signal does NOT scan source text — the graph already carries
// the kind metadata we need:
//
//   - Function / method / type / interface / class / struct / enum
//     definitions are *definitions* of the named symbol
//   - Import edges, references, callers are *uses*, not definitions
//
// Matching tiers (returned in [0, 1]):
//
//   - 1.0  candidate is a definition kind AND its name matches the
//          query case-insensitively (Exact)
//   - 0.6  candidate is a definition kind AND its file stem matches
//          the query case-insensitively (Stem) — useful when the
//          query is "FooBar" and `FooBar.go` defines a related type
//   - 0.4  candidate is a definition kind AND its name shares the
//          first 4+ chars of the query (case-insensitive Prefix) —
//          weakest tier, handles abbreviations
//   - 0.0  none of the above (NL query, or a non-definition node, or
//          name/path mismatch)
//
// The pipeline weight scales how strongly the signal pushes
// definitions to the top. At weight 0.6 + tier 1.0 the boost is
// roughly the same magnitude as one full unit of fan-in, which is
// the desired calibration: definition + load-bearing > definition
// alone > use sites.
type DefinitionBiasSignal struct{}

// Name returns the canonical signal name registered in DefaultWeights.
func (DefinitionBiasSignal) Name() string { return SignalDefinitionBias }

// Contribute returns the definition-bias score in [0, 1].
func (DefinitionBiasSignal) Contribute(query string, c *Candidate, _ *Context) float64 {
	if c == nil || c.Node == nil {
		return 0
	}
	if !IsSymbolQuery(query) {
		return 0
	}
	if !isDefinitionKind(c.Node.Kind) {
		return 0
	}
	q := strings.TrimSpace(strings.ToLower(query))
	if q == "" {
		return 0
	}
	name := strings.ToLower(c.Node.Name)
	if name != "" {
		if name == q {
			return 1.0
		}
		if longCommonPrefix(name, q, 4) {
			// File-stem match below is stronger than a name prefix
			// match because file stems are more deliberate, so we
			// compute both and return the max.
			if stemMatches(c.Node.FilePath, q) {
				return 0.6
			}
			return 0.4
		}
	}
	if stemMatches(c.Node.FilePath, q) {
		return 0.6
	}
	return 0
}

// isDefinitionKind reports whether a graph node kind represents the
// canonical declaration of a symbol (function body, type body,
// interface body, etc.) as opposed to a reference or import.
func isDefinitionKind(k graph.NodeKind) bool {
	switch k {
	case
		graph.KindFunction,
		graph.KindMethod,
		graph.KindType,
		graph.KindInterface,
		graph.KindConstant,
		graph.KindVariable,
		graph.KindField,
		graph.KindEnumMember:
		return true
	}
	return false
}

// longCommonPrefix reports whether a and b share at least n leading
// runes (case-insensitive — caller has already lowered both). Used
// for the weaker "FooBar matches FooBarServer / FooBarLoader" tier.
func longCommonPrefix(a, b string, n int) bool {
	if len(a) < n || len(b) < n {
		return false
	}
	// Both inputs are pre-lowered; byte comparison is equivalent to
	// case-insensitive comparison for the ASCII range identifiers
	// use. Non-ASCII identifiers fall through to false, which is the
	// safe default for a soft-signal heuristic.
	for i := range n {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// stemMatches reports whether the file's stem (basename minus
// extension) matches the lowered query. Used as the secondary
// "file is named after the symbol" tier.
func stemMatches(filePath, loweredQuery string) bool {
	if filePath == "" || loweredQuery == "" {
		return false
	}
	// Take the last segment and strip the extension. Avoids pulling
	// in path/filepath because the graph stores forward-slash paths.
	idx := strings.LastIndexAny(filePath, "/\\")
	base := filePath
	if idx >= 0 {
		base = filePath[idx+1:]
	}
	if dot := strings.LastIndex(base, "."); dot > 0 {
		base = base[:dot]
	}
	return strings.EqualFold(base, loweredQuery)
}
