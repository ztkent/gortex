package sql

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// RebuildStats summarises a RebuildTablesFromStringRegistry pass. All
// counts are net (created — already-present). EmittersLinked counts the
// unique (caller, table) pairs that produced an EdgeQueries edge.
type RebuildStats struct {
	StringsVisited int `json:"strings_visited"`
	TablesCreated  int `json:"tables_created"`
	ColumnsCreated int `json:"columns_created"`
	QueryEdges     int `json:"query_edges_created"`
	ReadColEdges   int `json:"reads_col_edges_created"`
	WriteColEdges  int `json:"writes_col_edges_created"`
	EmittersLinked int `json:"emitters_linked"`
	Skipped        int `json:"skipped"`
}

// RebuildTablesFromStringRegistry is the short-circuit: rederive
// the KindTable / KindColumn / EdgeQueries / EdgeReadsCol /
// EdgeWritesCol layer from the KindString context="sql" registry
// already present in g, without re-parsing any source file. Idempotent
// — nodes and edges are deduped via graph.AddNode / AddEdge semantics,
// so running it twice produces the same graph.
//
// For each KindString context="sql" node:
//
//   - Re-extract tables and columns from node.Name (the verbatim
//     query) using the canonical ExtractTables / ExtractColumns
//     parsers — the same shape the source-time extractor uses.
//   - For every emitter (EdgeEmits.From → KindString), wire
//     EdgeQueries to each derived KindTable and EdgeReadsCol /
//     EdgeWritesCol to each derived KindColumn.
//
// Dialect is taken from Meta["dialect"] when present (set by the Go
// SQL extractor) and falls back to "generic". Origin on rebuilt edges
// is text_matched (same tier the source-time extractor uses for
// regex-derived SQL).
//
// Returns counts for telemetry; rebuilt edges idempotently replace
// any existing edges with the same edgeKey, so a second call after
// the first reports tablesCreated=0, emittersLinked=0.
func RebuildTablesFromStringRegistry(g graph.Store) RebuildStats {
	if g == nil {
		return RebuildStats{}
	}
	var stats RebuildStats
	// Snapshot the node set to avoid mutation-during-iteration; we
	// append KindTable / KindColumn nodes below.
	nodes := g.AllNodes()
	// Track per-call counts of new node/edge insertions; we infer
	// "created" by checking presence before AddNode / AddEdge.
	preExistingTables := make(map[string]struct{})
	preExistingCols := make(map[string]struct{})
	for _, n := range nodes {
		if n == nil {
			continue
		}
		switch n.Kind {
		case graph.KindTable:
			preExistingTables[n.ID] = struct{}{}
		case graph.KindColumn:
			preExistingCols[n.ID] = struct{}{}
		}
	}
	// EdgeQueries / EdgeReadsCol / EdgeWritesCol dedup tracker.
	// Seeded from the live graph so a re-run on an already-rebuilt
	// graph reports zero new edges (graph.AddEdge is idempotent by
	// edgeKey, but the stats counters live in this function so they
	// need to know what was already there).
	type edgeKey struct {
		from, to string
		kind     graph.EdgeKind
	}
	seenEdges := make(map[edgeKey]struct{})
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindTable && n.Kind != graph.KindColumn {
			continue
		}
		for _, e := range g.GetInEdges(n.ID) {
			if e == nil {
				continue
			}
			switch e.Kind {
			case graph.EdgeQueries, graph.EdgeReadsCol, graph.EdgeWritesCol:
				seenEdges[edgeKey{from: e.From, to: e.To, kind: e.Kind}] = struct{}{}
			}
		}
	}
	for _, n := range nodes {
		if n == nil || n.Kind != graph.KindString {
			continue
		}
		if ctx, _ := n.Meta["context"].(string); ctx != "sql" {
			continue
		}
		query := n.Name
		if query == "" {
			if v, ok := n.Meta["value"].(string); ok {
				query = v
			}
		}
		if query == "" {
			stats.Skipped++
			continue
		}
		stats.StringsVisited++
		dialect, _ := n.Meta["dialect"].(string)
		if dialect == "" {
			dialect = "generic"
		}
		tables := ExtractTables(query)
		columns := ExtractColumns(query)
		if len(tables) == 0 {
			stats.Skipped++
			continue
		}
		// Collect emitters from EdgeEmits to this KindString.
		emitters := make([]string, 0, 4)
		seenEmitter := make(map[string]struct{}, 4)
		for _, e := range g.GetInEdges(n.ID) {
			if e == nil || e.Kind != graph.EdgeEmits {
				continue
			}
			if _, dup := seenEmitter[e.From]; dup {
				continue
			}
			seenEmitter[e.From] = struct{}{}
			emitters = append(emitters, e.From)
		}
		// Stable ordering — emitter snapshots are otherwise shard-
		// order-dependent, which makes test assertions racy.
		sort.Strings(emitters)
		// Ensure KindTable nodes.
		tableNodes := make([]struct {
			id    string
			ref   TableRef
			added bool
		}, 0, len(tables))
		for _, ref := range tables {
			id := TableNodeID(dialect, ref.Schema, ref.Table)
			if _, exists := preExistingTables[id]; !exists {
				meta := map[string]any{
					"table":   ref.Table,
					"dialect": dialect,
				}
				if ref.Schema != "" {
					meta["schema"] = ref.Schema
				}
				g.AddNode(&graph.Node{
					ID:       id,
					Kind:     graph.KindTable,
					Name:     ref.Table,
					FilePath: n.FilePath,
					Language: "sql",
					Meta:     meta,
				})
				preExistingTables[id] = struct{}{}
				stats.TablesCreated++
				tableNodes = append(tableNodes, struct {
					id    string
					ref   TableRef
					added bool
				}{id, ref, true})
			} else {
				tableNodes = append(tableNodes, struct {
					id    string
					ref   TableRef
					added bool
				}{id, ref, false})
			}
		}
		// Ensure KindColumn nodes.
		colNodes := make([]struct {
			id  string
			ref ColumnRef
		}, 0, len(columns))
		for _, col := range columns {
			id := ColumnNodeID(dialect, col.Schema, col.Table, col.Column)
			if _, exists := preExistingCols[id]; !exists {
				meta := map[string]any{
					"table":   col.Table,
					"column":  col.Column,
					"dialect": dialect,
				}
				if col.Schema != "" {
					meta["schema"] = col.Schema
				}
				g.AddNode(&graph.Node{
					ID:       id,
					Kind:     graph.KindColumn,
					Name:     col.Column,
					FilePath: n.FilePath,
					Language: "sql",
					Meta:     meta,
				})
				preExistingCols[id] = struct{}{}
				stats.ColumnsCreated++
			}
			colNodes = append(colNodes, struct {
				id  string
				ref ColumnRef
			}{id, col})
		}
		// Wire emitters → tables / columns.
		for _, emitter := range emitters {
			for _, t := range tableNodes {
				k := edgeKey{from: emitter, to: t.id, kind: graph.EdgeQueries}
				if _, dup := seenEdges[k]; dup {
					continue
				}
				seenEdges[k] = struct{}{}
				g.AddEdge(&graph.Edge{
					From:     emitter,
					To:       t.id,
					Kind:     graph.EdgeQueries,
					FilePath: n.FilePath,
					Origin:   graph.OriginTextMatched,
					Meta: map[string]any{
						"op":     t.ref.Op,
						"source": "string_registry",
					},
				})
				stats.QueryEdges++
				stats.EmittersLinked++
			}
			for _, c := range colNodes {
				kind := graph.EdgeReadsCol
				if c.ref.Op == "write" {
					kind = graph.EdgeWritesCol
				}
				k := edgeKey{from: emitter, to: c.id, kind: kind}
				if _, dup := seenEdges[k]; dup {
					continue
				}
				seenEdges[k] = struct{}{}
				g.AddEdge(&graph.Edge{
					From:     emitter,
					To:       c.id,
					Kind:     kind,
					FilePath: n.FilePath,
					Origin:   graph.OriginTextMatched,
					Meta: map[string]any{
						"source": "string_registry",
					},
				})
				if kind == graph.EdgeReadsCol {
					stats.ReadColEdges++
				} else {
					stats.WriteColEdges++
				}
			}
		}
	}
	return stats
}
