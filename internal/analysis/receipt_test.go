package analysis

import (
	"encoding/json"
	"strings"
	"testing"
)

// sampleRisk is a synthetic PR-risk result carrying values that would otherwise
// require a live graph — the receipt is a pure projection, so it can be fed a
// hand-built result directly.
func sampleRisk() PRRiskResult {
	return PRRiskResult{
		Score: 68.5,
		Risk:  RiskHigh,
		Factors: []PRRiskFactor{
			{Axis: "flow", Score: 72.0, Reason: "wide fan-in on internal/auth/login.go::ValidateToken"},
			{Axis: "coverage", Score: 61.0, Reason: "3 changed symbols with no covering test"},
		},
		TotalAffected:    37,
		UncoveredSymbols: 4,
		CommunitySpan:    3,
		SecurityHits:     []string{"auth"},
	}
}

func TestBuildReviewReceipt_ProjectsNamedFields(t *testing.T) {
	r := BuildReviewReceipt(sampleRisk(), "PENDING", false, false)

	if r.ReceiptVersion != 1 {
		t.Fatalf("receipt_version = %d, want 1", r.ReceiptVersion)
	}
	if r.RiskTier != RiskHigh {
		t.Fatalf("risk_tier = %q, want HIGH", r.RiskTier)
	}
	if r.AffectedCount != 37 {
		t.Fatalf("affected_count = %d, want 37", r.AffectedCount)
	}
	if r.UncoveredCount != 4 {
		t.Fatalf("uncovered_count = %d, want 4", r.UncoveredCount)
	}
	if r.CommunitySpan != 3 {
		t.Fatalf("community_span = %d, want 3", r.CommunitySpan)
	}
	if !r.SecurityFlagged {
		t.Fatalf("security_flagged = false, want true (auth hit)")
	}
	// uncovered>0 is the most urgent action and outranks span/security.
	if r.NextSafeAction != actionAddTests {
		t.Fatalf("next_safe_action = %q, want %q", r.NextSafeAction, actionAddTests)
	}
	// HIGH (not top tier) + PENDING CI + no breaking flag => not a blocker.
	if r.MergeBlocker {
		t.Fatalf("merge_blocker = true, want false for HIGH+PENDING")
	}
	if r.BlockerReason != "" {
		t.Fatalf("blocker_reason = %q, want empty for a non-blocked receipt", r.BlockerReason)
	}
	if len(r.TopFactors) != 2 {
		t.Fatalf("top_factors len = %d, want 2", len(r.TopFactors))
	}
	if r.TopFactors[0].Axis != "flow" || r.TopFactors[0].Score != 72.0 {
		t.Fatalf("top_factors[0] = %+v, want {flow 72}", r.TopFactors[0])
	}
}

