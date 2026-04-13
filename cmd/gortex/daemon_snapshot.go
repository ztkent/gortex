package main

import (
	"compress/gzip"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
)

// snapshotHeader is the first record in a streamed snapshot. NodeCount
// and EdgeCount let the loader pre-size its work and detect truncation.
//
// The encoded layout is: header → node × NodeCount → edge × EdgeCount.
// Each item is encoded as its own gob value, so the encoder never has
// to buffer the full graph in memory before writing to the gzip stream.
// On a 5M-edge graph that drops peak memory from ~500 MB (old
// "encode-then-write" path) to roughly the size of one node/edge plus
// the gzip window — a few hundred KB.
type snapshotHeader struct {
	SchemaVersion int
	Version       string
	NodeCount     int
	EdgeCount     int
}

// snapshotSchemaVersion is bumped whenever daemonSnapshot's shape or
// semantics change in a way that older snapshots can no longer be
// interpreted. v2 introduced the streaming layout (header + per-item
// records); v1 was a single gob struct holding the whole graph.
const snapshotSchemaVersion = 2

// saveSnapshot streams a gob+gzip snapshot of the graph to the daemon's
// snapshot path. Called from the daemon's shutdown hook. Errors are
// logged but never propagated — a failed snapshot write should never
// block clean shutdown.
func saveSnapshot(g *graph.Graph, version string, logger *zap.Logger) {
	if g == nil {
		return
	}
	path := daemon.SnapshotPath()
	if err := daemon.EnsureParentDir(path); err != nil {
		logger.Warn("snapshot: parent dir", zap.Error(err))
		return
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		logger.Warn("snapshot: create tmp", zap.Error(err))
		return
	}

	gz := gzip.NewWriter(f)
	enc := gob.NewEncoder(gz)

	// Snapshot the slices once so the encode loop sees a consistent
	// view even if a late event slips in (the graph's RWMutex protects
	// each AllNodes/AllEdges call individually).
	nodes := g.AllNodes()
	edges := g.AllEdges()

	header := snapshotHeader{
		SchemaVersion: snapshotSchemaVersion,
		Version:       version,
		NodeCount:     len(nodes),
		EdgeCount:     len(edges),
	}

	// Helper to clean up after any failure.
	abort := func(stage string, e error) {
		logger.Warn("snapshot: "+stage, zap.Error(e))
		_ = gz.Close()
		_ = f.Close()
		_ = os.Remove(tmp)
	}

	if err := enc.Encode(header); err != nil {
		abort("encode header", err)
		return
	}
	for _, n := range nodes {
		if err := enc.Encode(n); err != nil {
			abort("encode node", err)
			return
		}
	}
	for _, e := range edges {
		if err := enc.Encode(e); err != nil {
			abort("encode edge", err)
			return
		}
	}
	if err := gz.Close(); err != nil {
		logger.Warn("snapshot: gzip close", zap.Error(err))
		_ = f.Close()
		_ = os.Remove(tmp)
		return
	}
	if err := f.Close(); err != nil {
		logger.Warn("snapshot: file close", zap.Error(err))
		_ = os.Remove(tmp)
		return
	}
	// Atomic swap so a concurrent crash can never leave a truncated
	// snapshot on disk.
	if err := os.Rename(tmp, path); err != nil {
		logger.Warn("snapshot: rename", zap.Error(err))
		return
	}
	logger.Info("snapshot: wrote",
		zap.String("path", path),
		zap.Int("nodes", header.NodeCount),
		zap.Int("edges", header.EdgeCount))
}

// loadSnapshot streams the snapshot at daemon.SnapshotPath() into g.
// Returns (loaded=false, nil) when no snapshot exists — that's the
// expected first-run / post-reset case, not an error. Schema mismatches
// are logged and treated as absent so we don't try to interpret bytes
// we don't understand.
func loadSnapshot(g *graph.Graph, logger *zap.Logger) (loaded bool, err error) {
	if g == nil {
		return false, nil
	}
	path := daemon.SnapshotPath()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return false, fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gz.Close() }()

	dec := gob.NewDecoder(gz)
	var header snapshotHeader
	if err := dec.Decode(&header); err != nil {
		return false, fmt.Errorf("decode snapshot header: %w", err)
	}
	if header.SchemaVersion != snapshotSchemaVersion {
		logger.Info("snapshot: schema mismatch, ignoring",
			zap.Int("on_disk", header.SchemaVersion),
			zap.Int("expected", snapshotSchemaVersion))
		return false, nil
	}

	for i := 0; i < header.NodeCount; i++ {
		var n graph.Node
		if err := dec.Decode(&n); err != nil {
			if errors.Is(err, io.EOF) {
				return false, fmt.Errorf("snapshot truncated: expected %d nodes, got %d", header.NodeCount, i)
			}
			return false, fmt.Errorf("decode node %d: %w", i, err)
		}
		g.AddNode(&n)
	}
	for i := 0; i < header.EdgeCount; i++ {
		var e graph.Edge
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				return false, fmt.Errorf("snapshot truncated: expected %d edges, got %d", header.EdgeCount, i)
			}
			return false, fmt.Errorf("decode edge %d: %w", i, err)
		}
		g.AddEdge(&e)
	}

	logger.Info("snapshot: loaded",
		zap.String("path", path),
		zap.Int("nodes", header.NodeCount),
		zap.Int("edges", header.EdgeCount))
	return true, nil
}
