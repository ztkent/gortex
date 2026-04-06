package analysis

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Community represents a discovered functional cluster in the codebase.
type Community struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Members  []string `json:"members"`   // node IDs
	Files    []string `json:"files"`     // unique file paths
	Size     int      `json:"size"`      // member count
	Cohesion float64  `json:"cohesion"`  // internal edge density (0-1)
}

// CommunityResult is the output of community detection.
type CommunityResult struct {
	Communities []Community `json:"communities"`
	NodeToComm  map[string]string `json:"node_to_community"` // nodeID → communityID
	Modularity  float64     `json:"modularity"`
}

// DetectCommunities runs Louvain community detection on the graph.
// It considers calls, references, imports, and member_of edges.
func DetectCommunities(g *graph.Graph) *CommunityResult {
	nodes := g.AllNodes()
	edges := g.AllEdges()

	// Filter to symbol nodes only (skip file and import nodes)
	symbolNodes := make(map[string]bool)
	for _, n := range nodes {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			symbolNodes[n.ID] = true
		}
	}

	// Build adjacency with weights for clustering-relevant edges
	type edgeKey struct{ a, b string }
	weights := make(map[edgeKey]float64)
	for _, e := range edges {
		if !symbolNodes[e.From] || !symbolNodes[e.To] {
			continue
		}
		w := edgeWeight(e.Kind)
		if w == 0 {
			continue
		}
		// Undirected: add both directions
		k1 := edgeKey{e.From, e.To}
		k2 := edgeKey{e.To, e.From}
		weights[k1] += w
		weights[k2] += w
	}

	// Build neighbor lists
	neighbors := make(map[string]map[string]float64)
	for k, w := range weights {
		if neighbors[k.a] == nil {
			neighbors[k.a] = make(map[string]float64)
		}
		neighbors[k.a][k.b] = w
	}

	// Total edge weight
	var totalWeight float64
	for _, w := range weights {
		totalWeight += w
	}
	totalWeight /= 2 // each edge counted twice

	if totalWeight == 0 {
		return &CommunityResult{NodeToComm: make(map[string]string)}
	}

	// Weighted degree per node
	degree := make(map[string]float64)
	for id := range symbolNodes {
		for _, w := range neighbors[id] {
			degree[id] += w
		}
	}

	// Louvain Phase 1: local moves
	comm := make(map[string]string)    // nodeID → communityID
	commNodes := make(map[string][]string) // communityID → nodeIDs
	for id := range symbolNodes {
		comm[id] = id
		commNodes[id] = []string{id}
	}

	// Sum of weights inside each community
	sigmaIn := make(map[string]float64)
	// Sum of all weights incident to nodes in community
	sigmaTot := make(map[string]float64)
	for id := range symbolNodes {
		sigmaTot[id] = degree[id]
	}

	improved := true
	for pass := 0; pass < 10 && improved; pass++ {
		improved = false
		for id := range symbolNodes {
			currentComm := comm[id]
			bestComm := currentComm
			bestGain := 0.0

			// Calculate weight to each neighbor community
			commWeights := make(map[string]float64)
			for neighbor, w := range neighbors[id] {
				commWeights[comm[neighbor]] += w
			}

			ki := degree[id]
			kiIn := commWeights[currentComm]

			// Remove node from current community for evaluation
			removeDelta := kiIn - sigmaTot[currentComm]*ki/(2*totalWeight)

			for c, wc := range commWeights {
				if c == currentComm {
					continue
				}
				gain := wc - sigmaTot[c]*ki/(2*totalWeight) - removeDelta
				if gain > bestGain {
					bestGain = gain
					bestComm = c
				}
			}

			if bestComm != currentComm {
				improved = true
				// Move node
				old := commNodes[currentComm]
				for i, nid := range old {
					if nid == id {
						commNodes[currentComm] = append(old[:i], old[i+1:]...)
						break
					}
				}
				sigmaIn[currentComm] -= 2 * kiIn
				sigmaTot[currentComm] -= ki

				comm[id] = bestComm
				commNodes[bestComm] = append(commNodes[bestComm], id)
				sigmaIn[bestComm] += 2 * commWeights[bestComm]
				sigmaTot[bestComm] += ki

				// Clean up empty communities
				if len(commNodes[currentComm]) == 0 {
					delete(commNodes, currentComm)
					delete(sigmaIn, currentComm)
					delete(sigmaTot, currentComm)
				}
			}
		}
	}

	// Build result
	nodeMap := make(map[string]*graph.Node)
	for _, n := range nodes {
		nodeMap[n.ID] = n
	}

	result := &CommunityResult{
		NodeToComm: make(map[string]string),
	}

	// Renumber communities
	commIndex := 0
	commRemap := make(map[string]string)
	for cid := range commNodes {
		if len(commNodes[cid]) < 2 {
			continue // skip singleton communities
		}
		newID := fmt.Sprintf("community-%d", commIndex)
		commRemap[cid] = newID
		commIndex++
	}

	for nodeID, cid := range comm {
		if newID, ok := commRemap[cid]; ok {
			result.NodeToComm[nodeID] = newID
		}
	}

	// Build Community objects
	for oldID, members := range commNodes {
		newID, ok := commRemap[oldID]
		if !ok {
			continue
		}

		fileSet := make(map[string]bool)
		for _, mid := range members {
			if n, ok := nodeMap[mid]; ok {
				fileSet[n.FilePath] = true
			}
		}

		files := make([]string, 0, len(fileSet))
		for f := range fileSet {
			files = append(files, f)
		}
		sort.Strings(files)

		label := inferCommunityLabel(members, nodeMap, files)
		cohesion := computeCohesion(members, neighbors)

		c := Community{
			ID:       newID,
			Label:    label,
			Members:  members,
			Files:    files,
			Size:     len(members),
			Cohesion: cohesion,
		}
		result.Communities = append(result.Communities, c)
	}

	// Sort by size descending
	sort.Slice(result.Communities, func(i, j int) bool {
		return result.Communities[i].Size > result.Communities[j].Size
	})

	// Compute modularity
	result.Modularity = computeModularity(comm, neighbors, degree, totalWeight)

	return result
}

