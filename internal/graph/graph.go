package graph

import (
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// GraphStats holds summary counts of the graph contents.
type GraphStats struct {
	TotalNodes int            `json:"total_nodes"`
	TotalEdges int            `json:"total_edges"`
	ByKind     map[string]int `json:"by_kind"`
	ByLanguage map[string]int `json:"by_language"`
}

// numShards controls the write-fan-out of the graph. Each shard owns a
// disjoint slice of node IDs (by FNV hash) and its own RWMutex, so
// parallel indexers writing distinct nodes don't contend for a single
// lock. 16 is a good default: even split across typical 8- or 12-core
// machines, small enough that operations which must walk every shard
// (AllNodes, Stats, EvictRepo) stay cheap.
//
// Trade-off: the old graph used one lock per graph; now we have 16, and
// cross-shard operations (AddEdge when source and target are in
// different shards, plus exhaustive reads) lock multiple shards. To
// avoid deadlock we always acquire shards in ascending index order.
const numShards = 16

// edgeKey is the logical identity of an edge — two edges with the same
// key are considered the same edge and dedup to one entry in the
// adjacency lists. Line is part of the key because a caller can hold
// the same target reference at two different call sites and both are
// real call-graph edges (see `foo(); foo();` in the same function).
//
// Both From and To are in the key even though the adjacency lists are
// keyed by one endpoint each — the other endpoint varies within an
// inEdges[to] slice (many From → same To) and within an outEdges[from]
// slice the inverse varies. Without both, property-test graphs with
// several callers to the same callee and no FilePath/Line distinction
// would dedup down to one entry and lose cross-caller traversal.
type edgeKey struct {
	From     string
	To       string
	Kind     EdgeKind
	FilePath string
	Line     int
}

func keyOf(e *Edge) edgeKey {
	return edgeKey{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line}
}

// edgeHash is the stored form of an edge's logical identity. A full
// edgeKey carries four strings plus an int — 72 bytes as a map key or
// sidecar element. Collapsing it to a 128-bit hash shrinks the
// outEdgeIdx / inEdgeIdx maps and the outEdgeKeys / inEdgeKeys sidecars
// by ~4.5×, the single largest line item in cold-warmup resident
// memory. 128 bits keeps a collision between any two distinct edges in
// a graph of realistic size out of reach (~1e-25 at 10M edges), so the
// indexes stay unique-key maps — the dedup and swap-with-last logic is
// byte-for-byte the same as the edgeKey version, only the key type
// changes.
type edgeHash struct{ lo, hi uint64 }

const fnvOffset64 = 1469598103934665603
const fnvPrime64 = 1099511628211

// hashEdgeKey computes the 128-bit identity hash of an edge key. Each
// 64-bit half is an independent FNV-1a pass: distinct seeds plus a
// reversed field order decorrelate the halves so the pair behaves as a
// 128-bit value rather than two views of the same 64-bit hash.
func hashEdgeKey(k edgeKey) edgeHash {
	return edgeHash{
		lo: fnv1aEdge(fnvOffset64, k, false),
		hi: fnv1aEdge(0x9e3779b97f4a7c15, k, true),
	}
}

// fnv1aEdge folds every edgeKey field into one FNV-1a 64-bit hash. A
// 0xff separator after each string keeps "ab"+"c" distinct from
// "a"+"bc"; reversed==true walks the string fields back-to-front.
func fnv1aEdge(seed uint64, k edgeKey, reversed bool) uint64 {
	h := seed
	mix := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= fnvPrime64
		}
		h ^= 0xff
		h *= fnvPrime64
	}
	if reversed {
		mix(k.FilePath)
		mix(string(k.Kind))
		mix(k.To)
		mix(k.From)
	} else {
		mix(k.From)
		mix(k.To)
		mix(string(k.Kind))
		mix(k.FilePath)
	}
	h ^= uint64(k.Line)
	h *= fnvPrime64
	return h
}

// hashEdgeIdentity computes the provenance-bearing identity hash of an
// edge: the logical key folded together with Origin. It reuses the
// edgeKey FNV machinery — each 64-bit half is hashEdgeKey's
// corresponding half with the Origin string mixed in as a sixth field
// (same 0xff-separated FNV-1a fold, same reversed-order decorrelation).
//
// This is deliberately distinct from hashEdgeKey: the dedup index
// (outEdgeIdx / inEdgeIdx) stays keyed on the Origin-free logical key
// so an Origin upgrade replaces the slot in place rather than creating
// a parallel edge. The identity hash is the separate value Edge.
// IdentityHash exposes so a provenance change is observable as a
// distinct identity.
func hashEdgeIdentity(k edgeKey, origin string) edgeHash {
	return edgeHash{
		lo: mixOriginInto(fnv1aEdge(fnvOffset64, k, false), origin),
		hi: mixOriginInto(fnv1aEdge(0x9e3779b97f4a7c15, k, true), origin),
	}
}

// mixOriginInto folds the Origin string into an already-computed
// fnv1aEdge hash with the same 0xff-separated FNV-1a step the key
// fields use, so Origin is a first-class sixth field of the identity
// rather than a bolt-on.
func mixOriginInto(h uint64, origin string) uint64 {
	for i := 0; i < len(origin); i++ {
		h ^= uint64(origin[i])
		h *= fnvPrime64
	}
	h ^= 0xff
	h *= fnvPrime64
	return h
}

// shard is a fragment of the graph's data. Each shard holds the node
// metadata for the subset of IDs that hash to it, plus the outgoing
// edges whose source ID is in this shard and the incoming edges whose
// target ID is in this shard. Secondary indexes (byName, byFile, etc.)
// inside a shard only contain nodes owned by that shard; aggregate
// queries iterate every shard and concatenate.
//
// Each slice-valued secondary index has a paired "Idx" sidecar that
// maps an entry's identity to its position in the slice. Two invariants
// the sidecars enforce:
//
//  1. Idempotent writes. AddNode/AddEdge consult the sidecar; duplicate
//     inserts replace the pointer in place instead of appending.
//     Without this, restart paths that load a snapshot and then re-run
//     IndexCtx (which doesn't evict first) silently double every edge
//     and every secondary-index entry. Fixes bug B1 at the write layer,
//     regardless of which call site forgets to evict first.
//  2. O(1) removal via swap-with-last. Removing a node from a
//     byRepo slice of 50k entries with filterEdge-style scanning was
//     O(N); now it's O(1) — flip the last element into the removed
//     slot and shrink the slice by one. Iteration order changes after a
//     removal, but callers that require stable ordering (snapshot
//     encoding) sort before emitting anyway.
type shard struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	outEdges map[string][]*Edge
	inEdges  map[string][]*Edge
	byFile   map[string][]*Node
	byName   map[string][]*Node
	byQual   map[string]*Node
	byRepo   map[string][]*Node // repoPrefix → nodes owned by this shard

	// Sidecar position indexes — see comment on shard. Reads are
	// unchanged (callers still iterate the slices); only writes
	// consult these maps.
	byFileIdx  map[string]map[string]int   // filePath   → id   → position
	byNameIdx  map[string]map[string]int   // name       → id   → position
	byRepoIdx  map[string]map[string]int   // repoPrefix → id   → position
	outEdgeIdx map[string]map[edgeHash]int // fromID     → hash → position
	inEdgeIdx  map[string]map[edgeHash]int // toID       → hash → position

	// Reverse-index slices that remember each entry's insertion-time
	// edgeHash, parallel to outEdges / inEdges. Required because keyOf
	// is computed from live Edge fields (To, From, ...) — and the
	// resolver mutates Edge.To when retargeting an unresolved edge.
	// During swap-with-last in removeEdgeFromBucket, computing
	// keyOf(swapped) on a swapped element whose To was just mutated
	// produced a different key than the one it was originally indexed
	// under, leaving a stale sidecar position pointing past the slice
	// (panic: "index out of range [42] with length 41" in
	// addEdgeToBucket on the next insert that collided with the stale
	// key). Storing the original hash here makes the swap update
	// independent of live Edge state.
	outEdgeKeys map[string][]edgeHash // fromID → position → hash
	inEdgeKeys  map[string][]edgeHash // toID   → position → hash

	// Running per-repo memory totals maintained under the shard's
	// existing write lock. Reading them out of RepoMemoryEstimate is
	// O(shard count) instead of the O(repo nodes + total edges) walk
	// the function used to do — and AllRepoMemoryEstimates collapses
	// R repo-by-repo queries into one O(R · S) sum. Edges are charged
	// to the source node's RepoPrefix; the shard owning the source
	// edge bucket owns the counter, so accounting matches what the
	// old AllEdges walk attributed.
	repoNodeBytes map[string]uint64
	repoNodeCount map[string]int
	repoEdgeBytes map[string]uint64
	repoEdgeCount map[string]int
}

func newShard() *shard {
	return &shard{
		nodes:         make(map[string]*Node),
		outEdges:      make(map[string][]*Edge),
		inEdges:       make(map[string][]*Edge),
		byFile:        make(map[string][]*Node),
		byName:        make(map[string][]*Node),
		byQual:        make(map[string]*Node),
		byRepo:        make(map[string][]*Node),
		byFileIdx:     make(map[string]map[string]int),
		byNameIdx:     make(map[string]map[string]int),
		byRepoIdx:     make(map[string]map[string]int),
		outEdgeIdx:    make(map[string]map[edgeHash]int),
		inEdgeIdx:     make(map[string]map[edgeHash]int),
		outEdgeKeys:   make(map[string][]edgeHash),
		inEdgeKeys:    make(map[string][]edgeHash),
		repoNodeBytes: make(map[string]uint64),
		repoNodeCount: make(map[string]int),
		repoEdgeBytes: make(map[string]uint64),
		repoEdgeCount: make(map[string]int),
	}
}

// repoNodeAdd registers a node under its RepoPrefix bucket. Caller
// must hold s.mu.Lock. No-op for nodes without a prefix (single-repo
// mode and synthetic nodes that intentionally skip byRepo accounting).
func (s *shard) repoNodeAdd(n *Node) {
	if n == nil || n.RepoPrefix == "" {
		return
	}
	s.repoNodeBytes[n.RepoPrefix] += nodeBytes(n)
	s.repoNodeCount[n.RepoPrefix]++
}

// repoNodeRemove undoes repoNodeAdd. Clamps to zero on underflow so a
// stale counter cannot wrap a uint64.
func (s *shard) repoNodeRemove(n *Node) {
	if n == nil || n.RepoPrefix == "" {
		return
	}
	b := nodeBytes(n)
	if cur := s.repoNodeBytes[n.RepoPrefix]; cur >= b {
		s.repoNodeBytes[n.RepoPrefix] = cur - b
	} else {
		s.repoNodeBytes[n.RepoPrefix] = 0
	}
	if s.repoNodeCount[n.RepoPrefix] > 0 {
		s.repoNodeCount[n.RepoPrefix]--
	}
	if s.repoNodeBytes[n.RepoPrefix] == 0 && s.repoNodeCount[n.RepoPrefix] == 0 {
		delete(s.repoNodeBytes, n.RepoPrefix)
		delete(s.repoNodeCount, n.RepoPrefix)
	}
}

