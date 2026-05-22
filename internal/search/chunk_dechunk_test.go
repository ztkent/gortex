package search

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedEmbedder is a deterministic test embedding provider: every query
// embeds to the same constant vector, so HybridBackend.Search exercises
// the vector channel without any model. The vector backend is what
// decides ranking via the indexed vectors.
type fixedEmbedder struct{ dims int }

func (f fixedEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	v := make([]float32, f.dims)
	if f.dims > 0 {
		v[0] = 1
	}
	return v, nil
}

func (f fixedEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i], _ = f.Embed(ctx, texts[i])
	}
	return out, nil
}

func (f fixedEmbedder) Dimensions() int { return f.dims }
func (f fixedEmbedder) Close() error    { return nil }

// TestHybridSearch_DeChunksToParent proves the query-time de-chunk
// contract: when the vector index holds chunk vectors, a chunk hit is
// mapped back to its parent symbol, the symbol appears at most once,
// and no synthetic "#chunk" ID ever leaks into the results.
func TestHybridSearch_DeChunksToParent(t *testing.T) {
	const dims = 3
	vec := NewVector(dims)
	// Two symbols. "big.go::Big" is sub-chunked into three chunk
	// vectors; "small.go::Small" has a single plain vector. All chunk
	// vectors of Big point the same direction so a query hits several
	// of them at once — the case de-chunking must collapse.
	vec.Add("big.go::Big#chunk0", []float32{1, 0, 0})
	vec.Add("big.go::Big#chunk1", []float32{0.99, 0.01, 0})
	vec.Add("big.go::Big#chunk2", []float32{0.98, 0.02, 0})
	vec.Add("small.go::Small", []float32{0.5, 0.5, 0})
	vec.SetChunkMap(map[string]string{
		"big.go::Big#chunk0": "big.go::Big",
		"big.go::Big#chunk1": "big.go::Big",
		"big.go::Big#chunk2": "big.go::Big",
	})

	text := NewBM25()
	text.Add("big.go::Big", "Big", "big.go", "")
	text.Add("small.go::Small", "Small", "small.go", "")

	h := NewHybrid(text, vec, fixedEmbedder{dims: dims})
	results := h.Search("anything", 10)
	require.NotEmpty(t, results)

	seen := map[string]int{}
	for _, r := range results {
		assert.NotContains(t, r.ID, "#chunk",
			"a synthetic chunk ID must never leak into search results")
		seen[r.ID]++
	}
	assert.Equal(t, 1, seen["big.go::Big"],
		"the sub-chunked symbol must appear exactly once despite owning three chunk vectors")
	assert.LessOrEqual(t, seen["small.go::Small"], 1)
}

// TestHybridSearch_DeChunkPreservesOrder asserts the de-chunk step
// keeps a symbol at the rank of its best-scoring chunk.
func TestHybridSearch_DeChunkPreservesOrder(t *testing.T) {
	const dims = 3
	vec := NewVector(dims)
	// The query vector is {1,0,0}. "A" owns the closest chunk; "B"
	// owns a chunk slightly further away. A must therefore outrank B.
	vec.Add("a.go::A#chunk0", []float32{1, 0, 0})
	vec.Add("b.go::B#chunk0", []float32{0.7, 0.7, 0})
	vec.SetChunkMap(map[string]string{
		"a.go::A#chunk0": "a.go::A",
		"b.go::B#chunk0": "b.go::B",
	})

	// Empty text backend so only the vector channel decides ordering.
	h := NewHybrid(NewBM25(), vec, fixedEmbedder{dims: dims})
	h.SetAutoAlpha(false) // plain RRF — vector ranks drive the order

	got := h.dechunkVectorIDs(vec.Search([]float32{1, 0, 0}, 8), 8)
	require.Len(t, got, 2)
	assert.Equal(t, "a.go::A", got[0], "the symbol with the nearest chunk must rank first")
	assert.Equal(t, "b.go::B", got[1])
}

