package main

import (
	"compress/gzip"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
)

// snapshotRepo carries the per-repo metadata needed to reconcile a
// restarting daemon with the filesystem: specifically, FileMtimes so
// IncrementalReindex can skip unchanged files and evict deleted ones.
// Added additively — absent in v≤2 snapshots, where RepoCount decodes
// as zero and the repo section is empty.
type snapshotRepo struct {
	RepoPrefix string
	RootPath   string
	FileMtimes map[string]int64
}

// snapshotContract is the wire form of contracts.Contract. Persisted so
// per-repo contract registries survive daemon restarts without having to
// re-run extractContracts during warmup — in steady state IncrementalReindex
// skips the extraction step entirely, which used to leave the registry nil
// for every repo whose mtimes hadn't drifted. Isolates the wire schema from
// unrelated evolution of the runtime Contract type: an additive field on
// contracts.Contract does not force a snapshot migration so long as the two
// shapes stay aligned on the fields we care about persisting.
type snapshotContract struct {
	ID         string
	Type       string
	Role       string
	SymbolID   string
	FilePath   string
	Line       int
	RepoPrefix string
	Meta       map[string]any
	Confidence float64
}

// snapshotLoadResult reports the outcome of loadSnapshot. Partial is
// true when any record was skipped (corrupt, dangling, or structurally
// invalid) so warmup can decide whether to force a fuller reconcile.
type snapshotLoadResult struct {
	Loaded    bool
	Partial   bool
	Repos     map[string]*snapshotRepo
	Contracts map[string][]contracts.Contract
}

// isStaleAbsPathID reports whether a node ID begins with an absolute
// filesystem path — a leftover from a prior-version code path that wrote
// abs paths into IDs instead of repo-prefixed relative ones. Current
// indexing never produces such IDs, so any found in a snapshot are stale
// duplicates of a properly-prefixed node and must be dropped on load.
func isStaleAbsPathID(id string) bool {
	return strings.HasPrefix(id, "/")
}

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
	// RepoCount is the number of snapshotRepo records that follow the
	// nodes and edges sections. Added additively in the resilience work;
	// older snapshots decode this as zero (gob skips unknown fields),
	// so a newer daemon reading an older snapshot simply gets no
	// per-repo reconciliation metadata and falls back to full re-index.
	RepoCount int
	// ContractCount is the number of snapshotContract records that follow
	// the repo section. Added additively: older snapshots decode this as
	// zero and the loader emits an empty Contracts map, which warmup
	// treats as "re-extract on next stale file" — identical to the
	// pre-contracts-persistence behaviour.
	ContractCount int
}

// snapshotSchemaVersion is bumped whenever daemonSnapshot's shape or
// semantics change in a way that older snapshots can no longer be
// interpreted. v2 introduced the streaming layout (header + per-item
// records); v1 was a single gob struct holding the whole graph.
//
// ──────────────────────────── Wire contract ─────────────────────────────
// graph.Node, graph.Edge, snapshotHeader, and snapshotRepo are wire
// contracts. Daemons in the wild write v_n snapshots; daemons at v_{n+k}
// must still load them. Rules:
//
//   • Additive field changes (new field, unused by older readers) do
//     NOT require a schema bump — gob decodes unknown fields as zero,
//     and newer fields on older writers stay zero on newer readers.
//
//   • Renames, type changes, or removals on existing fields DO require
//     a schema bump + migration entry in snapshotMigrations. The gob
//     stream is field-name-tagged; renaming breaks decode silently.
//
//   • CI guard: TestWireContractFingerprint (wire_contract_test.go)
//     hashes the exported fields of the four wire types above and
//     fails any PR that drifts the fingerprint without updating the
//     pinned golden. Runs as part of the normal `go test ./...` sweep.
//
// See spec-daemon-resilience.md §"Snapshot Durability" for the
// rationale — we explicitly chose graceful degradation + additive
// discipline over a heavy migration framework that would ossify these
// structs prematurely.
const snapshotSchemaVersion = 2

// snapshotMigration runs when an on-disk snapshot is at a lower
// schema version than the daemon. It reads the old-format gob stream
// from `in`, rewrites it as the next version's layout, and writes the
// result to `out`. Chained by loadSnapshot when a version gap spans
// multiple steps. Start empty — premature migration frameworks encode
// the wrong abstractions; we add entries only on genuine breaking
// changes.
type snapshotMigration func(in io.Reader, out io.Writer) error

// snapshotMigrations is the in-process migration registry. Keyed by
// the source schema version: migrations[N] turns an N-format snapshot
// into (N+1)-format. Absence of a migration for some version in the
// gap → fall through to rebuild (current behaviour unchanged).
//
// Intentionally empty. Phase 1's graceful per-record decode plus the
// additive-field discipline above handles ~90% of upgrades without a
// migration. The first entry lands the day a real breaking change
// ships; until then the map existing is just scaffolding so the
// migration call site doesn't have to be invented under deadline
// pressure later.
var snapshotMigrations = map[int]snapshotMigration{}

