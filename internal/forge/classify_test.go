package forge

import (
	"testing"
	"time"
)

func TestClassifyStatus(t *testing.T) {
	now := time.Now()
	old := now.AddDate(0, 0, -45)
	recent := now.AddDate(0, 0, -2)

	tests := []struct {
		name         string
		pr           PR
		defaultBase  string
		wantState    string
		wantMismatch bool
		wantBlockers []string
	}{
		{
			name:      "draft wins over everything",
			pr:        PR{IsDraft: true, BaseRef: "feature", ReviewDecision: "CHANGES_REQUESTED", UpdatedAt: recent},
			wantState: "DRAFT",
			// base unset so no mismatch; blockers = draft + changes-requested
			wantBlockers: []string{"draft", "changes-requested"},
		},
		{
			name:         "base mismatch",
			pr:           PR{BaseRef: "develop", UpdatedAt: recent},
			defaultBase:  "main",
			wantState:    "BASE_MISMATCH",
			wantMismatch: true,
			wantBlockers: []string{"base-mismatch"},
		},
		{
			name:         "changes requested",
			pr:           PR{BaseRef: "main", ReviewDecision: "CHANGES_REQUESTED", UpdatedAt: recent},
			defaultBase:  "main",
			wantState:    "CHANGES_REQUESTED",
			wantBlockers: []string{"changes-requested"},
		},
		{
			name:        "approved",
			pr:          PR{BaseRef: "main", ReviewDecision: "APPROVED", UpdatedAt: recent},
			defaultBase: "main",
			wantState:   "APPROVED",
		},
		{
			name:        "stale by age",
			pr:          PR{BaseRef: "main", UpdatedAt: old},
			defaultBase: "main",
			wantState:   "STALE",
		},
		{
			name:        "ready",
			pr:          PR{BaseRef: "main", UpdatedAt: recent},
			defaultBase: "main",
			wantState:   "READY",
		},
		{
			name:         "ci failure adds a blocker",
			pr:           PR{BaseRef: "main", CIRollup: "FAILURE", UpdatedAt: recent},
			defaultBase:  "main",
			wantState:    "READY",
			wantBlockers: []string{"ci-failure"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyStatus(tt.pr, tt.defaultBase)
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}
			if got.BaseMismatch != tt.wantMismatch {
				t.Errorf("BaseMismatch = %v, want %v", got.BaseMismatch, tt.wantMismatch)
			}
			if tt.wantBlockers != nil {
				if !sameStringSet(got.Blockers, tt.wantBlockers) {
					t.Errorf("Blockers = %v, want %v", got.Blockers, tt.wantBlockers)
				}
			}
		})
	}
}

func TestRollupCI(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "NONE"},
		{"NONE", "NONE"},
		{"FAILURE", "FAILURE"},
		{"PENDING", "PENDING"},
		{"SUCCESS", "SUCCESS"},
		{"garbage", "NONE"},
	}
	for _, tt := range tests {
		if got := RollupCI(PR{CIRollup: tt.in}); got != tt.want {
			t.Errorf("RollupCI(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCollapseStates(t *testing.T) {
	tests := []struct {
		name   string
		states []string
		want   string
	}{
		{"empty", nil, "NONE"},
		{"all success", []string{"success", "success"}, "SUCCESS"},
		{"any failure", []string{"success", "failure"}, "FAILURE"},
		{"error counts as failure", []string{"success", "error"}, "FAILURE"},
		{"pending no failure", []string{"success", "pending"}, "PENDING"},
		{"in_progress is pending", []string{"in_progress"}, "PENDING"},
		{"failure beats pending", []string{"pending", "failure"}, "FAILURE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := collapseStates(tt.states); got != tt.want {
				t.Errorf("collapseStates(%v) = %q, want %q", tt.states, got, tt.want)
			}
		})
	}
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
