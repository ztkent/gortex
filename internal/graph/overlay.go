package graph

import (
	"sort"
	"strings"
	"sync"
)

// OverlayLayer is one MCP session's parsed editor-buffer state. It
// holds the nodes and edges that the overlay introduces (or hides via
// tombstones) on top of an immutable base graph. The layer is built
// once per (session, content-hash) tuple by the MCP overlay middleware
// (`internal/mcp/overlay_view.go::buildOverlayLayer`) and is consulted
// read-only by `OverlaidView`.
//
// **Identity is preserved.** Gortex node IDs are derived from
// `file::symbol` paths, so a symbol that exists in both the on-disk
// and overlay versions of a file ends up with the same ID — the
// view substitutes the overlay's version transparently. New overlay
// symbols (a function the user just typed) get IDs that don't exist
// in base; deleted symbols (removed from the buffer) simply aren't in
// the layer's per-file node list.
//
// The layer is immutable after construction. The middleware never
// mutates it once the View is in flight; the base graph is never
// mutated by overlay flow at all. This is what makes the design
// safe for concurrent multi-session deployments — no shared mutable
// state between sessions or between an overlay-active session and a
// non-overlay session.
type OverlayLayer struct {
	// Files covered by the overlay. The key is the file's graph path
	// (repo-prefixed in multi-repo mode). Presence in this map means
	// "the View should hide base's view of this path" — either to
	// replace it with overlay content (entries[path] != nil) or to
	// tombstone it (entries[path].Deleted).
	entries map[string]*overlayFileEntry

	// nodeByID lets GetNode hit a single map lookup. Holds every
	// non-tombstoned overlay node across every overlay file.
	nodeByID map[string]*Node

	// outEdges maps each overlay-introduced source node ID to its
	// resolved outgoing edges. Filled by the local resolver pass at
	// layer construction.
	outEdges map[string][]*Edge

	// inEdges is the reverse index of outEdges keyed by target ID,
	// so OverlaidView.GetInEdges can merge overlay-originating
	// edges with base in-edges in O(1).
	inEdges map[string][]*Edge

	// nodesByName/Qual index overlay nodes for FindNodesByName /
	// GetNodeByQualName fast paths.
	nodesByName map[string][]*Node
	nodesByQual map[string]*Node

	// nameRemoved is the set of (name → IDs from base that are no
	// longer present under the View). FindNodesByName uses this to
	// filter base hits whose enclosing file is overlaid but whose
	// id disappeared from the overlay's node list.
	nameRemoved map[string]map[string]bool
}

// overlayFileEntry carries one file's overlay state inside the
// layer. Deleted=true is the tombstone variant — no nodes, no edges.
type overlayFileEntry struct {
	Path    string
	Deleted bool
	Nodes   []*Node
}

// NewOverlayLayer constructs an empty layer. Callers build it up via
// AddFile / AddNode / AddEdge during the per-request layer-build
// pass, then freeze it by handing it to NewOverlaidView. After that
// point the layer is treated as immutable; the View never writes
// back.
func NewOverlayLayer() *OverlayLayer {
	return &OverlayLayer{
		entries:     make(map[string]*overlayFileEntry),
		nodeByID:    make(map[string]*Node),
		outEdges:    make(map[string][]*Edge),
		inEdges:     make(map[string][]*Edge),
		nodesByName: make(map[string][]*Node),
		nodesByQual: make(map[string]*Node),
		nameRemoved: make(map[string]map[string]bool),
	}
}

// MarkFile registers an overlay file. Call once per overlay path
// before AddNode / AddEdge for that file. `deleted` true means the
// path is a tombstone — the View hides base's view of the path
// entirely, returning no nodes from GetFileNodes and treating the
// path's node IDs as non-existent for GetNode.
func (l *OverlayLayer) MarkFile(graphPath string, deleted bool) {
	l.entries[graphPath] = &overlayFileEntry{Path: graphPath, Deleted: deleted}
}

// AddNode attaches one parsed overlay node to the layer. Must be
// called after MarkFile for the node's file. Idempotent on (graphPath,
// node ID) — second add silently replaces.
func (l *OverlayLayer) AddNode(graphPath string, n *Node) {
	if n == nil {
		return
	}
	entry, ok := l.entries[graphPath]
	if !ok {
		entry = &overlayFileEntry{Path: graphPath}
		l.entries[graphPath] = entry
	}
	if entry.Deleted {
		// Tombstone: silently drop. Caller bug — but cheap to absorb.
		return
	}
	entry.Nodes = append(entry.Nodes, n)
	l.nodeByID[n.ID] = n
	if n.Name != "" {
		l.nodesByName[n.Name] = append(l.nodesByName[n.Name], n)
	}
	if n.QualName != "" {
		l.nodesByQual[n.QualName] = n
	}
}

