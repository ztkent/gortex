package graph

import (
	"iter"
	"sync"
)

// EdgeReindex is the per-edge payload for ReindexEdges. Edge points
// at the (already mutated) Edge value the caller wants the store to
// re-bind; OldTo is the To target the edge had BEFORE the mutation,
// so the store can drop the stale in-edge index entry for OldTo
// while writing the new one for Edge.To.
type EdgeReindex struct {
	Edge  *Edge
	OldTo string
}

// EdgeProvenanceUpdate is the per-edge payload for
// SetEdgeProvenanceBatch. Edge points at the stored Edge whose
// origin should be promoted; NewOrigin is the target tier. The store
// only persists the change (and bumps EdgeIdentityRevisions) when
// NewOrigin differs from the currently stored Origin.
type EdgeProvenanceUpdate struct {
	Edge      *Edge
	NewOrigin string
}

// Store is the persistence-and-query backend the rest of gortex sees
// behind the *Graph type. The only implementation today is the
// in-memory *Graph; future implementations will include an on-disk
// embedded-DB backend (local single-binary) and a remote network
// client. The interface is the seam that lets the rest of the
// codebase be backend-agnostic.
//
// The method set deliberately mirrors *Graph's current public API so
// the codebase compiles unchanged the day this interface lands. A few
// notes on shape:
//
//   - Slice-shaped reads (AllNodes / AllEdges / FindNodesByName / …)
//     materialise their result in memory — fine for the in-memory
//     store, but disk / remote backends will want iterator-shaped
//     variants added alongside as those implementations come online.
//
//   - Memory-estimate methods (RepoMemoryEstimate /
//     AllRepoMemoryEstimates) are inherently in-memory specific; disk
//     and remote backends return whatever they can compute and callers
//     treat the result as advisory.
//
//   - ResolveMutex() returns a backend-owned mutex that resolver
//     instances (cross-repo, temporal, external) share to serialise
//     their edge-mutation passes against each other and against the
//     indexer's incremental rewrites. Every backend needs equivalent
//     coordination; the in-memory store uses its existing
//     graph-wide resolveMu, disk backends keep a dedicated mutex
//     alongside their own write serialisation. The returned pointer
//     is owned by the store and must not be Unlocked when not held.
type Store interface {
	// --- Writes -----------------------------------------------------

	AddNode(n *Node)
	AddBatch(nodes []*Node, edges []*Edge)
	AddEdge(e *Edge)
	SetEdgeProvenance(e *Edge, newOrigin string) bool
	ReindexEdge(e *Edge, oldTo string)
	// Batched siblings of the per-edge mutators. Same semantics, but
	// disk backends amortise the per-call transaction overhead by
	// committing in implementation-chosen chunks (the in-memory
	// backend just loops). The resolver fans out per-edge mutations
	// across thousands of edges in a single ResolveAll pass, so the
	// per-call form was unusable on disk backends without these.
	// Callers MUST first mutate the *Edge fields they want persisted
	// (To / Kind / Origin / …) before handing the entry over — these
	// methods read the post-mutation Edge state and update the
	// backend's indexes accordingly.
	ReindexEdges(batch []EdgeReindex)
	SetEdgeProvenanceBatch(batch []EdgeProvenanceUpdate) (changed int)
	RemoveEdge(from, to string, kind EdgeKind) bool
	EvictFile(filePath string) (nodesRemoved, edgesRemoved int)
	EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int)

	// --- Point lookups ---------------------------------------------

	GetNode(id string) *Node
	GetNodeByQualName(qualName string) *Node

	// --- Name + scope queries --------------------------------------

	FindNodesByName(name string) []*Node
	FindNodesByNameInRepo(name, repoPrefix string) []*Node
	// FindNodesByNameContaining returns nodes whose Name (case-
	// insensitive) contains the given substring. The implementation
	// pushes the filter into the backend so only matching rows cross
	// the cgo boundary — the old search-substring fallback's
	// AllNodes()-then-filter pattern materialised the whole node set
	// per query and breaks at Linux-kernel scale (10M+ symbols).
	// limit caps the result set so a very common substring can't blow
	// up memory; pass 0 for "no limit" (caller's responsibility to
	// handle). The order is implementation-defined — callers that
	// need deterministic output sort the result.
	FindNodesByNameContaining(substr string, limit int) []*Node
	GetFileNodes(filePath string) []*Node
	GetRepoNodes(repoPrefix string) []*Node

	// --- Edge adjacency --------------------------------------------

	GetOutEdges(nodeID string) []*Edge
	GetInEdges(nodeID string) []*Edge

	// GetInEdgesByNodeIDs / GetOutEdgesByNodeIDs batch the per-node
	// edge fan-out into a single backend round-trip. The rerank
	// pipeline calls these once per Rerank() to materialise every
	// candidate's incoming + outgoing edges in two cgo round-trips
	// instead of 6N per-candidate calls. Missing IDs are absent from
	// the returned map (callers can index without an ok-check via the
	// nil-slice semantics of map[k][]*Edge — range over nil is a no-op).
	GetInEdgesByNodeIDs(ids []string) map[string][]*Edge
	GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge

	// GetRepoEdges returns every edge whose source node has the given
	// RepoPrefix. Equivalent to GetRepoNodes(r) followed by
	// GetOutEdges(n.ID) for every n, but executes as a single backend
	// query — critical on disk backends (Ladybug, SQLite, DuckDB)
	// where the per-node loop is O(repo_nodes) round-trips. The
	// in-memory backend forwards to that same nested walk; the disk
	// backends push the join into one server-side query.
	//
	// Empty repoPrefix returns nothing — use AllEdges() for the
	// global view. Nodes with an empty RepoPrefix are unreachable
	// through this method by design (they don't belong to any repo).
	GetRepoEdges(repoPrefix string) []*Edge

	// --- Bulk reads ------------------------------------------------

	AllNodes() []*Node
	AllEdges() []*Edge

	// --- Predicate-shaped reads (push filters into the store) ------
	//
	// These methods replace the pre-Store idiom of `for _, e := range
	// AllEdges() { if cond { ... } }`. On the in-memory backend they
	// iterate the existing internal byKind / byPrefix buckets — same
	// algorithmic cost as the inline filter. On disk backends they
	// fan out to dedicated indexes (idx_edge_kind / idx_node_kind /
	// the to_id LIKE prefix scan, etc.) so the row count actually
	// materialised is proportional to the predicate match, not the
	// whole table.
	//
	// The resolver alone calls AllEdges/AllNodes 34× per pass and
	// throws away >99% of each scan; using these predicate methods
	// instead cut a 503-second disk-backed resolver pass on a 122k-node
	// graph down to seconds.
	//
	// Iterators stop when the consumer's yield returns false.
	// Implementations MUST honour early-stop so callers can break
	// out of a search.

	// EdgesByKind yields every edge whose Kind matches.
	EdgesByKind(kind EdgeKind) iter.Seq[*Edge]

	// NodesByKind yields every node whose Kind matches.
	NodesByKind(kind NodeKind) iter.Seq[*Node]

	// EdgesWithUnresolvedTarget yields every edge whose To has the
	// "unresolved::" prefix. The resolver's main loop calls this
	// once per pass; on disk backends it should range-scan a
	// to-keyed index over the single contiguous "unresolved::" slice
	// rather than materialise the whole edges table.
	EdgesWithUnresolvedTarget() iter.Seq[*Edge]

	// --- Batched point lookups -------------------------------------
	//
	// The resolver fires ~3-10 GetNode / FindNodesByName calls per
	// unresolved edge across its workers. With 10-30k pending edges
	// that's 100k-300k individual queries. On in-memory that's
	// fine (map lookups, nanoseconds). On a disk backend each point
	// lookup is ~ms — at 100k+ calls the per-pass cost is hundreds
	// of seconds, dominating the resolver. The batched variants
	// collapse those into one (or chunked) bulk query.

	// GetNodesByIDs returns a map id→*Node for every input ID present
	// in the store. IDs not in the store are simply absent from the
	// returned map (no nil values). Callers may pass duplicates; the
	// returned map dedupes naturally.
	GetNodesByIDs(ids []string) map[string]*Node

	// FindNodesByNames returns a map name→[]*Node where each slot
	// holds every node whose Name field matches. Names that match no
	// node are absent. Used by the resolver to pre-warm its name-only
	// fallback lookup across the whole pending-edge slice in one
	// batched call instead of one query per edge.
	FindNodesByNames(names []string) map[string][]*Node

	// --- Counts and stats ------------------------------------------

	NodeCount() int
	EdgeCount() int
	Stats() GraphStats
	RepoStats() map[string]GraphStats
	RepoPrefixes() []string

	// --- Provenance verification -----------------------------------

	EdgeIdentityRevisions() int
	VerifyEdgeIdentities() error

	// --- Memory estimation (advisory; in-memory-specific) ----------

	RepoMemoryEstimate(repoPrefix string) RepoMemoryEstimate
	AllRepoMemoryEstimates() map[string]RepoMemoryEstimate

	// --- Coordination ----------------------------------------------

	// ResolveMutex returns a backend-owned mutex resolver instances
	// share to serialise edge-mutation passes. See the package doc
	// above for the full contract.
	ResolveMutex() *sync.Mutex
}

