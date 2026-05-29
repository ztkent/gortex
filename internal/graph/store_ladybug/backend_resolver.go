package store_ladybug

import (
	"fmt"
	"strings"
)

// upgradeUnresolvedStubs stamps `kind='unresolved'` plus the extracted
// `name` and `repo_prefix` on every auto-stub the bulk COPY created for
// an unresolved call target. Without this, the per-rule resolver
// queries below would never find the stubs in multi-repo mode because:
//
//   - copyBulkLocked rewrites unresolved IDs to `<repoPrefix>::unresolved::<name>`
//     (to dodge cross-repo PK collisions on the shared SymbolFTS / Node
//     tables).
//   - The auto-stub at copyBulkLocked creates Node rows for these
//     rewritten IDs with empty Name / Kind / RepoPrefix.
//   - Every original resolver rule did
//     `WHERE stub.id STARTS WITH 'unresolved::'` — literal — which
//     never matches `gortex::unresolved::AddNode`. The fallback
//     `substring(stub.id, 13, ...)` for name extraction was also
//     keyed to the un-prefixed form.
//
// The upgrade runs once per ResolveAllBulk pass, before the
// downstream rules. After it runs, every stub carries:
//   - kind = 'unresolved'
//   - name = the bare symbol name (last segment after `unresolved::`)
//   - repo_prefix = empty for the legacy form, or the prefix for the
//                   multi-repo form
//
// The rules below then MATCH `stub.kind = 'unresolved'` and read
// `stub.name` directly — no substring math, no format coupling.
func (s *Store) upgradeUnresolvedStubs() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Stub IDs come in two encodings:
	//   unresolved::Name                 (legacy / single-repo)
	//   <repoPrefix>::unresolved::Name   (multi-repo COPY rewrite)
	//
	// regexp_replace strips everything up to and including the
	// last `unresolved::` substring, leaving the bare name on
	// `stub.name`. The repo prefix is everything before
	// `::unresolved::` (or empty for the single-repo form).
	const q = `
MATCH (stub:Node)
WHERE (stub.id STARTS WITH 'unresolved::' OR stub.id CONTAINS '::unresolved::')
  AND (stub.kind = '' OR stub.kind IS NULL)
SET stub.kind = 'unresolved',
    stub.name = regexp_replace(stub.id, '^.*unresolved::', ''),
    stub.repo_prefix = CASE
        WHEN stub.id STARTS WITH 'unresolved::' THEN ''
        ELSE regexp_replace(stub.id, '::unresolved::.*$', '')
    END
RETURN count(stub) AS upgraded`
	return s.runResolverQueryLocked(q, "upgradeUnresolvedStubs")
}

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
WHERE stub.kind = 'unresolved' AND caller.file_path <> ''
WITH e, caller, stub, stub.name AS name
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
WHERE stub.kind = 'unresolved'
  AND caller.file_path <> ''
  AND caller.file_path CONTAINS '/'
