package clones

import (
	"hash/fnv"
	"runtime"
	"sort"
	"sync"
)

// maxSeenCandidates bounds the dedup map size inside EmitCandidatesTo.
// Once the map hits this many entries it is dropped and dedup is
// disabled for the remainder of the walk; surviving duplicates land
// the same candidate pair through the Jaccard filter twice, which is
// strictly cheap (64 uint32 compares) compared to the alternative of
// the map growing into the hundreds of MB on a k8s-scale graph.
// Pair semantic output is unaffected — emit-side deduplication is
// the caller's responsibility (DetectPairsWithStats hands every
// Jaccard survivor to its slice).
const maxSeenCandidates = 8_000_000

// Item pairs a graph node ID with its MinHash signature — the input
// unit for the LSH detection pass. TokenCount carries the normalised-
// token count of the body and is used by DetectPairsStratifiedWithStats
// to length-bucket items before LSH; zero is treated as "unknown" and
// places the item in every class (the legacy single-bucket path).
type Item struct {
	ID         string
	Sig        Signature
	TokenCount int
}

// Pair is a detected clone relationship between two symbols, carrying
// the estimated Jaccard similarity of their bodies. A is always the
// lexicographically smaller ID so a pair has one canonical form.
type Pair struct {
	A          string
	B          string
	Similarity float64
}

// Cluster is a connected component of the clone graph — a set of
// symbols that are all transitively near-duplicates of one another.
type Cluster struct {
	Members       []string
	Size          int
	AvgSimilarity float64
}

// Index is an LSH banding index over MinHash signatures. Signatures
// are split into Bands bands of Rows rows; two signatures land in the
// same bucket of some band iff those Rows slots are identical, which
// makes them a candidate pair worth an exact-similarity check.
type Index struct {
	bands [Bands]map[uint64][]string
	sigs  map[string]Signature

	// Skipped-bucket counters set by the most recent CandidatePairs
	// call. Reads are safe only when no concurrent CandidatePairs is
	// running on the same Index. Exposed via SkippedBuckets so callers
	// (DetectPairs) can log how much fan-out the cap dropped without
	// changing the exported CandidatePairs signature.
	lastSkippedBuckets     int
	lastSkippedBucketItems int
}

// SkippedBuckets returns (bucket_count, total_items_in_skipped_buckets)
// for the most recent CandidatePairs call. Both values are 0 when the
// cap never tripped — i.e. every bucket was small enough that the
// pairwise expansion was performed in full.
func (ix *Index) SkippedBuckets() (int, int) {
	return ix.lastSkippedBuckets, ix.lastSkippedBucketItems
}

// NewIndex returns an empty LSH index.
func NewIndex() *Index {
	ix := &Index{sigs: make(map[string]Signature)}
	for b := range ix.bands {
		ix.bands[b] = make(map[uint64][]string)
	}
	return ix
}

// Add inserts one signed item into the index. Adding the same ID twice
// keeps the last signature but double-banks the band buckets; callers
// should add each ID once.
func (ix *Index) Add(id string, sig Signature) {
	ix.sigs[id] = sig
	for b := range Bands {
		key := bandKey(b, sig)
		ix.bands[b][key] = append(ix.bands[b][key], id)
	}
}

// Remove deletes an item from the index, undoing a prior Add of the
// same ID. If the ID was never added (no signature recorded) the call
// is a no-op. For each band it recomputes the bucket key from the
// stored signature, drops the ID from that bucket's member slice, and
// removes the bucket entry entirely once it is empty so the band map
// does not accumulate dead keys. The signature is then forgotten.
//
// Add(id, sig) followed by Remove(id) returns the index to a state in
// which id sits in no band bucket and contributes no candidate — the
// invariant the incremental maintenance path relies on when a body is
// re-shingled or deleted.
func (ix *Index) Remove(id string) {
	sig, ok := ix.sigs[id]
	if !ok {
		return
	}
	for b := range Bands {
		key := bandKey(b, sig)
		ids := ix.bands[b][key]
		// Drop the first occurrence of id; Add banks each ID once per
		// band, so a single removal clears the membership.
		for i, v := range ids {
			if v == id {
				ids = append(ids[:i], ids[i+1:]...)
				break
			}
		}
		if len(ids) == 0 {
			delete(ix.bands[b], key)
		} else {
			ix.bands[b][key] = ids
		}
	}
	delete(ix.sigs, id)
}

