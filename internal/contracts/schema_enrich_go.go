package contracts

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// -----------------------------------------------------------------------------
// Go — net/http (stdlib) provider
// -----------------------------------------------------------------------------

// Matches:
//
//	json.NewDecoder(r.Body).Decode(&req)
//	jsoniter.NewDecoder(r.Body).Decode(&req)
//	decoder.Decode(&req)
//
// The capture is the target variable; the detector then looks up its
// type inside the handler body or the file-scoped node list.
var goStdlibDecodeRe = regexp.MustCompile(`(?:json|jsoniter|decoder)\.?(?:NewDecoder)?\([^)]*\.Body\)?\.Decode\(\s*&?(\w+)\s*\)`)

// Matches:
//
//	json.Unmarshal(body, &req)  |  json.Unmarshal(data, req)
var goUnmarshalRe = regexp.MustCompile(`json\.Unmarshal\([^,]+,\s*&?(\w+)\s*\)`)

// Matches provider-side response encoders:
//
//	json.NewEncoder(w).Encode(resp)
//	WriteJSON(w, status, resp)
var goStdlibEncodeRe = regexp.MustCompile(`json\.NewEncoder\([^)]+\)\.Encode\(\s*&?(\w+)\s*\)`)

// JSON response helpers. Custom wrappers are the norm in handwritten
// Go servers — `respondJSON`, `writeJSON`, `sendJSON`, `renderJSON`,
// `h.json`, `render.JSON` all converge on the same (w, code, value)
// shape. Matching any of these gets us the status code and response
// value expression in one pass. The name capture is case-insensitive
// only for the leading letter so `WriteJSON` still matches.
var goWriteJSONRe = regexp.MustCompile(`\b(?:[A-Za-z_]\w*\.)?(?:[Rr]espond|[Ww]rite|[Ss]end|[Rr]ender)(?:JSON|Json)\(\s*\w+\s*,\s*([^,]+?)\s*,\s*([^)]+?)\s*\)`)

// (Envelope splitting now uses splitMapLiteralBody — a brace/bracket-
// balanced byte walker — rather than a regex. The regex form
// truncated nested literals like `[]any{evt1, evt2}` at the first
// `}` and produced unusable expr values like `"[]any{"`.)

// r.URL.Query().Get("x"), r.FormValue("x"), r.PostFormValue("x").
var goQueryParamRe = regexp.MustCompile(`\b(?:URL\.Query\(\)\.Get|FormValue|PostFormValue)\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]\s*\)`)

// w.WriteHeader(<expr>) — literal int or http.StatusX.
var goWriteHeaderRe = regexp.MustCompile(`\bWriteHeader\(\s*([^)]+?)\s*\)`)

// Return bare status literal: "return http.StatusBadRequest" in helpers.
var goStatusConstRe = regexp.MustCompile(`\bhttp\.(Status[A-Z]\w+)\b`)

func init() {
	// Provider-side detectors. Each one's regex is narrow enough that
	// running all of them on every Go provider handler doesn't cause
	// cross-framework false positives.
	schemaEnrichers = append(schemaEnrichers,
		schemaEnricher{
			name:      "go-stdlib-provider",
			languages: []string{"go"},
			roles:     []Role{RoleProvider},
			detect:    goNetHTTPDetect,
		},
		schemaEnricher{
			name:      "go-gin-provider",
			languages: []string{"go"},
			roles:     []Role{RoleProvider},
			detect:    goGinDetect,
		},
		schemaEnricher{
			name:      "go-fiber-provider",
			languages: []string{"go"},
			roles:     []Role{RoleProvider},
			detect:    goFiberDetect,
		},
		schemaEnricher{
			name:      "go-echo-provider",
			languages: []string{"go"},
			roles:     []Role{RoleProvider},
			detect:    goEchoDetect,
		},

		// Consumer side — picks up the outgoing payload and the
		// decode target around the call site. Same detector handles
		// all Go HTTP clients (stdlib, resty, etc.) because the
		// surrounding idioms are the same.
		schemaEnricher{
			name:      "go-consumer",
			languages: []string{"go"},
			roles:     []Role{RoleConsumer},
			detect:    goConsumerDetect,
		},
	)
}

// -----------------------------------------------------------------------------
// Go provider detectors
// -----------------------------------------------------------------------------

func goNetHTTPDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints

	if m := goStdlibDecodeRe.FindStringSubmatch(body); len(m) > 1 {
		setRequestType(&h, m[1], body, fileNodes, m[0])
	} else if m := goUnmarshalRe.FindStringSubmatch(body); len(m) > 1 {
		setRequestType(&h, m[1], body, fileNodes, m[0])
	}

	if m := goStdlibEncodeRe.FindStringSubmatch(body); len(m) > 1 {
		setResponseType(&h, m[1], body, fileNodes, m[0])
	} else if m := goWriteJSONRe.FindStringSubmatch(body); len(m) > 2 {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
		setResponseType(&h, m[2], body, fileNodes, m[0])
	}

	h.QueryParams = append(h.QueryParams, allSubmatches(body, goQueryParamRe, 1)...)
	for _, m := range goWriteHeaderRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	for _, m := range goStatusConstRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr("http." + m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	return h
}

// Gin: c.BindJSON / c.ShouldBindJSON / c.ShouldBind, c.JSON(status, obj),
// c.Query("x"), c.DefaultQuery("x", ...), c.Param("x").
var (
	ginBindRe   = regexp.MustCompile(`\b(?:ShouldBindJSON|BindJSON|ShouldBind)\(\s*&?(\w+)\s*\)`)
	ginJSONRe   = regexp.MustCompile(`\.JSON\(\s*([^,]+?)\s*,\s*([A-Za-z_][\w\.]*(?:\{[^}]*\})?)\s*\)`)
	ginQueryRe  = regexp.MustCompile(`\b(?:DefaultQuery|Query)\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]`)
	ginStatusRe = regexp.MustCompile(`\.Status\(\s*([^)]+?)\s*\)`)
)

func goGinDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints
	if m := ginBindRe.FindStringSubmatch(body); len(m) > 1 {
		setRequestType(&h, m[1], body, fileNodes, m[0])
	}
	for _, m := range ginJSONRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
		setResponseType(&h, m[2], body, fileNodes, m[0])
	}
	h.QueryParams = append(h.QueryParams, allSubmatches(body, ginQueryRe, 1)...)
	for _, m := range ginStatusRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	// Pick up any bare http.StatusX references too.
	for _, m := range goStatusConstRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr("http." + m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	return h
}

// Fiber: c.BodyParser(&req), c.JSON(obj), c.Status(200), c.Query("x").
var (
	fiberBindRe   = regexp.MustCompile(`\bBodyParser\(\s*&?(\w+)\s*\)`)
	fiberJSONRe   = regexp.MustCompile(`\.JSON\(\s*&?([A-Za-z_][\w\.]*(?:\{[^}]*\})?)\s*\)`)
	fiberQueryRe  = regexp.MustCompile(`\.Query\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]`)
	fiberStatusRe = regexp.MustCompile(`\.Status\(\s*([^)]+?)\s*\)`)
)

func goFiberDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints
	if m := fiberBindRe.FindStringSubmatch(body); len(m) > 1 {
		setRequestType(&h, m[1], body, fileNodes, m[0])
	}
	if m := fiberJSONRe.FindStringSubmatch(body); len(m) > 1 {
		setResponseType(&h, m[1], body, fileNodes, m[0])
	}
	h.QueryParams = append(h.QueryParams, allSubmatches(body, fiberQueryRe, 1)...)
	for _, m := range fiberStatusRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	for _, m := range goStatusConstRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr("http." + m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	return h
}

// Echo: c.Bind(&req), c.JSON(status, obj), c.QueryParam("x"),
// c.Param("x").
var (
	echoBindRe  = regexp.MustCompile(`\bc\.Bind\(\s*&?(\w+)\s*\)`)
	echoQueryRe = regexp.MustCompile(`\bQueryParam\(\s*["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]`)
)

func goEchoDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints
	if m := echoBindRe.FindStringSubmatch(body); len(m) > 1 {
		setRequestType(&h, m[1], body, fileNodes, m[0])
	}
	// Echo's JSON signature is identical to gin's, so the gin regex
	// also fires here via the shared enricher chain — the driver
	// merges. But we still pull status codes from h.WriteHeader etc.
	for _, m := range ginJSONRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr(m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
		setResponseType(&h, m[2], body, fileNodes, m[0])
	}
	h.QueryParams = append(h.QueryParams, allSubmatches(body, echoQueryRe, 1)...)
	for _, m := range goStatusConstRe.FindAllStringSubmatch(body, -1) {
		if code, ok := parseStatusExpr("http." + m[1]); ok {
			h.StatusCodes = append(h.StatusCodes, code)
		}
	}
	return h
}

// -----------------------------------------------------------------------------
// Go consumer detector — best-effort extraction of the outgoing payload
// and any decode target near the call site.
// -----------------------------------------------------------------------------

