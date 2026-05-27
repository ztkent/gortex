package resolver

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

const unresolvedPrefix = "unresolved::"

// ResolveStats holds counts from a resolution pass.
type ResolveStats struct {
	Resolved   int `json:"resolved"`
	Unresolved int `json:"unresolved"`
	External   int `json:"external"`
}

// Resolver resolves unresolved edge targets to actual graph node IDs.
//
// dirIndex / lastDirIndex are scratch maps populated for the duration
// of a single ResolveAll/ResolveFile pass so resolveImport can look up
// candidate file nodes in O(1) instead of scanning the whole graph per
// import edge. On large repos (vscode ≈ 150k nodes / 5k imports) the
// old full scan made ResolveAll the dominant cost of a cold index
// (8m of a 9m wall-clock). Maps are cleared between passes.
//
// mu serializes ResolveAll and ResolveFile because both reset and
// repopulate the scratch maps as part of their first step. Without
// it, two concurrent file-watcher debounce goroutines firing on the
// same per-repo Indexer (each calls Resolver.ResolveFile via
// Indexer.IndexFile) crash the daemon with "concurrent map writes"
// in buildDirIndexes.
type Resolver struct {
	graph        graph.Store
	dirIndex     map[string][]*graph.Node
	lastDirIndex map[string][]*graph.Node
	// providesForIdx maps `provides_for: AbstractName` (from @Module
	// useClass entries) → the set of concrete class names bound to it.
	// Populated once at the start of ResolveAll; consulted in O(1) by
	// resolveMethodCall's DI-binding fallback instead of re-walking
	// graph.AllEdges per call edge. Nil outside a resolution pass and
	// empty-but-non-nil when the graph has no @Module bindings, so
	// callers can short-circuit with len().
	providesForIdx map[string]map[string]struct{}
	// reachableDirsByFile maps caller-file ID → set of directories
	// reachable from that file (own dir ∪ directories of files reached
	// via EdgeImports). Populated once at the start of ResolveAll/
	// ResolveFile; consulted by resolveMethodCall to drop candidates
	// that live in packages the caller doesn't import. Without this,
	// the name-only fallback picks an arbitrary alphabetically-first
	// candidate across the whole graph, which produced bugs like
	// `RegisterAll` resolving to `OverlayManager.Register` simply
	// because "OverlayManager" sorts before "Registry".
	reachableDirsByFile map[string]map[string]struct{}
	// depModuleIndex bridges Go imports to dep::<module> contract
	// nodes emitted from go.mod. Keyed by RepoPrefix (the dep node's
	// owning repo) so we never link an import in repo A to a dep
	// declared by repo B's go.mod. Each entry list is sorted by
	// modulePath length descending so longest-prefix wins when
	// modules nest (e.g. aws-sdk-go-v2 vs aws-sdk-go-v2/service/s3).
	// Without this index, every dep::* contract node sits in the
	// graph with zero incoming edges — go.mod records the dependency
	// but no edge points consumers at it. Built once per Resolve*
	// pass, torn down at the end.
	depModuleIndex map[string][]depModuleEntry
	// mu serialises resolution phases against the shared graph.
	// Pointer so every Resolver built from the same graph.Store
	// locks the same mutex — necessary for MultiIndexer's per-repo
	// goroutines, each of which spawns its own Resolver instance.
	// Without the shared lock, concurrent ResolveAll passes race on
	// edge mutations (resolveImport writes e.To while another
	// goroutine iterates via graph.AllEdges()).
	mu *sync.Mutex

	// lookupCache holds per-pass batched results from GetNodesByIDs /
	// FindNodesByNames. Populated by ResolveAll/ResolveFile before
	// the worker fan-out and cleared on return. Workers consult these
	// maps first; misses fall through to the underlying Store.
	//
	// Without the cache, the resolver fires ~3-10 store point lookups
	// per pending edge — across 10-30k unresolved edges that's 100k+
	// queries, each one a round trip on disk backends (~ms each).
	// With the cache the same information lands in two batched
	// queries per pass.
	nodeByID    map[string]*graph.Node
	nodesByName map[string][]*graph.Node

	// lspHelper, when non-nil, is consulted before falling back to
	// AST heuristics for cross-file dispatch in languages whose
	// helper-reported extensions match (today: TS/JS/JSX/TSX via
	// tsserver). See lsp_helper.go for the contract. Set via
	// SetLSPHelper before ResolveAll runs.
	lspHelper LSPHelper

	// lspIndex caches a (filePath, oneBasedLine) → *graph.Node
	// lookup table populated lazily on first LSP hit per pass so
	// matchNodeByLocation runs in O(1) instead of scanning every
	// node in the file. Cleared between passes.
	lspIndex   map[lspLocKey]*graph.Node
	lspIndexMu sync.RWMutex

	// npmAlias, when non-nil, rewrites a JS/TS import specifier that
	// matches an npm-alias dependency key in the importing file's
	// nearest-ancestor package.json. See npm_alias.go for the
	// contract. Set via SetNpmAliasResolver before ResolveAll runs.
	npmAlias NpmAliasResolver

	// workspaceMembers, when non-nil, maps a file path to the
	// package-manager workspace it belongs to. Used to break a
	// same-named import collision in favour of the candidate that
	// shares the importing file's workspace. See
	// workspace_membership.go for the contract. Set via
	// SetWorkspaceMembership before ResolveAll runs.
	workspaceMembers WorkspaceMembership
}

// lspLocKey identifies a node by (filePath, 1-based line) and is the
// key for lspIndex. Tsserver's textDocument/definition reports the
// declaration's start position, which graph.Node.StartLine matches.
type lspLocKey struct {
	filePath string
	line     int
}

// depModuleEntry pairs a Go module path (parsed from a dep:: contract
// node ID) with the node itself, so import-path prefix matches can
// jump straight to the target.
type depModuleEntry struct {
	modulePath string
	node       *graph.Node
}

// New creates a Resolver for the given store. The returned Resolver
// shares store.ResolveMutex() with every other Resolver built from
// the same Store, so their ResolveAll / ResolveFile calls serialise
// end-to-end across cross-repo / temporal / external passes.
func New(g graph.Store) *Resolver {
	return &Resolver{graph: g, mu: g.ResolveMutex()}
}

// SetGraph retargets the Resolver at a different Store. The indexer's
// in-memory shadow-swap path needs this: the Resolver is constructed
// against the disk Store at indexer-New time, but during IndexCtx the
// indexer reassigns its own graph pointer to an in-memory shadow.
// Without SetGraph the Resolver kept reading the (empty) disk Store
// and short-circuited on len(pending) == 0, silently disabling every
// resolver pass for backends that opt into the shadow swap.
//
// Holds the resolve mutex so a concurrent ResolveAll / ResolveFile
// can't observe a half-rotated graph reference, and switches mu to
// the new store's resolve mutex so subsequent passes serialise
// against any Resolver built directly on the new Store.
func (r *Resolver) SetGraph(g graph.Store) {
	if g == nil {
		return
	}
	oldMu := r.mu
	if oldMu != nil {
		oldMu.Lock()
	}
	r.graph = g
	r.mu = g.ResolveMutex()
	if oldMu != nil {
		oldMu.Unlock()
	}
}

