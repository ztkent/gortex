package indexer

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
)

// poolEmbedder is a fake API-style embedding provider for the pool
// tests. It reports Concurrent() == true so embedAllChunks picks the
// parallel path, records the peak number of EmbedBatch calls running
// at once, and embeds each text to a deterministic vector derived from
// the text so callers can assert results landed at the right index.
type poolEmbedder struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	calls    int32
	// delay, when set, makes every EmbedBatch sleep so concurrent
	// calls genuinely overlap and the peak measurement is meaningful.
	delay time.Duration
	// failOnText, when non-empty, makes EmbedBatch return an error for
	// any batch containing that exact text — the error-injection hook.
	failOnText string
}

func (p *poolEmbedder) Concurrent() bool { return true }
func (p *poolEmbedder) Dimensions() int  { return 3 }
func (p *poolEmbedder) Close() error     { return nil }

func (p *poolEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	out, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

func (p *poolEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	atomic.AddInt32(&p.calls, 1)
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()

	if p.delay > 0 {
		time.Sleep(p.delay)
	}

	out := make([][]float32, len(texts))
	for i, txt := range texts {
		if p.failOnText != "" && txt == p.failOnText {
			return nil, fmt.Errorf("poolEmbedder: injected failure on %q", txt)
		}
		// Vector encodes the text's numeric suffix so a caller can
		// verify the result landed at the matching index.
		out[i] = []float32{textToScalar(txt), 0, 0}
	}
	return out, nil
}

// textToScalar maps "t<N>" to N as a float32 so each embedded text has
// a unique, position-revealing vector.
func textToScalar(txt string) float32 {
	if len(txt) > 1 && txt[0] == 't' {
		if n, err := strconv.Atoi(txt[1:]); err == nil {
			return float32(n)
		}
	}
	return -1
}

// newPoolTestIndexer builds a minimal Indexer wired with the given
// embedder and API concurrency cap, for direct embedAllChunks tests.
func newPoolTestIndexer(t *testing.T, emb interface {
	Embed(context.Context, string) ([]float32, error)
	EmbedBatch(context.Context, []string) ([][]float32, error)
	Dimensions() int
	Close() error
}, concurrency int) *Indexer {
	t.Helper()
	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetEmbedder(emb)
	idx.SetEmbeddingAPIConcurrency(concurrency)
	return idx
}

// makeTexts builds n texts "t0".."t<n-1>".
func makeTexts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "t" + strconv.Itoa(i)
	}
	return out
}

// passthroughEmbedFn is the embedFn embedAllChunks expects — here it
// just forwards to the indexer's embedder with no retry wrapping, so
// the tests exercise the pool itself.
func passthroughEmbedFn(idx *Indexer) func(context.Context, []string) ([][]float32, error) {
	return func(ctx context.Context, items []string) ([][]float32, error) {
		return idx.embedder.EmbedBatch(ctx, items)
	}
}

// TestEmbedAllChunks_PoolRespectsConcurrencyCap asserts the worker
// pool never runs more EmbedBatch calls at once than the configured
// cap, embeds every chunk, and returns vectors in input order.
func TestEmbedAllChunks_PoolRespectsConcurrencyCap(t *testing.T) {
	emb := &poolEmbedder{delay: 15 * time.Millisecond}
	const cap = 3
	idx := newPoolTestIndexer(t, emb, cap)

	texts := makeTexts(60) // 60 texts, batch 5 → 12 batches > cap
	vectors, err := idx.embedAllChunks(texts, 5, passthroughEmbedFn(idx))
	require.NoError(t, err)

	require.Len(t, vectors, len(texts), "every text must be embedded")
	assert.LessOrEqual(t, emb.peak, cap,
		"peak in-flight EmbedBatch calls (%d) must not exceed the cap (%d)", emb.peak, cap)
	assert.Greater(t, emb.peak, 1, "the pool must actually run batches in parallel")

	// Order preserved: vectors[i] must encode texts[i].
	for i := range texts {
		require.Len(t, vectors[i], 3)
		assert.Equal(t, float32(i), vectors[i][0],
			"vector at index %d must correspond to text %q", i, texts[i])
	}
}

