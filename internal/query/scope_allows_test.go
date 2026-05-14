package query

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// TestQueryOptions_ScopeAllows is the per-node enforcement matrix for
// the workspace boundary. It covers the singleton fallback (a node
// with no declared workspace is keyed on its repo prefix) which keeps
// the session boundary consistent with how the indexer stamps nodes.
func TestQueryOptions_ScopeAllows(t *testing.T) {
	node := func(ws, proj, prefix string) *graph.Node {
		return &graph.Node{WorkspaceID: ws, ProjectID: proj, RepoPrefix: prefix}
	}

	tests := []struct {
		name string
		opts QueryOptions
		node *graph.Node
		want bool
	}{
		{
			name: "empty scope allows everything",
			opts: QueryOptions{},
			node: node("vio", "", "rate_checkers_detector"),
			want: true,
		},
		{
			// ScopeAllows treats a nil node as "allow" — every caller
			// nil-checks before calling (the engine traversal does, and
			// the MCP layer's nodeInSessionScope rejects nil itself).
			name: "nil node passes (callers nil-check upstream)",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: nil,
			want: true,
		},
		{
			name: "same workspace passes",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: node("gortex", "", "gortex"),
			want: true,
		},
		{
			name: "different workspace is rejected",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: node("vio", "", "rate_checkers_detector"),
			want: false,
		},
		{
			name: "node with empty workspace falls back to repo prefix — match",
			opts: QueryOptions{WorkspaceID: "rate_checkers_detector"},
			node: node("", "", "rate_checkers_detector"),
			want: true,
		},
		{
			name: "node with empty workspace falls back to repo prefix — mismatch",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: node("", "", "rate_checkers_detector"),
			want: false,
		},
		{
			name: "project narrows within the workspace — match",
			opts: QueryOptions{WorkspaceID: "gortex", ProjectID: "web"},
			node: node("gortex", "web", "web"),
			want: true,
		},
		{
			name: "project narrows within the workspace — mismatch",
			opts: QueryOptions{WorkspaceID: "gortex", ProjectID: "web"},
			node: node("gortex", "core", "core"),
			want: false,
		},
		{
			name: "empty project on opts allows any project in the workspace",
			opts: QueryOptions{WorkspaceID: "gortex"},
			node: node("gortex", "web", "web"),
			want: true,
		},
		{
			name: "project match cannot rescue a workspace mismatch",
			opts: QueryOptions{WorkspaceID: "gortex", ProjectID: "web"},
			node: node("vio", "web", "web"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.opts.ScopeAllows(tt.node))
		})
	}
}