// ResolveAll resolves all unresolved edges in the graph.
//
// Edge resolution is partitioned across runtime.NumCPU() workers.
// Each worker iterates a disjoint slice and calls resolveEdge, which:
//
//   - mutates only its own e.To field (per-edge ownership, no
//     write-write races between workers),
//   - reads graph state via Find/Get methods that take per-shard
//     RLocks (concurrent-safe),
//   - calls graph.ReindexEdge which acquires write locks on three
//     specific shards (e.From, oldTo, newTo) — concurrency between
//     workers serialises only on shard collisions, not globally.
//
// Stats are aggregated per-worker and summed at the end so
// `Resolved++` etc. don't race. r.mu serialises ResolveAll calls
// against each other; nothing inside this function takes that lock.
func (r *Resolver) ResolveAll() *ResolveStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buildDirIndexes()
	defer r.clearDirIndexes()
	r.buildDepModuleIndex()
	defer r.clearDepModuleIndex()
	r.buildProvidesForIndex()
	defer r.clearProvidesForIndex()
	r.buildReachabilityIndex()
	defer r.clearReachabilityIndex()
	defer r.clearLSPIndex()

	// Backend-delegated resolution: when the store implements
	// graph.BackendResolver AND the GORTEX_BACKEND_RESOLVER env var
	// is set, drain the bulk-tractable subset of the resolver's
	// work via a sequence of Cypher / SQL / Datalog statements that
	// run inside the backend engine. ResolveAllBulk chains the
	// per-rule methods (SameFile → SamePackage → ImportAware → …)
	// in precision-descending order, so higher-precision rules bind
	// first and unique-name fallback only resolves what nothing
	// more specific covered.
	//
	// This is the disk-only / large-repo path: when the in-memory
	// shadow swap is disabled, the resolver's ~100k+ per-edge round
	// trips dominate wall time. The bulk pass typically drains
	// 50-80% of pending edges before the Go worker pool runs, and
	// the remaining set fits cheaply into a single per-pass
	// warmLookupCache. Errors are non-fatal — the Go resolver
	// always re-runs on whatever's left.
	if backendResolverEnabled() {
		if br, ok := r.graph.(graph.BackendResolver); ok {
			if n, err := br.ResolveAllBulk(); err != nil {
				// Non-fatal: the Go path resolves the same edges
				// correctly, just slower.
				_ = n
			}
		}
	}

	// Use the predicate-shaped Store method so disk backends scan
	// only the contiguous "unresolved::*" slice instead of pulling
	// the whole edges table back to the client and filtering in Go.
	// In-memory keeps the same cost as the old AllEdges()+prefix-check
	// loop.
	var pending []*graph.Edge
	for e := range r.graph.EdgesWithUnresolvedTarget() {
		pending = append(pending, e)
	}
	if len(pending) == 0 {
		return &ResolveStats{}
	}

	// Pre-warm the per-pass lookup cache. The resolver workers below
	// will call store.GetNode for endpoints and store.FindNodesByName
	// for resolution candidates — across 10-30k pending edges that's
	// 100k+ individual queries on a disk backend
	// (hundreds of seconds wall time). Collecting the
	// IDs / names upfront and batch-loading them collapses those
	// queries to ~10 chunked SELECT IN statements. Cleared on return
	// via defer so callers outside ResolveAll see the empty caches and
	// fall through to the underlying store on every lookup.
	r.warmLookupCache(pending)
	defer r.clearLookupCache()

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	if workers > len(pending) {
		workers = len(pending)
	}

	perWorkerStats := make([]ResolveStats, workers)
	perWorkerJobs := make([][]reindexJob, workers)
	var wg sync.WaitGroup
	chunk := (len(pending) + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start := w * chunk
		end := start + chunk
		if end > len(pending) {
			end = len(pending)
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(idx int, slice []*graph.Edge) {
			defer wg.Done()
			ws := &perWorkerStats[idx]
			// Pre-size the jobs slice to the worker's chunk; over-
			// allocates by ~5% (only resolved/external edges produce
			// a job), but a few KB of headroom beats the growth
			// amortisation cost across millions of edges.
			jobs := make([]reindexJob, 0, len(slice))
			for _, e := range slice {
				clone := cloneEdgeForResolve(e)
				oldTo, changed := r.resolveEdge(clone, ws)
				if changed {
					jobs = append(jobs, reindexJob{
						edge:       e,
						oldTo:      oldTo,
						newTo:      clone.To,
						kind:       clone.Kind,
						crossRepo:  clone.CrossRepo,
						confidence: clone.Confidence,
						origin:     clone.Origin,
						meta:       clone.Meta,
					})
				}
				// Return the clone shell to the pool. The Meta map (if
				// any) is owned by the reindexJob now and lives on; the
				// Edge struct itself is per-iteration garbage and is
				// the part worth recycling.
				releaseResolverClone(clone)
			}
			perWorkerJobs[idx] = jobs
		}(w, pending[start:end])
	}
	wg.Wait()

	// Apply mutations + ReindexEdge serially. Mutating e.To inside
	// a worker would race with the bucket-maintenance reads inside
	// every other worker's ReindexEdge — keyOf(swappedEdge) reads
	// swappedEdge.To, which a neighbouring worker might be writing.
	// Running both phases serially after the worker barrier removes
	// the race entirely; it costs ~5% of resolver wall time on a
	// 12k-edge vscode pass and buys a clean -race run plus simpler
	// reasoning.
	// Collect every mutation across all workers into one slice and hand
	// the whole batch to ReindexEdges. Disk-backed stores commit per
	// chunk inside the implementation; the in-memory store loops
	// through the existing per-edge code. Per-edge ReindexEdge was the
	// resolver's bottleneck against bbolt (10k+ ACID round-trips); the
	// batch form folds it to ≤(N/5000) commits without changing any
	// observable semantics.
	totalJobs := 0
	for i := range perWorkerJobs {
		totalJobs += len(perWorkerJobs[i])
	}
	reindexBatch := make([]graph.EdgeReindex, 0, totalJobs)
	for i := range perWorkerJobs {
		for _, j := range perWorkerJobs[i] {
			j.edge.To = j.newTo
			j.edge.Kind = j.kind
			j.edge.CrossRepo = j.crossRepo
			j.edge.Confidence = j.confidence
			j.edge.Origin = j.origin
			j.edge.Meta = j.meta
			reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: j.edge, OldTo: j.oldTo})
		}
	}
	r.graph.ReindexEdges(reindexBatch)

	// Cross-package name-match guard. The heuristic fallbacks above can
	// resolve a call by name alone to a candidate in a package the
	// caller never imports. Now that every EdgeImports edge in this pass
	// is resolved, re-check each weak-tier call edge against the import
	// closure and revert the ones whose target is unreachable. The
	// closure is built once and shared; each job still carries its
	// pre-resolution target so a reverted edge is restored exactly.
	guarded := 0
	if closure := r.buildImportClosure(); len(closure) > 0 {
		for i := range perWorkerJobs {
			guarded += r.guardCrossPackageCallEdges(perWorkerJobs[i], closure)
		}
	}

	// Rebind cross-file Go method receivers onto the canonical type
	// node ID. The Go extractor builds the EdgeMemberOf target as
	// `<methodfile>::TypeName` because it parses one file at a time;
	// methods declared in files other than the type's defining file
	// point at a phantom ID until this pass collapses them onto the
	// real `<typefile>::TypeName` node. See rebindGoMethodReceivers
	// for the full rationale (InferImplements + find_implementations
	// + class_hierarchy correctness all ride on this).
	r.rebindGoMethodReceivers()

	// Scope-aware bare-name binding. Walks `unresolved::<name>` edges
	// whose source is inside a function and rewrites them onto the
	// matching KindLocal / KindParam node when exactly one in-scope
	// binding wins under the Go shadowing rules. Without this pass
	// the worker-pool fallback would scan FindNodesByName(name)
	// across the whole graph and fall through to `unresolved::*` for
	// every common identifier (err / data / src / ...). The bind
	// uses #77's KindLocal nodes — pre-#77 there was nothing to
	// bind to.
	r.bindBareNameScopeRefs()

	// Bind in-body references to a function's own generic type
	// parameters (`var x T`, `func F[T any]() T { ... }`) onto the
	// pre-existing KindGenericParam nodes — without this pass they
	// stayed as `unresolved::T` even though the parser had already
	// materialised the tparam node.
	r.bindGenericParamRefs()

	// Attribute Go language intrinsics (append / len / make / string
	// / int / ...) to canonical `builtin::go::*` IDs and materialise
	// one KindBuiltin node per unique builtin. Eliminates ~50k of
	// the bare-name `unresolved::*` population on a Go-heavy
	// codebase and turns the analytics queries that need these
	// targets (`find_usages(builtin::go::type::float64)` for
	// type-drift analysis) into one-hop lookups.
	r.attributeGoBuiltins()

	// Materialise stdlib / dep / external call targets as
	// KindFunction nodes with KindModule parents so cross-package
	// queries (`find_usages(stdlib::fmt::Sprintf)`,
	// `get_callers(dep::github.com/stretchr/testify/assert::True)`,
	// "what's our usage surface on encoding/json") become one-hop
	// lookups. Must run AFTER resolveExtern (which classifies
	// `unresolved::extern::*` into the stdlib/dep/external buckets)
	// so we materialise the post-classification state, not the
	// pre-classification shape.
	r.attributeGoExternalCalls()

	// Relative-import resolution for Python and Dart files. Runs
	// before module attribution so internal-target stems never get
	// mis-mapped to a phantom pypi/pub package.
	r.resolveRelativeImports()

	// Module attribution for ecosystems without a CGO type-checker
	// path (Python, Dart, …). Runs serially on the post-resolution
	// graph so it sees the final `external::*` set after the
	// dep-module bridge has had its chance.
	r.attributeNonGoModuleImports()

	total := &ResolveStats{}
	for i := range perWorkerStats {
		total.Resolved += perWorkerStats[i].Resolved
		total.Unresolved += perWorkerStats[i].Unresolved
		total.External += perWorkerStats[i].External
	}
	// A guarded edge was counted as resolved by the fallback that
	// produced it; reverting it moves the tally back to unresolved.
	if guarded > 0 {
		if total.Resolved >= guarded {
			total.Resolved -= guarded
		} else {
			total.Resolved = 0
		}
		total.Unresolved += guarded
	}
	return total
}

// buildDirIndexes builds two lookup maps for resolveImport. Populated
// once per ResolveAll / ResolveFile pass and torn down after.
//
//   - dirIndex     keys on filepath.Dir(file.FilePath) for exact
//     importPath == dir matches.
//   - lastDirIndex keys on the last path component of that directory
//     so an import of "logger" matches any file under .../logger/.
func (r *Resolver) buildDirIndexes() {
	r.dirIndex = make(map[string][]*graph.Node, 128)
	r.lastDirIndex = make(map[string][]*graph.Node, 128)
	// NodesByKind pushes the file-kind filter into the store; disk
	// backends iterate just the file nodes instead of every node.
	for n := range r.graph.NodesByKind(graph.KindFile) {
		dir := filepath.Dir(n.FilePath)
		r.dirIndex[dir] = append(r.dirIndex[dir], n)
		last := lastPathComponent(dir)
		if last != "" && last != dir {
			r.lastDirIndex[last] = append(r.lastDirIndex[last], n)
		}
	}
}

func (r *Resolver) clearDirIndexes() {
	r.dirIndex = nil
	r.lastDirIndex = nil
}

