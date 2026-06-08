package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions that the SQLite Store satisfies the optional
// reference-facts persistence capability. Persisting resolved-reference facts
// in the same backend the graph lives in makes a reference's resolution an
// auditable, diffable record and a warm-restart seed.
var (
	_ graph.RefFactsWriter = (*Store)(nil)
	_ graph.RefFactsReader = (*Store)(nil)
)

// refFactChunk bounds rows per multi-row INSERT. 11 params/row; 80 rows = 880
// host params, under SQLite's 999 default. Mirrors shingleChunk.
const refFactChunk = 80

// candidate-list separator (unit separator — never appears in identifiers).
const refFactCandSep = "\x1f"

func encodeCandidates(c []string) string { return strings.Join(c, refFactCandSep) }

func decodeCandidates(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, refFactCandSep)
}

// BulkSetRefFacts persists resolved-reference facts for one repo prefix in a
// single transaction, chunked under the host-parameter limit. Idempotent on
// (repo_prefix, from_id, to_id, kind, line). Empty input is a no-op.
func (s *Store) BulkSetRefFacts(repoPrefix string, facts []graph.RefFact) error {
	if len(facts) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(facts); start += refFactChunk {
		end := start + refFactChunk
		if end > len(facts) {
			end = len(facts)
		}
		batch := facts[start:end]
		args := make([]any, 0, len(batch)*11)
		stmt := make([]byte, 0, 96+len(batch)*24)
		stmt = append(stmt, "INSERT OR REPLACE INTO ref_facts (repo_prefix, from_id, to_id, kind, ref_name, line, origin, tier, candidates, file_path, lang) VALUES "...)
		for i, f := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"...)
			args = append(args, repoPrefix, f.FromID, f.ToID, f.Kind, f.RefName, f.Line, f.Origin, f.Tier, encodeCandidates(f.Candidates), f.FilePath, f.Lang)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteRefFactsByFiles drops all facts sourced in the supplied files for one
// repo prefix, chunked into `file_path IN (…)` DELETEs. Empty input is a no-op.
func (s *Store) DeleteRefFactsByFiles(repoPrefix string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(files); start += refFactChunk {
		end := start + refFactChunk
		if end > len(files) {
			end = len(files)
		}
		chunk := files[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoPrefix)
		stmt := make([]byte, 0, 64+len(chunk)*2)
		stmt = append(stmt, "DELETE FROM ref_facts WHERE repo_prefix = ? AND file_path IN ("...)
		for i, f := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, f)
		}
		stmt = append(stmt, ')')
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadRefFactsByFiles returns the persisted facts for one repo prefix, scoped
// to the given files (all files when files is empty). Always non-nil.
func (s *Store) LoadRefFactsByFiles(repoPrefix string, files []string) ([]graph.RefFact, error) {
	out := []graph.RefFact{}
	scan := func(query string, args ...any) error {
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var f graph.RefFact
			var cand string
			if err := rows.Scan(&f.FromID, &f.ToID, &f.Kind, &f.RefName, &f.Line, &f.Origin, &f.Tier, &cand, &f.FilePath, &f.Lang); err != nil {
				return err
			}
			f.RepoPrefix = repoPrefix
			f.Candidates = decodeCandidates(cand)
			out = append(out, f)
		}
		return rows.Err()
	}
	const cols = `from_id, to_id, kind, ref_name, line, origin, tier, candidates, file_path, lang`
	if len(files) == 0 {
		if err := scan(`SELECT `+cols+` FROM ref_facts WHERE repo_prefix = ?`, repoPrefix); err != nil {
			return nil, err
		}
		return out, nil
	}
	for start := 0; start < len(files); start += refFactChunk {
		end := start + refFactChunk
		if end > len(files) {
			end = len(files)
		}
		chunk := files[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoPrefix)
		stmt := make([]byte, 0, 96+len(chunk)*2)
		stmt = append(stmt, "SELECT "+cols+" FROM ref_facts WHERE repo_prefix = ? AND file_path IN ("...)
		for i, f := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, f)
		}
		stmt = append(stmt, ')')
		if err := scan(string(stmt), args...); err != nil {
			return nil, err
		}
	}
	return out, nil
}
