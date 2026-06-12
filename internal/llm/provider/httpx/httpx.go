// Package httpx carries the retry behaviour shared by the pure-Go HTTP
// LLM providers (anthropic, openai, ollama, gemini, bedrock, deepseek).
//
// Some endpoints occasionally answer an otherwise valid request with
// an HTTP 200 whose body carries no completion at all — an empty
// choices array, a candidate with no text parts, a blank message. The
// cause is upstream (a truncated stream, a transient capacity blip, a
// safety filter that returned nothing); it is not a client error and a
// fresh attempt usually succeeds. Surfacing it as an empty completion
// instead lets the blank answer leak into the assist / agent layers.
//
// Complete wraps the request/parse cycle in a bounded retry: a hollow
// 200 is retried with exponential backoff, and a genuine transport or
// API error is returned immediately (the caller already classifies
// those). After the final attempt a hollow 200 becomes a clear error.
package httpx

import (
	"context"
	"fmt"
	"time"
)

// maxAttempts bounds how many times Complete will issue the request.
// A hollow 200 is rare and self-clears quickly, so two extra attempts
// past the first cover the realistic failure window without turning a
// persistently misbehaving endpoint into a long stall.
const maxAttempts = 3

// baseBackoff is the delay before the second attempt; each further
// attempt doubles it (200ms, then 400ms). The total added latency on
// the worst path stays well under a second — small next to the
// multi-second completion it is protecting.
const baseBackoff = 200 * time.Millisecond

// Usage carries the per-attempt token accounting a provider decodes out
// of a successful response body. Each provider's attempt populates the
// fields its API reports; a provider that does not surface usage leaves
// them zero. The values are provider-neutral — the caller maps them onto
// llm.TokenUsage.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Result is the outcome of one provider request attempt. Hollow marks
// an HTTP 200 that carried no usable completion — the one condition
// Complete retries; everything else (a transport failure, a non-200
// API error, a decode failure) is returned to the caller as Err.
type Result struct {
	// Text is the extracted completion. Meaningful only when Err is
	// nil and Hollow is false.
	Text string
	// Hollow is true when the request succeeded at the HTTP layer
	// (status 200, body decoded) but yielded no completion content.
	Hollow bool
	// Err is a transport, status, or decode failure. A non-nil Err
	// is terminal — Complete does not retry it.
	Err error
	// Usage is the token accounting decoded from the response. Zero
	// when the provider does not report usage. Carried back to the
	// caller by CompleteWithUsage; Complete drops it.
	Usage Usage
}

// Attempt performs one full provider request: build the HTTP request,
// execute it, read and decode the body, and extract the completion
// text. The provider supplies this; httpx only sequences the retries.
type Attempt func(ctx context.Context) Result

// Complete runs attempt with a bounded hollow-200 retry. It returns
// the first non-hollow result's text (success or terminal error). If
// every attempt yields a hollow 200 it returns a clear error naming the
// provider rather than an empty completion.
//
// provider is the short provider name ("openai", "ollama", …) used
// only in the exhausted-retries error message. Complete is the
// backward-compatible entry point: it drops the per-attempt Usage so
// existing callers keep their (string, error) signature. A provider
// that wants the token accounting calls CompleteWithUsage instead.
func Complete(ctx context.Context, provider string, attempt Attempt) (string, error) {
	text, _, err := CompleteWithUsage(ctx, provider, attempt)
	return text, err
}

// CompleteWithUsage is Complete plus the token accounting from the
// winning attempt. It runs the same bounded hollow-200 retry and
// returns the text, the Usage decoded by the successful attempt (zero
// when the provider does not report it), and any terminal error.
func CompleteWithUsage(ctx context.Context, provider string, attempt Attempt) (string, Usage, error) {
	var res Result
	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			// Exponential backoff: baseBackoff << (i-1). Abort the
			// wait early if the caller's context is cancelled.
			delay := baseBackoff << (i - 1)
			select {
			case <-ctx.Done():
				return "", Usage{}, ctx.Err()
			case <-time.After(delay):
			}
		}
		res = attempt(ctx)
		if res.Err != nil {
			return "", Usage{}, res.Err
		}
		if !res.Hollow {
			return res.Text, res.Usage, nil
		}
	}
	return "", Usage{}, fmt.Errorf("%s: endpoint returned an empty completion after %d attempts (HTTP 200, no content)", provider, maxAttempts)
}
