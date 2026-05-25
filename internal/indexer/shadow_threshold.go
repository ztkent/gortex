package indexer

import (
	"os"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// defaultShadowMaxFileCount caps the file count above which IndexCtx
// refuses to swap idx.graph for an in-memory shadow during cold start.
// Picked empirically from the in-memory store's prior profiling: at
// ~35k C files (drivers/) the in-memory store peaked at 8.6GB RSS; at
// 60k+ the peak is well past 16GB. The shadow path doubles that
// footprint (in-memory + persisted disk copy at the FlushBulk step),
// so the safe ceiling for a 32GB dev machine sits around 50k source
// files. Above that we fall through to the per-call disk path —
// slower per IndexCtx but bounded RAM.
const defaultShadowMaxFileCount = 50000

// defaultStreamingChunkSize is the per-chunk file count used by the
// streaming-flush path. At ~30 nodes / ~100 edges per file, 5000
// files per chunk yields a ~600MB shadow that fits comfortably in
// RAM even on 8GB build agents.
const defaultStreamingChunkSize = 5000

// shadowMaxFileCount returns the active file-count ceiling for the
// IndexCtx in-memory shadow swap. GORTEX_SHADOW_MAX_FILES overrides
// the default; setting it to 0 disables the shadow entirely (always
// run against the disk store directly), setting it to a high value
// (e.g. 10_000_000) effectively disables the guard. Non-numeric or
// negative values fall back to the default.
func shadowMaxFileCount() int {
	if v := os.Getenv("GORTEX_SHADOW_MAX_FILES"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n >= 0 {
			return n
		}
	}
	return defaultShadowMaxFileCount
}

// streamingFlushActive reports whether the streaming-flush parse path
// should engage for this IndexCtx. Requirements:
//
//   - the backing store implements graph.BulkLoader (ladybug,
//     duckdb, sqlite all do)
//   - the file count is above the shadow-max threshold (small repos
//     stay on the all-in-memory shadow path)
//   - GORTEX_STREAMING_FLUSH is enabled (off by default — the
//     streaming path leaves resolve to the disk-only per-call path,
//     so it's only useful when shadow swap can't fit in RAM)
func streamingFlushActive(store graph.Store, fileCount int) bool {
	if _, ok := store.(graph.BulkLoader); !ok {
		return false
	}
	if fileCount <= shadowMaxFileCount() {
		return false
	}
	v := os.Getenv("GORTEX_STREAMING_FLUSH")
	return v == "1" || strings.EqualFold(v, "true")
}

// streamingChunkSize returns the per-chunk file count for the
// streaming-flush path. Override via GORTEX_STREAMING_CHUNK_SIZE.
func streamingChunkSize() int {
	if v := os.Getenv("GORTEX_STREAMING_CHUNK_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
	}
	return defaultStreamingChunkSize
}
