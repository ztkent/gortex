package persistence

import (
	"database/sql"
	"fmt"
	"time"
)

// Token-savings ledger tables. The savings ledger is machine-global —
// unlike notes/memories it carries no repo_key partition; per-repo
// attribution rides on the bucket key of savings_totals and the repo
// column of savings_events instead.
//
// Three tables, one job each:
//   - savings_events: one row per recorded source-reading tool call.
//     Durable at the call (single INSERT inside the observation tx), so
//     a SIGKILLed server loses nothing — the property the flat-file
//     ledger's batched flush could not give.
//   - savings_totals: running aggregates keyed by bucket ('' top-line,
//     'repo:<prefix>', 'lang:<code>'), updated transactionally with the
//     event insert so reads are point lookups instead of full scans.
//   - savings_meta: first_seen / last_updated unix-nano stamps.

// SavingsEvent is one recorded source-reading observation.
type SavingsEvent struct {
	TS        time.Time
	SessionID string
	Tool      string
	Repo      string
	Language  string
	Returned  int64
	Saved     int64
}

// SavingsTotalsRow is the aggregate for one savings_totals bucket.
type SavingsTotalsRow struct {
	Saved    int64
	Returned int64
	Calls    int64
}

const savingsLegacyMigrationKind = "savings_files"

