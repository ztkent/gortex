package mcp

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/search/rerank"
)

// comboManager is the in-memory wrapper for ComboStore. It records
// (query → symbol) associations whenever an agent consumes a symbol that
// was returned by a recent search, and exposes a boost score used by the
// search-result reranker. The reranker applies a multiplier once a match
// has been seen at least comboMinHits times, matching FFF's min-3 gate.
type comboManager struct {
	mu    sync.Mutex
	store persistence.ComboStore
	dir   string
	mode  AgentMode

	// kwStore is the per-keyword association index -- a sibling of
	// `store` that keys on each surviving query token rather than the
	// whole normalized query. It is persisted to its own file
	// (kwDir) so the two schemas evolve independently. Guarded by the
	// same mu.
	kwStore persistence.KeywordStore
	kwDir   string

	// Overridable for tests. Seconds since the unix epoch, not a Time, so
	// comparisons stay fast and the struct stays gob-friendly.
	now func() int64
}

const (
	// Min hits before a (query, symbol) pair starts receiving a boost.
	// Below this we treat the association as noise.
	comboMinHits = 3
	// Boost per hit, capped at comboMaxBoost. BM25 scores in practice
	// land in the 1–5 range; a multiplier of ~1.3x per extra hit means a
	// well-established combo can dominate a cold result of similar BM25
	// score without overwhelming a much stronger BM25 hit.
	comboBoostPerHit = 0.3
	comboMaxBoost    = 3.0

	// Max age in seconds for a combo match before it's evicted on access.
	// AI mode: 7 days — agents churn through queries quickly, stale combos
	// become noise. Human mode: 30 days — sessions span weeks and a "I
	// always mean this file when I search for X" association is genuine.
	comboMaxAgeAISec    = int64(7 * 86400)
	comboMaxAgeHumanSec = int64(30 * 86400)

	// Per-keyword boost gate. A keyword is a coarser key than a whole
	// query, so the per-keyword path uses a lower min-hits threshold
	// and a smaller per-hit boost than the exact-query path -- a
	// keyword-only match should nudge, never dominate.
	keywordMinHits     = 2
	keywordBoostPerHit = 0.12
	// keywordMaxBoost caps the per-keyword multiplier well below
	// comboMaxBoost so an exact-query combo always out-boosts a
	// keyword-only one.
	keywordMaxBoost = 1.6
)

func newComboManager(cacheDir, repoPath string, mode AgentMode) *comboManager {
	nowFn := func() int64 { return time.Now().Unix() }
	if cacheDir == "" || repoPath == "" {
		return &comboManager{mode: mode, now: nowFn}
	}
	dir := persistence.ComboDir(cacheDir, repoPath)
	kwDir := persistence.KeywordDir(cacheDir, repoPath)
	cm := &comboManager{dir: dir, kwDir: kwDir, mode: mode, now: nowFn}
	if loaded, err := persistence.LoadCombo(dir); err == nil && loaded != nil {
		cm.store = *loaded
	}
	if loaded, err := persistence.LoadKeyword(kwDir); err == nil && loaded != nil {
		cm.kwStore = *loaded
	}
	return cm
}

func (cm *comboManager) maxAgeSec() int64 {
	if cm.mode == ModeHuman {
		return comboMaxAgeHumanSec
	}
	return comboMaxAgeAISec
}

// reapStaleLocked drops matches older than the mode's max age. Called
// lazily on Record and BoostMap so we never keep a background goroutine
// just for GC.
func (cm *comboManager) reapStaleLocked() {
	cutoff := cm.now() - cm.maxAgeSec()
	queries := cm.store.Queries[:0]
	for qi := range cm.store.Queries {
		q := &cm.store.Queries[qi]
		fresh := q.Matches[:0]
		for _, m := range q.Matches {
			if m.LastUsed >= cutoff {
				fresh = append(fresh, m)
			}
		}
		q.Matches = fresh
		// Drop a query once its last match expires. Without this the
		// emptied shell lingers forever: it bloats findQueryLocked's
		// linear scan and makes HasData report data that no longer exists.
		if len(fresh) > 0 {
			queries = append(queries, *q)
		}
	}
	cm.store.Queries = queries
	// The per-keyword index decays on the same clock as the
	// whole-query index -- a keyword association no agent has
	// reinforced inside the mode's window is stale noise.
	keywords := cm.kwStore.Keywords[:0]
	for ki := range cm.kwStore.Keywords {
		k := &cm.kwStore.Keywords[ki]
		fresh := k.Matches[:0]
		for _, m := range k.Matches {
			if m.LastUsed >= cutoff {
				fresh = append(fresh, m)
			}
		}
		k.Matches = fresh
		if len(fresh) > 0 {
			keywords = append(keywords, *k)
		}
	}
	cm.kwStore.Keywords = keywords
}

