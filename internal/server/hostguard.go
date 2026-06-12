package server

import (
	"net"
	"net/http"
	"strings"
)

// Route-scoped DNS-rebinding guard for the conversation-log routes.
//
// The conversation routes egress raw LLM request/response text, so they
// carry a tighter check than the rest of the HTTP surface. The guard is
// deliberately NOT a global Host-allowlist layer in ServeHTTP — adding
// one there would 403 a legitimately token-authed non-loopback dashboard
// deployment. Instead it is applied only inside the conversation
// handlers and COOPERATES with the existing --http-auth-token model: a
// request is allowed when EITHER its Host is loopback / explicitly
// allowlisted, OR a valid auth token was presented. An un-authed
// cross-origin / DNS-rebind request to a conversation route is the only
// thing rejected.

// guardConversationRoute reports whether a request to a /v1/conversations*
// route is allowed. It passes when the Host is loopback or in the
// allowlist, or when a valid auth token was presented (tokenOK). Only an
// un-authed, non-loopback, non-allowlisted request is rejected.
func guardConversationRoute(r *http.Request, allow []string, tokenOK bool) bool {
	if tokenOK {
		return true
	}
	return hostAllowed(r.Host, allow)
}

// hostAllowed reports whether host (a request's Host header, possibly
// with a port) is a loopback address/name or appears in the extra
// allowlist. An empty Host is treated as not allowed.
func hostAllowed(host string, extra []string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	hostname = strings.TrimSuffix(strings.TrimPrefix(hostname, "["), "]")
	if isLoopbackHost(hostname) {
		return true
	}
	for _, a := range extra {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		// Compare against the bare hostname and the host[:port] as given,
		// so an allowlist entry may name either form.
		if strings.EqualFold(a, hostname) || strings.EqualFold(a, host) {
			return true
		}
	}
	return false
}

// isLoopbackHost reports whether a bare hostname is the loopback name or
// a loopback IP literal (127.0.0.0/8, ::1).
func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	if ip := net.ParseIP(hostname); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
