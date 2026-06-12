package persistence

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestSidecar(t *testing.T) (*SidecarStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	sc, err := OpenSidecar(path)
	if err != nil {
		t.Fatal(err)
	}
	return sc, path
}

// The durability contract behind the savings ledger: an observation
// survives a full close + reopen of the database — no flush step exists
// to forget.
func TestSavings_DurableAcrossReopen(t *testing.T) {
	sc, path := openTestSidecar(t)

	ev := SavingsEvent{
		TS:        time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		SessionID: "sess-1",
		Tool:      "get_symbol_source",
		Repo:      "repo-a",
		Language:  "go",
		Returned:  23,
		Saved:     77,
	}
	if err := sc.AddSavingsObservation(ev); err != nil {
		t.Fatal(err)
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}

	sc2, err := OpenSidecar(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sc2.Close() }()

	buckets, firstSeen, lastUpdated, err := sc2.SavingsTotals()
	if err != nil {
		t.Fatal(err)
	}
	top := buckets[""]
	if top.Calls != 1 || top.Saved != 77 || top.Returned != 23 {
		t.Errorf("top-line bucket = %+v, want calls=1 saved=77 returned=23", top)
	}
	if r := buckets["repo:repo-a"]; r.Calls != 1 {
		t.Errorf("repo bucket = %+v, want calls=1", r)
	}
	if l := buckets["lang:go"]; l.Calls != 1 {
		t.Errorf("lang bucket = %+v, want calls=1", l)
	}
	if !firstSeen.Equal(ev.TS) || !lastUpdated.Equal(ev.TS) {
		t.Errorf("meta stamps = (%v, %v), want both %v", firstSeen, lastUpdated, ev.TS)
	}

	evs, err := sc2.SavingsEventsSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].SessionID != "sess-1" || !evs[0].TS.Equal(ev.TS) {
		t.Errorf("reloaded events = %+v", evs)
	}
}

func TestSavings_MetaStampsMinMax(t *testing.T) {
	sc, _ := openTestSidecar(t)
	defer func() { _ = sc.Close() }()

	t1 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	if err := sc.AddSavingsObservation(SavingsEvent{TS: t1, Tool: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := sc.AddSavingsObservation(SavingsEvent{TS: t2, Tool: "b"}); err != nil {
		t.Fatal(err)
	}

	_, firstSeen, lastUpdated, err := sc.SavingsTotals()
	if err != nil {
		t.Fatal(err)
	}
	if !firstSeen.Equal(t1) {
		t.Errorf("first_seen = %v, want %v (first observation wins)", firstSeen, t1)
	}
	if !lastUpdated.Equal(t2) {
		t.Errorf("last_updated = %v, want %v (latest observation wins)", lastUpdated, t2)
	}
}

func TestSavings_ResetClearsButKeepsImportMark(t *testing.T) {
	sc, _ := openTestSidecar(t)
	defer func() { _ = sc.Close() }()

	if err := sc.ImportLegacySavings(
		map[string]SavingsTotalsRow{"": {Saved: 100, Returned: 10, Calls: 1}},
		time.Now().UTC(), time.Now().UTC(), nil,
	); err != nil {
		t.Fatal(err)
	}
	if !sc.SavingsLegacyImportDone() {
		t.Fatal("import mark must be set after ImportLegacySavings")
	}
	if err := sc.ResetSavings(); err != nil {
		t.Fatal(err)
	}
	buckets, firstSeen, _, err := sc.SavingsTotals()
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 0 || !firstSeen.IsZero() {
		t.Errorf("reset must clear totals + meta, got buckets=%v firstSeen=%v", buckets, firstSeen)
	}
	if !sc.SavingsLegacyImportDone() {
		t.Error("reset must NOT clear the legacy-import mark (renamed files would re-import)")
	}
}

func TestSavings_ImportIsIdempotent(t *testing.T) {
	sc, _ := openTestSidecar(t)
	defer func() { _ = sc.Close() }()

	rows := map[string]SavingsTotalsRow{"": {Saved: 100, Returned: 10, Calls: 1}}
	if err := sc.ImportLegacySavings(rows, time.Time{}, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := sc.ImportLegacySavings(rows, time.Time{}, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	buckets, _, _, err := sc.SavingsTotals()
	if err != nil {
		t.Fatal(err)
	}
	if got := buckets[""].Calls; got != 1 {
		t.Errorf("calls after double import = %d, want 1", got)
	}
}
