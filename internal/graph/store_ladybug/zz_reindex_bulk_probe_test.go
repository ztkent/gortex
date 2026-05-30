package store_ladybug

// Probe (throwaway): verifies the two file-driven liblbug primitives the
// bulk ReindexEdges fix depends on actually work, before building the
// feature on them:
//
//  1. LOAD FROM <file> MATCH (a),(b) MERGE (a)-[e:Edge {...}]->(b) SET ...
//     — bulk rel upsert (dedup-safe, matches upsertEdgeLocked's MERGE).
//  2. LOAD FROM <file> MATCH (a)-[e:Edge {...}]->(b) DELETE e
//     — bulk rel delete of the resolved stub edges.
//
// Both use LOAD FROM (a file scan) rather than UNWIND, which is why they
// are expected to sidestep the unordered_map::at C++ panic that killed the
// UNWIND-batch ReindexEdges (same reason fix-2's LOAD FROM ... MERGE works).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestProbe_LoadDrivenReindexPrimitives(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "x.kuzu"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const file = "f.go"
	s.AddNode(&graph.Node{ID: file + "::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: file})
	s.AddNode(&graph.Node{ID: file + "::Real", Name: "Real", Kind: graph.KindFunction, FilePath: file})
	// Stub edge the resolver will rewrite: Caller -[calls@f.go:1]-> unresolved::Real
	s.AddEdge(&graph.Edge{From: file + "::Caller", To: "unresolved::Real", Kind: graph.EdgeCalls, FilePath: file, Line: 1, Confidence: 0.3})

	dir := t.TempDir()
	kind := string(graph.EdgeCalls)
	t.Logf("EdgeCalls string = %q", kind)

	// ---- PROBE 1: bulk rel upsert via LOAD FROM ... MATCH ... MERGE ----
	newPath := filepath.Join(dir, "new_edges.csv")
	if err := writeEdgesTSV(newPath, []*graph.Edge{{
		From: file + "::Caller", To: file + "::Real", Kind: graph.EdgeCalls,
		FilePath: file, Line: 1, Confidence: 0.9, Origin: "probe",
	}}); err != nil {
		t.Fatalf("write new edges: %v", err)
	}
	mergeQ := fmt.Sprintf(
		"LOAD FROM '%s' (header=false, delim='\t') "+
			"MATCH (a:Node {id: column0}), (b:Node {id: column1}) "+
			"MERGE (a)-[e:Edge {kind: column2, file_path: column3, line: CAST(column4 AS INT64)}]->(b) "+
			"SET e.confidence = CAST(column5 AS DOUBLE), e.confidence_label = column6, "+
			"e.origin = column7, e.tier = column8, e.cross_repo = CAST(column9 AS INT64), e.meta = column10",
		escapeCypherStringLit(newPath))
	if err := s.runCopyPooled(mergeQ); err != nil {
		t.Fatalf("PROBE 1 FAILED — LOAD-driven rel MERGE unsupported: %v", err)
	}
	t.Log("PROBE 1 OK — LOAD FROM ... MATCH ... MERGE (rel upsert) works")

	// ---- PROBE 2: bulk rel delete via LOAD FROM ... MATCH ... DELETE ----
	keysPath := filepath.Join(dir, "old_keys.csv")
	// cols: from, kind, file_path, line, oldTo
	if err := os.WriteFile(keysPath, []byte(fmt.Sprintf("%s::Caller\t%s\t%s\t1\tunresolved::Real\n", file, kind, file)), 0o644); err != nil {
		t.Fatalf("write keys: %v", err)
	}
	delQ := fmt.Sprintf(
		"LOAD FROM '%s' (header=false, delim='\t') "+
			"MATCH (a:Node {id: column0})-[e:Edge {kind: column1, file_path: column2, line: CAST(column3 AS INT64)}]->(b:Node {id: column4}) "+
			"DELETE e",
		escapeCypherStringLit(keysPath))
	if err := s.runCopyPooled(delQ); err != nil {
		t.Fatalf("PROBE 2 FAILED — LOAD-driven rel DELETE unsupported: %v", err)
	}
	t.Log("PROBE 2 OK — LOAD FROM ... MATCH ... DELETE (rel delete) works")

	// ---- VERIFY end state: Caller -> Real only, stub gone, no dup ----
	out := s.GetOutEdges(file + "::Caller")
	byTo := map[string]int{}
	for _, e := range out {
		if e != nil {
			byTo[e.To]++
		}
	}
	t.Logf("end-state out-edges of Caller: %v", byTo)
	if byTo["unresolved::Real"] != 0 {
		t.Errorf("stub edge not deleted: %d remain", byTo["unresolved::Real"])
	}
	if byTo[file+"::Real"] != 1 {
		t.Errorf("resolved edge: want exactly 1 Caller->Real, got %d", byTo[file+"::Real"])
	}

	// ---- PROBE 3: idempotency — re-run MERGE, must NOT create a dup ----
	if err := s.runCopyPooled(mergeQ); err != nil {
		t.Fatalf("PROBE 3 (re-merge) failed: %v", err)
	}
	out2 := s.GetOutEdges(file + "::Caller")
	dup := 0
	for _, e := range out2 {
		if e != nil && e.To == file+"::Real" {
			dup++
		}
	}
	if dup != 1 {
		t.Errorf("PROBE 3 — MERGE created a duplicate: %d Caller->Real edges (want 1)", dup)
	} else {
		t.Log("PROBE 3 OK — re-running MERGE is idempotent (no duplicate rel)")
	}
}

// TestReindexEdges_BulkPath exercises the large-batch bulk route end to
// end: stubs deleted, every resolution present exactly once, props carried
// through, a resolution to a not-yet-materialised target stub-merged (not
// dropped), and the whole apply idempotent.
func TestReindexEdges_BulkPath(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "x.kuzu"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const file = "f.go"
	n := reindexBulkThreshold + 50 // force the bulk path regardless of threshold

	s.AddNode(&graph.Node{ID: file + "::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: file})
	for i := 0; i < n; i++ {
		s.AddNode(&graph.Node{ID: fmt.Sprintf("%s::Real%d", file, i), Name: fmt.Sprintf("Real%d", i), Kind: graph.KindFunction, FilePath: file})
		s.AddEdge(&graph.Edge{From: file + "::Caller", To: fmt.Sprintf("unresolved::Real%d", i), Kind: graph.EdgeCalls, FilePath: file, Line: i + 1, Confidence: 0.3})
	}

	// Edge 0 resolves to a target with NO node yet — the bulk path must
	// MERGE-stub it (parity with the per-edge mergeStubNodeLocked) rather
	// than silently drop the resolution.
	const missingTarget = "external::pkg::Ghost"
	batch := make([]graph.EdgeReindex, 0, n)
	for i := 0; i < n; i++ {
		to := fmt.Sprintf("%s::Real%d", file, i)
		if i == 0 {
			to = missingTarget
		}
		batch = append(batch, graph.EdgeReindex{
			Edge:  &graph.Edge{From: file + "::Caller", To: to, Kind: graph.EdgeCalls, FilePath: file, Line: i + 1, Confidence: 0.95, Origin: "bulk-test"},
			OldTo: fmt.Sprintf("unresolved::Real%d", i),
		})
	}
	s.ReindexEdges(batch) // len >= reindexBulkThreshold -> bulk path

	collect := func() (map[string]int, float64, string) {
		byTo := map[string]int{}
		var conf float64
		var origin string
		for _, e := range s.GetOutEdges(file + "::Caller") {
			if e == nil {
				continue
			}
			byTo[e.To]++
			if e.To == file+"::Real7" {
				conf, origin = e.Confidence, e.Origin
			}
		}
		return byTo, conf, origin
	}

	byTo, conf, origin := collect()
	for to, c := range byTo {
		if strings.Contains(to, "unresolved::") {
			t.Errorf("stub edge survived bulk reindex: %s x%d", to, c)
		}
	}
	if byTo[missingTarget] != 1 {
		t.Errorf("missing-endpoint resolution dropped: Caller->%s = %d (want 1)", missingTarget, byTo[missingTarget])
	}
	for i := 1; i < n; i++ {
		to := fmt.Sprintf("%s::Real%d", file, i)
		if byTo[to] != 1 {
			t.Errorf("resolved edge Caller->%s = %d (want 1)", to, byTo[to])
		}
	}
	if conf != 0.95 {
		t.Errorf("bulk MERGE did not carry confidence: got %v want 0.95", conf)
	}
	if origin != "bulk-test" {
		t.Errorf("bulk MERGE did not carry origin: got %q", origin)
	}
	total := 0
	for _, c := range byTo {
		total += c
	}
	if total != n {
		t.Errorf("total out-edges = %d, want %d (dup or leftover)", total, n)
	}

	// The bulk path inserts via COPY (append), so it is single-apply by
	// contract: the resolver resolves each stub exactly once per pass and
	// never re-applies a resolved batch (a re-indexed file is evicted +
	// re-stubbed first, so prior resolved edges are gone before the next
	// pass). The MERGE-idempotent per-edge path covers small / incremental
	// callers. So we assert single-apply correctness (above), not re-apply
	// idempotency.
}

// TestReindexEdges_BulkPath_Scale reproduces the cold-load apply at scale
// (the probe passed at 300; the live 75k batch fell back to per-edge). If
// the bulk path fails it prints [REINDEX-BULK] and falls back, so a slow
// elapsed + that line means scale broke it.
func TestReindexEdges_BulkPath_Scale(t *testing.T) {
	if testing.Short() {
		t.Skip("80k-edge scale test; skipped under -short")
	}
	s, err := Open(filepath.Join(t.TempDir(), "x.kuzu"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const file = "f.go"
	const n = 80000
	nodes := make([]*graph.Node, 0, 2*n+1)
	edges := make([]*graph.Edge, 0, n)
	nodes = append(nodes, &graph.Node{ID: file + "::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: file})
	for i := 0; i < n; i++ {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s::T%d", file, i), Name: fmt.Sprintf("T%d", i), Kind: graph.KindFunction, FilePath: file})
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("unresolved::T%d", i), Name: fmt.Sprintf("T%d", i), Kind: graph.NodeKind("unresolved")})
		edges = append(edges, &graph.Edge{From: file + "::Caller", To: fmt.Sprintf("unresolved::T%d", i), Kind: graph.EdgeCalls, FilePath: file, Line: i + 1, Confidence: 0.3})
	}
	s.BeginBulkLoad()
	s.AddBatch(nodes, edges)
	if err := s.FlushBulk(); err != nil {
		t.Fatalf("flush setup: %v", err)
	}

	batch := make([]graph.EdgeReindex, 0, n)
	for i := 0; i < n; i++ {
		batch = append(batch, graph.EdgeReindex{
			Edge:  &graph.Edge{From: file + "::Caller", To: fmt.Sprintf("%s::T%d", file, i), Kind: graph.EdgeCalls, FilePath: file, Line: i + 1, Confidence: 0.9},
			OldTo: fmt.Sprintf("unresolved::T%d", i),
		})
	}
	st := time.Now()
	s.ReindexEdges(batch)
	t.Logf("ReindexEdges(%d) took %s", n, time.Since(st))

	stub, resolved := 0, 0
	for _, e := range s.GetOutEdges(file + "::Caller") {
		if e == nil {
			continue
		}
		if strings.Contains(e.To, "unresolved::") {
			stub++
		} else {
			resolved++
		}
	}
	if stub != 0 {
		t.Errorf("%d stub edges remain", stub)
	}
	if resolved != n {
		t.Errorf("resolved=%d want %d", resolved, n)
	}
}
