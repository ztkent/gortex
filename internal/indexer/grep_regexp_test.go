package indexer

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestExtractRegexLiterals(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		want    []string
	}{
		{"plain literal", "helper", []string{"helper"}},
		{"literal then class", `helper\(`, []string{"helper("}},
		{"concat with class in middle", `func [A-Z]\w+`, []string{"func "}},
		{"short literal dropped", "ab", nil},
		{"alternation drops both", "foo|bar", nil},
		{"star is not mandatory", "x*helper", []string{"helper"}},
		{"plus keeps literal", "(abc)+", []string{"abc"}},
		{"invalid regex yields none", "[unclosed", nil},
		{"anchored literal", "^validateToken$", []string{"validateToken"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRegexLiterals(tc.pattern)
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			assert.Equal(t, want, got)
		})
	}
}

func TestGrepRegexp(t *testing.T) {
	dir := setupTestDir(t)
	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	t.Run("matches a regex use site", func(t *testing.T) {
		hits, rerr := idx.GrepRegexp(`helper\(\)`, "", 0)
		require.NoError(t, rerr)
		require.NotEmpty(t, hits, "helper() should be found by regex")
		found := false
		for _, h := range hits {
			if h.Path == "main.go" {
				found = true
			}
		}
		assert.True(t, found, "regex match should be located in main.go")
	})

	t.Run("character-class regex", func(t *testing.T) {
		hits, rerr := idx.GrepRegexp(`func [a-z]+\(\)`, "", 0)
		require.NoError(t, rerr)
		assert.NotEmpty(t, hits, "func helper() should match the lowercase-name regex")
	})

	t.Run("path prefix scopes the search", func(t *testing.T) {
		hits, rerr := idx.GrepRegexp(`package \w+`, "pkg/", 0)
		require.NoError(t, rerr)
		for _, h := range hits {
			assert.Truef(t, len(h.Path) >= 4 && h.Path[:4] == "pkg/",
				"path-prefixed result %q must be under pkg/", h.Path)
		}
	})

	t.Run("invalid regex returns an error", func(t *testing.T) {
		_, rerr := idx.GrepRegexp("[unclosed", "", 0)
		require.Error(t, rerr)
	})

	t.Run("empty pattern yields nothing", func(t *testing.T) {
		hits, rerr := idx.GrepRegexp("", "", 0)
		require.NoError(t, rerr)
		assert.Empty(t, hits)
	})

	t.Run("limit caps results", func(t *testing.T) {
		hits, rerr := idx.GrepRegexp(`.`, "", 3)
		require.NoError(t, rerr)
		assert.LessOrEqual(t, len(hits), 3)
	})
}
