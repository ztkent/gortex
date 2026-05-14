package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestJSPubsub_EventEmitterOnEmit(t *testing.T) {
	src := `const EventEmitter = require('events');

function setup(emitter) {
  emitter.on('connection', handler);
}

function fire(emitter) {
  emitter.emit('connection', data);
}
`
	fix := runJSExtractFixture(t, "app.js", src)

	events := fix.nodesByKind[graph.KindEvent]
	if len(events) != 1 {
		t.Fatalf("expected 1 pubsub topic node, got %d: %+v", len(events), events)
	}
	if events[0].ID != "event::pubsub::eventemitter::connection" {
		t.Errorf("topic id = %q", events[0].ID)
	}
	if tr, _ := events[0].Meta["transport"].(string); tr != "eventemitter" {
		t.Errorf("transport = %q (want eventemitter)", tr)
	}
	if got := len(fix.edgesByKind[graph.EdgeEmits]); got != 1 {
		t.Errorf("expected 1 EdgeEmits (emit), got %d", got)
	}
	if got := len(fix.edgesByKind[graph.EdgeListensOn]); got != 1 {
		t.Errorf("expected 1 EdgeListensOn (on), got %d", got)
	}
}

func TestJSPubsub_SocketIOImportGated(t *testing.T) {
	src := `import { io } from 'socket.io-client';

function chat(socket) {
  socket.emit('message', text);
  socket.on('message', render);
}
`
	fix := runJSExtractFixture(t, "chat.js", src)
	events := fix.nodesByKind[graph.KindEvent]
	if len(events) != 1 {
		t.Fatalf("expected 1 topic, got %d", len(events))
	}
	if tr, _ := events[0].Meta["transport"].(string); tr != "socketio" {
		t.Errorf("transport = %q (want socketio)", tr)
	}
}

func TestJSPubsub_GenericMethodNoImportSkipped(t *testing.T) {
	// Without a recognised pub/sub import, generic emit/on calls are
	// just domain methods and must not enter the graph.
	src := `function wire(thing) {
  thing.on('change', cb);
  thing.emit('change', v);
}
`
	fix := runJSExtractFixture(t, "x.js", src)
	if got := len(fix.nodesByKind[graph.KindEvent]); got != 0 {
		t.Errorf("generic on/emit with no pub/sub import should produce no topic node, got %d", got)
	}
}

func TestTSPubsub_KafkaObjectArg(t *testing.T) {
	// kafkajs names the topic as a property of an options object
	// rather than a positional string.
	src := `import { Kafka } from 'kafkajs';

async function produce(producer: Producer) {
  await producer.send({ topic: 'orders', messages: [] });
}

async function consume(consumer: Consumer) {
  await consumer.subscribe({ topic: 'orders' });
}
`
	fix := runTSExtractFixture(t, "kafka.ts", src)
	events := fix.nodesByKind[graph.KindEvent]
	if len(events) != 1 {
		t.Fatalf("expected 1 topic, got %d: %+v", len(events), events)
	}
	if events[0].ID != "event::pubsub::kafka::orders" {
		t.Errorf("topic id = %q (want event::pubsub::kafka::orders)", events[0].ID)
	}
	if got := len(fix.edgesByKind[graph.EdgeEmits]); got != 1 {
		t.Errorf("expected 1 EdgeEmits (send), got %d", got)
	}
	if got := len(fix.edgesByKind[graph.EdgeListensOn]); got != 1 {
		t.Errorf("expected 1 EdgeListensOn (subscribe), got %d", got)
	}
}

func TestTSPubsub_NATSPositionalString(t *testing.T) {
	src := `import { connect } from 'nats';

async function run(nc: NatsConnection) {
  nc.publish('events.user', payload);
  nc.subscribe('events.user');
}
`
	fix := runTSExtractFixture(t, "nats.ts", src)
	events := fix.nodesByKind[graph.KindEvent]
	if len(events) != 1 {
		t.Fatalf("expected 1 topic, got %d", len(events))
	}
	if tr, _ := events[0].Meta["transport"].(string); tr != "nats" {
		t.Errorf("transport = %q (want nats)", tr)
	}
	if events[0].Name != "events.user" {
		t.Errorf("topic name = %q", events[0].Name)
	}
}
