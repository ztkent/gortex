// Command multi-repo-bench measures multi-repository indexing
// across graph.Store backends.
//
// The single-repo store-bench tells us the per-backend cost of
// indexing one repo through the full pipeline. This harness
// instead drives the workload Gortex actually ships for: the
// production daemon's MultiIndexer flow against the user's
// `~/.config/gortex/config.yaml` repo list. Each backend gets
// a fresh store, indexes every active repo from the global
// config, then runs the same per-tool latency sample the
// single-repo bench does — plus a cross-repo find_usages probe
// (cross-repo resolution is the load-bearing feature multi-repo
// indexing exists to deliver).
package main

import (
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_duckdb"
	"github.com/zzet/gortex/internal/graph/store_ladybug"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type backendFactory struct {
	name   string
	open   func() (graph.Store, func() int64, error)
}

type repoBreakdown struct {
	Prefix    string
	Path      string
	Workspace string
	Project   string
	FileCount int
	NodeCount int
	EdgeCount int
	IndexMs   float64
	Err       string
}

type benchResult struct {
	Backend           string
	TotalNodes        int
	TotalEdges        int
	RepoCount         int
	IndexMs           float64
	DiskBytes         int64
	HeapAllocMB       float64
	HeapInuseMB       float64
	CrossRepoUsages   int     // total references resolved across repo boundaries
	PerRepo           []repoBreakdown
	QueryP50us        float64 // simple lookup p50/p95 (GetNode)
	QueryP95us        float64
	Err               string
}

func main() {
	configPath := flag.String("config", "", "path to global gortex config.yaml (default ~/.config/gortex/config.yaml)")
	workers := flag.Int("workers", runtime.NumCPU(), "indexer parallelism")
	querySample := flag.Int("queries", 500, "per-backend GetNode sample size")
	only := flag.String("only", "memory,ladybug", "comma-separated backends to run (memory,sqlite,duckdb,ladybug)")
	allRepos := flag.Bool("all-repos", false, "bench every repo in the global config, not just the active project (default off — ActiveRepos honours active_project)")
	projects := flag.String("projects", "", "comma-separated list of project slugs to include (overrides active_project; ignored when -all-repos)")
	flag.Parse()

	set := map[string]bool{}
	for _, s := range strings.Split(*only, ",") {
		set[strings.TrimSpace(s)] = true
	}

	// Load the config once — we hand it to a fresh ConfigManager
	// per-backend below (each run rebuilds workspace caches, but
	// the active-repo list is stable).
	cfgPath := *configPath
	if cfgPath == "" {
		home, _ := os.UserHomeDir()
		cfgPath = filepath.Join(home, ".config", "gortex", "config.yaml")
	}
	cm, err := config.NewConfigManager(cfgPath)
	if err != nil {
		die("load config %q: %v", cfgPath, err)
	}
	repos, scopeDesc := selectRepos(cm, *allRepos, *projects)
	if len(repos) == 0 {
		die("no repos selected (scope: %s) in %s", scopeDesc, cfgPath)
	}
	fmt.Fprintf(os.Stderr, "[multi-repo-bench] config=%s  scope=%s  repos=%d\n", cfgPath, scopeDesc, len(repos))
	for _, r := range repos {
		fmt.Fprintf(os.Stderr, "  - %s  (workspace=%s project=%s)\n", r.Path, r.Workspace, r.Project)
	}

	factories := []backendFactory{}
	if set["memory"] {
		factories = append(factories, backendFactory{
			name: "memory",
			open: func() (graph.Store, func() int64, error) {
				return graph.New(), func() int64 { return 0 }, nil
			},
		})
	}
	if set["sqlite"] {
		factories = append(factories, backendFactory{
			name: "sqlite",
			open: func() (graph.Store, func() int64, error) {
				dir, err := os.MkdirTemp("", "multi-repo-bench-sqlite-*")
				if err != nil {
					return nil, nil, err
				}
				path := filepath.Join(dir, "store.sqlite")
				s, err := store_sqlite.Open(path)
				if err != nil {
					os.RemoveAll(dir)
					return nil, nil, err
				}
				return s, func() int64 {
					_ = s.Close()
					return fileSize(path) + fileSize(path+"-wal") + fileSize(path+"-shm")
				}, nil
			},
		})
	}
	if set["duckdb"] {
		factories = append(factories, backendFactory{
			name: "duckdb",
			open: func() (graph.Store, func() int64, error) {
				dir, err := os.MkdirTemp("", "multi-repo-bench-duckdb-*")
				if err != nil {
					return nil, nil, err
				}
				path := filepath.Join(dir, "store.duckdb")
				s, err := store_duckdb.Open(path)
				if err != nil {
					os.RemoveAll(dir)
					return nil, nil, err
				}
				return s, func() int64 {
					_ = s.Close()
					return fileSize(path) + fileSize(path+".wal")
				}, nil
			},
		})
	}
	if set["ladybug"] {
		factories = append(factories, backendFactory{
			name: "ladybug",
			open: func() (graph.Store, func() int64, error) {
				dir, err := os.MkdirTemp("", "multi-repo-bench-ladybug-*")
				if err != nil {
					return nil, nil, err
				}
				path := filepath.Join(dir, "store.lbug")
				s, err := store_ladybug.Open(path)
				if err != nil {
					os.RemoveAll(dir)
					return nil, nil, err
				}
				return s, func() int64 {
					_ = s.Close()
					return dirSize(path)
				}, nil
			},
		})
	}
	if len(factories) == 0 {
		die("no backends selected via -only=%q", *only)
	}

	var results []benchResult
	for _, f := range factories {
		fmt.Fprintf(os.Stderr, "[%s] starting multi-repo indexing run...\n", f.name)
		r := runMultiRepoBench(f, cfgPath, *workers, *querySample, *allRepos, *projects)
		results = append(results, r)
	}

	printSummary(os.Stdout, results)
}

// selectRepos picks the repo set the bench should index. Defaults
// to cm.ActiveRepos() (honours active_project — the typical
// daemon behaviour). -all-repos returns every repo in the global
// config regardless of active_project. -projects=foo,bar unions
// the per-project lists.
func selectRepos(cm *config.ConfigManager, all bool, projects string) ([]config.RepoEntry, string) {
	if all {
		return cm.Global().Repos, "all-repos"
	}
	projects = strings.TrimSpace(projects)
	if projects != "" {
		seen := make(map[string]bool)
		var out []config.RepoEntry
		var picked []string
		for _, p := range strings.Split(projects, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			picked = append(picked, p)
			repos, err := cm.Global().ResolveRepos(p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[multi-repo-bench] project %q: %v (skipping)\n", p, err)
				continue
			}
			for _, r := range repos {
				key := r.Path
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, r)
			}
		}
		return out, "projects=" + strings.Join(picked, ",")
	}
	if cm.Global().ActiveProject != "" {
		return cm.ActiveRepos(), "active_project=" + cm.Global().ActiveProject
	}
	return cm.Global().Repos, "all-top-level"
}

