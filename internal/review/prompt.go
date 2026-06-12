package review

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/config"
)

// promptInput is the deterministic grounding the main review prompt is built
// from. It is assembled by the PLAN phase and consumed by buildReviewPrompt — a
// pure function, so the prompt is fully testable without an LLM.
type promptInput struct {
	// Rules is the resolved review rule per changed file (file → rule), the
	// path-glob layer's verdict. It grounds the model in the convention that
	// governs each file (test rulepack vs general, severity floor, etc.).
	Rules map[string]config.ReviewRule
	// Pack is the tiered diff/source context the model reviews against.
	Pack *ReviewPack
	// Deterministic is the set of rule findings already produced by the
	// deterministic rulepack. They are handed to the model as established facts
	// so it complements rather than duplicates them.
	Deterministic []Finding
	// Deep, when true, grounds the prompt in the reference-only planner tool
	// catalogue — the deep path tells the model which graph analyses already
	// characterised the change so it can reason about them. The catalogue is
	// reference-only context, never a callable tool surface.
	Deep bool
}

// reviewCandidate is one finding the model is asked to emit, as parsed back out
// of its JSON reply. snippet is the verbatim code the finding is about, used by
// the relocation phase to anchor it to an exact line.
type reviewCandidate struct {
	File     string `json:"file"`
	Snippet  string `json:"snippet"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	Category string `json:"category"`
}

// buildReviewPrompt renders the rule-grounded main-review prompt. It is a pure
// function of its input: the resolved per-file rules, the deterministic findings
// already produced, and the tiered review pack. The output instructs the model
// to return strict JSON candidates carrying a verbatim snippet so the
// deterministic relocation phase can anchor each to an exact line.
func buildReviewPrompt(in promptInput) string {
	var b strings.Builder

	b.WriteString("You are a precise code reviewer. Review the change below and report ")
	b.WriteString("correctness, security, and idiomatic issues you are confident about.\n\n")

	// Deep path: ground the model in the reference-only planner catalogue so it
	// reasons about the graph analyses that already characterised the change.
	if in.Deep {
		if cat := renderPlannerCatalogue(); cat != "" {
			b.WriteString(cat)
			b.WriteByte('\n')
		}
	}

	// Rule grounding: the convention that governs each changed file.
	if len(in.Rules) > 0 {
		b.WriteString("## Review rules in effect (per file)\n")
		for _, file := range sortedRuleFiles(in.Rules) {
			r := in.Rules[file]
			fmt.Fprintf(&b, "- %s → rule %q", file, ruleLabel(r))
			if r.Rulepack != "" {
				fmt.Fprintf(&b, " (rulepack %s)", r.Rulepack)
			}
			if r.Severity != "" {
				fmt.Fprintf(&b, " (min severity %s)", r.Severity)
			}
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	// Established deterministic findings: complement, do not duplicate.
	if len(in.Deterministic) > 0 {
		b.WriteString("## Findings already reported by the deterministic rulepack\n")
		b.WriteString("Do not repeat these; only add issues they miss.\n")
		for _, f := range in.Deterministic {
			fmt.Fprintf(&b, "- [%s] %s:%d %s\n", f.Severity, f.File, f.Line, f.Message)
		}
		b.WriteByte('\n')
	}

	// The change itself, as the tiered review pack.
	if in.Pack != nil {
		b.WriteString("## Change under review\n")
		b.WriteString(in.Pack.Render())
		b.WriteByte('\n')
	}

	b.WriteString("## Output format\n")
	b.WriteString("Respond with ONLY a JSON array (no prose, no code fence). Each element:\n")
	b.WriteString(`{"file":"<path>","snippet":"<verbatim line of code the issue is about>",`)
	b.WriteString(`"message":"<concise issue>","severity":"info|warning|error|critical","category":"<short>"}` + "\n")
	b.WriteString("Use the EXACT file paths shown above. The snippet must be copied verbatim from the change so it can be located.\n")
	b.WriteString("If you find no issues, respond with an empty array: []\n")

	return b.String()
}

// ruleLabel is the human label for a resolved rule — its Name, falling back to
// its Rulepack, then to a generic marker.
func ruleLabel(r config.ReviewRule) string {
	switch {
	case r.Name != "":
		return r.Name
	case r.Rulepack != "":
		return r.Rulepack
	default:
		return "general"
	}
}

func sortedRuleFiles(rules map[string]config.ReviewRule) []string {
	files := make([]string, 0, len(rules))
	for f := range rules {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// parseCandidates extracts the JSON finding array from a model reply. It is
// lenient about surrounding prose / a ```json fence (models add them despite the
// instruction) by slicing to the outermost array. A reply with no array, or one
// that does not parse, yields no candidates and no error — the flow degrades to
// the deterministic findings rather than failing.
func parseCandidates(out string) []reviewCandidate {
	body := sliceJSONArray(out)
	if body == "" {
		return nil
	}
	var cands []reviewCandidate
	if err := json.Unmarshal([]byte(body), &cands); err != nil {
		return nil
	}
	cleaned := cands[:0]
	for _, c := range cands {
		c.File = strings.TrimSpace(c.File)
		c.Message = strings.TrimSpace(c.Message)
		if c.File == "" || c.Message == "" {
			continue
		}
		cleaned = append(cleaned, c)
	}
	return cleaned
}

// sliceJSONArray returns the substring from the first '[' to its matching ']',
// tolerating leading/trailing prose or a code fence around the array.
func sliceJSONArray(s string) string {
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// candidateToFinding turns a parsed LLM candidate into a Finding with the
// content fields populated (Source="llm"). Location fields are left for the
// relocation phase. The severity floor from the file's resolved rule is applied
// so a rule's `severity:` raises a weaker model verdict.
func candidateToFinding(c reviewCandidate, rule config.ReviewRule) Finding {
	sev := normalizeSeverity(c.Severity)
	if floor := normalizeSeverity(rule.Severity); severityRank(floor) > severityRank(sev) {
		sev = floor
	}
	category := strings.TrimSpace(c.Category)
	if category == "" {
		category = "review"
	}
	return Finding{
		Rule:       ruleLabel(rule),
		Severity:   sev,
		Category:   category,
		Confidence: 0.5,
		Source:     "llm",
		File:       cleanPath(c.File),
		Message:    c.Message,
		Body:       c.Message,
	}
}

// normalizeSeverity maps a free-text severity (from a model reply or a rulepack
// match) onto the canonical Severity vocabulary, tolerating the impact-ladder
// synonyms (high/medium/low) the model may emit.
func normalizeSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return SevCritical
	case "error", "high":
		return SevError
	case "warning", "warn", "medium":
		return SevWarning
	case "info", "low", "":
		return SevInfo
	default:
		return SevInfo
	}
}

func severityRank(s Severity) int {
	switch s {
	case SevCritical:
		return 3
	case SevError:
		return 2
	case SevWarning:
		return 1
	default:
		return 0
	}
}

// ruleFindingFromMatch turns a deterministic rulepack match into a Finding
// (Source="rulepack"). The match already carries an exact (file,line) so no
// relocation is needed; the per-file rule supplies the rule name and severity
// floor.
func ruleFindingFromMatch(m astquery.Match, rule config.ReviewRule) Finding {
	sev := normalizeSeverity(m.Severity)
	if floor := normalizeSeverity(rule.Severity); severityRank(floor) > severityRank(sev) {
		sev = floor
	}
	category := strings.TrimSpace(m.Detector)
	if category == "" {
		category = "review"
	}
	message := strings.TrimSpace(m.Text)
	if message == "" {
		message = m.Detector
	}
	file := cleanPath(m.File)
	return Finding{
		Rule:       firstNonEmpty(m.Detector, ruleLabel(rule)),
		Severity:   sev,
		Category:   category,
		Confidence: 1.0,
		Source:     "rulepack",
		SymbolID:   m.SymbolID,
		File:       file,
		Line:       m.Line,
		StartLine:  m.Line,
		EndLine:    maxInt(m.Line, m.EndLine),
		Side:       sideRight,
		Anchor:     AnchorExactHunk,
		Message:    message,
		Body:       message,
	}
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if b > a {
		return b
	}
	return a
}
