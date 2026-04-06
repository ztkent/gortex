package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Process represents a discovered execution flow in the codebase.
type Process struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`       // human-readable name
	EntryPoint string   `json:"entry_point"` // node ID of the entry function
	Steps      []string `json:"steps"`       // ordered node IDs in the flow
	StepCount  int      `json:"step_count"`
	Files      []string `json:"files"`       // unique files touched
	Score      float64  `json:"score"`       // entry point confidence score
}

// ProcessResult is the output of process discovery.
type ProcessResult struct {
	Processes    []Process         `json:"processes"`
	NodeToProcs  map[string][]string `json:"node_to_processes"` // nodeID → process IDs
}

// DiscoverProcesses finds execution flows by identifying entry points and tracing forward.
func DiscoverProcesses(g *graph.Graph) *ProcessResult {
	nodes := g.AllNodes()
	edges := g.AllEdges()

	// Build call graph adjacency (forward only)
	callees := make(map[string][]string)  // who does this function call?
	callers := make(map[string][]string)  // who calls this function?

	for _, e := range edges {
		if e.Kind == graph.EdgeCalls {
			callees[e.From] = append(callees[e.From], e.To)
			callers[e.To] = append(callers[e.To], e.From)
		}
	}

	// Score each function/method as a potential entry point
	type scored struct {
		node  *graph.Node
		score float64
	}
	var candidates []scored

	nodeMap := make(map[string]*graph.Node)
	for _, n := range nodes {
		nodeMap[n.ID] = n
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}

		score := scoreEntryPoint(n, len(callees[n.ID]), len(callers[n.ID]))
		if score > 0.5 {
			candidates = append(candidates, scored{n, score})
		}
	}

	// Sort by score descending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	// Trace forward from each entry point to build processes
	result := &ProcessResult{
		NodeToProcs: make(map[string][]string),
	}

	seen := make(map[string]bool) // avoid duplicate processes
	for i, c := range candidates {
		if i >= 50 { // cap at 50 processes
			break
		}
		if seen[c.node.ID] {
			continue
		}

		steps := traceForward(c.node.ID, callees, 15) // max depth 15
		if len(steps) < 2 {
			continue // not interesting
		}
		seen[c.node.ID] = true

		fileSet := make(map[string]bool)
		for _, sid := range steps {
			if n, ok := nodeMap[sid]; ok {
				fileSet[n.FilePath] = true
			}
		}
		files := make([]string, 0, len(fileSet))
		for f := range fileSet {
			files = append(files, f)
		}
		sort.Strings(files)

		procID := fmt.Sprintf("process-%d", len(result.Processes))
		proc := Process{
			ID:         procID,
			Name:       inferProcessName(c.node),
			EntryPoint: c.node.ID,
			Steps:      steps,
			StepCount:  len(steps),
			Files:      files,
			Score:      c.score,
		}
		result.Processes = append(result.Processes, proc)

		for _, sid := range steps {
			result.NodeToProcs[sid] = append(result.NodeToProcs[sid], procID)
		}
	}

	return result
}

func scoreEntryPoint(n *graph.Node, calleeCount, callerCount int) float64 {
	if calleeCount == 0 {
		return 0 // leaf functions are not entry points
	}

	// Base score: ratio of outgoing to incoming calls
	base := float64(calleeCount) / (float64(callerCount) + 1.0)

	// Name pattern multiplier
	nameMult := namePatternMultiplier(n.Name, n.Language)

	// Export/visibility multiplier
	exportMult := 1.0
	if isExported(n.Name, n.Language) {
		exportMult = 1.5
	}

	// Low caller count bonus (true entry points have few callers)
	callerMult := 1.0
	if callerCount == 0 {
		callerMult = 2.0
	} else if callerCount <= 2 {
		callerMult = 1.3
	}

	return base * nameMult * exportMult * callerMult
}

func namePatternMultiplier(name, lang string) float64 {
	lower := strings.ToLower(name)

	// High-value entry point patterns
	entryPatterns := []string{
		"main", "init", "run", "start", "serve", "listen",
		"handle", "handler", "controller", "middleware",
		"route", "endpoint", "dispatch",
	}
	for _, p := range entryPatterns {
		if strings.HasPrefix(lower, p) || strings.HasSuffix(lower, p) {
			return 1.5
		}
	}

	// Go-specific
	if lang == "go" {
		if strings.HasPrefix(name, "New") || strings.HasPrefix(name, "Serve") {
			return 1.3
		}
		if strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark") {
			return 0.3
		}
	}

	// Utility patterns (deprioritize)
	utilPatterns := []string{
		"get", "set", "is", "has", "to", "from", "parse",
		"format", "validate", "helper", "util", "string",
	}
	for _, p := range utilPatterns {
		if strings.HasPrefix(lower, p) {
			return 0.5
		}
	}

	return 1.0
}

func isExported(name, lang string) bool {
	if lang == "go" {
		return len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
	}
	// For other languages, assume exported if not starting with underscore
	return !strings.HasPrefix(name, "_")
}

func traceForward(startID string, callees map[string][]string, maxDepth int) []string {
	var result []string
	visited := make(map[string]bool)

	var dfs func(id string, depth int)
	dfs = func(id string, depth int) {
		if visited[id] || depth > maxDepth {
			return
		}
		visited[id] = true
		result = append(result, id)

		for _, callee := range callees[id] {
			if !visited[callee] {
				dfs(callee, depth+1)
			}
		}
	}

	dfs(startID, 0)
	return result
}

func inferProcessName(n *graph.Node) string {
	name := n.Name
	lower := strings.ToLower(name)

	// Try to extract a descriptive name
	if lower == "main" {
		return "main execution"
	}
	if strings.HasPrefix(lower, "handle") {
		subject := strings.TrimPrefix(name, "Handle")
		subject = strings.TrimPrefix(subject, "handle")
		if subject != "" {
			return strings.ToLower(subject[:1]) + subject[1:] + " handling"
		}
	}
	if strings.HasPrefix(lower, "serve") {
		return name + " flow"
	}
	if strings.HasPrefix(name, "New") {
		return strings.TrimPrefix(name, "New") + " initialization"
	}
	if strings.HasPrefix(name, "Test") {
		return name
	}

	return name + " flow"
}