// normalizeQuery collapses whitespace and lowercases. Matches FFF's
// blake3(project + "::" + query) pattern at a coarser grain — we don't
// need exact byte fidelity, just "treat variant spacings as the same".
func normalizeQuery(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return ""
	}
	fields := strings.Fields(q)
	return strings.Join(fields, " ")
}

// Record tallies one (query → symbol) hit. If the query is unseen, a new
// entry is created; if the symbol already has an entry for the query, its
// HitCount bumps by one and LastUsed refreshes. Always flushed to disk.
func (cm *comboManager) Record(rawQuery, symbolID string) {
	if cm == nil {
		return
	}
	q := normalizeQuery(rawQuery)
	if q == "" || symbolID == "" {
		return
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.reapStaleLocked()
	cm.recordOneLocked(rawQuery, q, symbolID, cm.now())
	cm.persistLocked()
}

// RecordBatch tallies several (query → symbol) hits for the same query
// in one locked section with a single persist. Used by the tool-call
// observer: when the agent opens a file (get_editing_context /
// read_file) that contains symbols a recent search returned, every such
// symbol is credited to that search's query at once.
func (cm *comboManager) RecordBatch(rawQuery string, symbolIDs []string) {
	if cm == nil || len(symbolIDs) == 0 {
		return
	}
	q := normalizeQuery(rawQuery)
	if q == "" {
		return
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.reapStaleLocked()
	now := cm.now()
	recorded := false
	for _, id := range symbolIDs {
		if id == "" {
			continue
		}
		cm.recordOneLocked(rawQuery, q, id, now)
		recorded = true
	}
	if recorded {
		cm.persistLocked()
	}
}

// RecordNegative tallies an implicit negative — the agent was shown
// these symbols for a query carrying the given keywords but skipped over
// them to pick a lower-ranked result. Recorded only in the per-keyword
// index (the cluster grain), so KeywordBoostMap nets hits−misses and a
// persistently-skipped symbol loses its learned boost. The exact
// whole-query index stays positive-only.
func (cm *comboManager) RecordNegative(rawQuery string, skippedIDs []string) {
	if cm == nil || len(skippedIDs) == 0 {
		return
	}
	keywords := keywordTokens(rawQuery)
	if len(keywords) == 0 {
		return
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.reapStaleLocked()
	now := cm.now()
	recorded := false
	for _, id := range skippedIDs {
		if id == "" {
			continue
		}
		for _, kw := range keywords {
			cm.recordKeywordMissLocked(kw, id, now)
			recorded = true
		}
	}
	if recorded && cm.kwDir != "" {
		_ = persistence.SaveKeyword(cm.kwDir, &cm.kwStore)
	}
}

// recordOneLocked tallies one (query → symbol) hit in both the
// whole-query and per-keyword indexes. Caller holds cm.mu and is
// responsible for persistence.
func (cm *comboManager) recordOneLocked(rawQuery, q, symbolID string, now int64) {
	idx := cm.findQueryLocked(q)
	if idx < 0 {
		cm.store.Queries = append(cm.store.Queries, persistence.ComboQuery{
			Query:   q,
			Matches: []persistence.ComboMatch{{SymbolID: symbolID, HitCount: 1, LastUsed: now}},
		})
	} else {
		cq := &cm.store.Queries[idx]
		mIdx := -1
		for i := range cq.Matches {
			if cq.Matches[i].SymbolID == symbolID {
				mIdx = i
				break
			}
		}
		if mIdx < 0 {
			cq.Matches = append(cq.Matches, persistence.ComboMatch{
				SymbolID: symbolID, HitCount: 1, LastUsed: now,
			})
		} else {
			cq.Matches[mIdx].HitCount++
			cq.Matches[mIdx].LastUsed = now
		}
		// Keep matches ordered by hit count descending so hot symbols float
		// to the top — cheap because the list is tiny (capped below).
		sort.Slice(cq.Matches, func(i, j int) bool {
			return cq.Matches[i].HitCount > cq.Matches[j].HitCount
		})
		if cap := persistence.MaxComboEntries(); len(cq.Matches) > cap {
			cq.Matches = cq.Matches[:cap]
		}
		// Move the recently-touched query to the tail so SaveCombo's MRU
		// trim preserves it.
		cm.moveToEndLocked(idx)
	}

	// Mirror the hit into the per-keyword index: one (keyword ->
	// symbol) association per surviving query token. A later task
	// with overlapping keywords -- but different phrasing -- inherits
	// these even though its whole-query key never matches.
	cm.recordKeywordsLocked(rawQuery, symbolID, now)
}

// persistLocked flushes both stores. Caller holds cm.mu.
func (cm *comboManager) persistLocked() {
	if cm.dir != "" {
		_ = persistence.SaveCombo(cm.dir, &cm.store)
	}
	if cm.kwDir != "" {
		_ = persistence.SaveKeyword(cm.kwDir, &cm.kwStore)
	}
}

// BoostMap returns a per-symbol multiplier derived from combo history for
// the given query. Returns nil when the query is empty or has no matches
// above the minimum-hits threshold; callers treat nil as "no reweight".
func (cm *comboManager) BoostMap(rawQuery string) map[string]float64 {
	if cm == nil {
		return nil
	}
	q := normalizeQuery(rawQuery)
	if q == "" {
		return nil
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.reapStaleLocked()
	idx := cm.findQueryLocked(q)
	if idx < 0 {
		return nil
	}
	out := make(map[string]float64, len(cm.store.Queries[idx].Matches))
	for _, m := range cm.store.Queries[idx].Matches {
		if int(m.HitCount) < comboMinHits {
			continue
		}
		extra := float64(int(m.HitCount)-comboMinHits+1) * comboBoostPerHit
		boost := 1.0 + extra
		if boost > comboMaxBoost {
			boost = comboMaxBoost
		}
		out[m.SymbolID] = boost
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// findQueryLocked returns the index of q in store.Queries, or -1.
// Caller must hold cm.mu.
func (cm *comboManager) findQueryLocked(q string) int {
	for i := range cm.store.Queries {
		if cm.store.Queries[i].Query == q {
			return i
		}
	}
	return -1
}

// moveToEndLocked rotates the slice so element at idx lives at the end,
// preserving relative order of the rest. Used to make the MRU trim cheap.
// Caller must hold cm.mu.
func (cm *comboManager) moveToEndLocked(idx int) {
	if idx < 0 || idx >= len(cm.store.Queries)-1 {
		return
	}
	q := cm.store.Queries[idx]
	copy(cm.store.Queries[idx:], cm.store.Queries[idx+1:])
	cm.store.Queries[len(cm.store.Queries)-1] = q
}

// moveKeywordToEndLocked rotates kwStore.Keywords so the entry at idx
// lives at the tail, mirroring moveToEndLocked for the whole-query store.
// SaveKeyword front-trims on overflow, so without this an early-inserted
// but frequently-reinforced keyword would be evicted before a cold
// late-inserted one. Caller must hold cm.mu.
func (cm *comboManager) moveKeywordToEndLocked(idx int) {
	if idx < 0 || idx >= len(cm.kwStore.Keywords)-1 {
		return
	}
	k := cm.kwStore.Keywords[idx]
	copy(cm.kwStore.Keywords[idx:], cm.kwStore.Keywords[idx+1:])
	cm.kwStore.Keywords[len(cm.kwStore.Keywords)-1] = k
}

// HasData reports whether any queries have been recorded. Used by the
// feedback tool to decide whether to surface combo stats.
func (cm *comboManager) HasData() bool {
	if cm == nil {
		return false
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	// Reap first so a store holding only expired (soon-to-be-pruned)
	// entries doesn't report itself as having data.
	cm.reapStaleLocked()
	return len(cm.store.Queries) > 0
}

// keywordGenericStopWords are generic software nouns that match
// thousands of unrelated symbols -- mirroring the LLM-expansion
// stoplist. Recording a (keyword -> symbol) hit for one of these
// would pollute the index: the keyword would "associate" with
// whatever symbol the agent happened to pick. Combined with
// assistStopWords (English function words) this is the filter the
// per-keyword recorder applies to every query token.
var keywordGenericStopWords = map[string]struct{}{
	"function": {}, "functions": {}, "method": {}, "methods": {},
	"library": {}, "module": {}, "modules": {}, "package": {}, "packages": {},
	"system": {}, "service": {}, "services": {}, "code": {}, "source": {},
	"data": {}, "value": {}, "values": {}, "object": {}, "objects": {},
	"item": {}, "items": {}, "info": {}, "information": {}, "content": {},
	"thing": {}, "things": {}, "stuff": {}, "general": {}, "common": {},
	"basic": {}, "simple": {}, "main": {}, "text": {}, "logic": {},
	"process": {}, "handle": {}, "handler": {}, "handling": {}, "flow": {},
	"action": {}, "helper": {}, "helpers": {}, "util": {}, "utils": {},
	"utility": {}, "type": {}, "kind": {}, "name": {}, "list": {},
}

// keywordTokens extracts the meaningful query keywords from a raw
// query: the camelCase-aware tokens, lowercased and deduplicated,
// minus English function words (assistStopWords) and generic
// software nouns (keywordGenericStopWords), and minus sub-3-char
// fragments that carry no association signal.
func keywordTokens(rawQuery string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, tok := range rerank.Tokenize(rawQuery) {
		t := strings.ToLower(strings.TrimSpace(tok))
		if len(t) < 3 {
			continue
		}
		if _, stop := assistStopWords[t]; stop {
			continue
		}
		if _, stop := keywordGenericStopWords[t]; stop {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// recordKeywordsLocked records a (keyword -> symbol) hit for every
// surviving keyword of rawQuery. Caller holds cm.mu. The per-keyword
// match list is kept hit-ordered and capped tighter than the
// whole-query list.
func (cm *comboManager) recordKeywordsLocked(rawQuery, symbolID string, now int64) {
	for _, kw := range keywordTokens(rawQuery) {
		idx := cm.findKeywordLocked(kw)
		if idx < 0 {
			cm.kwStore.Keywords = append(cm.kwStore.Keywords, persistence.KeywordAssoc{
				Keyword: kw,
				Matches: []persistence.KeywordMatch{{SymbolID: symbolID, HitCount: 1, LastUsed: now}},
			})
			continue
		}
		ka := &cm.kwStore.Keywords[idx]
		mIdx := -1
		for i := range ka.Matches {
			if ka.Matches[i].SymbolID == symbolID {
				mIdx = i
				break
			}
		}
		if mIdx < 0 {
			ka.Matches = append(ka.Matches, persistence.KeywordMatch{
				SymbolID: symbolID, HitCount: 1, LastUsed: now,
			})
		} else {
			ka.Matches[mIdx].HitCount++
			ka.Matches[mIdx].LastUsed = now
		}
		sort.Slice(ka.Matches, func(i, j int) bool {
			return ka.Matches[i].HitCount > ka.Matches[j].HitCount
		})
		if cap := persistence.MaxKeywordEntries(); len(ka.Matches) > cap {
			ka.Matches = ka.Matches[:cap]
		}
		// An existing keyword was re-touched: rotate it to the tail so
		// SaveKeyword's front-trim evicts least-recently-used keywords.
		cm.moveKeywordToEndLocked(idx)
	}
}

// recordKeywordMissLocked bumps the implicit-negative MissCount for a
// (keyword -> symbol) association, creating a miss-only entry when the
// symbol has no prior association for the keyword. Caller holds cm.mu.
func (cm *comboManager) recordKeywordMissLocked(kw, symbolID string, now int64) {
	idx := cm.findKeywordLocked(kw)
	if idx < 0 {
		cm.kwStore.Keywords = append(cm.kwStore.Keywords, persistence.KeywordAssoc{
			Keyword: kw,
			Matches: []persistence.KeywordMatch{{SymbolID: symbolID, MissCount: 1, LastUsed: now}},
		})
		return
	}
	ka := &cm.kwStore.Keywords[idx]
	for i := range ka.Matches {
		if ka.Matches[i].SymbolID == symbolID {
			ka.Matches[i].MissCount++
			ka.Matches[i].LastUsed = now
			cm.moveKeywordToEndLocked(idx)
			return
		}
	}
	ka.Matches = append(ka.Matches, persistence.KeywordMatch{SymbolID: symbolID, MissCount: 1, LastUsed: now})
	// Keep the list bounded; order by hit count so a miss-only entry is
	// the first to be dropped under pressure (it carries no boost).
	if cap := persistence.MaxKeywordEntries(); len(ka.Matches) > cap {
		sort.Slice(ka.Matches, func(i, j int) bool {
			return ka.Matches[i].HitCount > ka.Matches[j].HitCount
		})
		ka.Matches = ka.Matches[:cap]
	}
	cm.moveKeywordToEndLocked(idx)
}

// findKeywordLocked returns the index of kw in kwStore.Keywords, or
// -1. Caller holds cm.mu.
func (cm *comboManager) findKeywordLocked(kw string) int {
	for i := range cm.kwStore.Keywords {
		if cm.kwStore.Keywords[i].Keyword == kw {
			return i
		}
	}
	return -1
}

// KeywordBoostMap returns a per-symbol multiplier derived from the
// per-keyword association index for rawQuery. It tokenizes the query,
// looks up each keyword's recorded symbols, and boosts a symbol by
// how many of the N query keywords it matched (K) scaled by K/N and
// the keyword hit count. A symbol that matches every query keyword
// gets the full keyword boost; one that matches a single keyword of
// five gets a fifth of it.
//
// Returns nil when the query has no usable keywords or nothing clears
// the keywordMinHits gate; callers treat nil as "no reweight". The
// boost is capped at keywordMaxBoost -- below comboMaxBoost -- so an
// exact whole-query combo always out-boosts a keyword-only match.
func (cm *comboManager) KeywordBoostMap(rawQuery string) map[string]float64 {
	if cm == nil {
		return nil
	}
	keywords := keywordTokens(rawQuery)
	if len(keywords) == 0 {
		return nil
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.reapStaleLocked()

	n := float64(len(keywords))
	// Per symbol: how many distinct query keywords it matched, and the
	// summed hit count across those keywords.
	type acc struct {
		matched int
		hits    int
	}
	per := map[string]*acc{}
	for _, kw := range keywords {
		idx := cm.findKeywordLocked(kw)
		if idx < 0 {
			continue
		}
		for _, m := range cm.kwStore.Keywords[idx].Matches {
			// Net the implicit-negative misses against the hits: a
			// symbol the agent keeps skipping over loses its boost.
			effective := int(m.HitCount) - int(m.MissCount)
			if effective < keywordMinHits {
				continue
			}
			a := per[m.SymbolID]
			if a == nil {
				a = &acc{}
				per[m.SymbolID] = a
			}
			a.matched++
			a.hits += effective
		}
	}
	if len(per) == 0 {
		return nil
	}
	out := make(map[string]float64, len(per))
	for sym, a := range per {
		coverage := float64(a.matched) / n
		// Each hit above the gate adds keywordBoostPerHit, scaled by the
		// fraction of query keywords the symbol covered. Use the AVERAGE
		// per-keyword strength, not the summed hits: a.hits already grows
		// with the number of matched keywords, so multiplying the sum by
		// coverage (which also grows with that count) would make the boost
		// super-linear in coverage instead of the documented ~K/N.
		avgHits := float64(a.hits) / float64(a.matched)
		extra := (avgHits - keywordMinHits + 1) * keywordBoostPerHit * coverage
		if extra < 0 {
			extra = 0
		}
		boost := 1.0 + extra
		if boost > keywordMaxBoost {
			boost = keywordMaxBoost
		}
		if boost > 1.0 {
			out[sym] = boost
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
