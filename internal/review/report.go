package review

import (
	"sort"

	"github.com/zzet/gortex/internal/analysis"
)

// ReviewReport is the output of the hybrid review flow: a worst-of verdict over
// the merged deterministic + LLM findings, the per-file risk ranking, a one-line
// summary, and the run statistics. The findings are already line-anchored (the
// relocation phase ran) and deduplicated.
type ReviewReport struct {
	Verdict  Verdict     `json:"verdict"`
	Findings []Finding   `json:"findings"`
	FileRisk []FileRisk  `json:"file_risk"`
	Summary  string      `json:"summary"`
	Stats    ReviewStats `json:"stats"`
	// Cost is the per-review token + USD accounting. Set only when the
	// review ran through a usage-aware LLM seam (RunWithUsage); nil for
	// the deterministic-only or legacy Run path.
	Cost *CostBreakdown `json:"cost,omitempty"`
}

// FileRisk ranks one changed file by its impact-derived risk tier and the number
// of findings anchored to it.
type FileRisk struct {
	File     string `json:"file"`
	Risk     string `json:"risk"`
	Findings int    `json:"findings"`
}

// ReviewStats records how the report was assembled: how many findings each
// source contributed and how many LLM candidates were dropped because no exact
// line could be grounded.
type ReviewStats struct {
	Rulepack     int            `json:"rulepack"`      // deterministic rule findings merged in
	LLM          int            `json:"llm"`           // LLM findings that anchored to a line
	Dropped      int            `json:"dropped"`       // LLM candidates dropped (unresolved anchor)
	Total        int            `json:"total"`         // findings in the report after dedup
	BySeverity   map[string]int `json:"by_severity"`   // severity histogram of the report
	Truncated    bool           `json:"truncated"`     // a budget bound dropped some candidates
	LLMRequested bool           `json:"llm_requested"` // the LLM phase was asked to run
	Gate         GateStats      `json:"gate"`          // confidence / severity / category / cap suppression summary
}

// computeVerdict is the worst-of verdict over the report's findings and per-file
// risk: any error/critical finding (or a critical-risk file) → BLOCK; any
// warning finding (or a high/medium-risk file) → REVIEW; otherwise APPROVE. It
// reuses the FromSeverity / FromRisk adapters so the ladder is never
// re-implemented here.
func computeVerdict(findings []Finding, fileRisk []FileRisk) Verdict {
	worst := VerdictApprove
	for _, f := range findings {
		worst = worseVerdict(worst, FromSeverity(f.Severity))
	}
	for _, fr := range fileRisk {
		worst = worseVerdict(worst, FromRisk(analysis.RiskLevel(fr.Risk)))
	}
	return worst
}

// verdictRank orders the verdicts so "worst-of" is a max over the rank.
func verdictRank(v Verdict) int {
	switch v {
	case VerdictBlock:
		return 2
	case VerdictReview:
		return 1
	default:
		return 0
	}
}

// worseVerdict returns the more severe of two verdicts.
func worseVerdict(a, b Verdict) Verdict {
	if verdictRank(b) > verdictRank(a) {
		return b
	}
	return a
}

// rankFileRisk derives the per-file risk ranking from the impact analysis and the
// findings anchored to each changed file. A file's risk tier is the impact tier
// of the worst changed symbol it contains; with no impact data it falls back to a
// finding-count heuristic via the same assessRisk ladder. The result is sorted
// worst-first, then by file for determinism.
func rankFileRisk(diff *analysis.DiffResult, impact map[string]*analysis.ImpactResult, findings []Finding) []FileRisk {
	byFile := map[string]string{}
	findingCount := map[string]int{}

	for _, f := range findings {
		if f.File != "" {
			findingCount[f.File]++
		}
	}

	if diff != nil {
		for _, cs := range diff.ChangedSymbols {
			file := cleanPath(cs.FilePath)
			if file == "" {
				continue
			}
			risk := string(analysis.RiskLow)
			if impact != nil {
				if ir := impact[cs.ID]; ir != nil && ir.Risk != "" {
					risk = string(ir.Risk)
				}
			}
			byFile[file] = worseRisk(byFile[file], risk)
		}
		for _, file := range diff.ChangedFiles {
			file = cleanPath(file)
			if file == "" {
				continue
			}
			if _, ok := byFile[file]; !ok {
				byFile[file] = ""
			}
		}
	}

	// Files that carry findings but no diff/impact entry still get a row.
	for file := range findingCount {
		if _, ok := byFile[file]; !ok {
			byFile[file] = ""
		}
	}

	rows := make([]FileRisk, 0, len(byFile))
	for file, risk := range byFile {
		if risk == "" {
			risk = string(riskFromFindingCount(findingCount[file]))
		}
		rows = append(rows, FileRisk{File: file, Risk: risk, Findings: findingCount[file]})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := riskRank(rows[i].Risk), riskRank(rows[j].Risk)
		if ri != rj {
			return ri > rj
		}
		return rows[i].File < rows[j].File
	})
	return rows
}

// riskFromFindingCount maps the number of findings anchored to a file onto the
// impact-risk ladder so a file with no graph impact still ranks by how much the
// review flagged in it.
func riskFromFindingCount(n int) analysis.RiskLevel {
	switch {
	case n >= 5:
		return analysis.RiskHigh
	case n >= 2:
		return analysis.RiskMedium
	case n >= 1:
		return analysis.RiskLow
	default:
		return analysis.RiskLow
	}
}

func riskRank(r string) int {
	switch analysis.RiskLevel(r) {
	case analysis.RiskCritical:
		return 3
	case analysis.RiskHigh:
		return 2
	case analysis.RiskMedium:
		return 1
	default:
		return 0
	}
}

func worseRisk(a, b string) string {
	if riskRank(b) > riskRank(a) {
		return b
	}
	return a
}
