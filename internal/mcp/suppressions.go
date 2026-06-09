package mcp

import (
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/review"
)

// suppressionManager binds the durable per-repo review-finding suppression
// store (review.SuppressionStore over the SQLite sidecar) to a single
// repository's cache key. It is the MCP-side hang-point the review tools read:
// the gate consults Store for the active repo's never-flag-again list, and the
// suppress_finding tool mutates it. A nil store / empty repoKey is tolerated by
// every review.SuppressionStore method, so a manager with no disk still works.
type suppressionManager struct {
	store   *review.SuppressionStore
	repoKey string
}

// newSuppressionManager opens (or reuses) the shared sidecar at
// <cacheDir>/sidecar.sqlite and binds a suppression store to repoPath's cache
// key. Empty cacheDir/repoPath — or an unopenable sidecar — yields an in-memory
// no-op manager whose store suppresses nothing.
func newSuppressionManager(cacheDir, repoPath string) *suppressionManager {
	if cacheDir == "" || repoPath == "" {
		return &suppressionManager{store: review.NewSuppressionStore(nil)}
	}
	sidecar, err := persistence.OpenSidecar(persistence.DefaultSidecarPath(cacheDir))
	if err != nil || sidecar == nil {
		return &suppressionManager{store: review.NewSuppressionStore(nil)}
	}
	return &suppressionManager{
		store:   review.NewSuppressionStore(sidecar),
		repoKey: persistence.RepoCacheKey(repoPath),
	}
}

// RepoKey returns the per-repo suppression scope. Empty for an in-memory
// manager.
func (m *suppressionManager) RepoKey() string {
	if m == nil {
		return ""
	}
	return m.repoKey
}

// Store returns the underlying review.SuppressionStore (nil-safe — a nil
// manager returns a nil store, which review.SuppressionStore tolerates).
func (m *suppressionManager) Store() *review.SuppressionStore {
	if m == nil {
		return nil
	}
	return m.store
}