// AddEdge attaches one resolved overlay edge. The local-resolver
// pass at layer construction is expected to have rewritten any
// `unresolved::*` placeholders to point at concrete (overlay or
// base) node IDs before calling this; edges still carrying the
// placeholder are kept verbatim so OverlaidView.GetOutEdges still
// surfaces them — query tools can decide how to handle them, just
// like base's resolver-skipped edges.
func (l *OverlayLayer) AddEdge(e *Edge) {
	if e == nil {
		return
	}
	l.outEdges[e.From] = append(l.outEdges[e.From], e)
	l.inEdges[e.To] = append(l.inEdges[e.To], e)
}

// MarkRemoved tells the layer that a base node ID is hidden by the
// overlay even though the overlay didn't re-emit it (a symbol the
// user deleted from the buffer). FindNodesByName uses this to filter
// stale base hits.
func (l *OverlayLayer) MarkRemoved(baseName, baseID string) {
	if baseName == "" || baseID == "" {
		return
	}
	set, ok := l.nameRemoved[baseName]
	if !ok {
		set = make(map[string]bool)
		l.nameRemoved[baseName] = set
	}
	set[baseID] = true
}

// HasFile reports whether the overlay covers a particular graph path
// (either with replacement content or as a tombstone). The View uses
// this to decide whether to consult overlay or base for the path's
// reads.
func (l *OverlayLayer) HasFile(graphPath string) bool {
	if l == nil {
		return false
	}
	_, ok := l.entries[graphPath]
	return ok
}

// IsTombstone reports whether the overlay marks the path as deleted.
func (l *OverlayLayer) IsTombstone(graphPath string) bool {
	if l == nil {
		return false
	}
	e := l.entries[graphPath]
	return e != nil && e.Deleted
}