// repoEdgeAdd attributes an edge to its source repo. repoPrefix is the
// source node's RepoPrefix as resolved at the time the edge is
// inserted; storing it here means a later swap of Edge.To (ReindexEdge)
// doesn't have to walk the source-node lookup again.
func (s *shard) repoEdgeAdd(repoPrefix string, e *Edge) {
	if repoPrefix == "" || e == nil {
		return
	}
	s.repoEdgeBytes[repoPrefix] += edgeBytes(e)
	s.repoEdgeCount[repoPrefix]++
}

// repoEdgeRemove undoes repoEdgeAdd. Same clamp-to-zero discipline as
// repoNodeRemove.
func (s *shard) repoEdgeRemove(repoPrefix string, e *Edge) {
	if repoPrefix == "" || e == nil {
		return
	}
	b := edgeBytes(e)
	if cur := s.repoEdgeBytes[repoPrefix]; cur >= b {
		s.repoEdgeBytes[repoPrefix] = cur - b
	} else {
		s.repoEdgeBytes[repoPrefix] = 0
	}
	if s.repoEdgeCount[repoPrefix] > 0 {
		s.repoEdgeCount[repoPrefix]--
	}
	if s.repoEdgeBytes[repoPrefix] == 0 && s.repoEdgeCount[repoPrefix] == 0 {
		delete(s.repoEdgeBytes, repoPrefix)
		delete(s.repoEdgeCount, repoPrefix)
	}
}

// addNodeToBucket appends n to bucket[key] unless an entry with that id
// is already present, in which case the existing slot is overwritten
// with the new pointer. Returns the position of the entry.
func addNodeToBucket(bucket map[string][]*Node, idx map[string]map[string]int, key, id string, n *Node) {
	if inner, ok := idx[key]; ok {
		if pos, exists := inner[id]; exists {
			bucket[key][pos] = n
			return
		}
	}
	pos := len(bucket[key])
	bucket[key] = append(bucket[key], n)
	inner, ok := idx[key]
	if !ok {
		inner = make(map[string]int)
		idx[key] = inner
	}
	inner[id] = pos
}

// removeNodeFromBucket swap-removes the entry with id from bucket[key],
// updating the sidecar position of the swapped-in element. No-op when
// the entry is absent. Cleans up the bucket + sidecar when the last
// entry for key leaves.
func removeNodeFromBucket(bucket map[string][]*Node, idx map[string]map[string]int, key, id string) {
	inner, ok := idx[key]
	if !ok {
		return
	}
	pos, exists := inner[id]
	if !exists {
		return
	}
	slice := bucket[key]
	last := len(slice) - 1
	if pos != last {
		swapped := slice[last]
		slice[pos] = swapped
		inner[swapped.ID] = pos
	}
	slice = slice[:last]
	delete(inner, id)
	if len(inner) == 0 {
		delete(idx, key)
		delete(bucket, key)
	} else {
		bucket[key] = slice
	}
}

// addEdgeToBucket appends e to bucket[key] unless an entry with the
// same logical identity (edgeKey) is already there, in which case the
// existing slot is overwritten. Reports whether this was a new insert
// and, on an in-place replace, whether the replacement carried a
// different Origin than the edge it displaced.
//
// keys is the parallel slice that remembers each slot's insertion-time
// edgeKey so removeEdgeFromBucket can update sidecars without
// recomputing keyOf on a possibly-mutated swapped Edge.
//
// originChanged surfaces the resolver path where an edge is re-added
// (via AddEdge) with upgraded provenance rather than mutated in place:
// the logical key still matches, so the slot is replaced, but the
// edge's identity has changed. AddEdge funnels this into the
// graph-level identity-revision counter so that re-add path is as
// observable as an explicit SetEdgeProvenance call.
func addEdgeToBucket(bucket map[string][]*Edge, keys map[string][]edgeHash, idx map[string]map[edgeHash]int, key string, e *Edge) (inserted, originChanged bool) {
	h := hashEdgeKey(keyOf(e))
	if inner, ok := idx[key]; ok {
		if pos, exists := inner[h]; exists {
			old := bucket[key][pos]
			originChanged = old != nil && old != e && old.Origin != e.Origin
			bucket[key][pos] = e
			// keys[key][pos] already equals h — same logical identity.
			return false, originChanged
		}
	}
	pos := len(bucket[key])
	bucket[key] = append(bucket[key], e)
	keys[key] = append(keys[key], h)
	inner, ok := idx[key]
	if !ok {
		inner = make(map[edgeHash]int)
		idx[key] = inner
	}
	inner[h] = pos
	return true, false
}

// removeEdgeFromBucket removes the entry with key k from bucket[key]
// using swap-with-last, maintaining the sidecar. No-op when absent.
func removeEdgeFromBucket(bucket map[string][]*Edge, keys map[string][]edgeHash, idx map[string]map[edgeHash]int, key string, k edgeHash) bool {
	inner, ok := idx[key]
	if !ok {
		return false
	}
	pos, exists := inner[k]
	if !exists {
		return false
	}
	slice := bucket[key]
	keySlice := keys[key]
	last := len(slice) - 1
	if pos != last {
		slice[pos] = slice[last]
		// Use the swapped slot's STORED insertion-time hash, not
		// hashEdgeKey(keyOf(swapped)). The Edge's To may have been
		// mutated by the resolver between insertion and now, in which
		// case keyOf would yield a different hash than the sidecar
		// entry that actually points at this slot — leaking a stale
		// "last" position that later panics in addEdgeToBucket.
		swappedKey := keySlice[last]
		keySlice[pos] = swappedKey
		inner[swappedKey] = pos
	}
	slice = slice[:last]
	keySlice = keySlice[:last]
	delete(inner, k)
	if len(inner) == 0 {
		delete(idx, key)
		delete(bucket, key)
		delete(keys, key)
	} else {
		bucket[key] = slice
		keys[key] = keySlice
	}
	return true
}

// Graph is a thread-safe in-memory knowledge graph. Internally sharded
// by node-ID hash so writers touching disjoint IDs run in parallel.
//
// resolveMu is a graph-wide lock shared by every resolver.Resolver
// constructed against this Graph. Per-shard locks protect individual
// node/edge writes, but resolution phases (ResolveAll, ResolveFile)
// iterate every shard and mutate edge targets in place — two
// resolvers racing on the same edge produced the data race that
// MultiIndexer.indexMultiRepo triggered when its per-repo goroutines
// each created their own Resolver. Routing every resolver through
// this single mutex serialises those phases without blocking
// ordinary shard-scoped reads and writes.
type Graph struct {
	shards    [numShards]*shard
	resolveMu sync.Mutex
	// edgeIdentityRevisions counts how many times an in-graph edge's
	// provenance-bearing identity changed — i.e. its Origin was
	// upgraded or reverted while its logical (From,To,Kind,FilePath,
	// Line) key stayed fixed. Each such change is conceptually a
	// retirement of the old identity and creation of a new one. Both
	// sanctioned mutation paths feed this counter: SetEdgeProvenance
	// (an explicit in-place change) and addEdgeToBucket's in-place
	// replace branch (a re-add of the same logical edge carrying an
	// upgraded Origin). The count is the tamper-evidence surface:
	// provenance cannot churn without it moving.
	edgeIdentityRevisions atomic.Int64
	// edgeMutGen bumps whenever the AllEdges output would change —
	// new edge inserted, existing edge removed, or an edge's
	// canonical key changed via ReindexEdge. Origin-only updates
	// (counted by edgeIdentityRevisions) do not bump this because the
	// slice content is unaffected. AllEdges uses the counter as a
	// cache-validity tag so repeated post-resolve analysis walks
	// share one materialised slice instead of each rebuilding the
	// 4 M-edge snapshot.
	edgeMutGen atomic.Uint64

	allEdgesCacheMu  sync.Mutex
	allEdgesCache    []*Edge
	allEdgesCacheGen uint64
}

// New creates an empty graph.
func New() *Graph {
	g := &Graph{}
	for i := range g.shards {
		g.shards[i] = newShard()
	}
	return g
}

// ResolveMutex returns the graph-wide mutex resolvers use to
// serialise resolution phases against this graph. Exposed for the
// resolver package; every Resolver built from the same Graph shares
// the same lock.
func (g *Graph) ResolveMutex() *sync.Mutex {
	return &g.resolveMu
}

// ReindexEdges is the batched sibling of ReindexEdge. The in-memory
// store has no per-call commit overhead so the implementation is a
// straight loop; the value of the batch API lives in the disk
// backends, where it collapses N transaction commits into one.
func (g *Graph) ReindexEdges(batch []EdgeReindex) {
	for _, r := range batch {
		if r.Edge == nil {
			continue
		}
		g.ReindexEdge(r.Edge, r.OldTo)
	}
}

// EdgesByKind yields every edge whose Kind matches. In-memory
// implementation iterates the materialised AllEdges() slice and
// filters; the algorithmic cost is identical to a hand-written
// "for _, e := range g.AllEdges() { if e.Kind == kind }" loop, which
// is what most call sites used before the predicate API existed.
// Disk backends override this with an index-backed scan.
func (g *Graph) EdgesByKind(kind EdgeKind) iter.Seq[*Edge] {
	return func(yield func(*Edge) bool) {
		for _, e := range g.AllEdges() {
			if e == nil || e.Kind != kind {
				continue
			}
			if !yield(e) {
				return
			}
		}
	}
}

// EdgesByKinds is the in-memory reference implementation of
// EdgesByKindsScanner. Single pass over AllEdges with a small
// pre-built kind set — same algorithmic cost as the legacy `for _, e
// := range g.AllEdges() { if e.Kind == X || e.Kind == Y }` loop the
// edge-driven analyzers used before this capability existed. Disk
// backends override with a single `WHERE kind IN $kinds` query so the
// edge-driven analyzers stop firing one EdgesByKind per kind (or
// worse, scanning AllEdges and filtering Go-side).
//
// Empty kinds yields nothing — matches the disk contract.
func (g *Graph) EdgesByKinds(kinds []EdgeKind) iter.Seq[*Edge] {
	if len(kinds) == 0 {
		return func(yield func(*Edge) bool) {}
	}
	set := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		set[k] = struct{}{}
	}
	if len(set) == 0 {
		return func(yield func(*Edge) bool) {}
	}
	return func(yield func(*Edge) bool) {
		for _, e := range g.AllEdges() {
			if e == nil {
				continue
			}
			if _, ok := set[e.Kind]; !ok {
				continue
			}
			if !yield(e) {
				return
			}
		}
	}
}

// NodesByKind yields every node whose Kind matches. Same semantics
// and same in-memory cost story as EdgesByKind.
func (g *Graph) NodesByKind(kind NodeKind) iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		for _, n := range g.AllNodes() {
			if n == nil || n.Kind != kind {
				continue
			}
			if !yield(n) {
				return
			}
		}
	}
}

// GetNodesByIDs returns a map id→*Node for every input ID that
// exists in the store. The in-memory implementation loops the
// existing GetNode — algorithmic cost identical to a hand-written
// loop in the caller, no concurrency win here. The value of the
// batched API lives in the disk backends, where it collapses N
// per-id SQL/bolt queries into one.
func (g *Graph) GetNodesByIDs(ids []string) map[string]*Node {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]*Node, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := out[id]; ok {
			continue
		}
		if n := g.GetNode(id); n != nil {
			out[id] = n
		}
	}
	return out
}

