package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// indexedVectorBackend indexes dir with the given chunk options and
// returns the vector backend buildSearchIndex produced.
func indexedVectorBackend(t *testing.T, dir string, opts embedding.ChunkOptions) *search.VectorBackend {
	t.Helper()
	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())

	cfg := config.Default().Index
	cfg.Workers = 1

	idx := New(g, reg, cfg, zap.NewNop())
	idx.SetEmbedder(stubEmbedder{})
	idx.SetEmbeddingChunkOptions(opts)

	_, err := idx.Index(dir)
	require.NoError(t, err)

	sw, ok := idx.Search().(*search.Swappable)
	require.True(t, ok)
	hyb, ok := sw.Inner().(*search.HybridBackend)
	require.True(t, ok, "an embedder-equipped index must produce a HybridBackend")
	return hyb.VectorIndex()
}

// TestBuildSearchIndex_LongSymbolYieldsMultipleChunks proves the
// pipeline change: a function whose source span exceeds the chunk
// threshold is split into several chunk vectors, and the chunk →
// parent mapping is recorded on the vector backend.
func TestBuildSearchIndex_LongSymbolYieldsMultipleChunks(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package main\n\nfunc BigFunc() {\n")
	for i := 0; i < 80; i++ {
		b.WriteString("\tprintln(\"line\")\n")
	}
	b.WriteString("}\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.go"), []byte(b.String()), 0o644))

	// Low threshold + small window so BigFunc is forced to split.
	vec := indexedVectorBackend(t, dir, embedding.ChunkOptions{ThresholdLines: 20, WindowLines: 15})

	require.True(t, vec.HasChunks(),
		"a long function past the threshold must produce chunk vectors")

	// At least two of BigFunc's chunk vectors must resolve back to it.
	chunkCount := 0
	for i := 0; i < 20; i++ {
		id := bigFuncChunkID(i)
		if parent, isChunk := vec.ResolveChunk(id); isChunk {
			assert.Equal(t, "big.go::BigFunc", parent)
			chunkCount++
		}
	}
	assert.GreaterOrEqual(t, chunkCount, 2,
		"BigFunc must be split into at least two chunk vectors")
}

// bigFuncChunkID builds the synthetic chunk ID buildSearchIndex stamps
// on BigFunc's i-th window.
func bigFuncChunkID(i int) string {
	return "big.go::BigFunc#chunk" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// TestBuildSearchIndex_ShortSymbolStaysWhole proves the inverse: a file
// of only small functions produces no chunk vectors — every symbol is
// embedded under its own ID.
func TestBuildSearchIndex_ShortSymbolStaysWhole(t *testing.T) {
	dir := t.TempDir()
	src := `package main

func Tiny() {
	println("a")
}

func AlsoTiny() {
	println("b")
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "small.go"), []byte(src), 0o644))

	vec := indexedVectorBackend(t, dir, embedding.ChunkOptions{ThresholdLines: 60, WindowLines: 40})
	assert.False(t, vec.HasChunks(),
		"a file of only short functions must produce no chunk vectors")
	assert.Greater(t, vec.Count(), 0, "the symbols must still be embedded")
}

// TestBuildSearchIndex_ChunkedSymbolNotDuplicatedInSearch proves the
// end-to-end de-chunk contract through a real index: searching for a
// long symbol returns it once, and no synthetic chunk ID surfaces.
func TestBuildSearchIndex_ChunkedSymbolNotDuplicatedInSearch(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package main\n\nfunc ValidateRequestPayload() {\n")
	for i := 0; i < 80; i++ {
		b.WriteString("\tcheckField()\n")
	}
	b.WriteString("}\n\nfunc checkField() {}\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "v.go"), []byte(b.String()), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	idx := New(g, reg, cfg, zap.NewNop())
	idx.SetEmbedder(stubEmbedder{})
	idx.SetEmbeddingChunkOptions(embedding.ChunkOptions{ThresholdLines: 20, WindowLines: 15})
	_, err := idx.Index(dir)
	require.NoError(t, err)

	results := idx.Search().Search("ValidateRequestPayload", 20)
	require.NotEmpty(t, results)

	seen := map[string]int{}
	for _, r := range results {
		assert.NotContains(t, r.ID, "#chunk",
			"a chunk ID must never appear in search output")
		seen[r.ID]++
	}
	assert.LessOrEqual(t, seen["v.go::ValidateRequestPayload"], 1,
		"the chunked symbol must not be returned more than once")
}
