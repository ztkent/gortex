package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// TestSnapshotRoundTrip_Contracts is the regression guard for the bug
// where per-repo contract registries came back empty after every daemon
// restart. The symptom: `contracts` returned only the contracts of the
// repo the user was actively editing, because IncrementalReindex skipped
// extractContracts when no files were stale — and that was the only
// code path writing to idx.contractRegistry. The fix persists each
// repo's registry in the snapshot and warmup rehydrates on load. If
// either side regresses, cross-repo contract queries silently go back
// to returning a fraction of the data.
//
// Test strategy: two repos' worth of contracts get encoded, the
// snapshot is decoded, and every contract must come back with its
// RepoPrefix intact. Comparing against the live Contract values
// (rather than a hand-rolled expected slice) catches any field drift
// between the runtime struct and the wire form.
func TestSnapshotRoundTrip_Contracts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GORTEX_DAEMON_SNAPSHOT", filepath.Join(dir, "snap.gob.gz"))

	repoA := []contracts.Contract{
		{
			ID:         "http::GET::/api/users",
			Type:       contracts.ContractHTTP,
			Role:       contracts.RoleProvider,
			SymbolID:   "repo-a/handler.go::ListUsers",
			FilePath:   "repo-a/handler.go",
			Line:       42,
			RepoPrefix: "repo-a",
			Meta:       map[string]any{"framework": "fiber", "method": "GET", "path": "/api/users"},
			Confidence: 0.9,
		},
		{
			ID:         "grpc::Users::GetUser",
			Type:       contracts.ContractGRPC,
			Role:       contracts.RoleConsumer,
			SymbolID:   "repo-a/client.go::UserClient.Get",
			FilePath:   "repo-a/client.go",
			Line:       17,
			RepoPrefix: "repo-a",
			Meta:       map[string]any{"service": "Users", "method": "GetUser", "lang": "go"},
			Confidence: 0.95,
		},
	}
	repoB := []contracts.Contract{
		{
			ID:         "http::POST::/api/users",
			Type:       contracts.ContractHTTP,
			Role:       contracts.RoleConsumer,
			SymbolID:   "repo-b/client.ts::createUser",
			FilePath:   "repo-b/client.ts",
			Line:       8,
			RepoPrefix: "repo-b",
			Meta:       map[string]any{"framework": "fetch", "method": "POST", "path": "/api/users"},
			Confidence: 0.7,
		},
	}

	var wire []snapshotContract
	for _, c := range repoA {
		wire = append(wire, toSnapshotContract(c))
	}
	for _, c := range repoB {
		wire = append(wire, toSnapshotContract(c))
	}

	g := graph.New()
	saveSnapshot(g, nil, wire, "v-test", zap.NewNop())

	restored := graph.New()
	result, err := loadSnapshot(restored, zap.NewNop())
	require.NoError(t, err)
	require.True(t, result.Loaded)
	require.NotNil(t, result.Contracts)

	// Every contract must come back with its repo_prefix. Without the
	// fix, result.Contracts either wouldn't exist (no map) or would be
	// keyed under "" and conflate providers from different services.
	assert.Len(t, result.Contracts["repo-a"], len(repoA),
		"repo-a contracts must round-trip count-for-count")
	assert.Len(t, result.Contracts["repo-b"], len(repoB),
		"repo-b contracts must round-trip count-for-count")

	// Spot-check a field that used to be silently lost under the
	// "read from graph" fix we considered: confidence. If it's zero,
	// someone removed it from snapshotContract and the wire form is
	// no longer round-tripping.
	got := result.Contracts["repo-a"][0]
	assert.Equal(t, repoA[0].ID, got.ID)
	assert.Equal(t, repoA[0].Type, got.Type)
	assert.Equal(t, repoA[0].Role, got.Role)
	assert.Equal(t, repoA[0].Confidence, got.Confidence)
	assert.Equal(t, repoA[0].Meta["framework"], got.Meta["framework"])
	assert.Equal(t, repoA[0].RepoPrefix, got.RepoPrefix)
}

// TestSnapshotRoundTrip_EmptyContractsCompat proves that a snapshot
// saved with a nil contracts slice still decodes cleanly — the
// `contracts` section is additive, so old-behaviour call sites (tests,
// or a daemon that hasn't collected anything yet) must not produce a
// corrupt or unreadable file. Without this guard, a nil slice plus a
// positive ContractCount from any future refactor would read past the
// stream and corrupt a subsequent snapshot load.
func TestSnapshotRoundTrip_EmptyContractsCompat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GORTEX_DAEMON_SNAPSHOT", filepath.Join(dir, "snap.gob.gz"))

	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Foo", Name: "Foo", Kind: graph.KindFunction, FilePath: "a.go"})

	saveSnapshot(g, nil, nil, "v-test", zap.NewNop())

	restored := graph.New()
	result, err := loadSnapshot(restored, zap.NewNop())
	require.NoError(t, err)
	require.True(t, result.Loaded)
	require.NotNil(t, result.Contracts, "Contracts map must always be non-nil post-load")
	assert.Empty(t, result.Contracts)
}
