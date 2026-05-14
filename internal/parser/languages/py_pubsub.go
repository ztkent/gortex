package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Python event pub/sub detection (F15). The Python extractor buffers
// every attribute-style call (`callattr.expr`); this file recognises
// the subset that publish to or subscribe from a message broker —
// NATS (`nc.publish` / `nc.subscribe`), Kafka (`producer.send`),
// RabbitMQ / pika (`channel.basic_publish` / `channel.basic_consume`),
// Redis (`r.publish` / `pubsub.subscribe` / `pubsub.psubscribe`).
// Classification, transport inference, and graph emission are the
// cross-language helpers in `pubsub.go`.

// pyPubsubKeywordKeys is the set of keyword-argument names that carry a
// pub/sub topic when the broker call names it as a keyword rather than
// a positional string. pika's `basic_publish(exchange=…, routing_key=…)`
// and `basic_consume(queue=…)` are the motivating cases.
var pyPubsubKeywordKeys = map[string]struct{}{
	"routing_key": {},
	"queue":       {},
	"exchange":    {},
	"subject":     {},
	"channel":     {},
	"topic":       {},
	"event":       {},
}

// detectPyPubsubCall inspects an attribute call and, when its method is
// a known pub/sub operation with a resolvable topic, returns the
// classified event. importPaths is the file's import set, used to
// disambiguate generic method names and infer the transport.
func detectPyPubsubCall(callExpr *sitter.Node, method string, src []byte, importPaths []string, line int) (pubsubEvent, bool) {
	if callExpr == nil {
		return pubsubEvent{}, false
	}
	if _, known := pubsubMethods[method]; !known {
		return pubsubEvent{}, false
	}
	topic := firstPyPubsubTopicArg(callExpr, src)
	if topic == "" {
		return pubsubEvent{}, false
	}
	return classifyPubsubCall(method, topic, importPaths, line)
}

// firstPyPubsubTopicArg pulls the topic string out of a Python `call`
// node — the first positional string literal, or the first recognised
// topic keyword argument (`routing_key=` / `queue=` / …). Returns ""
// for f-strings, concatenations, and non-string values.
func firstPyPubsubTopicArg(callExpr *sitter.Node, src []byte) string {
	args := callExpr.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i := 0; i < int(args.NamedChildCount()); i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "string":
			if s := pyStringLiteralContent(c, src); s != "" {
				return s
			}
		case "keyword_argument":
			nameNode := c.ChildByFieldName("name")
			valNode := c.ChildByFieldName("value")
			if nameNode == nil || valNode == nil || valNode.Type() != "string" {
				continue
			}
			key := strings.ToLower(nameNode.Content(src))
			if _, ok := pyPubsubKeywordKeys[key]; !ok {
				continue
			}
			if s := pyStringLiteralContent(valNode, src); s != "" {
				return s
			}
		}
	}
	return ""
}

// pyStringLiteralContent returns the body of a tree-sitter Python
// `string` node. The grammar splits the literal into `string_start`,
// `string_content`, and `string_end`; reading the `string_content`
// child drops the quotes and any prefix (r / b / f). A prefixed string
// (f-string / b-string) with interpolation is still returned by its
// literal content — callers gate on the method classification, not the
// string shape, and a stable-enough topic name is the common case.
func pyStringLiteralContent(strNode *sitter.Node, src []byte) string {
	for i := 0; i < int(strNode.NamedChildCount()); i++ {
		c := strNode.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "string_content":
			return strings.TrimSpace(c.Content(src))
		case "interpolation":
			// f-string with a substitution — no stable topic name.
			return ""
		}
	}
	// Fallback for grammars that don't split the body out: trim quotes
	// and any single-letter prefix.
	text := strings.TrimSpace(strNode.Content(src))
	text = strings.TrimLeft(text, "rbfRBF")
	return strings.TrimSpace(strings.Trim(text, "\"'"))
}
