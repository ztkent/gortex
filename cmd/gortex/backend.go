package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/platform"
)

// openBackend constructs the graph.Store the daemon will run
// against. Picks the implementation by the --backend flag:
//
//   - "memory" (default) — in-process *graph.Graph; nothing
//     persists across runs; matches every existing test fixture.
//
// Returns the store, a cleanup func the caller must defer (closes
// the underlying handle on disk-backed stores), and any error
// constructing or opening the store.
//
// The actual per-backend Open* helpers live in their own
// build-tagged files (backend_memory.go is always built; the
// disk-backed ones are gated by build tags). This file is the
// shared dispatch.
func openBackend(name, path string, bufferPoolMB uint64, logger *zap.Logger) (graph.Store, func(), error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "memory", "mem", "in-memory":
		s := graph.New()
		return s, func() {}, nil
	case "sqlite", "sqlite3":
		resolved, err := resolveBackendPath(path, "store.sqlite")
		if err != nil {
			return nil, nil, err
		}
		logger.Info("opening sqlite backend", zap.String("path", resolved))
		return openSqliteBackend(resolved, bufferPoolMB)
	default:
		return nil, nil, fmt.Errorf("unknown --backend %q (expected: memory, sqlite)", name)
	}
}

// resolveBackendPath turns an empty --backend-path into a default
// under the unified store directory (~/.gortex/store/<filename>, or the
// XDG_DATA_HOME equivalent). Otherwise expands ~ and returns the
// absolute path. Creates the parent directory if missing — the
// disk-backed stores expect the parent dir to exist.
func resolveBackendPath(in, filename string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		in = filepath.Join(platform.StoreDir(), filename)
	} else if strings.HasPrefix(in, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		in = filepath.Join(home, in[2:])
	}
	abs, err := filepath.Abs(in)
	if err != nil {
		return "", fmt.Errorf("abs path %q: %w", in, err)
	}
	// The on-disk store opens the leaf path (file or directory). We
	// MkdirAll the parent so the path is reachable; the store itself
	// creates the leaf.
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent %q: %w", filepath.Dir(abs), err)
	}
	return abs, nil
}
