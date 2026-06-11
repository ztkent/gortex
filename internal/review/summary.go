package review

import (
	"fmt"
	"sort"
	"strings"
)

// Audience selects how a ReviewReport is rendered to text. The human audience
// gets a readable section packet (verdict header, per-file grouped findings, a
// summary line, and the cost block); the agent audience gets a terse,
// machine-first summary with no narrative prose — a one-line verdict, one
// compact line per finding, and a single cost line — so a coding agent shelling
// the review verb can parse the output cheaply and act on it.
type Audience int

const (
	// AudienceHuman renders the readable section packet. This is the default.
	AudienceHuman Audience = iota
	// AudienceAgent renders the terse machine-first summary.
	AudienceAgent
)

// RenderSummary renders a review report to text for the given audience.
//
// AudienceAgent produces a terse, prose-free summary:
//
//	VERDICT: block (1 critical, 2 error)
//	findings:
//	  internal/x.go:14 critical nil-deref — possible nil dereference
//	  internal/y.go:7 error inverted-err — inverted error check
//	cost: in=1234 out=456 cache_r=2000 cache_w=0 usd=0.012000 elapsed=4.2s
//
// AudienceHuman produces the readable section packet: a verdict header, an
// optional one-line summary, the per-file risk ranking, the findings grouped by
// file and ordered by line, and the cost block.
//
// A nil report renders an empty APPROVE verdict for the requested audience so
// the caller never has to nil-check before formatting.
func RenderSummary(report *ReviewReport, audience Audience) string {
	if report == nil {
		report = &ReviewReport{Verdict: VerdictApprove}
	}
	if audience == AudienceAgent {
		return renderAgentSummary(report)
	}
	return renderHumanSummary(report)
}

// renderAgentSummary is the terse, machine-first rendering: a single verdict
// line, one compact line per finding, and one cost line. It carries no narrative
// prose — no section headers, no sentences — so an agent can parse it cheaply.
func renderAgentSummary(report *ReviewReport) string {
	var b strings.Builder

	verdict := report.Verdict
	if verdict == "" {
		verdict = VerdictApprove
	}
	fmt.Fprintf(&b, "VERDICT: %s", strings.ToLower(string(verdict)))
	if hist := severityHistogram(report.Findings); hist != "" {
		fmt.Fprintf(&b, " (%s)", hist)
	}
	b.WriteByte('\n')

	if len(report.Findings) > 0 {
		b.WriteString("findings:\n")
		for _, f := range sortedFindings(report.Findings) {
			fmt.Fprintf(&b, "  %s\n", agentFindingLine(f))
		}
	}

	b.WriteString(costLine(report.Cost))
	b.WriteByte('\n')
	return b.String()
}

// agentFindingLine formats one finding as a single compact, prose-free line:
//
//	file:line severity rule — message
//
// The rule falls back to the category, then to "rule" so the field is never
// empty. The message is the short headline; the full markdown body is never
// emitted in the terse form.
func agentFindingLine(f Finding) string {
	file := cleanPath(f.File)
	if file == "" {
		file = "?"
	}
	line := findingLine(f)
	rule := f.Rule
	if rule == "" {
		rule = f.Category
	}
	if rule == "" {
		rule = "rule"
	}
	sev := f.Severity
	if sev == "" {
		sev = SevInfo
	}
	msg := strings.TrimSpace(firstLine(f.Message))
	if msg == "" {
		msg = strings.TrimSpace(firstLine(f.Body))
	}
	return fmt.Sprintf("%s:%d %s %s — %s", file, line, sev, rule, msg)
}