// warmLookupCache batches the per-edge GetNode / FindNodesByName
// queries the worker loop would otherwise fire serially. We collect
// every From/To node ID across the pending slice and the bare
// identifier name embedded in each `unresolved::*` target, then issue
// the two batched queries the Store exposes. Workers consult the
// resulting maps via cachedGetNode / cachedFindNodesByName; misses
// fall through to the underlying store.
func (r *Resolver) warmLookupCache(pending []*graph.Edge) {
	if len(pending) == 0 {
		return
	}
	idSet := make(map[string]struct{}, len(pending)*2)
	nameSet := make(map[string]struct{}, len(pending))
	for _, e := range pending {
		if e == nil {
			continue
		}
		if e.From != "" {
			idSet[e.From] = struct{}{}
		}
		// e.To at this point still carries the "unresolved::" prefix;
		// pre-loading by that string isn't useful (no node has that
		// id). We seed the name cache from the embedded identifier so
		// the worker's FindNodesByName hit lands in the cache.
		if name := identifierFromTarget(e.To); name != "" {
			nameSet[name] = struct{}{}
		}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	r.nodeByID = r.graph.GetNodesByIDs(ids)
	r.nodesByName = r.graph.FindNodesByNames(names)
	// Fold every candidate node returned by the name lookup into the
	// id cache too: when a worker picks a candidate and the
	// downstream guard (cross_pkg / cross_repo) calls GetNode on the
	// chosen target, the cache should hit instead of falling through
	// to a per-id store call.
	if r.nodeByID == nil && len(r.nodesByName) > 0 {
		r.nodeByID = make(map[string]*graph.Node, len(r.nodesByName))
	}
	for _, hits := range r.nodesByName {
		for _, n := range hits {
			if n == nil || n.ID == "" {
				continue
			}
			if _, ok := r.nodeByID[n.ID]; !ok {
				r.nodeByID[n.ID] = n
			}
		}
	}
}

func (r *Resolver) clearLookupCache() {
	r.nodeByID = nil
	r.nodesByName = nil
}

// cachedGetNode returns the node for id, consulting the per-pass
// lookup cache first and falling through to the underlying store on
// miss. The cache is a positive-only fast path — absence means "not
// pre-warmed", not "doesn't exist", so a miss still asks the store.
// Outside a ResolveAll pass the cache is nil and every call goes
// straight to the store.
func (r *Resolver) cachedGetNode(id string) *graph.Node {
	if id == "" {
		return nil
	}
	if r.nodeByID != nil {
		if n, ok := r.nodeByID[id]; ok {
			return n
		}
	}
	return r.graph.GetNode(id)
}

// cachedFindNodesByName returns the candidates for name, consulting
// the per-pass cache first and falling through to the store on miss.
// Returns the in-cache slice directly when hit — callers MUST treat
// the result as read-only.
func (r *Resolver) cachedFindNodesByName(name string) []*graph.Node {
	if name == "" {
		return nil
	}
	if r.nodesByName != nil {
		if hits, ok := r.nodesByName[name]; ok {
			return hits
		}
	}
	return r.graph.FindNodesByName(name)
}

// buildDepModuleIndex collects every dep::<module-path> contract node
// (one per non-indirect `require` line in a tracked go.mod) and groups
// them by the owning repo's prefix so resolveImport can bridge a Go
// import to the dep node it satisfies. Entries are sorted by
// modulePath length descending, which keeps longest-prefix-wins for
// nested modules (e.g. importing "github.com/aws/aws-sdk-go-v2/service/s3"
// must hit the s3 dep, not the parent aws-sdk-go-v2 dep).
//
// Skips dep IDs of the form `dep::<repoName>::<shortName>`, which
// GoModExtractor emits when the dependency is itself a tracked sibling
// repo — those resolve through the cross-repo file graph instead and
// have no module path embedded in the ID.
func (r *Resolver) buildDepModuleIndex() {
	by := make(map[string][]depModuleEntry)
	for n := range r.graph.NodesByKind(graph.KindContract) {
		if !strings.HasPrefix(n.ID, "dep::") {
			continue
		}
		mp := strings.TrimPrefix(n.ID, "dep::")
		if mp == "" || strings.Contains(mp, "::") {
			continue
		}
		by[n.RepoPrefix] = append(by[n.RepoPrefix], depModuleEntry{
			modulePath: mp,
			node:       n,
		})
	}
	for k := range by {
		entries := by[k]
		sort.Slice(entries, func(i, j int) bool {
			return len(entries[i].modulePath) > len(entries[j].modulePath)
		})
	}
	r.depModuleIndex = by
}

func (r *Resolver) clearDepModuleIndex() {
	r.depModuleIndex = nil
}

// lookupDepModule returns the dep::<module> contract node whose
// module path is a prefix of importPath, scoped to the caller's repo.
// Returns nil if no dep declaration covers this import.
func (r *Resolver) lookupDepModule(callerRepo, importPath string) *graph.Node {
	for _, entry := range r.depModuleIndex[callerRepo] {
		if importPath == entry.modulePath || strings.HasPrefix(importPath, entry.modulePath+"/") {
			return entry.node
		}
	}
	return nil
}

// ResolveFile resolves unresolved edges originating from a specific file.
func (r *Resolver) ResolveFile(filePath string) *ResolveStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buildDirIndexes()
	defer r.clearDirIndexes()
	r.buildDepModuleIndex()
	defer r.clearDepModuleIndex()
	r.buildProvidesForIndex()
	defer r.clearProvidesForIndex()
	r.buildReachabilityIndex()
	defer r.clearReachabilityIndex()
	defer r.clearLSPIndex()

	stats := &ResolveStats{}

	// Get all nodes in the file, then check their outgoing edges.
	// Single-threaded path — collect mutations into a batch and flush
	// in one ReindexEdges call after the file's edges are walked, so a
	// per-file ResolveFile pass produces one Tx commit on disk
	// backends instead of one per resolved edge. Resolved edges are
	// also recorded as jobs so the cross-package guard can re-check
	// (and, if needed, revert) the weak-tier ones.
	var jobs []reindexJob
	var reindexBatch []graph.EdgeReindex
	nodes := r.graph.GetFileNodes(filePath)
	for _, n := range nodes {
		edges := r.graph.GetOutEdges(n.ID)
		for _, e := range edges {
			if !strings.HasPrefix(e.To, unresolvedPrefix) {
				continue
			}
			oldTo, changed := r.resolveEdge(e, stats)
			if changed {
				reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
				jobs = append(jobs, reindexJob{
					edge:       e,
					oldTo:      oldTo,
					newTo:      e.To,
					kind:       e.Kind,
					confidence: e.Confidence,
					origin:     e.Origin,
				})
			}
		}
	}
	if len(reindexBatch) > 0 {
		r.graph.ReindexEdges(reindexBatch)
	}

	// Cross-package name-match guard — same contract as in ResolveAll.
	if len(jobs) > 0 {
		if closure := r.buildImportClosure(); len(closure) > 0 {
			if guarded := r.guardCrossPackageCallEdges(jobs, closure); guarded > 0 {
				if stats.Resolved >= guarded {
					stats.Resolved -= guarded
				} else {
					stats.Resolved = 0
				}
				stats.Unresolved += guarded
			}
		}
	}

	// Re-run the attribution passes that ResolveAll runs. ResolveFile
	// handles incremental updates — a re-parse of one file emits
	// fresh `unresolved::<name>` edges that haven't been seen by these
	// passes yet, so without re-running them the incremental graph
	// diverges from a cold re-index (caught by
	// TestIncrementalReindex_ConvergesToFullIndex). Each pass is
	// idempotent on already-rewritten edges (the `unresolved::`
	// prefix check makes a second sweep a no-op).
	r.rebindGoMethodReceivers()
	r.bindBareNameScopeRefs()
	r.bindGenericParamRefs()
	r.attributeGoBuiltins()
	r.attributeGoExternalCalls()

	return stats
}

// reindexJob captures the resolved state for an edge whose target
// changed during a parallel resolution pass.
//
// Workers operate on shallow clones of each edge (cloneEdgeForResolve
// below) so mutating helpers can write to the clone freely without
// racing with: (a) other workers reading neighbouring edges' fields
// during bucket maintenance, or (b) the serial post-pass that reads
// each edge's To via keyOf. Once the worker phase completes, the
// resolved fields (To, Kind, CrossRepo, Confidence, Origin, Meta) are
// copied onto the real edge and graph.ReindexEdge is called — both
// serially.
//
// Kind is propagated because resolveEdge may promote it after
// resolution (e.g. `*.foo` with EdgeReads that lands on a method gets
// promoted to EdgeReferences so get_callers / find_usages surface the
// method-value reference).
type reindexJob struct {
	edge       *graph.Edge
	oldTo      string
	newTo      string
	kind       graph.EdgeKind
	crossRepo  bool
	confidence float64
	origin     string
	meta       map[string]any
}

// resolverClonePool recycles the *graph.Edge shells handed out by
// cloneEdgeForResolve. The clone is per-iteration garbage in the
// ResolveAll worker — Get / Put across the inner loop turns the per-
// edge alloc into pool churn. Profile #4 (post lineBytesUpTo) showed
// cloneEdgeForResolve still pulling its share of the resolver's flat
// CPU; pooling removes it. The cloned Edge's Meta map is intentionally
// NOT pooled — when a resolution succeeds, the map travels onto the
// real edge via reindexJob.meta and is owned there afterwards.
var resolverClonePool = sync.Pool{
	New: func() any { return &graph.Edge{} },
}

// cloneEdgeForResolve returns a deep-enough copy of e for safe
// worker-local mutation by resolveEdge: every scalar / string field
// is value-copied; Meta is duplicated when present so a helper
// writing `clone.Meta["resolution"] = ...` doesn't mutate a map
// shared with the original (and therefore with other goroutines
// inspecting that map). Meta is the only reference-typed field on
// Edge that resolveEdge may write to today; any future Edge field
// of map / slice type will need handling here too.
//
// The returned *Edge must be released with releaseResolverClone once
// the worker is done with it (after any reindexJob has captured the
// Meta pointer). Forgetting to release just means the clone falls
// back to GC, not a leak.
func cloneEdgeForResolve(e *graph.Edge) *graph.Edge {
	clone := resolverClonePool.Get().(*graph.Edge)
	*clone = *e
	if clone.Meta != nil {
		dup := make(map[string]any, len(clone.Meta))
		for k, v := range clone.Meta {
			dup[k] = v
		}
		clone.Meta = dup
	}
	return clone
}

// releaseResolverClone returns a clone produced by cloneEdgeForResolve
// to the pool. Safe to call after the worker has copied any needed
// fields (To, Kind, Origin, Meta, …) into a reindexJob — the job
// retains its own references to those values, and the Edge shell is
// no longer needed. Zeroing prevents the next Get from seeing stale
// pointer fields the GC would otherwise be unable to reclaim.
func releaseResolverClone(clone *graph.Edge) {
	if clone == nil {
		return
	}
	*clone = graph.Edge{}
	resolverClonePool.Put(clone)
}

