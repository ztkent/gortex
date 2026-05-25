// Command store-bench compares the three graph.Store implementations
// (in-memory, bbolt-on-disk, SQLite-on-disk) by running the FULL
// indexer pipeline against the same source repo through each backend.
//
// What changed from the earlier "migration" harness: previously this
// bench built an in-memory reference graph once, then bulk-loaded it
// into each backend via AddBatch. That measured the cost of migrating
// a pre-built graph between stores, NOT the cost of indexing through
// the store. The disk backends' real workload — write per-file batches
// streaming out of the parser — was never exercised, so the numbers
// understated bbolt's per-Tx commit fan-out and overstated sqlite's
// bulk-insert efficiency.
//
// Now each backend gets its own indexer.New(store, ...) call and runs
// the complete IndexCtx pipeline (parse → resolve → search index →
// contracts → clones → stub resolution → external-call synthesis).
// That's apples-to-apples: the same work the daemon would do on a
// cold start, against the backend that would persist it.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	mrand "math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_duckdb"
	"github.com/zzet/gortex/internal/graph/store_ladybug"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/search"
)

// stageReporter prints per-stage timings to stderr so a long-running
// backend (full indexer pipeline through bbolt on a 35k-file repo)
// shows progress instead of looking hung.
type stageReporter struct {
	start time.Time
	last  string
}

func (s *stageReporter) Report(stage string, cur, total int) {
	if stage == s.last && (cur == 0 || (cur != total && cur%5000 != 0)) {
		return
	}
	s.last = stage
	if cur == 0 && total == 0 {
		fmt.Fprintf(os.Stderr, "    [%6.2fs] %s\n", time.Since(s.start).Seconds(), stage)
		return
	}
	fmt.Fprintf(os.Stderr, "    [%6.2fs] %s %d/%d\n", time.Since(s.start).Seconds(), stage, cur, total)
}

type benchResult struct {
	Backend    string
	NodeCount  int
	EdgeCount  int
	IndexMs    float64 // full indexer pipeline wall time
	DiskBytes  int64   // on-disk size after Close (0 for in-memory)
	QueryP50us float64
	QueryP95us float64
	HeapAllocMB float64 // live allocated bytes after GC
	HeapInuseMB float64 // span footprint after GC
	// Per-MCP-tool latency. Each entry is keyed by the MCP tool name
	// (get_symbol, find_usages, get_callers, get_dependencies,
	// search_symbols, get_file_summary) and holds the Store-level
	// operation cost the tool incurs at the persistence layer.
	PerTool map[string]toolStats
	Err     string
}

type toolStats struct {
	P50us float64
	P95us float64
	N     int
}

type queryWorkload struct {
	nodeIDs   []string
	outIDs    []string
	inIDs     []string
	names     []string
	filePaths []string
}

