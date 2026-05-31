package store_ladybug

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestDeleteThenCreateReadsDeletedEdge isolates the exact Cypher pattern
// every backend-resolver rule shares:
//
//	MATCH (caller)-[e:Edge]->(stub)
//	...
//	MATCH (target {name: name})
//	DELETE e
//	CREATE (caller)-[newE {kind: e.kind, file_path: e.file_path, line: e.line, ...}]->(target)
//
// i.e. the CREATE reads e.kind / e.file_path / e.line off the relationship
// that was just DELETEd, across a vector of many edges in one statement.
// The hypothesis is that reading the deleted e's stored properties yields
// ANOTHER edge's kind/file_path (column-vector recycling) while caller/
// target (From/To) and possibly line survive.
func TestDeleteThenCreateReadsDeletedEdge(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "x.kuzu"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// N callers, each with a UNIQUE-name stub and a UNIQUE real target of
	// the SAME name. Distinct kinds + distinct file_paths so any cross-edge
	// bleed of kind/file_path is detectable. All same repo so a single
	// MATCH ... WHERE name-equality statement sweeps the whole vector.
	kinds := []graph.EdgeKind{
		graph.EdgeCalls, graph.EdgeReturns, graph.EdgeInstantiates,
		graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeReads,
	}
	const N = 48
	type want struct {
		from, to, file string
		kind           graph.EdgeKind
		line           int
	}
	var wants []want
	for i := 0; i < N; i++ {
		kind := kinds[i%len(kinds)]
		file := fmt.Sprintf("repo/a/caller%02d.go", i)
		caller := file + "::Caller"
		name := fmt.Sprintf("Sym%02d", i)
		tfile := fmt.Sprintf("repo/b/sym%02d.go", i)
		target := tfile + "::" + name
		stub := "unresolved::" + name
		s.AddNode(&graph.Node{ID: caller, Name: fmt.Sprintf("Caller%02d", i), Kind: graph.KindFunction, FilePath: file, RepoPrefix: "repo"})
		// Target is a plain type so the kind-gate accepts every kind.
		s.AddNode(&graph.Node{ID: target, Name: name, Kind: graph.KindType, FilePath: tfile, RepoPrefix: "repo"})
		s.AddNode(&graph.Node{ID: stub, Name: name, Kind: graph.NodeKind("unresolved"), FilePath: "", RepoPrefix: "repo"})
		s.AddEdge(&graph.Edge{From: caller, To: stub, Kind: kind, FilePath: file, Line: 500 + i, Confidence: 0.5, Origin: "ast"})
		wants = append(wants, want{caller, target, file, kind, 500 + i})
	}

	// The EXACT shared rule body, name-equality flavour (ResolveUniqueNames).
	const q = `
MATCH (caller:Node)-[e:Edge]->(stub:Node)
WHERE stub.kind = 'unresolved'
WITH e, caller, stub, stub.name AS name
OPTIONAL MATCH (cnd:Node {name: name})
WHERE cnd.kind IN ['type', 'interface']
WITH e, caller, stub, name, count(cnd) AS cnt
WHERE cnt = 1
MATCH (target:Node {name: name})
WHERE target.kind IN ['type', 'interface']
DELETE e
CREATE (caller)-[newE:Edge {
    kind: e.kind,
    file_path: e.file_path,
    line: e.line,
    confidence: e.confidence,
    confidence_label: e.confidence_label,
    origin: 'ast_resolved',
    tier: 'ast_resolved',
    cross_repo: e.cross_repo,
    meta: e.meta
}]->(target)
RETURN count(newE) AS resolved`

	res, err := s.conn.Query(q)
	if err != nil {
		t.Fatalf("rule query: %v", err)
	}
	if res.HasNext() {
		row, _ := res.Next()
		vals, _ := row.GetAsSlice()
		row.Close()
		t.Logf("rule reported resolved=%v (input edges=%d)", vals, N)
	}
	res.Close()

	// Read every resulting edge straight off the rel table.
	all := s.AllEdges()
	type got struct {
		from, to, kind, file string
		line                 int
	}
	var rows []got
	for _, e := range all {
		rows = append(rows, got{e.From, e.To, string(e.Kind), e.FilePath, e.Line})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].line < rows[j].line })

	t.Logf("=== %d edges in rel table after rule (input %d) ===", len(rows), N)
	scrambledKind, scrambledFile, missing, dup := 0, 0, 0, 0
	seenTo := map[string]int{}
	for _, r := range rows {
		seenTo[r.to]++
		t.Logf("  line=%d from=%-26s to=%-26s kind=%-13s file=%s", r.line, r.from, r.to, r.kind, r.file)
	}
	for _, w := range wants {
		// Find the resolved edge for this caller (To == real target).
		var found *got
		for i := range rows {
			if rows[i].from == w.from && rows[i].to == w.to {
				found = &rows[i]
				break
			}
		}
		if found == nil {
			missing++
			continue
		}
		if found.kind != string(w.kind) {
			scrambledKind++
		}
		if found.file != w.file {
			scrambledFile++
		}
	}
	for to, c := range seenTo {
		if c > 1 {
			dup += c - 1
			t.Logf("DUP target %s has %d edges", to, c)
		}
	}
	t.Logf("RESULT: total=%d input=%d missing=%d scrambledKind=%d scrambledFile=%d dupExtra=%d",
		len(rows), N, missing, scrambledKind, scrambledFile, dup)
	if scrambledKind > 0 || scrambledFile > 0 {
		t.Errorf("FIELD SCRAMBLE PROVEN: kind=%d file=%d (from/to preserved)", scrambledKind, scrambledFile)
	}
	if missing > 0 || dup > 0 {
		t.Errorf("EDGE MULTIPLICITY BROKEN: missing=%d dupExtra=%d (count reported != real)", missing, dup)
	}
}