// TestHybridSearch_NoChunkMapUnaffected asserts that when no symbol is
// chunked, search behaves exactly as before — IDs pass through.
func TestHybridSearch_NoChunkMapUnaffected(t *testing.T) {
	const dims = 3
	vec := NewVector(dims)
	vec.Add("a.go::A", []float32{1, 0, 0})
	vec.Add("b.go::B", []float32{0, 1, 0})
	require.False(t, vec.HasChunks(), "no SetChunkMap → HasChunks must be false")

	h := NewHybrid(NewBM25(), vec, fixedEmbedder{dims: dims})
	results := h.Search("anything", 10)
	for _, r := range results {
		assert.NotContains(t, r.ID, "#chunk")
	}
}

// TestVectorBackend_ResolveChunk covers the chunk → parent lookup in
// isolation: a mapped ID resolves to its parent, an unmapped ID passes
// through unchanged.
func TestVectorBackend_ResolveChunk(t *testing.T) {
	v := NewVector(2)
	v.SetChunkMap(map[string]string{"f.go::F#chunk1": "f.go::F"})

	parent, isChunk := v.ResolveChunk("f.go::F#chunk1")
	assert.True(t, isChunk)
	assert.Equal(t, "f.go::F", parent)

	plain, isChunk := v.ResolveChunk("g.go::G")
	assert.False(t, isChunk)
	assert.Equal(t, "g.go::G", plain, "an unmapped ID must pass through unchanged")
}

// TestVectorBackend_ChunkMapSurvivesSaveLoad asserts the chunk map is
// persisted by Save and restored by LoadFrom — the daemon snapshot and
// the per-repo cache both rely on this so de-chunking still works after
// a restart.
func TestVectorBackend_ChunkMapSurvivesSaveLoad(t *testing.T) {
	src := NewVector(3)
	src.Add("big.go::Big#chunk0", []float32{1, 0, 0})
	src.Add("big.go::Big#chunk1", []float32{0, 1, 0})
	src.SetChunkMap(map[string]string{
		"big.go::Big#chunk0": "big.go::Big",
		"big.go::Big#chunk1": "big.go::Big",
	})

	var buf strings.Builder
	require.NoError(t, src.Save(&stringWriter{&buf}))

	dst := NewVector(3)
	require.NoError(t, dst.LoadFrom(strings.NewReader(buf.String())))
	require.True(t, dst.HasChunks(), "chunk map must survive a Save/Load round-trip")

	parent, isChunk := dst.ResolveChunk("big.go::Big#chunk1")
	assert.True(t, isChunk)
	assert.Equal(t, "big.go::Big", parent)
}

// TestVectorBackend_LegacyBlobLoadsWithoutChunkMap asserts a legacy raw
// HNSW export (written before the framed format) still loads, with an
// empty chunk map — the back-compat path.
func TestVectorBackend_LegacyBlobLoadsWithoutChunkMap(t *testing.T) {
	// A VectorBackend with no chunk map, saved, then a fresh backend
	// loaded from a stream that has had the frame magic stripped to
	// simulate a pre-framing blob.
	src := NewVector(3)
	src.Add("a.go::A", []float32{1, 0, 0})
	var framed strings.Builder
	require.NoError(t, src.Save(&stringWriter{&framed}))

	raw := framed.String()
	// Frame layout: 4-byte magic + 4-byte map length + map JSON + HNSW.
	// Strip magic+length+"{}" (an empty map JSON) to get the bare HNSW.
	require.Greater(t, len(raw), 10)
	bare := raw[4+4+2:] // 4 magic, 4 length, 2 = len("{}")

	dst := NewVector(3)
	require.NoError(t, dst.LoadFrom(strings.NewReader(bare)),
		"a legacy un-framed HNSW blob must still load")
	assert.False(t, dst.HasChunks(), "a legacy blob has no chunk map")
}

// stringWriter adapts a strings.Builder to io.Writer for the tests
// above (strings.Builder already satisfies io.Writer, but the wrapper
// keeps the intent explicit).
type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }
