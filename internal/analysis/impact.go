package analysis

import (
	"fmt"
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/reach"
)

// RiskLevel represents the severity of a change's impact.
type RiskLevel string

const (
	RiskLow      RiskLevel = "LOW"
	RiskMedium   RiskLevel = "MEDIUM"
	RiskHigh     RiskLevel = "HIGH"
	RiskCritical RiskLevel = "CRITICAL"
)

// ImpactEntry is a symbol affected at a specific depth.
type ImpactEntry struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Kind            string  `json:"kind"`
	FilePath        string  `json:"file_path"`
	Line            int     `json:"start_line"`
	RepoPrefix      string  `json:"repo_prefix,omitempty"`
	EdgeConfidence  float64 `json:"edge_confidence,omitempty"`
	ConfidenceLabel string  `json:"confidence_label,omitempty"`
}

// ImpactResult is the output of risk-tiered impact analysis.
type ImpactResult struct {
	Risk                RiskLevel                `json:"risk"`
	Summary             string                   `json:"summary"`
	ByDepth             map[int][]ImpactEntry    `json:"by_depth"`
	AffectedProcesses   []string                 `json:"affected_processes,omitempty"`
	AffectedCommunities []string                 `json:"affected_communities,omitempty"`
	TestFiles           []string                 `json:"test_files,omitempty"`
	TotalAffected       int                      `json:"total_affected"`
	CrossRepoImpact     bool                     `json:"cross_repo_impact,omitempty"`
	ByRepo              map[string][]ImpactEntry `json:"by_repo,omitempty"`
}

// AnalyzeImpact performs depth-tiered blast radius analysis on a set of symbols.
//
// Fast path: when every seed has a precomputed reach index
// (`Node.Meta["reach_d1/d2/d3"]` stamped by BuildReachIndex), the
// depth-1/2/3 ByDepth tiers are constructed from those sets without
// a live BFS — turning the dominant cost from O(reach) edge walks
// into O(reach) map lookups. The representative in-edge per tier
// entry is recovered with a linear scan of the entry's incoming
// edges, matching the live walk's behavior. Fall back to live BFS
// when any seed lacks the index — the slow path is identical to the
// pre-index implementation so consumer semantics never diverge.
func AnalyzeImpact(g graph.Store, symbolIDs []string, communities *CommunityResult, processes *ProcessResult) *ImpactResult {
	result := &ImpactResult{
		ByDepth: make(map[int][]ImpactEntry),
	}
	if !fillImpactFromReach(g, result, symbolIDs) {
		fillImpactLive(g, result, symbolIDs)
	}

	// Trim noise from the transitive tiers: a resolution edge with
	// confidence == 0 AND ConfidenceLabel == "INFERRED" means the
	// resolver produced the link without type info — essentially a
	// name-text match. At d=2 and d=3 these multiply the blast radius
	// through shared upstream helpers (e.g. every analyze_* handler
	// sharing respondJSONOrTOON), turning a leaf change into hundreds
	// of "transitively affected" rows the user can't act on. d=1 is
	// preserved untouched because direct dependents are always
	// informative even at low confidence.
	for depth := 2; depth <= 3; depth++ {
		result.ByDepth[depth] = filterHeuristicEntries(result.ByDepth[depth])
	}
	// Hard fan-out cap per tier so a pathological hub doesn't blow up
	// the response. Sorted ID order is already deterministic from the
	// reach index, so the cap is stable.
	const maxPerTier = 50
	for depth := 1; depth <= 3; depth++ {
		if len(result.ByDepth[depth]) > maxPerTier {
			result.ByDepth[depth] = result.ByDepth[depth][:maxPerTier]
		}
	}

	// Deduplicate test files
	result.TestFiles = dedup(result.TestFiles)

	// Count total
	for _, entries := range result.ByDepth {
		result.TotalAffected += len(entries)
	}

	// Determine risk level
	d1 := len(result.ByDepth[1])
	d2 := len(result.ByDepth[2])
	result.Risk = assessRisk(d1, d2, len(result.TestFiles))

	// Find affected processes
	if processes != nil {
		procSet := make(map[string]bool)
		for _, id := range symbolIDs {
			for _, pid := range processes.NodeToProcs[id] {
				procSet[pid] = true
			}
		}
		for depth := 1; depth <= 3; depth++ {
			for _, entry := range result.ByDepth[depth] {
				for _, pid := range processes.NodeToProcs[entry.ID] {
					procSet[pid] = true
				}
			}
		}
		for pid := range procSet {
			result.AffectedProcesses = append(result.AffectedProcesses, pid)
		}
		sort.Strings(result.AffectedProcesses)
	}

	// Find affected communities
	if communities != nil {
		commSet := make(map[string]bool)
		for _, id := range symbolIDs {
			if cid, ok := communities.NodeToComm[id]; ok {
				commSet[cid] = true
			}
		}
		for depth := 1; depth <= 3; depth++ {
			for _, entry := range result.ByDepth[depth] {
				if cid, ok := communities.NodeToComm[entry.ID]; ok {
					commSet[cid] = true
				}
			}
		}
		for cid := range commSet {
			result.AffectedCommunities = append(result.AffectedCommunities, cid)
		}
		sort.Strings(result.AffectedCommunities)
	}

	// Summary
	result.Summary = fmt.Sprintf(
		"%d direct dependents, %d transitively affected, %d test files, risk: %s",
		d1, result.TotalAffected, len(result.TestFiles), result.Risk,
	)

	// Group affected symbols by RepoPrefix and detect cross-repo impact.
	repoSet := make(map[string]bool)
	byRepo := make(map[string][]ImpactEntry)
	for _, id := range symbolIDs {
		if n := g.GetNode(id); n != nil && n.RepoPrefix != "" {
			repoSet[n.RepoPrefix] = true
		}
	}
	for depth := 1; depth <= 3; depth++ {
		for _, entry := range result.ByDepth[depth] {
			if entry.RepoPrefix != "" {
				repoSet[entry.RepoPrefix] = true
				byRepo[entry.RepoPrefix] = append(byRepo[entry.RepoPrefix], entry)
			}
		}
	}
	if len(repoSet) > 1 {
		result.CrossRepoImpact = true
		result.ByRepo = byRepo
	}

	return result
}