var (
	// Outbound body: json.Marshal(<expr>) within the call window.
	goMarshalRe = regexp.MustCompile(`json\.Marshal\(\s*&?(\w+)\s*\)`)
	// Decode target: json.NewDecoder(resp.Body).Decode(&result) or
	// json.Unmarshal(body, &result).
	goDecodeRespRe = regexp.MustCompile(`json\.NewDecoder\([^)]+\.Body\)\.Decode\(\s*&?(\w+)\s*\)`)
)

func goConsumerDetect(body string, fileNodes []*graph.Node) schemaHints {
	var h schemaHints
	if m := goMarshalRe.FindStringSubmatch(body); len(m) > 1 {
		setRequestType(&h, m[1], body, fileNodes, m[0])
	}
	if m := goDecodeRespRe.FindStringSubmatch(body); len(m) > 1 {
		setResponseType(&h, m[1], body, fileNodes, m[0])
	} else if m := goUnmarshalRe.FindStringSubmatch(body); len(m) > 1 {
		setResponseType(&h, m[1], body, fileNodes, m[0])
	}
	return h
}

// -----------------------------------------------------------------------------
// Shared helpers for Go detectors
// -----------------------------------------------------------------------------

// setRequestType records a request-type hint when the regex pattern
// captured an identifier that's directly a type name (e.g.
// `Decode(&Request{})` → "Request"). Variable-to-type resolution and
// envelope walking moved to the BodyFacts AST overlay (Phase 1 of
// spec-contract-extraction.md), which runs after this function and
// overrides the hint with structurally-correct facts.
//
// The body parameter is retained for signature symmetry with the
// callers (the regex detectors take body as their primary input).
func setRequestType(h *schemaHints, ident, body string, fileNodes []*graph.Node, matchText string) {
	_ = body
	if looksLikeType(ident) {
		h.RequestType = resolveTypeInFile(ident, fileNodes)
		return
	}
	if h.RequestExpr == "" {
		h.RequestExpr = strings.TrimSpace(matchText)
	}
}

// setResponseType records a response-type hint. Type resolution and
// envelope walking moved to the BodyFacts AST overlay (Phase 1 of
// spec-contract-extraction.md). This function only handles the cases
// the AST overlay can't override safely:
//
//	1. ident is itself a type name (e.g. `JSON(http.StatusOK, ErrResp{...})`)
//	2. ident is a compound literal expression — record as ResponseExpr
//	   for the post-pass to chase
//	3. ident is a bare identifier — record as ResponseExpr; the AST
//	   overlay will replace it if it can resolve the var binding
//
// The body parameter is retained for signature symmetry; it's no
// longer consulted (the AST overlay reads from the parse tree).
func setResponseType(h *schemaHints, ident, body string, fileNodes []*graph.Node, matchText string) {
	_ = body
	trimmed := strings.TrimSpace(ident)
	if looksLikeType(trimmed) {
		h.ResponseType = resolveTypeInFile(trimmed, fileNodes)
		return
	}
	if h.ResponseExpr == "" {
		switch {
		case isCompoundExpr(trimmed):
			h.ResponseExpr = trimmed
		case isPlainIdent(trimmed):
			h.ResponseExpr = trimmed
		default:
			h.ResponseExpr = strings.TrimSpace(matchText)
		}
	}
}

func isCompoundExpr(s string) bool {
	return strings.ContainsAny(s, "{[(")
}

// looksLikeType is a quick heuristic: starts with an uppercase letter,
// contains only identifier-ish characters. Filters out things like
// "err" or "nil" while keeping "LoginRequest" and "pkg.User".
func looksLikeType(s string) bool {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "&")
	s = strings.TrimPrefix(s, "*")
	if s == "" {
		return false
	}
	// Drop trailing literal `{}` so `Foo{}` still counts as a type.
	if i := strings.Index(s, "{"); i >= 0 {
		s = s[:i]
	}
	first := rune(s[0])
	if first < 'A' || first > 'Z' {
		// `pkg.User` — check the part after the last dot too.
		if i := strings.LastIndex(s, "."); i >= 0 {
			return looksLikeType(s[i+1:])
		}
		return false
	}
	return true
}

func allSubmatches(body string, re *regexp.Regexp, grp int) []string {
	ms := re.FindAllStringSubmatch(body, -1)
	if len(ms) == 0 {
		return nil
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if grp < len(m) {
			out = append(out, m[grp])
		}
	}
	return out
}
