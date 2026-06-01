package clones

import (
	"reflect"
	"sort"
	"testing"
)

// shinglesFrom builds a deterministic shingle-hash set from a slice of
// integer shingle ids. Using small distinct integers as the raw shingle
// hashes lets a test author dial in an exact Jaccard overlap between two
// items: |A ∩ B| / |A ∪ B| over the integer sets is what MinHash
// estimates, so near-duplicates and distinct items are constructed by
// choosing how many shingle ids two sets share.
func shinglesFrom(ids ...uint64) []uint64 {
	out := make([]uint64, len(ids))
	copy(out, ids)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sigFromShingles is a test helper: SignatureFromShingles with no
// minimum-shingle floor, failing the test if the set is degenerate.
func sigFromShingles(t *testing.T, shingles []uint64) Signature {
	t.Helper()
	sig, ok := SignatureFromShingles(shingles, 0)
	if !ok {
		t.Fatalf("SignatureFromShingles failed for %v", shingles)
	}
	return sig
}

// makeShingleRange returns the shingle ids base, base+1, …, base+n-1 —
// a contiguous block, so two blocks overlap by a controllable amount.
func makeShingleRange(base, n uint64) []uint64 {
	out := make([]uint64, 0, n)
	for i := uint64(0); i < n; i++ {
		out = append(out, base+i)
	}
	return out
}

// fixtureItems builds the deterministic correctness fixture:
//   - a / b: a high-overlap near-duplicate pair in the small length class
//   - c: distinct from a/b, same small length class (a non-clone neighbour)
//   - d / e: a second high-overlap near-duplicate pair, sized so they sit
//     in a different (larger) length class than a/b — exercising >1 class
//   - f: distinct, in the large class (a non-clone neighbour for d/e)
//
// Overlaps are tuned so EstimateJaccard clears DefaultThreshold for the
// (a,b) and (d,e) pairs and stays well below it for everything else.
func fixtureItems(t *testing.T) []Item {
	t.Helper()

	// Small length class (TokenCount 60 → class 0 only, [0,80)).
	// a and b share 116 of 120 shingles → exact Jaccard ≈ 0.967.
	aSh := makeShingleRange(1000, 120)
	bSh := makeShingleRange(1004, 120) // shifted by 4 → 116 shared
	// c shares almost nothing with a/b.
	cSh := makeShingleRange(9000, 120)

	// Large length class (TokenCount 250 → class 3 only, [200,640)).
	// d and e share 116 of 120 shingles → exact Jaccard ≈ 0.967.
	dSh := makeShingleRange(2000, 120)
	eSh := makeShingleRange(2004, 120)
	// f shares almost nothing with d/e.
	fSh := makeShingleRange(7000, 120)

	return []Item{
		{ID: "a", Sig: sigFromShingles(t, shinglesFrom(aSh...)), TokenCount: 60},
		{ID: "b", Sig: sigFromShingles(t, shinglesFrom(bSh...)), TokenCount: 60},
		{ID: "c", Sig: sigFromShingles(t, shinglesFrom(cSh...)), TokenCount: 60},
		{ID: "d", Sig: sigFromShingles(t, shinglesFrom(dSh...)), TokenCount: 250},
		{ID: "e", Sig: sigFromShingles(t, shinglesFrom(eSh...)), TokenCount: 250},
		{ID: "f", Sig: sigFromShingles(t, shinglesFrom(fSh...)), TokenCount: 250},
	}
}

// canonicalPairSet reduces a slice of Pairs to the set of canonical
// (A<B) id pairs, ignoring similarity — the unit of comparison between
// the batch and the maintained detection paths.
func canonicalPairSet(pairs []Pair) map[[2]string]struct{} {
	set := make(map[[2]string]struct{}, len(pairs))
	for _, p := range pairs {
		a, b := p.A, p.B
		if a > b {
			a, b = b, a
		}
		set[[2]string{a, b}] = struct{}{}
	}
	return set
}

// populatedLengthClasses counts how many length classes hold ≥1 item
// from the fixture — used to assert the equivalence test is non-vacuous
// (more than one class actually exercised).
func populatedLengthClasses(items []Item) int {
	hit := make(map[int]struct{})
	for _, it := range items {
		for _, c := range lengthClassesOf(it.TokenCount) {
			hit[c] = struct{}{}
		}
	}
	return len(hit)
}

// TestStratifiedIndexEquivalence proves the incrementally maintained
// per-item query reproduces the batch detection exactly: the union of
// QueryPairs over every item equals the canonical pair set the batch
// DetectPairsStratifiedWithStats produces over the same corpus.
func TestStratifiedIndexEquivalence(t *testing.T) {
	items := fixtureItems(t)
	const threshold = DefaultThreshold

	batchPairs, _, _ := DetectPairsStratifiedWithStats(items, threshold)
	batchSet := canonicalPairSet(batchPairs)

	// Non-vacuous fixture: the batch must find at least one pair and the
	// items must span more than one length class, else the equivalence
	// is trivially satisfied by an empty set in a single bucket.
	if len(batchSet) < 1 {
		t.Fatalf("fixture vacuous: batch found no pairs")
	}
	if n := populatedLengthClasses(items); n <= 1 {
		t.Fatalf("fixture vacuous: only %d length class populated, want >1", n)
	}

	s := NewStratifiedIndex()
	for _, it := range items {
		s.Add(it)
	}

	maintained := make(map[[2]string]struct{})
	for _, it := range items {
		for _, p := range s.QueryPairs(it, threshold) {
			a, b := p.A, p.B
			if a > b {
				a, b = b, a
			}
			maintained[[2]string{a, b}] = struct{}{}
		}
	}

	if !reflect.DeepEqual(batchSet, maintained) {
		t.Fatalf("maintained query set != batch set\n  batch=%v\n  maintained=%v", batchSet, maintained)
	}
}

// TestStratifiedIndexRemoveAndReadd proves Remove pulls a
// clone-participating id out of every candidate set, and that re-Adding
// it restores the original equivalence set.
func TestStratifiedIndexRemoveAndReadd(t *testing.T) {
	items := fixtureItems(t)
	const threshold = DefaultThreshold

	batchPairs, _, _ := DetectPairsStratifiedWithStats(items, threshold)
	batchSet := canonicalPairSet(batchPairs)
	if len(batchSet) < 1 {
		t.Fatalf("fixture vacuous: batch found no pairs")
	}

	s := NewStratifiedIndex()
	for _, it := range items {
		s.Add(it)
	}

	// "a" participates in the (a,b) clone pair.
	const removed = "a"
	var removedItem Item
	for _, it := range items {
		if it.ID == removed {
			removedItem = it
		}
	}

	s.Remove(removed)

	// After removal no QueryPairs over the remaining items may yield a
	// pair touching the removed id.
	for _, it := range items {
		if it.ID == removed {
			continue
		}
		for _, p := range s.QueryPairs(it, threshold) {
			if p.A == removed || p.B == removed {
				t.Fatalf("pair %+v still references removed id %q", p, removed)
			}
		}
	}
	// The removed item must also produce no surviving pairs of its own,
	// since its former partner can no longer be a live candidate for it.
	if pairs := s.QueryPairs(removedItem, threshold); len(pairs) != 0 {
		t.Fatalf("removed item still produced pairs: %+v", pairs)
	}

	// Re-Add restores the full equivalence set.
	s.Add(removedItem)
	restored := make(map[[2]string]struct{})
	for _, it := range items {
		for _, p := range s.QueryPairs(it, threshold) {
			a, b := p.A, p.B
			if a > b {
				a, b = b, a
			}
			restored[[2]string{a, b}] = struct{}{}
		}
	}
	if !reflect.DeepEqual(batchSet, restored) {
		t.Fatalf("re-add did not restore equivalence set\n  batch=%v\n  restored=%v", batchSet, restored)
	}
}

// TestCMSDecrementRoundTrip proves Decrement floors at 0 and that Count
// reflects the live multiset remainder after a subset is decremented:
// it stays an upper bound on the surviving true count and returns to the
// 0 floor for keys decremented down to nothing.
func TestCMSDecrementRoundTrip(t *testing.T) {
	cms := NewCMS(4096, 4)

	// A multiset of keys with known multiplicities.
	multiset := map[uint64]int{
		11: 3,
		22: 5,
		33: 1,
		44: 2,
	}
	for key, n := range multiset {
		for i := 0; i < n; i++ {
			cms.Add(key)
		}
	}

	// Decrement a subset: drop 33 entirely (1→0), drop two of 22 (5→3).
	decrements := map[uint64]int{
		33: 1,
		22: 2,
	}
	remaining := make(map[uint64]int, len(multiset))
	for key, n := range multiset {
		remaining[key] = n - decrements[key]
	}
	for key, n := range decrements {
		for i := 0; i < n; i++ {
			cms.Decrement(key)
		}
	}

	// Count is an upper bound on the live true count, and exactly the
	// floor (0) for the fully-removed key.
	for key, want := range remaining {
		got := cms.Count(key)
		if got < uint32(want) {
			t.Fatalf("Count(%d)=%d below true remaining count %d (CMS must stay an upper bound)", key, got, want)
		}
		if want == 0 && got != 0 {
			t.Fatalf("Count(%d)=%d, want 0 after full removal", key, got)
		}
	}

	// Decrementing a never-added key is a no-op and never drives any
	// counter negative — Count stays at the 0 floor.
	const neverAdded = uint64(999)
	cms.Decrement(neverAdded)
	if got := cms.Count(neverAdded); got != 0 {
		t.Fatalf("Count(neverAdded)=%d after no-op Decrement, want 0", got)
	}
}