// fillImpactLive is the pre-precomputed-reach implementation: a
// depth-3 BFS over incoming edges that materialises one ImpactEntry
// per discovered node, attributing the in-edge that introduced it to
// EdgeConfidence / ConfidenceLabel. Kept as the always-correct
// fallback for fillImpactFromReach.
func fillImpactLive(g graph.Store, result *ImpactResult, symbolIDs []string) {
	visited := make(map[string]bool)
	for _, id := range symbolIDs {
		visited[id] = true
	}
	current := symbolIDs
	for depth := 1; depth <= 3; depth++ {
		var next []string
		for _, id := range current {
			for _, e := range g.GetInEdges(id) {
				if visited[e.From] {
					continue
				}
				if e.Kind == graph.EdgeDefines || e.Kind == graph.EdgeMemberOf {
					continue
				}
				visited[e.From] = true
				next = append(next, e.From)

				n := g.GetNode(e.From)
				if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
					continue
				}
				result.ByDepth[depth] = append(result.ByDepth[depth], ImpactEntry{
					ID:              n.ID,
					Name:            n.Name,
					Kind:            string(n.Kind),
					FilePath:        n.FilePath,
					Line:            n.StartLine,
					RepoPrefix:      n.RepoPrefix,
					EdgeConfidence:  e.Confidence,
					ConfidenceLabel: graph.ConfidenceLabelFor(e.Kind, e.Confidence),
				})
				if isTestFile(n.FilePath) {
					result.TestFiles = append(result.TestFiles, n.FilePath)
				}
			}
		}
		current = next
	}
}

