package progress

import (
	"context"
	"sync"
	"testing"
)

func TestNop_NoPanic(t *testing.T) {
	Nop{}.Report("anything", 0, 0)
	Nop{}.Report("stage", 1, 100)
}

func TestFromContext_NilCtx_ReturnsNop(t *testing.T) {
	r := FromContext(context.TODO())
	if r == nil {
		t.Fatal("FromContext must never return nil")
	}
	r.Report("x", 0, 0) // must not panic
}

func TestFromContext_NoReporter_ReturnsNop(t *testing.T) {
	r := FromContext(context.Background())
	if _, ok := r.(Nop); !ok {
		t.Errorf("expected Nop when no reporter attached, got %T", r)
	}
}

func TestWithReporter_Roundtrip(t *testing.T) {
	rec := &recorder{}
	ctx := WithReporter(context.Background(), rec)

	FromContext(ctx).Report("parse", 3, 10)

	if len(rec.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.stage != "parse" || ev.current != 3 || ev.total != 10 {
		t.Errorf("unexpected event: %+v", ev)
	}
}

func TestWithReporter_NilReporter_NoOp(t *testing.T) {
	ctx := WithReporter(context.Background(), nil)
	r := FromContext(ctx)
	if _, ok := r.(Nop); !ok {
		t.Errorf("nil reporter must not leak through context, got %T", r)
	}
}

type recEvent struct {
	stage          string
	current, total int
}

type recorder struct {
	mu     sync.Mutex
	events []recEvent
}

func (r *recorder) Report(stage string, current, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recEvent{stage, current, total})
}
