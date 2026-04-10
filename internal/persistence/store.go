// Package persistence provides snapshot-based graph persistence with pluggable backends.
package persistence

import (
	"errors"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// ErrNotFound is returned when no snapshot exists for the given key.
var ErrNotFound = errors.New("persistence: snapshot not found")

// Snapshot is the unit of persistence — a complete graph state at a point in time.
type Snapshot struct {
	Version    string            `json:"version"`
	RepoPath   string            `json:"repo_path"`
	CommitHash string            `json:"commit_hash"`
	IndexedAt  time.Time         `json:"indexed_at"`
	Nodes      []*graph.Node     `json:"nodes"`
	Edges      []*graph.Edge     `json:"edges"`
	FileMtimes map[string]int64  `json:"file_mtimes"`

	// VectorIndex is the serialized HNSW vector index (nil when embeddings are disabled).
	VectorIndex []byte `json:"vector_index,omitempty"`
	// VectorDims is the embedding dimensionality (0 when embeddings are disabled).
	VectorDims int `json:"vector_dims,omitempty"`
	// VectorCount is the number of vectors in the index.
	VectorCount int `json:"vector_count,omitempty"`
}

// Store is the pluggable persistence backend interface.
// Implementations must be safe for sequential use (not required to be concurrent).
type Store interface {
	// Check returns true if a valid snapshot exists for the given key.
	Check(repoPath, commitHash string) bool

	// Load deserializes and returns the snapshot for the given key.
	// Returns ErrNotFound if no snapshot exists.
	Load(repoPath, commitHash string) (*Snapshot, error)

	// Save serializes the snapshot to persistent storage.
	Save(snap *Snapshot) error

	// Validate checks that an existing snapshot is compatible with
	// the current gortex version. Returns false on version mismatch.
	Validate(repoPath, commitHash string) bool

	// Evict removes the snapshot for the given key.
	Evict(repoPath, commitHash string) error

	// Close releases any resources held by the backend.
	Close() error
}