WITH e, caller, stub, stub.name AS name,
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
WHERE stub.kind = 'unresolved' AND caller.file_path <> ''
WITH e, caller, stub, stub.name AS name
MATCH (callerFile:Node {file_path: caller.file_path})
WHERE callerFile.kind = 'file'
MATCH (callerFile)-[imp:Edge {kind: 'imports'}]->(importedFile:Node)
WHERE importedFile.kind = 'file'
  AND NOT (importedFile.id STARTS WITH 'external::')
  AND importedFile.kind <> 'unresolved'
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
WHERE stub.kind = 'unresolved' AND stub.name STARTS WITH 'pyrel::'
WITH e, caller, stub, substring(stub.name, 7, size(stub.name) - 7) AS stem
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
// ResolveCrossRepo drains unresolved edges that bind unambiguously
// to a Node in a different repo. Only fires when the caller has a
// non-empty repo_prefix (i.e. we're in a multi-repo workspace) and
// exactly one candidate exists in a different repo. Sets
// cross_repo=true on the resulting edge so downstream consumers
// know the binding crosses a workspace boundary.
func (s *Store) ResolveCrossRepo() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	const q = `
MATCH (caller:Node)-[e:Edge]->(stub:Node)
WHERE stub.kind = 'unresolved'
  AND caller.repo_prefix <> ''
WITH e, caller, stub, stub.name AS name
OPTIONAL MATCH (cnd:Node {name: name})
WHERE cnd.repo_prefix <> caller.repo_prefix
  AND cnd.repo_prefix <> ''
  AND cnd.id <> stub.id
WITH e, caller, stub, name, count(cnd) AS cnt
WHERE cnt = 1
MATCH (target:Node {name: name})
WHERE target.repo_prefix <> caller.repo_prefix
  AND target.repo_prefix <> ''
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
    cross_repo: 1,
    meta: e.meta
}]->(target)
RETURN count(newE) AS resolved`
	return s.runResolverQueryLocked(q, "ResolveCrossRepo")
}
// ResolveExternalCallStubs ensures every external::* edge target
// has a corresponding Node row with kind='external' and promotes
// the edge's origin to ast_resolved. Kuzu's AddEdge already
// auto-stubs the endpoint node via mergeStubNodeLocked, so the
// only work here is the kind/name update + edge origin promotion.
// ResolveMethodCalls drains the receiver-method-call stub form
// `unresolved::*.<method>` — the target the parsers emit for a call
// `x.Method()` when they can't name x's type at extraction time (Go:
// internal/parser/languages/golang.go:646; same `*.` convention in
// java/ruby/typescript/...). upgradeUnresolvedStubs leaves
// stub.name = "*.<method>" (the `*.` is kept), so the name-EQUALITY
// rules above never match it, and the Go-side resolver's
// EdgesWithUnresolvedTarget scan (literal `unresolved::` prefix) never
// sees the repo-prefixed `<repo>::unresolved::*.<method>` form — so in
// multi-repo mode method callers were invisible to find_usages /
// get_callers entirely.
//
// We bind the stub to a concrete method node when EXACTLY ONE method
// in the caller's repo carries that name. Method nodes store the BARE
// method name in the `name` column (e.g. "querySelect"; the receiver
// lives in meta.receiver / enclosing), so once the `*.` is stripped
// the stub name equals the method node name exactly — an indexed
// equality match, no suffix scan. The uniqueness guard means no false
// edges: an ambiguous method name (String / Close / Get, defined on
// several types) is left unresolved for a future receiver-type-aware
// pass (the edge carries a `receiver_type` meta hint) rather than
// bound to an arbitrary type.
func (s *Store) ResolveMethodCalls() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	const q = `
MATCH (caller:Node)-[e:Edge]->(stub:Node)
WHERE stub.kind = 'unresolved' AND stub.name STARTS WITH '*.'
WITH e, caller, stub, substring(stub.name, 3, size(stub.name) - 2) AS mname
WHERE mname <> ''
OPTIONAL MATCH (cnd:Node)
WHERE cnd.kind = 'method'
  AND cnd.repo_prefix = caller.repo_prefix
  AND cnd.id <> stub.id
  AND cnd.name = mname
WITH e, caller, stub, mname, count(cnd) AS cnt
WHERE cnt = 1
MATCH (target:Node)
WHERE target.kind = 'method'
  AND target.repo_prefix = caller.repo_prefix
  AND target.name = mname
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
	return s.runResolverQueryLocked(q, "ResolveMethodCalls")
}

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
		s.writeGen.Add(1)
	}
	return int(n), nil
}

// ResolveAllBulk chains every backend-resolver rule in precision-
// descending order and sums the resolved counts. Errors from a single
// rule are non-fatal: the chain CONTINUES so one failing rule can't
// disable every rule after it. (The previous code `return`ed on the
// first error — which silently skipped e.g. ResolveMethodCalls whenever
// an earlier rule errored on a large graph, the bug that made method
// callers invisible. The Store has no logger, so the failing rule
// names ride on the returned error instead; the caller can surface
// them.)
func (s *Store) ResolveAllBulk() (int, error) {
	var total int
	var ruleErrs []string
	rules := []struct {
		name string
		fn   func() (int, error)
	}{
		// MUST run first: stamps kind='unresolved' + name + repo_prefix
		// on the auto-stub Node rows so the rules below can match them
		// in both `unresolved::*` and `<prefix>::unresolved::*` forms.
		{"upgradeUnresolvedStubs", s.upgradeUnresolvedStubs},
		{"ResolveSameFile", s.ResolveSameFile},
		{"ResolveSamePackage", s.ResolveSamePackage},
		{"ResolveImportAware", s.ResolveImportAware},
		{"ResolveRelativeImports", func() (int, error) { return s.ResolveRelativeImports("") }},
		{"ResolveCrossRepo", s.ResolveCrossRepo},
		{"ResolveUniqueNames", s.ResolveUniqueNames},
		{"ResolveMethodCalls", s.ResolveMethodCalls},
		{"ResolveExternalCallStubs", s.ResolveExternalCallStubs},
	}
	for _, r := range rules {
		n, err := r.fn()
		total += n
		if err != nil {
			ruleErrs = append(ruleErrs, fmt.Sprintf("%s: %v", r.name, err))
		}
	}
	if len(ruleErrs) > 0 {
		return total, fmt.Errorf("backend-resolver rule errors: %s", strings.Join(ruleErrs, "; "))
	}
	return total, nil
}
