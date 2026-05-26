package resolver

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// attributeGoExternalCalls materialises a KindFunction node for every
// unique `stdlib::<importPath>::<symbol>` / `dep::<importPath>::<symbol>`
// / `external::<importPath>::<symbol>` edge target, plus a KindModule
// parent for each owning import path. Without this pass the targets
// are stubs in storage backends that enforce rel-table FK (Ladybug)
// and invisible nodes in the in-memory backend, so a query like
// `find_usages(stdlib::encoding/json::Marshal)`
// can't surface "every function in this codebase that calls
// json.Marshal" — the destination doesn't exist as a graph node.
//
// Mirrors the Python / Dart attributeNonGoModuleImports pass for Go.
// Runs after resolveExtern (which classifies extern targets into the
// three prefix buckets) so we materialise the post-classification
// state rather than the pre-classification `unresolved::extern::*`
// shape.
//
// ID conventions:
//   - Module node:    `module::go:<importPath>` — shared across every
//     repo that imports the same path. Carries
//     Meta["ecosystem"]="go" and Meta["import_path"]=<path>.
//     Meta["role"]="stdlib" for stdlib paths.
//   - Symbol node:    the original `stdlib::*` / `dep::*` /
//     `external::*` ID stays the symbol's ID so existing edges land
//     on it without rewriting. Carries Meta["external"]=true and
//     Meta["module_path"]=<importPath>.
//   - EdgeMemberOf:   symbol → module so `get_callers` on the module
//     surfaces every symbol used from that package.
//
// All AddNode / AddEdge calls are idempotent on ID, so a second run
// of this pass (incremental ResolveFile re-invocation) is a no-op.
func (r *Resolver) attributeGoExternalCalls() {
	// Scan every edge whose target sits in one of the three external
	// prefixes. Collect unique (repoPrefix, prefix, importPath, symbol)
	// tuples so we materialise each one once even when many edges
	// reference the same target. repoPrefix is included because
	// stdlib stubs are per-repo (see internal/graph/stub.go) — two
	// repos on different Go SDK versions emit semantically distinct
	// `<repoA>::stdlib::fmt::Errorf` and `<repoB>::stdlib::fmt::Errorf`
	// stubs that MUST round-trip through this attribution pass as
	// distinct nodes, not collide into one.
	type extKey struct {
		repoPrefix, prefix, importPath, symbol string
	}
	seen := map[extKey]struct{}{}
	depEdgesScan := func(kind graph.EdgeKind) {
		for e := range r.graph.EdgesByKind(kind) {
			if e.To == "" {
				continue
			}
			prefix, importPath, symbol := splitGoExternalTarget(e.To)
			if prefix == "" {
				continue
			}
			seen[extKey{graph.StubRepoPrefix(e.To), prefix, importPath, symbol}] = struct{}{}
		}
	}
	// Same edge-kind set as attributeGoBuiltins — anywhere an
	// extern-prefixed target can show up.
	for _, k := range []graph.EdgeKind{
		graph.EdgeCalls,
		graph.EdgeReferences,
		graph.EdgeReads,
		graph.EdgeArgOf,
		graph.EdgeValueFlow,
		graph.EdgeReturnsTo,
		graph.EdgeTypedAs,
		graph.EdgeReturns,
		graph.EdgeInstantiates,
		graph.EdgeCaptures,
		graph.EdgeThrows,
	} {
		depEdgesScan(k)
	}
	if len(seen) == 0 {
		return
	}

	// Materialise the parent KindModule for each unique import path,
	// then the per-symbol KindFunction. Module-side dedupe is via
	// the `modules` map; the per-symbol nodes are unique by (prefix,
	// path, symbol) by construction.
	// Module IDs are also per-repo now — a module node carries the
	// same SDK-version sensitivity its symbols do. Key includes the
	// repo prefix so two repos importing the same path get distinct
	// module nodes.
	type modKey struct{ repoPrefix, importPath string }
	modules := map[modKey]string{}
	for k := range seen {
		modKey := modKey{repoPrefix: k.repoPrefix, importPath: k.importPath}
		moduleID, ok := modules[modKey]
		if !ok {
			moduleID = graph.StubID(k.repoPrefix, graph.StubKindModule, "go", k.importPath)
			modules[modKey] = moduleID
			role := "external"
			switch k.prefix {
			case "stdlib::":
				role = "stdlib"
			case "dep::":
				role = "dep"
			}
			r.graph.AddNode(&graph.Node{
				ID:       moduleID,
				Kind:     graph.KindModule,
				Name:     lastImportSegment(k.importPath),
				Language: "go",
				Meta: map[string]any{
					"ecosystem":   "go",
					"role":        role,
					"import_path": k.importPath,
				},
			})
		}
		var symbolID string
		switch k.prefix {
		case "stdlib::":
			symbolID = graph.StubID(k.repoPrefix, graph.StubKindStdlib, k.importPath, k.symbol)
		default:
			// dep:: / external:: keep their legacy unprefixed form for
			// now — they aren't covered by the stub-prefix migration
			// (different module paths already provide repo-level
			// distinction; same version pinning is enforced by go.mod
			// per-repo).
			symbolID = k.prefix + k.importPath + "::" + k.symbol
		}
		r.graph.AddNode(&graph.Node{
			ID:       symbolID,
			Kind:     graph.KindFunction,
			Name:     k.symbol,
			Language: "go",
			Meta: map[string]any{
				"external":    true,
				"module_path": k.importPath,
				"module_role": map[string]string{
					"stdlib::":   "stdlib",
					"dep::":      "dep",
					"external::": "external",
				}[k.prefix],
			},
		})
		// EdgeMemberOf: symbol → module. AddEdge is idempotent on the
		// edge-key tuple so a re-run doesn't duplicate.
		r.graph.AddEdge(&graph.Edge{
			From:   symbolID,
			To:     moduleID,
			Kind:   graph.EdgeMemberOf,
			Origin: graph.OriginASTResolved,
		})
	}
}

