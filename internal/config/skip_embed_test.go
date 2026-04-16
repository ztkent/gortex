package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldSkipEmbed_Matches(t *testing.T) {
	rules := []SkipEmbedRule{
		{Language: "css", Kinds: []string{"variable", "type"}},
		{Language: "hcl", Kinds: []string{"type"}},
	}
	cases := []struct {
		lang, kind string
		want       bool
	}{
		{"css", "variable", true},
		{"css", "type", true},
		{"css", "function", false},
		{"hcl", "type", true},
		{"hcl", "variable", false},
		{"go", "function", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got := ShouldSkipEmbed(rules, tc.lang, tc.kind)
		if got != tc.want {
			t.Errorf("ShouldSkipEmbed(%q,%q) = %v, want %v", tc.lang, tc.kind, got, tc.want)
		}
	}
}

func TestShouldSkipEmbed_NilRules(t *testing.T) {
	if ShouldSkipEmbed(nil, "css", "variable") {
		t.Error("nil rule list should match nothing")
	}
}

func TestDefaultSkipEmbed_CoversExpectedLanguages(t *testing.T) {
	rules := DefaultSkipEmbed()
	languages := map[string]bool{}
	for _, r := range rules {
		languages[r.Language] = true
	}
	// Don't hard-code the full list — just pin the ones the team asked
	// for so regressions are obvious.
	for _, want := range []string{"css", "hcl", "yaml", "toml", "bash"} {
		if !languages[want] {
			t.Errorf("DefaultSkipEmbed missing language %q", want)
		}
	}
}

func TestGetRepoConfig_SkipEmbedFallsBackToDefault(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	cfg := cm.GetRepoConfig("unknown-repo")
	// No workspace → should get compiled-in defaults populated in
	// Semantic.SkipEmbed by Default(), and mirrored into Index.SkipEmbed
	// by GetRepoConfig.
	assert.NotEmpty(t, cfg.Index.SkipEmbed, "Index.SkipEmbed should be populated for indexer")
	assert.NotEmpty(t, cfg.Semantic.SkipEmbed, "Semantic.SkipEmbed should carry the defaults")
}

func TestGetRepoConfig_SkipEmbedFromWorkspace(t *testing.T) {
	cm, err := NewConfigManager("/tmp/nonexistent-gortex-test-cm/config.yaml")
	require.NoError(t, err)

	repoDir := t.TempDir()
	wsContent := `
semantic:
  skip_embed:
    - language: foo
      kinds: [bar]
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("r", repoDir)

	cfg := cm.GetRepoConfig("r")
	require.Len(t, cfg.Index.SkipEmbed, 1)
	assert.Equal(t, "foo", cfg.Index.SkipEmbed[0].Language)
	assert.Equal(t, []string{"bar"}, cfg.Index.SkipEmbed[0].Kinds)
}