// FilePaths returns the sorted list of overlay-covered paths. Used
// by analyzers / the diff tool to enumerate the overlay's footprint.
func (l *OverlayLayer) FilePaths() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.entries))
	for p := range l.entries {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// HasNode reports whether the overlay layer carries a node with this
// ID. Used by the local-resolver pass in the mcp layer to drop base
// hits whose file is overlaid but whose specific ID wasn't kept by
// the overlay (i.e. the user deleted that symbol from the buffer).
func (l *OverlayLayer) HasNode(id string) bool {
	if l == nil {
		return false
	}
	_, ok := l.nodeByID[id]
	return ok
}

// NodesByName returns the overlay-introduced nodes with the given
// short name. Empty slice when none. Used by the local-resolver
// pass.
func (l *OverlayLayer) NodesByName(name string) []*Node {
	if l == nil {
		return nil
	}
	src := l.nodesByName[name]
	out := make([]*Node, len(src))
	copy(out, src)
	return out
}

// OutEdgesByFromAll returns a snapshot of the layer's outgoing-edge
// map keyed by source ID. The resolver pass iterates this to rewrite
// `unresolved::*` placeholders. The returned map shares its slices
// with the layer (resolver mutates Edge.To in place); the map keys
// are stable for the snapshot.
func (l *OverlayLayer) OutEdgesByFromAll() map[string][]*Edge {
	if l == nil {
		return nil
	}
	out := make(map[string][]*Edge, len(l.outEdges))
	for k, v := range l.outEdges {
		out[k] = v
	}
	return out
}

// RebuildInEdges rebuilds the reverse-index map after the local
// resolver pass mutates Edge.To in place. Cheap: O(#overlay edges).
func (l *OverlayLayer) RebuildInEdges() {
	if l == nil {
		return
	}
	l.inEdges = make(map[string][]*Edge, len(l.outEdges))
	for _, edges := range l.outEdges {
		for _, e := range edges {
			l.inEdges[e.To] = append(l.inEdges[e.To], e)
		}
	}
}

// nodesForFile returns the overlay nodes for a path (empty for
// tombstones). Internal — used by OverlaidView.
func (l *OverlayLayer) nodesForFile(graphPath string) []*Node {
	if l == nil {
		return nil
	}
	e := l.entries[graphPath]
	if e == nil || e.Deleted {
		return nil
	}
	out := make([]*Node, len(e.Nodes))
	copy(out, e.Nodes)
	return out
}

// OverlaidView composes an immutable base Reader with a per-session
// overlay layer. Every read path consults the layer first for paths
// the overlay covers; falls through to base otherwise. The base is
// never mutated; the layer is built once per request and discarded
// with the request. This means concurrent sessions — overlay-active
// or not — each see their own consistent view, and the file watcher's
// reindex passes (which mutate base) don't corrupt overlay queries.
type OverlaidView struct {
	base  Reader
	layer *OverlayLayer

	// statsOnce caches the (potentially expensive) Stats walk so
	// repeated calls within one request don't pay the AllNodes /
	// AllEdges cost twice.
	statsOnce sync.Once
	stats     GraphStats
}

// NewOverlaidView builds a view. If layer is nil the view is a pure
// pass-through and consumers pay no overlay overhead.
func NewOverlaidView(base Reader, layer *OverlayLayer) *OverlaidView {
	return &OverlaidView{base: base, layer: layer}
}

// Base exposes the underlying base reader. The diff tool reads
// against (view.Base()) and against (view) directly to compute the
// delta induced by the overlay.
func (v *OverlaidView) Base() Reader { return v.base }

// Layer exposes the per-session overlay layer (nil when none).
// Diagnostic / debug tools use it to introspect what the overlay
// covers.
func (v *OverlaidView) Layer() *OverlayLayer { return v.layer }

// IDFile returns the file path encoded in a Gortex node ID, or "" if
// the id isn't file-anchored. Gortex IDs follow the pattern
// `<filepath>::<symbol>[.member][#param:name]` so the file prefix is
// the substring before the first `::`. Module / package / virtual
// nodes use other prefixes that won't match an overlay path.
func IDFile(id string) string {
	if id == "" {
		return ""
	}
	if i := strings.Index(id, "::"); i > 0 {
		return id[:i]
	}
	return ""
}

// nodeBelongsToOverlay reports whether an ID's file is covered by
// the layer.
func (v *OverlaidView) nodeBelongsToOverlay(id string) bool {
	if v.layer == nil {
		return false
	}
	return v.layer.HasFile(IDFile(id))
}

// GetNode returns the overlay's version of a node when the ID
// belongs to an overlaid file, the base node otherwise. Returns nil
// when the symbol exists in base but was removed in the overlay
// (the per-file overlay node list didn't include it).
func (v *OverlaidView) GetNode(id string) *Node {
	if v.layer != nil {
		if v.nodeBelongsToOverlay(id) {
			return v.layer.nodeByID[id] // may be nil — overlay deleted it
		}
	}
	if v.base == nil {
		return nil
	}
	return v.base.GetNode(id)
}

// GetNodesByIDs returns the overlay-aware *Node for each input ID.
// Overlay-owned IDs short-circuit to the per-session layer (and may
// resolve to nil when the overlay deleted the node); the remainder
// fans out as a single batched lookup against the base store. Missing
// IDs are simply absent from the returned map.
func (v *OverlaidView) GetNodesByIDs(ids []string) map[string]*Node {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]*Node, len(ids))
	baseIDs := ids[:0:0] // fresh backing array — never aliases caller's slice
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := out[id]; dup {
			continue
		}
		if v.layer != nil && v.nodeBelongsToOverlay(id) {
			if n := v.layer.nodeByID[id]; n != nil {
				out[id] = n
			}
			// Overlay tombstone — ID is hidden, do not fall back to base.
			continue
		}
		// Track for the single base round-trip; reserve a slot in `out`
		// only after the batched lookup returns.
		baseIDs = append(baseIDs, id)
	}
	if len(baseIDs) > 0 && v.base != nil {
		for id, n := range v.base.GetNodesByIDs(baseIDs) {
			if n != nil {
				out[id] = n
			}
		}
	}
	return out
}

// GetNodeByQualName: overlay first, then base. Base hits are filtered
// to drop entries whose file is overlaid (the overlay's view wins).
func (v *OverlaidView) GetNodeByQualName(qualName string) *Node {
	if v.layer != nil {
		if n := v.layer.nodesByQual[qualName]; n != nil {
			return n
		}
	}
	if v.base == nil {
		return nil
	}
	n := v.base.GetNodeByQualName(qualName)
	if n != nil && v.layer != nil && v.layer.HasFile(IDFile(n.ID)) {
		// Base hit landed in an overlaid file but the overlay didn't
		// re-emit a node with this qualified name → it's gone.
		return nil
	}
	return n
}

