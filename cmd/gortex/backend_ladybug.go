package main

import (
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_ladybug"
)

// openLadybugBackend opens (or creates) the ladybug store at
// path. Returns a cleanup func that closes the underlying handle
// — important because ladybug's writer locks the directory and
// a subsequent reopen on the same path would fail until the
// previous handle is closed.
func openLadybugBackend(path string, bufferPoolMB uint64) (graph.Store, func(), error) {
	s, err := store_ladybug.OpenWithOptions(path, store_ladybug.Options{
		BufferPoolMB: bufferPoolMB,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open ladybug store at %q: %w", path, err)
	}
	return s, func() { _ = s.Close() }, nil
}

// The daemon warm-restart path consults this optional capability
// (cmd/gortex/daemon_state.go: storeNeedsRebuild) to force a full re-index
// when a schema migration crossed a rebuild rung. This assertion keeps the
// concrete store and the daemon's optional-interface check from drifting.
var _ interface{ NeedsRebuild() bool } = (*store_ladybug.Store)(nil)