// canMigrate reports whether a migration chain exists that bridges
// `from` → `to`. Used by loadSnapshot to decide between "migrate" and
// "discard the cache." Today this always returns false because the
// registry is empty; wired up so adding a migration doesn't require
// touching the loader's conditional.
func canMigrate(from, to int) bool {
	if from >= to {
		return false
	}
	for v := from; v < to; v++ {
		if _, ok := snapshotMigrations[v]; !ok {
			return false
		}
	}
	return true
}

// saveSnapshot streams a gob+gzip snapshot of the graph to the daemon's
// snapshot path. Called from the daemon's shutdown hook. Errors are
// logged but never propagated — a failed snapshot write should never
// block clean shutdown. The repos slice carries per-repo FileMtimes so
// the next warmup can use IncrementalReindex instead of a full re-scan.
// The contracts slice carries per-repo contract entries so the warmup
// can rehydrate each indexer's contracts.Registry without re-running the
// extractors — IncrementalReindex skips extraction in steady state, so
// without this the registries came back nil after every restart.
func saveSnapshot(g *graph.Graph, repos []snapshotRepo, snapContracts []snapshotContract, version string, logger *zap.Logger) {
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
		RepoCount:     len(repos),
		ContractCount: len(snapContracts),
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
	for i := range repos {
		if err := enc.Encode(repos[i]); err != nil {
			abort("encode repo", err)
			return
		}
	}
	for i := range snapContracts {
		if err := enc.Encode(snapContracts[i]); err != nil {
			abort("encode contract", err)
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
		zap.Int("edges", header.EdgeCount),
		zap.Int("repos", header.RepoCount),
		zap.Int("contracts", header.ContractCount))
}

// toSnapshotContract flattens a contracts.Contract into its wire form.
// The runtime type alias members (ContractType, Role) are stringified so
// the snapshot struct carries only primitive-typed fields and the
// migration rules stay predictable.
func toSnapshotContract(c contracts.Contract) snapshotContract {
	return snapshotContract{
		ID:         c.ID,
		Type:       string(c.Type),
		Role:       string(c.Role),
		SymbolID:   c.SymbolID,
		FilePath:   c.FilePath,
		Line:       c.Line,
		RepoPrefix: c.RepoPrefix,
		Meta:       c.Meta,
		Confidence: c.Confidence,
	}
}

// fromSnapshotContract rebuilds the runtime Contract from its wire form.
// Unknown Type / Role strings are passed through — the extractors wrote
// them, and rejecting a value we still understand structurally would
// silently drop real contracts in an edge case we have no reason to
// force.
func fromSnapshotContract(s snapshotContract) contracts.Contract {
	return contracts.Contract{
		ID:         s.ID,
		Type:       contracts.ContractType(s.Type),
		Role:       contracts.Role(s.Role),
		SymbolID:   s.SymbolID,
		FilePath:   s.FilePath,
		Line:       s.Line,
		RepoPrefix: s.RepoPrefix,
		Meta:       s.Meta,
		Confidence: s.Confidence,
	}
}

// loadSnapshot streams the snapshot at daemon.SnapshotPath() into g.
// Returns (Loaded=false) when no snapshot exists — that's the expected
// first-run / post-reset case, not an error. Schema mismatches are
// logged and treated as absent so we don't try to interpret bytes we
// don't understand.
//
// Per-record decode failures do not abort the load — they're logged and
// counted, the whole record is dropped, and the graph state is
// structurally validated before return (dangling edges pruned). This
// trades "one bad byte poisons the entire cache" for "N bad records
// cost at most N files being re-indexed on next warmup."
func loadSnapshot(g *graph.Graph, logger *zap.Logger) (snapshotLoadResult, error) {
	// Allocate Contracts up front so every early-return path (missing
	// file, gzip error, header decode error, schema mismatch) hands the
	// caller a safe-to-read zero-value instead of a nil map. The warmup
	// path `range state.snapshotContracts` over a nil map is fine in Go,
	// but a nil result is a gotcha other call sites have hit before.
	result := snapshotLoadResult{
		Contracts: make(map[string][]contracts.Contract),
	}
	if g == nil {
		return result, nil
	}
	path := daemon.SnapshotPath()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return result, fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gz.Close() }()

	dec := gob.NewDecoder(gz)
	var header snapshotHeader
	if err := dec.Decode(&header); err != nil {
		return result, fmt.Errorf("decode snapshot header: %w", err)
	}
	if header.SchemaVersion != snapshotSchemaVersion {
		// Schema gap — try the migration chain. Empty registry
		// falls through to "ignoring" with the same logging the old
		// code emitted, so no behaviour change for v2 users today.
		if canMigrate(header.SchemaVersion, snapshotSchemaVersion) {
			logger.Info("snapshot: schema migration available but not yet wired up",
				zap.Int("on_disk", header.SchemaVersion),
				zap.Int("expected", snapshotSchemaVersion))
		}
		logger.Info("snapshot: schema mismatch, ignoring",
			zap.Int("on_disk", header.SchemaVersion),
			zap.Int("expected", snapshotSchemaVersion))
		return result, nil
	}

	// Snapshots can carry stale nodes whose IDs begin with an absolute
	// filesystem path — leftovers from prior-version indexing bugs. Drop
	// them on load; re-indexing the tracked repos recreates clean
	// repo-prefixed replacements. Edges pointing at dropped nodes are
	// skipped so the graph never contains dangling references.
	droppedNodes := make(map[string]struct{})
	var skippedNodes, skippedEdges, corruptNodes, corruptEdges, corruptRepos, corruptContracts int
	loadedIDs := make(map[string]struct{})
	for i := 0; i < header.NodeCount; i++ {
		var n graph.Node
		if err := dec.Decode(&n); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// Truncation is irrecoverable — the remaining records
				// are gone. Validate what we have and return partial.
				logger.Warn("snapshot: truncated during nodes",
					zap.Int("expected", header.NodeCount),
					zap.Int("read", i),
					zap.Error(err))
				result.Partial = true
				goto validate
			}
			// A single corrupt record in an otherwise-valid stream:
			// skip it, keep going. Surviving the bad byte is the whole
			// point of per-record decode; the alternative is dropping
			// millions of good nodes over one bad one.
			corruptNodes++
			result.Partial = true
			continue
		}
		if n.ID == "" {
			corruptNodes++
			result.Partial = true
			continue
		}
		if isStaleAbsPathID(n.ID) {
			droppedNodes[n.ID] = struct{}{}
			skippedNodes++
			continue
		}
		g.AddNode(&n)
		loadedIDs[n.ID] = struct{}{}
	}
	for i := 0; i < header.EdgeCount; i++ {
		var e graph.Edge
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				logger.Warn("snapshot: truncated during edges",
					zap.Int("expected", header.EdgeCount),
					zap.Int("read", i),
					zap.Error(err))
				result.Partial = true
				goto validate
			}
			corruptEdges++
			result.Partial = true
			continue
		}
		if _, drop := droppedNodes[e.From]; drop {
			skippedEdges++
			continue
		}
		if _, drop := droppedNodes[e.To]; drop {
			skippedEdges++
			continue
		}
		// Structural validation: drop edges whose endpoints weren't
		// loaded (either corrupt-skipped or never in the snapshot).
		if _, ok := loadedIDs[e.From]; !ok {
			skippedEdges++
			continue
		}
		if _, ok := loadedIDs[e.To]; !ok {
			skippedEdges++
			continue
		}
		g.AddEdge(&e)
	}

	if header.RepoCount > 0 {
		result.Repos = make(map[string]*snapshotRepo, header.RepoCount)
		for i := 0; i < header.RepoCount; i++ {
			var r snapshotRepo
			if err := dec.Decode(&r); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					logger.Warn("snapshot: truncated during repos",
						zap.Int("expected", header.RepoCount),
						zap.Int("read", i),
						zap.Error(err))
					result.Partial = true
					goto validate
				}
				corruptRepos++
				result.Partial = true
				continue
			}
			if r.RepoPrefix == "" {
				corruptRepos++
				result.Partial = true
				continue
			}
			result.Repos[r.RepoPrefix] = &r
		}
	}

	if header.ContractCount > 0 {
		for i := 0; i < header.ContractCount; i++ {
			var sc snapshotContract
			if err := dec.Decode(&sc); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					logger.Warn("snapshot: truncated during contracts",
						zap.Int("expected", header.ContractCount),
						zap.Int("read", i),
						zap.Error(err))
					result.Partial = true
					goto validate
				}
				corruptContracts++
				result.Partial = true
				continue
			}
			if sc.ID == "" {
				corruptContracts++
				result.Partial = true
				continue
			}
			result.Contracts[sc.RepoPrefix] = append(result.Contracts[sc.RepoPrefix], fromSnapshotContract(sc))
		}
	}

validate:
	// The load reached here either cleanly or via a truncation goto —
	// in both cases validate what's in the graph before returning.

	totalContracts := 0
	for _, cs := range result.Contracts {
		totalContracts += len(cs)
	}
	logger.Info("snapshot: loaded",
		zap.String("path", path),
		zap.Int("nodes", header.NodeCount-skippedNodes-corruptNodes),
		zap.Int("edges", header.EdgeCount-skippedEdges-corruptEdges),
		zap.Int("repos", len(result.Repos)),
		zap.Int("contracts", totalContracts),
		zap.Int("stale_nodes_dropped", skippedNodes),
		zap.Int("stale_edges_dropped", skippedEdges),
		zap.Int("corrupt_nodes_skipped", corruptNodes),
		zap.Int("corrupt_edges_skipped", corruptEdges),
		zap.Int("corrupt_repos_skipped", corruptRepos),
		zap.Int("corrupt_contracts_skipped", corruptContracts))
	result.Loaded = true
	return result, nil
}