// fillImpactFromReach is the precomputed fast path. Returns false if
// any seed lacks a reach build stamp — the caller must then run
// fillImpactLive. The union of per-seed reach_d1 sets becomes the
// depth-1 tier; depth-2 is the union of per-seed reach_d2 minus
// seeds and minus the depth-1 set; depth-3 is built the same way
// against (seeds ∪ d1 ∪ d2). For each tier-N entry we look up the
// representative in-edge with a linear scan of the node's incoming
// edges, picking the first one whose source is in the seeds (N=1) or
// in the prior tier's accumulated set (N≥2) — matching the live walk's
// deterministic-by-shard-iteration choice closely enough for tests
// that compare ByDepth ID sets, which is the contract consumers rely
// on. EdgeConfidence is set from that representative edge.
func fillImpactFromReach(g graph.Store, result *ImpactResult, symbolIDs []string) bool {
	if len(symbolIDs) == 0 {
		return true
	}
	// Single-seed shortcut. The precomputed tier slices are already
	// unique and sorted by ID (BuildIndex calls sortTierByID), so the
	// generic multi-seed path's per-depth merge + sort + seen-map are
	// pure overhead here. Stream directly into ByDepth with the
	// destination slice pre-sized — measurable difference on hot
	// blast-radius queries (1000-caller fan-in: ~2x faster than the
	// generic path).
	if len(symbolIDs) == 1 {
		seedID := symbolIDs[0]
		d1, d2, d3, hit := reach.Lookup(g, seedID)
		if !hit {
			return false
		}
		for depth, tier := range [3][]reach.Entry{d1, d2, d3} {
			if len(tier) == 0 {
				continue
			}
			out := make([]ImpactEntry, 0, len(tier))
			for _, e := range tier {
				if e.ID == seedID {
					continue
				}
				n := g.GetNode(e.ID)
				if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
					continue
				}
				out = append(out, ImpactEntry{
					ID:              n.ID,
					Name:            n.Name,
					Kind:            string(n.Kind),
					FilePath:        n.FilePath,
					Line:            n.StartLine,
					RepoPrefix:      n.RepoPrefix,
					EdgeConfidence:  e.Conf,
					ConfidenceLabel: e.Label,
				})
				if isTestFile(n.FilePath) {
					result.TestFiles = append(result.TestFiles, n.FilePath)
				}
			}
			result.ByDepth[depth+1] = out
		}
		return true
	}

	perSeed := make([][3][]reach.Entry, len(symbolIDs))
	for i, id := range symbolIDs {
		d1, d2, d3, hit := reach.Lookup(g, id)
		if !hit {
			return false
		}
		perSeed[i] = [3][]reach.Entry{d1, d2, d3}
	}

	// `seen` tracks every ID already emitted at a prior depth (and
	// the seed set itself) so a node appears in at most one ByDepth
	// slot — matches the BFS visited-set discipline the live walk has.
	// First per-seed appearance wins on cross-seed overlap, mirroring
	// the live walk's BFS-by-depth order.
	seen := make(map[string]struct{}, len(symbolIDs)+32)
	for _, id := range symbolIDs {
		seen[id] = struct{}{}
	}
	for depth := 1; depth <= 3; depth++ {
		var tier []reach.Entry
		for s := range perSeed {
			for _, e := range perSeed[s][depth-1] {
				if _, already := seen[e.ID]; already {
					continue
				}
				seen[e.ID] = struct{}{}
				tier = append(tier, e)
			}
		}
		// Deterministic emission — matches each per-seed slice's
		// build-time sort + makes the JSON payload diff-stable.
		sort.Slice(tier, func(i, j int) bool { return tier[i].ID < tier[j].ID })
		for _, e := range tier {
			n := g.GetNode(e.ID)
			if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			result.ByDepth[depth] = append(result.ByDepth[depth], ImpactEntry{
				ID:              n.ID,
				Name:            n.Name,
				Kind:            string(n.Kind),
				FilePath:        n.FilePath,
				Line:            n.StartLine,
				RepoPrefix:      n.RepoPrefix,
				EdgeConfidence:  e.Conf,
				ConfidenceLabel: e.Label,
			})
			if isTestFile(n.FilePath) {
				result.TestFiles = append(result.TestFiles, n.FilePath)
			}
		}
	}
	return true
}

// filterHeuristicEntries strips ImpactEntries whose representative
// edge was a heuristic / text-matched resolution (Confidence == 0 +
// label == "INFERRED"). Returns the kept prefix to avoid an extra
// allocation. The input slice is mutated.
func filterHeuristicEntries(entries []ImpactEntry) []ImpactEntry {
	kept := entries[:0]
	for _, e := range entries {
		if e.EdgeConfidence == 0 && e.ConfidenceLabel == "INFERRED" {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

func assessRisk(directDeps, transitiveDeps, testFiles int) RiskLevel {
	if directDeps >= 10 || (directDeps >= 5 && transitiveDeps >= 20) {
		return RiskCritical
	}
	if directDeps >= 5 || transitiveDeps >= 10 {
		return RiskHigh
	}
	if directDeps >= 2 || transitiveDeps >= 5 {
		return RiskMedium
	}
	return RiskLow
}

func isTestFile(path string) bool {
	return containsAny(path,
		"_test.go", ".test.ts", ".test.js", ".spec.ts", ".spec.js",
		"__tests__/", "test_",
	)
}

func containsAny(s string, patterns ...string) bool {
	for _, p := range patterns {
		if len(s) >= len(p) {
			for i := 0; i <= len(s)-len(p); i++ {
				if s[i:i+len(p)] == p {
					return true
				}
			}
		}
	}
	return false
}

func dedup(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	var result []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