// GetNodesByQualNames resolves each name through GetNodeByQualName so the
// overlay's layer-first / shadowed-file filtering applies — an inherited
// base batch would bypass the overlay. Per-name is fine: an interactive
// overlay's working set is small (the batch form exists for the
// cold-warmup scale on the base store, not here). Returns only hits.
func (v *OverlaidView) GetNodesByQualNames(qualNames []string) map[string]*Node {
	out := make(map[string]*Node, len(qualNames))
	for _, q := range qualNames {
		if q == "" {
			continue
		}
		if _, done := out[q]; done {
			continue
		}
		if n := v.GetNodeByQualName(q); n != nil {
			out[q] = n
		}
	}
	return out
}

// FindNodesByName merges base hits (filtered to drop nodes in
// overlaid files unless the overlay re-emitted them) with overlay
// hits. Order is overlay-first, then base — callers that picked
// "first match" semantics get the overlay version automatically.
func (v *OverlaidView) FindNodesByName(name string) []*Node {
	var out []*Node
	if v.layer != nil {
		out = append(out, v.layer.nodesByName[name]...)
	}
	if v.base == nil {
		return out
	}
	for _, n := range v.base.FindNodesByName(name) {
		if v.layer != nil {
			if v.layer.HasFile(IDFile(n.ID)) {
				// Overlaid file: base's node for this name is
				// always hidden. If the overlay re-emitted the same
				// ID it's already in `out` from the layer's
				// nodesByName prepend above; if the overlay deleted
				// the symbol it must not surface at all. Either way
				// we skip — no need to discriminate.
				continue
			}
			if v.layer.nameRemoved[name] != nil && v.layer.nameRemoved[name][n.ID] {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}

// FindNodesByNameContaining merges overlay-touched name hits with the
// base result, then re-applies the per-overlay-file masking the same
// way FindNodesByName does. Order is overlay-first, then base; the
// limit caps the merged total. Empty substr or both layers nil
// returns nil.
func (v *OverlaidView) FindNodesByNameContaining(substr string, limit int) []*Node {
	if substr == "" {
		return nil
	}
	needle := strings.ToLower(substr)
	var out []*Node
	// Overlay-side: walk the layer's nodesByName index — the same
	// bucket FindNodesByName reads from — and accept any name whose
	// lowercase form contains the needle.
	if v.layer != nil {
		for name, bucket := range v.layer.nodesByName {
			if strings.Contains(strings.ToLower(name), needle) {
				out = append(out, bucket...)
				if limit > 0 && len(out) >= limit {
					return out[:limit]
				}
			}
		}
	}
	if v.base == nil {
		return out
	}
	// Base-side: fetch with an inflated limit so overlay-mask drops
	// don't leave a short page. Then re-apply the same overlaid-file
	// + name-removed mask FindNodesByName uses.
	fetch := limit
	if fetch > 0 {
		fetch *= 2
	}
	for _, n := range v.base.FindNodesByNameContaining(substr, fetch) {
		if v.layer != nil {
			if v.layer.HasFile(IDFile(n.ID)) {
				continue
			}
			if v.layer.nameRemoved[n.Name] != nil && v.layer.nameRemoved[n.Name][n.ID] {
				continue
			}
		}
		out = append(out, n)
		if limit > 0 && len(out) >= limit {
			return out[:limit]
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// GetFileNodes: if the path is overlaid, return overlay's nodes
// (empty for tombstones). Otherwise pass through to base.
func (v *OverlaidView) GetFileNodes(filePath string) []*Node {
	if v.layer != nil && v.layer.HasFile(filePath) {
		return v.layer.nodesForFile(filePath)
	}
	if v.base == nil {
		return nil
	}
	return v.base.GetFileNodes(filePath)
}

// GetRepoNodes filters base's per-repo node list by dropping nodes
// whose file is overlaid (unless the overlay re-emitted them) and
// appending the overlay's nodes for any overlaid file inside the
// requested repo prefix.
func (v *OverlaidView) GetRepoNodes(repoPrefix string) []*Node {
	if v.base == nil {
		return nil
	}
	baseNodes := v.base.GetRepoNodes(repoPrefix)
	if v.layer == nil {
		return baseNodes
	}
	out := make([]*Node, 0, len(baseNodes))
	for _, n := range baseNodes {
		if v.layer.HasFile(IDFile(n.ID)) {
			// File is overlaid. Surface only if the overlay
			// re-emitted this exact ID; otherwise it's hidden.
			if v.layer.nodeByID[n.ID] == nil {
				continue
			}
		}
		out = append(out, n)
	}
	for _, path := range v.layer.FilePaths() {
		if !strings.HasPrefix(path, repoPrefix+"/") && path != repoPrefix {
			continue
		}
		out = append(out, v.layer.nodesForFile(path)...)
	}
	return out
}

// GetOutEdges: when the source node's file is overlaid, use the
// overlay's resolved out-edges. Otherwise return base's edges but
// drop any whose target points into an overlaid file at a node ID
// the overlay no longer carries (target deleted in buffer).
func (v *OverlaidView) GetOutEdges(nodeID string) []*Edge {
	if v.layer != nil && v.nodeBelongsToOverlay(nodeID) {
		src := v.layer.outEdges[nodeID]
		out := make([]*Edge, len(src))
		copy(out, src)
		return out
	}
	if v.base == nil {
		return nil
	}
	edges := v.base.GetOutEdges(nodeID)
	if v.layer == nil {
		return edges
	}
	out := edges[:0:0]
	for _, e := range edges {
		if v.layer.HasFile(IDFile(e.To)) {
			if v.layer.nodeByID[e.To] == nil {
				continue // target deleted in overlay
			}
		}
		out = append(out, e)
	}
	return out
}

// GetInEdges merges base's incoming edges (filtered to drop those
// originating in overlaid files, since those are replaced by overlay
// versions) with the overlay's in-edges for the same target.
func (v *OverlaidView) GetInEdges(nodeID string) []*Edge {
	if v.layer == nil {
		if v.base == nil {
			return nil
		}
		return v.base.GetInEdges(nodeID)
	}
	var out []*Edge
	if v.base != nil {
		for _, e := range v.base.GetInEdges(nodeID) {
			if v.layer.HasFile(IDFile(e.From)) {
				// Source is overlaid — the overlay's version of this
				// edge wins (or the overlay simply deleted the call).
				continue
			}
			if v.layer.HasFile(IDFile(e.To)) && v.layer.nodeByID[e.To] == nil {
				// Target was deleted by the overlay.
				continue
			}
			out = append(out, e)
		}
	}
	out = append(out, v.layer.inEdges[nodeID]...)
	return out
}

// GetOutEdgesByNodeIDs returns the overlay-aware outgoing-edge map for
// every input id. Overlay-owned ids short-circuit to the per-session
// layer; the remainder fans out as a single batched lookup against
// the base store. Output mirrors GetOutEdges's per-id semantics
// (target-side overlay deletions filtered out), but in one cgo
// round-trip per direction instead of N.
func (v *OverlaidView) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string][]*Edge, len(ids))
	baseIDs := ids[:0:0]
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if v.layer != nil && v.nodeBelongsToOverlay(id) {
			src := v.layer.outEdges[id]
			cp := make([]*Edge, len(src))
			copy(cp, src)
			out[id] = cp
			continue
		}
		baseIDs = append(baseIDs, id)
	}
	if len(baseIDs) > 0 && v.base != nil {
		base := v.base.GetOutEdgesByNodeIDs(baseIDs)
		for id, edges := range base {
			if v.layer == nil {
				out[id] = edges
				continue
			}
			filtered := edges[:0:0]
			for _, e := range edges {
				if v.layer.HasFile(IDFile(e.To)) {
					if v.layer.nodeByID[e.To] == nil {
						continue // target deleted in overlay
					}
				}
				filtered = append(filtered, e)
			}
			out[id] = filtered
		}
	}
	return out
}

// GetInEdgesByNodeIDs is the inbound sibling of GetOutEdgesByNodeIDs.
// Merges base in-edges (filtered to drop edges sourced in overlaid
// files) with overlay-introduced in-edges for each input id, all in a
// single batched base round-trip.
func (v *OverlaidView) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string][]*Edge, len(ids))
	seen := make(map[string]struct{}, len(ids))
	uniq := ids[:0:0]
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return out
	}
	if v.base != nil {
		base := v.base.GetInEdgesByNodeIDs(uniq)
		for _, id := range uniq {
			edges := base[id]
			if v.layer == nil {
				out[id] = edges
				continue
			}
			filtered := edges[:0:0]
			for _, e := range edges {
				if v.layer.HasFile(IDFile(e.From)) {
					continue // source is overlaid — overlay's version wins
				}
				if v.layer.HasFile(IDFile(e.To)) && v.layer.nodeByID[e.To] == nil {
					continue // target was deleted by overlay
				}
				filtered = append(filtered, e)
			}
			out[id] = filtered
		}
	}
	if v.layer != nil {
		for _, id := range uniq {
			if extras := v.layer.inEdges[id]; len(extras) > 0 {
				out[id] = append(out[id], extras...)
			}
		}
	}
	return out
}