// Compile-time assertion: *Graph satisfies the Store interface. If a
// *Graph method's signature ever drifts from the interface, the build
// fails fast here instead of at runtime when a different Store
// implementation gets swapped in.
var _ Store = (*Graph)(nil)

// BackendResolver is an optional interface backends MAY implement to
// drain the bulk-tractable subset of the resolver's work entirely
// inside the backend engine (Cypher MATCH+SET on Ladybug,
// UPDATE...FROM on DuckDB) instead of round-tripping every
// resolution decision back to Go.
//
// Sequencing matters: earlier rules are higher-precision than later
// ones. The orchestrator (ResolveAllBulk) runs them in the order
// listed below so that, e.g., an intra-file call binds to its same-
// file declaration before the unique-name pass would have bound it
// to a same-named symbol elsewhere in the repo.
//
// Each method returns the number of pending edges it drained.
// Unimplemented methods return (0, nil) and the orchestrator skips
// to the next. Errors surface as non-fatal — the orchestrator logs
// and continues with subsequent rules; the Go-side Resolver then
// picks up whatever the bulk pass didn't drain.
type BackendResolver interface {
	// ResolveSameFile: unresolved::Name where target is in the
	// caller's same source file. Strongest precision — a same-file
	// declaration is almost never ambiguous.
	ResolveSameFile() (resolved int, err error)

	// ResolveSamePackage: unresolved::Name where target is in the
	// caller's same directory (Go package). Repo_prefix must match
	// to keep the rule within one source tree.
	ResolveSamePackage() (resolved int, err error)

	// ResolveImportAware: caller's file imports F, target is a
	// symbol in F. Joins against the EdgeImports adjacency.
	ResolveImportAware() (resolved int, err error)

	// ResolveRelativeImports: unresolved::pyrel::<stem> / Dart
	// relative-URI stubs rewritten to the matching KindFile node
	// (e.g. <stem>.py or <stem>/__init__.py for Python).
	// `lang` selects the dialect; empty string runs all supported
	// dialects in turn.
	ResolveRelativeImports(lang string) (resolved int, err error)

	// ResolveCrossRepo: unresolved::Name where exactly one
	// cross-repo Node carries that name. Lower precision than the
	// same-repo rules; sets cross_repo = true on the resulting edge.
	ResolveCrossRepo() (resolved int, err error)

	// ResolveUniqueNames: unresolved::Name where exactly one Node
	// in the entire graph carries that name. Lowest-precision
	// "fallback" — runs after the same-file / same-package /
	// import-aware passes have drained anything they could resolve
	// more precisely.
	ResolveUniqueNames() (resolved int, err error)

	// ResolveExternalCallStubs: ensures every external::* edge
	// target has a corresponding Node row (the existing
	// SynthesizeExternalCalls pass on the Go side). Promotes
	// origin to ast_resolved for edges that now point at a real
	// stub.
	ResolveExternalCallStubs() (resolved int, err error)

	// ResolveAllBulk runs the bulk-tractable methods in
	// precision-descending order and returns the cumulative count
	// of edges resolved across all rules. The default backend
	// implementation should chain the methods above; callers use
	// ResolveAllBulk as the single Resolver-side hook.
	ResolveAllBulk() (totalResolved int, err error)
}

