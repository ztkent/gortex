package clones

import "sort"

// StratifiedIndex is the incrementally maintained counterpart of
// DetectPairsStratifiedWithStats. Where the batch path re-partitions
// every item into length classes and rebuilds a fresh per-class LSH
// index on each run, StratifiedIndex keeps one live Index per length
// class (one per entry in lengthBucketBounds) so a single edited body
// can be re-banked in O(its classes) — typically one or two Add/Remove
// calls — instead of rebuilding over the whole corpus.
//
// Stratification mirrors the batch path exactly: an item is banked into
// every class lengthClassesOf(TokenCount) returns, so an item in the
// overlap region of two adjacent classes lives in both. tokens records
// each id's TokenCount so Remove can recompute the same class set the
// item was added under without the caller re-supplying it.
//
// StratifiedIndex is NOT goroutine-safe by design: the maps and the
// per-class Index state are mutated without locking. The intended caller
// (the indexer's incremental clone-edge maintainer) serialises Add /
// Remove / QueryPairs under its own lock, the same way the batch Index
// is driven from a single goroutine.
type StratifiedIndex struct {
	// classes[i] is the live LSH index for length class i; len matches
	// lengthBucketBounds so a class index aligns with lengthClassesOf.
	classes []*Index
	// tokens maps an added id to the TokenCount it was banked under, so
	// Remove can recompute lengthClassesOf(tokens[id]) — the exact class
	// set the id occupies — and drop it from each of those class indexes.
	tokens map[string]int
}

// NewStratifiedIndex returns an empty StratifiedIndex with one live
// per-class Index for every entry in lengthBucketBounds.
func NewStratifiedIndex() *StratifiedIndex {
	classes := make([]*Index, len(lengthBucketBounds))
	for i := range classes {
		classes[i] = NewIndex()
	}
	return &StratifiedIndex{
		classes: classes,
		tokens:  make(map[string]int),
	}
}

// Add banks an item into every length class its TokenCount falls in
// (lengthClassesOf), recording the TokenCount so a later Remove can
// recover the same class set. Adding an id that is already present
// follows Index.Add's contract — callers should add each id once, and
// re-banking an edited body should Remove it first.
func (s *StratifiedIndex) Add(it Item) {
	for _, c := range lengthClassesOf(it.TokenCount) {
		s.classes[c].Add(it.ID, it.Sig)
	}
	s.tokens[it.ID] = it.TokenCount
}

// Remove undoes a prior Add: it drops the id from every length class it
// was banked under — recomputed from the recorded TokenCount via
// lengthClassesOf — and forgets the recorded count. An id that was
// never added is a no-op.
func (s *StratifiedIndex) Remove(id string) {
	tc, ok := s.tokens[id]
	if !ok {
		return
	}
	for _, c := range lengthClassesOf(tc) {
		s.classes[c].Remove(id)
	}
	delete(s.tokens, id)
}

// QueryPairs returns every clone pair touching it whose estimated
// Jaccard similarity is at or above threshold (DefaultThreshold when
// threshold ≤ 0), in canonical (A < B) form. It is the per-item query
// that the maintained index exposes in place of the batch
// DetectPairsStratifiedWithStats walk: unioning QueryPairs over every
// item reproduces the batch pair set exactly.
//
// For each class lengthClassesOf(it.TokenCount) places it in, the class
// index's QueryCandidates(it.ID) yields the candidate IDs sharing a band
// bucket; each candidate's stored signature is scored against it.Sig and
// kept when it clears threshold. A candidate that surfaces from more
// than one class (the overlap region) is deduplicated by canonical pair
// key, matching the batch merge.
//
// it does not need to already be in the index — its signature is read
// from the Item, so re-adding it before querying is fine but not
// required. Candidates are still drawn from the live class indexes, so
// for the union over all items to equal the batch set every item must
// have been Added first.
func (s *StratifiedIndex) QueryPairs(it Item, threshold float64) []Pair {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	seen := make(map[[2]string]struct{})
	var out []Pair
	for _, c := range lengthClassesOf(it.TokenCount) {
		idx := s.classes[c]
		for _, cand := range idx.QueryCandidates(it.ID) {
			if cand == it.ID {
				continue
			}
			candSig, ok := idx.sigs[cand]
			if !ok {
				continue
			}
			sim := EstimateJaccard(it.Sig, candSig)
			if sim < threshold {
				continue
			}
			a, b := it.ID, cand
			if a > b {
				a, b = b, a
			}
			key := [2]string{a, b}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, Pair{A: a, B: b, Similarity: sim})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}