// AllNodes returns base's nodes minus nodes in overlaid files, plus
// every node the overlay introduced. Bulk-read consumers (analyzers,
// search reindex, snapshot export) get an overlay-consistent view
// without paying any extra copy beyond the base snapshot's.
func (v *OverlaidView) AllNodes() []*Node {
	if v.base == nil {
		return nil
	}
	baseNodes := v.base.AllNodes()
	if v.layer == nil {
		return baseNodes
	}
	out := make([]*Node, 0, len(baseNodes))
	for _, n := range baseNodes {
		if v.layer.HasFile(IDFile(n.ID)) {
			if v.layer.nodeByID[n.ID] == nil {
				continue
			}
			// Else: overlay's version was kept under the same ID; the
			// layer's slice will include it below, so skip base's copy
			// to avoid duplicates.
			continue
		}
		out = append(out, n)
	}
	for _, n := range v.layer.nodeByID {
		out = append(out, n)
	}
	return out
}

// AllEdges returns base's edges minus those involving overlaid
// files, plus every overlay-introduced edge.
func (v *OverlaidView) AllEdges() []*Edge {
	if v.base == nil {
		return nil
	}
	baseEdges := v.base.AllEdges()
	if v.layer == nil {
		return baseEdges
	}
	out := make([]*Edge, 0, len(baseEdges))
	for _, e := range baseEdges {
		if v.layer.HasFile(IDFile(e.From)) || v.layer.HasFile(IDFile(e.To)) {
			continue
		}
		out = append(out, e)
	}
	for _, edges := range v.layer.outEdges {
		out = append(out, edges...)
	}
	return out
}