// BulkLoader is an optional interface backends MAY implement to expose
// a high-throughput cold-load fast path that bypasses per-call query
// overhead. The cold-start indexer fires ~2000 small AddBatch calls
// during its parse phase; on backends where every AddBatch round-trips
// through a query parser (Ladybug, DuckDB) that per-call cost
// dominates wall time. BulkLoader lets the indexer bracket the parse
// loop with BeginBulkLoad / FlushBulk: AddBatch calls inside the
// bracket buffer rows in memory, and FlushBulk commits them through
// the backend's native bulk primitive (Ladybug's COPY FROM,
// DuckDB's long-lived Appender).
//
// Contract:
//
//   - BeginBulkLoad may be called on a non-empty store. The cold-start
//     parse phase calls it on an empty store, but later passes (notably
//     the contracts pass, which appends a few hundred contract nodes /
//     edges after resolve) re-enter the bracket against a populated
//     backend. FlushBulk commits via the backend's native bulk
//     primitive in MERGE-on-primary-key mode, so re-appending rows
//     that share an ID with existing data does not duplicate them.
//
//   - Between BeginBulkLoad and FlushBulk, AddBatch is the only mutator
//     the caller may invoke. Reads (GetNode, AllEdges, EdgesByKind, …)
//     return whatever the backend can see — typically nothing buffered.
//     The resolver MUST NOT run until after FlushBulk.
//
//   - FlushBulk commits everything buffered since BeginBulkLoad and
//     returns the backend to normal per-call write mode. An error
//     leaves the store in an implementation-defined state.
//
//   - Calling BeginBulkLoad twice without an intervening FlushBulk,
//     or calling FlushBulk without a prior BeginBulkLoad, is a
//     programmer error; backends are free to panic.
//
// The in-memory *Graph deliberately does NOT implement BulkLoader —
// it's already optimal at the per-call path. bbolt and SQLite likewise
// skip it: their per-call overhead is already amortised by their own
// internal batching (chunked transactions, prepared statements). The
// interface is intentionally opt-in so the indexer can probe with a
// type assertion and fall through to today's per-batch path uniformly.
type BulkLoader interface {
	BeginBulkLoad()
	FlushBulk() error
}

// SymbolHit is a single full-text-search result: the matched node ID
// plus its relevance score from the backend's scorer (BM25 in
// Ladybug's FTS). Higher score = more relevant.
type SymbolHit struct {
	NodeID string
	Score  float64
}

// SymbolFTSItem is the payload BulkUpsertSymbolFTS takes per node:
// the node's ID and its pre-tokenised text. Reused so the indexer
// can preallocate one slice and the backend can iterate without
// per-element wrapper allocs.
type SymbolFTSItem struct {
	NodeID string
	Tokens string
}

// SymbolSearcher is an optional interface backends MAY implement to
// expose engine-native full-text search over the graph's symbol
// names. When the backing store implements it, the daemon's
// search_symbols path routes through the backend FTS instead of
// building a parallel in-process Bleve/BM25 index — saving ~100MB
// of heap on a vscode-scale repo and putting the search latency in
// the same address space as the rest of the graph.
//
// Contract:
//
//   - UpsertSymbolFTS is the per-call write path used by incremental
//     reindex. The store decides how to persist the pre-tokenised
//     text (a sidecar table, an FTS column, an in-engine index —
//     backend choice). Tokens are produced by
//     internal/search.Tokenize so camelCase / snake_case / path-
//     separator semantics match the existing BM25 corpus contract.
//
//   - BulkUpsertSymbolFTS is the cold-start fast path used by the
//     indexer's shadow-swap drain. Implementations SHOULD use the
//     backend's native bulk primitive (TSV + COPY FROM on Ladybug)
//     so a 600k-node repo doesn't pay per-row Cypher parse cost.
//     Idempotent on NodeID like UpsertSymbolFTS — re-running with
//     an overlapping set replaces in place.
//
//   - BuildSymbolIndex finalises the index after the bulk parse
//     phase. For backends whose FTS index updates automatically on
//     row writes (Ladybug), this is a one-shot cold-start call;
//     for backends that need an explicit build pass, it's where
//     the work happens. Idempotent — safe to call multiple times.
//
//   - SearchSymbols runs a query and returns hits ordered by score
//     descending. The query string is the user's raw input; the
//     backend is expected to tokenise it the same way it tokenised
//     the indexed text (typically by passing it through
//     internal/search.Tokenize before invoking the FTS).
//
//   - Close is implied by graph.Store.Close — no separate
//     teardown method here.
type SymbolSearcher interface {
	UpsertSymbolFTS(nodeID, tokens string) error
	BulkUpsertSymbolFTS(items []SymbolFTSItem) error
	BuildSymbolIndex() error
	SearchSymbols(query string, limit int) ([]SymbolHit, error)
}

// SymbolBundle is the rerank-shaped result of one search call: the
// matched node, its BM25 score, AND the in/out edges the rerank
// pipeline reads from. Backends that can compose this in a single
// engine round-trip implement SymbolBundleSearcher; callers can fall
// through to SymbolSearcher + GetNodesByIDs + GetIn/OutEdgesByNodeIDs
// when the backend doesn't.
//
// The same node may appear in successive bundles when a multi-call
// retrieval path (primary + expansion) returns it more than once; the
// caller's dedup-by-ID step keeps the per-call shape simple and the
// engine can merge across calls into a single rerank candidate set
// without paying for the duplicate edge fetch — the second occurrence
// already carries the same edges.
type SymbolBundle struct {
	Node     *Node
	Score    float64
	InEdges  []*Edge
	OutEdges []*Edge
}

// SymbolBundleSearcher is an optional capability backends MAY
// implement to fold the symbol-search hot path's three
// per-BM25-call cgo round-trips (FTS + GetNodesByIDs + the rerank
// prepare's batched in/out edge fetch) into one bundled
// engine-side call:
//
//   - FTS yields (id, score)
//   - One batched node materialise + one in-edge fan-in + one
//     out-edge fan-out, all keyed on the same id list, return the
//     bundle.
//
// Backends that do NOT implement this interface still serve the
// search path through SymbolSearcher; callers fall back to
// SymbolSearcher.SearchSymbols + GetNodesByIDs +
// GetIn/OutEdgesByNodeIDs and pay the per-call cgo cost the
// bundled form avoids. The contract is intentionally read-only —
// writes still go through UpsertSymbolFTS / BulkUpsertSymbolFTS on
// the SymbolSearcher.
//
// Today the Ladybug backend implements this via four cypher calls
// (FTS → IDs, then a node batch + an outgoing-edge batch + an
// inbound-edge batch on those IDs). A single combined Cypher with
// OPTIONAL MATCH + collect() is slower in practice — the
// cross-product Kuzu builds across the two OPTIONAL MATCH +
// collect frames outweighs the cgo saving (probe: 150ms median vs
// the 4-query split's 68ms median on the same id set).
type SymbolBundleSearcher interface {
	SearchSymbolBundles(query string, limit int) ([]SymbolBundle, error)
}

