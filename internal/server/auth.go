package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// WithAuth wraps h with a bearer-token check. Every request must carry
// `Authorization: Bearer <token>`; mismatches get a 401 JSON error.
//
// When token is empty, WithAuth returns h unchanged — the caller has
// opted into unauthenticated mode (the server command enforces this is
// only safe with a localhost bind; see cmd/gortex/server.go).
//
// CORS preflights (OPTIONS) bypass the check so browsers on a different
// origin can negotiate headers before the real request is issued.
//
// As a browser-EventSource workaround the middleware also accepts the
// token via `?token=<t>`. Query-string auth leaks more readily into
// access logs and referrer headers than a Bearer header, so prefer the
// header when the client can set it.
func WithAuth(h http.Handler, token string) http.Handler {
	if token == "" {
		return h
	}
	expectedHeader := []byte("Bearer " + token)
	expectedToken := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			h.ServeHTTP(w, r)
			return
		}
		if authMatches([]byte(r.Header.Get("Authorization")), expectedHeader) {
			h.ServeHTTP(w, r)
			return
		}
		if q := r.URL.Query().Get("token"); q != "" && tokenMatches([]byte(q), expectedToken) {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="gortex"`)
		WriteJSONError(w, http.StatusUnauthorized, "missing or invalid bearer token")
	})
}

// authMatches does a constant-time comparison to defeat timing attacks
// on token validation.
func authMatches(got, expected []byte) bool {
	if !strings.HasPrefix(string(got), "Bearer ") {
		return false
	}
	return tokenMatches(got, expected)
}

func tokenMatches(got, expected []byte) bool {
	// subtle.ConstantTimeCompare needs equal-length slices to return 1.
	// For unequal lengths the answer is obviously "no match" but we
	// still scan a fixed-size buffer so an attacker can't learn the
	// token length from timing.
	if len(got) != len(expected) {
		_ = subtle.ConstantTimeCompare(expected, expected)
		return false
	}
	return subtle.ConstantTimeCompare(got, expected) == 1
}
