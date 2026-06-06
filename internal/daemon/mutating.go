package daemon

import "sort"

// MutatingTools is the single source of truth for MCP tools that mutate
// state — files on disk, the graph store, or the project config. It is
// the verified superset of the three previously-disagreeing lists: the
// planning-mode editing set, the cloud proxy's write denylist, and the
// editing entries scattered through the eager-publish set.
//
// It is consulted by BOTH the planning-mode gate (which hides and
// hard-blocks these tools for a read-only session) and the federation
// write-gate (which refuses to route any of these to a remote in v1).
// A tool listed here is NEVER federated.
//
// Keep this list a superset: the parity test asserts every member of
// the legacy lists is present, so the consolidation can never silently
// shrink the deny-set.
var MutatingTools = map[string]bool{
	// File / symbol editors.
	"edit_file":          true,
	"edit_symbol":        true,
	"write_file":         true,
	"rename_symbol":      true,
	"batch_edit":         true,
	"move_symbol":        true,
	"inline_symbol":      true,
	"safe_delete_symbol": true,
	"scaffold":           true,
	// Repo / project / scope mutators.
	"index_repository":   true,
	"reindex_repository": true,
	"track_repository":   true,
	"untrack_repository": true,
	"set_active_project": true,
	"delete_scope":       true,
	"save_scope":         true,
}

// IsMutating reports whether a tool name mutates state.
func IsMutating(name string) bool { return MutatingTools[name] }

// SortedMutatingTools returns the canonical mutating-tool names in
// stable order — used where a deterministic list is surfaced to a
// client (e.g. set_planning_mode's response).
func SortedMutatingTools() []string {
	out := make([]string, 0, len(MutatingTools))
	for n := range MutatingTools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
