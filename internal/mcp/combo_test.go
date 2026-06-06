package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComboManager_RecordAndBoost(t *testing.T) {
	cm := &comboManager{now: func() int64 { return 1 }}

	// Below threshold: no boost yet.
	cm.Record("auth middleware", "sym-a")
	cm.Record("auth middleware", "sym-a")
	assert.Nil(t, cm.BoostMap("auth middleware"), "below min-hits should not boost")

	// Third hit tips over the threshold.
	cm.Record("auth middleware", "sym-a")
	boosts := cm.BoostMap("auth middleware")
	require.NotNil(t, boosts)
	assert.Greater(t, boosts["sym-a"], 1.0)
	assert.Less(t, boosts["sym-a"], comboMaxBoost+0.01)
}

func TestComboManager_NormalizeQuery(t *testing.T) {
	assert.Equal(t, "auth middleware", normalizeQuery("  AUTH   Middleware "))
	assert.Equal(t, "auth middleware", normalizeQuery("auth middleware"))
	assert.Equal(t, "", normalizeQuery("   "))
}

func TestComboManager_BoostCappedAtMax(t *testing.T) {
	cm := &comboManager{now: func() int64 { return 1 }}
	for i := 0; i < 50; i++ {
		cm.Record("q", "sym-hot")
	}
	boosts := cm.BoostMap("q")
	require.NotNil(t, boosts)
	assert.Equal(t, comboMaxBoost, boosts["sym-hot"])
}

func TestComboManager_PerQueryEntriesCapped(t *testing.T) {
	cm := &comboManager{now: func() int64 { return 1 }}
	// Record many distinct symbols against the same query — the matches
	// list must stay bounded so a single chatty query can't blow up the
	// file.
	for i := 0; i < 100; i++ {
		cm.Record("q", "sym-"+string(rune('A'+i%26))+string(rune('0'+i%10)))
	}
	// Lookup the query's entry via an internal peek: at most MaxComboEntries.
	cm.mu.Lock()
	defer cm.mu.Unlock()
	require.Len(t, cm.store.Queries, 1)
	assert.LessOrEqual(t, len(cm.store.Queries[0].Matches), 20)
}

func TestComboManager_MultipleQueriesIsolated(t *testing.T) {
	cm := &comboManager{now: func() int64 { return 1 }}
	for i := 0; i < 3; i++ {
		cm.Record("query one", "alpha")
		cm.Record("query two", "beta")
	}
	b1 := cm.BoostMap("query one")
	b2 := cm.BoostMap("query two")
	require.NotNil(t, b1)
	require.NotNil(t, b2)
	assert.Contains(t, b1, "alpha")
	assert.NotContains(t, b1, "beta")
	assert.Contains(t, b2, "beta")
}

func TestComboManager_Persistence(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	cacheDir := filepath.Join(dir, "cache")

	cm := newComboManager(cacheDir, repoPath, ModeAI)
	cm.now = func() int64 { return 1 }
	for i := 0; i < 3; i++ {
		cm.Record("auth middleware", "sym-a")
	}
	require.True(t, cm.HasData())

	// Reload and verify the boost survives a restart. Freeze the reloader's
	// clock at the same instant so the reap-on-BoostMap pass doesn't treat
	// the entries as stale relative to wall-clock now.
	cm2 := newComboManager(cacheDir, repoPath, ModeAI)
	cm2.now = func() int64 { return 1 }
	boosts := cm2.BoostMap("auth middleware")
	require.NotNil(t, boosts, "persisted combo should still boost after reload")
	assert.Greater(t, boosts["sym-a"], 1.0)
}

func TestComboManager_AIReapsFasterThanHuman(t *testing.T) {
	// Both modes record identical histories at t=0.
	ai := &comboManager{mode: ModeAI, now: func() int64 { return 0 }}
	hu := &comboManager{mode: ModeHuman, now: func() int64 { return 0 }}
	for i := 0; i < comboMinHits; i++ {
		ai.Record("q", "sym")
		hu.Record("q", "sym")
	}
	// Jump ahead 14 days: past AI's 7-day max age, within human's 30.
	ai.now = func() int64 { return int64(14 * 86400) }
	hu.now = func() int64 { return int64(14 * 86400) }
	assert.Nil(t, ai.BoostMap("q"), "AI mode should have reaped stale combos")
	assert.NotNil(t, hu.BoostMap("q"), "human mode keeps combos for 30 days")
}

func TestComboManager_NilSafe(t *testing.T) {
	// Nil-safe paths used by servers running in --no-cache modes.
	var cm *comboManager
	cm.Record("x", "y")
	assert.Nil(t, cm.BoostMap("x"))
	assert.False(t, cm.HasData())
}

// TestComboManager_ReapPrunesEmptyShells confirms that once every match
// for a query/keyword expires, the emptied outer entry is removed (not
// left as a zero-match shell) and HasData reflects that.
func TestComboManager_ReapPrunesEmptyShells(t *testing.T) {
	now := int64(1_000_000_000)
	cm := &comboManager{mode: ModeAI, now: func() int64 { return now }}
	for i := 0; i < comboMinHits; i++ {
		cm.Record("auth token", "sym-a")
	}
	require.True(t, cm.HasData())
	require.Len(t, cm.store.Queries, 1)
	require.NotEmpty(t, cm.kwStore.Keywords)

	// Jump past the AI max-age window so every match is stale.
	now += comboMaxAgeAISec + 1
	// HasData reaps first, so it must both prune the shells and report false.
	assert.False(t, cm.HasData(), "a store holding only expired matches must report no data")
	assert.Empty(t, cm.store.Queries, "emptied query shells must be pruned, not retained")
	assert.Empty(t, cm.kwStore.Keywords, "emptied keyword shells must be pruned, not retained")
}