// NodeCount / EdgeCount — derived from base counters adjusted by the
// overlay delta. Cheap enough to recompute per call.
func (v *OverlaidView) NodeCount() int {
	if v.base == nil {
		return 0
	}
	if v.layer == nil {
		return v.base.NodeCount()
	}
	delta := 0
	for path, entry := range v.layer.entries {
		baseCount := len(v.base.GetFileNodes(path))
		if entry.Deleted {
			delta -= baseCount
			continue
		}
		delta += len(entry.Nodes) - baseCount
	}
	return v.base.NodeCount() + delta
}

func (v *OverlaidView) EdgeCount() int {
	if v.base == nil {
		return 0
	}
	if v.layer == nil {
		return v.base.EdgeCount()
	}
	return len(v.AllEdges())
}

// EdgeIdentityRevisions delegates to the base graph: provenance churn
// is a property of the persistent graph, and an overlay layer is a
// non-mutating per-session shadow that never upgrades edge provenance.
func (v *OverlaidView) EdgeIdentityRevisions() int {
	if v.base == nil {
		return 0
	}
	return v.base.EdgeIdentityRevisions()
}

// Stats is best-effort under overlay: we report base's stats (the
// analyzer-shaped GraphStats requires per-kind / per-language
// breakdowns that the overlay layer doesn't expose cheaply). Caching
// keeps repeated Stats() calls inside one request to a single base
// lookup.
func (v *OverlaidView) Stats() GraphStats {
	if v.base == nil {
		return GraphStats{}
	}
	v.statsOnce.Do(func() {
		v.stats = v.base.Stats()
	})
	return v.stats
}

// RepoStats — same conservatism as Stats; overlay deltas are
// excluded. The handful of tools that read RepoStats are bookkeeping
// rather than load-bearing, and the overlay-affected nodes are still
// reachable through the per-node read paths.
func (v *OverlaidView) RepoStats() map[string]GraphStats {
	if v.base == nil {
		return nil
	}
	return v.base.RepoStats()
}

// Compile-time assertion that *OverlaidView satisfies Reader.
var _ Reader = (*OverlaidView)(nil)
