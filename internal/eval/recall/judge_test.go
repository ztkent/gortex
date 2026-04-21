package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerdictKey_OrderIndependent(t *testing.T) {
	k1 := verdictKey("claude", "find X", []string{"a/b.go::Foo", "c/d.go::Bar"})
	k2 := verdictKey("claude", "find X", []string{"c/d.go::Bar", "a/b.go::Foo"})
	assert.Equal(t, k1, k2, "verdict key must be independent of top-K order")
}

func TestVerdictKey_ChangesWithModel(t *testing.T) {
	k1 := verdictKey("claude-haiku-4-5", "q", []string{"a"})
	k2 := verdictKey("claude-opus-4", "q", []string{"a"})
	assert.NotEqual(t, k1, k2, "verdict key must differ when the model differs")
}

func TestJudgeCacheRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "judge_cache.json")
	j := &Judge{
		Model:     "claude-haiku-4-5",
		APIKey:    "test-key",
		CachePath: path,
	}
	j.loadCache()
	j.cacheMu.Lock()
	j.cache["abc"] = true
	j.cache["def"] = false
	j.cacheMu.Unlock()
	require.NoError(t, j.saveCache())

	// Reload from disk.
	j2 := &Judge{CachePath: path}
	j2.loadCache()
	assert.Equal(t, true, j2.cache["abc"])
	assert.Equal(t, false, j2.cache["def"])

	// Sanity: on-disk format is valid JSON.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var decoded map[string]bool
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Len(t, decoded, 2)
}

func TestNewJudge_NoAPIKeyReturnsNil(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	assert.Nil(t, NewJudge("claude-haiku-4-5"))
}

func TestNewJudge_EmptyModelReturnsNil(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fake")
	assert.Nil(t, NewJudge(""))
}

func TestApplyJudge_NilIsNoop(t *testing.T) {
	r := Report{Rankers: []RankerResult{
		{Name: "x", Hits: map[int]int{20: 5}, Recall: map[int]float64{20: 0.5}, Cases: 10},
	}}
	before := r.Rankers[0].Hits[20]
	rescued, errs := ApplyJudge(&r, nil)
	assert.Equal(t, 0, rescued)
	assert.Nil(t, errs)
	assert.Equal(t, before, r.Rankers[0].Hits[20])
}

func TestRun_CapturesMisses(t *testing.T) {
	fixture := Fixture{
		Name: "t",
		Cases: []Case{
			{ID: "hit",  Query: "q1", Tier: TierExact, Expected: []string{"a"}},
			{ID: "miss", Query: "q2", Tier: TierExact, Expected: []string{"z"}},
		},
	}
	r := staticRanker("x", []string{"a", "b", "c"})
	report := Run(fixture, []Ranker{r}, nil)
	require.Len(t, report.Rankers, 1)
	assert.Len(t, report.Rankers[0].Misses, 1)
	miss := report.Rankers[0].Misses[0]
	assert.Equal(t, "miss", miss.CaseID)
	assert.Equal(t, []string{"z"}, miss.Expected)
	assert.Equal(t, []string{"a", "b", "c"}, miss.Top)
	assert.Nil(t, miss.JudgedHit)
}

func TestApplyJudge_RescuesMissWithStubbedVerdict(t *testing.T) {
	// Shape-level test: we can't hit the real API, so pre-seed the
	// judge cache with a "yes" verdict for a known query+top.
	tmp := t.TempDir()
	cachePath := filepath.Join(tmp, "cache.json")
	query := "find me a writer"
	top := []string{"pkg/x.go::WriteThing"}

	judge := &Judge{
		Model:     "claude-stub",
		APIKey:    "stub",
		CachePath: cachePath,
	}
	judge.loadCache()
	judge.cacheMu.Lock()
	judge.cache[verdictKey("claude-stub", query, top)] = true
	judge.cacheMu.Unlock()

	report := Report{
		Rankers: []RankerResult{{
			Name:   "r",
			Cases:  1,
			Hits:   map[int]int{1: 0, 5: 0, 20: 0},
			Recall: map[int]float64{1: 0, 5: 0, 20: 0},
			Misses: []Miss{{
				CaseID:   "c1",
				Query:    query,
				Expected: []string{"pkg/x.go::OtherWriter"},
				Top:      top,
			}},
		}},
	}
	rescued, errs := ApplyJudge(&report, judge)
	assert.Empty(t, errs)
	assert.Equal(t, 1, rescued)
	// Judged hits bump the biggest-K bucket (20).
	assert.Equal(t, 1, report.Rankers[0].Hits[20])
	assert.Equal(t, 1.0, report.Rankers[0].Recall[20])
	// R@1 / R@5 are NOT credited by the judge — we don't know the
	// judged rank, so the conservative call is max-K only.
	assert.Equal(t, 0, report.Rankers[0].Hits[1])
	require.NotNil(t, report.Rankers[0].Misses[0].JudgedHit)
	assert.True(t, *report.Rankers[0].Misses[0].JudgedHit)
}
