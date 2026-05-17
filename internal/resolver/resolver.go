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
	graph        *graph.Graph
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
	// Pointer so every Resolver built from the same *graph.Graph
	// locks the same mutex — necessary for MultiIndexer's per-repo
	// goroutines, each of which spawns its own Resolver instance.
	// Without the shared lock, concurrent ResolveAll passes race on
	// edge mutations (resolveImport writes e.To while another
	// goroutine iterates via graph.AllEdges()).
	mu *sync.Mutex

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

// New creates a Resolver for the given graph. The returned Resolver
// shares graph.ResolveMutex() with every other Resolver built from
// the same Graph, so their ResolveAll / ResolveFile calls serialise
// end-to-end.
func New(g *graph.Graph) *Resolver {
	return &Resolver{graph: g, mu: g.ResolveMutex()}
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

	edges := r.graph.AllEdges()
	// Pre-filter to the unresolved subset so workers don't burn time
	// re-walking the whole edge slice — ~95% of edges in a settled
	// graph are already resolved.
	pending := edges[:0:0]
	for _, e := range edges {
		if strings.HasPrefix(e.To, unresolvedPrefix) {
			pending = append(pending, e)
		}
	}
	if len(pending) == 0 {
		return &ResolveStats{}
	}

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
	for i := range perWorkerJobs {
		for _, j := range perWorkerJobs[i] {
			j.edge.To = j.newTo
			j.edge.Kind = j.kind
			j.edge.CrossRepo = j.crossRepo
			j.edge.Confidence = j.confidence
			j.edge.Origin = j.origin
			j.edge.Meta = j.meta
			r.graph.ReindexEdge(j.edge, j.oldTo)
		}
	}

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
	nodes := r.graph.AllNodes()
	r.dirIndex = make(map[string][]*graph.Node, len(nodes)/4)
	r.lastDirIndex = make(map[string][]*graph.Node, len(nodes)/4)
	for _, n := range nodes {
		if n.Kind != graph.KindFile {
			continue
		}
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
	nodes := r.graph.AllNodes()
	by := make(map[string][]depModuleEntry)
	for _, n := range nodes {
		if n.Kind != graph.KindContract {
			continue
		}
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
	// Single-threaded path — apply ReindexEdge inline as before.
	nodes := r.graph.GetFileNodes(filePath)
	for _, n := range nodes {
		edges := r.graph.GetOutEdges(n.ID)
		for _, e := range edges {
			if !strings.HasPrefix(e.To, unresolvedPrefix) {
				continue
			}
			oldTo, changed := r.resolveEdge(e, stats)
			if changed {
				r.graph.ReindexEdge(e, oldTo)
			}
		}
	}
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

// cloneEdgeForResolve returns a deep-enough copy of e for safe
// worker-local mutation by resolveEdge: every scalar / string field
// is value-copied; Meta is duplicated when present so a helper
// writing `clone.Meta["resolution"] = ...` doesn't mutate a map
// shared with the original (and therefore with other goroutines
// inspecting that map). Meta is the only reference-typed field on
// Edge that resolveEdge may write to today; any future Edge field
// of map / slice type will need handling here too.
func cloneEdgeForResolve(e *graph.Edge) *graph.Edge {
	clone := *e
	if clone.Meta != nil {
		dup := make(map[string]any, len(clone.Meta))
		for k, v := range clone.Meta {
			dup[k] = v
		}
		clone.Meta = dup
	}
	return &clone
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
				if n := r.graph.GetNode(e.To); n != nil && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
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
	callerRepo := r.callerRepoPrefix(e)
	candidates := r.graph.FindNodesByName(symbol)
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
	// both as external for edge-walk purposes.
	prefix := "dep::"
	if isStdlibLike(importPath) {
		prefix = "stdlib::"
	}
	e.To = prefix + importPath + "::" + symbol
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
	var sameRepo, crossRepoNode *graph.Node
	consider := func(n *graph.Node) {
		if n.Kind != graph.KindFile {
			return
		}
		if callerRepo == "" || n.RepoPrefix == callerRepo {
			if sameRepo == nil {
				sameRepo = n
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
	if r.dirIndex != nil {
		for _, n := range r.dirIndex[importPath] {
			consider(n)
			if sameRepo != nil {
				break
			}
		}
		if sameRepo == nil {
			for _, n := range r.lastDirIndex[lastPathComponent(importPath)] {
				consider(n)
				if sameRepo != nil {
					break
				}
			}
		}
	} else {
		for _, n := range r.graph.AllNodes() {
			if n.Kind != graph.KindFile {
				continue
			}
			dir := filepath.Dir(n.FilePath)
			if strings.HasSuffix(dir, lastPathComponent(importPath)) || dir == importPath {
				consider(n)
				if sameRepo != nil {
					break
				}
			}
		}
	}

	if sameRepo != nil {
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

	// External/unresolvable import — create a stub target ID.
	e.To = "external::" + importPath
	stats.External++
}

func (r *Resolver) resolveFunctionCall(e *graph.Edge, funcName string, stats *ResolveStats) {
	callerRepo := r.callerRepoPrefix(e)
	candidates := filterSameRepo(callerRepo, r.graph.FindNodesByName(funcName))
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
	candidates := filterSameRepo(callerRepo, r.graph.FindNodesByName(name))
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
	candidates := filterSameRepo(callerRepo, r.graph.FindNodesByName(name))
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
	candidates := filterSameRepo(r.callerRepoPrefix(e), r.graph.FindNodesByName(fieldName))
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
	rawCandidates := filterSameRepo(r.callerRepoPrefix(e), r.graph.FindNodesByName(methodName))
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
	e.To = "builtin::" + lang + "::" + category + "::" + methodName
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
	candidates := filterSameRepo(r.callerRepoPrefix(e), r.graph.FindNodesByName(name))
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
	for _, ed := range r.graph.AllEdges() {
		if ed.Kind != graph.EdgeProvides || ed.Meta == nil {
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
	for _, n := range r.graph.AllNodes() {
		if n.Kind != graph.KindFile {
			continue
		}
		addDir(n.ID, filepath.Dir(n.FilePath))
	}

	for _, e := range r.graph.AllEdges() {
		if e.Kind != graph.EdgeImports {
			continue
		}
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

	allNodes := r.graph.AllNodes()
	for _, n := range allNodes {
		if n.Kind != graph.KindInterface {
			continue
		}
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
	typeMethods := make(map[string]map[string]bool)
	allEdges := r.graph.AllEdges()
	for _, e := range allEdges {
		if e.Kind != graph.EdgeMemberOf {
			continue
		}
		// EdgeMemberOf: From=method, To=type
		methodNode := r.graph.GetNode(e.From)
		if methodNode == nil || methodNode.Kind != graph.KindMethod {
			continue
		}
		typeID := e.To
		if typeMethods[typeID] == nil {
			typeMethods[typeID] = make(map[string]bool)
		}
		typeMethods[typeID][methodNode.Name] = true
	}

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
				typeNode := r.graph.GetNode(typeID)
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
	typeMembers := make(map[string]map[string]*graph.Node) // typeID → name → method node
	for _, e := range r.graph.AllEdges() {
		if e.Kind != graph.EdgeMemberOf {
			continue
		}
		method := r.graph.GetNode(e.From)
		if method == nil || method.Kind != graph.KindMethod {
			continue
		}
		set := typeMembers[e.To]
		if set == nil {
			set = make(map[string]*graph.Node)
			typeMembers[e.To] = set
		}
		set[method.Name] = method
	}
	if len(typeMembers) == 0 {
		return 0
	}

	// Step 2: for every (child → parent) extends/implements/composes
	// edge, walk the child's methods and emit EdgeOverrides where the
	// parent has a same-named method. Skip if the override edge
	// already exists.
	parentKinds := map[graph.EdgeKind]bool{
		graph.EdgeExtends:    true,
		graph.EdgeImplements: true,
		graph.EdgeComposes:   true,
	}
	type overridePair struct {
		from, to *graph.Node
		origin   string
	}
	var pending []overridePair
	for _, e := range r.graph.AllEdges() {
		if !parentKinds[e.Kind] {
			continue
		}
		child := r.graph.GetNode(e.From)
		parent := r.graph.GetNode(e.To)
		if child == nil || parent == nil || child.ID == parent.ID {
			continue
		}
		if child.Kind != graph.KindType && child.Kind != graph.KindInterface {
			continue
		}
		if parent.Kind != graph.KindType && parent.Kind != graph.KindInterface {
			continue
		}
		childMethods := typeMembers[child.ID]
		parentMethods := typeMembers[parent.ID]
		if len(childMethods) == 0 || len(parentMethods) == 0 {
			continue
		}
		// Origin selection: track the parent edge's confidence into
		// the override edge so blast-radius queries can filter by
		// min_tier consistently.
		origin := graph.OriginASTInferred
		if e.Origin == graph.OriginASTResolved {
			origin = graph.OriginASTResolved
		} else if rank := graph.OriginRank(e.Origin); rank >= graph.OriginRank(graph.OriginLSPDispatch) {
			origin = e.Origin
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
	for _, p := range pending {
		// Skip when the edge already exists.
		dup := false
		for _, existing := range r.graph.GetOutEdges(p.from.ID) {
			if existing.Kind == graph.EdgeOverrides && existing.To == p.to.ID {
				dup = true
				if graph.OriginRank(existing.Origin) < graph.OriginRank(p.origin) {
					existing.Origin = p.origin
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

// filterSameRepo drops every candidate proven to live in a different
// repo than callerRepo. Empty callerRepo (single-repo graph) or empty
// candidate RepoPrefix (synthetic / stdlib nodes) is treated as
// same-repo — the per-repo Resolver only ever rejects a *provably*
// cross-repo candidate. Genuine cross-repo resolution is left to
// CrossRepoResolver, which alone carries the workspace-boundary and
// import-reachability evidence needed to cross a repo line safely.
//
// Returns a fresh slice so the caller's input is never aliased.
func filterSameRepo(callerRepo string, candidates []*graph.Node) []*graph.Node {
	if callerRepo == "" {
		return candidates
	}
	out := make([]*graph.Node, 0, len(candidates))
	for _, c := range candidates {
		if c.RepoPrefix == "" || c.RepoPrefix == callerRepo {
			out = append(out, c)
		}
	}
	return out
}

// callerRepoPrefix returns the RepoPrefix of the node that owns the edge's From field.
func (r *Resolver) callerRepoPrefix(e *graph.Edge) string {
	fromNode := r.graph.GetNode(e.From)
	if fromNode != nil {
		return fromNode.RepoPrefix
	}
	return ""
}