func main() {
	root := flag.String("root", "", "repo root to index (required)")
	workers := flag.Int("workers", runtime.NumCPU(), "indexer parallelism")
	querySize := flag.Int("queries", 1000, "query workload size per backend")
	skipMemory := flag.Bool("skip-memory", false, "skip the in-memory baseline")
	skipSQLite := flag.Bool("skip-sqlite", false, "skip the sqlite backend")
	skipDuckDB := flag.Bool("skip-duckdb", false, "skip the duckdb (columnar SQL) backend")
	skipLadybug := flag.Bool("skip-ladybug", false, "skip the ladybug (embedded Cypher property-graph) backend")
	only := flag.String("only", "", "comma-separated subset to run (memory,sqlite,duckdb,ladybug); overrides skip-* flags")
	vectorCorpus := flag.Int("vectors", 0, "vector corpus size for HNSW bench (0 disables); needs a backend with graph.VectorSearcher")
	vectorDim := flag.Int("vector-dim", 384, "embedding dimensionality (MiniLM-L6-v2 default)")
	vectorQueries := flag.Int("vector-queries", 200, "number of SimilarTo / Search queries to time per backend")
	vectorSeed := flag.Int64("vector-seed", 1, "PRNG seed for deterministic vector generation across backends")
	flag.Parse()
	if *root == "" {
		die("usage: store-bench -root <path>")
	}
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		die("abs: %v", err)
	}

	// Resolve which backends to run. -only overrides every -skip flag.
	wantMem := !*skipMemory
	wantSQLite := !*skipSQLite
	wantDuckDB := !*skipDuckDB
	wantLadybug := !*skipLadybug
	if *only != "" {
		set := map[string]bool{}
		for _, s := range strings.Split(*only, ",") {
			set[strings.TrimSpace(s)] = true
		}
		wantMem, wantSQLite = set["memory"], set["sqlite"]
		wantDuckDB = set["duckdb"]
		wantLadybug = set["ladybug"]
	}

	// vectorBench is non-nil only when -vectors > 0. Generated once
	// so every backend benches against the exact same corpus + the
	// exact same query vectors — apples-to-apples between Ladybug's
	// engine-native HNSW and the in-process baseline.
	var vecBench *vectorWorkload
	if *vectorCorpus > 0 {
		vecBench = newVectorWorkload(*vectorCorpus, *vectorDim, *vectorQueries, *vectorSeed)
	}

	var results []benchResult
	if wantMem {
		fmt.Fprintln(os.Stderr, "[memory] indexing through in-memory Store...")
		results = append(results, runBackend("memory", absRoot, *workers, *querySize, vecBench,
			func() (graph.Store, func() int64, error) {
				return graph.New(), func() int64 { return 0 }, nil
			}))
	}
	if wantSQLite {
		fmt.Fprintln(os.Stderr, "[sqlite] indexing through sqlite on-disk Store...")
		results = append(results, runBackend("sqlite", absRoot, *workers, *querySize, vecBench,
			func() (graph.Store, func() int64, error) {
				dir, err := os.MkdirTemp("", "store-bench-sqlite-*")
				if err != nil {
					return nil, nil, err
				}
				path := filepath.Join(dir, "store.sqlite")
				s, err := store_sqlite.Open(path)
				if err != nil {
					os.RemoveAll(dir)
					return nil, nil, err
				}
				diskFn := func() int64 {
					_ = s.Close()
					return fileSize(path) + fileSize(path+"-wal") + fileSize(path+"-shm")
				}
				return s, diskFn, nil
			}))
	}
	if wantDuckDB {
		fmt.Fprintln(os.Stderr, "[duckdb] indexing through DuckDB (columnar SQL) Store...")
		results = append(results, runBackend("duckdb", absRoot, *workers, *querySize, vecBench,
			func() (graph.Store, func() int64, error) {
				dir, err := os.MkdirTemp("", "store-bench-duckdb-*")
				if err != nil {
					return nil, nil, err
				}
				path := filepath.Join(dir, "store.duckdb")
				s, err := store_duckdb.Open(path)
				if err != nil {
					os.RemoveAll(dir)
					return nil, nil, err
				}
				diskFn := func() int64 {
					_ = s.Close()
					return fileSize(path) + fileSize(path+".wal")
				}
				return s, diskFn, nil
			}))
	}
	if wantLadybug {
		fmt.Fprintln(os.Stderr, "[ladybug] indexing through Ladybug (embedded Cypher property-graph) Store...")
		results = append(results, runBackend("ladybug", absRoot, *workers, *querySize, vecBench,
			func() (graph.Store, func() int64, error) {
				dir, err := os.MkdirTemp("", "store-bench-ladybug-*")
				if err != nil {
					return nil, nil, err
				}
				path := filepath.Join(dir, "store.lbug")
				s, err := store_ladybug.Open(path)
				if err != nil {
					os.RemoveAll(dir)
					return nil, nil, err
				}
				diskFn := func() int64 {
					_ = s.Close()
					return dirSize(path)
				}
				return s, diskFn, nil
			}))
	}

	// In-process HNSW baseline. Reported as a synthetic backend row
	// so the per-tool table can show vector_search side-by-side with
	// every store's engine-native number. The row's index/heap/disk
	// columns are intentionally zeroed — it's a search-only baseline,
	// not a full pipeline run.
	if vecBench != nil {
		fmt.Fprintln(os.Stderr, "[in-process HNSW] running search.VectorBackend baseline...")
		results = append(results, runInProcVectorBaseline(vecBench))
	}

	printTable(os.Stdout, results)
}

