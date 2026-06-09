// Package review holds the pure value types shared by every PR-review layer:
// the single Finding type, the verdict envelope, and the adapters that bridge
// the existing risk / severity vocabularies into a BLOCK/REVIEW/APPROVE verdict.
//
// These types carry no graph / mcp dependencies so every layer can import them
// without an import cycle. The dependency is strictly one-way: review imports
// analysis (for RiskLevel), never the reverse.
package review

// Verdict is the canonical PR-review outcome.
type Verdict string

const (
	VerdictApprove Verdict = "APPROVE"
	VerdictReview  Verdict = "REVIEW"
	VerdictBlock   Verdict = "BLOCK"
)

// Severity classifies a single finding.
type Severity string

const (
	SevInfo     Severity = "info"
	SevWarning  Severity = "warning"
	SevError    Severity = "error"
	SevCritical Severity = "critical"
)

// Finding is the canonical review finding shared by every layer. It is the
// union of every field consumed across the review pipeline. Each field's first
// writer is noted in the comments; later layers only fill more of them — they
// never redefine the type.
type Finding struct {
	// identity / classification
	ID          string   `json:"id,omitempty"`           // stable per-finding id
	IdentityKey string   `json:"identity_key,omitempty"` // suppression key
	Rule        string   `json:"rule"`                   // detector / rule name
	Severity    Severity `json:"severity"`
	Category    string   `json:"category"`
	CWE         string   `json:"cwe,omitempty"`
	Confidence  float64  `json:"confidence"`
	Source      string   `json:"source,omitempty"` // "rulepack" | "llm"

	// location
	SymbolID  string `json:"symbol_id"`
	File      string `json:"file"`
	Line      int    `json:"line"`                 // grounded new-side line; == StartLine for single-line
	StartLine int    `json:"start_line,omitempty"` // multi-line anchor start
	EndLine   int    `json:"end_line,omitempty"`   // multi-line anchor end
	Side      string `json:"side,omitempty"`       // "RIGHT" (new) / "LEFT" (old)
	Anchor    string `json:"anchor_method,omitempty"`

	// content
	Message    string `json:"message"`              // short headline
	Body       string `json:"body,omitempty"`       // full markdown body for posting
	Suggestion string `json:"suggestion,omitempty"` //
	GenTokens  int    `json:"gen_tokens,omitempty"` // tokens attributed to this finding
}
