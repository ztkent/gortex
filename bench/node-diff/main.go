// Command node-diff indexes the same repo twice — once through the
// in-memory Store and once through a disk Store — then prints the
// symmetric difference of the two node sets so we can classify which
// nodes one path has that the other drops.
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

func main() {
	root := flag.String("root", "", "repo root (required)")
	workers := flag.Int("workers", runtime.NumCPU(), "indexer parallelism")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "usage: node-diff -root <path>")
		os.Exit(1)
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		panic(err)
	}

	memNodes := indexAndCollect(abs, *workers, "memory", func() graph.Store {
		return graph.New()
	})
	dskNodes := indexAndCollect(abs, *workers, "sqlite", func() graph.Store {
		dir, err := os.MkdirTemp("", "node-diff-sqlite-*")
		if err != nil {
			panic(err)
		}
		s, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
		if err != nil {
			panic(err)
		}
		return s
	})

	// Smoke-test: write one of the "missing" nodes directly to a
	// fresh sqlite store. If it round-trips, sqlite is innocent and
	// the loss is upstream (shadow drain, indexer pipeline ordering,
	// etc). If it doesn't, sqlite is silently dropping these nodes.
	{
		dir, _ := os.MkdirTemp("", "node-diff-smoke-*")
		s, _ := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
		probe := &graph.Node{
			ID:       "module::pypi:agents",
			Kind:     "module",
			Name:     "agents.gortex_agent",
			Language: "python",
		}
		s.AddNode(probe)
		got := s.GetNode("module::pypi:agents")
		fmt.Fprintf(os.Stderr, "smoke: direct AddNode(module::pypi:agents) -> GetNode round-trip: present=%v\n", got != nil)
		all := s.AllNodes()
		fmt.Fprintf(os.Stderr, "smoke: AllNodes() returned %d nodes after one AddNode\n", len(all))
	}

	memIDs := nodeIDSet(memNodes)
	dskIDs := nodeIDSet(dskNodes)

	onlyMem := diff(memIDs, dskIDs)
	onlyDsk := diff(dskIDs, memIDs)

	fmt.Printf("memory: %d nodes\n", len(memIDs))
	fmt.Printf("sqlite: %d nodes\n", len(dskIDs))
	fmt.Printf("only in memory: %d\n", len(onlyMem))
	fmt.Printf("only in sqlite: %d\n", len(onlyDsk))
	fmt.Println()

	if len(onlyMem) > 0 {
		fmt.Println("=== nodes only in memory ===")
		describe(memIDs, onlyMem)
	}
	if len(onlyDsk) > 0 {
		fmt.Println("=== nodes only in sqlite ===")
		describe(dskIDs, onlyDsk)
	}
}

func indexAndCollect(absRoot string, workers int, label string, factory func() graph.Store) []*graph.Node {
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
	return store.AllNodes()
}

func nodeIDSet(nodes []*graph.Node) map[string]*graph.Node {
	out := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		out[n.ID] = n
	}
	return out
}

func diff(a, b map[string]*graph.Node) []string {
	out := make([]string, 0)
	for id := range a {
		if _, ok := b[id]; !ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func describe(idx map[string]*graph.Node, ids []string) {
	type cat struct {
		kind, lang string
		empty      bool
	}
	hist := map[cat]int{}
	const sampleLimit = 30
	samples := []string{}
	for _, id := range ids {
		n := idx[id]
		c := cat{kind: string(n.Kind), lang: n.Language, empty: n.ID == "" || n.Name == ""}
		hist[c]++
		if len(samples) < sampleLimit {
			samples = append(samples, fmt.Sprintf("  id=%q kind=%q name=%q lang=%q file=%q line=%d-%d",
				n.ID, n.Kind, n.Name, n.Language, n.FilePath, n.StartLine, n.EndLine))
		}
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
	fmt.Println("histogram (kind/lang/empty -> count):")
	for _, r := range rows {
		fmt.Printf("  kind=%-20s lang=%-8s empty=%-5v -> %d\n", r.c.kind, r.c.lang, r.c.empty, r.n)
	}
	fmt.Printf("samples (up to %d):\n", sampleLimit)
	for _, s := range samples {
		fmt.Println(s)
	}
	fmt.Println()
}
