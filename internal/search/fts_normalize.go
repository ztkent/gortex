package search

import (
	"os"
	"strings"
	"unicode"

	porterstemmer "github.com/blevesearch/go-porterstemmer"
)

// ftsStemmingEnabled gates the token-normalization pass — stopword
// removal plus Porter stemming — applied to the full-text-search index
// and query paths. Default OFF: on the recall fixture stemming trades
// exact-symbol-lookup precision (exact-tier R@5 −3.1pp) for broader
// recall (R@20 +5.7pp), so it ships as an opt-in rather than quietly
// reranking every identifier query. Enable it with
// GORTEX_FTS_STEMMING=1 (also true / yes / on).
//
// Read once at process start, like the bigram-typo flag: the index
// built during a daemon's lifetime and every query against it share a
// single setting, so a mid-session toggle can't desynchronise stemmed
// postings from stemmed query terms. When enabled, the same
// normalization runs on both the posting list and the query, so the
// two never disagree.
var ftsStemmingEnabled = ftsStemmingFromEnv()

func ftsStemmingFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_FTS_STEMMING"))) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}

// ftsStopWords is the stopword set: English glue words that carry
// no code-search signal. Deliberately tight — it holds only unambiguous
// function words, never code keywords (for / if / case / switch / type)
// or code-meaningful nouns (get / set / new / value / key), so dropping
// a member can never erase intent from a query. Tokens arrive already
// lowercased from Tokenize / TokenizeQuery.
var ftsStopWords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "and": {}, "or": {}, "but": {},
	"of": {}, "to": {}, "in": {}, "on": {}, "at": {}, "by": {},
	"with": {}, "as": {}, "is": {}, "are": {}, "was": {}, "were": {},
	"be": {}, "been": {}, "that": {}, "this": {}, "these": {},
	"those": {}, "from": {}, "into": {}, "than": {}, "then": {},
	"it": {}, "its": {}, "so": {}, "such": {}, "via": {}, "per": {},
}

// NormalizeFTSTokens applies the FR63 stopword filter and Porter stemmer
// to a token list produced by Tokenize / TokenizeQuery. The index path
// (BM25Backend.Add, BleveBackend.Add) and the query path
// (BM25Backend.Search, BleveBackend.Search) both call it, so a stemmed
// posting list is always probed with stemmed query terms.
//
// Stopwords are dropped before stemming so a stemmed form can never
// collide with a stopword entry. The result is a freshly allocated
// slice; the input is left untouched. When stemming is disabled the
// input slice is returned unchanged.
func NormalizeFTSTokens(tokens []string) []string {
	if !ftsStemmingEnabled || len(tokens) == 0 {
		return tokens
	}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, stop := ftsStopWords[t]; stop {
			continue
		}
		out = append(out, stemFTSToken(t))
	}
	return out
}

// stemFTSToken returns the Porter stem of one lowercase token. Tokens
// shorter than four runes are returned unchanged — Porter's measure-
// gated rules almost never fire that low, and the trailing-"s" rule
// that does would conflate distinct short code fragments (ids vs id).
// Tokens carrying a digit or any non-ASCII rune are returned verbatim:
// the Porter algorithm is defined over the English letter alphabet, so
// "sha256" or "utf8" must not be mangled.
func stemFTSToken(t string) string {
	if len(t) < 4 {
		return t
	}
	for _, r := range t {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) {
			return t
		}
	}
	return porterstemmer.StemString(t)
}
