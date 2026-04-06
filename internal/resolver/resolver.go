package resolver

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const unresolvedPrefix = "unresolved::"

// ResolveStats holds counts from a resolution pass.
type ResolveStats struct {
	Resolved   int `json:"resolved"`
	Unresolved int `json:"unresolved"`
	External   int `json:"external"`
}

// Resolver resolves unresolved edge targets to actual graph node IDs.
type Resolver struct {
	graph *graph.Graph
}

// New creates a Resolver for the given graph.
func New(g *graph.Graph) *Resolver {
	return &Resolver{graph: g}
}

// ResolveAll resolves all unresolved edges in the graph.
func (r *Resolver) ResolveAll() *ResolveStats {
	stats := &ResolveStats{}

	edges := r.graph.AllEdges()
	for _, e := range edges {
		if !strings.HasPrefix(e.To, unresolvedPrefix) {
			continue
		}
		r.resolveEdge(e, stats)
	}
	return stats
}

// ResolveFile resolves unresolved edges originating from a specific file.
func (r *Resolver) ResolveFile(filePath string) *ResolveStats {
	stats := &ResolveStats{}

	// Get all nodes in the file, then check their outgoing edges.
	nodes := r.graph.GetFileNodes(filePath)
	for _, n := range nodes {
		edges := r.graph.GetOutEdges(n.ID)
		for _, e := range edges {
			if !strings.HasPrefix(e.To, unresolvedPrefix) {
				continue
			}
			r.resolveEdge(e, stats)
		}
	}
	return stats
}

func (r *Resolver) resolveEdge(e *graph.Edge, stats *ResolveStats) {
	target := strings.TrimPrefix(e.To, unresolvedPrefix)

	switch {
	case strings.HasPrefix(target, "import::"):
		r.resolveImport(e, strings.TrimPrefix(target, "import::"), stats)
	case strings.HasPrefix(target, "*."):
		r.resolveMethodCall(e, strings.TrimPrefix(target, "*."), stats)
	default:
		r.resolveFunctionCall(e, target, stats)
	}
}

func (r *Resolver) resolveImport(e *graph.Edge, importPath string, stats *ResolveStats) {
	// Look for a package node with matching qualified name.
	node := r.graph.GetNodeByQualName(importPath)
	if node != nil {
		e.To = node.ID
		stats.Resolved++
		return
	}

	// Look for file nodes whose directory matches the import path suffix.
	// This handles in-repo packages.
	candidates := r.graph.AllNodes()
	for _, n := range candidates {
		if n.Kind != graph.KindFile {
			continue
		}
		dir := filepath.Dir(n.FilePath)
		if strings.HasSuffix(dir, lastPathComponent(importPath)) || dir == importPath {
			e.To = n.ID
			stats.Resolved++
			return
		}
	}

	// External/unresolvable import — create a stub target ID.
	e.To = "external::" + importPath
	stats.External++
}

func (r *Resolver) resolveFunctionCall(e *graph.Edge, funcName string, stats *ResolveStats) {
	candidates := r.graph.FindNodesByName(funcName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	// Prefer same-package (same directory) match.
	callerFile := e.FilePath
	callerDir := filepath.Dir(callerFile)

	for _, c := range candidates {
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// Fall back to first function match.
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

func (r *Resolver) resolveMethodCall(e *graph.Edge, methodName string, stats *ResolveStats) {
	candidates := r.graph.FindNodesByName(methodName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	// Prefer same-package match for methods.
	callerDir := filepath.Dir(e.FilePath)
	for _, c := range candidates {
		if c.Kind == graph.KindMethod && filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// Fall back to any method match.
	for _, c := range candidates {
		if c.Kind == graph.KindMethod {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

// InferImplements detects structural interface satisfaction by comparing
// method sets and adds EdgeImplements edges from types to interfaces.
// Returns the number of edges added.
func (r *Resolver) InferImplements() int {
	// Step 1: Collect all interfaces with their required method names.
	type ifaceInfo struct {
		id      string
		methods map[string]bool
	}
	var ifaces []ifaceInfo

	allNodes := r.graph.AllNodes()
	for _, n := range allNodes {
		if n.Kind != graph.KindInterface {
			continue
		}
		if n.Meta == nil {
			continue
		}
		raw, ok := n.Meta["methods"]
		if !ok {
			continue
		}
		// Meta["methods"] may be []string or []any (after JSON round-trip).
		methodSet := make(map[string]bool)
		switch v := raw.(type) {
		case []string:
			for _, m := range v {
				methodSet[m] = true
			}
		case []any:
			for _, m := range v {
				if s, ok := m.(string); ok {
					methodSet[s] = true
				}
			}
		}
		if len(methodSet) == 0 {
			continue
		}
		ifaces = append(ifaces, ifaceInfo{id: n.ID, methods: methodSet})
	}

	if len(ifaces) == 0 {
		return 0
	}

	// Step 2: Build map of type ID -> set of method names via EdgeMemberOf edges.
	typeMethods := make(map[string]map[string]bool)
	allEdges := r.graph.AllEdges()
	for _, e := range allEdges {
		if e.Kind != graph.EdgeMemberOf {
			continue
		}
		// EdgeMemberOf: From=method, To=type
		methodNode := r.graph.GetNode(e.From)
		if methodNode == nil || methodNode.Kind != graph.KindMethod {
			continue
		}
		typeID := e.To
		if typeMethods[typeID] == nil {
			typeMethods[typeID] = make(map[string]bool)
		}
		typeMethods[typeID][methodNode.Name] = true
	}

	// Step 3: For each type, check if its method set satisfies each interface.
	added := 0
	for typeID, methods := range typeMethods {
		typeNode := r.graph.GetNode(typeID)
		if typeNode == nil || (typeNode.Kind != graph.KindType && typeNode.Kind != graph.KindInterface) {
			continue
		}
		// Don't let a type implement itself.
		for _, iface := range ifaces {
			if iface.id == typeID {
				continue
			}
			// Check if all required methods are present.
			satisfies := true
			for m := range iface.methods {
				if !methods[m] {
					satisfies = false
					break
				}
			}
			if satisfies {
				r.graph.AddEdge(&graph.Edge{
					From:     typeID,
					To:       iface.id,
					Kind:     graph.EdgeImplements,
					FilePath: typeNode.FilePath,
					Line:     typeNode.StartLine,
				})
				added++
			}
		}
	}

	return added
}

func lastPathComponent(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return path
	}
	return parts[len(parts)-1]
}
