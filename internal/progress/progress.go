// Package progress provides a small reporter interface for long-running
// operations (indexing, embedding, enrichment) to emit incremental updates.
// The MCP server uses this to convert indexer stage events into
// `notifications/progress` messages when the client supplies a progressToken.
package progress

import "context"

// Reporter receives progress updates. Implementations must be safe for
// concurrent use and must never panic — callers emit updates from hot paths.
type Reporter interface {
	// Report a progress tick. stage is a short human-readable label.
	// current and total are within-stage counters; total may be 0 when
	// the total work is unknown (e.g., before a walk completes).
	Report(stage string, current, total int)
}

// Nop is a Reporter that discards all updates. Returned by FromContext when
// no reporter is attached, so callers can unconditionally invoke Report.
type Nop struct{}

// Report does nothing.
func (Nop) Report(string, int, int) {}

type ctxKey struct{}

// WithReporter returns a context carrying the given reporter. Passing a nil
// reporter returns ctx unchanged so FromContext still yields Nop.
func WithReporter(ctx context.Context, r Reporter) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, r)
}

// FromContext returns the reporter in ctx, or Nop if none is attached.
// Never returns nil — callers may call Report directly on the result.
func FromContext(ctx context.Context) Reporter {
	if ctx == nil {
		return Nop{}
	}
	if r, ok := ctx.Value(ctxKey{}).(Reporter); ok && r != nil {
		return r
	}
	return Nop{}
}
