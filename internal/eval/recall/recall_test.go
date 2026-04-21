package recall

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/search"
)

// staticRanker returns a predetermined ranked list regardless of query.
// Lets us pin exact metrics without standing up the full indexer.
func staticRanker(name string, ids []string) Ranker {
	return Ranker{
		Name: name,
		Search: func(_ string, limit int) []string {
			if limit >= len(ids) {
				return ids
			}
			return ids[:limit]
		},
	}
}

func TestRun_AllHitsAtRankOne(t *testing.T) {
	fixture := Fixture{
		Name: "t",
		Cases: []Case{
			{Query: "q1", Tier: TierExact, Expected: []string{"a"}},
			{Query: "q2", Tier: TierExact, Expected: []string{"a"}},
		},
	}
	r := staticRanker("perfect", []string{"a", "b", "c"})
	report := Run(fixture, []Ranker{r}, nil)
	assert.Equal(t, 2, report.Cases)
	res := report.Rankers[0]
	assert.Equal(t, 2, res.Hits[1])
	assert.Equal(t, 2, res.Hits[5])
	assert.Equal(t, 2, res.Hits[20])
	assert.Equal(t, 1.0, res.Recall[1])
	assert.Equal(t, 1.0, res.MeanRRank)
	// Per-tier bookkeeping: exact tier should get both hits.
	assert.Equal(t, 2, res.PerTier[TierExact].Hits[1])
	assert.Equal(t, 0, res.PerTier[TierConcept].Hits[1])
}

func TestRun_MissAndRankTwo(t *testing.T) {
	fixture := Fixture{
		Name: "t",
		Cases: []Case{
			{Query: "q1", Tier: TierExact, Expected: []string{"b"}}, // rank 2
			{Query: "q2", Tier: TierExact, Expected: []string{"z"}}, // miss
		},
	}
	r := staticRanker("partial", []string{"a", "b", "c"})
	report := Run(fixture, []Ranker{r}, nil)
	res := report.Rankers[0]
	assert.Equal(t, 0, res.Hits[1])
	assert.Equal(t, 1, res.Hits[5])
	assert.Equal(t, 1, res.Hits[20])
	assert.Equal(t, 0.5, res.Recall[5])
	assert.InDelta(t, 0.25, res.MeanRRank, 1e-9)
}

func TestRun_AnyExpectedIsHit(t *testing.T) {
	fixture := Fixture{
		Name: "t",
		Cases: []Case{
			{Query: "q1", Tier: TierMultiHop, Expected: []string{"z", "b", "y"}},
		},
	}
	r := staticRanker("multi-expected", []string{"a", "b", "c"})
	report := Run(fixture, []Ranker{r}, nil)
	res := report.Rankers[0]
	assert.Equal(t, 1, res.Hits[5])
	assert.InDelta(t, 0.5, res.MeanRRank, 1e-9)
	assert.Equal(t, 1, res.PerTier[TierMultiHop].Hits[5])
}

func TestRun_LatencyPercentiles(t *testing.T) {
	fixture := Fixture{
		Name: "t",
		Cases: []Case{
			{Query: "q", Tier: TierExact, Expected: []string{"a"}},
			{Query: "q", Tier: TierExact, Expected: []string{"a"}},
			{Query: "q", Tier: TierExact, Expected: []string{"a"}},
		},
	}
	r := staticRanker("x", []string{"a"})
	report := Run(fixture, []Ranker{r}, nil)
	// Latency buckets exist and are non-negative.
	assert.GreaterOrEqual(t, report.Rankers[0].P50Micros, int64(0))
	assert.GreaterOrEqual(t, report.Rankers[0].P95Micros, report.Rankers[0].P50Micros)
	assert.GreaterOrEqual(t, report.Rankers[0].MaxMicros, report.Rankers[0].P95Micros)
}

func TestRun_TokensReturnedCounted(t *testing.T) {
	fixture := Fixture{
		Name:  "t",
		Cases: []Case{{Query: "q", Tier: TierExact, Expected: []string{"a"}}},
	}
	r := staticRanker("x", []string{"aaaaaaaa", "bbbbbbbb"})
	// Token counter that counts bytes to make the assertion deterministic.
	report := Run(fixture, []Ranker{r}, func(s string) int { return len(s) })
	assert.Greater(t, report.Rankers[0].MeanTokens, 0.0)
}

func TestMarkdownStableOrder(t *testing.T) {
	fixture := Fixture{
		Name:  "t",
		Cases: []Case{{Query: "q", Tier: TierExact, Expected: []string{"a"}}},
	}
	a := staticRanker("zeta", []string{"a"})
	b := staticRanker("alpha", []string{"a"})
	report := Run(fixture, []Ranker{a, b}, nil)
	md := Markdown(report)
	alphaIdx := strings.Index(md, "| alpha ")
	zetaIdx := strings.Index(md, "| zeta ")
	assert.Greater(t, alphaIdx, 0)
	assert.Greater(t, zetaIdx, alphaIdx)
	// Per-tier section present.
	assert.Contains(t, md, "## Per tier (R@5)")
}

func TestMarkdownSkippedRankerRow(t *testing.T) {
	fixture := Fixture{
		Name:  "t",
		Cases: []Case{{Query: "q", Tier: TierExact, Expected: []string{"a"}}},
	}
	r := Ranker{Name: "semantic", Search: func(_ string, _ int) []string { return nil }}
	report := Run(fixture, []Ranker{r}, nil)
	report.Rankers[0].Skipped = "no embedder"
	md := Markdown(report)
	assert.Contains(t, md, "skipped: no embedder")
}

// TestBM25Ranker_AgainstRealBackend wires the adapter to a live BM25
// backend and spot-checks ranked output.
func TestBM25Ranker_AgainstRealBackend(t *testing.T) {
	backend := search.NewBM25()
	backend.Add("pkg/a.go::Foo", "Foo", "pkg/a.go", "")
	backend.Add("pkg/b.go::Bar", "Bar", "pkg/b.go", "")

	r := BM25Ranker("bm25", backend)
	hits := r.Search("Foo", 5)
	assert.NotEmpty(t, hits)
	assert.Equal(t, "pkg/a.go::Foo", hits[0])

	fixture := Fixture{Cases: []Case{
		{Query: "Bar", Tier: TierExact, Expected: []string{"pkg/b.go::Bar"}},
		{Query: "Foo", Tier: TierExact, Expected: []string{"pkg/a.go::Foo"}},
	}}
	report := Run(fixture, []Ranker{r}, nil)
	assert.Equal(t, 2, report.Rankers[0].Hits[1])
}

func TestAdaptCasesForFileRanker(t *testing.T) {
	in := []Case{
		{Query: "q", Expected: []string{"a/b.go::Foo", "a/b.go::Bar", "c/d.go::Baz"}},
	}
	out := AdaptCasesForFileRanker(in)
	assert.Equal(t, []string{"a/b.go", "c/d.go"}, out[0].Expected)
}
