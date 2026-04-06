package hub

import (
	"fmt"
	"os"
	"sync"

	"github.com/zzet/gortex/internal/indexer"
)

// Hub fans out watcher events to multiple subscribers.
type Hub struct {
	subscribers map[string]chan indexer.GraphChangeEvent
	mu          sync.RWMutex
	nextID      int
	done        chan struct{}
}

// New creates a new Hub.
func New() *Hub {
	return &Hub{
		subscribers: make(map[string]chan indexer.GraphChangeEvent),
		done:        make(chan struct{}),
	}
}

// Run reads from the watcher events channel and broadcasts to all subscribers.
// It also logs each event to stderr. Blocks until the events channel is closed
// or Stop is called.
func (h *Hub) Run(events <-chan indexer.GraphChangeEvent) {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			// Log to stderr (replaces the inline goroutine in serve.go)
			fmt.Fprintf(os.Stderr, "[gortex watch] %-10s %s  +%d nodes  +%d edges  -%d nodes  -%d edges  (%dms)\n",
				ev.Kind, ev.FilePath, ev.NodesAdded, ev.EdgesAdded, ev.NodesRemoved, ev.EdgesRemoved, ev.DurationMs)

			h.broadcast(ev)
		case <-h.done:
			return
		}
	}
}

func (h *Hub) broadcast(ev indexer.GraphChangeEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subscribers {
		select {
		case ch <- ev:
		default:
			// Slow subscriber — drop event to avoid blocking
		}
	}
}

// Subscribe creates a new subscriber channel. Returns an ID for unsubscribing
// and a receive-only channel that will receive graph change events.
func (h *Hub) Subscribe() (string, <-chan indexer.GraphChangeEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := fmt.Sprintf("sub-%d", h.nextID)
	ch := make(chan indexer.GraphChangeEvent, 16)
	h.subscribers[id] = ch
	return id, ch
}

// Unsubscribe removes a subscriber and closes its channel.
func (h *Hub) Unsubscribe(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subscribers[id]; ok {
		close(ch)
		delete(h.subscribers, id)
	}
}

// Stop signals the hub to stop processing events.
func (h *Hub) Stop() {
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}