// resolveEdge mutates e.To in place and returns the prior value
// when a resolution actually happened (i.e. e.To != oldTo). The
// caller decides whether to call graph.ReindexEdge immediately
// (single-threaded ResolveFile) or to defer the reindex (parallel
// ResolveAll). When nothing changed the returned bool is false.
func (r *Resolver) resolveEdge(e *graph.Edge, stats *ResolveStats) (oldTo string, changed bool) {
	oldTo = e.To
	target := strings.TrimPrefix(e.To, unresolvedPrefix)

	// Resolve-time LSP hot-path. Consulted for TS/JS/JSX/TSX files
	// (and any other languages a future helper claims via
	// SupportsPath). When the LSP wins, the edge is stamped with
	// OriginLSPResolved and resolved_by=lsp; the heuristic path is
	// skipped. When it loses (no helper, no answer, no match), we
	// fall through to the existing heuristic cascade unchanged so
	// the edge still gets the best best-effort target.
	if r.tryResolveViaLSP(e, target, stats) {
		return oldTo, e.To != oldTo
	}

	switch {
	case strings.HasPrefix(target, "grpc::"):
		// gRPC client-stub call placeholder
		// (`unresolved::grpc::<Service>::<Method>`). Landed on the
		// server-side handler by the graph-wide ResolveGRPCStubCalls
		// pass, which needs the whole graph plus InferImplements — the
		// per-edge resolver can't see that. Leave the edge untouched.
		return oldTo, false
	case strings.HasPrefix(target, "pyrel::"):
		// Python relative-import placeholder
		// (`unresolved::pyrel::<projectRootedStem>`). The graph-wide
		// resolveRelativeImports pass lands these on the matching
		// KindFile node once the whole index is built; the per-edge
		// resolver can't see project-layout context. Leave untouched
		// so the post-pass owns rewriting.
		return oldTo, false
	case strings.HasPrefix(target, "import::"):
		r.resolveImport(e, strings.TrimPrefix(target, "import::"), stats)
	case strings.HasPrefix(target, "extern::"):
		// Package-qualified call (json.NewEncoder): the parser attached
		// the full import path + symbol so we don't have to guess a
		// receiver type. resolveExtern accepts type candidates too, so a
		// package-qualified embedded type (`extern::pkg::Base`) keeps
		// its precise import-path evidence here rather than falling to
		// the same-repo-only resolveTypeRef below.
		r.resolveExtern(e, strings.TrimPrefix(target, "extern::"), stats)
	case e.Kind == graph.EdgeExtends || e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeComposes ||
		e.Kind == graph.EdgeReturns || e.Kind == graph.EdgeTypedAs:
		// Type-hierarchy and type-position edges must land on a type
		// or interface — never a function or method. Without this
		// gate the default case routes them through resolveFunctionCall
		// which happily matches any same-named function (e.g.
		// `*tsitter.Language` as a return type landed on a method
		// named `Language` instead of the `Language` type alias,
		// hiding every cross-package type reference from the graph
		// and making aliased types look completely unused). The four
		// kinds covered here:
		//   - EdgeExtends/EdgeImplements/EdgeComposes: type hierarchy
		//   - EdgeReturns: function/method return types
		//   - EdgeTypedAs: parameter / variable / field declared types
		// resolveTypeRef accepts only KindType / KindInterface
		// candidates and is placed ahead of the `*.` cases so a
		// selector-shaped supertype target can't slip into method
		// resolution. extern:: targets are handled above — their
		// import path is real cross-repo evidence.
		r.resolveTypeRef(e, target, stats)
	case strings.HasPrefix(target, "*.") && (e.Kind == graph.EdgeWrites || e.Kind == graph.EdgeReads):
		// Field write/read: prefer a KindField candidate whose
		// receiver matches the edge's receiver_type hint. Falls back
		// to the method-resolution path when no field candidate
		// lands — gives degraded-but-useful behaviour for graphs
		// where the field-node pass hasn't caught up yet.
		//
		// When the fallback resolves to a method, the extractor's
		// EdgeReads label was a placeholder for "selector used as a
		// value" (e.g. `mux.HandleFunc("/p", h.foo)` — h.foo passed,
		// not called). Promote to EdgeReferences so find_usages and
		// get_callers surface the method-value reference. Writes stay
		// as EdgeWrites: assigning a func value to a method-typed
		// field slot is still a write semantically.
		fieldName := strings.TrimPrefix(target, "*.")
		if !r.resolveFieldRef(e, fieldName, stats) {
			before := e.To
			r.resolveMethodCall(e, fieldName, stats)
			if e.Kind == graph.EdgeReads && e.To != before {
				e.Kind = graph.EdgeReferences
			}
		}
	case strings.HasPrefix(target, "*."):
		// Method call or method-value reference (e.g. h.handleHealth)
		r.resolveMethodCall(e, strings.TrimPrefix(target, "*."), stats)
	case e.Kind == graph.EdgeProvides || e.Kind == graph.EdgeConsumes:
		// DI-token reference — the target is a named value (injection
		// token), usually an `export const`, that the resolver's
		// function/method passes would miss because they only accept
		// method/function candidates.
		r.resolveTokenRef(e, target, stats)
	default:
		// For instantiates/references edges, try to resolve as a type first;
		// for calls edges, resolve as a function (original behavior).
		if e.Kind == graph.EdgeInstantiates || e.Kind == graph.EdgeReferences {
			r.resolveTypeOrFunc(e, target, stats)
		} else {
			before := e.To
			r.resolveFunctionCall(e, target, stats)
			// Promote EdgeReads → EdgeReferences when the resolved
			// target is a function or method. The extractor emits
			// EdgeReads for "bare identifier as value" (e.g. a cobra
			// command's `RunE: runClean` or `&Command{RunE: runFoo}`),
			// because at parse time it can't tell a function pointer
			// from a variable read. Now that we know the target is a
			// function, treat it as a reference so get_callers /
			// find_usages surface the wire-up site. Without this,
			// every CLI-wired command and command-table entry looks
			// like dead code.
			if e.Kind == graph.EdgeReads && e.To != before {
				if n := r.cachedGetNode(e.To); n != nil && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
					e.Kind = graph.EdgeReferences
				}
			}
		}
	}

	return oldTo, e.To != oldTo
}

// resolveExtern handles "extern::<importPath>::<symbol>" targets produced
// by the parser when a selector call's receiver matches an import alias.
//
// Resolution order:
//  1. Look for <symbol> defined in a file whose dir matches the import
//     path — this catches cross-repo calls into another indexed tree
//     (e.g. service A calls service B's exported function).
//  2. Otherwise, keep the package-qualified target so the UI can render
//     "crosses web → encoding/json" instead of a bare em-dash. The
//     prefix chosen encodes whether the path looks stdlib-like (no dot
//     in first segment, for Go) vs a module path (dotted or vendored).
//
// Nothing is created as a graph node — these are bookkeeping strings,
// same as the existing "external::<path>" stubs for unresolved imports.
func (r *Resolver) resolveExtern(e *graph.Edge, spec string, stats *ResolveStats) {
	sep := strings.LastIndex(spec, "::")
	if sep < 0 {
		// Malformed — treat as unresolved so we don't leak the
		// "unresolved::extern::" prefix into the graph.
		e.To = "external::" + spec
		stats.External++
		return
	}
	importPath := spec[:sep]
	symbol := spec[sep+2:]

	// Pass 1: does the symbol live in a file under this import path?
	// Reuse dirIndex populated by buildDirIndexes — no extra scan.
	// cachedFindNodesByName lands in the per-pass batch cache for
	// the common worker hot path; falls through to the store when
	// called outside ResolveAll.
	callerRepo := r.callerRepoPrefix(e)
	candidates := r.cachedFindNodesByName(symbol)
	for _, c := range candidates {
		if c.Kind != graph.KindFunction && c.Kind != graph.KindMethod && c.Kind != graph.KindType && c.Kind != graph.KindInterface {
			continue
		}
		dir := filepath.Dir(c.FilePath)
		crossRepo := callerRepo != "" && c.RepoPrefix != "" && c.RepoPrefix != callerRepo
		var matches bool
		if crossRepo {
			// Cross-repo extern call: require a precise import-path
			// suffix match. The old loose last-component test
			// (`*/go`) resolved every tree-sitter binding's
			// `Language` to whichever repo sorted first.
			matches = dirMatchesImport(dir, importPath)
		} else {
			matches = strings.HasSuffix(dir, "/"+lastPathComponent(importPath)) || dir == importPath || strings.HasSuffix(dir, importPath)
		}
		if matches {
			e.To = c.ID
			if crossRepo {
				e.CrossRepo = true
			}
			stats.Resolved++
			return
		}
	}

	// Pass 2: classify the import path. "stdlib::" when the path looks
	// like a Go stdlib package (no dot in the first segment and not a
	// known module vendor prefix). "dep::" otherwise. Callers can treat
	// both as external for edge-walk purposes. The stdlib stub carries
	// the caller's repo prefix (see internal/graph/stub.go) so two repos
	// pinned to different Go SDK versions get distinct fmt::Errorf nodes
	// instead of one shared, version-conflated terminal.
	if isStdlibLike(importPath) {
		e.To = graph.StubID(callerRepo, graph.StubKindStdlib, importPath, symbol)
	} else {
		e.To = "dep::" + importPath + "::" + symbol
	}
	stats.External++
}

// isStdlibLike reports whether the import path looks like a Go stdlib
// package. Heuristic: the first path segment must have no dot (module
// paths like github.com/foo, golang.org/x, etc. always dot the first
// segment). Vetted against the list of real stdlib roots used by
// go/build — any new single-word non-stdlib package (very rare) is
// mis-classified as stdlib, which is cosmetic only.
func isStdlibLike(importPath string) bool {
	first := importPath
	if i := strings.Index(importPath, "/"); i >= 0 {
		first = importPath[:i]
	}
	return first != "" && !strings.Contains(first, ".")
}