// splitGoExternalTarget recognises the three external-target prefixes
// the resolver emits after resolveExtern. Returns the prefix
// (`stdlib::` / `dep::` / `external::`), the import path, and the
// symbol name. Returns ("", "", "") for any other shape so the pass
// can skip it cleanly.
//
// The stdlib case is matched via graph.IsStdlibStub so both the
// legacy `stdlib::fmt::Errorf` shape and the per-repo-prefixed
// `<repo>::stdlib::fmt::Errorf` shape (see internal/graph/stub.go)
// route the same way. The returned bucket label stays `stdlib::` for
// downstream `k.prefix == "stdlib::"` comparisons.
func splitGoExternalTarget(target string) (prefix, importPath, symbol string) {
	var body string
	switch {
	case graph.IsStdlibStub(target):
		prefix = "stdlib::"
		body = graph.StubRest(target)
	case strings.HasPrefix(target, "dep::"):
		prefix = "dep::"
		body = strings.TrimPrefix(target, prefix)
	case strings.HasPrefix(target, "external::"):
		prefix = "external::"
		body = strings.TrimPrefix(target, prefix)
	default:
		return "", "", ""
	}
	// The body shape produced by resolveExtern is
	// `<importPath>::<symbol>`. Split on the LAST `::` because import
	// paths can include slashes but not `::`, so the rightmost
	// separator is always between path and symbol.
	sep := strings.LastIndex(body, "::")
	if sep < 0 {
		// `external::os` style (just the package, no symbol —
		// the resolveImport path). Treat the whole body as the path
		// and leave symbol empty so we still materialise the module
		// node but skip the symbol.
		return prefix, body, ""
	}
	return prefix, body[:sep], body[sep+2:]
}

// lastImportSegment returns the rightmost path component, used as
// the human-readable Name on the KindModule node. For
// `github.com/stretchr/testify/assert` the segment is `assert`; for
// `encoding/json` it's `json`; for `fmt` it's `fmt`.
func lastImportSegment(importPath string) string {
	if importPath == "" {
		return ""
	}
	return path.Base(importPath)
}