// VectorItem is the payload BulkUpsertEmbeddings takes per node:
// the node's ID and its embedding vector. Length of Vec must
// match the dim the corresponding BuildVectorIndex call declared
// — backends with fixed-width vector columns (Ladybug's
// FLOAT[N]) reject inserts that don't match.
type VectorItem struct {
	NodeID string
	Vec    []float32
}
// VectorHit is a single ANN search result: the matched node ID
// plus its distance to the query vector under the backend's
// metric (cosine by default in Ladybug). LOWER distance = more
// similar. Callers that need a similarity score in [0,1] should
// translate via `1 - distance` for cosine.
type VectorHit struct {
	NodeID   string
	Distance float64
}

// VectorSearcher is an optional interface backends MAY implement to
// expose engine-native HNSW vector indexing over per-symbol
// embedding vectors. When the backing store implements it, the
// daemon's semantic-search path routes through the backend's
// native ANN index instead of holding a parallel in-process
// HNSW — saving roughly `dim × 4 × N` bytes of heap (≈ 1 GB for
// 384-dim × 663k symbols on a Vscode-scale repo).
//
// The bigger win — and the reason Option B exists alongside
// Option C in the storage-engine roadmap — is that vector
// neighbours and graph traversal can be combined in a single
// Cypher round-trip:
//
//	CALL QUERY_VECTOR_INDEX('SymbolVec', 'idx_emb', $vec, 50)
//	  YIELD node AS seed
//	MATCH (seed)<-[:calls]-(caller:KindFunction)
//	WHERE caller.RepoPrefix = $repo AND NOT caller.id CONTAINS '_test'
//	RETURN seed.name, caller.name
//
// Today this query is three round-trips on the in-process HNSW
// path (ANN → IDs → graph fetch → Go-side filter); with
// VectorSearcher it's one engine-vectorised pipeline.
//
// Contract:
//
//   - UpsertEmbedding is the per-call write path used by
//     incremental reindex when one file's embeddings change.
//
//   - BulkUpsertEmbeddings is the cold-start fast path used by
//     the indexer's embedding pass. Implementations SHOULD use
//     the backend's native bulk primitive (TSV + COPY FROM on
//     Ladybug) so a 600k-node corpus doesn't pay per-row Cypher
//     parse cost. Idempotent on NodeID — re-running with an
//     overlapping set replaces in place.
//
//   - BuildVectorIndex finalises the HNSW index after the bulk
//     populate. The dim parameter declares the embedding
//     width; backends with fixed-width columns lazily create
//     the storage schema on the first BuildVectorIndex call.
//     Idempotent — safe to call multiple times with the same dim.
//
//   - SimilarTo runs an ANN query: given a vector, return the k
//     closest stored vectors ordered by ascending distance.
//
//   - Close is implied by graph.Store.Close — no separate
//     teardown method here.
type VectorSearcher interface {
	UpsertEmbedding(nodeID string, vec []float32) error
	BulkUpsertEmbeddings(items []VectorItem) error
	BuildVectorIndex(dims int) error
	SimilarTo(vec []float32, limit int) ([]VectorHit, error)
}

// PageRankOpts tunes the PageRank computation. Zero values request
// the backend default — only set fields you genuinely want to
// override so backends can pick their own parallel-tuned defaults
// without the caller second-guessing the constants.
//
// NodeKinds / EdgeKinds restrict the projected subgraph the
// algorithm runs over. Empty means "all kinds" — the algo sees the
// full graph. A non-empty filter is rewritten into the projected-
// graph predicate (Ladybug supports per-table predicates of the
// form 'n.kind = "function"').
type PageRankOpts struct {
	NodeKinds      []NodeKind
	EdgeKinds      []EdgeKind
	DampingFactor  float64
	MaxIterations  int
	Tolerance      float64
	Limit          int // 0 = return every ranked node
}

// PageRankHit is one row of the PageRank output: the node ID plus
// its rank score. Hits come back sorted by rank descending.
type PageRankHit struct {
	NodeID string
	Rank   float64
}

// PageRanker is an optional interface backends MAY implement to
// expose engine-native PageRank centrality. When the store
// implements it, the daemon's hotspot / authority-ranking path
// routes through the backend's parallel implementation (Ligra-
// based on Ladybug) instead of computing degree-centrality
// in-process.
//
// Engine-native PageRank is qualitatively different from the
// degree-based hotspot analyzer: random-walk authority weights
// rare-but-influential nodes the degree count would miss
// (a low-fan-in API that's called from every domain layer ranks
// higher than a high-fan-in test helper).
//
// Contract:
//
//   - PageRank runs the algorithm against a projected subgraph and
//     returns hits sorted by rank descending. The projection is
//     declared and torn down per call — callers don't manage
//     PROJECT_GRAPH lifecycle directly.
//
//   - The score is normalized so the full corpus sums to 1
//     (Ladybug's default). Relative ordering — not the absolute
//     value — is what callers should consume.
//
//   - Close is implied by graph.Store.Close.
type PageRanker interface {
	PageRank(opts PageRankOpts) ([]PageRankHit, error)
}

// CommunityOpts tunes Louvain community detection over a projected
// subgraph. Zero values request the backend default
// (maxPhases=20, maxIterations=20 on Ladybug). NodeKinds / EdgeKinds
// restrict the projection; an empty filter runs over the full graph.
type CommunityOpts struct {
	NodeKinds     []NodeKind
	EdgeKinds     []EdgeKind
	MaxPhases     int
	MaxIterations int
}

// CommunityHit is one row of the Louvain output: the node ID plus
// the integer community label the algorithm assigned. Two nodes
// with the same CommunityID are in the same community; the actual
// integer is opaque (Ladybug uses internal node offsets and
// promises no stability across runs).
type CommunityHit struct {
	NodeID      string
	CommunityID int64
}

// CommunityDetector is an optional interface backends MAY
// implement to expose engine-native Louvain community detection
// (Ladybug uses a parallel Grappolo implementation). When the
// store implements it, the daemon's analysis.DetectCommunitiesLouvain
// path can delegate the partitioning step and keep the existing
// post-processing (label disambiguation, hub detection, cohesion,
// parent assignment).
//
// Contract:
//
//   - Louvain runs the algorithm against a projected subgraph and
//     returns one hit per node assigning it to a community. The
//     projection is declared and torn down per call.
//
//   - Ladybug's implementation treats edges as undirected (the
//     modularity score is computed on the undirected graph even
//     though the projected Edge table is directed). Callers that
//     care about directed modularity should consult the in-process
//     fallback.
//
//   - Close is implied by graph.Store.Close.
type CommunityDetector interface {
	Louvain(opts CommunityOpts) ([]CommunityHit, error)
}