// FindNodesByNames is the batched sibling of FindNodesByName.
func (g *Graph) FindNodesByNames(names []string) map[string][]*Node {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string][]*Node, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := out[name]; ok {
			continue
		}
		matches := g.FindNodesByName(name)
		if len(matches) > 0 {
			out[name] = matches
		}
	}
	return out
}

// EdgesWithUnresolvedTarget yields every edge whose To has the
// "unresolved::" prefix — the resolver's main pending-edge filter.
// In-memory iterates all edges and prefix-checks; disk backends back
// it with a range scan on a to-keyed index.
func (g *Graph) EdgesWithUnresolvedTarget() iter.Seq[*Edge] {
	return func(yield func(*Edge) bool) {
		for _, e := range g.AllEdges() {
			if e == nil {
				continue
			}
			if !strings.HasPrefix(e.To, "unresolved::") {
				continue
			}
			if !yield(e) {
				return
			}
		}
	}
}

// DeadCodeCandidates is the in-memory reference implementation of
// DeadCodeCandidator. Iterates the requested node kinds and filters
// out anything whose incoming-edge bucket contains an allowlist match
// — same algorithm the analysis.FindDeadCode loop runs, just exposed
// as a single capability the disk backends can short-circuit with
// one Cypher per kind. Pure map / slice walks here; the win lives
// in disk backends where the equivalent path materialises the full
// in-edge map over cgo.
func (g *Graph) DeadCodeCandidates(allowedNodeKinds []NodeKind, allowedInEdgeKinds map[NodeKind][]EdgeKind) []*Node {
	if len(allowedNodeKinds) == 0 {
		return nil
	}
	// Build a per-kind set so the inner loop can match against a map
	// instead of re-scanning the allowlist slice for every edge.
	allowedSet := make(map[NodeKind]map[EdgeKind]struct{}, len(allowedNodeKinds))
	for _, k := range allowedNodeKinds {
		set := make(map[EdgeKind]struct{}, len(allowedInEdgeKinds[k]))
		for _, ek := range allowedInEdgeKinds[k] {
			set[ek] = struct{}{}
		}
		allowedSet[k] = set
	}

	var out []*Node
	for _, k := range allowedNodeKinds {
		allowed, hasAllow := allowedSet[k]
		anyKindCounts := !hasAllow || len(allowed) == 0
		for n := range g.NodesByKind(k) {
			if n == nil {
				continue
			}
			incoming := g.GetInEdges(n.ID)
			dead := true
			for _, e := range incoming {
				if e == nil {
					continue
				}
				if anyKindCounts {
					dead = false
					break
				}
				if _, ok := allowed[e.Kind]; ok {
					dead = false
					break
				}
			}
			if dead {
				out = append(out, n)
			}
		}
	}
	return out
}

// IfaceImplementsRows is the in-memory reference implementation of
// IfaceImplementsScanner. Joins KindInterface nodes carrying
// Meta["methods"] with their EdgeImplements predecessors and returns
// one row per (typeID, ifaceID, ifaceMeta) tuple.
func (g *Graph) IfaceImplementsRows() []IfaceImplementsRow {
	// Index interfaces with methods by ID so the edge walk is O(edges)
	// rather than O(edges × interfaces).
	ifaceMeta := make(map[string]map[string]any)
	for n := range g.NodesByKind(KindInterface) {
		if n == nil || n.Meta == nil {
			continue
		}
		if _, ok := n.Meta["methods"]; !ok {
			continue
		}
		ifaceMeta[n.ID] = n.Meta
	}
	if len(ifaceMeta) == 0 {
		return nil
	}
	var out []IfaceImplementsRow
	for e := range g.EdgesByKind(EdgeImplements) {
		if e == nil {
			continue
		}
		meta, ok := ifaceMeta[e.To]
		if !ok {
			continue
		}
		out = append(out, IfaceImplementsRow{
			TypeID:    e.From,
			IfaceID:   e.To,
			IfaceMeta: meta,
		})
	}
	return out
}