// dirSize totals every regular file under root in bytes. Used for
// backends whose persisted state is a directory (Ladybug's
// catalog/data/wal split) rather than a single file.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// runBackend executes the full indexer pipeline through one backend
// and reports the metrics. Each backend gets a fresh Store, a fresh
// Indexer, a fresh query workload sampled from its own populated
// state. The reference-graph step is gone: there is no shared graph
// alive across backends, so heap measurements are not contaminated by
// the previous backend's resident state.
func runBackend(
	name string,
	absRoot string,
	workers int,
	querySize int,
	vec *vectorWorkload,
	factory func() (graph.Store, func() int64, error),
) benchResult {
	r := benchResult{Backend: name}

	store, diskFn, err := factory()
	if err != nil {
		r.Err = "factory: " + err.Error()
		return r
	}

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Config{}
	cfg.Index.Workers = workers

	idx := indexer.New(store, reg, cfg.Index, zap.NewNop())

	rep := &stageReporter{start: time.Now()}
	ctx := progress.WithReporter(context.Background(), rep)

	t0 := time.Now()
	_, err = idx.IndexCtx(ctx, absRoot)
	r.IndexMs = msSince(t0)
	if err != nil {
		r.Err = "index: " + err.Error()
		return r
	}
	r.NodeCount = store.NodeCount()
	r.EdgeCount = store.EdgeCount()

	// Build query workload from THIS backend's populated state. Each
	// backend gets its own deterministic-ish sample so the queries hit
	// genuine state, not random IDs guessed at.
	wl := pickQueriesFromStore(store, querySize)

	r.PerTool = map[string]toolStats{}

	// get_symbol — single node fetch by ID.
	getSym := make([]time.Duration, 0, len(wl.nodeIDs))
	for _, id := range wl.nodeIDs {
		t := time.Now()
		_ = store.GetNode(id)
		getSym = append(getSym, time.Since(t))
	}
	r.PerTool["get_symbol"] = toolStatsFrom(getSym)

	// get_dependencies — outgoing edges from a symbol.
	getDeps := make([]time.Duration, 0, len(wl.outIDs))
	for _, id := range wl.outIDs {
		t := time.Now()
		_ = store.GetOutEdges(id)
		getDeps = append(getDeps, time.Since(t))
	}
	r.PerTool["get_dependencies"] = toolStatsFrom(getDeps)

	// find_usages — incoming references edges.
	findUses := make([]time.Duration, 0, len(wl.inIDs))
	for _, id := range wl.inIDs {
		t := time.Now()
		edges := store.GetInEdges(id)
		_ = filterEdgeKind(edges, graph.EdgeReferences)
		findUses = append(findUses, time.Since(t))
	}
	r.PerTool["find_usages"] = toolStatsFrom(findUses)

	// get_callers — incoming call edges.
	getCallers := make([]time.Duration, 0, len(wl.inIDs))
	for _, id := range wl.inIDs {
		t := time.Now()
		edges := store.GetInEdges(id)
		_ = filterEdgeKind(edges, graph.EdgeCalls)
		getCallers = append(getCallers, time.Since(t))
	}
	r.PerTool["get_callers"] = toolStatsFrom(getCallers)

	// search_symbols — name lookup (Store-level; the BM25 rerank on top
	// is backend-independent).
	searchSym := make([]time.Duration, 0, len(wl.names))
	for _, n := range wl.names {
		t := time.Now()
		_ = store.FindNodesByName(n)
		searchSym = append(searchSym, time.Since(t))
	}
	r.PerTool["search_symbols"] = toolStatsFrom(searchSym)

	// get_file_summary — all symbols in a file.
	getFile := make([]time.Duration, 0, len(wl.filePaths))
	for _, fp := range wl.filePaths {
		t := time.Now()
		_ = store.GetFileNodes(fp)
		getFile = append(getFile, time.Since(t))
	}
	r.PerTool["get_file_summary"] = toolStatsFrom(getFile)

	// vector_search — engine-native HNSW via graph.VectorSearcher.
	// The vector workload is generated once (deterministic seed) so
	// every backend sees identical inputs; the in-process baseline at
	// the bottom of the table uses the same workload for comparison.
	// Skipped when -vectors=0 or the backend doesn't implement the
	// capability — leaving the cell blank keeps the column honest.
	if vec != nil && vec.corpus > 0 {
		if vs, ok := store.(graph.VectorSearcher); ok && len(wl.nodeIDs) > 0 {
			items := vec.itemsForIDs(wl.nodeIDs)
			if len(items) > 0 {
				if err := vs.BulkUpsertEmbeddings(items); err != nil {
					fmt.Fprintf(os.Stderr, "    [vector_search] %s BulkUpsertEmbeddings: %v\n", name, err)
				} else if err := vs.BuildVectorIndex(vec.dim); err != nil {
					fmt.Fprintf(os.Stderr, "    [vector_search] %s BuildVectorIndex: %v\n", name, err)
				} else {
					vecSearch := make([]time.Duration, 0, vec.queries)
					for i := 0; i < vec.queries; i++ {
						q := vec.queryVecs[i%len(vec.queryVecs)]
						t := time.Now()
						_, _ = vs.SimilarTo(q, 20)
						vecSearch = append(vecSearch, time.Since(t))
					}
					r.PerTool["vector_search"] = toolStatsFrom(vecSearch)
				}
			}
		}
	}

	// Graph-algorithm timings: pagerank / louvain / wcc / scc / kcore.
	// Each cell is a single wall-clock measurement of the algorithm
	// running over the populated store. For backends that implement
	// the capability interface (today only ladybug) we time the
	// engine-native CALL; for the memory backend (which IS *graph.Graph)
	// we time the in-process analysis.* fallback. sqlite / duckdb
	// don't get a number — converting their state into *graph.Graph
	// would add a one-time copy cost that would dominate the
	// measurement and make the comparison meaningless.
	measureAlgos(store, &r)

	// fts_search — backend-native full-text search via the
	// graph.SymbolSearcher capability. Bypasses BM25/Bleve entirely
	// and measures the disk store's own FTS round-trip. Skipped on
	// backends that don't implement the capability so the column
	// stays meaningful (zeroes for non-FTS stores would imply
	// "instant" which is false). Workload mirrors search_symbols:
	// every sampled node name becomes one query.
	if searcher, ok := store.(graph.SymbolSearcher); ok && len(wl.names) > 0 {
		// Build the FTS index on the corpus we just populated.
		// BuildSymbolIndex is idempotent; the indexer also calls
		// it post-drain so this is a defensive belt+suspenders
		// for store-bench's standalone runtime.
		_ = searcher.BuildSymbolIndex()
		ftsSearch := make([]time.Duration, 0, len(wl.names))
		for _, n := range wl.names {
			t := time.Now()
			_, _ = searcher.SearchSymbols(n, 20)
			ftsSearch = append(ftsSearch, time.Since(t))
		}
		r.PerTool["fts_search"] = toolStatsFrom(ftsSearch)
	}

	// Legacy aggregate (kept for the headline number in the main table).
	all := append(append(append(append(append(getSym, getDeps...), findUses...), getCallers...), searchSym...), getFile...)
	r.QueryP50us = pctUs(all, 50)
	r.QueryP95us = pctUs(all, 95)

	// Sample heap. Force GC first so the figure reflects retained
	// state (the live graph + indexer state), not allocation churn
	// from the workload loop. Report both HeapAlloc (live bytes,
	// the honest "how much does the daemon really need" number) and
	// HeapInuse (span footprint, what `ps` would show).
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	r.HeapAllocMB = float64(m.HeapAlloc) / 1e6
	r.HeapInuseMB = float64(m.HeapInuse) / 1e6

	// On-disk size — diskFn closes the store and stats the file.
	r.DiskBytes = diskFn()

	return r
}