// AddSavingsObservation books one observation: the event row, the
// affected totals buckets, and the meta stamps, in a single transaction.
func (s *SidecarStore) AddSavingsObservation(ev SavingsEvent) error {
	if s == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("persistence: savings tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ts := ev.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	tsN := ts.UTC().UnixNano()

	if _, err := tx.Exec(
		`INSERT INTO savings_events (ts, session_id, tool, repo, language, returned, saved) VALUES (?,?,?,?,?,?,?)`,
		tsN, ev.SessionID, ev.Tool, ev.Repo, ev.Language, ev.Returned, ev.Saved,
	); err != nil {
		return fmt.Errorf("persistence: savings event: %w", err)
	}

	buckets := []string{""}
	if ev.Repo != "" {
		buckets = append(buckets, "repo:"+ev.Repo)
	}
	if ev.Language != "" {
		buckets = append(buckets, "lang:"+ev.Language)
	}
	for _, bucket := range buckets {
		if err := upsertSavingsBucket(tx, bucket, ev.Saved, ev.Returned, 1); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(
		`INSERT INTO savings_meta (key, value) VALUES ('first_seen', ?) ON CONFLICT(key) DO NOTHING`, tsN,
	); err != nil {
		return fmt.Errorf("persistence: savings meta: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO savings_meta (key, value) VALUES ('last_updated', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, tsN,
	); err != nil {
		return fmt.Errorf("persistence: savings meta: %w", err)
	}

	return tx.Commit()
}

func upsertSavingsBucket(tx *sql.Tx, bucket string, saved, returned, calls int64) error {
	_, err := tx.Exec(
		`INSERT INTO savings_totals (bucket, saved, returned, calls) VALUES (?,?,?,?)
		 ON CONFLICT(bucket) DO UPDATE SET
		   saved    = savings_totals.saved + excluded.saved,
		   returned = savings_totals.returned + excluded.returned,
		   calls    = savings_totals.calls + excluded.calls`,
		bucket, saved, returned, calls,
	)
	if err != nil {
		return fmt.Errorf("persistence: savings totals: %w", err)
	}
	return nil
}

// SavingsTotals returns every totals bucket plus the first_seen /
// last_updated stamps. Buckets map keys are '' (top-line),
// 'repo:<prefix>', and 'lang:<code>'. The zero time means "never".
func (s *SidecarStore) SavingsTotals() (map[string]SavingsTotalsRow, time.Time, time.Time, error) {
	if s == nil {
		return map[string]SavingsTotalsRow{}, time.Time{}, time.Time{}, nil
	}
	rows, err := s.db.Query(`SELECT bucket, saved, returned, calls FROM savings_totals`)
	if err != nil {
		return nil, time.Time{}, time.Time{}, fmt.Errorf("persistence: savings totals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]SavingsTotalsRow)
	for rows.Next() {
		var bucket string
		var r SavingsTotalsRow
		if err := rows.Scan(&bucket, &r.Saved, &r.Returned, &r.Calls); err != nil {
			return nil, time.Time{}, time.Time{}, err
		}
		out[bucket] = r
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, time.Time{}, err
	}

	return out, s.savingsMetaTime("first_seen"), s.savingsMetaTime("last_updated"), nil
}

func (s *SidecarStore) savingsMetaTime(key string) time.Time {
	var n int64
	row := s.db.QueryRow(`SELECT value FROM savings_meta WHERE key = ?`, key)
	if err := row.Scan(&n); err != nil || n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// SavingsEventsSince returns events with ts >= since, oldest first.
// since=zero returns everything.
func (s *SidecarStore) SavingsEventsSince(since time.Time) ([]SavingsEvent, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT ts, session_id, tool, repo, language, returned, saved
		 FROM savings_events WHERE ts >= ? ORDER BY ts, id`,
		unixOrZero(since),
	)
	if err != nil {
		return nil, fmt.Errorf("persistence: savings events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SavingsEvent
	for rows.Next() {
		var ev SavingsEvent
		var tsN int64
		if err := rows.Scan(&tsN, &ev.SessionID, &ev.Tool, &ev.Repo, &ev.Language, &ev.Returned, &ev.Saved); err != nil {
			return nil, err
		}
		ev.TS = time.Unix(0, tsN).UTC()
		out = append(out, ev)
	}
	return out, rows.Err()
}

// SavingsLegacyImportDone reports whether the one-shot flat-file
// (savings.json + savings.jsonl) import has already run.
func (s *SidecarStore) SavingsLegacyImportDone() bool {
	if s == nil {
		return true
	}
	return s.migrationDone("", savingsLegacyMigrationKind)
}

// ImportLegacySavings seeds the ledger from the flat-file era: bucket
// totals from the cumulative savings.json and event rows from the
// savings.jsonl log. Idempotent — guarded by a migration mark, which is
// set even for an empty import so the file probing never repeats. The
// mark is checked and written inside the import transaction, so two
// processes racing the first start (daemon + CLI) cannot both seed the
// ledger: the loser either sees the winner's mark or aborts on the
// write conflict. The caller owns reading (and afterwards renaming)
// the legacy files.
func (s *SidecarStore) ImportLegacySavings(buckets map[string]SavingsTotalsRow, firstSeen, lastUpdated time.Time, events []SavingsEvent) error {
	if s == nil || s.migrationDone("", savingsLegacyMigrationKind) {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("persistence: savings import tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-check under the transaction — the cheap pre-check above races
	// with other writers (in-process via writeMu it cannot, but another
	// process opening the same database can).
	var marked int
	if err := tx.QueryRow(
		`SELECT COUNT(1) FROM migration_marks WHERE repo_key = '' AND kind = ?`, savingsLegacyMigrationKind,
	).Scan(&marked); err != nil {
		return fmt.Errorf("persistence: savings import mark check: %w", err)
	}
	if marked > 0 {
		return nil
	}

	for bucket, r := range buckets {
		if err := upsertSavingsBucket(tx, bucket, r.Saved, r.Returned, r.Calls); err != nil {
			return err
		}
	}
	for _, ev := range events {
		if _, err := tx.Exec(
			`INSERT INTO savings_events (ts, session_id, tool, repo, language, returned, saved) VALUES (?,?,?,?,?,?,?)`,
			unixOrZero(ev.TS), ev.SessionID, ev.Tool, ev.Repo, ev.Language, ev.Returned, ev.Saved,
		); err != nil {
			return fmt.Errorf("persistence: savings import event: %w", err)
		}
	}
	if !firstSeen.IsZero() {
		if _, err := tx.Exec(
			`INSERT INTO savings_meta (key, value) VALUES ('first_seen', ?)
			 ON CONFLICT(key) DO UPDATE SET value = MIN(savings_meta.value, excluded.value)`,
			unixOrZero(firstSeen),
		); err != nil {
			return fmt.Errorf("persistence: savings import meta: %w", err)
		}
	}
	if !lastUpdated.IsZero() {
		if _, err := tx.Exec(
			`INSERT INTO savings_meta (key, value) VALUES ('last_updated', ?)
			 ON CONFLICT(key) DO UPDATE SET value = MAX(savings_meta.value, excluded.value)`,
			unixOrZero(lastUpdated),
		); err != nil {
			return fmt.Errorf("persistence: savings import meta: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO migration_marks (repo_key, kind, done_at) VALUES ('', ?, ?)`,
		savingsLegacyMigrationKind, time.Now().UTC().UnixNano(),
	); err != nil {
		return fmt.Errorf("persistence: savings import mark: %w", err)
	}
	return tx.Commit()
}

// ResetSavings wipes the savings ledger (events, totals, meta). The
// legacy-import migration mark survives so renamed flat files are not
// re-imported after a reset.
func (s *SidecarStore) ResetSavings() error {
	if s == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for _, stmt := range []string{
		`DELETE FROM savings_events`,
		`DELETE FROM savings_totals`,
		`DELETE FROM savings_meta`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("persistence: savings reset: %w", err)
		}
	}
	return nil
}
