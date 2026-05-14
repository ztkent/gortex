package svc

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/llm"
)

// assistCache is a tiny FIFO-evicting LRU keyed on a string. Values
// are []string lists of either expanded query terms or reranked node
// IDs. Hand-rolled to avoid a dependency for what's a few-hundred-entry
// map.
//
// Concurrent-safe: every public method takes the lock. Sub-microsecond
// per op, fine for the inline call sites in ExpandQuery / RerankSymbols.
type assistCache struct {
	mu   sync.Mutex
	max  int
	data map[string][]string
	keys []string // insertion order; oldest first
}

func newAssistCache(max int) *assistCache {
	if max <= 0 {
		max = 256
	}
	return &assistCache{
		max:  max,
		data: make(map[string][]string, max),
		keys: make([]string, 0, max),
	}
}

func (c *assistCache) Get(key string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	if !ok {
		return nil, false
	}
	// Copy so callers can't mutate the cached slice.
	out := make([]string, len(v))
	copy(out, v)
	return out, true
}

func (c *assistCache) Set(key string, val []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.data[key]; ok {
		// Update in place; keep position in keys.
		stored := make([]string, len(val))
		copy(stored, val)
		c.data[key] = stored
		return
	}
	if len(c.keys) >= c.max {
		// Evict oldest.
		oldest := c.keys[0]
		c.keys = c.keys[1:]
		delete(c.data, oldest)
	}
	stored := make([]string, len(val))
	copy(stored, val)
	c.data[key] = stored
	c.keys = append(c.keys, key)
}

// Len reports current cache size. Test-only convenience.
func (c *assistCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.data)
}

// rerankCacheKey hashes the candidate ID set together with the query
// so cache lookups are stable across input orderings (two callers that
// pass the same candidates in a different order still hit the cache).
func rerankCacheKey(query string, cands []llm.RerankCandidate) string {
	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.ID
	}
	sort.Strings(ids)
	h := sha256.New()
	h.Write([]byte(query))
	h.Write([]byte{0x1f}) // unit separator
	h.Write([]byte(strings.Join(ids, "\x1e")))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// verifyCacheKey hashes (query, sorted-ids, body-fingerprint) so a
// re-indexed codebase doesn't serve stale verifications. The body
// fingerprint is intentionally NOT just the ID set: an agent could
// re-issue the same query after editing one of the candidates' source,
// and we want to re-verify in that case.
func verifyCacheKey(query string, cands []llm.VerifyCandidate) string {
	type idBody struct{ id, body string }
	entries := make([]idBody, len(cands))
	for i, c := range cands {
		entries[i] = idBody{id: c.ID, body: c.Body}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })

	h := sha256.New()
	h.Write([]byte(query))
	h.Write([]byte{0x1f})
	for _, e := range entries {
		h.Write([]byte(e.id))
		h.Write([]byte{0x1e})
		h.Write([]byte(e.body))
		h.Write([]byte{0x1d}) // record separator
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
