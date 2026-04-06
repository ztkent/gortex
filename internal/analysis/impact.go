package analysis

import (
	"fmt"
	"sort"

	"github.com/zzet/gortex/internal/graph"
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
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
	Line     int    `json:"start_line"`
}

// ImpactResult is the output of risk-tiered impact analysis.
type ImpactResult struct {
	Risk               RiskLevel              `json:"risk"`
	Summary            string                 `json:"summary"`
	ByDepth            map[int][]ImpactEntry  `json:"by_depth"`
	AffectedProcesses  []string               `json:"affected_processes,omitempty"`
	AffectedCommunities []string              `json:"affected_communities,omitempty"`
	TestFiles          []string               `json:"test_files,omitempty"`
	TotalAffected      int                    `json:"total_affected"`
}

// AnalyzeImpact performs depth-tiered blast radius analysis on a set of symbols.
func AnalyzeImpact(g *graph.Graph, symbolIDs []string, communities *CommunityResult, processes *ProcessResult) *ImpactResult {
	result := &ImpactResult{
		ByDepth: make(map[int][]ImpactEntry),
	}

	visited := make(map[string]bool)
	for _, id := range symbolIDs {
		visited[id] = true
	}

	// BFS with depth tracking
	current := symbolIDs
	for depth := 1; depth <= 3; depth++ {
		var next []string
		for _, id := range current {
			// Get all incoming edges (things that depend on this symbol)
			inEdges := g.GetInEdges(id)
			for _, e := range inEdges {
				if visited[e.From] {
					continue
				}
				if e.Kind == graph.EdgeDefines || e.Kind == graph.EdgeMemberOf {
					continue // skip structural edges
				}
				visited[e.From] = true
				next = append(next, e.From)

				n := g.GetNode(e.From)
				if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
					continue
				}

				result.ByDepth[depth] = append(result.ByDepth[depth], ImpactEntry{
					ID:       n.ID,
					Name:     n.Name,
					Kind:     string(n.Kind),
					FilePath: n.FilePath,
					Line:     n.StartLine,
				})

				if isTestFile(n.FilePath) {
					result.TestFiles = append(result.TestFiles, n.FilePath)
				}
			}
		}
		current = next
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

	return result
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