// TestEmbedAllChunks_AbortsOnError asserts that a single batch failure
// aborts the whole embed: embedAllChunks returns the error and no
// partial vector slice.
func TestEmbedAllChunks_AbortsOnError(t *testing.T) {
	emb := &poolEmbedder{delay: 5 * time.Millisecond, failOnText: "t37"}
	idx := newPoolTestIndexer(t, emb, 4)

	vectors, err := idx.embedAllChunks(makeTexts(80), 5, passthroughEmbedFn(idx))
	require.Error(t, err, "one failing chunk must fail the whole embed")
	assert.Nil(t, vectors, "a failed embed must return no partial result")
	assert.Contains(t, err.Error(), "t37")
}

// TestBuildSearchIndex_AbortOnEmbedErrorKeepsTextOnly asserts the
// end-to-end abort contract: when embedding fails, buildSearchIndex
// leaves the search backend text-only — no HybridBackend is swapped in.
func TestBuildSearchIndex_AbortOnEmbedErrorKeepsTextOnly(t *testing.T) {
	g := graph.New()
	// Two function nodes; their embed metadata text is
	// "function <Name> ...". poolEmbedder fails on an exact text
	// match, so failing on a metadata string aborts the embed.
	g.AddNode(&graph.Node{ID: "a.go::Alpha", Name: "Alpha", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Beta", Name: "Beta", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"})

	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	emb := &poolEmbedder{failOnText: "function Alpha  a.go"}
	idx.SetEmbedder(emb)

	idx.buildSearchIndex()

	// The backend must NOT be a HybridBackend — embedding aborted, so
	// the search stays text-only.
	sw, ok := idx.Search().(*search.Swappable)
	require.True(t, ok)
	_, isHybrid := sw.Inner().(*search.HybridBackend)
	assert.False(t, isHybrid,
		"buildSearchIndex must not install a HybridBackend when embedding fails")
}

// TestEmbedAllChunks_DeterministicRegardlessOfOrder runs the pool many
// times with overlapping workers and asserts the vectors always land
// at the input-aligned indices — completion order must never reshuffle
// the result.
func TestEmbedAllChunks_DeterministicRegardlessOfOrder(t *testing.T) {
	for trial := 0; trial < 8; trial++ {
		emb := &poolEmbedder{delay: time.Millisecond}
		idx := newPoolTestIndexer(t, emb, 6)

		texts := makeTexts(50)
		vectors, err := idx.embedAllChunks(texts, 3, passthroughEmbedFn(idx))
		require.NoError(t, err)
		require.Len(t, vectors, len(texts))
		for i := range texts {
			assert.Equal(t, float32(i), vectors[i][0],
				"trial %d: index %d must hold the vector for %q regardless of completion order",
				trial, i, texts[i])
		}
	}
}

// TestEmbedAllChunks_SerialForNonConcurrentEmbedder asserts an
// embedder that does not implement Concurrent() is run serially — the
// peak in-flight count stays at 1.
func TestEmbedAllChunks_SerialForNonConcurrentEmbedder(t *testing.T) {
	emb := &serialOnlyEmbedder{delay: 5 * time.Millisecond}
	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetEmbedder(emb)
	idx.SetEmbeddingAPIConcurrency(8)

	vectors, err := idx.embedAllChunks(makeTexts(30), 3, passthroughEmbedFn(idx))
	require.NoError(t, err)
	require.Len(t, vectors, 30)
	assert.Equal(t, 1, emb.peak,
		"an embedder without Concurrent() must be driven serially (peak in-flight 1)")
}

// serialOnlyEmbedder is a fake embedder that does NOT implement
// Concurrent(), modelling an in-process transformer backend.
type serialOnlyEmbedder struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	delay    time.Duration
}

func (e *serialOnlyEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	out, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

func (e *serialOnlyEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.inFlight++
	if e.inFlight > e.peak {
		e.peak = e.inFlight
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.inFlight--
		e.mu.Unlock()
	}()
	if e.delay > 0 {
		time.Sleep(e.delay)
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func (e *serialOnlyEmbedder) Dimensions() int { return 3 }
func (e *serialOnlyEmbedder) Close() error    { return nil }
