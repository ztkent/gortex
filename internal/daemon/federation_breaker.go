package daemon

import (
	"context"
	"sync"
	"time"
)

// circuitBreaker skips a remote that has failed K times in a row until a
// cooldown elapses, so a chronically-dead remote stops costing a per-call
// timeout. Half-opens after the cooldown (one trial call) by resetting.
type circuitBreaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	state     map[string]*breakerState
}

type breakerState struct {
	failures  int
	openUntil time.Time
}

func newCircuitBreaker(threshold int, cooldown time.Duration) *circuitBreaker {
	return &circuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
		state:     map[string]*breakerState{},
	}
}

func (b *circuitBreaker) isOpen(slug string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[slug]
	if st == nil || st.openUntil.IsZero() {
		return false
	}
	if time.Now().After(st.openUntil) {
		// Cooldown elapsed — half-open: let the next call through.
		st.openUntil = time.Time{}
		st.failures = 0
		return false
	}
	return true
}

func (b *circuitBreaker) fail(slug string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[slug]
	if st == nil {
		st = &breakerState{}
		b.state[slug] = st
	}
	st.failures++
	if st.failures >= b.threshold {
		st.openUntil = time.Now().Add(b.cooldown)
	}
}

func (b *circuitBreaker) success(slug string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if st := b.state[slug]; st != nil {
		st.failures = 0
		st.openUntil = time.Time{}
	}
}

// healthCache memoises each remote's /v1/health advertisement for a short
// TTL so the per-call capability + readiness negotiation costs at most one
// extra round-trip per remote per window.
type healthCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]healthEntry
}

type healthEntry struct {
	h   RemoteHealth
	at  time.Time
	err error
}

func newHealthCache(ttl time.Duration) *healthCache {
	return &healthCache{ttl: ttl, entries: map[string]healthEntry{}}
}

func (c *healthCache) get(ctx context.Context, cli *ServerClient, timeout time.Duration) (RemoteHealth, error) {
	slug := cli.Entry.Slug
	c.mu.Lock()
	if e, ok := c.entries[slug]; ok && time.Since(e.at) < c.ttl {
		c.mu.Unlock()
		return e.h, e.err
	}
	c.mu.Unlock()

	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	h, err := cli.FetchHealth(hctx)

	c.mu.Lock()
	c.entries[slug] = healthEntry{h: h, at: time.Now(), err: err}
	c.mu.Unlock()
	return h, err
}
