package search

import (
	"context"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/search/rerank"
)

// HybridBackend combines text search (BM25/Bleve) with vector search (HNSW)
// using Reciprocal Rank Fusion (RRF) for result ranking.
//
// When autoAlpha is true (the default), Search() classifies the query as
// identifier-shaped or natural-language and applies an α-weighted fusion
// instead of even-weight RRF: identifier queries lean toward BM25 (small
// α) where exact-token matches are most reliable, NL queries balance both
// channels (larger α) so semantic similarity catches synonymous wording.
// Set autoAlpha=false via SetAutoAlpha to fall back to the original
// equal-weight RRF — useful for tests pinning the legacy ranking.
type HybridBackend struct {
	text      Backend
	vector    *VectorBackend
	embedder  embedding.Provider
	k         int // RRF constant (default 60)
	autoAlpha bool
}

// NewHybrid creates a hybrid search backend with auto-α enabled.
func NewHybrid(text Backend, vector *VectorBackend, embedder embedding.Provider) *HybridBackend {
	return &HybridBackend{
		text:      text,
		vector:    vector,
		embedder:  embedder,
		k:         60,
		autoAlpha: true,
	}
}

// SetAutoAlpha toggles auto-α fusion. When false, Search() reverts to
// the original equal-weight RRF.
func (h *HybridBackend) SetAutoAlpha(on bool) { h.autoAlpha = on }

// AutoAlpha reports whether auto-α fusion is active.
func (h *HybridBackend) AutoAlpha() bool { return h.autoAlpha }

// Add indexes a symbol in both text and vector backends.
func (h *HybridBackend) Add(id string, fields ...string) {
	h.text.Add(id, fields...)
}

// AddVector adds a vector for a symbol to the vector backend.
func (h *HybridBackend) AddVector(id string, vector []float32) {
	h.vector.Add(id, vector)
}

// Remove removes a symbol from the text backend.
func (h *HybridBackend) Remove(id string) {
	h.text.Remove(id)
	// Note: coder/hnsw doesn't support removal. The vector index
	// is rebuilt on full re-index. Stale vectors are harmless —
	// they won't match graph nodes and will be filtered out.
}

// Search runs both text and vector search, fuses results with RRF
// (equal weight) when autoAlpha is off, or α-weighted RRF when on.
// Auto-α leans toward BM25 for identifier queries (where exact-token
// matches are the most reliable signal) and balances both channels
// for natural-language queries (where semantic similarity catches
// synonymous wording).
func (h *HybridBackend) Search(query string, limit int) []SearchResult {
	textResults, vecIDs := h.searchChannels(query, limit)
	if len(vecIDs) == 0 {
		if len(textResults) > limit {
			return textResults[:limit]
		}
		return textResults
	}
	if h.autoAlpha {
		return alphaFuse(textResults, vecIDs, rerank.AlphaFor(query), h.k, limit)
	}
	return rrfFuse(textResults, vecIDs, h.k, limit)
}

// SearchChannels returns the raw per-channel results — BM25 ranks
// (with scores) and the parallel vector-search ID list — without
// RRF fusion. The rerank pipeline calls this so each channel can
// contribute as a separate Signal instead of being collapsed into a
// single RRF score upstream of the rerank.
func (h *HybridBackend) SearchChannels(query string, limit int) (textResults []SearchResult, vectorIDs []string) {
	return h.searchChannels(query, limit)
}

func (h *HybridBackend) searchChannels(query string, limit int) ([]SearchResult, []string) {
	textResults := h.text.Search(query, limit*2)

	var vecIDs []string
	if h.vector.Count() > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		queryVec, err := h.embedder.Embed(ctx, query)
		if err == nil && queryVec != nil {
			vecIDs = h.vector.Search(queryVec, limit*2)
		}
	}
	return textResults, vecIDs
}

// Count returns the text backend document count.
func (h *HybridBackend) Count() int { return h.text.Count() }

// Close releases resources.
func (h *HybridBackend) Close() {
	h.text.Close()
}

// TextBackend returns the underlying text search backend.
func (h *HybridBackend) TextBackend() Backend { return h.text }

// VectorBackend returns the underlying vector search backend.
func (h *HybridBackend) VectorIndex() *VectorBackend { return h.vector }

// Embedder returns the embedding provider.
func (h *HybridBackend) Embedder() embedding.Provider { return h.embedder }

// SizeBytes returns the sum of text and vector backend sizes.
func (h *HybridBackend) SizeBytes() uint64 {
	return BackendSize(h.text) + h.vector.SizeBytes()
}

// TextSizeBytes returns just the text backend's size — used by the
// daemon status report to split "search" from "vectors" visually.
func (h *HybridBackend) TextSizeBytes() uint64 { return BackendSize(h.text) }

// VectorSizeBytes returns just the vector backend's size.
func (h *HybridBackend) VectorSizeBytes() uint64 { return h.vector.SizeBytes() }

// alphaFuse combines text and vector results with an α-weighted blend
// of their reciprocal-rank contributions. Higher α gives the vector
// channel more weight (good for natural-language queries where
// semantic similarity catches synonyms); lower α gives BM25 more
// weight (good for identifier queries where exact-token matches are
// the most reliable signal).
//
// Formula:
//
//	score(doc) = (1-α) × 1/(k+rank_text+1) + α × 1/(k+rank_vector+1)
//
// α=0 reduces to text-only; α=1 reduces to vector-only; α=0.5 is
// equivalent to rrfFuse with each channel halved (so absolute scores
// differ from rrfFuse but the relative ordering is the same).
func alphaFuse(textResults []SearchResult, vecIDs []string, alpha float64, k, limit int) []SearchResult {
	if alpha < 0 {
		alpha = 0
	}
	if alpha > 1 {
		alpha = 1
	}
	textWeight := 1.0 - alpha
	vecWeight := alpha
	scores := make(map[string]float64)

	for rank, r := range textResults {
		scores[r.ID] += textWeight / float64(k+rank+1)
	}
	for rank, id := range vecIDs {
		scores[id] += vecWeight / float64(k+rank+1)
	}

	type scored struct {
		id    string
		score float64
	}
	results := make([]scored, 0, len(scores))
	for id, score := range scores {
		results = append(results, scored{id: id, score: score})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		// Stable secondary key: id ascending so identical-score
		// runs ship in a deterministic order across calls.
		return results[i].id < results[j].id
	})

	if len(results) > limit {
		results = results[:limit]
	}
	out := make([]SearchResult, len(results))
	for i, r := range results {
		out[i] = SearchResult{ID: r.id, Score: r.score}
	}
	return out
}

// rrfFuse combines text and vector results using Reciprocal Rank Fusion.
// score(doc) = 1/(k+rank_text) + 1/(k+rank_vector)
func rrfFuse(textResults []SearchResult, vecIDs []string, k, limit int) []SearchResult {
	scores := make(map[string]float64)

	// Text ranks.
	for rank, r := range textResults {
		scores[r.ID] += 1.0 / float64(k+rank+1)
	}

	// Vector ranks.
	for rank, id := range vecIDs {
		scores[id] += 1.0 / float64(k+rank+1)
	}

	// Sort by combined RRF score.
	type scored struct {
		id    string
		score float64
	}
	var results []scored
	for id, score := range scores {
		results = append(results, scored{id: id, score: score})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if len(results) > limit {
		results = results[:limit]
	}

	// Convert back to SearchResult (use RRF score).
	out := make([]SearchResult, len(results))
	for i, r := range results {
		out[i] = SearchResult{
			ID:    r.id,
			Score: r.score,
		}
	}
	return out
}
