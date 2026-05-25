package store_duckdb

import "fmt"

// ResolveSameFile pushes the same-source-file resolution pass into
// DuckDB as a single UPDATE...FROM. For every edge whose to_id is
// `unresolved::Name`, if exactly one Node with that name shares
// the caller's file_path, rewrite to_id in place and promote
// origin/tier to ast_resolved.
func (s *Store) ResolveSameFile() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	const q = `
WITH unique_candidates AS (
    SELECT e.edge_id, MIN(t.id) AS target_id
    FROM edges e
    JOIN nodes c ON c.id = e.from_id
    JOIN nodes t ON t.name = substring(e.to_id, 13)
                AND t.file_path = c.file_path
                AND t.id <> e.to_id
                AND c.file_path <> ''
    WHERE e.to_id LIKE 'unresolved::%'
    GROUP BY e.edge_id
    HAVING COUNT(*) = 1
)
UPDATE edges
SET to_id  = u.target_id,
    origin = 'ast_resolved',
    tier   = 'ast_resolved'
FROM unique_candidates u
WHERE edges.edge_id = u.edge_id`
	return s.runResolverUpdateLocked(q, "ResolveSameFile")
}

// ResolveSamePackage drains the "same Go-style package" case in
// DuckDB SQL: caller and a unique candidate share the same
// directory portion of file_path and the same repo_prefix.
// Directory is extracted via regexp_extract.
func (s *Store) ResolveSamePackage() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	const q = `
WITH unique_candidates AS (
    SELECT e.edge_id, MIN(t.id) AS target_id
    FROM edges e
    JOIN nodes c ON c.id = e.from_id
    JOIN nodes t ON t.name = substring(e.to_id, 13)
                AND regexp_extract(t.file_path, '^(.*)/[^/]+$', 1) =
                    regexp_extract(c.file_path, '^(.*)/[^/]+$', 1)
                AND t.repo_prefix = c.repo_prefix
                AND t.id <> e.to_id
                AND t.file_path <> c.file_path
                AND c.file_path <> ''
                AND regexp_extract(c.file_path, '^(.*)/[^/]+$', 1) <> ''
    WHERE e.to_id LIKE 'unresolved::%'
    GROUP BY e.edge_id
    HAVING COUNT(*) = 1
)
UPDATE edges
SET to_id  = u.target_id,
    origin = 'ast_resolved',
    tier   = 'ast_resolved'
FROM unique_candidates u
WHERE edges.edge_id = u.edge_id`
	return s.runResolverUpdateLocked(q, "ResolveSamePackage")
}
// ResolveImportAware drains the "imported-symbol" case in DuckDB.
// Multi-JOIN: caller's file_path → KindFile node → EdgeImports →
// imported file_path → candidate Node with the unresolved name.
// Unique candidate across the caller's import set wins.
func (s *Store) ResolveImportAware() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	const q = `
WITH unique_candidates AS (
    SELECT e.edge_id, MIN(t.id) AS target_id
    FROM edges e
    JOIN nodes c          ON c.id = e.from_id
    JOIN nodes cf         ON cf.file_path = c.file_path AND cf.kind = 'file'
    JOIN edges ie         ON ie.from_id = cf.id AND ie.kind = 'imports'
    JOIN nodes imf        ON imf.id = ie.to_id
                          AND imf.kind = 'file'
                          AND imf.id NOT LIKE 'external::%'
                          AND imf.id NOT LIKE 'unresolved::%'
    JOIN nodes t          ON t.file_path = imf.file_path
                          AND t.name = substring(e.to_id, 13)
                          AND t.id <> e.to_id
    WHERE e.to_id LIKE 'unresolved::%'
      AND c.file_path <> ''
    GROUP BY e.edge_id
    HAVING COUNT(DISTINCT t.id) = 1
)
UPDATE edges
SET to_id  = u.target_id,
    origin = 'ast_resolved',
    tier   = 'ast_resolved'
FROM unique_candidates u
WHERE edges.edge_id = u.edge_id`
	return s.runResolverUpdateLocked(q, "ResolveImportAware")
}
// ResolveRelativeImports drains `unresolved::pyrel::<stem>` edges
// to KindFile nodes (.py or /__init__.py form).
func (s *Store) ResolveRelativeImports(lang string) (int, error) {
	if lang != "" && lang != "python" {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var total int
	for _, suffix := range []string{".py", "/__init__.py"} {
		q := `
WITH candidates AS (
    SELECT e.edge_id, t.id AS target_id
    FROM edges e
    JOIN nodes t ON t.kind = 'file'
                AND t.id = substring(e.to_id, 20) || '` + suffix + `'
    WHERE e.to_id LIKE 'unresolved::pyrel::%'
      AND e.kind = 'imports'
)
UPDATE edges
SET to_id  = c.target_id,
    origin = 'ast_resolved',
    tier   = 'ast_resolved'
FROM candidates c
WHERE edges.edge_id = c.edge_id`
		n, err := s.runResolverUpdateLocked(q, "ResolveRelativeImports "+suffix)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
func (s *Store) ResolveCrossRepo() (int, error)             { return 0, nil }
// ResolveExternalCallStubs creates a Node row for every external::*
// edge target that doesn't yet have one, sets kind='external' and
// derives name from the id, then promotes the edge origin to
// ast_resolved.
//
// Unlike Kuzu, DuckDB's AddBatch does not auto-stub endpoints, so
// the node insertion is required (not just kind upgrade). Uses
// INSERT ... ON CONFLICT DO NOTHING to keep the operation
// idempotent.
func (s *Store) ResolveExternalCallStubs() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Step 1: insert missing external::* node rows. The schema
	// has id as PRIMARY KEY so the conflict clause silently skips
	// rows already present.
	const insertStubs = `
INSERT INTO nodes (id, kind, name, qual_name, file_path, start_line,
                   end_line, language, repo_prefix, workspace_id,
                   project_id, absolute_file_path, meta)
SELECT DISTINCT e.to_id, 'external', substring(e.to_id, 11), '', '',
                0, 0, '', '', '', '', '', NULL
FROM edges e
LEFT JOIN nodes n ON n.id = e.to_id
WHERE e.to_id LIKE 'external::%' AND n.id IS NULL
ON CONFLICT DO NOTHING`
	if _, err := s.db.Exec(insertStubs); err != nil {
		return 0, fmt.Errorf("backend-resolver ResolveExternalCallStubs insert: %w", err)
	}

	// Also upgrade any pre-existing rows with empty kind (e.g.
	// dummy stubs from prior workloads).
	const upgradeStubs = `
UPDATE nodes
SET kind = 'external', name = substring(id, 11)
WHERE id LIKE 'external::%' AND (kind = '' OR kind <> 'external')`
	if _, err := s.db.Exec(upgradeStubs); err != nil {
		return 0, fmt.Errorf("backend-resolver ResolveExternalCallStubs upgrade: %w", err)
	}

	// Step 2: promote edge origin for external::* edges.
	const promote = `
UPDATE edges
SET origin = 'ast_resolved', tier = 'ast_resolved'
WHERE to_id LIKE 'external::%'
  AND (origin = '' OR origin IS NULL)`
	return s.runResolverUpdateLocked(promote, "ResolveExternalCallStubs promote")
}

// runResolverUpdateLocked is shared boilerplate for a backend-
// resolver UPDATE that returns RowsAffected. Bumps the identity-
// revision counter by the resolved count.
func (s *Store) runResolverUpdateLocked(query, ruleName string) (int, error) {
	res, err := s.db.Exec(query)
	if err != nil {
		return 0, fmt.Errorf("backend-resolver %s: %w", ruleName, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.edgeIdentityRevs.Add(n)
	}
	return int(n), nil
}

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