// pickQueriesFromStore samples a deterministic-ish query workload
// from a populated Store. Uses AllNodes (which every backend
// implements) so the sampling code stays backend-agnostic.
func pickQueriesFromStore(s graph.Store, n int) queryWorkload {
	nodes := s.AllNodes()
	if len(nodes) == 0 {
		return queryWorkload{}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	pickN := func(count int) []*graph.Node {
		if count >= len(nodes) {
			out := make([]*graph.Node, len(nodes))
			copy(out, nodes)
			return out
		}
		out := make([]*graph.Node, 0, count)
		seen := make(map[int]bool, count)
		for len(out) < count {
			var b [4]byte
			_, _ = rand.Read(b[:])
			i := int(binary.BigEndian.Uint32(b[:])) % len(nodes)
			if seen[i] {
				continue
			}
			seen[i] = true
			out = append(out, nodes[i])
		}
		return out
	}

	sampleNodes := pickN(n)
	wl := queryWorkload{
		nodeIDs: make([]string, 0, n),
		outIDs:  make([]string, 0, n/2),
		inIDs:   make([]string, 0, n/2),
	}
	nameSet := map[string]struct{}{}
	fileSet := map[string]struct{}{}
	for i, nd := range sampleNodes {
		wl.nodeIDs = append(wl.nodeIDs, nd.ID)
		if i%2 == 0 {
			wl.outIDs = append(wl.outIDs, nd.ID)
		} else {
			wl.inIDs = append(wl.inIDs, nd.ID)
		}
		nameSet[nd.Name] = struct{}{}
		if nd.FilePath != "" {
			fileSet[nd.FilePath] = struct{}{}
		}
	}
	for k := range nameSet {
		wl.names = append(wl.names, k)
	}
	for k := range fileSet {
		wl.filePaths = append(wl.filePaths, k)
	}
	if len(wl.names) > n/4 {
		wl.names = wl.names[:n/4]
	}
	if len(wl.filePaths) > n/4 {
		wl.filePaths = wl.filePaths[:n/4]
	}
	return wl
}

// measureAlgos times the five graph algorithms (pagerank, louvain,
// wcc, scc, kcore) over the populated store. Each cell is one
// wall-clock measurement of the algorithm running once.
//
// Routing per backend:
//   - implements the capability interface → time the engine-native
//     CALL.
//   - is *graph.Graph (the memory backend) → time the in-process
//     analysis.* fallback over the same graph the indexer wrote
//     into.
//   - anything else → skip (zeroing the cell for sqlite/duckdb
//     would imply "instant" which is false).
//
// Each cell holds a single-sample p50 / p95 — both are the same
// value, the per-tool table column shape just expects the
// toolStats triple.
func measureAlgos(store graph.Store, r *benchResult) {
	g, _ := store.(*graph.Graph)

	if pr, ok := store.(graph.PageRanker); ok {
		t := time.Now()
		_, _ = pr.PageRank(graph.PageRankOpts{Limit: 20})
		r.PerTool["pagerank"] = singleSample(time.Since(t))
	} else if g != nil {
		t := time.Now()
		_ = analysis.ComputePageRank(g)
		r.PerTool["pagerank"] = singleSample(time.Since(t))
	}

	if cd, ok := store.(graph.CommunityDetector); ok {
		t := time.Now()
		_, _ = cd.Louvain(graph.CommunityOpts{})
		r.PerTool["louvain"] = singleSample(time.Since(t))
	} else if g != nil {
		t := time.Now()
		_ = analysis.DetectCommunitiesLouvain(g)
		r.PerTool["louvain"] = singleSample(time.Since(t))
	}

	if cf, ok := store.(graph.ComponentFinder); ok {
		t := time.Now()
		_, _ = cf.WeaklyConnectedComponents(graph.ComponentOpts{})
		r.PerTool["wcc"] = singleSample(time.Since(t))
		t = time.Now()
		_, _ = cf.StronglyConnectedComponents(graph.ComponentOpts{})
		r.PerTool["scc"] = singleSample(time.Since(t))
	} else if g != nil {
		t := time.Now()
		_ = analysis.ComputeWCC(g, analysis.ComponentOptions{})
		r.PerTool["wcc"] = singleSample(time.Since(t))
		t = time.Now()
		_ = analysis.ComputeSCC(g, analysis.ComponentOptions{})
		r.PerTool["scc"] = singleSample(time.Since(t))
	}

	if kc, ok := store.(graph.KCorer); ok {
		t := time.Now()
		_, _ = kc.KCoreDecomposition(graph.KCoreOpts{})
		r.PerTool["kcore"] = singleSample(time.Since(t))
	} else if g != nil {
		t := time.Now()
		_ = analysis.ComputeKCore(g, analysis.KCoreOptions{})
		r.PerTool["kcore"] = singleSample(time.Since(t))
	}
}

// singleSample turns a one-shot measurement into the toolStats
// triple the per-tool table prints. Both p50 and p95 land on
// the same value; N is 1.
func singleSample(d time.Duration) toolStats {
	us := float64(d.Microseconds())
	return toolStats{P50us: us, P95us: us, N: 1}
}

// vectorWorkload is the shared corpus + query set fed to every
// VectorSearcher-implementing backend AND to the in-process HNSW
// baseline. Generating it once (deterministic seed) guarantees the
// Ladybug-vs-in-process comparison is apples-to-apples: same vector
// distribution, same query vectors, same k.
type vectorWorkload struct {
	corpus    int
	dim       int
	queries   int
	corpusVec [][]float32 // length corpus
	queryVecs [][]float32 // length queries
}

// newVectorWorkload generates the shared vector corpus + query set.
// Each vector is L2-normalised — HNSW under cosine distance behaves
// best on unit-norm inputs, matching the embedder's output. The
// seed is the user-supplied -vector-seed so re-runs are reproducible.
func newVectorWorkload(corpus, dim, queries int, seed int64) *vectorWorkload {
	if corpus <= 0 || dim <= 0 || queries <= 0 {
		return nil
	}
	rng := mrand.New(mrand.NewSource(seed))
	wl := &vectorWorkload{
		corpus:    corpus,
		dim:       dim,
		queries:   queries,
		corpusVec: make([][]float32, corpus),
		queryVecs: make([][]float32, queries),
	}
	for i := 0; i < corpus; i++ {
		wl.corpusVec[i] = randomUnitVec(rng, dim)
	}
	for i := 0; i < queries; i++ {
		wl.queryVecs[i] = randomUnitVec(rng, dim)
	}
	return wl
}

// itemsForIDs pairs node IDs with vectors from the corpus. The
// corpus may be shorter or longer than the IDs slice — we use
// modular indexing so every ID gets a stable vector regardless of
// the populated store size.
func (w *vectorWorkload) itemsForIDs(ids []string) []graph.VectorItem {
	out := make([]graph.VectorItem, 0, len(ids))
	if w == nil || len(w.corpusVec) == 0 {
		return out
	}
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, graph.VectorItem{
			NodeID: id,
			Vec:    w.corpusVec[i%len(w.corpusVec)],
		})
	}
	return out
}

