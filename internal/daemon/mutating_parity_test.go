package daemon

import "testing"

// legacyEditingToolNames mirrors the planning-mode editing set that used
// to live in internal/mcp (tools_mode.go::editingToolNames) before the
// consolidation. Duplicated here as a fixture so the parity test fails
// closed if the canonical set ever drops one of them.
var legacyEditingToolNames = []string{
	"edit_file", "edit_symbol", "write_file", "rename_symbol",
}

// cloudMutatingDenied mirrors gortex-cloud internal/proxy.MutatingDenied
// (a separate repo, not buildable from here). Duplicated as a fixture so
// the canonical set can never silently shrink below the cloud denylist.
var cloudMutatingDenied = []string{
	"edit_symbol", "batch_edit", "rename_symbol", "scaffold",
	"index_repository", "track_repository", "untrack_repository",
	"set_active_project", "edit_file", "write_file",
}

// TestMutatingTools_Superset asserts the canonical MutatingTools set is a
// superset of both legacy write-tool lists — the single source of truth
// the planning-mode gate and the federation write-gate both consult.
func TestMutatingTools_Superset(t *testing.T) {
	for _, name := range legacyEditingToolNames {
		if !MutatingTools[name] {
			t.Errorf("MutatingTools is missing legacy editing tool %q", name)
		}
		if !IsMutating(name) {
			t.Errorf("IsMutating(%q) should be true", name)
		}
	}
	for _, name := range cloudMutatingDenied {
		if !MutatingTools[name] {
			t.Errorf("MutatingTools is missing cloud-denied tool %q", name)
		}
	}
}

// TestMutatingTools_ReadToolsExcluded guards against over-broad blocking:
// pure read traversal tools must never be classified as mutating, or the
// planning-mode gate and write-gate would block reads.
func TestMutatingTools_ReadToolsExcluded(t *testing.T) {
	reads := []string{
		"find_usages", "get_callers", "get_call_chain", "find_implementations",
		"get_dependents", "search_symbols", "smart_context", "get_symbol_source",
		"read_file", "graph_stats",
	}
	for _, name := range reads {
		if IsMutating(name) {
			t.Errorf("read tool %q must not be classified as mutating", name)
		}
	}
}

// TestSortedMutatingTools_Stable asserts the surfaced list is sorted and
// complete.
func TestSortedMutatingTools_Stable(t *testing.T) {
	sorted := SortedMutatingTools()
	if len(sorted) != len(MutatingTools) {
		t.Fatalf("sorted list length %d != set size %d", len(sorted), len(MutatingTools))
	}
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1] >= sorted[i] {
			t.Fatalf("not strictly sorted at %d: %q >= %q", i, sorted[i-1], sorted[i])
		}
	}
}