func (r *Resolver) resolveImport(e *graph.Edge, importPath string, stats *ResolveStats) {
	callerRepo := r.callerRepoPrefix(e)

	// npm-alias rewrite: a JS/TS import of a package.json alias key
	// (`"shared": "npm:@acme/shared-lib@1.4.0"`) actually targets the
	// real package. Rewrite the specifier before any lookup so a
	// locally-vendored `@acme/shared-lib` resolves to its real node
	// instead of falling through to an external stub. A no-op for
	// non-aliased specifiers and non-JS/TS callers.
	importPath, npmAliased := rewriteNpmAliasImport(r.npmAlias, e.FilePath, importPath)

	// Look for a package node with matching qualified name.
	node := r.graph.GetNodeByQualName(importPath)
	if node != nil {
		e.To = node.ID
		if callerRepo != "" && node.RepoPrefix != "" && node.RepoPrefix != callerRepo {
			e.CrossRepo = true
		}
		stats.Resolved++
		return
	}

	// Inverted-index lookup instead of a per-edge AllNodes() scan —
	// the old scan was O(N) per import and the dominant cost of
	// ResolveAll on large repos (e.g. vscode: 5k imports × 150k nodes
	// = 750M comparisons per cold index). Falls back to a scan only
	// when the indexes aren't populated (ResolveEdge invoked outside
	// of ResolveAll/ResolveFile).
	//
	// When a package-manager workspace lookup is installed, all
	// same-repo candidates are collected (not just the first) so a
	// same-named collision across two workspace members can be broken
	// in favour of the importer's own workspace. Without the lookup
	// the first same-repo hit short-circuits the scan, preserving the
	// pre-feature cost.
	collectAll := r.workspaceMembers != nil
	var sameRepo, crossRepoNode *graph.Node
	var sameRepoAll []*graph.Node
	consider := func(n *graph.Node) {
		if n.Kind != graph.KindFile {
			return
		}
		if callerRepo == "" || n.RepoPrefix == callerRepo {
			if sameRepo == nil {
				sameRepo = n
			}
			if collectAll {
				sameRepoAll = append(sameRepoAll, n)
			}
			return
		}
		// Cross-repo file candidate: require a precise import-path
		// suffix match. The lastDirIndex / full-scan fallbacks key on
		// the last path component only, so without this gate an import
		// of `.../tree-sitter-c/bindings/go` would resolve to whichever
		// `*/bindings/go` directory sorts first.
		if crossRepoNode == nil && dirMatchesImport(filepath.Dir(n.FilePath), importPath) {
			crossRepoNode = n
		}
	}
	// stop reports whether the candidate scan can short-circuit: once a
	// same-repo hit is found and we are not collecting every candidate
	// for workspace disambiguation.
	stop := func() bool { return sameRepo != nil && !collectAll }
	if r.dirIndex != nil {
		for _, n := range r.dirIndex[importPath] {
			consider(n)
			if stop() {
				break
			}
		}
		if sameRepo == nil || collectAll {
			for _, n := range r.lastDirIndex[lastPathComponent(importPath)] {
				consider(n)
				if stop() {
					break
				}
			}
		}
	} else {
		for n := range r.graph.NodesByKind(graph.KindFile) {
			dir := filepath.Dir(n.FilePath)
			if strings.HasSuffix(dir, lastPathComponent(importPath)) || dir == importPath {
				consider(n)
				if stop() {
					break
				}
			}
		}
	}

	if sameRepo != nil {
		// Name-collision tie-break: when several same-repo files match
		// a bare import name, prefer the one in the importing file's
		// own package-manager workspace.
		if ws := r.preferSameWorkspaceFile(e.FilePath, sameRepoAll); ws != nil {
			sameRepo = ws
		}
		e.To = sameRepo.ID
		stats.Resolved++
		return
	}
	if crossRepoNode != nil {
		e.To = crossRepoNode.ID
		if callerRepo != "" && crossRepoNode.RepoPrefix != "" && crossRepoNode.RepoPrefix != callerRepo {
			e.CrossRepo = true
		}
		stats.Resolved++
		return
	}

	// No same- or cross-repo file matched. Before falling back to an
	// `external::` stub, try the dep::<module> contract nodes from the
	// caller's go.mod — that bridge is what gives third-party imports
	// like "github.com/foo/bar/sub/pkg" an incoming edge on the
	// dep::github.com/foo/bar node.
	if depNode := r.lookupDepModule(callerRepo, importPath); depNode != nil {
		e.To = depNode.ID
		stats.Resolved++
		return
	}

	// npm-alias sub-path: a rewritten import like `@acme/shared-lib/util`
	// addresses a path inside the real package. Nothing matched the
	// full path, so fall back to the package node itself — the
	// cross-package edge belongs on the package regardless of which
	// sub-module the importer reached for.
	if npmAliased {
		if pkg := npmPackagePrefix(importPath); pkg != "" {
			if node := r.graph.GetNodeByQualName(pkg); node != nil {
				e.To = node.ID
				if callerRepo != "" && node.RepoPrefix != "" && node.RepoPrefix != callerRepo {
					e.CrossRepo = true
				}
				stats.Resolved++
				return
			}
		}
	}

	// External/unresolvable import — create a stub target ID.
	e.To = "external::" + importPath
	stats.External++
}

