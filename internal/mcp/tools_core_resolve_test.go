package mcp

import (
	"context"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// TestResolveRepoFilterStaleActiveProject verifies that an active project
// pointing at a name that does not exist in the loaded GlobalConfig (because
// the user uses per-repo `project: <slug>` annotations under the top-level
// `repos:` list rather than a `projects:` map, for example) does NOT cause
// every per-repo MCP tool to fail. Instead the filter falls back to "all
// repos" so the call still succeeds — mirroring ConfigManager.ActiveRepos.
func TestResolveRepoFilterStaleActiveProject(t *testing.T) {
	g := graph.New()

	// GlobalConfig with NO Projects map. Mirrors the real-world bug:
	// active_project: gortex is set, but the user's config defines repos
	// under a flat top-level `repos:` list with `project: gortex` per-entry,
	// not under a `projects: { gortex: {...} }` map.
	gc := &config.GlobalConfig{
		ActiveProject: "gortex",
		Repos: []config.RepoEntry{
			{Path: "/tmp/repo-a", Name: "alpha"},
			{Path: "/tmp/repo-b", Name: "beta"},
		},
	}

	dir := t.TempDir()
	gcPath := filepath.Join(dir, "config.yaml")
	gc.SetConfigPath(gcPath)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(gcPath)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		ActiveProject: "gortex", // matches the stale config
	})

	// Call resolveRepoFilter with no explicit narrower. Active project
	// "gortex" is not in gc.Projects, so ResolveRepos returns an error.
	// The fix: degrade to nil (no filter), don't propagate the error.
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{}

	allowed, err := srv.resolveRepoFilter(context.Background(), req)
	require.NoError(t, err, "stale active_project must not produce an error")
	require.Nil(t, allowed, "stale active_project must fall back to nil filter (all repos)")
}

// TestResolveRepoFilterExplicitUnknownProject verifies that an explicit
// project parameter naming an unknown project DOES still surface as an
// error — the caller asked for X by name and deserves to know X is wrong.
// This guards against the resilience fix going too far and silently
// swallowing all project-resolution failures.
func TestResolveRepoFilterExplicitUnknownProject(t *testing.T) {
	g := graph.New()
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: "/tmp/repo-a", Name: "alpha"}},
	}

	dir := t.TempDir()
	gcPath := filepath.Join(dir, "config.yaml")
	gc.SetConfigPath(gcPath)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(gcPath)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
	})

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project": "does-not-exist"}

	_, err = srv.resolveRepoFilter(context.Background(), req)
	require.Error(t, err, "explicit unknown project must still surface as error")
	assert.Contains(t, err.Error(), "does-not-exist")
}
