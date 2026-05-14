package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// JavaScript / TypeScript event pub/sub detection (F15). The JS and TS
// extractors share the same tree-sitter call_expression / string /
// object node shapes, so the AST-shaped work lives here once and both
// `javascript.go` and `typescript.go` funnel their buffered member
// calls through it. Classification, transport inference, and graph
// emission are the cross-language helpers in `pubsub.go`.
//
// Two argument shapes are recognised:
//
//   - positional string — EventEmitter / Socket.IO / NATS.js / ioredis
//     name the channel as the first string literal
//     (`socket.emit("chat:message", …)`, `nc.subscribe("orders.*")`).
//   - object option — kafkajs names it as a property of the first
//     argument object (`producer.send({ topic: "orders" })`,
//     `consumer.subscribe({ topic: "orders" })`).

// jsPubsubObjectKeys is the set of object-literal property names that
// carry a pub/sub topic when the broker call takes an options object
// instead of a positional string (kafkajs and friends).
var jsPubsubObjectKeys = map[string]struct{}{
	"topic":      {},
	"channel":    {},
	"subject":    {},
	"event":      {},
	"queue":      {},
	"exchange":   {},
	"routingkey": {},
	"name":       {},
}

// detectJSPubsubCall inspects a member call_expression and, when its
// method is a known pub/sub operation with a resolvable topic, returns
// the classified event. importPaths is the file's import set, used to
// disambiguate generic method names (emit / on / send / publish) and to
// infer the transport. ok is false when the call is not a pub/sub
// operation.
func detectJSPubsubCall(callExpr *sitter.Node, method string, src []byte, importPaths []string, line int) (pubsubEvent, bool) {
	if callExpr == nil {
		return pubsubEvent{}, false
	}
	if _, known := pubsubMethods[method]; !known {
		return pubsubEvent{}, false
	}
	topic := firstJSPubsubTopicArg(callExpr, src)
	if topic == "" {
		return pubsubEvent{}, false
	}
	return classifyPubsubCall(method, topic, importPaths, line)
}

// firstJSPubsubTopicArg pulls the topic string out of a JS/TS
// call_expression — the first positional string literal, or the first
// recognised topic property of a leading options object. Returns "" for
// template strings, computed names, and non-string values, which the
// heuristic deliberately can't resolve to a stable topic node.
func firstJSPubsubTopicArg(callExpr *sitter.Node, src []byte) string {
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
			if s := jsStringLiteralContent(c, src); s != "" {
				return s
			}
		case "object":
			if s := jsObjectTopicValue(c, src); s != "" {
				return s
			}
		}
	}
	return ""
}

// jsStringLiteralContent returns the unquoted content of a tree-sitter
// `string` node. The grammar wraps the body in a `string_fragment`
// child; an empty literal (`""`) has no fragment and yields "".
func jsStringLiteralContent(strNode *sitter.Node, src []byte) string {
	for i := 0; i < int(strNode.NamedChildCount()); i++ {
		c := strNode.NamedChild(i)
		if c != nil && c.Type() == "string_fragment" {
			return strings.TrimSpace(c.Content(src))
		}
	}
	// Fallback: trim the surrounding quote characters directly.
	return strings.TrimSpace(strings.Trim(strNode.Content(src), "\"'`"))
}

// jsObjectTopicValue scans an object literal for the first `pair` whose
// key is a recognised pub/sub topic property (topic / channel /
// subject / …) and whose value is a string literal, returning that
// string. Used for kafkajs-style option-object call shapes.
func jsObjectTopicValue(objNode *sitter.Node, src []byte) string {
	for i := 0; i < int(objNode.NamedChildCount()); i++ {
		pair := objNode.NamedChild(i)
		if pair == nil || pair.Type() != "pair" {
			continue
		}
		keyNode := pair.ChildByFieldName("key")
		valNode := pair.ChildByFieldName("value")
		if keyNode == nil || valNode == nil || valNode.Type() != "string" {
			continue
		}
		key := strings.ToLower(strings.Trim(keyNode.Content(src), "\"'`"))
		if _, ok := jsPubsubObjectKeys[key]; !ok {
			continue
		}
		if s := jsStringLiteralContent(valNode, src); s != "" {
			return s
		}
	}
	return ""
}
