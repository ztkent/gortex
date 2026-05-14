package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"

	"github.com/zzet/gortex/internal/parser"
)

// Go-side event pub/sub detection (F15). The Go extractor already walks
// every selector-style call (`callm.expr`); this file recognises the
// subset of those calls that publish to or subscribe from a message
// broker — NATS (`nc.Publish` / `nc.Subscribe` / `nc.QueueSubscribe`),
// Kafka (`w.WriteMessages` / `p.Produce` / `producer.SendMessage`),
// RabbitMQ (`ch.PublishWithContext` / `ch.Consume`), Redis go-redis
// (`rdb.Publish` / `rdb.Subscribe` / `rdb.PSubscribe`). Classification
// and graph-artifact emission live in the shared pubsub.go module; the
// Go-specific part is pulling the first string-literal argument out of
// a tree-sitter call_expression.

// goPubsubCandidate is a deferred pub/sub call site. Transport
// resolution + role classification happen in emitGoPubsubEvents once
// the file's full import set is known.
type goPubsubCandidate struct {
	method string
	topic  string
	line   int
}

// detectGoPubsubCall inspects a callm.expr capture and, when the method
// is a known pub/sub publish/subscribe operation with a string-literal
// topic argument, returns the candidate. The transport and role aren't
// resolved here — that needs the file's imports, which the extractor
// finishes collecting on the same tree walk.
func detectGoPubsubCall(callExpr *sitter.Node, method string, src []byte) (goPubsubCandidate, bool) {
	if callExpr == nil {
		return goPubsubCandidate{}, false
	}
	if _, ok := pubsubMethods[method]; !ok {
		return goPubsubCandidate{}, false
	}
	topic := firstGoStringLiteralArg(callExpr, src)
	if topic == "" {
		return goPubsubCandidate{}, false
	}
	return goPubsubCandidate{
		method: method,
		topic:  topic,
		line:   int(callExpr.StartPoint().Row) + 1,
	}, true
}

// firstGoStringLiteralArg returns the content of the first
// string-literal argument of a Go call_expression, or "" when no
// argument is a string literal. NATS / Redis pass the subject or
// channel as a positional string after a context argument
// (`rdb.Publish(ctx, "channel", payload)`), so scanning for the first
// literal anywhere in the argument list — rather than requiring it in
// position zero — catches the common broker call shapes.
func firstGoStringLiteralArg(callExpr *sitter.Node, src []byte) string {
	args := callExpr.ChildByFieldName("arguments")
	if args == nil {
		return ""
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
			continue
		}
		return text
	}
	return ""
}

// emitGoPubsubEvents resolves each deferred candidate against the
// file's imports and emits the KindEvent topic nodes + EdgeEmits /
// EdgeListensOn edges. callerLookup maps a 1-based line to the
// enclosing function ID.
func emitGoPubsubEvents(candidates []goPubsubCandidate, importPaths []string, callerLookup func(line int) string, filePath string, result *parser.ExtractionResult) {
	if len(candidates) == 0 {
		return
	}
	events := make([]pubsubEvent, 0, len(candidates))
	for _, c := range candidates {
		ev, ok := classifyPubsubCall(c.method, c.topic, importPaths, c.line)
		if !ok {
			continue
		}
		events = append(events, ev)
	}
	emitPubsubEvents(events, callerLookup, filePath, "go", result)
}