// ComponentOpts tunes connected-component computation over a
// projected subgraph. Zero values request the backend default
// (maxIterations=100 on Ladybug). NodeKinds / EdgeKinds restrict
// the projection.
type ComponentOpts struct {
	NodeKinds     []NodeKind
	EdgeKinds     []EdgeKind
	MaxIterations int
}

// ComponentHit is one row of a connected-component output: the
// node ID plus the integer component label the algorithm assigned.
// Two nodes with the same ComponentID are in the same component.
// The integer is opaque (Ladybug uses internal node offsets).
type ComponentHit struct {
	NodeID      string
	ComponentID int64
}

// ComponentFinder is an optional interface backends MAY implement
// to expose engine-native weakly- and strongly-connected-component
// algorithms. Two methods because the algorithms answer different
// questions:
//
//   - WeaklyConnectedComponents treats edges as undirected — every
//     pair of nodes reachable from each other (ignoring direction)
//     lands in one component. Useful for "is this symbol part of
//     the connected core?" diagnostics.
//
//   - StronglyConnectedComponents respects edge direction — only
//     nodes mutually reachable end up in the same component. The
//     SCC of a call graph is the cycle structure: every non-
//     trivial SCC (size > 1) is a mutual-recursion ring.
//
// When the store implements ComponentFinder, the daemon's
// connectivity diagnostics and circular-dependency detection
// (`analyze kind=wcc` / `analyze kind=scc`) route through it;
// otherwise the in-process analysis.ComputeWCC / analysis.ComputeSCC
// fallbacks run.
type ComponentFinder interface {
	WeaklyConnectedComponents(opts ComponentOpts) ([]ComponentHit, error)
	StronglyConnectedComponents(opts ComponentOpts) ([]ComponentHit, error)
}

// KCoreOpts tunes k-core decomposition. NodeKinds / EdgeKinds
// restrict the projection. The algorithm itself takes no
// per-call parameters — it always computes the full
// decomposition (every node gets its k-degree).
type KCoreOpts struct {
	NodeKinds []NodeKind
	EdgeKinds []EdgeKind
}

// KCoreHit is one row of the k-core output: the node ID plus the
// largest k for which the node remains in the k-core after
// iteratively pruning nodes with degree < k. A node's KDegree is
// its position in the core hierarchy — high values mean the node
// sits inside a densely connected centre.
type KCoreHit struct {
	NodeID  string
	KDegree int64
}

// KCorer is an optional interface backends MAY implement to
// expose engine-native k-core decomposition. When the store
// implements it, the daemon's `analyze kind=kcore` path delegates
// to the engine-native implementation; otherwise
// analysis.ComputeKCore runs in-process.
//
// k-core finds the densest subgraph: the k-core of a graph is
// the largest subgraph where every node has at least k
// neighbours. The k-degree of a node is the largest k for which
// it stays in the k-core — useful for "find the hub-of-hubs", or
// "what's the core infrastructure code that everything depends
// on".
type KCorer interface {
	KCoreDecomposition(opts KCoreOpts) ([]KCoreHit, error)
}

// DeadCodeCandidator is an optional capability backends MAY implement
// to compute the dead-code candidate set server-side. The default Go
// path in analysis.FindDeadCode pulls every node + a batched in-edge
// map and filters in Go; on disk backends (Ladybug) that's
// ~1.3M edge rows over cgo per call. A backend that implements
// DeadCodeCandidator runs the equivalent WHERE-NOT-EXISTS filter
// inside the query engine and returns ~hundreds of true candidates,
// skipping the materialise-then-filter loop entirely.
//
// The opts mirror analysis.FindDeadCodeOptions to keep the surface
// in sync — only the fields the backend can act on (kinds + the
// per-kind in-edge allowlist) are honoured. File-path / build-tag
// / well-known-name exclusions stay in Go because they need
// string parsing the backend can't do efficiently.
type DeadCodeCandidator interface {
	// DeadCodeCandidates returns nodes matching the allowed node
	// kinds that have NO incoming edges of the corresponding
	// allowed in-edge kinds. The map keys the in-edge allowlist by
	// node kind — backends evaluate the right allowlist per row.
	// Empty allowedInEdgeKinds for a kind means "any incoming edge
	// counts as usage".
	DeadCodeCandidates(allowedNodeKinds []NodeKind, allowedInEdgeKinds map[NodeKind][]EdgeKind) []*Node
}

// IfaceImplementsRow is the per-row payload returned by
// IfaceImplementsScanner — one tuple per EdgeImplements edge whose
// target is a KindInterface node carrying Meta["methods"]. TypeID
// is the implementing type (the edge's source); IfaceID is the
// interface (the edge's target); IfaceMeta is the interface
// node's decoded Meta map, from which the caller pulls the
// "methods" field. Rows where the interface had no Meta are
// elided server-side.
type IfaceImplementsRow struct {
	TypeID    string
	IfaceID   string
	IfaceMeta map[string]any
}

// IfaceImplementsScanner returns the set of (typeID, interfaceID,
// interfaceMeta) tuples for every EdgeImplements edge where the
// target is a KindInterface node carrying Meta["methods"]. Used by
// analysis.FindDeadCode to compute "type implements interface, so
// these methods are alive even if never called directly". The
// server-side join is one Cypher; the Go-side equivalent fetched
// every interface node then every implements edge separately.
//
// Optional capability — analysis.FindDeadCode falls back to the
// Go-side scan when the backend doesn't implement it.
type IfaceImplementsScanner interface {
	IfaceImplementsRows() []IfaceImplementsRow
}

// NodeDegreeRow is one tuple returned by NodeDegreeAggregator. InCount
// counts EVERY incoming edge (any kind); OutCount counts EVERY outgoing
// edge; UsageInCount counts only the subset whose kind is in the
// "usage" set (Calls, References, Instantiates, Implements, Extends,
// Reads, Writes, Tests). The split exists because connectivity_health
// needs the totals (for isolated / leaf classification) AND the
// usage-edge presence (to fold ClassifyZeroEdge's logic in
// server-side); pulling them in one row saves a second cgo trip per
// node.
type NodeDegreeRow struct {
	NodeID       string
	InCount      int
	OutCount     int
	UsageInCount int
}