// NodeDegreeCounts is the in-memory reference implementation of
// NodeDegreeAggregator. Walks the per-node in/out edge buckets the
// in-memory backend already maintains — same cost as the per-node
// loop GraphConnectivity ran before this capability landed, just
// folded into one method call so the analyzer can pick the disk
// backend's bulk implementation transparently. Missing ids are
// elided from the result (matching the disk contract).
func (g *Graph) NodeDegreeCounts(ids []string, usageKinds []EdgeKind) []NodeDegreeRow {
	if len(ids) == 0 {
		return nil
	}
	usage := make(map[EdgeKind]struct{}, len(usageKinds))
	for _, k := range usageKinds {
		usage[k] = struct{}{}
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]NodeDegreeRow, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		// Skip unknown ids — the disk backend's WHERE n.id IN $ids
		// clause naturally drops them; mirror that here so both
		// backends return the same row count.
		if g.GetNode(id) == nil {
			continue
		}
		in := g.GetInEdges(id)
		row := NodeDegreeRow{
			NodeID:   id,
			InCount:  len(in),
			OutCount: len(g.GetOutEdges(id)),
		}
		if len(usage) > 0 {
			for _, e := range in {
				if e == nil {
					continue
				}
				if _, ok := usage[e.Kind]; ok {
					row.UsageInCount++
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// FileImporters is the in-memory reference implementation of the
// FileImporters capability. Iterates EdgeImports via the byKind
// bucket — same cost as the legacy AllEdges()+filter loop in
// handleCheckReferences, but exposes the predicate as a single call
// the disk backends can short-circuit with one Cypher.
//
// Matches edges whose To node satisfies filePath == n.FilePath OR
// filePath == n.ID. The dual match keeps parity with the indexer's
// two import shapes: file-targeted imports point at the file node
// (n.ID == filePath), while symbol-targeted imports land on a symbol
// whose FilePath equals filePath.
func (g *Graph) FileImporters(filePath string) []FileImporterRow {
	if filePath == "" {
		return nil
	}
	var out []FileImporterRow
	for e := range g.EdgesByKind(EdgeImports) {
		if e == nil {
			continue
		}
		to := g.GetNode(e.To)
		if to == nil {
			continue
		}
		if to.FilePath != filePath && to.ID != filePath {
			continue
		}
		from := g.GetNode(e.From)
		if from == nil {
			continue
		}
		out = append(out, FileImporterRow{
			FromFile: from.FilePath,
			FromID:   from.ID,
			FromName: from.Name,
			FromKind: from.Kind,
		})
	}
	return out
}

// NodeFanCounts is the in-memory reference implementation of
// NodeFanAggregator. Two passes over the per-node in/out edge buckets
// the in-memory backend already maintains, filtered by the caller's
// kind sets. Disk backends override with one Cypher per direction
// to drop the AllEdges() materialisation FindHotspots / health_score
// were running every call.
func (g *Graph) NodeFanCounts(ids []string, fanInKinds []EdgeKind, fanOutKinds []EdgeKind) []NodeFanRow {
	if len(ids) == 0 {
		return nil
	}
	inSet := make(map[EdgeKind]struct{}, len(fanInKinds))
	for _, k := range fanInKinds {
		inSet[k] = struct{}{}
	}
	outSet := make(map[EdgeKind]struct{}, len(fanOutKinds))
	for _, k := range fanOutKinds {
		outSet[k] = struct{}{}
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]NodeFanRow, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if g.GetNode(id) == nil {
			continue
		}
		row := NodeFanRow{NodeID: id}
		if len(inSet) > 0 {
			for _, e := range g.GetInEdges(id) {
				if e == nil {
					continue
				}
				if _, ok := inSet[e.Kind]; ok {
					row.FanIn++
				}
			}
		}
		if len(outSet) > 0 {
			for _, e := range g.GetOutEdges(id) {
				if e == nil {
					continue
				}
				if _, ok := outSet[e.Kind]; ok {
					row.FanOut++
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// InEdgeCountsByKind is the in-memory reference implementation of
// the InEdgeCounter capability. Walks each requested EdgeKind via
// the byKind bucket and increments a per-To counter. Same algorithm
// the AllEdges-bucketing fallback in handleGetUntestedSymbols runs;
// the win lives in disk backends where AllEdges() materialises every
// edge over cgo just to bucket by target.
//
// Dedupes the kind set up front so a sloppy caller passing the same
// kind twice doesn't double-count — matches the Cypher backend's
// IN-list dedup.
func (g *Graph) InEdgeCountsByKind(kinds []EdgeKind) map[string]int {
	if len(kinds) == 0 {
		return nil
	}
	seen := make(map[EdgeKind]struct{}, len(kinds))
	out := make(map[string]int)
	for _, k := range kinds {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		for e := range g.EdgesByKind(k) {
			if e == nil {
				continue
			}
			out[e.To]++
		}
	}
	return out
}

// NodesInFilesByKind is the in-memory reference implementation of
// the NodesInFilesByKindFinder capability. Filters NodesByKind for
// each requested kind down to the file set. Same algorithm as the
// Go-side loop in find_declaration's buildDeclFileIndex; the win
// lives in disk backends where AllNodes() over cgo dwarfs the few
// hundred surviving rows.
func (g *Graph) NodesInFilesByKind(files []string, kinds []NodeKind) []*Node {
	if len(files) == 0 || len(kinds) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(files))
	for _, f := range files {
		if f == "" {
			continue
		}
		wanted[f] = struct{}{}
	}
	if len(wanted) == 0 {
		return nil
	}
	// Dedup the kinds so a sloppy caller doesn't double-scan.
	seenKind := make(map[NodeKind]struct{}, len(kinds))
	var out []*Node
	for _, k := range kinds {
		if _, ok := seenKind[k]; ok {
			continue
		}
		seenKind[k] = struct{}{}
		for n := range g.NodesByKind(k) {
			if n == nil {
				continue
			}
			if _, ok := wanted[n.FilePath]; !ok {
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

// NodesByKinds is the in-memory reference implementation of the
// NodesByKindsScanner capability. Loops the existing NodesByKind
// iterator per requested kind — algorithmic cost identical to the
// hand-written `for _, n := range AllNodes() if n.Kind == K` pattern
// the metadata analyzers used before. The win lives in the disk
// backends, where one IN-list Cypher replaces the AllNodes() pull.
//
// Dedupes the kind set up front so a sloppy caller passing the same
// kind twice doesn't double-yield — matches the Cypher backend's
// IN-list dedup. Empty kinds returns nil without touching the store.
func (g *Graph) NodesByKinds(kinds []NodeKind) []*Node {
	if len(kinds) == 0 {
		return nil
	}
	seen := make(map[NodeKind]struct{}, len(kinds))
	var out []*Node
	for _, k := range kinds {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		for n := range g.NodesByKind(k) {
			if n == nil {
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

// EdgeAdjacencyForKinds is the in-memory reference implementation of
// the EdgeAdjacencyForKinds capability. One AllEdges scan that yields
// (from, to) pairs whose Kind is in the supplied edge-kind set AND
// whose endpoints both have a Kind in the node-kind set — identical
// shape to the Cypher join the disk backends fold into a single
// query.
//
// Empty edgeKinds or empty nodeKinds yields nothing — matches the
// disk contract.
func (g *Graph) EdgeAdjacencyForKinds(edgeKinds []EdgeKind, nodeKinds []NodeKind) iter.Seq[[2]string] {
	if len(edgeKinds) == 0 || len(nodeKinds) == 0 {
		return func(yield func([2]string) bool) {}
	}
	eset := make(map[EdgeKind]struct{}, len(edgeKinds))
	for _, k := range edgeKinds {
		if k == "" {
			continue
		}
		eset[k] = struct{}{}
	}
	nset := make(map[NodeKind]struct{}, len(nodeKinds))
	for _, k := range nodeKinds {
		if k == "" {
			continue
		}
		nset[k] = struct{}{}
	}
	if len(eset) == 0 || len(nset) == 0 {
		return func(yield func([2]string) bool) {}
	}
	return func(yield func([2]string) bool) {
		for _, e := range g.AllEdges() {
			if e == nil {
				continue
			}
			if _, ok := eset[e.Kind]; !ok {
				continue
			}
			from := g.GetNode(e.From)
			to := g.GetNode(e.To)
			if from == nil || to == nil {
				continue
			}
			if _, ok := nset[from.Kind]; !ok {
				continue
			}
			if _, ok := nset[to.Kind]; !ok {
				continue
			}
			if !yield([2]string{e.From, e.To}) {
				return
			}
		}
	}
}

// CommunityCrossingsByKind is the in-memory reference implementation
// of the CommunityCrossingsByKind capability. AllEdges scan with the
// kind-set filter, then a Go-side community comparison per edge —
// the exact loop FindHotspots.countCrossings ran before this
// capability existed.
//
// Empty kinds or empty nodeToComm returns nil. Zero-count sources
// never surface (matches the disk contract — callers probe by
// existence).
func (g *Graph) CommunityCrossingsByKind(kinds []EdgeKind, nodeToComm map[string]string) map[string]int {
	if len(kinds) == 0 || len(nodeToComm) == 0 {
		return nil
	}
	set := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		set[k] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		if _, ok := set[e.Kind]; !ok {
			continue
		}
		from := nodeToComm[e.From]
		to := nodeToComm[e.To]
		if from == "" || to == "" || from == to {
			continue
		}
		out[e.From]++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NodeIDsByKinds is the in-memory reference implementation of the
// NodeIDsByKinds capability. Single AllNodes pass with a kind-set
// filter, deduped on input — same algorithm as NodesByKinds but
// returns only the ID column. The disk-backend win is the projection
// drop, not the algorithmic shape.
func (g *Graph) NodeIDsByKinds(kinds []NodeKind) []string {
	if len(kinds) == 0 {
		return nil
	}
	seen := make(map[NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		seen[k] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	var out []string
	for _, n := range g.AllNodes() {
		if n == nil {
			continue
		}
		if _, ok := seen[n.Kind]; !ok {
			continue
		}
		out = append(out, n.ID)
	}
	return out
}

// EdgeKindCounts is the in-memory reference implementation of the
// EdgeKindCounter capability. One AllEdges scan with a per-kind
// tally — the exact loop the get_surprising_connections Go fallback
// already runs today, just exposed as a single method call so the
// disk backends can short-circuit with a Cypher GROUP BY.
//
// Empty graph returns nil so callers can short-circuit a downstream
// "kindCounts != nil" gate.
func (g *Graph) EdgeKindCounts() map[EdgeKind]int {
	out := map[EdgeKind]int{}
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		out[e.Kind]++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CrossRepoEdgeCounts is the in-memory reference implementation of
// CrossRepoEdgeAggregator. Iterates the four cross_repo_* byKind
// buckets and groups by (kind, fromRepoPrefix, toRepoPrefix). Same
// algorithm as the architecture handler's AllEdges loop but exposes
// it as a single capability so disk backends can fold the join into
// one Cypher.
//
// Returns nil when the graph carries no cross-repo edges (single-
// repo mode) so the caller's empty-list rendering kicks in without
// allocating.
func (g *Graph) CrossRepoEdgeCounts() []CrossRepoEdgeRow {
	type key struct {
		kind     EdgeKind
		fromRepo string
		toRepo   string
	}
	counts := map[key]int{}
	for _, k := range []EdgeKind{
		EdgeCrossRepoCalls,
		EdgeCrossRepoImplements,
		EdgeCrossRepoExtends,
	} {
		for e := range g.EdgesByKind(k) {
			if e == nil {
				continue
			}
			from := g.GetNode(e.From)
			to := g.GetNode(e.To)
			if from == nil || to == nil {
				continue
			}
			counts[key{kind: e.Kind, fromRepo: from.RepoPrefix, toRepo: to.RepoPrefix}]++
		}
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]CrossRepoEdgeRow, 0, len(counts))
	for k, c := range counts {
		out = append(out, CrossRepoEdgeRow{
			Kind: k.kind, FromRepo: k.fromRepo, ToRepo: k.toRepo, Count: c,
		})
	}
	return out
}

// FileImportCounts is the in-memory reference implementation of
// FileImportAggregator. Iterates the EdgeImports byKind bucket and
// groups by the target file path — coalescing to To-node FilePath
// or, when the indexer pointed the import edge at the file node
// directly, the target ID. Same algorithm as the AllEdges loop in
// mostImportedFiles; the win lives in disk backends where AllEdges
// + per-edge GetNode round-trips over cgo dwarf the few hundred
// surviving rows.
//
// scope, when non-nil, bounds the result to edges whose target ID
// lies in the slice (session-workspace clamp). A nil scope counts
// every imports edge. An empty (non-nil) scope returns nil — never
// a whole-graph scan.
func (g *Graph) FileImportCounts(scope []string) []FileImportCountRow {
	if scope != nil && len(scope) == 0 {
		return nil
	}
	var allowed map[string]struct{}
	if scope != nil {
		allowed = make(map[string]struct{}, len(scope))
		for _, id := range scope {
			if id == "" {
				continue
			}
			allowed[id] = struct{}{}
		}
		if len(allowed) == 0 {
			return nil
		}
	}
	counts := map[string]int{}
	for e := range g.EdgesByKind(EdgeImports) {
		if e == nil {
			continue
		}
		target := g.GetNode(e.To)
		if target == nil {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[target.ID]; !ok {
				continue
			}
		}
		path := target.FilePath
		if path == "" {
			path = target.ID
		}
		if path == "" {
			continue
		}
		counts[path]++
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]FileImportCountRow, 0, len(counts))
	for p, c := range counts {
		out = append(out, FileImportCountRow{FilePath: p, Count: c})
	}
	return out
}

// SetEdgeProvenanceBatch is the batched sibling of SetEdgeProvenance.
// Same story as ReindexEdges: per-call in memory, one transaction in
// the disk backends. Returns the number of edges whose Origin
// actually changed (matches the sum of per-edge SetEdgeProvenance
// boolean returns).
func (g *Graph) SetEdgeProvenanceBatch(batch []EdgeProvenanceUpdate) int {
	changed := 0
	for _, u := range batch {
		if u.Edge == nil {
			continue
		}
		if g.SetEdgeProvenance(u.Edge, u.NewOrigin) {
			changed++
		}
	}
	return changed
}

// shardIdx picks the shard index for an ID using FNV-1a. Inlined to
// avoid the per-call hash-object allocation that the stdlib's
// fnv.New32a() incurs — shardIdx is on the hottest path in the graph
// (every AddNode / AddEdge / GetNode call), and the heap profile shows
// 690 MB/30 s of fnv state allocations during cold-start indexing.
func shardIdx(id string) int {
	var h uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		h ^= uint32(id[i])
		h *= 16777619
	}
	return int(h % numShards)
}

// shardFor returns the shard that owns the given ID.
func (g *Graph) shardFor(id string) *shard {
	return g.shards[shardIdx(id)]
}

// lockTwoWrite locks two shards for write in ascending index order to
// prevent deadlock. If both IDs land in the same shard, the mutex is
// locked exactly once. Returns a closure the caller defers to unlock.
func (g *Graph) lockTwoWrite(idA, idB string) func() {
	a := shardIdx(idA)
	b := shardIdx(idB)
	if a == b {
		s := g.shards[a]
		s.mu.Lock()
		return s.mu.Unlock
	}
	lo, hi := a, b
	if lo > hi {
		lo, hi = hi, lo
	}
	sLo := g.shards[lo]
	sHi := g.shards[hi]
	sLo.mu.Lock()
	sHi.mu.Lock()
	return func() {
		sHi.mu.Unlock()
		sLo.mu.Unlock()
	}
}

// lockThreeWrite locks up to three shards for write in ascending index
// order, deduplicating any that collide. Used by ReindexEdge, which
// mutates the From shard's outEdgeIdx plus both in-edge buckets when a
// resolver step retargets an edge; missing any of the three races with
// concurrent AddEdge on that shard (bug: "concurrent map read and map
// write" in addEdgeToBucket).
func (g *Graph) lockThreeWrite(idA, idB, idC string) func() {
	a, b, c := shardIdx(idA), shardIdx(idB), shardIdx(idC)
	// Sort (a, b, c) ascending without allocating a slice.
	if a > b {
		a, b = b, a
	}
	if b > c {
		b, c = c, b
	}
	if a > b {
		a, b = b, a
	}
	// Dedupe: lock each distinct index once.
	idxs := [3]int{a, -1, -1}
	n := 1
	if b != a {
		idxs[n] = b
		n++
	}
	if c != b && c != a {
		idxs[n] = c
		n++
	}
	for i := 0; i < n; i++ {
		g.shards[idxs[i]].mu.Lock()
	}
	return func() {
		for i := n - 1; i >= 0; i-- {
			g.shards[idxs[i]].mu.Unlock()
		}
	}
}

// lockAllWrite / lockAllRead take every shard's lock in order. Used by
// operations that have to touch the whole graph (AllNodes, Stats,
// EvictRepo). Callers must match with unlockAllWrite / unlockAllRead.
func (g *Graph) lockAllWrite() {
	for _, s := range g.shards {
		s.mu.Lock()
	}
}

func (g *Graph) unlockAllWrite() {
	for i := len(g.shards) - 1; i >= 0; i-- {
		g.shards[i].mu.Unlock()
	}
}

func (g *Graph) lockAllRead() {
	for _, s := range g.shards {
		s.mu.RLock()
	}
}

func (g *Graph) unlockAllRead() {
	for i := len(g.shards) - 1; i >= 0; i-- {
		g.shards[i].mu.RUnlock()
	}
}

// AddNode inserts or updates a node in the graph and all secondary
// indexes. Idempotent — a second call with the same ID replaces the
// existing Node pointer in place instead of appending duplicates to the
// byFile / byName / byRepo slices. If the new Node's FilePath, Name, or
// RepoPrefix differs from the stored one, the secondary-index entries
// are migrated from the old bucket to the new one atomically under the
// shard lock.
func (g *Graph) AddNode(n *Node) {
	s := g.shardFor(n.ID)
	s.mu.Lock()
	g.addNodeLocked(s, n)
	s.mu.Unlock()
}

// addNodeLocked is AddNode's body, expecting the caller to already hold
// the shard's write lock. Used by AddBatch to amortise lock acquisition
// across many node inserts targeting the same shard.
func (g *Graph) addNodeLocked(s *shard, n *Node) {
	prev, hadPrev := s.nodes[n.ID]
	// Subtract the previous size/count before overwriting; the new
	// node's contribution is re-added after the RepoPrefix-preservation
	// logic below has settled on the final prefix.
	if hadPrev {
		s.repoNodeRemove(prev)
	}
	s.nodes[n.ID] = n

	if hadPrev {
		if prev.FilePath != n.FilePath {
			removeNodeFromBucket(s.byFile, s.byFileIdx, prev.FilePath, n.ID)
		}
		if prev.Name != n.Name {
			removeNodeFromBucket(s.byName, s.byNameIdx, prev.Name, n.ID)
		}
		if prev.QualName != n.QualName && prev.QualName != "" {
			// byQual is a 1:1 index, not a slice — only delete when
			// the stored entry still points at this node ID (a
			// different node may have since taken the slot).
			if cur, ok := s.byQual[prev.QualName]; ok && cur.ID == n.ID {
				delete(s.byQual, prev.QualName)
			}
		}
		if prev.RepoPrefix != n.RepoPrefix && prev.RepoPrefix != "" {
			// Preserve a previously-set RepoPrefix rather than letting
			// an empty-prefix re-add silently strip the node out of its
			// byRepo bucket. The downgrade-to-empty case has no
			// legitimate caller (contract nodes use distinct IDs that
			// never collide with symbol IDs; the parse and IncrementalReindex
			// paths route through applyRepoPrefix which always stamps
			// the active idx.repoPrefix on the new node) and previously
			// caused per-repo `byRepo[prefix]` to drain mid-warmup,
			// breaking RepoStats / RepoMemoryEstimate / GetRepoNodes.
			// Restore the old prefix on the new node so the bucket
			// stays populated. The legitimate RepoPrefix-change case
			// (snapshot prefix → new prefix because config moved) still
			// works because n.RepoPrefix is non-empty there.
			if n.RepoPrefix == "" {
				n.RepoPrefix = prev.RepoPrefix
			} else {
				removeNodeFromBucket(s.byRepo, s.byRepoIdx, prev.RepoPrefix, n.ID)
			}
		}
	}

	addNodeToBucket(s.byFile, s.byFileIdx, n.FilePath, n.ID, n)
	addNodeToBucket(s.byName, s.byNameIdx, n.Name, n.ID, n)
	if n.QualName != "" {
		s.byQual[n.QualName] = n
	}
	if n.RepoPrefix != "" {
		addNodeToBucket(s.byRepo, s.byRepoIdx, n.RepoPrefix, n.ID, n)
	}
	s.repoNodeAdd(n)
}

// AddBatch inserts a set of nodes and edges in shard-grouped passes,
// acquiring each involved shard's write lock at most once across the
// whole batch. Replaces the O(N + 2E) per-item lock acquisitions of
// AddNode / AddEdge with O(distinct_shards) — typically ~16 instead of
// ~450 per per-file worker batch. The contention profile measured 69
// of 102 goroutines blocked on lockTwoWrite during cold-start parsing;
// batching is the throughput fix.
//
// Observable semantics differ slightly from a sequence of AddNode /
// AddEdge calls: for cross-shard edges, the From shard's outEdges
// receives the edge before the To shard's inEdges does. Readers that
// query one side only see the change atomically per shard; readers
// that join both sides may briefly see an outgoing edge whose
// reciprocal in-edge hasn't landed yet. The parser worker path that
// drives this is followed by ResolveAll / global derivation passes
// that take the resolver mutex graph-wide, so no concurrent reader is
// expected to depend on cross-side atomicity during warmup.
func (g *Graph) AddBatch(nodes []*Node, edges []*Edge) {
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	var nodesByShard [numShards][]*Node
	var outEdgesByShard [numShards][]*Edge
	var inEdgesByShard [numShards][]*Edge
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		i := shardIdx(n.ID)
		nodesByShard[i] = append(nodesByShard[i], n)
	}
	for _, e := range edges {
		if e == nil {
			continue
		}
		outEdgesByShard[shardIdx(e.From)] = append(outEdgesByShard[shardIdx(e.From)], e)
		inEdgesByShard[shardIdx(e.To)] = append(inEdgesByShard[shardIdx(e.To)], e)
	}

	for i := range numShards {
		if len(nodesByShard[i]) == 0 && len(outEdgesByShard[i]) == 0 && len(inEdgesByShard[i]) == 0 {
			continue
		}
		s := g.shards[i]
		s.mu.Lock()
		for _, n := range nodesByShard[i] {
			g.addNodeLocked(s, n)
		}
		// Out-side writes own the "was this a new insert?" signal that
		// drives the per-repo edge counter and the "did Origin change?"
		// signal that drives the identity-revision counter — the in-side
		// write is bookkeeping only and charges neither (mirrors
		// AddEdge's behaviour).
		for _, e := range outEdgesByShard[i] {
			inserted, originChanged := addEdgeToBucket(s.outEdges, s.outEdgeKeys, s.outEdgeIdx, e.From, e)
			if originChanged {
				g.edgeIdentityRevisions.Add(1)
			}
			if inserted {
				g.edgeMutGen.Add(1)
				var srcRepo string
				if src, ok := s.nodes[e.From]; ok && src != nil {
					srcRepo = src.RepoPrefix
				}
				s.repoEdgeAdd(srcRepo, e)
			}
		}
		for _, e := range inEdgesByShard[i] {
			addEdgeToBucket(s.inEdges, s.inEdgeKeys, s.inEdgeIdx, e.To, e)
		}
		s.mu.Unlock()
	}
}

// AddEdge inserts or updates a directed edge in the graph. Locks both
// the From and To shards (same shard locked once if they collide) so
// outEdges and inEdges stay consistent. Idempotent: a second call with
// the same (From, To, Kind, FilePath, Line) replaces the stored *Edge
// pointer in place — newer metadata (Confidence, Origin, etc.) wins,
// adjacency-list length is unchanged. Drops the double-edge problem
// that used to surface after daemon restarts (bug B1).
func (g *Graph) AddEdge(e *Edge) {
	unlock := g.lockTwoWrite(e.From, e.To)
	defer unlock()
	sFrom := g.shardFor(e.From)
	sTo := g.shardFor(e.To)
	// Only charge the source-repo counter on a brand-new insert.
	// Idempotent re-adds (same edgeKey) replace the slot in place and
	// would otherwise double-count. addEdgeToBucket reports inserted ==
	// true only when it actually appended. The out-side write owns both
	// signals: the new-insert flag (per-repo counter) and the origin-
	// changed flag (identity-revision counter). The in-side write is
	// bookkeeping only — charging either counter there would double it.
	inserted, originChanged := addEdgeToBucket(sFrom.outEdges, sFrom.outEdgeKeys, sFrom.outEdgeIdx, e.From, e)
	addEdgeToBucket(sTo.inEdges, sTo.inEdgeKeys, sTo.inEdgeIdx, e.To, e)
	if originChanged {
		// A re-add with the same logical key but an upgraded Origin:
		// the old identity is retired, a new one created.
		g.edgeIdentityRevisions.Add(1)
	}
	if inserted {
		g.edgeMutGen.Add(1)
		var srcRepo string
		if src, ok := sFrom.nodes[e.From]; ok && src != nil {
			srcRepo = src.RepoPrefix
		}
		sFrom.repoEdgeAdd(srcRepo, e)
	}
}

// SetEdgeProvenance changes the Origin of an edge already in the graph
// and is the only sanctioned way to do so. Conceptually it is a
// delete-then-insert of the edge's identity: because Origin is part of
// IdentityHash, the old provenance-bearing identity is retired and a
// new one created — even though the logical (From,To,Kind,FilePath,
// Line) key, and therefore the adjacency-list slot, is unchanged.
//
// It computes the edge's old and new IdentityHash. When they are equal
// (newOrigin matches the current Origin) nothing changes and it returns
// false. When they differ it applies e.Origin = newOrigin, re-derives
// the Origin-derived Tier label when one was set (Confidence and
// ConfidenceLabel are score-derived, not Origin-derived, so they are
// left intact), increments the graph-level identity-revision counter,
// and returns true.
//
// Mutating Edge.Origin directly on an in-graph edge bypasses the
// counter and is a provenance-tampering bug — route every such change
// here so the churn stays observable via EdgeIdentityRevisions.
func (g *Graph) SetEdgeProvenance(e *Edge, newOrigin string) bool {
	if e == nil {
		return false
	}
	unlock := g.lockTwoWrite(e.From, e.To)
	defer unlock()
	oldIdentity := e.IdentityHash()
	newIdentity := hashEdgeIdentity(keyOf(e), newOrigin)
	if oldIdentity == newIdentity {
		return false
	}
	e.Origin = newOrigin
	// Tier is a pure projection of Origin (graph.ResolvedBy). Re-derive
	// it only when it was already populated — an empty Tier is the
	// in-memory default and re-deriving would silently start stamping
	// it. The same *Edge pointer lives in both the out- and in-edge
	// buckets, so this write is visible from every adjacency view.
	if e.Tier != "" {
		e.Tier = ResolvedBy(newOrigin)
	}
	g.edgeIdentityRevisions.Add(1)
	return true
}

// EdgeIdentityRevisions returns how many times an in-graph edge's
// provenance-bearing identity has changed over this graph's lifetime —
// the running total fed by SetEdgeProvenance and by AddEdge's in-place
// re-add path. It is monotonic and never decremented; a reindex that
// retires and recreates edges does not roll it back. Surfaced through
// graph_stats as the tamper-evidence signal for provenance churn.
func (g *Graph) EdgeIdentityRevisions() int {
	return int(g.edgeIdentityRevisions.Load())
}

// VerifyEdgeIdentities walks every edge and confirms its provenance-
// bearing identity is internally consistent: the edge stored in a
// source node's outEdges bucket and the edge stored in the target
// node's inEdges bucket are the same *Edge pointer and therefore agree
// on IdentityHash. addEdgeToBucket stores one shared pointer in both
// buckets, so a consistent graph always passes; a divergence means
// some code mutated Origin on a copied edge (e.g. a resolver clone)
// and wrote it into only one adjacency view, leaving the two sides
// disagreeing about provenance. Returns nil when every edge is
// consistent, or an error naming the first divergent edge.
//
// This is the assertion a test uses to prove provenance cannot be
// silently changed outside SetEdgeProvenance.
func (g *Graph) VerifyEdgeIdentities() error {
	g.lockAllRead()
	defer g.unlockAllRead()
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			for _, e := range edges {
				if e == nil {
					continue
				}
				want := e.IdentityHash()
				sTo := g.shardFor(e.To)
				found := false
				for _, in := range sTo.inEdges[e.To] {
					if in == e {
						if in.IdentityHash() != want {
							return &edgeIdentityError{edge: e, reason: "inEdges pointer disagrees on identity hash"}
						}
						found = true
						break
					}
				}
				if !found {
					return &edgeIdentityError{edge: e, reason: "outEdges edge missing from target inEdges bucket"}
				}
			}
		}
	}
	return nil
}

// edgeIdentityError reports the first edge VerifyEdgeIdentities found
// to be inconsistent across the out- and in-edge adjacency views.
type edgeIdentityError struct {
	edge   *Edge
	reason string
}

func (e *edgeIdentityError) Error() string {
	return "edge identity inconsistent (" + e.reason + "): " +
		e.edge.From + " -" + string(e.edge.Kind) + "-> " + e.edge.To
}

// ReindexEdge updates the inEdges index after an edge's To field has
// been mutated (e.g., by the resolver changing "unresolved::X" to a
// real target). oldTo is the previous value of e.To before mutation.
//
// Three sidecar entries change: the outEdgeIdx key for From (since the
// edgeKey depends on To), the inEdgeIdx entry on the old target bucket
// (removed), and the inEdgeIdx entry on the new target bucket (added).
func (g *Graph) ReindexEdge(e *Edge, oldTo string) {
	if oldTo == e.To {
		return
	}
	// Must lock the From shard too — we mutate sFrom.outEdgeIdx below,
	// and without its lock a concurrent AddEdge on From panics the
	// runtime with "concurrent map read and map write".
	unlock := g.lockThreeWrite(e.From, oldTo, e.To)
	defer unlock()

	// Old identity uses oldTo; the current edge struct already has the
	// new To set, so we reconstruct the key before mutation.
	oldKey := hashEdgeKey(edgeKey{From: e.From, To: oldTo, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line})
	newKey := hashEdgeKey(keyOf(e))

	sFrom := g.shardFor(e.From)
	// outEdges slot position doesn't move — only the key under which
	// the sidecar records it changes. Avoid a churn of slice growth by
	// swapping the sidecar entry in place.
	//
	// The parallel outEdgeKeys slice MUST be updated alongside
	// outEdgeIdx. removeEdgeFromBucket reads outEdgeKeys[pos] to
	// learn the swapped slot's insertion-time key during swap-with-
	// last; leaving outEdgeKeys stale here would re-insert the old
	// key into outEdgeIdx pointing at a swapped position, and the
	// next swap on that key would compute a pos past the (now
	// shorter) slice — the exact index-out-of-range panic that
	// surfaces during evictEdgesLocked when warmup retargets a lot
	// of edges via ReindexEdge.
	if fromIdx, ok := sFrom.outEdgeIdx[e.From]; ok {
		if pos, exists := fromIdx[oldKey]; exists {
			delete(fromIdx, oldKey)
			fromIdx[newKey] = pos
			if keys, ok := sFrom.outEdgeKeys[e.From]; ok && pos < len(keys) {
				keys[pos] = newKey
			}
		}
	}

	// Move from the old target's inEdges bucket to the new one.
	sOld := g.shardFor(oldTo)
	removeEdgeFromBucket(sOld.inEdges, sOld.inEdgeKeys, sOld.inEdgeIdx, oldTo, oldKey)
	sNew := g.shardFor(e.To)
	addEdgeToBucket(sNew.inEdges, sNew.inEdgeKeys, sNew.inEdgeIdx, e.To, e)
	// No edgeMutGen bump here: outEdges retains the same *Edge slot,
	// and the cached AllEdges slice holds pointers — readers see the
	// already-mutated e.To via that pointer. The cache stays valid.
}

// GetNode returns a node by ID, or nil if not found.
func (g *Graph) GetNode(id string) *Node {
	s := g.shardFor(id)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[id]
}

// GetNodeByQualName returns a node by fully-qualified name, or nil.
// The qual name index is partitioned across shards (each shard owns the
// qual names of the nodes it stores), so we ask every shard.
func (g *Graph) GetNodeByQualName(qualName string) *Node {
	for _, s := range g.shards {
		s.mu.RLock()
		if n, ok := s.byQual[qualName]; ok {
			s.mu.RUnlock()
			return n
		}
		s.mu.RUnlock()
	}
	return nil
}

// FindNodesByName returns all nodes matching the short name.
//
// Implementation walks every shard's byName bucket. The two-pass shape
// (sum then allocate) trades one extra read-lock round trip per shard
// for a single right-sized allocation — the prior single-pass append
// re-grew `out` on every hot shard (1 + log2(N) reallocations), which
// the cold-index heap profile attributed 5.22 GB / 14% of total alloc
// to. Names with a long candidate list (`Visit`, `init`, `create`)
// see the biggest win.
func (g *Graph) FindNodesByName(name string) []*Node {
	total := 0
	for _, s := range g.shards {
		s.mu.RLock()
		total += len(s.byName[name])
		s.mu.RUnlock()
	}
	if total == 0 {
		return nil
	}
	out := make([]*Node, 0, total)
	for _, s := range g.shards {
		s.mu.RLock()
		if src := s.byName[name]; len(src) > 0 {
			out = append(out, src...)
		}
		s.mu.RUnlock()
	}
	return out
}

// FindNodesByNameInRepo returns nodes matching the short name that are
// either in the given repoPrefix or carry an empty RepoPrefix (synthetic
// / stdlib nodes — kept same-repo by convention). Equivalent to
// filterSameRepo(repoPrefix, FindNodesByName(name)) but skips the
// intermediate cross-repo candidate slice.
//
// In single-repo graphs (repoPrefix == ""), behaves identically to
// FindNodesByName.
func (g *Graph) FindNodesByNameInRepo(name, repoPrefix string) []*Node {
	if repoPrefix == "" {
		return g.FindNodesByName(name)
	}
	// First pass: count matches that pass the repo filter. Counting in
	// a separate pass keeps `out` right-sized even when ~95% of the
	// byName bucket lives in unrelated repos.
	total := 0
	for _, s := range g.shards {
		s.mu.RLock()
		for _, n := range s.byName[name] {
			if n.RepoPrefix == "" || n.RepoPrefix == repoPrefix {
				total++
			}
		}
		s.mu.RUnlock()
	}
	if total == 0 {
		return nil
	}
	out := make([]*Node, 0, total)
	for _, s := range g.shards {
		s.mu.RLock()
		for _, n := range s.byName[name] {
			if n.RepoPrefix == "" || n.RepoPrefix == repoPrefix {
				out = append(out, n)
			}
		}
		s.mu.RUnlock()
	}
	return out
}

// FindNodesByNameContaining returns nodes whose Name (case-insensitive)
// contains substr. The in-memory backend has no name-substring index,
// so this is a single pass over the byName buckets (which already group
// nodes by exact name — the same allocation we'd pay for one FindNodesByName
// call per distinct name). limit caps the slice; 0 means "no limit".
//
// Stable order is the caller's responsibility — bucket iteration is
// deterministic per shard but cross-shard order isn't fixed.
func (g *Graph) FindNodesByNameContaining(substr string, limit int) []*Node {
	if substr == "" {
		return nil
	}
	needle := strings.ToLower(substr)
	var out []*Node
	for _, s := range g.shards {
		s.mu.RLock()
		for name, bucket := range s.byName {
			if !strings.Contains(strings.ToLower(name), needle) {
				continue
			}
			out = append(out, bucket...)
			if limit > 0 && len(out) >= limit {
				s.mu.RUnlock()
				return out[:limit]
			}
		}
		s.mu.RUnlock()
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// GetFileNodes returns all nodes defined in the given file.
func (g *Graph) GetFileNodes(filePath string) []*Node {
	var out []*Node
	for _, s := range g.shards {
		s.mu.RLock()
		if src := s.byFile[filePath]; len(src) > 0 {
			out = append(out, src...)
		}
		s.mu.RUnlock()
	}
	return out
}

// GetOutEdges returns outgoing edges for a node.
func (g *Graph) GetOutEdges(nodeID string) []*Edge {
	s := g.shardFor(nodeID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.outEdges[nodeID]
	out := make([]*Edge, len(src))
	copy(out, src)
	return out
}

// GetInEdges returns incoming edges for a node.
func (g *Graph) GetInEdges(nodeID string) []*Edge {
	s := g.shardFor(nodeID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.inEdges[nodeID]
	out := make([]*Edge, len(src))
	copy(out, src)
	return out
}

// GetOutEdgesByNodeIDs returns a map id→outgoing edges for every input
// id. The in-memory backend loops the existing GetOutEdges — cost
// matches a hand-written loop in the caller. The value of the batched
// API lives in disk backends, where it collapses N point lookups into
// one bulk Cypher query. Empty input returns nil; duplicate ids are
// deduped naturally. Missing ids are absent from the returned map.
func (g *Graph) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string][]*Edge, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := out[id]; ok {
			continue
		}
		out[id] = g.GetOutEdges(id)
	}
	return out
}

// GetInEdgesByNodeIDs is the inbound sibling of GetOutEdgesByNodeIDs.
// See that doc-comment for the contract.
func (g *Graph) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string][]*Edge, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := out[id]; ok {
			continue
		}
		out[id] = g.GetInEdges(id)
	}
	return out
}

// EvictFile removes all nodes and edges belonging to the given file
// path. Nodes for one file can span many shards (different IDs hash
// differently), so we lock all shards for this multi-shard operation.
func (g *Graph) EvictFile(filePath string) (nodesRemoved, edgesRemoved int) {
	g.lockAllWrite()
	defer g.unlockAllWrite()

	// Gather nodes across shards.
	var nodes []*Node
	for _, s := range g.shards {
		nodes = append(nodes, s.byFile[filePath]...)
	}
	if len(nodes) == 0 {
		return 0, 0
	}
	// id → source-repo captured BEFORE we delete the node from
	// s.nodes; evictEdgesLocked needs the repo to debit per-repo
	// edge counters and the live node would already be gone.
	evictedIDs := make(map[string]string, len(nodes))
	for _, n := range nodes {
		evictedIDs[n.ID] = n.RepoPrefix
	}

	for _, n := range nodes {
		s := g.shardFor(n.ID)
		s.repoNodeRemove(n)
		delete(s.nodes, n.ID)
		if n.QualName != "" {
			if cur, ok := s.byQual[n.QualName]; ok && cur.ID == n.ID {
				delete(s.byQual, n.QualName)
			}
		}
		removeNodeFromBucket(s.byName, s.byNameIdx, n.Name, n.ID)
		removeNodeFromBucket(s.byFile, s.byFileIdx, filePath, n.ID)
		if n.RepoPrefix != "" {
			removeNodeFromBucket(s.byRepo, s.byRepoIdx, n.RepoPrefix, n.ID)
		}
	}
	nodesRemoved = len(nodes)

	edgesRemoved = g.evictEdgesLocked(evictedIDs)
	return nodesRemoved, edgesRemoved
}

// evictEdgesLocked is the shared edge-removal core used by EvictFile
// and EvictRepo. Callers must hold every shard's write lock.
//
// For each evicted node we remove its outEdges and inEdges entries. To
// clean the reverse index on non-evicted endpoints we do a swap-with-
// last removal via sidecar, which is O(1) per edge instead of the
// O(slice-size) filterEdge scan the older implementation used.
func (g *Graph) evictEdgesLocked(evictedIDs map[string]string) int {
	removed := 0
	defer func() {
		if removed > 0 {
			g.edgeMutGen.Add(1)
		}
	}()

	// Phase 1: remove outgoing edges from every evicted node. Use the
	// parallel outEdgeKeys slice to look up each entry's insertion-time
	// edgeKey rather than recomputing keyOf — the latter races with
	// resolver-driven Edge.To mutations elsewhere in the graph and
	// can yield a key that doesn't match the inEdges sidecar.
	for id, srcRepo := range evictedIDs {
		s := g.shardFor(id)
		edges := s.outEdges[id]
		keys := s.outEdgeKeys[id]
		removed += len(edges)
		for i, e := range edges {
			s.repoEdgeRemove(srcRepo, e)
			if _, evicted := evictedIDs[e.To]; !evicted {
				sTo := g.shardFor(e.To)
				removeEdgeFromBucket(sTo.inEdges, sTo.inEdgeKeys, sTo.inEdgeIdx, e.To, keys[i])
			}
		}
		delete(s.outEdges, id)
		delete(s.outEdgeKeys, id)
		delete(s.outEdgeIdx, id)
	}

	// Phase 2: remove incoming edges to every evicted node (from
	// non-evicted sources — same-direction edges were already handled
	// in phase 1 and counted). Each surviving source's repo is read
	// from its live node — sFrom.nodes[e.From] is still present in
	// this phase because that source wasn't evicted.
	for id := range evictedIDs {
		s := g.shardFor(id)
		edges := s.inEdges[id]
		keys := s.inEdgeKeys[id]
		for i, e := range edges {
			if _, evicted := evictedIDs[e.From]; !evicted {
				removed++
				sFrom := g.shardFor(e.From)
				var srcRepo string
				if src, ok := sFrom.nodes[e.From]; ok && src != nil {
					srcRepo = src.RepoPrefix
				}
				removeEdgeFromBucket(sFrom.outEdges, sFrom.outEdgeKeys, sFrom.outEdgeIdx, e.From, keys[i])
				sFrom.repoEdgeRemove(srcRepo, e)
			}
		}
		delete(s.inEdges, id)
		delete(s.inEdgeKeys, id)
		delete(s.inEdgeIdx, id)
	}

	return removed
}

// RemoveEdge removes a specific edge by from, to, and kind. Returns
// true if the edge was found and removed. When multiple edges match
// (same from/to/kind but different file/line — rare but possible),
// removes the first one encountered.
func (g *Graph) RemoveEdge(from, to string, kind EdgeKind) bool {
	unlock := g.lockTwoWrite(from, to)
	defer unlock()

	sFrom := g.shardFor(from)
	outList := sFrom.outEdges[from]
	outKeys := sFrom.outEdgeKeys[from]
	targetIdx := -1
	for i, e := range outList {
		if e.To == to && e.Kind == kind {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return false
	}

	// Snapshot the edge plus its source-repo before mutating the
	// buckets — once removeEdgeFromBucket swaps the tail in, outList[i]
	// is no longer the edge we're removing.
	removed := outList[targetIdx]
	var srcRepo string
	if src, ok := sFrom.nodes[from]; ok && src != nil {
		srcRepo = src.RepoPrefix
	}

	// Use the stored insertion-time key rather than keyOf(target) so
	// removal is robust against in-flight Edge.To mutations.
	k := outKeys[targetIdx]
	removeEdgeFromBucket(sFrom.outEdges, sFrom.outEdgeKeys, sFrom.outEdgeIdx, from, k)
	sTo := g.shardFor(to)
	removeEdgeFromBucket(sTo.inEdges, sTo.inEdgeKeys, sTo.inEdgeIdx, to, k)
	sFrom.repoEdgeRemove(srcRepo, removed)
	g.edgeMutGen.Add(1)
	return true
}

// NodeCount returns the total number of nodes.
func (g *Graph) NodeCount() int {
	total := 0
	for _, s := range g.shards {
		s.mu.RLock()
		total += len(s.nodes)
		s.mu.RUnlock()
	}
	return total
}

// EdgeCount returns the total number of edges.
func (g *Graph) EdgeCount() int {
	total := 0
	for _, s := range g.shards {
		s.mu.RLock()
		for _, edges := range s.outEdges {
			total += len(edges)
		}
		s.mu.RUnlock()
	}
	return total
}

// AllNodes returns a snapshot of all nodes. Locks every shard for read
// to produce a coherent view — callers use this for snapshots,
// contracts extraction, etc. where a consistent crop matters.
func (g *Graph) AllNodes() []*Node {
	g.lockAllRead()
	defer g.unlockAllRead()
	total := 0
	for _, s := range g.shards {
		total += len(s.nodes)
	}
	out := make([]*Node, 0, total)
	for _, s := range g.shards {
		for _, n := range s.nodes {
			out = append(out, n)
		}
	}
	return out
}

// AllEdges returns a snapshot of all outgoing edges across every shard.
//
// Cached by the edgeMutGen counter: once built, subsequent calls
// return the same slice pointer as long as no edge has been
// inserted, removed, or had its slot pointer replaced. Mutations
// bump edgeMutGen, the next AllEdges sees the mismatch and rebuilds.
//
// On a 4 M-edge graph (k8s) one snapshot is ~32 MB of pointer slice;
// the post-resolve analysis fan-out (cycles, communities, deadcode,
// hierarchy, pagerank, hits, betweenness, several MCP analyzers) used
// to call this dozens of times per cold-index, all allocating fresh
// — 2.72 GB / 8 % of total in the heap profile. Caching collapses
// that to a single allocation per generation.
//
// Callers MUST treat the returned slice as read-only. Mutating its
// pointers is a data race against any concurrent reader holding the
// same cached reference. The underlying *Edge structs are themselves
// shared with the graph and may be mutated by ReindexEdge /
// SetEdgeProvenance — that's intentional, and readers see those
// mutations through the pointer.
func (g *Graph) AllEdges() []*Edge {
	curGen := g.edgeMutGen.Load()
	g.allEdgesCacheMu.Lock()
	defer g.allEdgesCacheMu.Unlock()
	if g.allEdgesCache != nil && g.allEdgesCacheGen == curGen {
		return g.allEdgesCache
	}

	g.lockAllRead()
	// Pre-size from the per-shard outEdges entry counts. EdgeCount
	// would re-lock; just sum inline while holding the locks.
	total := 0
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			total += len(edges)
		}
	}
	out := make([]*Edge, 0, total)
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			out = append(out, edges...)
		}
	}
	g.unlockAllRead()

	g.allEdgesCache = out
	g.allEdgesCacheGen = curGen
	return out
}

// DrainNodes yields every node and FREES the graph's internal node
// storage shard-by-shard as it goes. After Drain finishes the graph
// holds zero nodes. Intended for the one-shot persist path where the
// shadow is about to be discarded: AllNodes would pin the full 11 GB
// graph for the entire persist phase; Drain releases each shard's
// node map (and the per-name / per-file / per-repo indexes) as soon
// as that shard's iteration completes, so GC can reclaim ~700 MB at
// a time on a Linux-scale graph instead of waiting for the indexer's
// defer to return.
//
// The graph remains structurally consistent during Drain — edges and
// other indexes are untouched, only the node maps are emptied. If
// you also need DrainEdges, call them in either order; both are
// destructive and idempotent (a second call yields nothing).
func (g *Graph) DrainNodes() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		for _, s := range g.shards {
			s.mu.Lock()
			nodes := s.nodes
			// Replace with an empty map so the shard's read methods
			// keep working (return zero) instead of nil-panicking.
			s.nodes = map[string]*Node{}
			s.byFile = map[string][]*Node{}
			s.byName = map[string][]*Node{}
			s.byQual = map[string]*Node{}
			s.byRepo = map[string][]*Node{}
			s.byFileIdx = map[string]map[string]int{}
			s.byNameIdx = map[string]map[string]int{}
			s.byRepoIdx = map[string]map[string]int{}
			s.mu.Unlock()
			for _, n := range nodes {
				if !yield(n) {
					return
				}
			}
			// nodes goes out of scope here — the shard's old map plus
			// every *Node it referenced is now GC-eligible (assuming
			// the caller has dropped any remaining reference).
		}
	}
}

// DrainEdges yields every edge and FREES the graph's internal edge
// storage shard-by-shard. Same semantics as DrainNodes — meant for
// the persist hand-off, not for general queries.
func (g *Graph) DrainEdges() iter.Seq[*Edge] {
	// Invalidate the AllEdges cache so any subsequent caller doesn't
	// see drained-shard zombies. The cache holds direct *Edge slice
	// references that DrainEdges is about to start freeing.
	g.allEdgesCacheMu.Lock()
	g.allEdgesCache = nil
	g.allEdgesCacheGen = 0
	g.allEdgesCacheMu.Unlock()
	return func(yield func(*Edge) bool) {
		for _, s := range g.shards {
			s.mu.Lock()
			outEdges := s.outEdges
			s.outEdges = map[string][]*Edge{}
			s.inEdges = map[string][]*Edge{}
			s.outEdgeIdx = map[string]map[edgeHash]int{}
			s.inEdgeIdx = map[string]map[edgeHash]int{}
			s.outEdgeKeys = map[string][]edgeHash{}
			s.inEdgeKeys = map[string][]edgeHash{}
			s.mu.Unlock()
			for _, edges := range outEdges {
				for _, e := range edges {
					if !yield(e) {
						return
					}
				}
			}
		}
	}
}

// Stats returns summary counts by kind and language.
func (g *Graph) Stats() GraphStats {
	g.lockAllRead()
	defer g.unlockAllRead()

	byKind := make(map[string]int)
	byLang := make(map[string]int)
	totalNodes := 0
	for _, s := range g.shards {
		totalNodes += len(s.nodes)
		for _, n := range s.nodes {
			byKind[string(n.Kind)]++
			if n.Language != "" {
				byLang[n.Language]++
			}
		}
	}

	edgeCount := 0
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			edgeCount += len(edges)
		}
	}

	return GraphStats{
		TotalNodes: totalNodes,
		TotalEdges: edgeCount,
		ByKind:     byKind,
		ByLanguage: byLang,
	}
}

// GetRepoNodes returns all nodes belonging to the given repository
// prefix. Each shard holds a byRepo slice for nodes it owns; we
// aggregate across shards.
func (g *Graph) GetRepoNodes(repoPrefix string) []*Node {
	var out []*Node
	for _, s := range g.shards {
		s.mu.RLock()
		if src := s.byRepo[repoPrefix]; len(src) > 0 {
			out = append(out, src...)
		}
		s.mu.RUnlock()
	}
	return out
}

// GetRepoEdges returns every edge whose source node has the given
// RepoPrefix — the in-memory reference implementation of the
// Store-interface method. Walks each shard's byRepo bucket and
// concatenates that node's outEdges in place (no per-node
// GetOutEdges call, so no per-call slice copy). Equivalent in
// observable behaviour to the GetRepoNodes(r) × GetOutEdges loop
// callers used before this method existed; meant to give disk
// backends a single-query hook without changing in-memory cost.
// Empty repoPrefix returns nil (callers use AllEdges() instead).
func (g *Graph) GetRepoEdges(repoPrefix string) []*Edge {
	if repoPrefix == "" {
		return nil
	}
	var out []*Edge
	for _, s := range g.shards {
		s.mu.RLock()
		for _, n := range s.byRepo[repoPrefix] {
			if src := s.outEdges[n.ID]; len(src) > 0 {
				out = append(out, src...)
			}
		}
		s.mu.RUnlock()
	}
	return out
}

// EvictRepo removes all nodes with matching RepoPrefix and all edges
// referencing those nodes. Returns counts of removed nodes and edges.
func (g *Graph) EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	g.lockAllWrite()
	defer g.unlockAllWrite()

	var nodes []*Node
	for _, s := range g.shards {
		nodes = append(nodes, s.byRepo[repoPrefix]...)
	}
	if len(nodes) == 0 {
		return 0, 0
	}
	evictedIDs := make(map[string]string, len(nodes))
	for _, n := range nodes {
		evictedIDs[n.ID] = repoPrefix
	}

	for _, n := range nodes {
		s := g.shardFor(n.ID)
		s.repoNodeRemove(n)
		delete(s.nodes, n.ID)
		if n.QualName != "" {
			if cur, ok := s.byQual[n.QualName]; ok && cur.ID == n.ID {
				delete(s.byQual, n.QualName)
			}
		}
		removeNodeFromBucket(s.byName, s.byNameIdx, n.Name, n.ID)
		removeNodeFromBucket(s.byFile, s.byFileIdx, n.FilePath, n.ID)
		removeNodeFromBucket(s.byRepo, s.byRepoIdx, repoPrefix, n.ID)
	}
	nodesRemoved = len(nodes)

	edgesRemoved = g.evictEdgesLocked(evictedIDs)
	return nodesRemoved, edgesRemoved
}

// RepoStats returns per-repository node and edge counts.
func (g *Graph) RepoStats() map[string]GraphStats {
	g.lockAllRead()
	defer g.unlockAllRead()

	// Aggregate byRepo across shards first.
	repoNodes := make(map[string][]*Node)
	for _, s := range g.shards {
		for prefix, nodes := range s.byRepo {
			repoNodes[prefix] = append(repoNodes[prefix], nodes...)
		}
	}

	stats := make(map[string]GraphStats, len(repoNodes))
	repoByKind := make(map[string]map[string]int)
	repoByLang := make(map[string]map[string]int)
	repoNodeCount := make(map[string]int)
	for prefix, nodes := range repoNodes {
		repoNodeCount[prefix] = len(nodes)
		byKind := make(map[string]int)
		byLang := make(map[string]int)
		for _, n := range nodes {
			byKind[string(n.Kind)]++
			if n.Language != "" {
				byLang[n.Language]++
			}
		}
		repoByKind[prefix] = byKind
		repoByLang[prefix] = byLang
	}

	// Count edges per repo by the From node's repo. Need to look up the
	// From node in whichever shard owns it.
	repoEdgeCount := make(map[string]int)
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			for _, e := range edges {
				fromShard := g.shardFor(e.From)
				if fromNode, ok := fromShard.nodes[e.From]; ok && fromNode.RepoPrefix != "" {
					repoEdgeCount[fromNode.RepoPrefix]++
				}
			}
		}
	}

	for prefix := range repoNodes {
		stats[prefix] = GraphStats{
			TotalNodes: repoNodeCount[prefix],
			TotalEdges: repoEdgeCount[prefix],
			ByKind:     repoByKind[prefix],
			ByLanguage: repoByLang[prefix],
		}
	}

	return stats
}

// RepoMemoryEstimate is an approximate breakdown of how many bytes a
// single repository's graph contribution occupies. It covers the
// sharded node/edge maps only — search and vector indexes are
// orthogonal and computed elsewhere.
type RepoMemoryEstimate struct {
	NodeBytes uint64 `json:"node_bytes"`
	EdgeBytes uint64 `json:"edge_bytes"`
	NodeCount int    `json:"node_count"`
	EdgeCount int    `json:"edge_count"`
}

// Total returns the sum of NodeBytes and EdgeBytes.
func (e RepoMemoryEstimate) Total() uint64 { return e.NodeBytes + e.EdgeBytes }

// per-node fixed overhead: the struct header plus the amortised cost
// of the pointers held by byRepo/byFile/byName/byQual secondary
// indexes inside each shard (4 maps × ~24 bytes for map bucket + slice
// element ≈ 100 bytes). Tuned against runtime.ReadMemStats deltas on a
// 50k-node repo; within ~10% of actual.
const nodeStructOverhead = 240

// per-edge fixed overhead: two string pointers, kind, filepath, line,
// plus slice-header and adjacency-map amortisation for outEdges AND
// inEdges (every edge is stored once as a struct but is referenced from
// both the source's out-adjacency list and the target's in-adjacency
// list, so the amortised overhead is ~2× slice-element + map-bucket).
const edgeStructOverhead = 144

// RepoMemoryEstimate sums the running per-shard counters maintained
// by AddNode / AddEdge / RemoveEdge / EvictFile / EvictRepo. O(shard
// count) instead of the O(repo nodes + total edges) walk this used to
// do — relevant because daemon-status queries call this once per
// tracked repo, and on a 488-repo / 1.9M-edge graph the old
// implementation was the single biggest source of writer contention
// during warmup.
func (g *Graph) RepoMemoryEstimate(repoPrefix string) RepoMemoryEstimate {
	g.lockAllRead()
	defer g.unlockAllRead()

	var est RepoMemoryEstimate
	for _, s := range g.shards {
		est.NodeBytes += s.repoNodeBytes[repoPrefix]
		est.NodeCount += s.repoNodeCount[repoPrefix]
		est.EdgeBytes += s.repoEdgeBytes[repoPrefix]
		est.EdgeCount += s.repoEdgeCount[repoPrefix]
	}
	return est
}

// AllRepoMemoryEstimates returns the per-repo estimate for every
// repo with a tracked counter — one pass across shards, one read lock
// acquisition. Callers driving a per-repo loop (daemon status) should
// prefer this over the single-repo variant.
func (g *Graph) AllRepoMemoryEstimates() map[string]RepoMemoryEstimate {
	g.lockAllRead()
	defer g.unlockAllRead()

	out := make(map[string]RepoMemoryEstimate)
	for _, s := range g.shards {
		for prefix, bytes := range s.repoNodeBytes {
			est := out[prefix]
			est.NodeBytes += bytes
			est.NodeCount += s.repoNodeCount[prefix]
			out[prefix] = est
		}
		for prefix, bytes := range s.repoEdgeBytes {
			est := out[prefix]
			est.EdgeBytes += bytes
			est.EdgeCount += s.repoEdgeCount[prefix]
			out[prefix] = est
		}
	}
	return out
}

// nodeBytes estimates the memory footprint of a single graph.Node.
func nodeBytes(n *Node) uint64 {
	if n == nil {
		return 0
	}
	b := uint64(nodeStructOverhead)
	b += uint64(len(n.ID) + len(n.Name) + len(n.QualName) + len(n.FilePath) + len(n.Language) + len(n.RepoPrefix))
	b += metaBytes(n.Meta)
	return b
}

// edgeBytes estimates the memory footprint of a single graph.Edge.
func edgeBytes(e *Edge) uint64 {
	if e == nil {
		return 0
	}
	b := uint64(edgeStructOverhead)
	b += uint64(len(e.From) + len(e.To) + len(e.Kind) + len(e.FilePath))
	return b
}

// metaBytes approximates the size of a Node.Meta map. Only handles the
// kinds of values we actually produce (string, bool, numeric, nested
// map, []string) — more exotic types fall back to a conservative
// constant rather than reflecting recursively.
func metaBytes(m map[string]any) uint64 {
	if m == nil {
		return 0
	}
	// map header + bucket amortisation for small maps.
	b := uint64(48 + 8*len(m))
	for k, v := range m {
		b += uint64(len(k)) + 16 // key entry overhead
		switch val := v.(type) {
		case string:
			b += uint64(len(val)) + 16
		case bool:
			b += 1 + 16
		case int, int32, int64, uint, uint32, uint64, float32, float64:
			b += 8 + 16
		case []string:
			b += 24 // slice header
			for _, s := range val {
				b += uint64(len(s)) + 16
			}
		case map[string]any:
			b += metaBytes(val)
		default:
			b += 32 // unknown — leave a sensible estimate
		}
	}
	return b
}

// RepoPrefixes returns a list of unique repository prefixes in the
// graph.
func (g *Graph) RepoPrefixes() []string {
	seen := make(map[string]struct{})
	for _, s := range g.shards {
		s.mu.RLock()
		for prefix := range s.byRepo {
			seen[prefix] = struct{}{}
		}
		s.mu.RUnlock()
	}
	prefixes := make([]string, 0, len(seen))
	for prefix := range seen {
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// InDegreeForNodes is the in-memory reference implementation of the
// InDegreeForNodes capability. Walks the per-target in-edge buckets
// directly — the same arithmetic the disk backends push into a single
// Cypher COUNT.
func (g *Graph) InDegreeForNodes(ids []string) map[string]int {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]int, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		c := len(g.GetInEdges(id))
		if c == 0 {
			continue
		}
		out[id] = c
	}
	return out
}

// ReachableForwardByKinds is the in-memory reference implementation
// of the ReachableForwardByKinds capability. Layer-by-layer BFS from
// the seed frontier, following only edges whose Kind is in the
// supplied set. Pure map / slice walks here — the win is the disk
// backends fold the BFS into one variable-length Cypher match.
func (g *Graph) ReachableForwardByKinds(seeds []string, kinds []EdgeKind) map[string]bool {
	if len(seeds) == 0 {
		return nil
	}
	covered := make(map[string]bool, len(seeds))
	frontier := make([]string, 0, len(seeds))
	for _, id := range seeds {
		if id == "" || covered[id] {
			continue
		}
		covered[id] = true
		frontier = append(frontier, id)
	}
	if len(kinds) == 0 {
		return covered
	}
	allowed := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		allowed[k] = struct{}{}
	}
	for len(frontier) > 0 {
		next := frontier[:0:0]
		for _, id := range frontier {
			for _, e := range g.GetOutEdges(id) {
				if e == nil {
					continue
				}
				if _, ok := allowed[e.Kind]; !ok {
					continue
				}
				if !covered[e.To] {
					covered[e.To] = true
					next = append(next, e.To)
				}
			}
		}
		frontier = next
	}
	return covered
}

