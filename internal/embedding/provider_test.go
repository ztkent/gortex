package embedding

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStaticProvider_Embed(t *testing.T) {
	p, err := NewStaticProvider()
	require.NoError(t, err)

	// Inject test vectors.
	p.SetVectors(map[string][]float32{
		"validate": {1, 0, 0},
		"check":    {0.9, 0.1, 0},
		"token":    {0, 1, 0},
		"auth":     {0, 0.9, 0.1},
	})

	ctx := context.Background()

	// "validate token" should produce a non-zero vector.
	vec, err := p.Embed(ctx, "validateToken")
	require.NoError(t, err)
	assert.Len(t, vec, 3)
	assert.NotEqual(t, float32(0), vec[0], "should have non-zero components")

	// "check auth" should be similar to "validate token".
	vec2, err := p.Embed(ctx, "checkAuth")
	require.NoError(t, err)
	assert.Len(t, vec2, 3)

	// Both should be non-zero (found words in vocabulary).
	hasNonZero := false
	for _, v := range vec2 {
		if v != 0 {
			hasNonZero = true
			break
		}
	}
	assert.True(t, hasNonZero, "should find words in vocabulary")
}

func TestStaticProvider_EmbedBatch(t *testing.T) {
	p, err := NewStaticProvider()
	require.NoError(t, err)
	p.SetVectors(map[string][]float32{
		"hello": {1, 0},
		"world": {0, 1},
	})

	ctx := context.Background()
	vecs, err := p.EmbedBatch(ctx, []string{"hello", "world", "unknown"})
	require.NoError(t, err)
	assert.Len(t, vecs, 3)
}

func TestStaticProvider_UnknownWords(t *testing.T) {
	p, err := NewStaticProvider()
	require.NoError(t, err)
	p.SetVectors(map[string][]float32{
		"known": {1, 0},
	})

	ctx := context.Background()
	vec, err := p.Embed(ctx, "completelyunknownword")
	require.NoError(t, err)
	// Should return zero vector for unknown words.
	for _, v := range vec {
		assert.Equal(t, float32(0), v)
	}
}

func TestNopProvider(t *testing.T) {
	var p NopProvider
	ctx := context.Background()

	_, err := p.Embed(ctx, "test")
	assert.ErrorIs(t, err, ErrDisabled)

	_, err = p.EmbedBatch(ctx, []string{"test"})
	assert.ErrorIs(t, err, ErrDisabled)

	assert.Equal(t, 0, p.Dimensions())
	assert.NoError(t, p.Close())
}

func TestStaticProvider_SemanticSimilarity(t *testing.T) {
	p, err := NewStaticProvider()
	require.NoError(t, err)
	require.Greater(t, len(p.vectors), 1000, "should have loaded GloVe vectors")

	ctx := context.Background()

	// "validate token" and "check authentication" should produce non-zero, similar vectors.
	vec1, err := p.Embed(ctx, "validate token")
	require.NoError(t, err)

	vec2, err := p.Embed(ctx, "check authentication")
	require.NoError(t, err)

	// Both should be non-zero.
	nonZero1 := false
	for _, v := range vec1 {
		if v != 0 {
			nonZero1 = true
			break
		}
	}
	assert.True(t, nonZero1, "validate token should produce non-zero vector")

	// Compute cosine similarity.
	var dot float64
	for i := range vec1 {
		dot += float64(vec1[i]) * float64(vec2[i])
	}
	// Both vectors are normalized, so dot product = cosine similarity.
	assert.Greater(t, dot, 0.3, "semantically similar queries should have cosine > 0.3")
}

func TestNewLocalProvider_ReturnsWorkingProvider(t *testing.T) {
	p, err := NewLocalProvider()
	require.NoError(t, err)
	defer func() { _ = p.Close() }()

	// Default build walks ONNX → GoMLX → Hugot → Static and returns
	// the first that initialises. Pre-2026-04 the Hugot path failed on
	// the multi-onnx HuggingFace repo so Static was the fallback; with
	// that pinned to onnx/model.onnx, Hugot now succeeds when the
	// model is cached or the network is reachable. Either is fine —
	// the invariant is "NewLocalProvider returns a working provider."
	assert.NotNil(t, p)
	assert.Greater(t, p.Dimensions(), 0, "provider must report positive dimensions")
}

func TestTokenizeForEmbedding(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"validateToken", []string{"validate", "token"}},
		{"get_user_by_id", []string{"get", "user", "by", "id"}},
		{"internal/auth/service.go", []string{"internal", "auth", "service", "go"}},
		{"a b", nil}, // single chars dropped
		{"HandleRequest", []string{"handle", "request"}},
	}
	for _, tt := range tests {
		got := tokenizeForEmbedding(tt.input)
		assert.Equal(t, tt.expected, got, "tokenize(%q)", tt.input)
	}
}
