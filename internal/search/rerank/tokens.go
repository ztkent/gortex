package rerank

import (
	"strings"
	"unicode"
)

// tokenize lowercases and splits a string on non-alphanumeric and
// camelCase boundaries. The output is a deduplicated slice — the
// rerank signals only ever ask for *set* membership, so dropping
// duplicates here saves callers a step.
//
// Example: "ParseHTTPHeader" → ["parse", "http", "header"].
// Example: "validate_user_token" → ["validate", "user", "token"].
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	var (
		out  []string
		cur  strings.Builder
		seen = map[string]struct{}{}
	)
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		w := strings.ToLower(cur.String())
		cur.Reset()
		if _, ok := seen[w]; ok {
			return
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	var prev rune
	for i, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if i > 0 {
				// camelCase split: lowercase → uppercase.
				if unicode.IsUpper(r) && unicode.IsLower(prev) {
					flush()
				}
				// SCREAMING → Camel split: keep the last upper as
				// the start of the next token: HTTPHeader → HTTP +
				// Header.
				if unicode.IsUpper(r) && unicode.IsUpper(prev) && i+1 < len(s) {
					next := []rune(s[i:])[1]
					if unicode.IsLower(next) {
						flush()
					}
				}
				// digit ↔ letter boundary.
				if unicode.IsDigit(r) != unicode.IsDigit(prev) && cur.Len() > 0 {
					flush()
				}
			}
			cur.WriteRune(r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	return out
}

// Jaccard returns |A ∩ B| / |A ∪ B| over two token slices in [0, 1].
// Returns 0 when either is empty. Exported for use in tests and
// for callers that want a symmetric comparator.
func Jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	bset := make(map[string]struct{}, len(b))
	for _, t := range b {
		bset[t] = struct{}{}
	}
	aset := make(map[string]struct{}, len(a))
	for _, t := range a {
		aset[t] = struct{}{}
	}
	var inter int
	for t := range aset {
		if _, ok := bset[t]; ok {
			inter++
		}
	}
	union := len(aset) + len(bset) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// overlap returns |A ∩ B| / |A| — the fraction of query tokens that
// appear in the target. Asymmetric: a long signature matching all
// query tokens scores 1.0 even if it contains many extras.
func overlap(query, target []string) float64 {
	if len(query) == 0 || len(target) == 0 {
		return 0
	}
	tset := make(map[string]struct{}, len(target))
	for _, t := range target {
		tset[t] = struct{}{}
	}
	var hit int
	for _, t := range query {
		if _, ok := tset[t]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(query))
}