func randomUnitVec(rng *mrand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	var sum float64
	for i := 0; i < dim; i++ {
		// Box-Muller-ish normal-ish without the heavy machinery; uniform
		// in [-1,1] is plenty for an HNSW microbenchmark.
		x := rng.Float32()*2 - 1
		v[i] = x
		sum += float64(x * x)
	}
	if sum == 0 {
		v[0] = 1
		return v
	}
	inv := float32(1.0 / sqrt(sum))
	for i := 0; i < dim; i++ {
		v[i] *= inv
	}
	return v
}

func sqrt(x float64) float64 {
	// Local Newton-Raphson to dodge math import noise; cheap enough
	// for setup-time work.
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 16; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}

// runInProcVectorBaseline times the same Add/Search workload through
// search.VectorBackend (in-process HNSW). Returned as a benchResult
// with only PerTool["vector_search"] populated — the other columns
// are deliberately zeroed so the caller knows this row is search-
// only, not a full pipeline run.
func runInProcVectorBaseline(vec *vectorWorkload) benchResult {
	r := benchResult{Backend: "(in-process HNSW)", PerTool: map[string]toolStats{}}
	if vec == nil || vec.corpus == 0 {
		return r
	}
	v := search.NewVector(vec.dim)
	for i := 0; i < vec.corpus; i++ {
		v.Add(fmt.Sprintf("n%07d", i), vec.corpusVec[i])
	}
	r.NodeCount = vec.corpus
	samples := make([]time.Duration, 0, vec.queries)
	for i := 0; i < vec.queries; i++ {
		q := vec.queryVecs[i%len(vec.queryVecs)]
		t := time.Now()
		_ = v.Search(q, 20)
		samples = append(samples, time.Since(t))
	}
	r.PerTool["vector_search"] = toolStatsFrom(samples)
	// Heap snapshot reflects the in-process HNSW's footprint after
	// the corpus has been loaded — the headline "what does the
	// daemon save by delegating to Ladybug" number.
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	r.HeapAllocMB = float64(m.HeapAlloc) / 1e6
	r.HeapInuseMB = float64(m.HeapInuse) / 1e6
	return r
}

