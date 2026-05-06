package languages

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// goStringContext labels the API position the literal was found in.
// Used as a discriminator on KindString node IDs and on the
// EdgeEmits.Meta["context"] field so analyzers can filter by domain.
type goStringContext string

const (
	stringCtxMetric   goStringContext = "metric"
	stringCtxErrorMsg goStringContext = "error_msg"
	stringCtxRoute    goStringContext = "route"
)

// goMetricMethods is the whitelist of method names where the first
// string-literal argument is taken as the metric name. Limited to
// statsd / dogstatsd-style APIs (which always pass the metric name
// as the first arg). Prometheus needs a separate composite-literal
// extractor (CounterOpts{Name: "..."} etc.) — out of scope here.
//
// Generic method names like Set / Inc / Add are deliberately
// excluded — they appear on too many unrelated types.
var goMetricMethods = map[string]bool{
	"Increment":          true, // statsd, dogstatsd
	"Decrement":          true,
	"Count":              true,
	"Gauge":              true,
	"Histogram":          true,
	"Distribution":       true,
	"Timing":             true,
	"TimeInMilliseconds": true,
	"Event":              true, // dogstatsd Event
	"ServiceCheck":       true, // dogstatsd ServiceCheck
}

// goErrorMessageCalls maps (package-or-receiver, function) to the
// "error_msg" context. Both shapes look like selector calls
// syntactically, so we match against the receiver name as a
// heuristic — `errors.New(...)` and `fmt.Errorf(...)` are by far the
// dominant idioms.
var goErrorMessageCalls = map[[2]string]bool{
	{"errors", "New"}:    true,
	{"fmt", "Errorf"}:    true,
	{"xerrors", "New"}:   true, // golang.org/x/xerrors
	{"xerrors", "Errorf"}: true,
}

// goRouteMethods is the set of method names that, when called with a
// string-literal path-like first argument, emit a "route" node.
// Mixes net/http (HandleFunc/Handle), gorilla/mux (HandleFunc/Handle),
// chi (Get/Post/...), gin/echo (GET/POST/...), and a handful of
// common router shapes that don't go through the contracts pipeline.
var goRouteMethods = map[string]bool{
	"Handle":     true,
	"HandleFunc": true,
	"Get":        true,
	"Post":       true,
	"Put":        true,
	"Delete":     true,
	"Patch":      true,
	"Options":    true,
	"Head":       true,
	"Connect":    true,
	"Trace":      true,
	"GET":        true,
	"POST":       true,
	"PUT":        true,
	"DELETE":     true,
	"PATCH":      true,
	"OPTIONS":    true,
	"HEAD":       true,
	"CONNECT":    true,
	"TRACE":      true,
}

// goStringEvent is one deferred string-literal observation, queued
// during AST traversal and flushed at end-of-file by emitGoStringEvents.
type goStringEvent struct {
	context goStringContext
	method  string
	value   string
	line    int
}

// detectGoMetric checks a method call against the metric whitelist;
// returns the metric name when arg[0] is a string literal.
func detectGoMetric(callExpr *sitter.Node, method string, src []byte) (string, bool) {
	if callExpr == nil {
		return "", false
	}
	if !goMetricMethods[method] {
		return "", false
	}
	return firstStringLiteralArg(callExpr, src)
}

// detectGoErrorMessage checks for errors.New / fmt.Errorf-style calls
// where the first argument is a string literal.
func detectGoErrorMessage(callExpr *sitter.Node, receiver, method string, src []byte) (string, bool) {
	if callExpr == nil {
		return "", false
	}
	if !goErrorMessageCalls[[2]string{receiver, method}] {
		return "", false
	}
	return firstStringLiteralArg(callExpr, src)
}

// detectGoRoute checks for HTTP-router shapes where arg[0] is a
// path-like string literal. Path-likeness is enforced (must start
// with "/" or contain a "/" segment) to suppress false positives
// from generic method names like Get/Set on map-like types.
func detectGoRoute(callExpr *sitter.Node, method string, src []byte) (string, bool) {
	if callExpr == nil {
		return "", false
	}
	if !goRouteMethods[method] {
		return "", false
	}
	value, ok := firstStringLiteralArg(callExpr, src)
	if !ok {
		return "", false
	}
	if !looksLikeRoute(value) {
		return "", false
	}
	return value, true
}

// looksLikeRoute is a cheap sanity check — keeps map.Get("foo")
// and similar generics out of the route bucket. Accepts paths that
// start with "/" (most common), "GET /…" / "POST /…" mux-1.22 form,
// or a wildcard segment.
func looksLikeRoute(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "/") {
		return true
	}
	// net/http 1.22+ pattern syntax: "GET /foo".
	for _, m := range []string{"GET ", "POST ", "PUT ", "DELETE ", "PATCH ", "OPTIONS ", "HEAD "} {
		if strings.HasPrefix(s, m) {
			return true
		}
	}
	return false
}

// firstStringLiteralArg returns the value of the first string-literal
// argument of a call expression, with surrounding quotes stripped. The
// helper is shared with detectGoLogEvent's logic but kept separate so
// each context can apply its own filtering.
func firstStringLiteralArg(callExpr *sitter.Node, src []byte) (string, bool) {
	args := callExpr.ChildByFieldName("arguments")
	if args == nil {
		return "", false
	}
	for i := 0; i < int(args.NamedChildCount()); i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() != "interpreted_string_literal" && c.Type() != "raw_string_literal" {
			continue
		}
		text := strings.Trim(c.Content(src), "\"`")
		if text == "" {
			return "", false
		}
		return text, true
	}
	return "", false
}

// emitGoStringEvents creates one KindString node per (context, value)
// pair seen and an EdgeEmits from the enclosing function/method to
// each. Mirrors emitGoObservabilityEvents — same per-repo dedup
// behaviour, same caller-line lookup contract.
func emitGoStringEvents(events []goStringEvent, callerLookup func(line int) string, filePath string, result *parser.ExtractionResult) {
	if len(events) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(events))
	for _, e := range events {
		callerID := callerLookup(e.line)
		if callerID == "" {
			continue
		}
		strID := goStringNodeID(e.context, e.value)
		if _, ok := seen[strID]; !ok {
			seen[strID] = struct{}{}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       strID,
				Kind:     graph.KindString,
				Name:     e.value,
				FilePath: filePath, // first sighting; not authoritative
				Language: "go",
				Meta: map[string]any{
					"context": string(e.context),
					"value":   e.value,
				},
			})
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     callerID,
			To:       strID,
			Kind:     graph.EdgeEmits,
			FilePath: filePath,
			Line:     e.line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"context": string(e.context),
				"method":  e.method,
			},
		})
	}
}

// goStringNodeID composes the canonical synthetic ID for a string
// node. Long values (over 200 chars) are hashed to keep IDs sane —
// the original text is preserved in node.Name and node.Meta["value"].
func goStringNodeID(ctx goStringContext, value string) string {
	if len(value) > 200 {
		h := sha1.Sum([]byte(value))
		return "string::" + string(ctx) + "::sha1:" + hex.EncodeToString(h[:])[:16]
	}
	return "string::" + string(ctx) + "::" + value
}
