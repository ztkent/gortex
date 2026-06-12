package conversationlog

import "context"

// Meta carries the session / file / phase labels a recorder stamps onto
// each Record. Callers attach it to the context they pass into the LLM
// service so the service can label the turn without a direct dependency
// on the caller's session machinery.
type Meta struct {
	Session string
	Repo    string
	File    string
	Phase   string
}

type metaContextKey struct{}

// WithMeta returns a context carrying the conversation-log labels. The
// LLM service reads them via MetaFromContext when it records a turn.
func WithMeta(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, metaContextKey{}, m)
}

// MetaFromContext returns the labels attached by WithMeta, or the zero
// Meta when none are present.
func MetaFromContext(ctx context.Context) Meta {
	if ctx == nil {
		return Meta{}
	}
	if m, ok := ctx.Value(metaContextKey{}).(Meta); ok {
		return m
	}
	return Meta{}
}
