// Package search provides full-text search over code symbols with
// camelCase/snake_case-aware tokenization and BM25 ranking.
//
// Two backends are available:
//   - BM25Backend: custom in-memory inverted index (fast, zero deps)
//   - BleveBackend: bleve-based index (better for large repos, multi-repo)
//
// Use AutoBackend to pick the right one based on symbol count.
package search

// SearchResult is a single search hit.
type SearchResult struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// Backend is the interface for search backends.
type Backend interface {
	// Add indexes a symbol with the given text fields.
	Add(id string, fields ...string)

	// Remove deletes a symbol from the index.
	Remove(id string)

	// Search queries the index and returns ranked results.
	Search(query string, limit int) []SearchResult

	// Count returns the number of indexed documents.
	Count() int

	// Close releases resources.
	Close()
}

// Sizer is an optional interface a Backend can implement to report its
// approximate in-memory footprint. Used by `gortex daemon status` to
// break down per-repo memory; callers should type-assert and treat a
// missing implementation as zero.
type Sizer interface {
	SizeBytes() uint64
}

// BackendSize returns the estimated byte size of b if it implements
// Sizer, or zero otherwise. Safe to call on a nil Backend.
func BackendSize(b Backend) uint64 {
	if b == nil {
		return 0
	}
	if s, ok := b.(Sizer); ok {
		return s.SizeBytes()
	}
	return 0
}

// AutoThreshold is the symbol count above which BleveBackend is used.
const AutoThreshold = 50000

// NewAuto creates a BM25Backend initially. Call Upgrade() after indexing
// if the count exceeds AutoThreshold and multi-repo mode is desired.
func NewAuto() Backend {
	return NewBM25()
}
