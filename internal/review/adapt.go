package review

import "github.com/zzet/gortex/internal/analysis"

// FromRisk bridges the impact-analysis risk ladder into a review verdict:
// CRITICAL / HIGH → BLOCK, MEDIUM → REVIEW, LOW → APPROVE.
func FromRisk(r analysis.RiskLevel) Verdict {
	switch r {
	case analysis.RiskCritical, analysis.RiskHigh:
		return VerdictBlock
	case analysis.RiskMedium:
		return VerdictReview
	default:
		return VerdictApprove
	}
}

// FromSeverity bridges a single finding's severity into a verdict:
// critical / error → BLOCK, warning → REVIEW, info → APPROVE.
func FromSeverity(s Severity) Verdict {
	switch s {
	case SevCritical, SevError:
		return VerdictBlock
	case SevWarning:
		return VerdictReview
	default:
		return VerdictApprove
	}
}
