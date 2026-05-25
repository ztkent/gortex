// Command edge-diff indexes the same repo twice (memory + sqlite) and
// prints the symmetric difference of the edge sets, classified by
// (Kind, FromKind, ToKind). Helps localise the source of any remaining
// edge-count gap after a backend or pipeline fix.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type edgeKey struct {
	From, To string
	Kind     graph.EdgeKind
	FilePath string
	Line     int
}

func main() {
	root := flag.String("root", "", "repo root (required)")
	workers := flag.Int("workers", runtime.NumCPU(), "indexer parallelism")
	sampleLimit := flag.Int("samples", 30, "max sample edges to print per side")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "usage: edge-diff -root <path>")
		os.Exit(1)
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		panic(err)
	}

	memNodes, memEdges := indexAndCollect(abs, *workers, "memory", func() graph.Store {
		return graph.New()
	})
	dskNodes, dskEdges := indexAndCollect(abs, *workers, "sqlite", func() graph.Store {
		dir, err := os.MkdirTemp("", "edge-diff-sqlite-*")
		if err != nil {
			panic(err)
		}
		s, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
		if err != nil {
			panic(err)
		}
		return s
	})

	memSet := edgeKeyMap(memEdges)
	dskSet := edgeKeyMap(dskEdges)

	fmt.Printf("memory: %d nodes / %d edges (unique keys %d)\n", len(memNodes), len(memEdges), len(memSet))
	fmt.Printf("sqlite: %d nodes / %d edges (unique keys %d)\n", len(dskNodes), len(dskEdges), len(dskSet))

	onlyMem := keysOnlyIn(memSet, dskSet)
	onlyDsk := keysOnlyIn(dskSet, memSet)
	fmt.Printf("only in memory: %d unique edges\n", len(onlyMem))
	fmt.Printf("only in sqlite: %d unique edges\n", len(onlyDsk))

	if dups := len(memEdges) - len(memSet); dups > 0 {
		fmt.Printf("\nmemory: %d duplicate edge slots (raw count - unique-key count)\n", dups)
	}
	if dups := len(dskEdges) - len(dskSet); dups > 0 {
		fmt.Printf("sqlite: %d duplicate edge slots (raw count - unique-key count)\n", dups)
	}

	if len(onlyMem) > 0 {
		fmt.Println("\n=== edges only in memory ===")
		describeEdges(memSet, onlyMem, memNodes, *sampleLimit)
	}
	if len(onlyDsk) > 0 {
		fmt.Println("\n=== edges only in sqlite ===")
		describeEdges(dskSet, onlyDsk, dskNodes, *sampleLimit)
	}
}

func indexAndCollect(absRoot string, workers int, label string, factory func() graph.Store) ([]*graph.Node, []*graph.Edge) {
	fmt.Fprintf(os.Stderr, "indexing through %s...\n", label)
	store := factory()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Config{}
	cfg.Index.Workers = workers
	idx := indexer.New(store, reg, cfg.Index, zap.NewNop())
	if _, err := idx.IndexCtx(context.Background(), absRoot); err != nil {
		panic(err)
	}
	return store.AllNodes(), store.AllEdges()
}

func edgeKeyMap(edges []*graph.Edge) map[edgeKey]*graph.Edge {
	out := make(map[edgeKey]*graph.Edge, len(edges))
	for _, e := range edges {
		out[edgeKey{e.From, e.To, e.Kind, e.FilePath, e.Line}] = e
	}
	return out
}

func keysOnlyIn(a, b map[edgeKey]*graph.Edge) []edgeKey {
	out := []edgeKey{}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

func describeEdges(idx map[edgeKey]*graph.Edge, keys []edgeKey, nodes []*graph.Node, sampleLimit int) {
	nodeIdx := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		nodeIdx[n.ID] = n
	}
	type cat struct {
		kind, fromKind, toKind string
		fromExternal           bool
		toExternal             bool
	}
	hist := map[cat]int{}
	for _, k := range keys {
		c := cat{kind: string(k.Kind)}
		if n, ok := nodeIdx[k.From]; ok {
			c.fromKind = string(n.Kind)
		} else {
			c.fromKind = "<missing>"
			c.fromExternal = true
		}
		if n, ok := nodeIdx[k.To]; ok {
			c.toKind = string(n.Kind)
		} else {
			c.toKind = "<missing>"
			c.toExternal = true
		}
		hist[c]++
	}
	type row struct {
		c cat
		n int
	}
	rows := make([]row, 0, len(hist))
	for c, n := range hist {
		rows = append(rows, row{c, n})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].n > rows[j].n })
	fmt.Println("histogram (Kind / FromKind / ToKind -> count):")
	for _, r := range rows {
		fmt.Printf("  kind=%-22s from=%-12s to=%-12s -> %d\n", r.c.kind, r.c.fromKind, r.c.toKind, r.n)
	}
	fmt.Printf("\nsamples (up to %d):\n", sampleLimit)
	for i, k := range keys {
		if i >= sampleLimit {
			break
		}
		e := idx[k]
		fmt.Printf("  from=%q to=%q kind=%s file=%q line=%d origin=%q tier=%q\n",
			k.From, k.To, k.Kind, k.FilePath, k.Line, e.Origin, e.Tier)
	}
}
