package languages

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Event pub/sub detection (F15). Mirrors the observability extractor's
// shape — a method-name + first-string-literal-argument heuristic — but
// classifies the call as a *publish* or *subscribe* operation against a
// message broker (NATS / Kafka / RabbitMQ / Redis pub-sub) or an
// in-process event channel (Node EventEmitter / Socket.IO). Publishers
// emit EdgeEmits and subscribers EdgeListensOn, both targeting a shared
// KindEvent topic node so "who publishes / who listens on topic X" is a
// single node's in-edge walk split by edge kind.
//
// The language extractors do the AST-shaped work (find the call, pull
// the receiver method name, extract the first string-literal argument);
// this file owns the cross-language classification, transport
// inference, node-ID convention, and graph-artifact emission so every
// language stays consistent.

// pubsubRolePublish / pubsubRoleSubscribe are the two operation classes
// a recognised pub/sub call falls into.
const (
	pubsubRolePublish   = "publish"
	pubsubRoleSubscribe = "subscribe"
)

// pubsubClassification describes how a method name maps onto the pub/sub
// layer.
//
//	role — pubsubRolePublish | pubsubRoleSubscribe.
//	weak — the name is generic enough (emit / on / send / publish / …)
//	       that it should only be treated as pub/sub when the file also
//	       imports a recognised pub/sub library. Distinctive names
//	       (WriteMessages / basic_publish / QueueSubscribe / …) are not
//	       weak — the name alone is strong evidence.
//	hint — a transport label when the method name alone identifies the
//	       broker; "" when the transport must be inferred from imports.
type pubsubClassification struct {
	role string
	weak bool
	hint string
}

// pubsubMethods maps a call's method (or function) name to its pub/sub
// classification. Case-sensitive on purpose: Go brokers use PascalCase
// (Publish / Subscribe / WriteMessages), JS/Python use camelCase /
// snake_case (publish / basic_publish), and matching exactly keeps the
// false-positive surface small.
var pubsubMethods = map[string]pubsubClassification{
	// EventEmitter / Socket.IO — generic names, import-gated.
	"emit":                {pubsubRolePublish, true, ""},
	"on":                  {pubsubRoleSubscribe, true, ""},
	"once":                {pubsubRoleSubscribe, true, ""},
	"addListener":         {pubsubRoleSubscribe, true, "eventemitter"},
	"prependListener":     {pubsubRoleSubscribe, true, "eventemitter"},
	"prependOnceListener": {pubsubRoleSubscribe, true, "eventemitter"},

	// NATS (nats.go / nats.js / nats-py).
	"Publish":        {pubsubRolePublish, true, ""},
	"publish":        {pubsubRolePublish, true, ""},
	"PublishMsg":     {pubsubRolePublish, false, "nats"},
	"PublishRequest": {pubsubRolePublish, false, "nats"},
	"Subscribe":      {pubsubRoleSubscribe, true, ""},
	"subscribe":      {pubsubRoleSubscribe, true, ""},
	"QueueSubscribe": {pubsubRoleSubscribe, false, "nats"},
	"SubscribeSync":  {pubsubRoleSubscribe, false, "nats"},
	"ChanSubscribe":  {pubsubRoleSubscribe, false, "nats"},

	// Kafka (segmentio/kafka-go, sarama, confluent-kafka-go, kafkajs,
	// kafka-python).
	"Produce":         {pubsubRolePublish, false, "kafka"},
	"WriteMessages":   {pubsubRolePublish, false, "kafka"},
	"SendMessage":     {pubsubRolePublish, false, "kafka"},
	"SendMessages":    {pubsubRolePublish, false, "kafka"},
	"SubscribeTopics": {pubsubRoleSubscribe, false, "kafka"},
	"send":            {pubsubRolePublish, true, ""},

	// RabbitMQ (amqp091-go / streadway-amqp, amqplib, pika).
	"PublishWithContext": {pubsubRolePublish, false, "rabbitmq"},
	"sendToQueue":        {pubsubRolePublish, false, "rabbitmq"},
	"basic_publish":      {pubsubRolePublish, false, "rabbitmq"},
	"Consume":            {pubsubRoleSubscribe, false, "rabbitmq"},
	"consume":            {pubsubRoleSubscribe, true, "rabbitmq"},
	"basic_consume":      {pubsubRoleSubscribe, false, "rabbitmq"},

	// Redis pub/sub (go-redis, ioredis / node-redis, redis-py).
	"PSubscribe": {pubsubRoleSubscribe, false, "redis"},
	"psubscribe": {pubsubRoleSubscribe, false, "redis"},
}

// pubsubLibrary maps an import-path substring to a transport label.
// Entries are checked in order against every import path in a file, so
// the more specific tokens (kafkajs, amqplib) come before their broader
// roots (kafka, amqp).
type pubsubLibrary struct {
	substr    string
	transport string
}

var pubsubLibraries = []pubsubLibrary{
	{"kafkajs", "kafka"},
	{"segmentio/kafka-go", "kafka"},
	{"confluent-kafka", "kafka"},
	{"confluentinc", "kafka"},
	{"sarama", "kafka"},
	{"kafka", "kafka"},
	{"amqplib", "rabbitmq"},
	{"amqp091", "rabbitmq"},
	{"streadway/amqp", "rabbitmq"},
	{"rabbitmq", "rabbitmq"},
	{"pika", "rabbitmq"},
	{"amqp", "rabbitmq"},
	{"ioredis", "redis"},
	{"go-redis", "redis"},
	{"redis", "redis"},
	{"socket.io", "socketio"},
	{"socketio", "socketio"},
	{"eventemitter3", "eventemitter"},
	{"eventemitter", "eventemitter"},
	{"nats", "nats"},
}