func TestNextSafeAction_Table(t *testing.T) {
	cases := []struct {
		name string
		in   PRRiskResult
		want string
	}{
		{"uncovered wins over span and security",
			PRRiskResult{UncoveredSymbols: 1, CommunitySpan: 5, SecurityHits: []string{"auth"}},
			actionAddTests},
		{"high span when fully covered",
			PRRiskResult{UncoveredSymbols: 0, CommunitySpan: 3},
			actionSplitPR},
		{"security when covered and span-local",
			PRRiskResult{UncoveredSymbols: 0, CommunitySpan: 1, SecurityHits: []string{"token"}},
			actionReviewSecurity},
		{"merge-ready when clean",
			PRRiskResult{UncoveredSymbols: 0, CommunitySpan: 1},
			actionMergeReady},
		{"span below threshold falls through to merge-ready",
			PRRiskResult{UncoveredSymbols: 0, CommunitySpan: 2},
			actionMergeReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextSafeAction(tc.in); got != tc.want {
				t.Fatalf("nextSafeAction = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMergeBlocker_TopTierOrCIFailure(t *testing.T) {
	// CRITICAL is the top tier: a blocker regardless of CI.
	crit := sampleRisk()
	crit.Risk = RiskCritical
	if r := BuildReviewReceipt(crit, "SUCCESS", false, false); !r.MergeBlocker {
		t.Fatalf("merge_blocker = false for CRITICAL, want true")
	} else if r.BlockerReason != blockerCriticalRisk {
		t.Fatalf("blocker_reason = %q, want %q", r.BlockerReason, blockerCriticalRisk)
	}

	// Any tier below the top is NOT a blocker on its own.
	for _, tier := range []RiskLevel{RiskLow, RiskMedium, RiskHigh} {
		in := sampleRisk()
		in.Risk = tier
		if r := BuildReviewReceipt(in, "SUCCESS", false, false); r.MergeBlocker {
			t.Fatalf("merge_blocker = true for %s+SUCCESS, want false (only top tier blocks)", tier)
		}
	}

	// CI=FAILURE blocks even a LOW-risk PR.
	low := sampleRisk()
	low.Risk = RiskLow
	if r := BuildReviewReceipt(low, "FAILURE", false, false); !r.MergeBlocker {
		t.Fatalf("merge_blocker = false for CI FAILURE, want true")
	} else if r.BlockerReason != blockerCIFailure {
		t.Fatalf("blocker_reason = %q, want %q", r.BlockerReason, blockerCIFailure)
	}

	// An out-of-band breaking-change flag blocks too.
	if r := BuildReviewReceipt(low, "SUCCESS", true, false); !r.MergeBlocker {
		t.Fatalf("merge_blocker = false for breaking flag, want true")
	} else if r.BlockerReason != blockerBreakingChange {
		t.Fatalf("blocker_reason = %q, want %q", r.BlockerReason, blockerBreakingChange)
	}
}

func TestBuildReviewReceipt_ScrubRemovesContextKeepsVerdict(t *testing.T) {
	// A result whose factor reasons embed a path, a symbol ID, and an email —
	// the kinds of context a shared receipt must never leak.
	in := sampleRisk()
	in.Factors = []PRRiskFactor{
		{Axis: "internal/auth/login.go::ValidateToken", Score: 72.0, Reason: "owner me@zzet.org"},
		{Axis: "flow", Score: 61.0, Reason: "changed pkg/x.go::Foo"},
	}
	in.SecurityHits = []string{"auth"}

	r := BuildReviewReceipt(in, "PENDING", false, true)

	// The tier and the decision survive scrubbing.
	if r.RiskTier != RiskHigh {
		t.Fatalf("scrub dropped risk_tier: %q", r.RiskTier)
	}
	if r.NextSafeAction != actionAddTests {
		t.Fatalf("scrub dropped next_safe_action: %q", r.NextSafeAction)
	}

	// Serialize the whole receipt and assert ZERO path-like / ::-bearing /
	// email-like values leak anywhere in the JSON.
	blob, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(blob)
	if strings.Contains(s, "::") {
		t.Fatalf("scrubbed receipt leaks a symbol ID separator: %s", s)
	}
	if strings.Contains(s, "@") {
		t.Fatalf("scrubbed receipt leaks an email: %s", s)
	}
	for _, frag := range []string{".go", "internal/auth", "pkg/x", "me@zzet.org", "ValidateToken"} {
		if strings.Contains(s, frag) {
			t.Fatalf("scrubbed receipt leaks %q: %s", frag, s)
		}
	}
}

func TestReviewReceipt_JSONIsSnakeCaseAndStable(t *testing.T) {
	r := BuildReviewReceipt(sampleRisk(), "FAILURE", false, false)
	blob, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Every expected snake_case key must be present.
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(blob, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantKeys := []string{
		"receipt_version", "risk_tier", "next_safe_action", "merge_blocker",
		"blocker_reason", "affected_count", "uncovered_count", "community_span",
		"security_flagged", "top_factors",
	}
	for _, k := range wantKeys {
		if _, ok := generic[k]; !ok {
			t.Fatalf("missing snake_case key %q in %s", k, blob)
		}
	}
	if len(generic) != len(wantKeys) {
		t.Fatalf("unexpected key count: got %d keys %v, want %d", len(generic), generic, len(wantKeys))
	}

	// No camelCase / PascalCase keys slipped through.
	for k := range generic {
		if strings.ToLower(k) != k {
			t.Fatalf("key %q is not snake_case", k)
		}
	}

	// top_factors entries are themselves snake_case (axis/score).
	var factors []map[string]json.RawMessage
	if err := json.Unmarshal(generic["top_factors"], &factors); err != nil {
		t.Fatalf("unmarshal top_factors: %v", err)
	}
	if len(factors) == 0 {
		t.Fatal("top_factors must not be empty for this input")
	}
	for _, f := range factors {
		if _, ok := f["axis"]; !ok {
			t.Fatalf("top_factors entry missing axis: %v", f)
		}
		if _, ok := f["score"]; !ok {
			t.Fatalf("top_factors entry missing score: %v", f)
		}
		if _, leaked := f["reason"]; leaked {
			t.Fatalf("top_factors must not carry reason (privacy leak): %v", f)
		}
	}

	// Re-marshalling produces byte-identical output (stable shape).
	again, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(again) != string(blob) {
		t.Fatalf("JSON not stable:\n %s\n %s", blob, again)
	}
}