func (r *Resolver) resolveFunctionCall(e *graph.Edge, funcName string, stats *ResolveStats) {
	callerRepo := r.callerRepoPrefix(e)
	candidates := r.graph.FindNodesByNameInRepo(funcName, callerRepo)
	if len(candidates) == 0 {
		// No same-repo candidate. A genuine cross-repo callee is left
		// unresolved here for CrossRepoResolver — which alone carries the
		// import-reachability + workspace-boundary evidence — to lift.
		// Guessing "first function named X anywhere in the graph" is the
		// exact name-collision bug this gate removes.
		stats.Unresolved++
		return
	}

	// Per-language scope-based static resolver. Consulted before the
	// generic locality cascade so C file-static / C++ namespace +
	// ADL / Java enclosing-class / PHP namespace + parent::/self::
	// rules can land a precise binding when their evidence is strong.
	// Returns nil when no language-specific rule applies; the cascade
	// below then runs unchanged.
	if pick := r.preferScopeCandidate(e, funcName, candidates); pick != nil {
		e.To = pick.ID
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.92
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["resolution"] = "scope"
		stats.Resolved++
		return
	}

	// Prefer same-package (same directory) match.
	callerDir := filepath.Dir(e.FilePath)
	for _, c := range candidates {
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// Fall back to the first same-repo function/method match.
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

// resolveTypeOrFunc resolves unresolved edges that could be either a type
// reference (composite literal, type assertion) or a function reference.
// It first tries to match a type/interface node, then falls back to functions.
// Candidates are restricted to the caller's own repo — a cross-repo
// match here would be a name-only guess; CrossRepoResolver handles the
// genuine cross-repo case with import-reachability evidence.
func (r *Resolver) resolveTypeOrFunc(e *graph.Edge, name string, stats *ResolveStats) {
	callerRepo := r.callerRepoPrefix(e)
	candidates := r.graph.FindNodesByNameInRepo(name, callerRepo)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerDir := filepath.Dir(e.FilePath)

	// Prefer same-package type match.
	for _, c := range candidates {
		if (c.Kind == graph.KindType || c.Kind == graph.KindInterface) &&
			filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// Fall back to any same-repo type match.
	for _, c := range candidates {
		if c.Kind == graph.KindType || c.Kind == graph.KindInterface {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// If no type found, try as function (e.g., bare function name passed as value).
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			if filepath.Dir(c.FilePath) == callerDir {
				e.To = c.ID
				stats.Resolved++
				return
			}
		}
	}
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

// resolveTypeRef resolves an extends / implements / composes edge to a
// type or interface node. It never accepts a function or method
// candidate — a type-hierarchy edge whose target is a function is
// always a misresolution (the bug that let `type EdgeKind string`
// "extend" a method named `string`). Candidates are restricted to the
// caller's own repo; a genuine cross-repo supertype is left unresolved
// for CrossRepoResolver.
func (r *Resolver) resolveTypeRef(e *graph.Edge, name string, stats *ResolveStats) {
	// A selector-shaped target (`*.Base`, from an embedded `pkg.Base`)
	// carries no usable package qualifier once it reaches here — strip
	// the `*.` and resolve on the bare type name.
	name = strings.TrimPrefix(name, "*.")
	callerRepo := r.callerRepoPrefix(e)
	candidates := r.graph.FindNodesByNameInRepo(name, callerRepo)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}
	callerDir := filepath.Dir(e.FilePath)

	// Prefer a same-directory type / interface (same package).
	for _, c := range candidates {
		if (c.Kind == graph.KindType || c.Kind == graph.KindInterface) &&
			filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	// Otherwise any same-repo type / interface.
	for _, c := range candidates {
		if c.Kind == graph.KindType || c.Kind == graph.KindInterface {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	stats.Unresolved++
}

// resolveFieldRef lands an EdgeWrites/EdgeReads edge on a KindField
// node when the receiver type is known. Returns true when a field
// candidate was picked — caller falls back to method resolution
// otherwise (handles cases where the extractor labelled the edge as a
// write but the runtime target is actually a method/property).
func (r *Resolver) resolveFieldRef(e *graph.Edge, fieldName string, stats *ResolveStats) bool {
	receiverType := edgeReceiverType(e)
	candidates := r.graph.FindNodesByNameInRepo(fieldName, r.callerRepoPrefix(e))
	if len(candidates) == 0 {
		return false
	}
	callerDir := filepath.Dir(e.FilePath)

	// Pass 1: same-directory + exact-receiver-type field.
	if receiverType != "" {
		for _, c := range candidates {
			if c.Kind == graph.KindField &&
				filepath.Dir(c.FilePath) == callerDir &&
				nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.95
				stats.Resolved++
				return true
			}
		}
		// Pass 2: exact-receiver-type field, any directory.
		for _, c := range candidates {
			if c.Kind == graph.KindField && nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.85
				stats.Resolved++
				return true
			}
		}
	}

	// Pass 3: caller is a method on type T, prefer a same-T field.
	if callerNode := r.graph.GetNode(e.From); callerNode != nil && callerNode.Kind == graph.KindMethod {
		callerRecv := nodeReceiverType(callerNode)
		if callerRecv != "" {
			for _, c := range candidates {
				if c.Kind == graph.KindField && nodeReceiverType(c) == callerRecv {
					e.To = c.ID
					e.Confidence = 0.85
					stats.Resolved++
					return true
				}
			}
		}
	}

	// Pass 4: same-directory field of any owner type — last resort
	// before falling through to method resolution.
	for _, c := range candidates {
		if c.Kind == graph.KindField && filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			e.Confidence = 0.6
			stats.Resolved++
			return true
		}
	}
	return false
}

func (r *Resolver) resolveMethodCall(e *graph.Edge, methodName string, stats *ResolveStats) {
	// Same-repo gate first: the per-repo Resolver never resolves a
	// method call across a repo boundary by name. A cross-repo method
	// call is left unresolved for CrossRepoResolver, which carries the
	// import-reachability + workspace-boundary evidence.
	rawCandidates := r.graph.FindNodesByNameInRepo(methodName, r.callerRepoPrefix(e))
	if len(rawCandidates) == 0 {
		if r.applyBuiltinIfKnown(e, methodName, stats) {
			return
		}
		stats.Unresolved++
		return
	}

	// Pass 0: import-reachability filter. Drop candidates whose package
	// the caller's file does not import (or sit in). This collapses
	// most cross-package name collisions before any later pass has to
	// guess. The filter is conservative — when the index is missing or
	// would empty the list, the original candidates pass through.
	candidates := r.filterByReachability(e.FilePath, rawCandidates)

	// Per-language scope rule lands binding when its evidence is
	// strong (C static / C++ namespace + ADL / Java enclosing class /
	// PHP parent::/self::/namespace). Empty return falls through to
	// the existing receiver-type cascade unchanged.
	if pick := r.preferScopeCandidate(e, methodName, candidates); pick != nil {
		e.To = pick.ID
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.92
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["resolution"] = "scope"
		stats.Resolved++
		return
	}

	callerDir := filepath.Dir(e.FilePath)
	receiverType := edgeReceiverType(e)

	// If we have a type hint, try exact type match first.
	if receiverType != "" {
		// Pass 1: same-directory + exact type match (highest confidence).
		for _, c := range candidates {
			if c.Kind == graph.KindMethod &&
				filepath.Dir(c.FilePath) == callerDir &&
				nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.95
				stats.Resolved++
				return
			}
		}
		// Pass 2: exact type match, any directory.
		for _, c := range candidates {
			if c.Kind == graph.KindMethod && nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.85
				stats.Resolved++
				return
			}
		}
		// Pass 2b: DI useClass binding. When receiver_type is an
		// abstract/base class that has no method of this name (Passes
		// 1 and 2 found nothing), look for a `provides_for: <type>`
		// edge in the graph — that tells us which concrete class a
		// @Module has bound this abstract to. Prefer candidate methods
		// on that concrete. Without this, the final name-only fallback
		// picks the first implementer alphabetically, which produced
		// SmsNotifier.notify instead of the module-bound EmailNotifier
		// on the NestJS DI fixture.
		if bound := r.boundImplsFor(receiverType); len(bound) > 0 {
			for _, c := range candidates {
				if c.Kind != graph.KindMethod {
					continue
				}
				recv := nodeReceiverType(c)
				if _, ok := bound[recv]; !ok {
					continue
				}
				e.To = c.ID
				e.Confidence = 0.9
				if e.Meta == nil {
					e.Meta = map[string]any{}
				}
				e.Meta["resolution"] = "useClass_binding"
				stats.Resolved++
				return
			}
		}
	}

	// Fallback: infer receiver type from the caller node.
	// If the caller is a method on type X and there's a candidate method on
	// type X with the same name, prefer it.  This handles e.extractFunctions()
	// where the type env doesn't have a hint for parameter-bound receivers.
	callerNode := r.graph.GetNode(e.From)
	if callerNode != nil && callerNode.Kind == graph.KindMethod {
		callerRecv := nodeReceiverType(callerNode)
		if callerRecv != "" {
			// Same receiver type + same directory = very high confidence.
			for _, c := range candidates {
				if c.Kind == graph.KindMethod &&
					filepath.Dir(c.FilePath) == callerDir &&
					nodeReceiverType(c) == callerRecv {
					e.To = c.ID
					e.Confidence = 0.9
					stats.Resolved++
					return
				}
			}
			// Same receiver type, any directory.
			for _, c := range candidates {
				if c.Kind == graph.KindMethod && nodeReceiverType(c) == callerRecv {
					e.To = c.ID
					e.Confidence = 0.8
					stats.Resolved++
					return
				}
			}
		}
	}

	// Locality fallback (replaces the previous alphabetical name-only
	// pick). At this point candidates have survived Pass 0 — they all
	// live in packages reachable from the caller. Prefer in this order:
	//
	//   1. Method, same directory  — same package, definitely reachable.
	//   2. Method, any reachable directory  — exactly one survivor: take it.
	//   3. Method, any reachable directory  — multiple survivors: see below.
	//   4. Function, same directory  — pkg.Func() calls land here too.
	//   5. Function, any reachable directory  — same logic as methods.
	//
	// When step 3 finds multiple methods, we prefer the same-package one
	// (locality bias is stronger than any cross-package signal we have
	// without LSP). If no candidate is same-package, we still take the
	// first reachable one — Pass 0 already guaranteed reachability, so
	// this is no longer an arbitrary alphabetical choice across the
	// whole graph but a choice within the caller's import closure.
	var sameDirMethod, sameDirFunc, anyMethod, anyFunc *graph.Node
	methodCount := 0
	for _, c := range candidates {
		switch c.Kind {
		case graph.KindMethod:
			methodCount++
			if filepath.Dir(c.FilePath) == callerDir && sameDirMethod == nil {
				sameDirMethod = c
			}
			if anyMethod == nil {
				anyMethod = c
			}
		case graph.KindFunction:
			if filepath.Dir(c.FilePath) == callerDir && sameDirFunc == nil {
				sameDirFunc = c
			}
			if anyFunc == nil {
				anyFunc = c
			}
		}
	}

	// Interface-dispatch annotation: when the receiver type names a
	// graph interface and multiple reachable methods of this name
	// exist, every candidate is a legal runtime target. Mark the edge
	// so downstream consumers don't treat the picked target as
	// definitive. Done before the locality picks so it applies whether
	// the chosen target lands in the same-dir or any-dir bucket.
	if methodCount > 1 && r.receiverIsInterface(receiverType) {
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["dispatch"] = "interface"
		e.Origin = graph.OriginLSPDispatch
	}

	if sameDirMethod != nil {
		e.To = sameDirMethod.ID
		stats.Resolved++
		return
	}
	if anyMethod != nil {
		e.To = anyMethod.ID
		stats.Resolved++
		return
	}
	if sameDirFunc != nil {
		e.To = sameDirFunc.ID
		stats.Resolved++
		return
	}
	if anyFunc != nil {
		e.To = anyFunc.ID
		stats.Resolved++
		return
	}

	// Name matched something, but not in a way we accepted. Give the
	// built-in classifier a chance before declaring the edge dead —
	// `arr.push` on an Array may also match an unrelated `push` method
	// elsewhere in the graph, in which case we'd rather label it as a
	// built-in than silently misresolve.
	if r.applyBuiltinIfKnown(e, methodName, stats) {
		return
	}
	stats.Unresolved++
}

// receiverIsInterface returns true when the named receiver type
// resolves to a graph node of kind interface. Used by the locality
// fallback to recognise interface-dispatch ambiguity rather than
// treat it as a single-target resolution. Empty receiver type returns
// false.
func (r *Resolver) receiverIsInterface(receiverType string) bool {
	if receiverType == "" {
		return false
	}
	for _, n := range r.graph.FindNodesByName(receiverType) {
		if n.Kind == graph.KindInterface {
			return true
		}
	}
	return false
}

// applyBuiltinIfKnown routes an unresolvable method call to the
// built-in stub (`builtin::<lang>::<category>::<method>`) when the
// caller's language and the method name are both in our lookup tables.
// Returns true when the edge was rewritten; caller should skip its
// Unresolved increment in that case.
func (r *Resolver) applyBuiltinIfKnown(e *graph.Edge, methodName string, stats *ResolveStats) bool {
	lang := langFromFilePath(e.FilePath)
	if lang == "" {
		return false
	}
	category, ok := classifyBuiltin(methodName, lang)
	if !ok {
		return false
	}
	e.To = graph.StubID(r.callerRepoPrefix(e), graph.StubKindBuiltin, lang, category, methodName)
	stats.External++
	return true
}

// resolveTokenRef resolves the target of an EdgeProvides / EdgeConsumes
// edge that refers to a DI injection token. Tokens are typically
// declared as `export const MY_TOKEN = '...'` (KindVariable) — the
// method/function passes skip them. We name-lookup and accept any kind,
// preferring same-directory matches so token names that happen to
// collide across unrelated files don't pull spurious edges.
func (r *Resolver) resolveTokenRef(e *graph.Edge, name string, stats *ResolveStats) {
	// Same-repo gate: DI token names collide readily across unrelated
	// repos ("TOKEN", "CONFIG", …); a cross-repo first-candidate pick
	// is a name-only guess. CrossRepoResolver handles genuine cross-repo
	// token references.
	candidates := r.graph.FindNodesByNameInRepo(name, r.callerRepoPrefix(e))
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}
	callerDir := filepath.Dir(e.FilePath)
	for _, c := range candidates {
		if filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			e.Confidence = 0.9
			stats.Resolved++
			return
		}
	}
	// No same-dir hit: take the first same-repo candidate so find_usages
	// can still surface the relationship. Confidence drops to reflect
	// uncertainty.
	e.To = candidates[0].ID
	e.Confidence = 0.7
	stats.Resolved++
}