// NodeDegreeAggregator is an optional capability backends MAY
// implement to return per-node in/out edge counts plus a usage-edge
// count, server-side. Used by analysis.GraphConnectivity to replace
// the per-node g.GetInEdges(id) + g.GetOutEdges(id) +
// graph.ClassifyZeroEdge(id) trio — three cgo round-trips per node
// on Ladybug, three full edge materialisations per node on disk.
// One round-trip returns all three counts and lets the analyzer
// classify isolated / leaf / source-only / sink-only / extraction-gap
// without ever materialising the underlying edge structs.
//
// The usageKinds slice MUST mirror graph.usageEdgeKinds (the set
// ClassifyZeroEdge consults). Empty usageKinds means UsageInCount is
// always 0; an empty input ids slice returns nil.
//
// Optional capability — GraphConnectivity falls back to the per-node
// GetInEdges/GetOutEdges path when the backend doesn't implement it.
type NodeDegreeAggregator interface {
	NodeDegreeCounts(ids []string, usageKinds []EdgeKind) []NodeDegreeRow
}

// NodeFanRow is one tuple returned by NodeFanAggregator. FanIn counts
// incoming edges whose kind is in the fanInKinds set; FanOut counts
// outgoing edges whose kind is in the fanOutKinds set. The two kind
// sets are passed by the caller so the same capability serves both
// FindHotspots (fanIn = Calls+References, fanOut = Calls) and any
// future analyzer with a different kind split.
type NodeFanRow struct {
	NodeID string
	FanIn  int
	FanOut int
}

// NodeFanAggregator is an optional capability backends MAY implement
// to compute per-node fan-in / fan-out counts filtered by edge kind,
// server-side. Used by analysis.FindHotspots and
// handleAnalyzeHealthScore to replace the AllEdges() materialisation
// they both ran every call (~500k edges over cgo on the gortex
// workspace, the bulk of the wall-clock cost on Ladybug). The Go-side
// crossing computation still needs per-edge (from, to) for the
// Calls/References kinds — that runs through EdgesByKind, which
// streams without materialising the full edge set.
//
// Empty ids => nil; empty fanInKinds / fanOutKinds means that side
// is always 0. Output order is unspecified.
//
// Optional capability — both analyzers fall back to the AllEdges scan
// when the backend doesn't implement it.
type NodeFanAggregator interface {
	NodeFanCounts(ids []string, fanInKinds []EdgeKind, fanOutKinds []EdgeKind) []NodeFanRow
}

// FileImporterRow is the per-row payload returned by FileImporters.
// FromFile is the importing file's path (the result the caller cares
// about); FromID / FromName / FromKind describe the node that owns
// the EdgeImports edge, in case the caller needs more than just the
// file list.
type FileImporterRow struct {
	FromFile string
	FromID   string
	FromName string
	FromKind NodeKind
}

// FileImporters is an optional capability backends MAY implement to
// answer "which files import filePath?" with a single backend round-
// trip instead of a Go-side AllEdges() scan. The MCP check_references
// tool's importing-files block hammered AllEdges() per call: ~286k
// edges materialised over cgo on the gortex workspace, then a per-
// edge GetNode(e.To) + GetNode(e.From) — multiple thousand cgo round-
// trips for a single check_references call. A backend that implements
// FileImporters runs the equivalent join inside the query engine and
// only surfaces the rows that match.
//
// Match semantics mirror the original handler: an EdgeImports edge
// counts when its To node's FilePath equals filePath OR when the To
// node's ID equals filePath (the file's own node id, used by the
// indexer for file-level import bindings). The same-file dedup the
// caller applies stays in Go — backends just stream the candidate
// rows.
//
// Optional capability — handleCheckReferences falls back to the
// AllEdges-driven loop when the backend doesn't implement it.
type FileImporters interface {
	FileImporters(filePath string) []FileImporterRow
}

// InEdgeCounter is an optional capability backends MAY implement to
// compute incoming-edge fan-in counts per target node for a fixed
// set of edge kinds in one backend round-trip. The fallback iterates
// AllEdges() Go-side; on Ladybug that materialises every edge over
// cgo (~286k rows on the gortex workspace) just to bucket by To.
// The capability instead runs `MATCH ()-[e:Edge]->(n) WHERE e.kind
// IN $kinds RETURN n.id, count(*)` and ships back only the per-target
// counts — a fraction of the rows and zero per-row Go object alloc.
//
// Used by handleGetUntestedSymbols to compute the calls+references
// fan-in ranking. The map keys are node IDs; values are the integer
// count of matching incoming edges. Targets with zero matching in-
// edges are absent from the map (callers index with `m[id]` and rely
// on the zero-value default).
//
// Optional capability — the handler falls back to AllEdges-driven
// bucketing when the backend doesn't implement it.
type InEdgeCounter interface {
	InEdgeCountsByKind(kinds []EdgeKind) map[string]int
}

// NodesInFilesByKindFinder is an optional capability backends MAY
// implement to answer "which nodes of kinds K live in files F?"
// with a single backend round-trip. The fallback iterates AllNodes()
// Go-side; on Ladybug that materialises the full node table over
// cgo per call. The capability instead runs `MATCH (n:Node) WHERE
// n.file_path IN $files AND n.kind IN $kinds RETURN ...` and ships
// only the matching rows.
//
// Used by handleFindDeclaration to build the per-file enclosing-
// symbol index off the small set of trigram-match file paths. The
// Go fallback's AllNodes pull was ~70k rows on the gortex workspace
// to land at ~hundreds of relevant rows.
//
// Empty files / empty kinds returns nil — never a whole-graph scan.
//
// Optional capability — the handler falls back to AllNodes when the
// backend doesn't implement it.
type NodesInFilesByKindFinder interface {
	NodesInFilesByKind(files []string, kinds []NodeKind) []*Node
}

