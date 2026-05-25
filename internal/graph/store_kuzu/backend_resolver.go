package store_kuzu

import "fmt"

// ResolveSameFile pushes the same-source-file resolution pass into
// the Kuzu engine. For every `unresolved::Name` edge, look for a
// Node with that name whose file_path matches the caller's
// file_path — if there's exactly one such candidate, rewrite the
// edge to point at it. Same-file calls are unambiguous in every
// language we index, so the match precision is high.
//
// One Cypher statement replaces what would otherwise be ~thousands
// of per-edge GetNode / FindNodesByName round-trips.
func (s *Store) ResolveSameFile() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Two-pass to keep `target` typed as Node through the CREATE.
	const q = `
MATCH (caller:Node)-[e:Edge]->(stub:Node)
WHERE stub.id STARTS WITH 'unresolved::' AND caller.file_path <> ''
WITH e, caller, stub, substring(stub.id, 13, size(stub.id) - 12) AS name
OPTIONAL MATCH (cnd:Node {name: name})
WHERE cnd.file_path = caller.file_path AND cnd.id <> stub.id
WITH e, caller, stub, name, count(cnd) AS cnt
WHERE cnt = 1
MATCH (target:Node {name: name})
WHERE target.file_path = caller.file_path AND target.id <> stub.id
DELETE e
CREATE (caller)-[newE:Edge {
    kind: e.kind,
    file_path: e.file_path,
    line: e.line,
    confidence: e.confidence,
    confidence_label: e.confidence_label,
    origin: 'ast_resolved',
    tier: 'ast_resolved',
    cross_repo: e.cross_repo,
    meta: e.meta
}]->(target)
RETURN count(newE) AS resolved`
	return s.runResolverQueryLocked(q, "ResolveSameFile")
}

