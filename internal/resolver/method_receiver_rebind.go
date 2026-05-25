package resolver

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// rebindGoMethodReceivers fixes Go EdgeMemberOf edges whose target is
// a phantom `<methodfile>::TypeName` ID — the artefact of the Go
// extractor building the receiver-type endpoint from the method's own
// file rather than the file the type is actually declared in. Methods
// spread across multiple files in the same package each emit a
// different `<file>::Type` target even though they all logically
// belong to the single type node defined elsewhere.
//
// Without this pass:
//   - ladybug materialises phantom Node rows to satisfy the
//     rel-table FK on every cross-file method-receiver edge;
//   - InferImplements builds a typeID → method-set map keyed on the
//     phantom IDs, so a type whose methods span N files appears as N
//     partial types each with a fraction of the real method set, and
//     interface satisfaction is under-detected;
//   - find_implementations / get_class_hierarchy / get_callers over
//     interface methods all return partial results for cross-file-
//     method types (which is most of any non-trivial Go codebase).
//
// Algorithm: index every Go KindType / KindInterface node by
// (filepath.Dir(file), name); walk EdgeMemberOf; for each Go method
// whose To doesn't resolve, look up (its file's dir, type name); if
// exactly one match, rewrite edge.To to the canonical type ID via
// ReindexEdges (one batched commit instead of per-edge round-trips).
//
// Scope: Go only — other languages (Java / TS / Python) group methods
// inside the class body in the same file, so the cross-file pattern
// doesn't arise. The method node's Language gates the rebind.
func (r *Resolver) rebindGoMethodReceivers() {
	type pkgKey struct{ pkg, name string }
	typesIdx := make(map[pkgKey]string)
	for _, kind := range []graph.NodeKind{graph.KindType, graph.KindInterface} {
		for n := range r.graph.NodesByKind(kind) {
			if n.Language != "go" || n.Name == "" || n.FilePath == "" {
				continue
			}
			k := pkgKey{filepath.Dir(n.FilePath), n.Name}
			if existing, ok := typesIdx[k]; ok && existing != n.ID {
				// Two distinct type nodes with the same name in the
				// same package directory shouldn't happen in valid Go,
				// but guard against it — leave the edge alone rather
				// than pick an arbitrary winner.
				typesIdx[k] = ""
				continue
			}
			typesIdx[k] = n.ID
		}
	}
	if len(typesIdx) == 0 {
		return
	}
	var batch []graph.EdgeReindex
	for e := range r.graph.EdgesByKind(graph.EdgeMemberOf) {
		method := r.graph.GetNode(e.From)
		if method == nil || method.Language != "go" || method.Kind != graph.KindMethod {
			continue
		}
		// Already resolves to a real type node — same-file methods
		// land here. Nothing to do.
		if n := r.graph.GetNode(e.To); n != nil && (n.Kind == graph.KindType || n.Kind == graph.KindInterface) {
			continue
		}
		// Parse `<methodfile>::<typename>`. The split is on the LAST
		// `::` so paths embedded in the ID (none in Go, but stay
		// defensive) can't trip us up.
		i := strings.LastIndex(e.To, "::")
		if i <= 0 {
			continue
		}
		file := e.To[:i]
		typeName := e.To[i+2:]
		if file == "" || typeName == "" {
			continue
		}
		canonicalID, ok := typesIdx[pkgKey{filepath.Dir(file), typeName}]
		if !ok || canonicalID == "" || canonicalID == e.To {
			continue
		}
		oldTo := e.To
		e.To = canonicalID
		batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(batch) > 0 {
		r.graph.ReindexEdges(batch)
	}
}
