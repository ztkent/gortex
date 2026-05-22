package mcp

import (
	"sort"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/graph"
)

// This file owns enclosing-scope resolution shared across the search
// and AST tools: the per-file line->symbol index (fileSymbolIndex)
// and the enclosing-owner derivation (enclosingName). search_ast,
// search_text, search_symbols, and the analyze-* detectors all need
// to answer "which symbol contains this?" -- they share this code so
// the answer stays consistent.

// buildFileSymbolIndex returns one fileSymbolIndex per Target's graph
// path. Building all indexes up-front (instead of lazily on first
// match) is fine because the cost is one graph pass per file's
// symbol list, and the alternative — locking inside the worker pool
// hot path — is worse for parallel runs.
func (s *Server) buildFileSymbolIndex(targets []astquery.Target) map[string]*fileSymbolIndex {
	if s.graph == nil {
		return nil
	}
	wanted := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		wanted[t.GraphPath] = struct{}{}
	}
	out := make(map[string]*fileSymbolIndex, len(wanted))
	for _, n := range s.graph.AllNodes() {
		if _, ok := wanted[n.FilePath]; !ok {
			continue
		}
		// Functions, methods, closures are the meaningful
		// "enclosing scope" candidates. KindType (struct/class)
		// is included too so a match in a class-body declaration
		// still gets a symbol_id (e.g. a Java field that's
		// flagged by string-equality detection).
		switch n.Kind {
		case graph.KindFunction, graph.KindMethod, graph.KindClosure, graph.KindType, graph.KindInterface:
			idx := out[n.FilePath]
			if idx == nil {
				idx = &fileSymbolIndex{}
				out[n.FilePath] = idx
			}
			idx.add(n)
		}
	}
	for _, idx := range out {
		idx.finalise()
	}
	return out
}

// fileSymbolIndex is the per-file lookup used by the SymbolLookup
// closure. We keep symbols sorted by [StartLine, EndLine] descending
// width so `find` returns the deepest enclosing scope (a closure
// inside a method beats the method itself).
type fileSymbolIndex struct {
	syms []*graph.Node
}

func (i *fileSymbolIndex) add(n *graph.Node) { i.syms = append(i.syms, n) }

func (i *fileSymbolIndex) finalise() {
	sort.Slice(i.syms, func(a, b int) bool {
		if i.syms[a].StartLine != i.syms[b].StartLine {
			return i.syms[a].StartLine < i.syms[b].StartLine
		}
		// For nodes at the same start line, narrowest-first so
		// the deepest scope wins on ties.
		return (i.syms[a].EndLine - i.syms[a].StartLine) <
			(i.syms[b].EndLine - i.syms[b].StartLine)
	})
}

// find returns (symbol_id, name) for the smallest enclosing symbol
// whose [StartLine, EndLine] range covers `line`. Lines are 1-based;
// graph nodes store the same convention.
func (i *fileSymbolIndex) find(line int) (string, string) {
	if i == nil {
		return "", ""
	}
	var best *graph.Node
	bestSpan := int(^uint(0) >> 1)
	for _, n := range i.syms {
		if n.StartLine > line {
			break
		}
		if n.EndLine < line {
			continue
		}
		span := n.EndLine - n.StartLine
		if best == nil || span < bestSpan {
			best = n
			bestSpan = span
		}
	}
	if best == nil {
		return "", ""
	}
	return best.ID, best.Name
}

// enclosingName derives the enclosing owner of a node -- the symbol
// the node is declared *inside* -- and returns its (id, name).
//
//   - For a method, the owner is its receiver type, recovered from
//     the EdgeMemberOf edge, or failing that from the node ID prefix
//     (the ID convention is "<file>::<Owner>.<method>").
//   - For a field, enum member, closure, or nested function, the
//     owner is whatever EdgeMemberOf points at -- the struct, enum,
//     or enclosing function.
//   - For everything else (a top-level function, type, package-level
//     variable) there is no enclosing owner; both return values are
//     empty.
//
// A nil node or reader yields two empty strings.
func enclosingName(n *graph.Node, g graph.Reader) (id, name string) {
	if n == nil {
		return "", ""
	}
	switch n.Kind {
	case graph.KindMethod, graph.KindField, graph.KindEnumMember,
		graph.KindClosure:
		// These kinds are always declared inside an owner.
	case graph.KindFunction:
		// A function is enclosed only when it is nested (a function
		// literal assigned inside another function); the EdgeMemberOf
		// lookup below detects that. A top-level function has none.
	default:
		return "", ""
	}

	// Primary path: the EdgeMemberOf edge records the structural
	// owner directly.
	if g != nil {
		for _, e := range g.GetOutEdges(n.ID) {
			if e.Kind != graph.EdgeMemberOf {
				continue
			}
			if owner := g.GetNode(e.To); owner != nil {
				return owner.ID, owner.Name
			}
			// The edge target may not be resolvable to a node (an
			// unresolved owner); still surface the ID.
			return e.To, graph.EnclosingShortName(e.To)
		}
	}

	// Fallback: derive the owner from the ID convention. This
	// covers method / field / enum-member / closure even when no
	// EdgeMemberOf edge was materialised.
	if ownerID, ownerName := graph.EnclosingFromID(n.ID, n.Kind); ownerName != "" {
		return ownerID, ownerName
	}
	return "", ""
}

// buildFileSymbolIndexForPaths builds one fileSymbolIndex per file
// path in `paths`. It is the plain-path sibling of
// buildFileSymbolIndex (which keys off astquery.Target values) --
// search_text works from trigram match paths, not AST targets, and
// needs the same enclosing-scope lookup.
func (s *Server) buildFileSymbolIndexForPaths(paths map[string]struct{}) map[string]*fileSymbolIndex {
	if s.graph == nil || len(paths) == 0 {
		return nil
	}
	out := make(map[string]*fileSymbolIndex, len(paths))
	for _, n := range s.graph.AllNodes() {
		if _, ok := paths[n.FilePath]; !ok {
			continue
		}
		switch n.Kind {
		case graph.KindFunction, graph.KindMethod, graph.KindClosure,
			graph.KindType, graph.KindInterface:
			idx := out[n.FilePath]
			if idx == nil {
				idx = &fileSymbolIndex{}
				out[n.FilePath] = idx
			}
			idx.add(n)
		}
	}
	for _, idx := range out {
		idx.finalise()
	}
	return out
}