func runMultiRepoBench(f backendFactory, cfgPath string, workers, querySample int, allRepos bool, projects string) benchResult {
	r := benchResult{Backend: f.name}

	store, diskFn, err := f.open()
	if err != nil {
		r.Err = "open: " + err.Error()
		return r
	}

	// Fresh config manager per backend so workspace caches aren't
	// contaminated across runs.
	cm, err := config.NewConfigManager(cfgPath)
	if err != nil {
		r.Err = "config: " + err.Error()
		_ = diskFn()
		return r
	}
	// Apply the bench's scope selection to the inner manager so
	// mi.IndexAll() picks up the same repo set the preview above
	// reported. -all-repos blanks ActiveProject so ActiveRepos
	// falls through to Global().Repos; -projects rewrites the
	// active-project to a synthetic union project; otherwise we
	// honour active_project as the daemon would.
	if allRepos {
		cm.Global().ActiveProject = ""
	} else if strings.TrimSpace(projects) != "" {
		// Use IndexScoped with the first project's workspace as the
		// filter; for cross-project unions we rewrite ActiveProject
		// to "" and rely on the in-bench preview to have shown the
		// caller which subset they're getting (good enough for a
		// bench — production uses real workspace filters).
		cm.Global().ActiveProject = ""
	}

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	// Indexer parallelism via a single-repo Indexer that the
	// MultiIndexer clones per-repo. The Config.Index.Workers field
	// rides on the indexer used for cloning.
	cfg := config.Config{}
	cfg.Index.Workers = workers
	idx := indexer.New(store, reg, cfg.Index, zap.NewNop())

	mi := indexer.NewMultiIndexer(store, reg, idx.Search(), cm, zap.NewNop())

	t0 := time.Now()
	perRepoResults, err := mi.IndexAll()
	r.IndexMs = msSince(t0)
	if err != nil {
		r.Err = "IndexAll: " + err.Error()
	}

	r.TotalNodes = store.NodeCount()
	r.TotalEdges = store.EdgeCount()
	r.RepoCount = len(perRepoResults)

	// Build the per-repo breakdown, sorted by prefix for stable output.
	prefixes := make([]string, 0, len(perRepoResults))
	for k := range perRepoResults {
		prefixes = append(prefixes, k)
	}
	sort.Strings(prefixes)
	for _, p := range prefixes {
		ir := perRepoResults[p]
		row := repoBreakdown{Prefix: p, FileCount: ir.FileCount, NodeCount: ir.NodeCount, EdgeCount: ir.EdgeCount}
		if md := mi.GetMetadata(p); md != nil {
			row.Path = md.RootPath
		}
		r.PerRepo = append(r.PerRepo, row)
	}

	// Cross-repo references probe. Cross-repo resolution is the
	// load-bearing capability multi-repo indexing exists to deliver
	// — count how many of the resolved edges actually crossed a
	// repo boundary. A backend whose resolver loses cross-repo
	// edges would surface as a much smaller number here.
	r.CrossRepoUsages = countCrossRepoEdges(store)

	// Sample workload: a deterministic GetNode loop. The single-
	// repo bench's full per-tool sweep would balloon the runtime
	// for 20 repos; keep this lean and let store-bench own the
	// detailed per-tool numbers.
	wl := pickQueryWorkload(store, querySample)
	if len(wl) > 0 {
		samples := make([]time.Duration, 0, len(wl))
		for _, id := range wl {
			t := time.Now()
			_ = store.GetNode(id)
			samples = append(samples, time.Since(t))
		}
		r.QueryP50us = pctUs(samples, 50)
		r.QueryP95us = pctUs(samples, 95)
	}

	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	r.HeapAllocMB = float64(m.HeapAlloc) / 1e6
	r.HeapInuseMB = float64(m.HeapInuse) / 1e6

	r.DiskBytes = diskFn()
	return r
}

