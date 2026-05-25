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
	GetFileNodes(filePath string) []*Node
	GetRepoNodes(repoPrefix string) []*Node

	// --- Edge adjacency --------------------------------------------

	GetOutEdges(nodeID string) []*Edge
	GetInEdges(nodeID string) []*Edge

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
	// instead cut a 503-second sqlite resolver pass on a 122k-node
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
	// fine (map lookups, nanoseconds). On sqlite each prepared-stmt
	// Exec through modernc.org/sqlite costs ~1-5 ms — at 100k+ calls
	// the per-pass cost is hundreds of seconds, dominating the
	// resolver. The batched variants collapse those into one (or
	// chunked) bulk query.

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
//   - BeginBulkLoad must be called on an empty store (NodeCount == 0,
//     EdgeCount == 0). Calling it on a non-empty store is a programmer
//     error; backends are free to refuse or no-op.
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
//   - UpsertSymbolFTS is called by the indexer for every node that
//     should be searchable. The store decides how to persist the
//     pre-tokenised text (a sidecar table, an FTS column, an
//     in-engine index — backend choice). Tokens are produced by
//     internal/search.Tokenize so camelCase / snake_case / path-
//     separator semantics match the existing BM25 corpus contract.
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
//     internal/search.TokenizeQuery before invoking the FTS).
//
//   - Close is implied by graph.Store.Close — no separate
//     teardown method here.
type SymbolSearcher interface {
	UpsertSymbolFTS(nodeID, tokens string) error
	BuildSymbolIndex() error
	SearchSymbols(query string, limit int) ([]SymbolHit, error)
}
