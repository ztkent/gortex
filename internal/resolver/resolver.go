package resolver

import (
	"path/filepath"
	"runtime"
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
	// mu serialises resolution phases against the shared graph.
	// Pointer so every Resolver built from the same *graph.Graph
	// locks the same mutex — necessary for MultiIndexer's per-repo
	// goroutines, each of which spawns its own Resolver instance.
	// Without the shared lock, concurrent ResolveAll passes race on
	// edge mutations (resolveImport writes e.To while another
	// goroutine iterates via graph.AllEdges()).
	mu *sync.Mutex
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
	r.buildProvidesForIndex()
	defer r.clearProvidesForIndex()

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
						crossRepo:  clone.CrossRepo,
						confidence: clone.Confidence,
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
			j.edge.CrossRepo = j.crossRepo
			j.edge.Confidence = j.confidence
			j.edge.Meta = j.meta
			r.graph.ReindexEdge(j.edge, j.oldTo)
		}
	}

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

// ResolveFile resolves unresolved edges originating from a specific file.
func (r *Resolver) ResolveFile(filePath string) *ResolveStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buildDirIndexes()
	defer r.clearDirIndexes()
	r.buildProvidesForIndex()
	defer r.clearProvidesForIndex()

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
// resolved fields (To, CrossRepo, Confidence, Meta) are copied onto
// the real edge and graph.ReindexEdge is called — both serially.
type reindexJob struct {
	edge       *graph.Edge
	oldTo      string
	newTo      string
	crossRepo  bool
	confidence float64
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

	switch {
	case strings.HasPrefix(target, "import::"):
		r.resolveImport(e, strings.TrimPrefix(target, "import::"), stats)
	case strings.HasPrefix(target, "extern::"):
		// Package-qualified call (json.NewEncoder): the parser attached
		// the full import path + symbol so we don't have to guess a
		// receiver type.
		r.resolveExtern(e, strings.TrimPrefix(target, "extern::"), stats)
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
			r.resolveFunctionCall(e, target, stats)
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
		if strings.HasSuffix(dir, "/"+lastPathComponent(importPath)) || dir == importPath || strings.HasSuffix(dir, importPath) {
			e.To = c.ID
			if callerRepo != "" && c.RepoPrefix != "" && c.RepoPrefix != callerRepo {
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
		if crossRepoNode == nil {
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

	// External/unresolvable import — create a stub target ID.
	e.To = "external::" + importPath
	stats.External++
}

func (r *Resolver) resolveFunctionCall(e *graph.Edge, funcName string, stats *ResolveStats) {
	candidates := r.graph.FindNodesByName(funcName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	// Prefer same-package (same directory) match.
	callerFile := e.FilePath
	callerDir := filepath.Dir(callerFile)

	for _, c := range candidates {
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// Fall back to first function match (may be cross-repo).
	callerRepo := r.callerRepoPrefix(e)
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			e.To = c.ID
			if callerRepo != "" && c.RepoPrefix != "" && c.RepoPrefix != callerRepo {
				e.CrossRepo = true
			}
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

// resolveTypeOrFunc resolves unresolved edges that could be either a type
// reference (composite literal, type assertion) or a function reference.
// It first tries to match a type/interface node, then falls back to functions.
func (r *Resolver) resolveTypeOrFunc(e *graph.Edge, name string, stats *ResolveStats) {
	candidates := r.graph.FindNodesByName(name)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerFile := e.FilePath
	callerDir := filepath.Dir(callerFile)

	// Prefer same-package type match.
	for _, c := range candidates {
		if (c.Kind == graph.KindType || c.Kind == graph.KindInterface) &&
			filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// Fall back to any type match.
	callerRepo := r.callerRepoPrefix(e)
	for _, c := range candidates {
		if c.Kind == graph.KindType || c.Kind == graph.KindInterface {
			e.To = c.ID
			if callerRepo != "" && c.RepoPrefix != "" && c.RepoPrefix != callerRepo {
				e.CrossRepo = true
			}
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
			if callerRepo != "" && c.RepoPrefix != "" && c.RepoPrefix != callerRepo {
				e.CrossRepo = true
			}
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

func (r *Resolver) resolveMethodCall(e *graph.Edge, methodName string, stats *ResolveStats) {
	candidates := r.graph.FindNodesByName(methodName)
	if len(candidates) == 0 {
		if r.applyBuiltinIfKnown(e, methodName, stats) {
			return
		}
		stats.Unresolved++
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

	// Final fallback: name-only heuristic (methods first, then functions for pkg.Func() calls).
	for _, c := range candidates {
		if c.Kind == graph.KindMethod && filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind == graph.KindMethod {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	// Package-qualified function calls (e.g. parser.ParseFile) arrive here
	// because the extractor sees "pkg.Func()" as a selector call with "*." prefix.
	for _, c := range candidates {
		if c.Kind == graph.KindFunction && filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind == graph.KindFunction {
			e.To = c.ID
			stats.Resolved++
			return
		}
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
	candidates := r.graph.FindNodesByName(name)
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
	// No same-dir hit: take the first candidate so find_usages can still
	// surface the relationship. Confidence drops to reflect uncertainty.
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
		ifaces = append(ifaces, ifaceInfo{id: n.ID, methods: methodSet})
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

func lastPathComponent(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return path
	}
	return parts[len(parts)-1]
}

// callerRepoPrefix returns the RepoPrefix of the node that owns the edge's From field.
func (r *Resolver) callerRepoPrefix(e *graph.Edge) string {
	fromNode := r.graph.GetNode(e.From)
	if fromNode != nil {
		return fromNode.RepoPrefix
	}
	return ""
}