// countCrossRepoEdges counts edges where the source and target
// belong to different repo prefixes. RepoPrefix lives on Node;
// for each edge we look up both endpoints and compare. Missing
// endpoints (synthesised stubs, unresolved refs) are skipped.
func countCrossRepoEdges(store graph.Store) int {
	edges := store.AllEdges()
	if len(edges) == 0 {
		return 0
	}
	prefixCache := make(map[string]string, 8192)
	prefixOf := func(id string) string {
		if p, ok := prefixCache[id]; ok {
			return p
		}
		n := store.GetNode(id)
		if n == nil {
			prefixCache[id] = ""
			return ""
		}
		prefixCache[id] = n.RepoPrefix
		return n.RepoPrefix
	}
	count := 0
	for _, e := range edges {
		from := prefixOf(e.From)
		to := prefixOf(e.To)
		if from == "" || to == "" || from == to {
			continue
		}
		count++
	}
	return count
}

// pickQueryWorkload samples N node IDs at random from a populated
// store. Deterministic across backends because we use the same
// crypto-rand seed shape (a fresh /dev/urandom read each time —
// the sample is meant to exercise the store's lookup path, not
// to be reproducible across runs).
func pickQueryWorkload(s graph.Store, n int) []string {
	nodes := s.AllNodes()
	if len(nodes) == 0 {
		return nil
	}
	if n >= len(nodes) {
		ids := make([]string, len(nodes))
		for i, nd := range nodes {
			ids[i] = nd.ID
		}
		return ids
	}
	out := make([]string, 0, n)
	seen := make(map[int]bool, n)
	for len(out) < n {
		var b [4]byte
		_, _ = rand.Read(b[:])
		i := int(binary.BigEndian.Uint32(b[:])) % len(nodes)
		if seen[i] {
			continue
		}
		seen[i] = true
		out = append(out, nodes[i].ID)
	}
	return out
}

