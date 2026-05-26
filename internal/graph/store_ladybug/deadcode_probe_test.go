//go:build ladybug

package store_ladybug

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestDeadCode_Probe probes the Cypher shapes that could implement the
// server-side dead-code candidate filter:
//
//   - "WHERE NOT EXISTS { MATCH ... }" — subquery existence check; the
//     spec-defined way to ask "no incoming edge of allowed kind".
//   - Per-node-kind UNWIND with the allowlist baked in as a Cypher list
//     literal (one query per kind).
//   - LEFT JOIN trick (OPTIONAL MATCH … WHERE other IS NULL) — the
//     classic anti-join pattern.
//
// The probe logs which shape Ladybug accepts and the row counts so the
// implementation can pick the one that compiles AND has reasonable
// runtime characteristics.
func TestDeadCode_Probe(t *testing.T) {
	dir, err := os.MkdirTemp("", "lbug-deadcode-probe-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	s, err := Open(filepath.Join(dir, "store.lbug"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Seed a small graph with:
	//   - Function "Alive" called by another function.
	//   - Function "Dead" never called.
	//   - Function "WrongKindOnly" referenced but only by reads (wrong
	//     allowlist for functions — should still appear dead).
	//   - Method "AliveMethod" called.
	//   - Method "DeadMethod" never touched.
	//   - Type "AliveType" referenced.
	//   - Type "DeadType" with no incoming edges.
	nodes := []*graph.Node{
		{ID: "Alive", Kind: graph.KindFunction, Name: "Alive", FilePath: "a.go"},
		{ID: "Dead", Kind: graph.KindFunction, Name: "Dead", FilePath: "a.go"},
		{ID: "WrongKindOnly", Kind: graph.KindFunction, Name: "WrongKindOnly", FilePath: "a.go"},
		{ID: "Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a.go"},
		{ID: "AliveMethod", Kind: graph.KindMethod, Name: "AliveMethod", FilePath: "a.go"},
		{ID: "DeadMethod", Kind: graph.KindMethod, Name: "DeadMethod", FilePath: "a.go"},
		{ID: "AliveType", Kind: graph.KindType, Name: "AliveType", FilePath: "a.go"},
		{ID: "DeadType", Kind: graph.KindType, Name: "DeadType", FilePath: "a.go"},
	}
	for _, n := range nodes {
		s.AddNode(n)
	}
	for _, e := range []*graph.Edge{
		{From: "Caller", To: "Alive", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1},
		{From: "Caller", To: "WrongKindOnly", Kind: graph.EdgeReads, FilePath: "a.go", Line: 2},
		{From: "Caller", To: "AliveMethod", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3},
		{From: "Caller", To: "AliveType", Kind: graph.EdgeReferences, FilePath: "a.go", Line: 4},
	} {
		s.AddEdge(e)
	}

	probes := []struct {
		name string
		q    string
		args map[string]any
	}{
		{
			// Shape A: per-kind WHERE NOT EXISTS subquery (Cypher spec
			// shape). One query per node kind; the allowlist is a list
			// literal in $allowed.
			name: "shape_A_not_exists_subquery",
			q: `
MATCH (n:Node {kind: $kind})
WHERE NOT EXISTS {
    MATCH (src:Node)-[e:Edge]->(n)
    WHERE e.kind IN $allowed
}
RETURN n.id`,
			args: map[string]any{
				"kind":    string(graph.KindFunction),
				"allowed": []any{string(graph.EdgeCalls), string(graph.EdgeReferences)},
			},
		},
		{
			// Shape B: LEFT-JOIN-style OPTIONAL MATCH + IS NULL anti-join.
			name: "shape_B_optional_match_isnull",
			q: `
MATCH (n:Node {kind: $kind})
OPTIONAL MATCH (src:Node)-[e:Edge]->(n) WHERE e.kind IN $allowed
WITH n, count(e) AS inc
WHERE inc = 0
RETURN n.id`,
			args: map[string]any{
				"kind":    string(graph.KindFunction),
				"allowed": []any{string(graph.EdgeCalls), string(graph.EdgeReferences)},
			},
		},
		{
			// Shape C: COUNT subquery (Cypher 9+ COUNT subquery form).
			name: "shape_C_count_subquery",
			q: `
MATCH (n:Node {kind: $kind})
WHERE COUNT { MATCH (src:Node)-[e:Edge]->(n) WHERE e.kind IN $allowed } = 0
RETURN n.id`,
			args: map[string]any{
				"kind":    string(graph.KindFunction),
				"allowed": []any{string(graph.EdgeCalls), string(graph.EdgeReferences)},
			},
		},
		{
			// Shape D: per-kind without explicit allowed (any incoming
			// edge counts as alive — fast path for kinds whose allowlist
			// is implicit).
			name: "shape_D_not_exists_any",
			q: `
MATCH (n:Node {kind: $kind})
WHERE NOT EXISTS { MATCH (src:Node)-[e:Edge]->(n) }
RETURN n.id`,
			args: map[string]any{"kind": string(graph.KindMethod)},
		},
		{
			// Shape E: NOT EXISTS with the WHERE inside as a property
			// match (no IN). Some Cypher dialects fail on IN inside
			// subquery WHERE — try a single-kind form as a fallback.
			name: "shape_E_not_exists_single_kind",
			q: `
MATCH (n:Node {kind: $kind})
WHERE NOT EXISTS { MATCH (src:Node)-[e:Edge {kind: $alloweKind}]->(n) }
RETURN n.id`,
			args: map[string]any{
				"kind":       string(graph.KindFunction),
				"alloweKind": string(graph.EdgeCalls),
			},
		},
	}

	for _, p := range probes {
		rows, qerr := tryQueryCypher(s, p.q, p.args)
		if qerr != nil {
			t.Logf("%s: error: %v", p.name, qerr)
			continue
		}
		t.Logf("%s → %d rows", p.name, len(rows))
		for _, r := range rows {
			t.Logf("  %v", r)
		}
	}

	// Probe interface-implements join shape used by IfaceImplementsScanner.
	t.Log("--- iface implements probes ---")
	s.AddNode(&graph.Node{
		ID: "iface1", Kind: graph.KindInterface, Name: "Foo", FilePath: "a.go",
		Meta: map[string]any{"methods": []string{"Bar"}},
	})
	s.AddNode(&graph.Node{
		ID: "type1", Kind: graph.KindType, Name: "FooImpl", FilePath: "a.go",
	})
	s.AddEdge(&graph.Edge{From: "type1", To: "iface1", Kind: graph.EdgeImplements, FilePath: "a.go", Line: 7})

	ifaceProbes := []struct {
		name string
		q    string
	}{
		{
			name: "iface_basic",
			q: `
MATCH (t:Node)-[e:Edge {kind: 'implements'}]->(iface:Node {kind: 'interface'})
WHERE iface.meta <> ''
RETURN t.id, iface.id, iface.meta`,
		},
		{
			name: "iface_strict_kind_param",
			q: `
MATCH (t:Node)-[e:Edge]->(iface:Node)
WHERE e.kind = $impl AND iface.kind = $iface AND iface.meta <> ''
RETURN t.id, iface.id, iface.meta`,
		},
	}
	for _, p := range ifaceProbes {
		args := map[string]any{
			"impl":  string(graph.EdgeImplements),
			"iface": string(graph.KindInterface),
		}
		rows, qerr := tryQueryCypher(s, p.q, args)
		if qerr != nil {
			t.Logf("%s: error: %v", p.name, qerr)
			continue
		}
		t.Logf("%s → %d rows", p.name, len(rows))
		for _, r := range rows {
			t.Logf("  %v", r)
		}
	}
}
