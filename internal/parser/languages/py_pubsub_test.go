package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestPyPubsub_NATSPublishSubscribe(t *testing.T) {
	src := `import nats

async def publish(nc):
    await nc.publish("orders.new", b"data")

async def listen(nc):
    await nc.subscribe("orders.new")
`
	nodes, edges := runPyExtract(t, "worker.py", src)

	events := nodesOfKind(nodes, graph.KindEvent)
	if len(events) != 1 {
		t.Fatalf("expected 1 pubsub topic node, got %d: %v", len(events), nodeNames(events))
	}
	if events[0].ID != "event::pubsub::nats::orders.new" {
		t.Errorf("topic id = %q", events[0].ID)
	}
	if tr, _ := events[0].Meta["transport"].(string); tr != "nats" {
		t.Errorf("transport = %q (want nats)", tr)
	}

	if got := len(edgesByKind(edges, graph.EdgeEmits)); got != 1 {
		t.Errorf("expected 1 EdgeEmits, got %d", got)
	}
	listens := edgesByKind(edges, graph.EdgeListensOn)
	if len(listens) != 1 {
		t.Fatalf("expected 1 EdgeListensOn, got %d", len(listens))
	}
	if listens[0].From != "worker.py::listen" {
		t.Errorf("listen from = %q (want worker.py::listen)", listens[0].From)
	}
}

func TestPyPubsub_PikaKeywordArgs(t *testing.T) {
	// pika names the topic via keyword arguments (routing_key / queue),
	// not a positional string.
	src := `import pika

def send(channel):
    channel.basic_publish(exchange='', routing_key='task_queue', body=b'x')

def recv(channel):
    channel.basic_consume(queue='task_queue', on_message_callback=cb)
`
	nodes, edges := runPyExtract(t, "mq.py", src)
	events := nodesOfKind(nodes, graph.KindEvent)
	if len(events) != 1 {
		t.Fatalf("expected 1 topic, got %d: %v", len(events), nodeNames(events))
	}
	if events[0].ID != "event::pubsub::rabbitmq::task_queue" {
		t.Errorf("topic id = %q (want event::pubsub::rabbitmq::task_queue)", events[0].ID)
	}
	if got := len(edgesByKind(edges, graph.EdgeEmits)); got != 1 {
		t.Errorf("expected 1 EdgeEmits (basic_publish), got %d", got)
	}
	if got := len(edgesByKind(edges, graph.EdgeListensOn)); got != 1 {
		t.Errorf("expected 1 EdgeListensOn (basic_consume), got %d", got)
	}
}

func TestPyPubsub_RedisChannel(t *testing.T) {
	src := `import redis

def notify(r):
    r.publish("channel:1", "msg")
`
	nodes, _ := runPyExtract(t, "cache.py", src)
	events := nodesOfKind(nodes, graph.KindEvent)
	if len(events) != 1 {
		t.Fatalf("expected 1 topic, got %d", len(events))
	}
	if tr, _ := events[0].Meta["transport"].(string); tr != "redis" {
		t.Errorf("transport = %q (want redis)", tr)
	}
}

func TestPyPubsub_GenericMethodNoImportSkipped(t *testing.T) {
	src := `def run(bus):
    bus.publish("x")
`
	nodes, _ := runPyExtract(t, "x.py", src)
	if got := len(nodesOfKind(nodes, graph.KindEvent)); got != 0 {
		t.Errorf("generic publish with no pub/sub import should produce no topic node, got %d", got)
	}
}