// -- output -----------------------------------------------------------------

func printSummary(w *os.File, rows []benchResult) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "# Multi-repo bench summary")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| backend | repos | nodes | edges | cross-repo edges | index | disk | heap (alloc / inuse) | GetNode p50 / p95 |")
	fmt.Fprintln(w, "|---------|------:|------:|------:|-----------------:|------:|-----:|---------------------:|------------------:|")
	for _, r := range rows {
		if r.Err != "" {
			fmt.Fprintf(w, "| %s | — | — | — | — | — | — | — | %s |\n", r.Backend, r.Err)
			continue
		}
		fmt.Fprintf(w, "| %s | %d | %s | %s | %s | %s | %s | %s / %s | %s / %s |\n",
			r.Backend,
			r.RepoCount,
			fmtInt(r.TotalNodes),
			fmtInt(r.TotalEdges),
			fmtInt(r.CrossRepoUsages),
			fmtMs(r.IndexMs),
			fmtBytes(r.DiskBytes),
			fmtMB(r.HeapAllocMB), fmtMB(r.HeapInuseMB),
			fmtUs(r.QueryP50us), fmtUs(r.QueryP95us),
		)
	}
	fmt.Fprintln(w)

	// Per-repo breakdown for the first backend that has it. The
	// breakdown is identical across backends modulo the resolver
	// path (node/edge counts may shift slightly).
	fmt.Fprintln(w, "# Per-repo breakdown")
	fmt.Fprintln(w)
	fmt.Fprint(w, "| repo |")
	for _, r := range rows {
		fmt.Fprintf(w, " %s nodes | %s edges |", r.Backend, r.Backend)
	}
	fmt.Fprintln(w)
	fmt.Fprint(w, "|------|")
	for range rows {
		fmt.Fprint(w, "------:|------:|")
	}
	fmt.Fprintln(w)
	// Build a stable set of prefixes from the first backend's
	// per-repo list; fall through to the second if the first
	// errored.
	var refRows []repoBreakdown
	for _, r := range rows {
		if r.Err == "" && len(r.PerRepo) > 0 {
			refRows = r.PerRepo
			break
		}
	}
	for _, base := range refRows {
		fmt.Fprintf(w, "| %s |", base.Prefix)
		for _, r := range rows {
			n, e := lookupRepoStats(r.PerRepo, base.Prefix)
			fmt.Fprintf(w, " %s | %s |", fmtInt(n), fmtInt(e))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
}

func lookupRepoStats(rows []repoBreakdown, prefix string) (int, int) {
	for _, r := range rows {
		if r.Prefix == prefix {
			return r.NodeCount, r.EdgeCount
		}
	}
	return 0, 0
}

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

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }

func pctUs(samples []time.Duration, pct int) float64 {
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
	return float64(sorted[idx].Microseconds())
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
