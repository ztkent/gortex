package main

import (
	"path/filepath"

	"github.com/zzet/gortex/internal/server"
)

// isLocalhostBind reports whether a bind address is a loopback host, used
// to decide whether the HTTP surface may run without an auth token.
func isLocalhostBind(bind string) bool {
	switch bind {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// resolveServerID loads or creates the per-machine server id. When
// cacheDir is empty the id lives alongside other gortex cache files
// (~/.gortex/cache/server.id); otherwise cacheDir/server.id.
func resolveServerID(cacheDir string) (string, error) {
	path := filepath.Join(cacheDir, "server.id")
	if cacheDir == "" {
		def, err := server.DefaultServerIDPath()
		if err != nil {
			return "", err
		}
		path = def
	}
	return server.LoadOrCreateServerID(path)
}
