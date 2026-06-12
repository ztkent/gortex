package review

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CritiqueVerdict is the second-pass adjudication of a single prior finding:
// whether the critique keeps it, drops it as a false positive / low-value, or
// is uncertain. The default — applied whenever the model is silent, unavailable,
// or ambiguous — is CritiqueKeep, so a critique pass can only ever remove a
// finding on an explicit, reasoned drop.
type CritiqueVerdict string

const (
	// CritiqueKeep retains the finding. The conservative default.
	CritiqueKeep CritiqueVerdict = "keep"
	// CritiqueDrop removes the finding as a false positive or low-value noise.
	CritiqueDrop CritiqueVerdict = "drop"
	// CritiqueUncertain flags a finding the critique could not confidently
	// adjudicate. Like keep it never removes the finding — it only annotates
	// the doubt so a human reviewer can look closer.
	CritiqueUncertain CritiqueVerdict = "uncertain"
)

// CritiquedFinding pairs a prior finding with the second pass's verdict over it
// and the reason the model gave. The finding itself is carried verbatim so the
// caller can re-emit the kept set or render the dropped set with its reasons.
type CritiquedFinding struct {
	Finding Finding         `json:"finding"`
	Verdict CritiqueVerdict `json:"critique_verdict"`
	Reason  string          `json:"critique_reason,omitempty"`
}

// CritiqueResult is the outcome of a self-critique pass over a prior review's
// findings: the kept findings (the filtered set the caller should keep acting
// on), the annotated drops (each with the reason it was removed), the count of
// uncertain-but-kept findings, and the revised worst-of verdict recomputed over
// the kept set. Summary is a one-line headline.
type CritiqueResult struct {
	Kept      []Finding          `json:"kept"`
	Dropped   []CritiquedFinding `json:"dropped"`
	Annotated []CritiquedFinding `json:"annotated"`
	Uncertain int                `json:"uncertain"`
	Verdict   Verdict            `json:"verdict"`
	Summary   string             `json:"summary"`
	// LLMUsed reports whether the critique LLM pass actually ran (a configured,
	// non-nil gen that produced a parseable response). False means the result is
	// the no-op pass-through: every finding kept, nothing dropped.
	LLMUsed bool `json:"llm_used"`
}

