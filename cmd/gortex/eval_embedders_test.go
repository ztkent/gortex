package main

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/embedding"
)

func TestPickVariants_DefaultNonEmpty(t *testing.T) {
	// Default (empty CSV) must return a non-empty list on any arch.
	got := pickVariants("")
	assert.NotEmpty(t, got)
	assert.Contains(t, got, "fp32")
}

func TestPickVariants_CSVParsing(t *testing.T) {
	got := pickVariants(" fp32 , qint8_arm64 , o3 ")
	assert.Equal(t, []string{"fp32", "qint8_arm64", "o3"}, got)
}

func TestPickVariants_EmptyCSVEntriesDropped(t *testing.T) {
	got := pickVariants("fp32,,qint8_arm64,")
	assert.Equal(t, []string{"fp32", "qint8_arm64"}, got)
}

func TestLatPercentiles_BasicShape(t *testing.T) {
	// Sorted ascending: p50 should be the middle-ish element, p95 near the top.
	lats := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	p50, p95 := latPercentiles(lats)
	assert.Greater(t, p95, p50)
	assert.GreaterOrEqual(t, p50, int64(5))
	assert.GreaterOrEqual(t, p95, int64(9))
}

func TestLatPercentiles_UnsortedInput(t *testing.T) {
	// Must sort internally, not trust caller ordering. p95 of a
	// 5-element slice is sorted[int(4 * 0.95)] = sorted[3] per the
	// rank-floor convention used throughout this package.
	lats := []int64{10, 1, 5, 3, 7}
	p50, p95 := latPercentiles(lats)
	assert.Equal(t, int64(5), p50)
	assert.GreaterOrEqual(t, p95, p50)
}

func TestLatPercentiles_Empty(t *testing.T) {
	p50, p95 := latPercentiles(nil)
	assert.Equal(t, int64(0), p50)
	assert.Equal(t, int64(0), p95)
}

func TestKnownHugotVariants_AllLookupRoundTrip(t *testing.T) {
	names := embedding.KnownHugotVariants()
	assert.NotEmpty(t, names)
	// Output must be sorted so help text is stable.
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	assert.Equal(t, sorted, names, "KnownHugotVariants output must be sorted")
	// Every advertised name must resolve via Lookup.
	for _, n := range names {
		v, ok := embedding.LookupHugotVariant(n)
		assert.True(t, ok, "lookup failed for %s", n)
		assert.NotEmpty(t, v.OnnxFile, "empty OnnxFile for %s", n)
	}
}

func TestLookupHugotVariant_UnknownReturnsFalse(t *testing.T) {
	_, ok := embedding.LookupHugotVariant("does-not-exist")
	assert.False(t, ok)
}

func TestRecommendation_MissingPairFallsBack(t *testing.T) {
	// Only fp32 measured → recommendation falls back to "run again
	// with both variants" message, not an arithmetic crash.
	r := embeddersReport{Rows: []embedderResult{{Variant: "fp32", EmbedP50Micros: 100}}}
	msg := recommendation(r, true)
	assert.Contains(t, msg, "Only one variant")
}

func TestRecommendation_QuantizedPair(t *testing.T) {
	r := embeddersReport{Rows: []embedderResult{
		{Variant: "fp32", EmbedP50Micros: 60000, ModelSizeMB: 86.0, Recall: map[int]float64{5: 0.20}},
		{Variant: "qint8_arm64", EmbedP50Micros: 25000, ModelSizeMB: 22.0, Recall: map[int]float64{5: 0.18}},
	}}
	msg := recommendation(r, false)
	assert.Contains(t, msg, "Pick `fp32`")
	assert.Contains(t, msg, "Pick `qint8_arm64`")
	assert.Contains(t, msg, "faster per query")
	assert.Contains(t, msg, "smaller on disk")
	assert.Contains(t, msg, "quality delta")
}

func TestPctOrDash(t *testing.T) {
	assert.Equal(t, "—", pctOrDash(0))
	assert.Equal(t, "12.5%", pctOrDash(0.125))
}
