package forge

import "time"

// staleAfterDays is the age past which an open PR is classified STALE
// (when nothing more pressing applies).
const staleAfterDays = 30

// Status is the pure-Go classification of a PR against a default base.
// It carries no network dependency and is table-testable.
type Status struct {
	State        string
	BaseMismatch bool
	Draft        bool
	AgeDays      int
	Blockers     []string
}

// ClassifyStatus reduces a PR to a single review-state label, computed
// purely from its already-fetched fields against the repo's default base.
// State precedence: DRAFT → BASE_MISMATCH → CHANGES_REQUESTED → APPROVED
// → STALE → READY. Blockers accumulates every condition that would hold
// a merge.
func ClassifyStatus(pr PR, defaultBase string) Status {
	s := Status{
		Draft:   pr.IsDraft,
		AgeDays: ageDays(pr.UpdatedAt),
	}
	if defaultBase != "" && pr.BaseRef != "" && pr.BaseRef != defaultBase {
		s.BaseMismatch = true
	}

	if pr.IsDraft {
		s.Blockers = append(s.Blockers, "draft")
	}
	if s.BaseMismatch {
		s.Blockers = append(s.Blockers, "base-mismatch")
	}
	if pr.ReviewDecision == "CHANGES_REQUESTED" {
		s.Blockers = append(s.Blockers, "changes-requested")
	}
	if RollupCI(pr) == "FAILURE" {
		s.Blockers = append(s.Blockers, "ci-failure")
	}

	switch {
	case pr.IsDraft:
		s.State = "DRAFT"
	case s.BaseMismatch:
		s.State = "BASE_MISMATCH"
	case pr.ReviewDecision == "CHANGES_REQUESTED":
		s.State = "CHANGES_REQUESTED"
	case pr.ReviewDecision == "APPROVED":
		s.State = "APPROVED"
	case s.AgeDays >= staleAfterDays:
		s.State = "STALE"
	default:
		s.State = "READY"
	}
	return s
}

// RollupCI echoes a PR's reconstructed CI rollup, normalized to one of
// NONE / FAILURE / PENDING / SUCCESS. An empty rollup reads as NONE.
func RollupCI(pr PR) string {
	switch pr.CIRollup {
	case "FAILURE", "PENDING", "SUCCESS":
		return pr.CIRollup
	default:
		return "NONE"
	}
}

// ageDays returns the whole-day age of t relative to now, clamped at 0
// for a zero or future timestamp.
func ageDays(t time.Time) int {
	if t.IsZero() {
		return 0
	}
	d := time.Since(t)
	if d < 0 {
		return 0
	}
	return int(d.Hours() / 24)
}
