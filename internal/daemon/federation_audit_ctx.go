package daemon

import "context"

// auditInfo carries the request-scoped fields the federation fan-out adds
// to its audit lines so a federated remote call records the same
// {session_id, cwd, tool, target_slug} tuple as the single-remote proxy
// route. It rides the context because the Federator's Augment/fanOut take
// the request ctx but not the router's RouteContext.
type auditInfo struct {
	Cwd       string
	SessionID string
}

type auditInfoKey struct{}

// withAuditInfo attaches the caller's cwd + session id to ctx for the
// fan-out audit. A no-op when both are empty (control/HTTP paths).
func withAuditInfo(ctx context.Context, cwd, sessionID string) context.Context {
	if cwd == "" && sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, auditInfoKey{}, auditInfo{Cwd: cwd, SessionID: sessionID})
}

// auditInfoFrom returns the audit fields carried on ctx, or the zero
// value when none were attached.
func auditInfoFrom(ctx context.Context) auditInfo {
	v, _ := ctx.Value(auditInfoKey{}).(auditInfo)
	return v
}