// ThrowerErrorSurface is the in-memory reference implementation of
// the ThrowerErrorSurfacer capability. Walks EdgeThrows once for the
// per-thrower target dedup, then walks each thrower's out-edges for
// the EdgeEmits → KindString(context=error_msg) attachment. The disk
// backends collapse both passes into two Cypher GROUP BYs.
func (g *Graph) ThrowerErrorSurface(pathPrefix string) []ThrowerErrorRow {
	byThrower := map[string]*ThrowerErrorRow{}
	addUnique := func(set []string, v string) []string {
		if slices.Contains(set, v) {
			return set
		}
		return append(set, v)
	}
	for e := range g.EdgesByKind(EdgeThrows) {
		if e == nil {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(e.FilePath, pathPrefix) {
			continue
		}
		row, ok := byThrower[e.From]
		if !ok {
			file := e.FilePath
			line := e.Line
			n := g.GetNode(e.From)
			if n != nil {
				if file == "" {
					file = n.FilePath
				}
				if line == 0 {
					line = n.StartLine
				}
			}
			row = &ThrowerErrorRow{ThrowerID: e.From, FilePath: file, Line: line}
			byThrower[e.From] = row
		}
		row.Throws++
		row.ErrorTargets = addUnique(row.ErrorTargets, e.To)
	}
	for thrower, row := range byThrower {
		for _, e := range g.GetOutEdges(thrower) {
			if e == nil || e.Kind != EdgeEmits {
				continue
			}
			n := g.GetNode(e.To)
			if n == nil || n.Kind != KindString {
				continue
			}
			ctxLabel, _ := n.Meta["context"].(string)
			if ctxLabel != "error_msg" {
				continue
			}
			row.ErrorMsgs = addUnique(row.ErrorMsgs, n.Name)
		}
	}
	out := make([]ThrowerErrorRow, 0, len(byThrower))
	for _, r := range byThrower {
		out = append(out, *r)
	}
	return out
}
