package daemon

import "context"

// RouteInputs is what a transport extracts from its inbound tools/call
// frame for routing: the tool name, the {"arguments":...} body
// RouteToolCall forwards verbatim, the session cwd, and an explicit
// workspace scope override (either may be empty).
type RouteInputs struct {
	ToolName string
	Body     []byte
	Cwd      string
	Scope    string
}

// Outcome tells the caller how to frame a routed tool call. When Proxied
// is true the route resolved to a remote (or a gate refusal) and
// Out/Status carry the response bytes to frame for the transport; when
// Proxied is false the caller runs its own in-process local dispatch.
type Outcome struct {
	Proxied bool
	Out     []byte
	Status  int
	Err     error // routing error (ErrRouteUnresolved etc.); caller may log
}

// ProxyDecision is the single peek→route→outcome helper shared by all
// three transports (the daemon AF_UNIX dispatcher, the /v1 HTTP handler,
// and the Streamable-HTTP transport). It owns the routing decision —
// computing the per-call effective enabled-set once and invoking
// RouteToolCall — while transport-specific frame parsing and RESPONSE
// FRAMING stay at each call site. The router is read through a dynamic
// accessor so a live ControlProxy swap is reflected.
type ProxyDecision struct {
	router func() *Router
}

// NewProxyDecision builds a decision helper over a dynamic router
// accessor (typically an atomic.Pointer[Router].Load).
func NewProxyDecision(router func() *Router) *ProxyDecision {
	return &ProxyDecision{router: router}
}

// Decide computes the effective enabled-set once (from the published
// roster overlaid with the session's overrides), threads it through
// RouteContext, runs RouteToolCall, and classifies the result. A nil
// router or an unresolved/local route yields Proxied=false (the caller
// dispatches locally); any resolved remote response or gate refusal
// yields Proxied=true with the bytes + status to frame.
func (d *ProxyDecision) Decide(ctx context.Context, in RouteInputs, sess *Session) Outcome {
	r := d.router()
	if r == nil {
		return Outcome{}
	}
	sid := ""
	if sess != nil {
		sid = sess.ID
	}
	out, status, err := r.RouteToolCall(ctx, in.ToolName, in.Body, RouteContext{
		Cwd:            in.Cwd,
		ScopeOverride:  in.Scope,
		SessionID:      sid,
		EnabledRemotes: r.EffectiveEnabledRemotes(sess),
	})
	if err != nil || status == 0 {
		// ErrRouteUnresolved or a routing failure — fall through to the
		// caller's local dispatch path (the same body works there).
		return Outcome{Err: err}
	}
	return Outcome{Proxied: true, Out: out, Status: status}
}
