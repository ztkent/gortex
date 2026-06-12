// Package tokens counts tokens the way LLMs actually count them, using the
// cl100k_base tokenizer (the encoding Claude and GPT-4 use). This replaces
// the `chars / 4` heuristic which undercounts code by 15-25%.
//
// All benchmarks, session metrics (`tokens_saved`, `tokens_returned`,
// `efficiency_ratio`) and budget calculations (`export_context`) go through
// this package, so any future tokenizer swap happens in one spot.
package tokens

const (
	// charsPerTokenFallback is the heuristic used when the real encoder fails
	// to initialize. Kept close to the old behaviour so metrics don't spike.
	charsPerTokenFallback = 4
)

// Count returns the number of tokens in s as counted by cl100k_base — the
// provider-neutral "how much content is this" measure. For a per-model
// estimate that picks the right tokenizer family, use CountFor. If the
// encoder failed to initialize for any reason, falls back to the legacy
// chars/4 heuristic rather than panicking — metrics stay usable.
func Count(s string) int {
	if s == "" {
		return 0
	}
	enc, err := encoderFor(encodingCL100K)
	if err != nil || enc == nil {
		return fallbackCount(s)
	}
	// EncodeOrdinary skips special-token handling — we're measuring raw content
	// tokens, not chat-formatted messages.
	return len(enc.EncodeOrdinary(s))
}

// CountInt64 is a convenience wrapper for call sites that store counts as int64
// (e.g. cumulative session metrics).
func CountInt64(s string) int64 {
	return int64(Count(s))
}

// TokensToChars approximates the character budget for a given token budget —
// used by export_context to decide how much source to include. Uses the
// inverse of the real chars/token ratio observed for code (~3.2) for a
// slightly tighter budget than the old *4 heuristic.
func TokensToChars(tokens int) int {
	if tokens <= 0 {
		return 0
	}
	// 3.2 chars per token is close to the empirical ratio on mixed code;
	// slightly conservative so we don't blow the budget.
	return tokens * 32 / 10
}

// EstimateFromSample estimates the token count of a body of `totalChars`
// characters using `sample` (a smaller chunk of the same content) to
// calibrate the chars-per-token ratio. Used by source-reading tools where
// we know the sample's text but only the full file's byte count — avoids
// re-reading the full file just to report `tokens_saved`.
//
// Falls back to the chars/4 heuristic when the sample is empty or the
// encoder is unavailable.
func EstimateFromSample(totalChars int, sample string) int {
	if totalChars <= 0 {
		return 0
	}
	if sample == "" {
		return totalChars / charsPerTokenFallback
	}
	// CachedCount, not Count: callers calibrate with a payload they have
	// typically just counted — the disk cache turns this second pass
	// over the same bytes into a hash lookup instead of a full BPE run
	// (~200ms per MB).
	sampleTokens := CachedCount(sample)
	if sampleTokens == 0 || len(sample) == 0 {
		return totalChars / charsPerTokenFallback
	}
	return int(int64(totalChars) * int64(sampleTokens) / int64(len(sample)))
}

// EncoderReady reports whether the tokenizer is loaded. Useful for tests and
// for surfacing fallback mode in telemetry.
func EncoderReady() bool {
	enc, err := encoderFor(encodingCL100K)
	return err == nil && enc != nil
}

// fallbackCount is the chars/4 heuristic used only when tiktoken fails.
func fallbackCount(s string) int {
	return len(s) / charsPerTokenFallback
}