func toolStatsFrom(latencies []time.Duration) toolStats {
	return toolStats{
		P50us: pctUs(latencies, 50),
		P95us: pctUs(latencies, 95),
		N:     len(latencies),
	}
}

func filterEdgeKind(edges []*graph.Edge, kind graph.EdgeKind) []*graph.Edge {
	out := edges[:0]
	for _, e := range edges {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// -- output -----------------------------------------------------------------

func printTable(w *os.File, rows []benchResult) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "# Store backend comparison (full indexer pipeline per backend)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "| backend | nodes | edges | index | disk size | heap (alloc / inuse) | query p50 | query p95 |")
	fmt.Fprintln(w, "|---------|------:|------:|------:|----------:|---------------------:|----------:|----------:|")
	for _, r := range rows {
		if r.Err != "" {
			fmt.Fprintf(w, "| %s | — | — | — | — | — | — | %s |\n", r.Backend, r.Err)
			continue
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s / %s | %s | %s |\n",
			r.Backend,
			fmtInt(r.NodeCount),
			fmtInt(r.EdgeCount),
			fmtMs(r.IndexMs),
			fmtBytes(r.DiskBytes),
			fmtMB(r.HeapAllocMB),
			fmtMB(r.HeapInuseMB),
			fmtUs(r.QueryP50us),
			fmtUs(r.QueryP95us),
		)
	}
	fmt.Fprintln(w, "")

	// Per-MCP-tool latency table. One row per backend, one column per
	// tool. Each cell is "p50 / p95" of the Store-level call the tool
	// runs at the persistence layer.
	tools := []string{
		"get_symbol", "get_dependencies", "find_usages", "get_callers",
		"search_symbols", "get_file_summary",
		"fts_search", "vector_search",
		"pagerank", "louvain", "wcc", "scc", "kcore",
	}
	fmt.Fprintln(w, "# Per-MCP-tool latency (Store-level p50 / p95)")
	fmt.Fprintln(w, "")
	fmt.Fprint(w, "| backend |")
	for _, t := range tools {
		fmt.Fprintf(w, " %s |", t)
	}
	fmt.Fprintln(w)
	fmt.Fprint(w, "|---------|")
	for range tools {
		fmt.Fprint(w, "------------------:|")
	}
	fmt.Fprintln(w)
	for _, r := range rows {
		if r.Err != "" || r.PerTool == nil {
			continue
		}
		fmt.Fprintf(w, "| %s |", r.Backend)
		for _, t := range tools {
			s := r.PerTool[t]
			fmt.Fprintf(w, " %s / %s |", fmtUs(s.P50us), fmtUs(s.P95us))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
}

// -- small helpers ----------------------------------------------------------

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }

func pctMs(samples []time.Duration, pct int) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted) * pct) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx].Microseconds()) / 1000.0
}

func pctUs(samples []time.Duration, pct int) float64 {
	return pctMs(samples, pct) * 1000.0
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func fmtInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func fmtMs(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.2fs", ms/1000)
	}
	return fmt.Sprintf("%.1fms", ms)
}

func fmtUs(us float64) string {
	if us >= 1000 {
		return fmt.Sprintf("%.2fms", us/1000)
	}
	return fmt.Sprintf("%.1fµs", us)
}

func fmtMB(mb float64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.2fGB", mb/1024)
	}
	return fmt.Sprintf("%.0fMB", mb)
}

func fmtBytes(b int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case b == 0:
		return "—"
	case b >= GB:
		return fmt.Sprintf("%.2fGB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1fMB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1fKB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func die(format string, args ...any) {
	fmt.Fprintln(os.Stderr, fmt.Sprintf(format, args...))
	os.Exit(1)
}