// critiqueVerdictRow is one finding's adjudication as the model is asked to
// return it. The Index ties the verdict back to the input finding's position so
// the model never has to echo the whole finding body.
type critiqueVerdictRow struct {
	Index   int    `json:"index"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// Critique runs a second, adversarial LLM pass over a prior review's findings.
// It asks the model — grounded in the changeset diff and any per-finding source
// context already on each Finding — to decide which findings are false positives
// or low-value. Each finding is annotated with a keep / drop / uncertain verdict
// plus a reason; only explicit drops are removed. The pass is TOTAL: a nil /
// disabled gen, a failing model, or an unparseable response is a no-op
// pass-through — every finding is kept, no error is returned, and LLMUsed is
// false. maxTokens caps the critique request; <= 0 falls back to a sane default.
func Critique(ctx context.Context, gen LLMGen, findings []Finding, diff string, maxTokens int) CritiqueResult {
	// Default to the conservative pass-through so the no-op / unavailable path is
	// indistinguishable from "the model kept everything".
	res := CritiqueResult{
		Kept:      append([]Finding(nil), findings...),
		Dropped:   []CritiquedFinding{},
		Annotated: make([]CritiquedFinding, 0, len(findings)),
		Verdict:   verdictForFindings(findings),
	}
	for _, f := range findings {
		res.Annotated = append(res.Annotated, CritiquedFinding{Finding: f, Verdict: CritiqueKeep})
	}
	res.Summary = critiqueSummary(res)

	if gen == nil || len(findings) == 0 {
		return res
	}

	if maxTokens <= 0 {
		maxTokens = defaultMaxLLMTokens
	}
	out, err := gen(ctx, BuildCritiquePrompt(findings, diff), maxTokens)
	if err != nil {
		// A failing model is not an error for the pass — degrade to pass-through.
		return res
	}
	rows, ok := parseCritiqueRows(out)
	if !ok {
		return res
	}

	return applyCritique(findings, rows)
}

// applyCritique folds the model's per-finding verdicts into the result: an
// explicit drop removes the finding (and records it with its reason); keep and
// uncertain both retain it; any finding the model did not adjudicate defaults to
// keep. The kept set's worst-of verdict is recomputed so a critique that drops
// the only error downgrades BLOCK → REVIEW/APPROVE honestly.
func applyCritique(findings []Finding, rows []critiqueVerdictRow) CritiqueResult {
	byIndex := make(map[int]critiqueVerdictRow, len(rows))
	for _, r := range rows {
		// Last write wins for a duplicated index; harmless and deterministic.
		byIndex[r.Index] = r
	}

	res := CritiqueResult{
		Kept:      make([]Finding, 0, len(findings)),
		Dropped:   []CritiquedFinding{},
		Annotated: make([]CritiquedFinding, 0, len(findings)),
		LLMUsed:   true,
	}
	for i, f := range findings {
		verdict, reason := CritiqueKeep, ""
		if r, found := byIndex[i]; found {
			verdict = normalizeCritiqueVerdict(r.Verdict)
			reason = strings.TrimSpace(r.Reason)
		}
		annotated := CritiquedFinding{Finding: f, Verdict: verdict, Reason: reason}
		res.Annotated = append(res.Annotated, annotated)
		switch verdict {
		case CritiqueDrop:
			res.Dropped = append(res.Dropped, annotated)
		case CritiqueUncertain:
			res.Uncertain++
			res.Kept = append(res.Kept, f)
		default: // keep
			res.Kept = append(res.Kept, f)
		}
	}

	res.Verdict = verdictForFindings(res.Kept)
	res.Summary = critiqueSummary(res)
	return res
}

// normalizeCritiqueVerdict maps a model-returned verdict token to a known
// CritiqueVerdict, defaulting to keep for anything unrecognised — so an
// ambiguous or malformed token can never silently drop a finding.
func normalizeCritiqueVerdict(s string) CritiqueVerdict {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "drop", "false_positive", "false-positive", "remove", "discard":
		return CritiqueDrop
	case "uncertain", "unsure", "maybe", "unknown":
		return CritiqueUncertain
	default:
		return CritiqueKeep
	}
}

// verdictForFindings recomputes the worst-of verdict over a finding set: any
// critical/error → BLOCK, any warning → REVIEW, else APPROVE. Shared with the
// envelope's statVerdict ladder so the critique never invents a second ladder.
func verdictForFindings(findings []Finding) Verdict {
	return statVerdict(recount(findings))
}

// critiqueSummary renders the one-line headline of a critique pass.
func critiqueSummary(res CritiqueResult) string {
	if len(res.Dropped) == 0 {
		return fmt.Sprintf("%s: kept all %d finding(s)", res.Verdict, len(res.Kept))
	}
	return fmt.Sprintf("%s: dropped %d false-positive / low-value finding(s), kept %d",
		res.Verdict, len(res.Dropped), len(res.Kept))
}

// BuildCritiquePrompt assembles the adversarial second-pass prompt: the change
// diff as grounding, the numbered prior findings (with each finding's flagged
// source line when the generator carried one), and an instruction to return a
// strict JSON array of {index, verdict, reason} adjudications. The index ties a
// verdict back to its finding so the model never re-emits a finding body.
func BuildCritiquePrompt(findings []Finding, diff string) string {
	var b strings.Builder
	b.WriteString("You are a senior reviewer performing an adversarial SECOND PASS over an automated code review.\n")
	b.WriteString("For EACH prior finding below, decide whether it is a genuine issue worth a reviewer's attention, ")
	b.WriteString("a false positive, or low-value noise. Be skeptical: drop findings that the diff does not actually support, ")
	b.WriteString("that restate a non-issue, or that are too speculative to act on. Keep findings that point at a real defect.\n\n")

	if strings.TrimSpace(diff) != "" {
		b.WriteString("CHANGESET DIFF (grounding):\n")
		b.WriteString(strings.TrimSpace(diff))
		b.WriteString("\n\n")
	}

	b.WriteString("PRIOR FINDINGS:\n")
	for i, f := range findings {
		fmt.Fprintf(&b, "[%d] rule=%s severity=%s category=%s file=%s line=%d\n",
			i, valueOr(f.Rule, "-"), valueOr(string(f.Severity), "-"),
			valueOr(f.Category, "-"), valueOr(f.File, "-"), f.Line)
		if msg := strings.TrimSpace(f.Message); msg != "" {
			fmt.Fprintf(&b, "    message: %s\n", msg)
		}
		if src := strings.TrimSpace(f.SourceLine); src != "" {
			fmt.Fprintf(&b, "    flagged source: %s\n", src)
		}
	}

	b.WriteString("\nRespond with ONLY a JSON array, one object per finding, no prose:\n")
	b.WriteString(`[{"index":0,"verdict":"keep|drop|uncertain","reason":"<one short sentence>"}]` + "\n")
	b.WriteString("Use \"drop\" only when you are confident the finding is a false positive or low-value. ")
	b.WriteString("Use \"uncertain\" when you cannot tell. When in doubt, \"keep\".")
	return b.String()
}

// parseCritiqueRows extracts the model's adjudication array from a freeform
// response. It tolerates surrounding prose / code fences by slicing the first
// top-level JSON array out of the text before unmarshalling. Returns ok=false
// when no array parses — the caller then degrades to the keep-everything
// pass-through.
func parseCritiqueRows(out string) ([]critiqueVerdictRow, bool) {
	raw := extractJSONArray(out)
	if raw == "" {
		return nil, false
	}
	var rows []critiqueVerdictRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, false
	}
	return rows, true
}

// extractJSONArray returns the substring spanning the first balanced top-level
// JSON array in s, or "" when none is present. It ignores brackets inside
// double-quoted strings (honouring backslash escapes) so a "[" in a reason
// string does not confuse the scan.
func extractJSONArray(s string) string {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// valueOr returns v, or fallback when v is empty after trimming.
func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// SortCritiquedBySeverity orders critiqued findings worst-severity-first for a
// deterministic dropped-list rendering.
func SortCritiquedBySeverity(rows []CritiquedFinding) {
	sort.SliceStable(rows, func(i, j int) bool {
		si, sj := severityRank(rows[i].Finding.Severity), severityRank(rows[j].Finding.Severity)
		if si != sj {
			return si > sj
		}
		if rows[i].Finding.File != rows[j].Finding.File {
			return rows[i].Finding.File < rows[j].Finding.File
		}
		return rows[i].Finding.Line < rows[j].Finding.Line
	})
}
