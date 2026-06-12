package review

// Envelope is a single tool's verdict over a set of findings, with a recounted
// severity histogram.
type Envelope struct {
	Tool     string           `json:"tool"`
	Verdict  Verdict          `json:"verdict"`
	Summary  string           `json:"summary"`
	Findings []Finding        `json:"findings"`
	Stats    map[Severity]int `json:"stats"`
}

// statVerdict maps a severity histogram to the worst-of verdict: any
// critical/error → BLOCK; any warning → REVIEW; otherwise APPROVE.
func statVerdict(stats map[Severity]int) Verdict {
	if stats[SevCritical] > 0 || stats[SevError] > 0 {
		return VerdictBlock
	}
	if stats[SevWarning] > 0 {
		return VerdictReview
	}
	return VerdictApprove
}

// recount rebuilds the severity histogram from a finding slice.
func recount(findings []Finding) map[Severity]int {
	stats := map[Severity]int{
		SevCritical: 0,
		SevError:    0,
		SevWarning:  0,
		SevInfo:     0,
	}
	for _, f := range findings {
		stats[f.Severity]++
	}
	return stats
}

// Aggregate unions the findings of every envelope into one envelope, recounts
// the severity histogram over the union, and resolves the verdict worst-of
// across all inputs: any critical/error → BLOCK; any warning → REVIEW; else
// APPROVE.
func Aggregate(envs ...Envelope) Envelope {
	out := Envelope{Tool: "aggregate"}
	for _, e := range envs {
		out.Findings = append(out.Findings, e.Findings...)
	}
	out.Stats = recount(out.Findings)
	out.Verdict = statVerdict(out.Stats)
	return out
}

// FindingRow is the flat projection of a Finding used by the GCX / TOON
// encoders.
type FindingRow struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Category string `json:"category"`
	CWE      string `json:"cwe"`
}

// ToFindingRows projects the envelope's findings into the flat row form.
func (e Envelope) ToFindingRows() []FindingRow {
	rows := make([]FindingRow, 0, len(e.Findings))
	for _, f := range e.Findings {
		rows = append(rows, FindingRow{
			Rule:     f.Rule,
			Severity: string(f.Severity),
			Message:  f.Message,
			File:     f.File,
			Line:     f.Line,
			Category: f.Category,
			CWE:      f.CWE,
		})
	}
	return rows
}
