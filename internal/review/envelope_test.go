package review

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/analysis"
)

func TestAggregateWorstOf(t *testing.T) {
	cases := []struct {
		name string
		envs []Envelope
		want Verdict
	}{
		{
			name: "all info -> approve",
			envs: []Envelope{
				{Findings: []Finding{{Severity: SevInfo}, {Severity: SevInfo}}},
			},
			want: VerdictApprove,
		},
		{
			name: "warning present -> review",
			envs: []Envelope{
				{Findings: []Finding{{Severity: SevInfo}}},
				{Findings: []Finding{{Severity: SevWarning}}},
			},
			want: VerdictReview,
		},
		{
			name: "error present -> block",
			envs: []Envelope{
				{Findings: []Finding{{Severity: SevWarning}}},
				{Findings: []Finding{{Severity: SevError}}},
			},
			want: VerdictBlock,
		},
		{
			name: "critical present -> block",
			envs: []Envelope{
				{Findings: []Finding{{Severity: SevCritical}}},
			},
			want: VerdictBlock,
		},
		{
			name: "no findings -> approve",
			envs: nil,
			want: VerdictApprove,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Aggregate(tc.envs...)
			if got.Verdict != tc.want {
				t.Errorf("verdict = %s, want %s", got.Verdict, tc.want)
			}
		})
	}
}

func TestAggregateStatsRecount(t *testing.T) {
	a := Envelope{Findings: []Finding{
		{Severity: SevCritical},
		{Severity: SevWarning},
	}}
	b := Envelope{Findings: []Finding{
		{Severity: SevWarning},
		{Severity: SevInfo},
		{Severity: SevError},
	}}
	got := Aggregate(a, b)

	if len(got.Findings) != 5 {
		t.Fatalf("union findings = %d, want 5", len(got.Findings))
	}
	want := map[Severity]int{
		SevCritical: 1,
		SevError:    1,
		SevWarning:  2,
		SevInfo:     1,
	}
	if !reflect.DeepEqual(got.Stats, want) {
		t.Errorf("stats = %v, want %v", got.Stats, want)
	}
	if got.Verdict != VerdictBlock {
		t.Errorf("verdict = %s, want BLOCK", got.Verdict)
	}
}

func TestEnvelopeJSONShape(t *testing.T) {
	e := Envelope{
		Tool:    "review_pack",
		Verdict: VerdictBlock,
		Summary: "1 critical",
		Findings: []Finding{{
			Rule: "nil-deref", Severity: SevCritical, Message: "m",
			File: "pkg/x.go", Line: 14, SymbolID: "pkg/x.go::Foo",
			Category: "nil-deref", Confidence: 0.9,
		}},
		Stats: map[Severity]int{SevCritical: 1},
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Tool != "review_pack" || back.Verdict != VerdictBlock {
		t.Errorf("round-trip mismatch: %+v", back)
	}
	if back.Stats[SevCritical] != 1 {
		t.Errorf("stats round-trip mismatch: %+v", back.Stats)
	}
}

func TestToFindingRows(t *testing.T) {
	e := Envelope{Findings: []Finding{
		{Rule: "r1", Severity: SevError, Message: "m1", File: "a.go", Line: 1, Category: "c1", CWE: "CWE-1"},
		{Rule: "r2", Severity: SevInfo, Message: "m2", File: "b.go", Line: 2, Category: "c2"},
	}}
	rows := e.ToFindingRows()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Rule != "r1" || rows[0].Severity != "error" || rows[0].File != "a.go" || rows[0].Line != 1 || rows[0].CWE != "CWE-1" {
		t.Errorf("row[0] mismatch: %+v", rows[0])
	}
	if rows[1].Severity != "info" || rows[1].Category != "c2" {
		t.Errorf("row[1] mismatch: %+v", rows[1])
	}
}

func TestFromRisk(t *testing.T) {
	cases := []struct {
		risk analysis.RiskLevel
		want Verdict
	}{
		{analysis.RiskCritical, VerdictBlock},
		{analysis.RiskHigh, VerdictBlock},
		{analysis.RiskMedium, VerdictReview},
		{analysis.RiskLow, VerdictApprove},
	}
	for _, tc := range cases {
		if got := FromRisk(tc.risk); got != tc.want {
			t.Errorf("FromRisk(%s) = %s, want %s", tc.risk, got, tc.want)
		}
	}
}

func TestFromSeverity(t *testing.T) {
	cases := []struct {
		sev  Severity
		want Verdict
	}{
		{SevCritical, VerdictBlock},
		{SevError, VerdictBlock},
		{SevWarning, VerdictReview},
		{SevInfo, VerdictApprove},
	}
	for _, tc := range cases {
		if got := FromSeverity(tc.sev); got != tc.want {
			t.Errorf("FromSeverity(%s) = %s, want %s", tc.sev, got, tc.want)
		}
	}
}