// buildProvidesForIndex walks AllEdges once and materialises a map of
// abstract type → concrete class names declared via `@Module({ providers:
// [{ provide: X, useClass: Y }] })`. boundImplsFor then consults this
// index in O(1) per call edge instead of the O(E) scan that made this
// path the dominant serial cost on large repos — e.g. a vscode index
// had ~10k call edges triggering a full 30k-edge scan each, for 300M
// comparisons that found nothing (vscode has zero NestJS modules).
func (r *Resolver) buildProvidesForIndex() {
	idx := make(map[string]map[string]struct{})
	for ed := range r.graph.EdgesByKind(graph.EdgeProvides) {
		if ed.Meta == nil {
			continue
		}
		pf, _ := ed.Meta["provides_for"].(string)
		if pf == "" {
			continue
		}
		if b, _ := ed.Meta["binding"].(string); b != "useClass" {
			continue
		}
		to := ed.To
		var name string
		if strings.HasPrefix(to, "unresolved::") {
			name = strings.TrimPrefix(to, "unresolved::")
		} else if cut := strings.LastIndex(to, "::"); cut >= 0 {
			name = to[cut+2:]
		} else {
			name = to
		}
		set, ok := idx[pf]
		if !ok {
			set = make(map[string]struct{})
			idx[pf] = set
		}
		set[name] = struct{}{}
	}
	r.providesForIdx = idx
}

func (r *Resolver) clearProvidesForIndex() { r.providesForIdx = nil }

// buildReachabilityIndex walks all EdgeImports edges once and records,
// for each caller file, the set of directories of imported (indexed)
// packages. Resolved import edges point at a file node directly;
// unresolved ones still carry `unresolved::import::<importPath>`,
// which we look up via the same dirIndex resolveImport uses, so the
// reachability index is correct even before import resolution races
// to completion in the parallel pass.
//
// Files always include their own directory in the reachable set so
// same-package calls survive the filter.
func (r *Resolver) buildReachabilityIndex() {
	idx := make(map[string]map[string]struct{})

	addDir := func(callerFileID, dir string) {
		if callerFileID == "" || dir == "" {
			return
		}
		set, ok := idx[callerFileID]
		if !ok {
			set = make(map[string]struct{})
			idx[callerFileID] = set
		}
		set[dir] = struct{}{}
	}

	// Seed with each indexed file's own directory.
	for n := range r.graph.NodesByKind(graph.KindFile) {
		addDir(n.ID, filepath.Dir(n.FilePath))
	}

	for e := range r.graph.EdgesByKind(graph.EdgeImports) {
		var importedDir string
		switch {
		case strings.HasPrefix(e.To, "unresolved::import::"):
			path := strings.TrimPrefix(e.To, "unresolved::import::")
			if files := r.dirIndex[path]; len(files) > 0 {
				importedDir = filepath.Dir(files[0].FilePath)
			} else if last := lastPathComponent(path); last != "" {
				if files := r.lastDirIndex[last]; len(files) > 0 {
					importedDir = filepath.Dir(files[0].FilePath)
				}
			}
		case strings.HasPrefix(e.To, "external::"):
			// External / unindexed package — nothing to add.
		default:
			if n := r.graph.GetNode(e.To); n != nil && n.Kind == graph.KindFile {
				importedDir = filepath.Dir(n.FilePath)
			}
		}
		if importedDir != "" {
			addDir(e.From, importedDir)
		}
	}

	r.reachableDirsByFile = idx
}

func (r *Resolver) clearReachabilityIndex() { r.reachableDirsByFile = nil }