// EdgesByKindsScanner is an optional capability backends MAY
// implement to stream every edge whose Kind is in the supplied set,
// in a single backend round-trip. The fallback iterates AllEdges()
// Go-side and filters in process — on Ladybug AllEdges materialises
// every edge over cgo (~286k rows on the gortex workspace) for the
// edge-driven analyzers (channel_ops, pubsub, k8s_resources,
// kustomize, error_surface, …) that only care about a handful of
// kinds. The capability runs `MATCH ()-[e:Edge]->() WHERE e.kind IN
// $kinds RETURN ...` and ships back only the matching rows.
//
// The single-kind variant EdgesByKind already exists, but the
// analyzers in question typically need 2-5 kinds in one pass; firing
// EdgesByKind once per kind would issue N independent backend queries
// when the planner can naturally batch them with an IN-list. Calling
// EdgesByKinds with one kind is equivalent to EdgesByKind for that
// kind — backends should still prefer the IN-list path so the call
// site never branches on len(kinds).
//
// Empty kinds yields nothing — never a whole-table scan. Iterators
// stop when the consumer's yield returns false; implementations MUST
// honour early-stop so callers can break out of a search.
//
// Optional capability — analyzers fall back to per-kind EdgesByKind
// iteration when the backend doesn't implement it.
type EdgesByKindsScanner interface {
	EdgesByKinds(kinds []EdgeKind) iter.Seq[*Edge]
}

// NodesByKindsScanner is an optional capability backends MAY implement
// to fetch every node whose Kind is in the supplied set in a single
// backend round-trip. Replaces the AllNodes() + Go-side `if n.Kind !=
// allowed` filter used by the metadata-oriented analyze handlers
// (todos, stale_code, stale_flags, ownership, coverage_gaps,
// coverage_summary, cgo_users, wasm_users, orphan_tables,
// unreferenced_tables). Each of those scans the entire node table just
// to keep one or two kinds — on Ladybug that's ~70k rows over cgo on
// the gortex workspace per call. The capability runs
// `MATCH (n:Node) WHERE n.kind IN $kinds RETURN ...` and ships only the
// matching rows.
//
// Why a separate kinds-IN scanner instead of looping the existing
// NodesByKind iterator per kind: on Ladybug NodesByKind is one query
// per call. Looping it for {function, method} doubles the round-trip
// count and rebuilds the row decoder for each pass. One IN-list query
// returns the union directly. The dedup is intentional — duplicated
// kinds in the input never reach the IN-list, matching the in-memory
// reference's behaviour.
//
// Optional capability — handlers fall back to AllNodes-driven scanning
// when the backend doesn't implement it. Empty kinds returns nil
// without touching the backend.
type NodesByKindsScanner interface {
	NodesByKinds(kinds []NodeKind) []*Node
}

// EdgeAdjacencyForKinds is an optional capability backends MAY
// implement to stream (from, to) id pairs for every edge whose Kind
// is in the supplied edge-kind set AND whose endpoints both belong
// to the supplied node-kind set. The shape covers the betweenness /
// centrality adjacency build that today calls EdgesByKinds and
// filters Go-side: on Ladybug the per-edge row carries ~10 string
// columns over cgo, multiplied by ~286k edges on the gortex
// workspace, just for a build that uses only From/To. The
// capability returns a 2-column projection from a single Cypher
// join — every endpoint kind is enforced by the planner, so neither
// the cross-kind edges nor the irrelevant columns ever cross cgo.
//
// Empty edgeKinds or empty nodeKinds yields nothing — never a
// whole-table scan. Iterators stop when the consumer's yield
// returns false; implementations MUST honour early-stop.
//
// Optional capability — analyzers fall back to EdgesByKinds when
// the backend doesn't implement it.
type EdgeAdjacencyForKinds interface {
	EdgeAdjacencyForKinds(edgeKinds []EdgeKind, nodeKinds []NodeKind) iter.Seq[[2]string]
}

// CommunityCrossingsByKind is an optional capability backends MAY
// implement to return per-source crossing counts for edges whose
// Kind is in the supplied set, given a node→community membership
// map. A "crossing" is an edge whose source community differs from
// its target community; the count is keyed by source id.
//
// Replaces the FindHotspots.countCrossings loop that today iterates
// EdgesByKind twice and tallies per-source Go-side: on the gortex
// workspace the two EdgesByKind passes materialised the full call /
// reference bucket over cgo (~286k rows × ~10 columns) just to
// derive a thousand-row aggregate. The capability ships only the
// (from, to) projection — the community comparison runs Go-side
// because the community map isn't a Node column today.
//
// Empty kinds or an empty community map returns nil. The map keys
// in the result MUST be source ids whose count is non-zero —
// implementations MUST drop zero-count rows so callers can probe
// existence without a >0 check.
//
// Optional capability — analyzers fall back to EdgesByKind iteration
// when the backend doesn't implement it.
type CommunityCrossingsByKind interface {
	CommunityCrossingsByKind(kinds []EdgeKind, nodeToComm map[string]string) map[string]int
}

// NodeIDsByKinds is an optional capability backends MAY implement
// to return just the IDs of nodes whose Kind is in the supplied
// set. Replaces NodesByKinds in ranking paths (betweenness,
// hotspots) that only need to iterate ids — the full *Node carries
// ~10 string columns over cgo per row, and the candidate set is
// thousands of function/method rows, so the projection drops the
// per-call cgo allocation count by an order of magnitude.
//
// Empty kinds returns nil without touching the backend. Duplicated
// input kinds must NOT duplicate the output — backends MUST dedup
// the kind set in the IN-list.
//
// Optional capability — callers fall back to NodesByKinds when the
// backend doesn't implement it.
type NodeIDsByKinds interface {
	NodeIDsByKinds(kinds []NodeKind) []string
}

// EdgeKindCounter is an optional capability backends MAY implement
// to return one row per distinct edge kind with its occurrence
// count, server-side. Used by handleGetSurprisingConnections to
// derive the "rare kinds" set (kinds whose share of all edges is at
// or below the rare_kind_pct threshold) without materialising every
// edge over cgo just to bucket by Kind. On the gortex workspace the
// AllEdges() bucket pass was ~286k edges over cgo per call; the
// aggregator returns ~30 rows.
//
// The map's key is the EdgeKind; the value is the integer occurrence
// count. Empty graph returns nil (or an empty map — callers MUST
// treat both as "no rare kinds detected").
//
// Optional capability — handleGetSurprisingConnections falls back
// to the AllEdges-driven kind bucketing when the backend doesn't
// implement it.
type EdgeKindCounter interface {
	EdgeKindCounts() map[EdgeKind]int
}

// CrossRepoEdgeRow is one tuple returned by CrossRepoEdgeAggregator.
// Kind is the cross_repo_* edge kind verbatim. FromRepo / ToRepo
// are the source / target node's RepoPrefix; Count is the number of
// underlying edges that share the triple.
type CrossRepoEdgeRow struct {
	Kind     EdgeKind
	FromRepo string
	ToRepo   string
	Count    int
}

