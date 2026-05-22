package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeRepoPrefixLookup struct{ prefixes []string }

func (f fakeRepoPrefixLookup) RepoPrefixes() []string              { return f.prefixes }
func (f fakeRepoPrefixLookup) RepoRoot(p string) (string, bool)    { return "/" + p, true }
func (f fakeRepoPrefixLookup) LinkedWorktreeRoots(string) []string { return nil }

// TestMatchedRepoPrefix_LongestMatchWins pins the path→repo inference: a
// nested repo must win over its parent for a path under the child, and
// the result must not depend on RepoPrefixes() iteration order.
func TestMatchedRepoPrefix_LongestMatchWins(t *testing.T) {
	for _, order := range [][]string{{"a", "a/b"}, {"a/b", "a"}} {
		mi := fakeRepoPrefixLookup{prefixes: order}
		require.Equalf(t, "a/b", matchedRepoPrefix(mi, "a/b/internal/x.go"),
			"longest matching prefix must win (order %v)", order)
		require.Equal(t, "a", matchedRepoPrefix(mi, "a/main.go"))
	}
	require.Equal(t, "", matchedRepoPrefix(fakeRepoPrefixLookup{prefixes: []string{"a"}}, "c/x.go"),
		"a path under no tracked repo resolves to no prefix")
	require.Equal(t, "", matchedRepoPrefix(nil, "a/x.go"))
}