// filterByReachability narrows candidates to those whose defining file
// sits in a package reachable from the caller file. "Reachable" means:
// (a) same directory as the caller (same package), or (b) directory of
// a file imported via EdgeImports. Returns the original list when the
// reachability index is unavailable (e.g. resolveEdge invoked outside
// a Resolve* pass) or when no candidate is reachable — better to keep
// candidates and let downstream passes handle them than to drop the
// edge in cases where the index is incomplete.
func (r *Resolver) filterByReachability(callerFileID string, candidates []*graph.Node) []*graph.Node {
	if r.reachableDirsByFile == nil || callerFileID == "" {
		return candidates
	}
	reachable, ok := r.reachableDirsByFile[callerFileID]
	if !ok || len(reachable) == 0 {
		return candidates
	}
	out := candidates[:0:0]
	for _, c := range candidates {
		if _, ok := reachable[filepath.Dir(c.FilePath)]; ok {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return candidates
	}
	return out
}

// boundImplsFor returns the set of concrete class names bound to the
// abstract type `abstractName` via @Module({ providers: [{ provide: X,
// useClass: Y }] })` entries. Keys are class names (e.g. "EmailNotifier")
// so the caller can match against nodeReceiverType of method candidates.
// Empty when no binding exists.
func (r *Resolver) boundImplsFor(abstractName string) map[string]struct{} {
	if abstractName == "" || len(r.providesForIdx) == 0 {
		return nil
	}
	return r.providesForIdx[abstractName]
}

// edgeReceiverType extracts the receiver_type from Edge.Meta, if present.
func edgeReceiverType(e *graph.Edge) string {
	if e.Meta == nil {
		return ""
	}
	if rt, ok := e.Meta["receiver_type"].(string); ok {
		return rt
	}
	return ""
}

// nodeReceiverType extracts the receiver type from a method Node.Meta.
func nodeReceiverType(n *graph.Node) string {
	if n.Meta == nil {
		return ""
	}
	if rt, ok := n.Meta["receiver"].(string); ok {
		return rt
	}
	return ""
}

// memberMethodInfosByType returns the storage layer's per-type member
// method projection verbatim. Routed through MemberMethodsByType when
// the backend implements it; falls back to an EdgesByKind +
// per-edge GetNode walk that synthesises matching info rows.
func memberMethodInfosByType(g graph.Store) map[string][]graph.MemberMethodInfo {
	if cap, ok := g.(graph.MemberMethodsByType); ok {
		return cap.MemberMethodsByType()
	}
	out := map[string][]graph.MemberMethodInfo{}
	for e := range g.EdgesByKind(graph.EdgeMemberOf) {
		method := g.GetNode(e.From)
		if method == nil || method.Kind != graph.KindMethod {
			continue
		}
		out[e.To] = append(out[e.To], graph.MemberMethodInfo{
			MethodID:   method.ID,
			Name:       method.Name,
			FilePath:   method.FilePath,
			StartLine:  method.StartLine,
			RepoPrefix: method.RepoPrefix,
		})
	}
	return out
}

// nodesByKindsOrAll returns every node whose Kind is in the given
// set, using the NodesByKindsScanner capability when the backend
// implements it (a single Cypher kind-IN scan, one C-string column
// per row) and falling back to AllNodes + Go-side filter otherwise.
func nodesByKindsOrAll(g graph.Store, kinds ...graph.NodeKind) []*graph.Node {
	if scan, ok := g.(graph.NodesByKindsScanner); ok {
		return scan.NodesByKinds(kinds)
	}
	set := make(map[graph.NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		set[k] = struct{}{}
	}
	var out []*graph.Node
	for _, n := range g.AllNodes() {
		if n == nil {
			continue
		}
		if _, ok := set[n.Kind]; ok {
			out = append(out, n)
		}
	}
	return out
}

// memberMethodsByType returns typeID → method-name-set for every
// EdgeMemberOf edge whose source is a KindMethod node. Routed through
// the storage layer's MemberMethodsByType capability when the backend
// implements it (one Cypher join, server-side), falling back to the
// EdgesByKind + per-edge GetNode loop the resolver used before the
// capability landed. Used by InferImplements (and shaped to match its
// existing map[string]map[string]bool API).
func memberMethodsByType(g graph.Store) map[string]map[string]bool {
	if cap, ok := g.(graph.MemberMethodsByType); ok {
		raw := cap.MemberMethodsByType()
		if len(raw) == 0 {
			return nil
		}
		out := make(map[string]map[string]bool, len(raw))
		for typeID, methods := range raw {
			set := make(map[string]bool, len(methods))
			for _, m := range methods {
				set[m.Name] = true
			}
			out[typeID] = set
		}
		return out
	}
	out := map[string]map[string]bool{}
	for e := range g.EdgesByKind(graph.EdgeMemberOf) {
		methodNode := g.GetNode(e.From)
		if methodNode == nil || methodNode.Kind != graph.KindMethod {
			continue
		}
		if out[e.To] == nil {
			out[e.To] = make(map[string]bool)
		}
		out[e.To][methodNode.Name] = true
	}
	return out
}

// memberMethodNodesByType returns typeID → name → method-node for
// every EdgeMemberOf edge whose source is a KindMethod node. Routed
// through the storage layer's MemberMethodsByType capability when the
// backend implements it (the projection ships only the four columns
// the consumer reads — ID / Name / FilePath / StartLine — packed into
// a synthetic *Node that carries no Meta / QualName / Language); falls
// back to the EdgesByKind + per-edge GetNode loop otherwise. Used by
// InferOverrides which keys methods by name and reads ID/FilePath/
// StartLine off the node when it emits an EdgeOverrides edge.
func memberMethodNodesByType(g graph.Store) map[string]map[string]*graph.Node {
	if cap, ok := g.(graph.MemberMethodsByType); ok {
		raw := cap.MemberMethodsByType()
		if len(raw) == 0 {
			return nil
		}
		out := make(map[string]map[string]*graph.Node, len(raw))
		for typeID, methods := range raw {
			set := make(map[string]*graph.Node, len(methods))
			for _, m := range methods {
				set[m.Name] = &graph.Node{
					ID:         m.MethodID,
					Kind:       graph.KindMethod,
					Name:       m.Name,
					FilePath:   m.FilePath,
					StartLine:  m.StartLine,
					RepoPrefix: m.RepoPrefix,
				}
			}
			out[typeID] = set
		}
		return out
	}
	out := map[string]map[string]*graph.Node{}
	for e := range g.EdgesByKind(graph.EdgeMemberOf) {
		method := g.GetNode(e.From)
		if method == nil || method.Kind != graph.KindMethod {
			continue
		}
		set := out[e.To]
		if set == nil {
			set = make(map[string]*graph.Node)
			out[e.To] = set
		}
		set[method.Name] = method
	}
	return out
}

// structuralParentEdges returns every EdgeExtends / EdgeImplements /
// EdgeComposes edge whose endpoints are both KindType / KindInterface,
// projected as the (FromID, ToID, Origin) tuples InferOverrides
// consumes. Routed through the storage layer's StructuralParentEdges
// capability when the backend implements it (one Cypher join with
// kind filters on both sides — no per-edge GetNode); falls back to
// the AllEdges + per-edge GetNode walk otherwise.
func structuralParentEdges(g graph.Store) []graph.StructuralParentEdgeRow {
	if cap, ok := g.(graph.StructuralParentEdges); ok {
		return cap.StructuralParentEdges()
	}
	parentKinds := map[graph.EdgeKind]bool{
		graph.EdgeExtends:    true,
		graph.EdgeImplements: true,
		graph.EdgeComposes:   true,
	}
	var out []graph.StructuralParentEdgeRow
	for _, e := range g.AllEdges() {
		if e == nil || !parentKinds[e.Kind] {
			continue
		}
		from := g.GetNode(e.From)
		to := g.GetNode(e.To)
		if from == nil || to == nil {
			continue
		}
		if from.Kind != graph.KindType && from.Kind != graph.KindInterface {
			continue
		}
		if to.Kind != graph.KindType && to.Kind != graph.KindInterface {
			continue
		}
		out = append(out, graph.StructuralParentEdgeRow{
			FromID:   from.ID,
			ToID:     to.ID,
			FromKind: from.Kind,
			ToKind:   to.Kind,
			Origin:   e.Origin,
		})
	}
	return out
}

// InferImplements detects structural interface satisfaction by comparing
// method sets and adds EdgeImplements edges from types to interfaces.
// Returns the number of edges added.
func (r *Resolver) InferImplements() int {
	// Step 1: Collect all interfaces with their required method names.
	type ifaceInfo struct {
		id      string
		repo    string
		methods map[string]bool
	}
	var ifaces []ifaceInfo

	for n := range r.graph.NodesByKind(graph.KindInterface) {
		if n.Meta == nil {
			continue
		}
		raw, ok := n.Meta["methods"]
		if !ok {
			continue
		}
		// Meta["methods"] may be []string or []any (after JSON round-trip).
		methodSet := make(map[string]bool)
		switch v := raw.(type) {
		case []string:
			for _, m := range v {
				methodSet[m] = true
			}
		case []any:
			for _, m := range v {
				if s, ok := m.(string); ok {
					methodSet[s] = true
				}
			}
		}
		if len(methodSet) == 0 {
			continue
		}
		ifaces = append(ifaces, ifaceInfo{id: n.ID, repo: n.RepoPrefix, methods: methodSet})
	}

	if len(ifaces) == 0 {
		return 0
	}

	// Step 2: Build map of type ID -> set of method names via EdgeMemberOf edges.
	typeMethods := memberMethodsByType(r.graph)

	// Step 3: For each type, check if its method set satisfies each interface.
	//
	// The (types × interfaces) cross product is embarrassingly parallel —
	// each type's check is independent and the only write is an AddEdge
	// at the end. We chunk types across NumCPU workers, collect pair
	// results into per-worker slices, and apply them serially at the end
	// (AddEdge contends on Graph mutation internally). On large repos
	// like vscode this cuts InferImplements wall time roughly N×.
	type pair struct {
		typeID, ifaceID, filePath string
		line                      int
	}

	typeList := make([]string, 0, len(typeMethods))
	for tid := range typeMethods {
		typeList = append(typeList, tid)
	}

	// Prefetch every type node referenced by EdgeMemberOf in one batch
	// before the workers spin up — on disk backends a per-worker
	// GetNode(typeID) was an N+1 over cgo that the workers' parallelism
	// could not hide.
	typeNodes := r.graph.GetNodesByIDs(typeList)

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	if workers > len(typeList) {
		workers = len(typeList)
	}
	if workers == 0 {
		return 0
	}

	results := make([][]pair, workers)
	var wg sync.WaitGroup
	chunk := (len(typeList) + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start := w * chunk
		end := start + chunk
		if end > len(typeList) {
			end = len(typeList)
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(idx int, slice []string) {
			defer wg.Done()
			var out []pair
			for _, typeID := range slice {
				methods := typeMethods[typeID]
				typeNode := typeNodes[typeID]
				if typeNode == nil || (typeNode.Kind != graph.KindType && typeNode.Kind != graph.KindInterface) {
					continue
				}
				for _, iface := range ifaces {
					if iface.id == typeID {
						continue
					}
					// Repo gate: structural method-set matching across
					// repos is almost always coincidental — every type
					// with a `String()` method would "implement" every
					// other repo's Stringer-shaped interface. Only infer
					// implementation when the type and the interface
					// live in the same repo. A genuine cross-repo
					// implementation still surfaces structurally when
					// the type explicitly embeds / names the interface.
					if typeNode.RepoPrefix != iface.repo {
						continue
					}
					satisfies := true
					for m := range iface.methods {
						if !methods[m] {
							satisfies = false
							break
						}
					}
					if satisfies {
						out = append(out, pair{
							typeID:   typeID,
							ifaceID:  iface.id,
							filePath: typeNode.FilePath,
							line:     typeNode.StartLine,
						})
					}
				}
			}
			results[idx] = out
		}(w, typeList[start:end])
	}
	wg.Wait()

	added := 0
	for _, chunkResults := range results {
		for _, p := range chunkResults {
			r.graph.AddEdge(&graph.Edge{
				From:     p.typeID,
				To:       p.ifaceID,
				Kind:     graph.EdgeImplements,
				FilePath: p.filePath,
				Line:     p.line,
			})
			added++
		}
	}

	return added
}

// InferOverrides materialises EdgeOverrides edges from method-name
// matches between a type and its supertype. Walks every type that has
// at least one EdgeExtends/EdgeImplements/EdgeComposes outgoing edge,
// then for every member of the type emits an EdgeOverrides edge to a
// matching member on the supertype (matched by name). Returns the
// number of new edges added.
//
// Origin tier is ast_resolved when the supertype edge itself was
// ast_resolved (extractor confirmed parent in the same compilation
// unit); ast_inferred when the supertype edge was inferred from name
// (e.g. InferImplements above); preserved when the parent edge was
// already lsp_resolved/lsp_dispatch (the LSP enrichment path stamps
// EdgeOverrides directly with that origin).
//
// This is the AST half of override inference — works without an LSP available.
func (r *Resolver) InferOverrides() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Step 1: index methods by their owning type via EdgeMemberOf.
	typeMembers := memberMethodNodesByType(r.graph) // typeID → name → method node
	if len(typeMembers) == 0 {
		return 0
	}

	// Step 2: for every (child → parent) extends/implements/composes
	// edge, walk the child's methods and emit EdgeOverrides where the
	// parent has a same-named method. Skip if the override edge
	// already exists.
	type overridePair struct {
		from, to *graph.Node
		origin   string
	}
	var pending []overridePair
	for _, row := range structuralParentEdges(r.graph) {
		if row.FromID == row.ToID {
			continue
		}
		childMethods := typeMembers[row.FromID]
		parentMethods := typeMembers[row.ToID]
		if len(childMethods) == 0 || len(parentMethods) == 0 {
			continue
		}
		// Origin selection: track the parent edge's confidence into
		// the override edge so blast-radius queries can filter by
		// min_tier consistently.
		origin := graph.OriginASTInferred
		if row.Origin == graph.OriginASTResolved {
			origin = graph.OriginASTResolved
		} else if rank := graph.OriginRank(row.Origin); rank >= graph.OriginRank(graph.OriginLSPDispatch) {
			origin = row.Origin
		}
		for name, cm := range childMethods {
			pm, ok := parentMethods[name]
			if !ok || pm.ID == cm.ID {
				continue
			}
			pending = append(pending, overridePair{from: cm, to: pm, origin: origin})
		}
	}

	added := 0
	var provBatch []graph.EdgeProvenanceUpdate
	for _, p := range pending {
		// Skip when the edge already exists.
		dup := false
		for _, existing := range r.graph.GetOutEdges(p.from.ID) {
			if existing.Kind == graph.EdgeOverrides && existing.To == p.to.ID {
				dup = true
				// Upgrade the provenance of the existing override edge
				// through SetEdgeProvenanceBatch so the identity change
				// is counted — a bare existing.Origin write would
				// bypass the revision counter. Batched so a large
				// hierarchy pass commits its provenance bumps in
				// chunks on disk backends.
				if graph.OriginRank(existing.Origin) < graph.OriginRank(p.origin) {
					provBatch = append(provBatch, graph.EdgeProvenanceUpdate{Edge: existing, NewOrigin: p.origin})
				}
				break
			}
		}
		if dup {
			continue
		}
		r.graph.AddEdge(&graph.Edge{
			From:            p.from.ID,
			To:              p.to.ID,
			Kind:            graph.EdgeOverrides,
			FilePath:        p.from.FilePath,
			Line:            p.from.StartLine,
			Confidence:      1.0,
			ConfidenceLabel: "EXTRACTED",
			Origin:          p.origin,
		})
		added++
	}
	if len(provBatch) > 0 {
		r.graph.SetEdgeProvenanceBatch(provBatch)
	}
	return added
}

func lastPathComponent(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return path
	}
	return parts[len(parts)-1]
}

// dirMatchesImport reports whether the (repo-relative) directory dir
// genuinely corresponds to importPath. Unlike a bare last-path-component
// match, dir must be a real *suffix* of the import path — so
// `tree-sitter-c/bindings/go` matches `github.com/x/tree-sitter-c/bindings/go`
// but `tree-sitter-dockerfile/bindings/go` does not. This is the
// precision gate that stops a loose `*/go` match from resolving every
// tree-sitter binding to whichever repo happens to sort first.
//
// Used only to authorise *cross-repo* candidates: a precise import-path
// match is real evidence the caller depends on that repo. Same-repo
// candidates don't need it — a same-repo match can't be the cross-repo
// false positive this guards against.
func dirMatchesImport(dir, importPath string) bool {
	if dir == "" || importPath == "" {
		return false
	}
	return dir == importPath || strings.HasSuffix(importPath, "/"+dir)
}

// callerRepoPrefix returns the RepoPrefix of the node that owns the edge's From field.
func (r *Resolver) callerRepoPrefix(e *graph.Edge) string {
	fromNode := r.graph.GetNode(e.From)
	if fromNode != nil {
		return fromNode.RepoPrefix
	}
	return ""
}
