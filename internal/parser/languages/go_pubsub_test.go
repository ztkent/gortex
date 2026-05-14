package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestGoPubsub_NATSPublishSubscribe(t *testing.T) {
	src := `package worker

import "github.com/nats-io/nats.go"

func Send(nc *nats.Conn) {
	nc.Publish("orders.created", []byte("x"))
}

func Listen(nc *nats.Conn) {
	nc.Subscribe("orders.created", handler)
}
`
	fix := runGoExtract(t, src)

	events := fix.nodesByKind[graph.KindEvent]
	if len(events) != 1 {
		t.Fatalf("expected 1 pubsub topic node, got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.ID != "event::pubsub::nats::orders.created" {
		t.Errorf("topic id = %q (want event::pubsub::nats::orders.created)", ev.ID)
	}
	if k, _ := ev.Meta["event_kind"].(string); k != "pubsub" {
		t.Errorf("event_kind = %q (want pubsub)", k)
	}
	if tr, _ := ev.Meta["transport"].(string); tr != "nats" {
		t.Errorf("transport = %q (want nats)", tr)
	}

	emits := fix.edgesByKind[graph.EdgeEmits]
	if len(emits) != 1 {
		t.Fatalf("expected 1 EdgeEmits, got %d", len(emits))
	}
	if emits[0].From != "pkg/foo.go::Send" {
		t.Errorf("emit from = %q (want pkg/foo.go::Send)", emits[0].From)
	}
	if emits[0].Origin != graph.OriginASTInferred {
		t.Errorf("emit origin = %q (want ast_inferred)", emits[0].Origin)
	}

	listens := fix.edgesByKind[graph.EdgeListensOn]
	if len(listens) != 1 {
		t.Fatalf("expected 1 EdgeListensOn, got %d", len(listens))
	}
	if listens[0].From != "pkg/foo.go::Listen" {
		t.Errorf("listen from = %q (want pkg/foo.go::Listen)", listens[0].From)
	}
	if listens[0].To != "event::pubsub::nats::orders.created" {
		t.Errorf("listen to = %q", listens[0].To)
	}
}

func TestGoPubsub_GenericMethodNoImportSkipped(t *testing.T) {
	// A generic `Publish` method in a file with no recognised pub/sub
	// import is a domain method, not a broker call — the weak-method
	// import gate must drop it.
	src := `package app

func Run(bus *EventBus) {
	bus.Publish("thing.happened", nil)
}
`
	fix := runGoExtract(t, src)
	if got := len(fix.nodesByKind[graph.KindEvent]); got != 0 {
		t.Errorf("generic Publish with no pub/sub import should not produce a topic node, got %d", got)
	}
	if got := len(fix.edgesByKind[graph.EdgeEmits]); got != 0 {
		t.Errorf("expected 0 emit edges, got %d", got)
	}
}

func TestGoPubsub_RedisChannelArgAfterContext(t *testing.T) {
	// go-redis passes the channel as the first string after the
	// context argument — the scanner takes the first string literal
	// anywhere in the argument list.
	src := `package cache

import "github.com/redis/go-redis/v9"

func Notify(rdb *redis.Client) {
	rdb.Publish(ctx, "cache:invalidate", "key1")
}
`
	fix := runGoExtract(t, src)
	events := fix.nodesByKind[graph.KindEvent]
	if len(events) != 1 {
		t.Fatalf("expected 1 topic, got %d", len(events))
	}
	if events[0].Name != "cache:invalidate" {
		t.Errorf("topic name = %q (want cache:invalidate)", events[0].Name)
	}
	if tr, _ := events[0].Meta["transport"].(string); tr != "redis" {
		t.Errorf("transport = %q (want redis)", tr)
	}
}

func TestGoPubsub_DistinctiveMethodNoImport(t *testing.T) {
	// QueueSubscribe is distinctive enough to classify without a
	// recognised import — the method's own transport hint is used.
	src := `package q

func Listen(nc *Conn) {
	nc.QueueSubscribe("jobs", "workers", handler)
}
`
	fix := runGoExtract(t, src)
	events := fix.nodesByKind[graph.KindEvent]
	if len(events) != 1 {
		t.Fatalf("expected 1 topic, got %d", len(events))
	}
	if events[0].ID != "event::pubsub::nats::jobs" {
		t.Errorf("id = %q (want event::pubsub::nats::jobs)", events[0].ID)
	}
	if got := len(fix.edgesByKind[graph.EdgeListensOn]); got != 1 {
		t.Errorf("expected 1 listens_on edge, got %d", got)
	}
}

func TestGoPubsub_DuplicateTopicDeduplicates(t *testing.T) {
	src := `package q

import "github.com/nats-io/nats.go"

func A(nc *nats.Conn) { nc.Publish("evt", nil) }
func B(nc *nats.Conn) { nc.Publish("evt", nil) }
`
	fix := runGoExtract(t, src)
	if got := len(fix.nodesByKind[graph.KindEvent]); got != 1 {
		t.Errorf("expected 1 deduped topic node, got %d", got)
	}
	if got := len(fix.edgesByKind[graph.EdgeEmits]); got != 2 {
		t.Errorf("expected 2 emit edges (one per call site), got %d", got)
	}
}

func TestGoPubsub_NonLiteralTopicSkipped(t *testing.T) {
	src := `package q

import "github.com/nats-io/nats.go"

func Run(nc *nats.Conn, subject string) {
	nc.Publish(subject, nil)
}
`
	fix := runGoExtract(t, src)
	if got := len(fix.nodesByKind[graph.KindEvent]); got != 0 {
		t.Errorf("dynamic subject should not produce a topic node, got %d", got)
	}
}
