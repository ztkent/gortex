package store_sqlite

import (
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions that the SQLite Store satisfies the optional
// git-churn enrichment sidecar capabilities (change A: enrichment moved
// out of nodes.meta into a typed table so the node hot path stops
// gob-encoding rarely-read data and get_churn_rate reads via an index
// instead of an AllNodes scan).
var (
	_ graph.ChurnEnrichmentWriter = (*Store)(nil)
	_ graph.ChurnEnrichmentReader = (*Store)(nil)
)

// churnChunk bounds rows per multi-row INSERT. churn_enrichment has 10
// columns, so at 10 params/row the 999 host-param limit caps a statement
// at 99 rows; 90 leaves headroom. Mirrors shingleChunk / mtimeChunk.
const churnChunk = 90

const churnCols = `node_id, repo_prefix, commit_count, age_days, churn_rate, last_author, last_commit_at, head_sha, branch, computed_at`

// BulkSetChurn persists every churn row for one repo prefix in a single
// transaction, chunked under the host-parameter limit. Idempotent on
// node_id (INSERT OR REPLACE). Empty input is a no-op.
func (s *Store) BulkSetChurn(repoPrefix string, rows []graph.ChurnEnrichment) error {
	if len(rows) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(rows); start += churnChunk {
		end := start + churnChunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*10)
		stmt := make([]byte, 0, 128+len(batch)*24)
		stmt = append(stmt, "INSERT OR REPLACE INTO churn_enrichment ("...)
		stmt = append(stmt, churnCols...)
		stmt = append(stmt, ") VALUES "...)
		for i, e := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?,?,?,?,?,?,?,?,?,?)"...)
			args = append(args, e.NodeID, repoPrefix, e.CommitCount, e.AgeDays,
				e.ChurnRate, e.LastAuthor, e.LastCommitAt, e.HeadSHA, e.Branch, e.ComputedAt)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteChurn drops churn rows for the supplied node ids, chunked into
// `node_id IN (?, …)` DELETEs. Empty input is a no-op.
func (s *Store) DeleteChurn(nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(nodeIDs))
	uniq := make([]string, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	for start := 0; start < len(uniq); start += churnChunk {
		end := start + churnChunk
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]
		args := make([]any, len(chunk))
		stmt := make([]byte, 0, 48+len(chunk)*2)
		stmt = append(stmt, "DELETE FROM churn_enrichment WHERE node_id IN ("...)
		for i, id := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args[i] = id
		}
		stmt = append(stmt, ')')
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ChurnRows returns every churn row for repoPrefix; an EMPTY repoPrefix
// returns ALL rows across repos. This is an index-only read over the
// (small) enriched set — the whole point of the sidecar, replacing the
// AllNodes()+gob-decode scan get_churn_rate used to do.
func (s *Store) ChurnRows(repoPrefix string) []graph.ChurnEnrichment {
	var (
		rows *sql.Rows
		err  error
	)
	if repoPrefix == "" {
		rows, err = s.db.Query(`SELECT ` + churnCols + ` FROM churn_enrichment`)
	} else {
		rows, err = s.db.Query(`SELECT `+churnCols+` FROM churn_enrichment WHERE repo_prefix = ?`, repoPrefix)
	}
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var out []graph.ChurnEnrichment
	for rows.Next() {
		var e graph.ChurnEnrichment
		if err := rows.Scan(&e.NodeID, &e.RepoPrefix, &e.CommitCount, &e.AgeDays,
			&e.ChurnRate, &e.LastAuthor, &e.LastCommitAt, &e.HeadSHA, &e.Branch, &e.ComputedAt); err != nil {
			return out
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return out
	}
	return out
}
