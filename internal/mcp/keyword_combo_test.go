package mcp

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeywordTokens_DropsStopWordsAndGenerics(t *testing.T) {
	got := keywordTokens("how do we validate the auth token function")
	// Function words (how, do, we, the) and generic nouns (function)
	// are dropped; the meaningful keywords survive.
	for _, want := range []string{"validate", "auth", "token"} {
		if !slices.Contains(got, want) {
			t.Errorf("keywordTokens missing %q; got %v", want, got)
		}
	}
	for _, drop := range []string{"how", "do", "the", "function", "we"} {
		if slices.Contains(got, drop) {
			t.Errorf("keywordTokens should have dropped %q; got %v", drop, got)
		}
	}
	// camelCase queries split into component tokens.
	cc := keywordTokens("validateAuthToken")
	for _, want := range []string{"validate", "auth", "token"} {
		if !slices.Contains(cc, want) {
			t.Errorf("camelCase keyword split missing %q; got %v", want, cc)
		}
	}
}

// TestKeywordBoost_CrossQueryInheritance is the core feature test: a
// symbol recorded under one phrasing is inherited by a later query
// that shares keywords but is phrased differently.
func TestKeywordBoost_CrossQueryInheritance(t *testing.T) {
	cm := &comboManager{now: func() int64 { return 1 }}

	// Train: "validate auth token" leads to sym-auth, recorded enough
	// times to clear the per-keyword min-hits gate.
	for i := 0; i < 3; i++ {
		cm.Record("validate auth token", "sym-auth")
	}

	// A differently-phrased query that shares the auth/token keywords.
	// Its whole-query key never matched, so the exact-combo path is
	// silent...
	assert.Nil(t, cm.BoostMap("check auth token here"),
		"a never-seen whole query should have no exact-combo boost")

	// ...but the per-keyword path inherits the association.
	kw := cm.KeywordBoostMap("check auth token here")
	require.NotNil(t, kw, "keyword boost should inherit across phrasings")
	assert.Greater(t, kw["sym-auth"], 1.0,
		"sym-auth should be boosted via its auth/token keyword associations")
}

// TestKeywordBoost_CoverageScaling confirms a symbol matching more of
// the query's keywords gets a larger boost than one matching fewer.
func TestKeywordBoost_CoverageScaling(t *testing.T) {
	cm := &comboManager{now: func() int64 { return 1 }}
	// sym-full is recorded under all three keywords; sym-partial under
	// only one. Record each enough to clear the gate.
	for i := 0; i < 4; i++ {
		cm.Record("parse encode validate", "sym-full")
		cm.Record("validate", "sym-partial")
	}
	kw := cm.KeywordBoostMap("parse encode validate")
	require.NotNil(t, kw)
	require.Contains(t, kw, "sym-full")
	require.Contains(t, kw, "sym-partial")
	assert.Greater(t, kw["sym-full"], kw["sym-partial"],
		"a symbol covering more query keywords should out-boost one covering fewer")
}

// TestKeywordBoost_CoverageIsLinear pins the boost above 1.0 to scale
// linearly with coverage when per-keyword strength is equal: a symbol
// matching 1 of 3 keywords earns ~1/3 of the full-coverage boost. Guards
// against the super-linear (coverage-times-summed-hits) regression.
func TestKeywordBoost_CoverageIsLinear(t *testing.T) {
	cm := &comboManager{now: func() int64 { return 1 }}
	// Equal per-keyword strength: sym-full under all 3 keywords, sym-partial
	// under 1, each recorded the same number of times.
	for i := 0; i < 4; i++ {
		cm.Record("parse encode validate", "sym-full")
		cm.Record("validate", "sym-partial")
	}
	kw := cm.KeywordBoostMap("parse encode validate")
	require.NotNil(t, kw)
	full := kw["sym-full"] - 1.0
	partial := kw["sym-partial"] - 1.0
	require.Greater(t, full, 0.0)
	require.Greater(t, partial, 0.0)
	// 3 keywords ⇒ full coverage boost is ~3× the 1-of-3 boost.
	assert.InDelta(t, 3.0, full/partial, 0.05,
		"partial-coverage boost should be ~1/N of full coverage (linear, not super-linear)")
}

// TestKeywordBoost_ExactQueryDominates confirms the exact whole-query
// combo boost out-ranks a keyword-only boost when both fire -- the
// keyword boost is capped below comboMaxBoost.
func TestKeywordBoost_ExactQueryDominates(t *testing.T) {
	cm := &comboManager{now: func() int64 { return 1 }}
	// Heavily train an exact query -> exact boost saturates near
	// comboMaxBoost.
	for i := 0; i < 50; i++ {
		cm.Record("auth token flow", "sym-x")
	}
	exact := cm.BoostMap("auth token flow")
	keyword := cm.KeywordBoostMap("auth token flow")
	require.NotNil(t, exact)
	require.NotNil(t, keyword)
	assert.Greater(t, exact["sym-x"], keyword["sym-x"],
		"exact whole-query combo must dominate the keyword-only boost")
	assert.LessOrEqual(t, keyword["sym-x"], keywordMaxBoost+1e-9,
		"keyword boost must stay under its cap")
}

// TestKeywordBoost_BelowGate confirms a keyword association below the
// min-hits gate does not boost.
func TestKeywordBoost_BelowGate(t *testing.T) {
	cm := &comboManager{now: func() int64 { return 1 }}
	cm.Record("auth token", "sym-q") // a single hit -- below keywordMinHits
	assert.Nil(t, cm.KeywordBoostMap("auth token"),
		"a single keyword hit should not clear the gate")
}

// TestKeywordBoost_Decay confirms stale keyword associations are
// reaped on access, just like the exact-combo store.
func TestKeywordBoost_Decay(t *testing.T) {
	now := int64(1_000_000_000)
	cm := &comboManager{mode: ModeAI, now: func() int64 { return now }}
	for i := 0; i < 3; i++ {
		cm.Record("auth token", "sym-stale")
	}
	require.NotNil(t, cm.KeywordBoostMap("auth token"), "fresh association should boost")

	// Jump past the AI max-age window.
	now += comboMaxAgeAISec + 1
	assert.Nil(t, cm.KeywordBoostMap("auth token"),
		"a keyword association past the decay window must be reaped")
}

// TestKeywordBoost_Persistence confirms the keyword store survives a
// manager reload from disk.
func TestKeywordBoost_Persistence(t *testing.T) {
	cacheDir := t.TempDir()
	repoPath := filepath.Join(t.TempDir(), "repo")

	cm := newComboManager(cacheDir, repoPath, ModeAI)
	for i := 0; i < 3; i++ {
		cm.Record("validate auth token", "sym-persist")
	}

	cm2 := newComboManager(cacheDir, repoPath, ModeAI)
	kw := cm2.KeywordBoostMap("check auth token now")
	require.NotNil(t, kw, "keyword associations should survive a reload")
	assert.Greater(t, kw["sym-persist"], 1.0)
}

// TestKeywordBoost_NilSafe confirms a nil manager is safe.
func TestKeywordBoost_NilSafe(t *testing.T) {
	var cm *comboManager
	assert.Nil(t, cm.KeywordBoostMap("auth token"))
}