func edgeWeight(kind graph.EdgeKind) float64 {
	switch kind {
	case graph.EdgeCalls:
		return 3.0
	case graph.EdgeMemberOf:
		return 2.0
	case graph.EdgeReferences:
		return 1.5
	case graph.EdgeImplements, graph.EdgeExtends:
		return 2.0
	case graph.EdgeImports:
		return 0.5
	case graph.EdgeInstantiates:
		return 1.0
	default:
		return 0
	}
}

func computeCohesion(members []string, neighbors map[string]map[string]float64) float64 {
	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		memberSet[m] = true
	}

	var internal, total float64
	for _, m := range members {
		for n, w := range neighbors[m] {
			total += w
			if memberSet[n] {
				internal += w
			}
		}
	}

	if total == 0 {
		return 0
	}
	return math.Round(internal/total*100) / 100
}

func computeModularity(comm map[string]string, neighbors map[string]map[string]float64, degree map[string]float64, totalWeight float64) float64 {
	if totalWeight == 0 {
		return 0
	}
	var q float64
	for i, ci := range comm {
		for j, w := range neighbors[i] {
			if comm[j] == ci {
				q += w - degree[i]*degree[j]/(2*totalWeight)
			}
		}
	}
	return math.Round(q/(2*totalWeight)*1000) / 1000
}

func inferCommunityLabel(members []string, nodeMap map[string]*graph.Node, files []string) string {
	// Try directory-based label
	if len(files) > 0 {
		dirCount := make(map[string]int)
		for _, f := range files {
			dir := filepath.Dir(f)
			// Use the most specific directory component
			parts := strings.Split(dir, "/")
			if len(parts) > 0 {
				last := parts[len(parts)-1]
				if last != "." && last != "" {
					dirCount[last]++
				}
			}
		}
		var bestDir string
		var bestCount int
		for d, c := range dirCount {
			if c > bestCount {
				bestCount = c
				bestDir = d
			}
		}
		if bestDir != "" && bestCount >= len(files)/2 {
			return bestDir
		}
	}

	// Try common name prefix
	prefixCount := make(map[string]int)
	for _, mid := range members {
		n := nodeMap[mid]
		if n == nil {
			continue
		}
		// Extract prefix: e.g., "HandleUser" → "Handle", "parseConfig" → "parse"
		name := n.Name
		for i := 1; i < len(name); i++ {
			if name[i] >= 'A' && name[i] <= 'Z' {
				prefix := strings.ToLower(name[:i])
				if len(prefix) >= 3 {
					prefixCount[prefix]++
				}
				break
			}
		}
	}
	var bestPrefix string
	var bestPrefixCount int
	for p, c := range prefixCount {
		if c > bestPrefixCount && c >= 3 {
			bestPrefixCount = c
			bestPrefix = p
		}
	}
	if bestPrefix != "" {
		return bestPrefix
	}

	// Fallback: use most common directory
	if len(files) > 0 {
		return filepath.Dir(files[0])
	}

	return fmt.Sprintf("cluster-%d", len(members))
}
