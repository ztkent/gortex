//go:build ladybug

// Command unresolved-audit indexes a repo and classifies every
// `unresolved::*` edge target by ID shape and edge-kind signature
// (calls, references, reads, writes). For each shape it prints
// counts, fan-in, and concrete samples — including the From symbol
// when available, so we can audit specific call sites to see why the
// resolver gave up. The goal: split the unresolved population into
// (a) resolver gaps we can close, (b) genuinely ambiguous cases,
// and (c) intrinsic externals that should be promoted to first-class
// nodes rather than left as unresolved.
//
// Uses the Ladybug rel-table FK as the stress test for stub
// classification — every edge endpoint must exist as a Node row,
// so unresolved::* IDs show up as empty stub nodes whose
// composition we can audit.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_ladybug"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func main() {
	root := flag.String("root", "", "repo root (required)")
	workers := flag.Int("workers", runtime.NumCPU(), "indexer parallelism")
	samplesPerShape := flag.Int("samples", 12, "max sample call sites per shape")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "usage: unresolved-audit -root <path>")
		os.Exit(1)
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		panic(err)
	}
	dir, err := os.MkdirTemp("", "unresolved-audit-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	store, err := store_ladybug.Open(filepath.Join(dir, "store.lbug"))
	if err != nil {
		panic(err)
	}

	fmt.Fprintln(os.Stderr, "indexing through ladybug...")
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Config{}
	cfg.Index.Workers = *workers
	if _, err := indexer.New(store, reg, cfg.Index, zap.NewNop()).IndexCtx(context.Background(), abs); err != nil {
		panic(err)
	}

	nodes := store.AllNodes()
	edges := store.AllEdges()

	// Build a node-ID → kind/name map for source-side context on
	// each sampled edge.
	byID := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}

	type sample struct {
		from, to string
		kind     graph.EdgeKind
		file     string
		line     int
	}
	type shapeBucket struct {
		count    int
		fanIn    map[graph.EdgeKind]int
		samples  []sample
		toUnique map[string]struct{}
	}
	shapes := map[string]*shapeBucket{}

	for _, e := range edges {
		if !strings.HasPrefix(e.To, "unresolved::") {
			continue
		}
		shape := classifyUnresolvedShape(e.To)
		b, ok := shapes[shape]
		if !ok {
			b = &shapeBucket{
				fanIn:    map[graph.EdgeKind]int{},
				toUnique: map[string]struct{}{},
			}
			shapes[shape] = b
		}
		b.count++
		b.fanIn[e.Kind]++
		b.toUnique[e.To] = struct{}{}
		if len(b.samples) < *samplesPerShape {
			b.samples = append(b.samples, sample{e.From, e.To, e.Kind, e.FilePath, e.Line})
		}
	}

	type row struct {
		shape string
		b     *shapeBucket
	}
	rows := make([]row, 0, len(shapes))
	for s, b := range shapes {
		rows = append(rows, row{s, b})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].b.count > rows[j].b.count })

	totalEdges, totalShapes, totalIDs := 0, 0, 0
	for _, r := range rows {
		totalEdges += r.b.count
		totalShapes++
		totalIDs += len(r.b.toUnique)
	}
	fmt.Printf("unresolved:: edges: %d across %d unique IDs / %d shape buckets\n\n",
		totalEdges, totalIDs, totalShapes)

	// Per-ID fan-in across the WHOLE edge set so the per-shape "top
	// 20 unresolved IDs" view has accurate counts (the sample list
	// only sees the first sample-limit edges).
	perID := map[string]int{}
	for _, e := range edges {
		if strings.HasPrefix(e.To, "unresolved::") {
			perID[e.To]++
		}
	}

	for _, r := range rows {
		fmt.Printf("### shape: %-34s edges: %d  unique IDs: %d\n",
			r.shape, r.b.count, len(r.b.toUnique))
		fmt.Printf("    fan-in by kind: %s\n", fmtFanIn(r.b.fanIn))

		// Top-N most-referenced unresolved IDs in this shape.
		idsInShape := make([]string, 0, len(r.b.toUnique))
		for id := range r.b.toUnique {
			idsInShape = append(idsInShape, id)
		}
		sort.Slice(idsInShape, func(i, j int) bool { return perID[idsInShape[i]] > perID[idsInShape[j]] })
		const topN = 20
		if len(idsInShape) > topN {
			idsInShape = idsInShape[:topN]
		}
		fmt.Printf("    top %d most-referenced IDs:\n", len(idsInShape))
		for _, id := range idsInShape {
			fmt.Printf("      %-50s -> %d edges\n", truncate(id, 50), perID[id])
		}

		fmt.Printf("    sample call sites (up to %d):\n", *samplesPerShape)
		for _, s := range r.b.samples {
			fromCtx := "<no-from-node>"
			if n := byID[s.from]; n != nil {
				fromCtx = fmt.Sprintf("%s:%s", n.Kind, n.Name)
			}
			fmt.Printf("      [%s] %s -> %q  %s:%d  (from %s)\n",
				s.kind, truncate(s.from, 60), s.to, filepath.Base(s.file), s.line, fromCtx)
		}
		fmt.Println()
	}
}

// classifyUnresolvedShape buckets an `unresolved::*` ID by structural
// shape so we can see whether the resolver's failures cluster on a
// fixable pattern (e.g. `bare-name` could be intra-function locals
// the resolver isn't checking) vs an intrinsically ambiguous one
// (e.g. `*.MethodName` requires receiver-type info we may not have).
func classifyUnresolvedShape(id string) string {
	body := strings.TrimPrefix(id, "unresolved::")
	switch {
	case strings.HasPrefix(body, "*.") && strings.Contains(body, "."):
		// `*.Method` — method on unknown receiver type.
		return "*.method-unknown-receiver"
	case strings.HasPrefix(body, "pyrel::"):
		return "pyrel-relative-import"
	case strings.Contains(body, "."):
		// `pkg.Name` — qualified reference where pkg didn't resolve.
		return "qualified.name"
	case strings.Contains(body, "::"):
		return "synthetic::other"
	default:
		// Bare identifier — usually a local, package-level name, or
		// builtin. With KindLocal nodes now in the graph, the
		// resolver should be able to bind same-function references.
		return "bare-name"
	}
}

func fmtFanIn(m map[graph.EdgeKind]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[graph.EdgeKind(k)]))
	}
	return strings.Join(parts, " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