// ResolveSamePackage drains the "same Go-style package" case: edges
// where the caller and a unique candidate share the same directory
// portion of file_path AND the same repo_prefix. Kuzu has no
// regex_extract, so directory is derived by splitting on "/" and
// reassembling all but the last segment with list_to_string.
func (s *Store) ResolveSamePackage() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Kuzu has neither regex_extract nor split — but it does have
	// regexp_replace, which we abuse to extract the directory by
	// stripping everything from the last "/" onward. Files with no
	// "/" come back unchanged so we add an explicit guard with
	// CONTAINS to skip top-level files.
	const q = `
MATCH (caller:Node)-[e:Edge]->(stub:Node)
WHERE stub.id STARTS WITH 'unresolved::'
  AND caller.file_path <> ''
  AND caller.file_path CONTAINS '/'
WITH e, caller, stub, substring(stub.id, 13, size(stub.id) - 12) AS name,
     regexp_replace(caller.file_path, '/[^/]+$', '') AS caller_dir
OPTIONAL MATCH (cnd:Node {name: name})
WHERE cnd.repo_prefix = caller.repo_prefix
  AND cnd.id <> stub.id
  AND cnd.file_path <> caller.file_path
  AND cnd.file_path CONTAINS '/'
  AND regexp_replace(cnd.file_path, '/[^/]+$', '') = caller_dir
WITH e, caller, stub, name, caller_dir, count(cnd) AS cnt
WHERE cnt = 1
MATCH (target:Node {name: name})
WHERE target.repo_prefix = caller.repo_prefix
  AND target.id <> stub.id
  AND target.file_path <> caller.file_path
  AND target.file_path CONTAINS '/'
  AND regexp_replace(target.file_path, '/[^/]+$', '') = caller_dir
DELETE e
CREATE (caller)-[newE:Edge {
    kind: e.kind,
    file_path: e.file_path,
    line: e.line,
    confidence: e.confidence,
    confidence_label: e.confidence_label,
    origin: 'ast_resolved',
    tier: 'ast_resolved',
    cross_repo: e.cross_repo,
    meta: e.meta
}]->(target)
RETURN count(newE) AS resolved`
	return s.runResolverQueryLocked(q, "ResolveSamePackage")
}
// ResolveImportAware drains the "imported-symbol" case: caller's
// file_path is the FROM of an EdgeImports to an imported file, and
// a Node with the unresolved name lives in that imported file.
// When exactly one such candidate exists across all the caller's
// imports, rewrite the edge to point at it.
//
// This is the highest-coverage rule for Python / JS / Rust-style
// `import X` semantics where the target is in a different file but
// reachable via the import set. Joins against the existing
// EdgeImports adjacency (which the parser populates).
func (s *Store) ResolveImportAware() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	const q = `
MATCH (caller:Node)-[e:Edge]->(stub:Node)
WHERE stub.id STARTS WITH 'unresolved::' AND caller.file_path <> ''
WITH e, caller, stub, substring(stub.id, 13, size(stub.id) - 12) AS name
MATCH (callerFile:Node {file_path: caller.file_path})
WHERE callerFile.kind = 'file'
MATCH (callerFile)-[imp:Edge {kind: 'imports'}]->(importedFile:Node)
WHERE importedFile.kind = 'file'
  AND NOT (importedFile.id STARTS WITH 'external::')
  AND NOT (importedFile.id STARTS WITH 'unresolved::')
OPTIONAL MATCH (cnd:Node {name: name})
WHERE cnd.file_path = importedFile.file_path
  AND cnd.id <> stub.id
WITH e, caller, stub, name, count(DISTINCT cnd) AS cnt
WHERE cnt = 1
MATCH (callerFile2:Node {file_path: caller.file_path})
WHERE callerFile2.kind = 'file'
MATCH (callerFile2)-[:Edge {kind: 'imports'}]->(importedFile2:Node)
MATCH (target:Node {name: name})
WHERE target.file_path = importedFile2.file_path
  AND target.id <> stub.id
DELETE e
CREATE (caller)-[newE:Edge {
    kind: e.kind,
    file_path: e.file_path,
    line: e.line,
    confidence: e.confidence,
    confidence_label: e.confidence_label,
    origin: 'ast_resolved',
    tier: 'ast_resolved',
    cross_repo: e.cross_repo,
    meta: e.meta
}]->(target)
RETURN count(newE) AS resolved`
	return s.runResolverQueryLocked(q, "ResolveImportAware")
}
// ResolveRelativeImports drains `unresolved::pyrel::<stem>` edges
// (Python's relative-import placeholder emitted by the parser) by
// rewriting them to either `<stem>.py` or `<stem>/__init__.py` —
// whichever KindFile node exists in the graph. Dart relative
// imports follow the same shape but are not pyrel-tagged so they
// fall through to the same-file / import-aware passes.
//
// Two Cypher passes run sequentially (one per file-naming
// convention) and the counts sum.
func (s *Store) ResolveRelativeImports(lang string) (int, error) {
	if lang != "" && lang != "python" {
		// Only python is meaningful here. Future Dart support
		// would add another pass.
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var total int
	for _, suffix := range []string{".py", "/__init__.py"} {
		q := `
MATCH (caller:Node)-[e:Edge {kind: 'imports'}]->(stub:Node)
WHERE stub.id STARTS WITH 'unresolved::pyrel::'
WITH e, caller, stub, substring(stub.id, 20, size(stub.id) - 19) AS stem
MATCH (target:Node {kind: 'file'})
WHERE target.id = stem + '` + suffix + `'
DELETE e
CREATE (caller)-[newE:Edge {
    kind: 'imports',
    file_path: e.file_path,
    line: e.line,
    confidence: e.confidence,
    confidence_label: e.confidence_label,
    origin: 'ast_resolved',
    tier: 'ast_resolved',
    cross_repo: e.cross_repo,
    meta: e.meta
}]->(target)
RETURN count(newE) AS resolved`
		n, err := s.runResolverQueryLocked(q, "ResolveRelativeImports "+suffix)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
func (s *Store) ResolveCrossRepo() (int, error)             { return 0, nil }
// ResolveExternalCallStubs ensures every external::* edge target
// has a corresponding Node row with kind='external' and promotes
// the edge's origin to ast_resolved. Kuzu's AddEdge already
// auto-stubs the endpoint node via mergeStubNodeLocked, so the
// only work here is the kind/name update + edge origin promotion.
func (s *Store) ResolveExternalCallStubs() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Step 1: stamp kind='external' + name on stub rows the
	// auto-stub created with empty kind.
	const upgradeNodes = `
MATCH (stub:Node)
WHERE stub.id STARTS WITH 'external::'
  AND (stub.kind = '' OR stub.kind IS NULL)
SET stub.kind = 'external',
    stub.name = substring(stub.id, 11, size(stub.id) - 10)
RETURN count(stub) AS upgraded`
	if _, err := s.runResolverQueryLocked(upgradeNodes, "ResolveExternalCallStubs upgrade"); err != nil {
		return 0, err
	}

	// Step 2: promote edge origin for any external::* edge that
	// still has no origin set.
	const promoteEdges = `
MATCH ()-[e:Edge]->(target:Node)
WHERE target.id STARTS WITH 'external::'
  AND (e.origin = '' OR e.origin IS NULL)
SET e.origin = 'ast_resolved', e.tier = 'ast_resolved'
RETURN count(e) AS resolved`
	return s.runResolverQueryLocked(promoteEdges, "ResolveExternalCallStubs promote")
}

// runResolverQueryLocked is the shared boilerplate for a backend-
// resolver Cypher query that returns a single COUNT column. Bumps
// the identity-revision counter by the resolved count.
func (s *Store) runResolverQueryLocked(query, ruleName string) (int, error) {
	res, err := s.conn.Query(query)
	if err != nil {
		return 0, fmt.Errorf("backend-resolver %s: %w", ruleName, err)
	}
	defer res.Close()
	if !res.HasNext() {
		return 0, nil
	}
	row, err := res.Next()
	if err != nil {
		return 0, fmt.Errorf("backend-resolver %s: read result: %w", ruleName, err)
	}
	defer row.Close()
	vals, err := row.GetAsSlice()
	if err != nil || len(vals) == 0 {
		return 0, err
	}
	n, _ := vals[0].(int64)
	if n > 0 {
		s.edgeIdentityRevs.Add(n)
	}
	return int(n), nil
}

// ResolveAllBulk chains every backend-resolver rule in precision-
// descending order and sums the resolved counts. Errors from a
// single rule are non-fatal; the orchestrator logs internally and
// continues so a buggy rule can't block the others.
func (s *Store) ResolveAllBulk() (int, error) {
	var total int
	for _, fn := range []func() (int, error){
		s.ResolveSameFile,
		s.ResolveSamePackage,
		s.ResolveImportAware,
		func() (int, error) { return s.ResolveRelativeImports("") },
		s.ResolveCrossRepo,
		s.ResolveUniqueNames,
		s.ResolveExternalCallStubs,
	} {
		n, err := fn()
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
