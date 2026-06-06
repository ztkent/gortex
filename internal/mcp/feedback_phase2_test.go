package mcp

import (
	"testing"
	"time"

	"github.com/zzet/gortex/internal/persistence"
)

// --- (a) per-keyword-cluster feedback scoping ---

func TestFeedbackKeywordScoping(t *testing.T) {
	fm := &feedbackManager{}
	// Useful for an auth task.
	if err := fm.Record(persistence.FeedbackEntry{
		Task:   "validate auth token",
		Useful: []string{"auth.go::ValidateToken"},
	}); err != nil {
		t.Fatal(err)
	}
	// Useful for an unrelated billing task.
	if err := fm.Record(persistence.FeedbackEntry{
		Task:   "compute invoice billing total",
		Useful: []string{"billing.go::Total"},
	}); err != nil {
		t.Fatal(err)
	}

	// Scoped to the auth task: ValidateToken is useful, billing is not.
	if got := fm.GetSymbolScoreForQuery("auth.go::ValidateToken", "auth token refresh"); got <= 0 {
		t.Fatalf("ValidateToken should score positive for an auth query, got %v", got)
	}
	if got := fm.GetSymbolScoreForQuery("billing.go::Total", "auth token refresh"); got != 0 {
		t.Fatalf("billing symbol must NOT contaminate an auth query, got %v", got)
	}
	// Scoped to the billing task: the inverse.
	if got := fm.GetSymbolScoreForQuery("billing.go::Total", "invoice billing amount"); got <= 0 {
		t.Fatalf("billing symbol should score positive for a billing query, got %v", got)
	}
	if got := fm.GetSymbolScoreForQuery("auth.go::ValidateToken", "invoice billing amount"); got != 0 {
		t.Fatalf("auth symbol must NOT contaminate a billing query, got %v", got)
	}
}

func TestFeedbackLegacyEntryMatchesAnyQuery(t *testing.T) {
	fm := &feedbackManager{}
	// A legacy entry with no Task/Keywords (pre-cluster-scoping data).
	fm.store.Entries = append(fm.store.Entries, persistence.FeedbackEntry{
		Useful: []string{"x.go::X"},
	})
	if got := fm.GetSymbolScoreForQuery("x.go::X", "any unrelated query"); got <= 0 {
		t.Fatalf("legacy keyword-less entry should match any query, got %v", got)
	}
}

func TestMissedSymbolsForQueryScoping(t *testing.T) {
	fm := &feedbackManager{}
	for i := 0; i < 3; i++ {
		_ = fm.Record(persistence.FeedbackEntry{Task: "auth login flow", Missing: []string{"auth.go::Login"}})
		_ = fm.Record(persistence.FeedbackEntry{Task: "render dashboard chart", Missing: []string{"ui.go::Chart"}})
	}
	got := fm.MissedSymbolsForQuery("auth session login", 2)
	if len(got) != 1 || got[0] != "auth.go::Login" {
		t.Fatalf("MissedSymbolsForQuery(auth) = %v, want [auth.go::Login]", got)
	}
}

// --- negative signal: skip-above netting in the keyword index ---

func TestKeywordNegativeNetsOutBoost(t *testing.T) {
	cm := &comboManager{now: func() int64 { return 1000 }}
	// Build up a keyword association above the gate.
	for i := 0; i < 5; i++ {
		cm.Record("auth token validate", "auth.go::ValidateToken")
	}
	if m := cm.KeywordBoostMap("auth token validate"); m["auth.go::ValidateToken"] <= 1.0 {
		t.Fatalf("expected a keyword boost before negatives, got %v", m["auth.go::ValidateToken"])
	}
	// The agent then keeps skipping over it for the same keywords.
	for i := 0; i < 6; i++ {
		cm.RecordNegative("auth token validate", []string{"auth.go::ValidateToken"})
	}
	if m := cm.KeywordBoostMap("auth token validate"); m["auth.go::ValidateToken"] != 0 {
		t.Fatalf("misses should net out the boost, got %v", m["auth.go::ValidateToken"])
	}
}

// --- skip-above drain on the session ---

func TestDrainSkippedNegatives(t *testing.T) {
	ss := newSessionState()
	ss.recordLastSearch("auth token", []string{"a", "b", "c", "d"})
	// Agent consumes rank 2 (c) — ranks 0,1 (a,b) were skipped over.
	if q := ss.attributedQuery("c"); q != "auth token" {
		t.Fatalf("attributedQuery(c) = %q", q)
	}
	q, skipped := ss.drainSkippedNegatives()
	if q != "auth token" {
		t.Fatalf("drain query = %q", q)
	}
	want := map[string]bool{"a": true, "b": true}
	if len(skipped) != 2 || !want[skipped[0]] || !want[skipped[1]] {
		t.Fatalf("skipped = %v, want a,b", skipped)
	}
}

func TestDrainNoSkipOnTopPick(t *testing.T) {
	ss := newSessionState()
	ss.recordLastSearch("q", []string{"a", "b", "c"})
	ss.attributedQuery("a") // consumed the top hit
	if _, skipped := ss.drainSkippedNegatives(); len(skipped) != 0 {
		t.Fatalf("top pick should yield no skips, got %v", skipped)
	}
}

func TestDrainNoConsumptionNoSkip(t *testing.T) {
	ss := newSessionState()
	ss.recordLastSearch("q", []string{"a", "b", "c"})
	if _, skipped := ss.drainSkippedNegatives(); len(skipped) != 0 {
		t.Fatalf("no consumption should yield no skips, got %v", skipped)
	}
}

func TestAttributedConsumptionBatch(t *testing.T) {
	ss := newSessionState()
	ss.recordLastSearch("auth", []string{"a", "b", "c"})
	q, matched := ss.attributedConsumptionBatch([]string{"b", "c", "zzz"})
	if q != "auth" {
		t.Fatalf("query = %q", q)
	}
	if len(matched) != 2 {
		t.Fatalf("matched = %v, want [b c]", matched)
	}
	// zzz wasn't returned, so it isn't credited.
	for _, m := range matched {
		if m == "zzz" {
			t.Fatal("zzz must not be credited")
		}
	}
}

func TestAttributedConsumptionBatchStale(t *testing.T) {
	ss := newSessionState()
	ss.recordLastSearch("auth", []string{"a"})
	ss.mu.Lock()
	ss.lastSearch.at = time.Now().Add(-2 * comboWindow)
	ss.mu.Unlock()
	if q, matched := ss.attributedConsumptionBatch([]string{"a"}); q != "" || matched != nil {
		t.Fatalf("stale search must not attribute, got q=%q matched=%v", q, matched)
	}
}
