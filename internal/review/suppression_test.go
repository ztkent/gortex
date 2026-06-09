package review

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/persistence"
)

func openTempSuppressionStore(t *testing.T) *SuppressionStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	sidecar, err := persistence.OpenSidecar(path)
	if err != nil || sidecar == nil {
		t.Fatalf("open sidecar: %v", err)
	}
	t.Cleanup(func() { _ = sidecar.Close() })
	return NewSuppressionStore(sidecar)
}

func suppFinding() Finding {
	return Finding{
		Rule:       "nil-deref",
		Category:   "nil-deref",
		File:       "pkg/x.go",
		SymbolID:   "pkg/x.go::Foo",
		Line:       14,
		SourceLine: "result := obj.Field()",
		Severity:   SevError,
		Message:    "possible nil dereference",
	}
}

// TestSuppressionStoreRoundTrip proves Suppress → IsSuppressed → List →
// Unsuppress round-trips through the sidecar.
func TestSuppressionStoreRoundTrip(t *testing.T) {
	store := openTempSuppressionStore(t)
	f := suppFinding()
	key := IdentityKey(f)

	// Not suppressed initially.
	if store.IsSuppressed("rk", key) {
		t.Fatal("finding must not be suppressed before Suppress")
	}

	// Suppress, then IsSuppressed is true.
	if err := store.Suppress("rk", f, "false positive: guarded above", "me@zzet.org"); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if !store.IsSuppressed("rk", key) {
		t.Fatal("finding must be suppressed after Suppress")
	}

	// Scope isolation: another repo key is unaffected.
	if store.IsSuppressed("other", key) {
		t.Fatal("suppression must be per-repo")
	}

	// List returns the row with its context + a non-zero hit count (the two
	// IsSuppressed calls above bumped it).
	rows, err := store.List("rk")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 suppression, got %d", len(rows))
	}
	r := rows[0]
	if r.IdentityKey != key || r.Rule != f.Rule || r.SymbolID != f.SymbolID {
		t.Fatalf("list row context wrong: %+v", r)
	}
	if r.Reason != "false positive: guarded above" || r.Author != "me@zzet.org" {
		t.Fatalf("list row reason/author wrong: %+v", r)
	}
	if r.HitCount < 1 {
		t.Fatalf("IsSuppressed must bump hit_count, got %d", r.HitCount)
	}

	// Unsuppress, then IsSuppressed is false again.
	if err := store.Unsuppress("rk", key); err != nil {
		t.Fatalf("unsuppress: %v", err)
	}
	if store.IsSuppressed("rk", key) {
		t.Fatal("finding must not be suppressed after Unsuppress")
	}
}

// TestSuppressionSurvivesLineDrift proves a suppression keyed off identity stays
// in effect after the finding moves to a different line (same trimmed source).
func TestSuppressionSurvivesLineDrift(t *testing.T) {
	store := openTempSuppressionStore(t)
	at10 := suppFinding()
	at10.Line = 10
	if err := store.Suppress("rk", at10, "", ""); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	at40 := suppFinding()
	at40.Line = 40 // same code, shifted down
	if !store.IsSuppressed("rk", IdentityKey(at40)) {
		t.Fatal("suppression must survive line drift")
	}
}

// TestNilSuppressionStoreTolerated proves a nil store and a store over a nil
// sidecar are no-ops: IsSuppressed false, mutators succeed, List empty.
func TestNilSuppressionStoreTolerated(t *testing.T) {
	for name, store := range map[string]*SuppressionStore{
		"nil store":   nil,
		"nil sidecar": NewSuppressionStore(nil),
	} {
		f := suppFinding()
		if store.IsSuppressed("rk", IdentityKey(f)) {
			t.Fatalf("%s: IsSuppressed must be false", name)
		}
		if err := store.Suppress("rk", f, "", ""); err != nil {
			t.Fatalf("%s: Suppress must be a no-op, got %v", name, err)
		}
		if err := store.Unsuppress("rk", IdentityKey(f)); err != nil {
			t.Fatalf("%s: Unsuppress must be a no-op, got %v", name, err)
		}
		rows, err := store.List("rk")
		if err != nil {
			t.Fatalf("%s: List must not error, got %v", name, err)
		}
		if len(rows) != 0 {
			t.Fatalf("%s: List must be empty, got %d", name, len(rows))
		}
	}
}

// TestGateDropsSuppressedFinding proves the gate, wired with a suppression
// store, drops a suppressed finding and counts it under IdentitySuppressed while
// keeping the rest — and that the count rolls into GateStats.Suppressed().
func TestGateDropsSuppressedFinding(t *testing.T) {
	store := openTempSuppressionStore(t)

	bad := suppFinding() // the one we will suppress
	good := Finding{
		Rule:       "check-then-act",
		Category:   "concurrency",
		File:       "pkg/y.go",
		SymbolID:   "pkg/y.go::Bar",
		Line:       7,
		SourceLine: "if !ok { return }",
		Severity:   SevWarning,
		Message:    "check then act",
	}

	if err := store.Suppress("rk", bad, "known FP", "tester"); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	in := []Finding{bad, good}
	kept, stats := NewGate(config.ReviewConfig{}).
		WithSuppression(store, "rk").
		Apply(in)

	if len(kept) != 1 {
		t.Fatalf("want 1 kept, got %d: %+v", len(kept), kept)
	}
	if kept[0].SymbolID != good.SymbolID {
		t.Fatalf("wrong finding kept: %+v", kept[0])
	}
	if stats.IdentitySuppressed != 1 {
		t.Fatalf("want IdentitySuppressed=1, got %d (%+v)", stats.IdentitySuppressed, stats)
	}
	if stats.Suppressed() != 1 {
		t.Fatalf("want Suppressed()=1, got %d", stats.Suppressed())
	}
	// The kept finding carries a stamped identity key (so a later suppress can
	// reference it without re-deriving).
	if kept[0].IdentityKey == "" {
		t.Fatal("gate must stamp IdentityKey on kept findings")
	}

	// A different repo key is unaffected: nothing is suppressed.
	kept2, stats2 := NewGate(config.ReviewConfig{}).
		WithSuppression(store, "other-repo").
		Apply(in)
	if len(kept2) != 2 || stats2.IdentitySuppressed != 0 {
		t.Fatalf("suppression must be per-repo: kept %d, suppressed %d", len(kept2), stats2.IdentitySuppressed)
	}

	// A nil store leaves everything (suppression is purely additive).
	kept3, stats3 := NewGate(config.ReviewConfig{}).
		WithSuppression(nil, "rk").
		Apply(in)
	if len(kept3) != 2 || stats3.IdentitySuppressed != 0 {
		t.Fatalf("nil suppression store must keep everything: kept %d, suppressed %d", len(kept3), stats3.IdentitySuppressed)
	}
}