// QueryCandidates returns the candidate set for a single item: every
// other ID that shares at least one band bucket with id, in canonical
// sorted order. It is the per-item analogue of EmitCandidatesTo — the
// pairs (id, c) for every c in the result are exactly the candidate
// pairs EmitCandidatesTo would emit that touch id.
//
// id itself is excluded, results are deduplicated across bands, and
// buckets larger than maxBucketSize are skipped using the identical cap
// EmitCandidatesTo applies — so a candidate dropped by the batch fan-out
// cap is also dropped here, keeping the maintained query and the batch
// walk in lock-step. An id with no recorded signature yields nil.
func (ix *Index) QueryCandidates(id string) []string {
	sig, ok := ix.sigs[id]
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	for b := range Bands {
		key := bandKey(b, sig)
		ids := ix.bands[b][key]
		if len(ids) < 2 {
			continue
		}
		if len(ids) > maxBucketSize {
			continue
		}
		for _, v := range ids {
			if v == id {
				continue
			}
			seen[v] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// bandKey hashes the Rows MinHash slots of band b into a bucket key.
// The band index is folded into the hash so identical row values in
// different bands cannot collide into the same logical bucket.
func bandKey(band int, sig Signature) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	put := func(v uint32) {
		buf[0] = byte(v)
		buf[1] = byte(v >> 8)
		buf[2] = byte(v >> 16)
		buf[3] = byte(v >> 24)
		h.Write(buf[:4])
	}
	put(uint32(band) + 1)
	for r := range Rows {
		put(sig[band*Rows+r])
	}
	return h.Sum64()
}

// maxBucketSize is the per-band-bucket fan-out cap. Buckets larger
// than this are skipped entirely: a bucket carrying N items emits
// N·(N-1)/2 candidate pairs, which for N=2000 is 2M pairs per band ×
// Bands = ~64M pairs from one bucket alone. At repo-set scale the
// large buckets are dominated by template / boilerplate signatures
// (empty getters, single-line lambdas, react component shells) whose
// pairwise comparisons are noise rather than signal — every "real"
// clone of practical interest still has ≥1 small-bucket band hit
// because MinHash banding is the AND of multiple bands.
//
// Without this cap, the warmup phase that runs detectClonesAndEmitEdges
// against a 200k-function multi-repo graph stalled for 20+ minutes
// inside CandidatePairs (observed in production profile #4 stall).
const maxBucketSize = 256

// CandidatePairs returns every unordered pair of IDs that collide in at
// least one band bucket. Each pair is returned once, in canonical
// (A < B) form. This is the candidate set the exact Jaccard filter
// runs over — it is a superset of the true clone pairs.
//
// Materialises the candidate set in memory. For huge graphs prefer
// EmitCandidatesTo, which streams the pairs through a callback without
// holding the full slice.
func (ix *Index) CandidatePairs() []Pair {
	var pairs []Pair
	skippedBuckets, skippedBucketItems := ix.EmitCandidatesTo(func(p Pair) bool {
		pairs = append(pairs, p)
		return true
	})
	ix.lastSkippedBuckets = skippedBuckets
	ix.lastSkippedBucketItems = skippedBucketItems
	return pairs
}

// EmitCandidatesTo walks the band buckets and calls emit(p) for each
// candidate pair in canonical (A < B) form. emit returns true to keep
// walking, false to stop early. Returns the per-bucket-cap telemetry
// for the walk that just completed.
//
// Memory: a single uint64-keyed dedup map (FNV-1a hash of the
// canonicalised pair). On a fan-out beyond maxSeenCandidates the map
// is released and dedup is disabled for the rest of the walk — the
// caller's Jaccard filter then runs twice on any duplicate, which is
// strictly cheap relative to holding hundreds of MB of map state. The
// previous slice-based CandidatePairs grew a parallel []Pair plus the
// dedup map; on k8s with 150k items both reached multi-GB and OOMed
// the daemon mid-detection. Streaming the candidate set caps in-flight
// memory at the dedup map alone (~120 MB at 8M entries) regardless of
// the global candidate count.
func (ix *Index) EmitCandidatesTo(emit func(Pair) bool) (skippedBuckets, skippedBucketItems int) {
	seen := make(map[uint64]struct{})
	for b := range Bands {
		for _, ids := range ix.bands[b] {
			if len(ids) < 2 {
				continue
			}
			if len(ids) > maxBucketSize {
				skippedBuckets++
				skippedBucketItems += len(ids)
				continue
			}
			for i := range ids {
				for j := i + 1; j < len(ids); j++ {
					a, c := ids[i], ids[j]
					if a == c {
						continue
					}
					if a > c {
						a, c = c, a
					}
					if seen != nil {
						key := pairKey(a, c)
						if _, ok := seen[key]; ok {
							continue
						}
						seen[key] = struct{}{}
						if len(seen) >= maxSeenCandidates {
							// Release the backing storage for GC and
							// stop deduplicating. Subsequent duplicates
							// pass through the Jaccard filter twice — a
							// known, bounded redundancy.
							seen = nil
						}
					}
					if !emit(Pair{A: a, B: c}) {
						ix.lastSkippedBuckets = skippedBuckets
						ix.lastSkippedBucketItems = skippedBucketItems
						return
					}
				}
			}
		}
	}
	ix.lastSkippedBuckets = skippedBuckets
	ix.lastSkippedBucketItems = skippedBucketItems
	return
}

// pairKey produces a 64-bit hash of a canonicalised (a < c) ID pair,
// using FNV-1a in two passes seeded by the offset basis. Inline and
// allocation-free so the hot-loop dedup stays fast.
func pairKey(a, c string) uint64 {
	const (
		offset uint64 = 14695981039346656037
		prime  uint64 = 1099511628211
	)
	h := offset
	for i := 0; i < len(a); i++ {
		h ^= uint64(a[i])
		h *= prime
	}
	// Sentinel byte between the two halves so the hashes of ("ab","c")
	// and ("a","bc") diverge.
	h ^= 0x1f
	h *= prime
	for i := 0; i < len(c); i++ {
		h ^= uint64(c[i])
		h *= prime
	}
	return h
}

// DetectPairs runs the full LSH detection pass: it bands every item,
// gathers candidate pairs, then keeps only those whose exact estimated
// Jaccard similarity is at or above threshold. Results are sorted by
// descending similarity, then by ID, so output is deterministic.
//
// A threshold ≤ 0 falls back to DefaultThreshold.
func DetectPairs(items []Item, threshold float64) []Pair {
	pairs, _, _ := DetectPairsWithStats(items, threshold)
	return pairs
}

// DetectPairsWithStats is DetectPairs plus the per-bucket-cap
// telemetry from the underlying Index.EmitCandidatesTo walk. Callers
// that want to know how much fan-out the cap dropped (warmup-time
// orchestrator logging) use this; everything else can stay on
// DetectPairs and ignore the counters.
//
// Streaming-collect wrapper: the Jaccard filter runs in a parallel
// worker pool over the candidate stream (no in-memory candidate
// slice), and surviving pairs are appended to the output. The
// candidate explosion that used to dominate cold-index memory on
// huge graphs is replaced by a bounded-channel pipeline.
func DetectPairsWithStats(items []Item, threshold float64) (pairs []Pair, skippedBuckets, skippedBucketItems int) {
	var mu sync.Mutex
	var out []Pair
	skippedBuckets, skippedBucketItems = DetectPairsStreamingWithStats(items, threshold, func(p Pair) {
		mu.Lock()
		out = append(out, p)
		mu.Unlock()
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out, skippedBuckets, skippedBucketItems
}

// DetectPairsStreamingWithStats is the streaming form of
// DetectPairsWithStats: each surviving clone pair is handed to emit
// in completion order rather than collected into a slice. Use this
// when the caller can act on each pair as it arrives (write an edge
// to the graph, log a result) so the surviving-pair set never has to
// be materialised globally.
//
// emit is called from a parallel worker pool. The function serialises
// the emit callback via an internal mutex so callers don't need their
// own — emit can do non-thread-safe work directly. The caller can
// expect at most NumCPU concurrent in-flight Jaccard estimates and
// one in-flight emit at a time.
//
// Returns the per-bucket-cap telemetry from the underlying LSH walk.
func DetectPairsStreamingWithStats(items []Item, threshold float64, emit func(Pair)) (skippedBuckets, skippedBucketItems int) {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	if len(items) < 2 {
		return 0, 0
	}

	ix := NewIndex()
	for _, it := range items {
		ix.Add(it.ID, it.Sig)
	}

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	// Buffer at workers*16 so the producer (single goroutine walking
	// band buckets) can stay ahead of the slowest consumer without
	// stalling on per-pair channel back-pressure. Bigger buffers don't
	// help — the producer's rate is bounded by map iteration, not by
	// allocation.
	candCh := make(chan Pair, workers*16)
	var wg sync.WaitGroup
	var emitMu sync.Mutex
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range candCh {
				sa, oka := ix.sigs[p.A]
				sb, okb := ix.sigs[p.B]
				if !oka || !okb {
					continue
				}
				sim := EstimateJaccard(sa, sb)
				if sim < threshold {
					continue
				}
				p.Similarity = sim
				emitMu.Lock()
				emit(p)
				emitMu.Unlock()
			}
		}()
	}

	skippedBuckets, skippedBucketItems = ix.EmitCandidatesTo(func(p Pair) bool {
		candCh <- p
		return true
	})
	close(candCh)
	wg.Wait()
	return skippedBuckets, skippedBucketItems
}

// ClusterPairs groups detected pairs into connected components via
// union-find. Each returned cluster lists its members sorted, its size,
// and the average similarity over the detected pairs that fall inside
// it. Clusters are sorted by descending size, then by first member.
func ClusterPairs(pairs []Pair) []Cluster {
	parent := make(map[string]string)
	find := func(x string) string {
		root := x
		for parent[root] != root {
			root = parent[root]
		}
		// path compression
		for parent[x] != root {
			parent[x], x = root, parent[x]
		}
		return root
	}
	add := func(x string) {
		if _, ok := parent[x]; !ok {
			parent[x] = x
		}
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for _, p := range pairs {
		add(p.A)
		add(p.B)
		union(p.A, p.B)
	}

	members := make(map[string][]string)
	for id := range parent {
		root := find(id)
		members[root] = append(members[root], id)
	}
	simSum := make(map[string]float64)
	simCnt := make(map[string]int)
	for _, p := range pairs {
		root := find(p.A)
		simSum[root] += p.Similarity
		simCnt[root]++
	}

	clusters := make([]Cluster, 0, len(members))
	for root, ids := range members {
		sort.Strings(ids)
		avg := 0.0
		if simCnt[root] > 0 {
			avg = simSum[root] / float64(simCnt[root])
		}
		clusters = append(clusters, Cluster{
			Members:       ids,
			Size:          len(ids),
			AvgSimilarity: avg,
		})
	}
	sort.Slice(clusters, func(i, j int) bool {
		if clusters[i].Size != clusters[j].Size {
			return clusters[i].Size > clusters[j].Size
		}
		return clusters[i].Members[0] < clusters[j].Members[0]
	})
	return clusters
}
