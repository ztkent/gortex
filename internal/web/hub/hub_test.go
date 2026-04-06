package hub

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func makeEvent(path string) indexer.GraphChangeEvent {
	return indexer.GraphChangeEvent{
		FilePath:   path,
		Kind:       indexer.ChangeModified,
		NodesAdded: 1,
		Timestamp:  time.Now(),
	}
}

func TestHub_MultipleSubscribersReceiveEvent(t *testing.T) {
	h := New()
	events := make(chan indexer.GraphChangeEvent, 8)
	go h.Run(events)
	defer h.Stop()

	_, ch1 := h.Subscribe()
	_, ch2 := h.Subscribe()

	ev := makeEvent("a.go")
	events <- ev

	select {
	case got := <-ch1:
		assert.Equal(t, "a.go", got.FilePath)
	case <-time.After(time.Second):
		t.Fatal("subscriber 1 did not receive event")
	}

	select {
	case got := <-ch2:
		assert.Equal(t, "a.go", got.FilePath)
	case <-time.After(time.Second):
		t.Fatal("subscriber 2 did not receive event")
	}
}

func TestHub_SlowSubscriberDoesNotBlock(t *testing.T) {
	h := New()
	events := make(chan indexer.GraphChangeEvent, 64)
	go h.Run(events)
	defer h.Stop()

	// slow subscriber: never reads
	h.Subscribe()

	// fast subscriber
	_, fastCh := h.Subscribe()

	// Send more events than the slow subscriber's buffer
	for i := 0; i < 32; i++ {
		events <- makeEvent("file.go")
	}

	// Fast subscriber should still get events
	received := 0
	timeout := time.After(time.Second)
	for {
		select {
		case <-fastCh:
			received++
			if received >= 16 {
				return // success
			}
		case <-timeout:
			require.GreaterOrEqual(t, received, 1, "fast subscriber should receive at least some events")
			return
		}
	}
}

func TestHub_Unsubscribe(t *testing.T) {
	h := New()
	events := make(chan indexer.GraphChangeEvent, 8)
	go h.Run(events)
	defer h.Stop()

	id, ch := h.Subscribe()
	h.Unsubscribe(id)

	events <- makeEvent("a.go")
	time.Sleep(50 * time.Millisecond)

	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel should be closed after unsubscribe")
	default:
		// Channel closed and empty — expected
	}
}

func TestHub_StopEndsRun(t *testing.T) {
	h := New()
	events := make(chan indexer.GraphChangeEvent, 8)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.Run(events)
	}()

	h.Stop()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Run exited — success
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after Stop")
	}
}