// renderHumanSummary is the readable section packet: a verdict header, an
// optional summary line, the per-file risk ranking, and the findings grouped by
// file and ordered by line, followed by the cost block.
func renderHumanSummary(report *ReviewReport) string {
	var b strings.Builder

	verdict := report.Verdict
	if verdict == "" {
		verdict = VerdictApprove
	}
	fmt.Fprintf(&b, "Verdict: %s\n", verdict)
	if s := strings.TrimSpace(report.Summary); s != "" {
		fmt.Fprintf(&b, "%s\n", s)
	}

	if len(report.FileRisk) > 0 {
		b.WriteString("\nFile risk:\n")
		for _, fr := range report.FileRisk {
			fmt.Fprintf(&b, "  %-8s %s (%d finding(s))", fr.Risk, fr.File, fr.Findings)
			// Evidence, when the impact analysis supplied it: the blast
			// radius behind the tier, and — when the graph indexes tests
			// at all — whether tests stand behind the change.
			if fr.Affected > 0 || fr.Symbols > 0 {
				fmt.Fprintf(&b, " — %d affected", fr.Affected)
				if fr.Symbols > 0 {
					if fr.Uncovered > 0 {
						fmt.Fprintf(&b, ", %d/%d changed symbols untested", fr.Uncovered, fr.Symbols)
					} else {
						fmt.Fprintf(&b, ", all %d changed symbols test-covered ✓", fr.Symbols)
					}
				}
			}
			b.WriteByte('\n')
		}
	}

	if len(report.Findings) == 0 {
		b.WriteString("\n✓ No inline findings — the deterministic rulepack passed.\n")
	} else {
		fmt.Fprintf(&b, "\nFindings (%d):\n", len(report.Findings))
		for _, file := range findingFilesSorted(report.Findings) {
			fmt.Fprintf(&b, "\n%s\n", file)
			for _, f := range findingsForFile(report.Findings, file) {
				rule := f.Rule
				if f.Category != "" {
					rule = strings.TrimSpace(f.Category + "/" + f.Rule)
				}
				fmt.Fprintf(&b, "  L%-5d %-8s %s", findingLine(f), f.Severity, firstLine(f.Message))
				if rule != "" {
					fmt.Fprintf(&b, "  [%s]", strings.Trim(rule, "/"))
				}
				b.WriteByte('\n')
			}
		}
	}

	b.WriteString("\n")
	b.WriteString(costLine(report.Cost))
	b.WriteByte('\n')
	return b.String()
}

// severityHistogram renders the kept-finding severity counts worst-first as a
// compact comma-joined string, e.g. "1 critical, 2 error". An empty finding set
// yields the empty string so the verdict line omits the parenthetical.
func severityHistogram(findings []Finding) string {
	counts := map[Severity]int{}
	for _, f := range findings {
		sev := f.Severity
		if sev == "" {
			sev = SevInfo
		}
		counts[sev]++
	}
	order := []Severity{SevCritical, SevError, SevWarning, SevInfo}
	parts := make([]string, 0, len(order))
	for _, sev := range order {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, sev))
		}
	}
	return strings.Join(parts, ", ")
}

// costLine renders the per-review token + USD accounting as a single line:
//
//	cost: in=1234 out=456 cache_r=2000 cache_w=0 usd=0.012000 elapsed=4.2s
//
// A nil cost (the deterministic-only / legacy path) renders the zero block so
// the line is always present and parseable.
func costLine(cost *CostBreakdown) string {
	var c CostBreakdown
	if cost != nil {
		c = *cost
	}
	elapsed := float64(c.ElapsedMs) / 1000.0
	return fmt.Sprintf("cost: in=%d out=%d cache_r=%d cache_w=%d usd=%.6f elapsed=%.1fs",
		c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheWriteTokens, c.USD, elapsed)
}

// findingLine returns the new-side line a finding is anchored to, falling back
// to StartLine when the grounder left Line at zero (a multi-line anchor).
func findingLine(f Finding) int {
	if f.Line != 0 {
		return f.Line
	}
	return f.StartLine
}

// firstLine returns the first line of a possibly multi-line string, trimmed of
// trailing space, so a finding's headline never bleeds the verdict / findings
// block onto extra lines.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimRight(s[:i], " \t\r")
	}
	return s
}

// sortedFindings returns the findings ordered worst-first (severity desc, then
// confidence desc, then file/line) without mutating the input slice — the same
// ranking the Gate applies, so the terse list reads most-severe first.
func sortedFindings(findings []Finding) []Finding {
	out := make([]Finding, len(findings))
	copy(out, findings)
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := severityRank(out[i].Severity), severityRank(out[j].Severity)
		if si != sj {
			return si > sj
		}
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		if fi, fj := cleanPath(out[i].File), cleanPath(out[j].File); fi != fj {
			return fi < fj
		}
		return findingLine(out[i]) < findingLine(out[j])
	})
	return out
}

// findingFilesSorted returns the distinct files the findings touch, in sorted
// order, so the human packet reads as a deterministic per-file pass.
func findingFilesSorted(findings []Finding) []string {
	seen := map[string]bool{}
	files := make([]string, 0)
	for _, f := range findings {
		file := cleanPath(f.File)
		if file == "" {
			file = "?"
		}
		if !seen[file] {
			seen[file] = true
			files = append(files, file)
		}
	}
	sort.Strings(files)
	return files
}

// findingsForFile returns the findings anchored to one file, ordered by line, so
// each file's block reads top-to-bottom.
func findingsForFile(findings []Finding, file string) []Finding {
	out := make([]Finding, 0)
	for _, f := range findings {
		ff := cleanPath(f.File)
		if ff == "" {
			ff = "?"
		}
		if ff == file {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return findingLine(out[i]) < findingLine(out[j])
	})
	return out
}
