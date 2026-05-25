//go:build ladybug

package store_ladybug

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestFTS_Probe is a one-shot capability probe: does the bundled
// liblbug actually expose the CALL CREATE_FTS_INDEX /
// CALL QUERY_FTS_INDEX surface? If it does, the production FTS
// integration is unblocked; if not, we need a different
// installation strategy or a fallback.
//
// Sequence:
//  1. seed three Node rows (search target, near miss, far miss)
//  2. try CALL CREATE_FTS_INDEX directly; on extension-not-loaded,
//     fall back to INSTALL fts + LOAD EXTENSION fts + retry
//  3. CALL QUERY_FTS_INDEX with a query that should rank the
//     two related rows above the unrelated one
//
// The test logs results rather than asserting strict ordering so a
// schema or scoring tweak doesn't fail the probe — what matters is
// "the surface exists and returns rows".
func TestFTS_Probe(t *testing.T) {
	dir, err := os.MkdirTemp("", "lbug-fts-probe-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	s, err := Open(filepath.Join(dir, "store.lbug"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, n := range []*graph.Node{
		{ID: "pkg/auth.go::ValidateToken", Kind: graph.KindFunction, Name: "ValidateToken", QualName: "auth.ValidateToken", FilePath: "pkg/auth.go", Language: "go"},
		{ID: "pkg/auth.go::ValidateSession", Kind: graph.KindFunction, Name: "ValidateSession", QualName: "auth.ValidateSession", FilePath: "pkg/auth.go", Language: "go"},
		{ID: "pkg/format.go::PrettyPrint", Kind: graph.KindFunction, Name: "PrettyPrint", QualName: "format.PrettyPrint", FilePath: "pkg/format.go", Language: "go"},
	} {
		s.AddNode(n)
	}
	t.Logf("seeded %d nodes", s.NodeCount())

	// Step 1: try CREATE_FTS_INDEX directly.
	createErr := tryRunCypher(s, `CALL CREATE_FTS_INDEX('Node', 'idx_name_fts', ['name', 'qual_name'])`)
	if createErr != nil {
		t.Logf("direct CREATE_FTS_INDEX failed: %v — falling through to INSTALL/LOAD", createErr)

		// Step 2: install + load + retry. Ladybug inherits Kuzu's
		// extension-loading semantics; FTS may need to be explicitly
		// loaded even though the symbols are compiled in.
		if err := tryRunCypher(s, `INSTALL fts`); err != nil {
			t.Logf("INSTALL fts: %v", err)
		}
		if err := tryRunCypher(s, `LOAD EXTENSION fts`); err != nil {
			t.Logf("LOAD EXTENSION fts: %v", err)
		}
		if err := tryRunCypher(s, `CALL CREATE_FTS_INDEX('Node', 'idx_name_fts', ['name', 'qual_name'])`); err != nil {
			t.Fatalf("CREATE_FTS_INDEX retry failed: %v", err)
		}
	}
	t.Log("FTS index created")

	// Capability check: does the index auto-update on a node added
	// AFTER index creation? Critical for incremental indexing.
	s.AddNode(&graph.Node{ID: "pkg/late.go::LateAdded", Kind: graph.KindFunction, Name: "lateadded", QualName: "late.lateadded", FilePath: "pkg/late.go", Language: "go"})
	postRows, postErr := tryQueryCypher(s, `CALL QUERY_FTS_INDEX('Node', 'idx_name_fts', 'lateadded') RETURN node.id AS id ORDER BY score DESC LIMIT 5`, nil)
	t.Logf("after post-create AddNode, query 'lateadded' → %d rows (err=%v): %v", len(postRows), postErr, postRows)

	// Step 3: query. The binder expects exactly three STRING args
	// (table, index, query) — no limit parameter; truncate with
	// LIMIT N at the Cypher level instead.
	//
	// Try several query shapes to learn how Ladybug's FTS tokenises:
	for _, probe := range []string{
		"validate token",     // two-word natural query
		"validatetoken",      // single concat (default tokeniser may have lower-cased CamelCase as one token)
		"ValidateToken",      // case-preserved
		"validate",           // single word
		"auth",               // qualifier token
		"PrettyPrint",        // far-miss target as control
	} {
		rows, qerr := tryQueryCypher(s, `CALL QUERY_FTS_INDEX('Node', 'idx_name_fts', $q) RETURN node.id AS id, score ORDER BY score DESC LIMIT 10`, map[string]any{
			"q": probe,
		})
		if qerr != nil {
			t.Logf("query %q: error: %v", probe, qerr)
			continue
		}
		t.Logf("query %q → %d rows", probe, len(rows))
		for _, r := range rows {
			t.Logf("  %v", r)
		}
	}
}

// tryRunCypher invokes runWriteLocked and captures any panic /
// runtime error the binding raises so the probe can react to
// "extension not loaded" without aborting.
func tryRunCypher(s *Store, q string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = recoverErr(r)
		}
	}()
	s.runWriteLocked(q, nil)
	return nil
}

func tryQueryCypher(s *Store, q string, args map[string]any) (rows [][]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = recoverErr(r)
		}
	}()
	rows = s.querySelect(q, args)
	return rows, nil
}

func recoverErr(r any) error {
	if e, ok := r.(error); ok {
		return e
	}
	return &probeErr{msg: strings.TrimSpace(toString(r))}
}

type probeErr struct{ msg string }

func (e *probeErr) Error() string { return e.msg }

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case error:
		return t.Error()
	default:
		return ""
	}
}
