package search

import (
	"context"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/embedding"
)

// HybridBackend combines text search (BM25/Bleve) with vector search (HNSW)
// using Reciprocal Rank Fusion (RRF) for result ranking.
type HybridBackend struct {
	text     Backend
	vector   *VectorBackend
	embedder embedding.Provider
	k        int // RRF constant (default 60)
}

// NewHybrid creates a hybrid search backend.
func NewHybrid(text Backend, vector *VectorBackend, embedder embedding.Provider) *HybridBackend {
	return &HybridBackend{
		text:     text,
		vector:   vector,
		embedder: embedder,
		k:        60,
	}
}

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

// Search runs both text and vector search, fuses results with RRF.
func (h *HybridBackend) Search(query string, limit int) []SearchResult {
	// Text search (BM25/Bleve).
	textResults := h.text.Search(query, limit*2)

	// Vector search — embed the query and search HNSW.
	var vecIDs []string
	if h.vector.Count() > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		queryVec, err := h.embedder.Embed(ctx, query)
		if err == nil && queryVec != nil {
			vecIDs = h.vector.Search(queryVec, limit*2)
		}
	}

	// If no vector results, return text results only.
	if len(vecIDs) == 0 {
		if len(textResults) > limit {
			return textResults[:limit]
		}
		return textResults
	}

	// RRF fusion.
	return rrfFuse(textResults, vecIDs, h.k, limit)
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
