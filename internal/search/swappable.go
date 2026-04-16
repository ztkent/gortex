package search

import "sync"

// Swappable wraps a Backend and lets a single in-place swap be performed
// concurrently with reads. Used by the indexer to upgrade from the
// in-memory BM25 backend to Bleve once the corpus crosses AutoThreshold,
// without making every call site re-thread a new Backend reference and
// without holding the indexer's lock during the (potentially seconds-long)
// re-population of Bleve.
//
// Callers see a stable *Swappable; reads delegate to whichever inner
// backend is currently active. Swap atomically replaces the inner
// backend and closes the previous one.
type Swappable struct {
	mu    sync.RWMutex
	inner Backend
}

// NewSwappable wraps b. Panics if b is nil — every Indexer must start
// with a real backend, even if it's the in-memory NewAuto() default.
func NewSwappable(b Backend) *Swappable {
	if b == nil {
		panic("search.NewSwappable: nil backend")
	}
	return &Swappable{inner: b}
}

// Swap installs the new backend and closes the old one. Safe to call
// concurrently with reads; the swap itself is brief (one pointer write
// under the write lock) so reads queued during the swap return promptly
// against the new backend.
func (s *Swappable) Swap(b Backend) {
	s.mu.Lock()
	old := s.inner
	s.inner = b
	s.mu.Unlock()
	if old != nil && old != b {
		old.Close()
	}
}

// Inner returns the currently-active backend. Used internally to test
// upgrade outcomes; production code should always go through the
// Backend interface methods on Swappable itself.
func (s *Swappable) Inner() Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner
}

// --- Backend interface ------------------------------------------------

func (s *Swappable) Add(id string, fields ...string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.inner.Add(id, fields...)
}

func (s *Swappable) Remove(id string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.inner.Remove(id)
}

func (s *Swappable) Search(query string, limit int) []SearchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner.Search(query, limit)
}

func (s *Swappable) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner.Count()
}

func (s *Swappable) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inner != nil {
		s.inner.Close()
		s.inner = nil
	}
}

// SizeBytes delegates to the inner backend's SizeBytes implementation
// if it provides one; otherwise zero.
func (s *Swappable) SizeBytes() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return BackendSize(s.inner)
}
