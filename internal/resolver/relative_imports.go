package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// resolveRelativeImports rewrites Python and Dart relative-import edges
// onto the internal `KindFile` node they actually reference. The Go
// resolver's resolveImport / dep-module bridge target language-agnostic
// directory paths, which never line up with Python file stems or Dart
// `..`-walking URIs; without this pass, every relative import landed as
// an `external::*` stub and the subsequent module-attribution sweep
// either left them alone (Dart) or mis-attributed them to a phantom
// pypi package called after the first path segment (Python).
//
// Runs serially after the main resolve loop and BEFORE
// attributeNonGoModuleImports so that any edge resolved to an internal
// file no longer participates in pypi/pub attribution. Edges whose
// target file is not in the graph stay as `external::*` so the
// module-attribution pass can decide what to do with them.
func (r *Resolver) resolveRelativeImports() {
	fileLang := r.collectFileLanguages()
	var reindexBatch []graph.EdgeReindex
	// EdgesByKind pushes the "kind = imports" filter into the store;
	// disk backends only enumerate import edges instead of every
	// edge in the graph.
	for e := range r.graph.EdgesByKind(graph.EdgeImports) {
		lang, ok := fileLang[e.From]
		if !ok {
			continue
		}
		var path string
		var resolved string
		switch {
		case strings.HasPrefix(e.To, "unresolved::pyrel::"):
			// Python parser-emitted relative-import placeholder.
			// Always resolvable via internal-file lookup.
			path = strings.TrimPrefix(e.To, "unresolved::pyrel::")
			if lang == "python" {
				resolved = resolvePythonRelativeImport(r.graph, path)
			}
		case strings.HasPrefix(e.To, "external::"):
			// Fallthrough path for Dart relative URIs the main
			// resolveImport sweep landed at `external::*`, plus a
			// safety net for any Python relative stem that arrived
			// here without the `pyrel::` marker.
			path = strings.TrimPrefix(e.To, "external::")
			switch lang {
			case "python":
				resolved = resolvePythonRelativeImport(r.graph, path)
			case "dart":
				resolved = resolveDartRelativeImport(r.graph, e.From, path)
			}
		default:
			continue
		}
		if resolved == "" {
			// pyrel:: edges that don't find an internal target are
			// downgraded to `external::` so the module-attribution
			// pass + audits don't see the internal marker prefix.
			if strings.HasPrefix(e.To, "unresolved::pyrel::") {
				oldTo := e.To
				e.To = "external::" + path
				reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			}
			continue
		}
		oldTo := e.To
		e.To = resolved
		e.Origin = graph.OriginASTResolved
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindexBatch) > 0 {
		r.graph.ReindexEdges(reindexBatch)
	}
}

// resolvePythonRelativeImport maps a project-rooted Python file-path
// stem ("app/util", "pkg/sub") to the matching `KindFile` node ID.
// Tries `<stem>.py` first, then `<stem>/__init__.py` (package). Returns
// "" if no candidate exists in the graph or if `stem` doesn't look like
// a relative-import stem (no slash separator — those are absolute
// module references handled by attributeNonGoModuleImports).
func resolvePythonRelativeImport(g graph.Store, stem string) string {
	if !strings.Contains(stem, "/") {
		return ""
	}
	for _, cand := range []string{stem + ".py", stem + "/__init__.py"} {
		if n := g.GetNode(cand); n != nil && n.Kind == graph.KindFile {
			return n.ID
		}
	}
	return ""
}

// resolveDartRelativeImport joins a relative Dart import URI against
// the importing file's directory and returns the matching `KindFile`
// node ID. Paths starting with `dart:` or `package:` are caller-
// validated to belong to the module-attribution pass and are skipped
// here. Returns "" when the resolved path escapes the repo root or
// when the target file is not in the graph.
func resolveDartRelativeImport(g graph.Store, importingFile, uri string) string {
	if uri == "" || strings.HasPrefix(uri, "dart:") || strings.HasPrefix(uri, "package:") {
		return ""
	}
	dir := ""
	if i := strings.LastIndex(importingFile, "/"); i >= 0 {
		dir = importingFile[:i]
	}
	target := joinRelativePath(dir, uri)
	if target == "" {
		return ""
	}
	if n := g.GetNode(target); n != nil && n.Kind == graph.KindFile {
		return n.ID
	}
	return ""
}

// joinRelativePath joins a relative URI onto a directory and collapses
// `.`/`..` segments. Returns "" when the path walks above the repo root
// (which we never want to silently silently fall through to an
// arbitrary file).
func joinRelativePath(dir, rel string) string {
	var parts []string
	if dir != "" {
		parts = strings.Split(dir, "/")
	}
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "", ".":
			// noop
		case "..":
			if len(parts) == 0 {
				return ""
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}
