package graph

import (
	"hash/fnv"
	"sync"
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

// shard is a fragment of the graph's data. Each shard holds the node
// metadata for the subset of IDs that hash to it, plus the outgoing
// edges whose source ID is in this shard and the incoming edges whose
// target ID is in this shard. Secondary indexes (byName, byFile, etc.)
// inside a shard only contain nodes owned by that shard; aggregate
// queries iterate every shard and concatenate.
type shard struct {
	mu       sync.RWMutex
	nodes    map[string]*Node
	outEdges map[string][]*Edge
	inEdges  map[string][]*Edge
	byFile   map[string][]*Node
	byName   map[string][]*Node
	byQual   map[string]*Node
	byRepo   map[string][]*Node // repoPrefix → nodes owned by this shard
}

func newShard() *shard {
	return &shard{
		nodes:    make(map[string]*Node),
		outEdges: make(map[string][]*Edge),
		inEdges:  make(map[string][]*Edge),
		byFile:   make(map[string][]*Node),
		byName:   make(map[string][]*Node),
		byQual:   make(map[string]*Node),
		byRepo:   make(map[string][]*Node),
	}
}

// Graph is a thread-safe in-memory knowledge graph. Internally sharded
// by node-ID hash so writers touching disjoint IDs run in parallel.
type Graph struct {
	shards [numShards]*shard
}

// New creates an empty graph.
func New() *Graph {
	g := &Graph{}
	for i := range g.shards {
		g.shards[i] = newShard()
	}
	return g
}

// shardIdx picks the shard index for an ID using FNV-1a. Stable across
// restarts but that doesn't matter — the hash is recomputed every call.
func shardIdx(id string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % numShards)
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

// AddNode inserts a node into the graph and all indexes.
func (g *Graph) AddNode(n *Node) {
	s := g.shardFor(n.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n
	s.byFile[n.FilePath] = append(s.byFile[n.FilePath], n)
	s.byName[n.Name] = append(s.byName[n.Name], n)
	if n.QualName != "" {
		s.byQual[n.QualName] = n
	}
	if n.RepoPrefix != "" {
		s.byRepo[n.RepoPrefix] = append(s.byRepo[n.RepoPrefix], n)
	}
}

// AddEdge inserts a directed edge into the graph. Locks both the From
// and To shards (same shard locked once if they collide) so outEdges
// and inEdges stay consistent.
func (g *Graph) AddEdge(e *Edge) {
	unlock := g.lockTwoWrite(e.From, e.To)
	defer unlock()
	g.shardFor(e.From).outEdges[e.From] = append(g.shardFor(e.From).outEdges[e.From], e)
	g.shardFor(e.To).inEdges[e.To] = append(g.shardFor(e.To).inEdges[e.To], e)
}

// ReindexEdge updates the inEdges index after an edge's To field has
// been mutated (e.g., by the resolver changing "unresolved::X" to a
// real target). oldTo is the previous value of e.To before mutation.
func (g *Graph) ReindexEdge(e *Edge, oldTo string) {
	if oldTo == e.To {
		return
	}
	// The outEdges entry doesn't move (source ID is unchanged); only
	// the inEdges slot shifts from oldTo → e.To.
	unlock := g.lockTwoWrite(oldTo, e.To)
	defer unlock()

	sOld := g.shardFor(oldTo)
	inList := sOld.inEdges[oldTo]
	for i, ie := range inList {
		if ie == e {
			sOld.inEdges[oldTo] = append(inList[:i], inList[i+1:]...)
			if len(sOld.inEdges[oldTo]) == 0 {
				delete(sOld.inEdges, oldTo)
			}
			break
		}
	}

	sNew := g.shardFor(e.To)
	sNew.inEdges[e.To] = append(sNew.inEdges[e.To], e)
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
func (g *Graph) FindNodesByName(name string) []*Node {
	var out []*Node
	for _, s := range g.shards {
		s.mu.RLock()
		if src := s.byName[name]; len(src) > 0 {
			out = append(out, src...)
		}
		s.mu.RUnlock()
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
	evictedIDs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		evictedIDs[n.ID] = true
	}

	for _, n := range nodes {
		s := g.shardFor(n.ID)
		delete(s.nodes, n.ID)
		if n.QualName != "" {
			delete(s.byQual, n.QualName)
		}
		s.byName[n.Name] = removeNode(s.byName[n.Name], n.ID)
		if len(s.byName[n.Name]) == 0 {
			delete(s.byName, n.Name)
		}
		if n.RepoPrefix != "" {
			s.byRepo[n.RepoPrefix] = removeNode(s.byRepo[n.RepoPrefix], n.ID)
			if len(s.byRepo[n.RepoPrefix]) == 0 {
				delete(s.byRepo, n.RepoPrefix)
			}
		}
	}
	// Clear the file's byFile entry in every shard (only the ones that
	// contained these nodes will actually have entries, but Go map
	// delete on a missing key is a no-op).
	for _, s := range g.shards {
		delete(s.byFile, filePath)
	}
	nodesRemoved = len(nodes)

	edgesRemoved = g.evictEdgesLocked(evictedIDs)
	return nodesRemoved, edgesRemoved
}

// evictEdgesLocked is the shared edge-removal core used by EvictFile
// and EvictRepo. Callers must hold every shard's write lock.
//
// For each evicted node we remove its outEdges and inEdges entries. To
// clean the reverse index on non-evicted endpoints we flip-look across
// shards using shardFor.
func (g *Graph) evictEdgesLocked(evictedIDs map[string]bool) int {
	removed := 0

	// Phase 1: remove outgoing edges from every evicted node.
	for id := range evictedIDs {
		s := g.shardFor(id)
		edges := s.outEdges[id]
		removed += len(edges)
		for _, e := range edges {
			if !evictedIDs[e.To] {
				sTo := g.shardFor(e.To)
				sTo.inEdges[e.To] = filterEdge(sTo.inEdges[e.To], e)
				if len(sTo.inEdges[e.To]) == 0 {
					delete(sTo.inEdges, e.To)
				}
			}
		}
		delete(s.outEdges, id)
	}

	// Phase 2: remove incoming edges to every evicted node (from
	// non-evicted sources — same-direction edges were already handled
	// in phase 1 and counted).
	for id := range evictedIDs {
		s := g.shardFor(id)
		edges := s.inEdges[id]
		for _, e := range edges {
			if !evictedIDs[e.From] {
				removed++
				sFrom := g.shardFor(e.From)
				sFrom.outEdges[e.From] = filterEdge(sFrom.outEdges[e.From], e)
				if len(sFrom.outEdges[e.From]) == 0 {
					delete(sFrom.outEdges, e.From)
				}
			}
		}
		delete(s.inEdges, id)
	}

	return removed
}

// RemoveEdge removes a specific edge by from, to, and kind. Returns
// true if the edge was found and removed.
func (g *Graph) RemoveEdge(from, to string, kind EdgeKind) bool {
	unlock := g.lockTwoWrite(from, to)
	defer unlock()

	sFrom := g.shardFor(from)
	outList := sFrom.outEdges[from]
	var target *Edge
	for _, e := range outList {
		if e.To == to && e.Kind == kind {
			target = e
			break
		}
	}
	if target == nil {
		return false
	}

	sFrom.outEdges[from] = filterEdge(sFrom.outEdges[from], target)
	if len(sFrom.outEdges[from]) == 0 {
		delete(sFrom.outEdges, from)
	}

	sTo := g.shardFor(to)
	sTo.inEdges[to] = filterEdge(sTo.inEdges[to], target)
	if len(sTo.inEdges[to]) == 0 {
		delete(sTo.inEdges, to)
	}

	return true
}

// filterEdge removes a specific edge pointer from a slice.
func filterEdge(edges []*Edge, target *Edge) []*Edge {
	for i, e := range edges {
		if e == target {
			return append(edges[:i], edges[i+1:]...)
		}
	}
	return edges
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

// AllEdges returns a snapshot of all edges.
func (g *Graph) AllEdges() []*Edge {
	g.lockAllRead()
	defer g.unlockAllRead()
	var out []*Edge
	for _, s := range g.shards {
		for _, edges := range s.outEdges {
			out = append(out, edges...)
		}
	}
	return out
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
	evictedIDs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		evictedIDs[n.ID] = true
	}

	for _, n := range nodes {
		s := g.shardFor(n.ID)
		delete(s.nodes, n.ID)
		if n.QualName != "" {
			delete(s.byQual, n.QualName)
		}
		s.byName[n.Name] = removeNode(s.byName[n.Name], n.ID)
		if len(s.byName[n.Name]) == 0 {
			delete(s.byName, n.Name)
		}
		s.byFile[n.FilePath] = removeNode(s.byFile[n.FilePath], n.ID)
		if len(s.byFile[n.FilePath]) == 0 {
			delete(s.byFile, n.FilePath)
		}
	}
	for _, s := range g.shards {
		delete(s.byRepo, repoPrefix)
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

// per-node fixed overhead: the struct header, the pointer from the
// byRepo/byFile/byName/byQual slices/maps, and the typical allocator
// padding. Tuned against runtime.ReadMemStats deltas on a 50k-node
// repo; within ~10%.
const nodeStructOverhead = 160

// per-edge fixed overhead: two string pointers, kind, filepath, line,
// plus slice-header and adjacency-map amortisation.
const edgeStructOverhead = 120

// RepoMemoryEstimate walks the per-repo partition and sums node and
// edge byte estimates. Approximate but cheap (O(repo size) with one
// read lock across all shards).
func (g *Graph) RepoMemoryEstimate(repoPrefix string) RepoMemoryEstimate {
	g.lockAllRead()
	defer g.unlockAllRead()

	var est RepoMemoryEstimate
	evictedIDs := make(map[string]struct{})
	for _, s := range g.shards {
		nodes := s.byRepo[repoPrefix]
		for _, n := range nodes {
			est.NodeBytes += nodeBytes(n)
			est.NodeCount++
			evictedIDs[n.ID] = struct{}{}
		}
	}
	// Edges whose source lives in this repo. Same accounting rule as
	// RepoStats so the numbers stay consistent.
	for _, s := range g.shards {
		for srcID, edges := range s.outEdges {
			if _, ok := evictedIDs[srcID]; !ok {
				continue
			}
			for _, e := range edges {
				est.EdgeBytes += edgeBytes(e)
				est.EdgeCount++
			}
		}
	}
	return est
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

func removeNode(nodes []*Node, id string) []*Node {
	for i, n := range nodes {
		if n.ID == id {
			return append(nodes[:i], nodes[i+1:]...)
		}
	}
	return nodes
}