// inferPubsubTransport scans a file's import paths for a recognised
// pub/sub library and returns its transport label. The Node builtin
// EventEmitter module (`events` / `node:events`) is matched exactly
// rather than as a substring — "events" as a substring would tag far
// too many local module paths.
func inferPubsubTransport(importPaths []string) (string, bool) {
	for _, p := range importPaths {
		lp := strings.ToLower(strings.TrimSpace(p))
		if lp == "events" || lp == "node:events" {
			return "eventemitter", true
		}
		for _, lib := range pubsubLibraries {
			if strings.Contains(lp, lib.substr) {
				return lib.transport, true
			}
		}
	}
	return "", false
}

// resolvePubsubTransport decides the transport label for a recognised
// pub/sub call. Weak (generic-name) calls are dropped entirely unless
// the file imports a pub/sub library — the import is the disambiguator
// that keeps a plain `emitter.on("click", …)` or domain `bus.publish()`
// out of the graph. A method's own transport hint always wins; failing
// that the inferred library transport is used; "unknown" is the last
// resort for a distinctive method name in a file with no recognised
// import.
func resolvePubsubTransport(c pubsubClassification, importPaths []string) (string, bool) {
	transport, imported := inferPubsubTransport(importPaths)
	if c.weak && !imported {
		return "", false
	}
	if c.hint != "" {
		return c.hint, true
	}
	if imported {
		return transport, true
	}
	return "unknown", true
}

// pubsubEvent is a deferred record of one resolved pub/sub publish or
// subscribe call site. The language extractor fills these in after its
// per-file imports and function ranges are known.
type pubsubEvent struct {
	role      string // pubsubRolePublish | pubsubRoleSubscribe
	transport string // nats|kafka|rabbitmq|redis|socketio|eventemitter|unknown
	topic     string // first string-literal argument — the channel/subject/topic
	method    string // the matched call method name
	line      int    // 1-based line of the call expression
}

// classifyPubsubCall is the single decision point every language
// extractor funnels a member call through. Given the call's method
// name, the topic string the extractor pulled from the first
// string-literal argument, and the file's import paths, it returns a
// fully-resolved pubsubEvent. ok is false when the method is not a
// pub/sub operation, the topic is empty, or a weak method fired in a
// file with no pub/sub import.
func classifyPubsubCall(method, topic string, importPaths []string, line int) (pubsubEvent, bool) {
	c, known := pubsubMethods[method]
	if !known || topic == "" {
		return pubsubEvent{}, false
	}
	transport, ok := resolvePubsubTransport(c, importPaths)
	if !ok {
		return pubsubEvent{}, false
	}
	return pubsubEvent{
		role:      c.role,
		transport: transport,
		topic:     topic,
		method:    method,
		line:      line,
	}, true
}

// pubsubEventNodeID is the canonical ID for a pub/sub topic node. The
// transport is part of the ID so a Kafka topic and a Redis channel that
// happen to share a name stay distinct nodes — they are different
// systems. The `event::` prefix matches the synthetic-ID convention the
// exporter and applyRepoPrefix already recognise (alongside
// `event::log::`), so topic nodes de-duplicate across files in a repo
// without a real source-file backing.
func pubsubEventNodeID(transport, topic string) string {
	return "event::pubsub::" + transport + "::" + topic
}

// emitPubsubEvents materialises one KindEvent topic node per distinct
// (transport, topic) pair plus the EdgeEmits (publish) / EdgeListensOn
// (subscribe) edge from each call site's enclosing function to that
// node. callerLookup maps a 1-based line to the enclosing function ID;
// call sites at file scope (callerLookup returns "") are skipped — a
// pub/sub call needs a function to attribute the edge to.
func emitPubsubEvents(events []pubsubEvent, callerLookup func(line int) string, filePath, language string, result *parser.ExtractionResult) {
	if len(events) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(events))
	for _, e := range events {
		callerID := callerLookup(e.line)
		if callerID == "" {
			continue
		}
		nodeID := pubsubEventNodeID(e.transport, e.topic)
		if _, ok := seen[nodeID]; !ok {
			seen[nodeID] = struct{}{}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       nodeID,
				Kind:     graph.KindEvent,
				Name:     e.topic,
				FilePath: filePath, // first sighting; not authoritative
				Language: language,
				Meta: map[string]any{
					"event_kind": "pubsub",
					"transport":  e.transport,
					"name":       e.topic,
				},
			})
		}
		edgeKind := graph.EdgeEmits
		if e.role == pubsubRoleSubscribe {
			edgeKind = graph.EdgeListensOn
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     callerID,
			To:       nodeID,
			Kind:     edgeKind,
			FilePath: filePath,
			Line:     e.line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"method":    e.method,
				"transport": e.transport,
			},
		})
	}
}

// importPathValues flattens an alias→path import map to the sorted set
// of distinct paths, the form inferPubsubTransport consumes. Sorting
// keeps transport inference deterministic when a file imports two
// recognised libraries.
func importPathValues(imports map[string]string) []string {
	if len(imports) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(imports))
	out := make([]string, 0, len(imports))
	for _, p := range imports {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