// CrossRepoEdgeAggregator is an optional capability backends MAY
// implement to return pre-grouped cross-repo edge counts. Used by
// the get_architecture handler's cross_repo rollup, which previously
// scanned AllEdges() + per-edge GetNode(from)+GetNode(to) just to
// emit one row per (kind, from_repo, to_repo). On the gortex
// workspace that meant ~286k edge rows + ~thousands of GetNode
// round-trips over cgo for typically <100 cross-repo rows. The
// aggregator runs one Cypher GROUP BY and ships only the surviving
// per-triple counts.
//
// Cross-repo edges are identified by graph.BaseKindForCrossRepo —
// the disk implementation MUST use the same kind list (so single-
// repo graphs return an empty slice, not a whole-graph scan).
//
// Optional capability — handleGetArchitecture falls back to the
// AllEdges-driven loop when the backend doesn't implement it.
type CrossRepoEdgeAggregator interface {
	CrossRepoEdgeCounts() []CrossRepoEdgeRow
}

// FileImportCountRow is one tuple returned by FileImportAggregator.
// FilePath is the imported file path (the target node's FilePath, or
// the target node's ID when the indexer pointed the import edge at
// the file node directly). Count is the number of distinct EdgeImports
// edges whose To resolves to that path.
type FileImportCountRow struct {
	FilePath string
	Count    int
}

// FileImportAggregator is an optional capability backends MAY
// implement to return per-target-file incoming-imports counts in
// one backend round-trip. Used by mostImportedFiles (shared between
// get_repo_outline and suggest_queries) which previously scanned
// AllEdges() + per-edge GetNode(to) just to bucket counts by path.
// On the gortex workspace that loop materialised ~286k edges + per-
// edge GetNode round-trips over cgo to produce a top-10 list. The
// aggregator GROUPs server-side and ships the per-file counts only.
//
// scope, when non-nil, bounds the counted edges to those whose target
// node ID lies in the slice (session-workspace clamp). An empty (but
// non-nil) scope returns nil — never a whole-graph scan. A nil scope
// means "no clamp" and counts every imports edge.
//
// Optional capability — mostImportedFiles falls back to the
// AllEdges-driven loop when the backend doesn't implement it.
type FileImportAggregator interface {
	FileImportCounts(scope []string) []FileImportCountRow
}

// InDegreeForNodes is an optional capability backends MAY implement to
// return the per-target incoming-edge count for the given node id set
// in one backend round-trip. Unlike InEdgeCounter (which filters by
// edge kind across the WHOLE graph), this counter is scoped to a
// caller-supplied id set and counts EVERY incoming edge regardless of
// kind. handleGetSurprisingConnections needs both the hub heuristic
// and the per-edge anomaly walk, but the hub check only cares about
// nodes already inside the session-scoped working set; counting every
// edge across the table just to bucket by `To` materialises the entire
// edge column (~286k rows over cgo on Ladybug).
//
// Empty ids returns nil — never a whole-table scan. Targets with zero
// matching in-edges may be absent from the returned map (callers index
// with `m[id]` and treat zero as the default).
//
// Optional capability — handleGetSurprisingConnections falls back to
// the AllEdges-driven bucketing when the backend doesn't implement it.
type InDegreeForNodes interface {
	InDegreeForNodes(ids []string) map[string]int
}

// ReachableForwardByKinds is an optional capability backends MAY
// implement to compute the set of node IDs reachable from the seed
// frontier via outgoing edges whose Kind is in the supplied set, in
// one backend round-trip. The Go fallback runs a layer-by-layer BFS
// firing GetOutEdges per node — on Ladybug that's N+1 cgo round-trips
// where N is the transitive frontier size; on a 100k-symbol repo with
// a few thousand test functions the BFS easily issues tens of
// thousands of edge fetches.
//
// reachableFromTests in handleGetUntestedSymbols is the primary
// caller: seeds are every function/method in a test file, kinds are
// {calls, references}, and the result is the closed set of symbols
// covered transitively by the test surface. The capability runs one
// variable-length match expression and ships the closure back as a
// single id list.
//
// Empty seeds returns nil; an empty kinds set returns the seed set
// unchanged (no edges to traverse). The returned map keys are the
// reachable node IDs (including the seeds); the bool value is always
// true — the shape mirrors the in-memory implementation's covered set
// so the caller's index expression stays identical.
//
// Optional capability — reachableFromTests falls back to the
// per-layer GetOutEdges BFS when the backend doesn't implement it.
type ReachableForwardByKinds interface {
	ReachableForwardByKinds(seeds []string, kinds []EdgeKind) map[string]bool
}

// ThrowerErrorRow is one tuple returned by ThrowerErrorSurfacer. ThrowerID
// is the symbol that originates the EdgeThrows edges; ErrorTargets is the
// distinct set of error-type node IDs the thrower reaches via EdgeThrows;
// ErrorMsgs is the distinct set of literal error-message strings the
// thrower emits (KindString nodes with meta.context = "error_msg", linked
// by EdgeEmits). Throws is the count of underlying EdgeThrows edges (one
// thrower may raise the same target multiple times from different sites).
// FilePath / Line are the row metadata the legacy handler propagated from
// the first edge / falling back to the thrower node — they ride here so
// the analyzer never has to issue a follow-up GetNode lookup.
type ThrowerErrorRow struct {
	ThrowerID    string
	FilePath     string
	Line         int
	Throws       int
	ErrorTargets []string
	ErrorMsgs    []string
}

// ThrowerErrorSurfacer is an optional capability backends MAY implement
// to evaluate the analyze(error_surface) rollup entirely inside the
// storage layer. The Go fallback walks EdgeThrows once for the per-
// thrower aggregation, then issues GetOutEdges per surviving thrower
// to attach the literal error-message strings. On Ladybug that's two
// scans of the edge table plus an N+1 cgo loop for the per-thrower
// emit walk; the capability runs two Cypher GROUP BYs and ships the
// pre-shaped rows back.
//
// pathPrefix narrows the EdgeThrows rows by their stored FilePath
// prefix; an empty prefix means "every thrower". Returned rows are
// already deduplicated per (thrower, error_target) and per (thrower,
// error_msg) — callers feed them directly into the analyzer's sort /
// truncate path without further bucketing.
//
// Optional capability — handleAnalyzeErrorSurface falls back to the
// AllEdges-driven loop when the backend doesn't implement it.
type ThrowerErrorSurfacer interface {
	ThrowerErrorSurface(pathPrefix string) []ThrowerErrorRow
}
