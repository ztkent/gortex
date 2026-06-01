package clones

import (
	"encoding/binary"
	"math/bits"
)

// CMS is a Count-Min Sketch over uint64 keys: a probabilistic
// frequency estimator with one-sided error — Count(x) ≥ true(x), never
// less. We use it on shingle-hash frequencies across a function-body
// corpus to identify boilerplate (shingles present in many bodies)
// before MinHash signature computation, so signatures reflect
// discriminative content rather than shared-API-pattern noise that
// drives LSH bucket explosions at monorepo scale.
//
// Sizing the sketch:
//
//	ε   = e/w  (additive error coefficient over total N adds)
//	δ   = (1/2)^d  (probability the bound is exceeded)
//	bound: Pr[Count(x) > true(x) + εN] ≤ δ
//
// We default to width=65536, depth=4 (1 MB) which gives ε ≈ 0.00004,
// so on a k8s-scale corpus of ~7.5M total Add calls the error is
// bounded at ~300 with 94% confidence — well below the typical
// boilerplate threshold of ~1500 (1% of 150k function bodies). At
// vscode scale the corpus is smaller and the error bound stays
// proportionally tight.
//
// The sketch is fully deterministic: hash seeds derive from a fixed
// xorshift64* state, so two runs over the same corpus produce
// identical counts — signature output is reproducible across daemon
// restarts and across snapshot reuse.
type CMS struct {
	width, depth int
	mask         uint64
	counts       [][]uint32
	seeds        []uint64
}

// NewCMS constructs an empty CMS with the requested width × depth.
// width is rounded up to the next power of two so the modulo can be
// a bitmask. depth ≤ 0 defaults to 4. Memory cost: 4 × width × depth
// bytes.
func NewCMS(width, depth int) *CMS {
	if width <= 0 {
		width = 65536
	}
	if depth <= 0 {
		depth = 4
	}
	// Round width up to the next power of two.
	if width&(width-1) != 0 {
		shift := bits.Len(uint(width))
		width = 1 << shift
	}
	cms := &CMS{
		width:  width,
		depth:  depth,
		mask:   uint64(width - 1),
		counts: make([][]uint32, depth),
		seeds:  make([]uint64, depth),
	}
	// Deterministic seed family: same xorshift64* pattern minhash.go's
	// hashParams uses, with a CMS-specific constant so the two
	// sketches don't share derived seeds.
	state := uint64(0xCEFCC9F3D741F271)
	next := func() uint64 {
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		return state * 0x2545F4914F6CDD1D
	}
	for i := range cms.counts {
		cms.counts[i] = make([]uint32, width)
		cms.seeds[i] = next()
	}
	return cms
}

// Add increments the counters for x by one across every hash row.
// Saturating at uint32 max — at our scale (≤ 8M total adds) saturation
// is unreachable, but the check costs nothing in the common case.
func (c *CMS) Add(x uint64) {
	for i := 0; i < c.depth; i++ {
		idx := cmsHash(x, c.seeds[i]) & c.mask
		if c.counts[i][idx] < ^uint32(0) {
			c.counts[i][idx]++
		}
	}
}

// Decrement decreases the counters for x by one across every hash row,
// flooring each at 0: a counter already at 0 is left untouched. It is
// the inverse of Add for incremental maintenance — when a body leaves
// the corpus its shingle hashes are decremented so the boilerplate
// estimate tracks the live set instead of growing monotonically.
//
// Decrementing a key that was never added is a no-op (every row sits
// at 0 already, or sits at some other key's count that this row shares
// — flooring at 0 keeps those undamaged). Because hash collisions can
// leave a row's counter above this key's true frequency, Count stays an
// upper bound after Decrement just as it is after Add; decrement never
// makes Count drop below the true count.
func (c *CMS) Decrement(x uint64) {
	for i := 0; i < c.depth; i++ {
		idx := cmsHash(x, c.seeds[i]) & c.mask
		if c.counts[i][idx] > 0 {
			c.counts[i][idx]--
		}
	}
}

// Count returns the minimum across all hash rows — the canonical CMS
// frequency estimate. The result is an upper bound on the true count.
func (c *CMS) Count(x uint64) uint32 {
	minCount := ^uint32(0)
	for i := 0; i < c.depth; i++ {
		idx := cmsHash(x, c.seeds[i]) & c.mask
		if c.counts[i][idx] < minCount {
			minCount = c.counts[i][idx]
		}
	}
	return minCount
}

// cmsHash is an inline FNV-1a over the (key, seed) pair. Kept tight
// because every Add/Count touches it depth times.
func cmsHash(x, seed uint64) uint64 {
	const (
		offset = uint64(14695981039346656037)
		prime  = uint64(1099511628211)
	)
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[:8], x)
	binary.LittleEndian.PutUint64(buf[8:], seed)
	h := offset
	for _, b := range buf {
		h ^= uint64(b)
		h *= prime
	}
	return h
}
