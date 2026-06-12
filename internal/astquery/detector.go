package astquery

import (
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/parser"
)

// Categories carried by SAST + adjacent rules. Free-form strings — the
// only consumers are the analyze-side dispatcher and the rule audit;
// the engine itself doesn't switch on these.
const (
	CategorySAST        = "sast"
	CategoryHygiene     = "hygiene"
	CategoryCorrectness = "correctness"
	CategoryPerformance = "performance"
	// CategoryReview groups idiomatic / correctness rules (nil-deref,
	// check-then-act, n-plus-one, logic errors) surfaced through the
	// review path. Kept distinct from hygiene so these carry
	// error/warning severity and feed the graph-grounding post-pass
	// that runs one layer up (the detectors themselves stay pure-AST).
	CategoryReview = "review"
)

// Detector is one named structural rule. The Languages map carries
// per-language tree-sitter S-expression queries; the engine compiles
// them once per run and runs the appropriate one for each target's
// language. PostFilter is an optional second-pass filter that
// receives the raw QueryResult plus the file's source bytes — used
// when a detector needs to do something beyond what tree-sitter
// query predicates support (e.g. "this regex matches the text of
// capture X" combined with structural shape).
type Detector struct {
	Name        string
	Description string
	Severity    string

	// Category lets the analyze layer fan rules out by purpose
	// ("sast", "hygiene", "performance", "correctness"). Empty
	// when the rule pre-dates the rule-library refactor — those
	// inherit Category="" and surface only through the legacy
	// unsafe_patterns bundle.
	Category string

	// CWE maps the rule to MITRE's Common Weakness Enumeration so
	// SARIF / DefectDojo / GitHub Code Scanning consumers can join
	// against canonical weakness IDs. Empty when the rule is pure
	// hygiene with no security implication.
	CWE string

	// OWASP maps the rule to the OWASP Top 10 category, e.g.
	// "A03:2021-Injection". Empty for non-web-app vulnerabilities.
	OWASP string

	// Tags are free-form taxonomy hooks: "injection", "deserialization",
	// "crypto", "xxe", "ssrf", "path-traversal", "secrets",
	// "deprecated", "django", "flask", etc. The analyze layer
	// supports tag-based filtering (`tags:"crypto,deserialization"`).
	Tags []string

	// References are URLs / CWE links / Bandit plugin IDs (e.g.
	// "B602", "bandit:subprocess_popen_with_shell_equals_true") so
	// an agent can cross-check the rule's intent without re-deriving
	// it from the description.
	References []string

	// Languages is keyed by the language string stored on KindFile
	// nodes ("go", "python", "typescript", …). Each value is a
	// tree-sitter S-expression. A capture named `match` is the
	// row's anchor; absent that, the engine falls back to the
	// longest captured node.
	Languages map[string]string

	// ExcludeTests defaults to true for detectors — a "panic in
	// library" rule firing inside `_test.go` is noise. Detectors
	// that intentionally inspect tests (e.g. a "test name doesn't
	// match prefix" rule) can flip this to false.
	ExcludeTests bool

	// PostFilter is optional. Return true to keep the match.
	PostFilter func(parser.QueryResult, []byte) bool
}

var (
	detectorMu       sync.RWMutex
	detectorRegistry = map[string]*Detector{}
)

// RegisterDetector adds d to the global detector registry. Called
// from package-level init in detectors.go for each bundled rule.
// Tests may register additional detectors via RegisterDetector — the
// API is intentionally exported so a downstream consumer (e.g. a
// project-specific lint set) can layer rules without forking the
// engine.
func RegisterDetector(d *Detector) {
	if d == nil || d.Name == "" {
		return
	}
	d.normalise()
	detectorMu.Lock()
	detectorRegistry[d.Name] = d
	detectorMu.Unlock()
}

func lookupDetector(name string) (*Detector, bool) {
	detectorMu.RLock()
	defer detectorMu.RUnlock()
	d, ok := detectorRegistry[name]
	return d, ok
}

// ListDetectors returns the names of every registered detector,
// sorted alphabetically. Used by the MCP layer to fail fast with a
// helpful error when a caller passes an unknown detector name.
func ListDetectors() []string {
	detectorMu.RLock()
	defer detectorMu.RUnlock()
	names := make([]string, 0, len(detectorRegistry))
	for n := range detectorRegistry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// DescribeDetectors returns rich metadata for every registered
// detector, suitable for surfacing in the MCP tool description so
// agents can pick the right rule without an out-of-band docs lookup.
func DescribeDetectors() []DetectorInfo {
	detectorMu.RLock()
	defer detectorMu.RUnlock()
	out := make([]DetectorInfo, 0, len(detectorRegistry))
	for _, d := range detectorRegistry {
		langs := make([]string, 0, len(d.Languages))
		for l := range d.Languages {
			langs = append(langs, l)
		}
		sort.Strings(langs)
		tags := append([]string(nil), d.Tags...)
		refs := append([]string(nil), d.References...)
		out = append(out, DetectorInfo{
			Name:        d.Name,
			Description: d.Description,
			Severity:    d.Severity,
			Category:    d.Category,
			CWE:         d.CWE,
			OWASP:       d.OWASP,
			Tags:        tags,
			References:  refs,
			Languages:   langs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DetectorsByCategory returns every registered rule whose Category
// matches one of the requested labels. Empty `cats` returns all rules
// (including legacy uncategorised ones). Used by the analyze layer
// to fan out `sast` / `hygiene` / etc. bundles.
func DetectorsByCategory(cats ...string) []*Detector {
	want := make(map[string]struct{}, len(cats))
	for _, c := range cats {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		want[c] = struct{}{}
	}
	detectorMu.RLock()
	defer detectorMu.RUnlock()
	out := make([]*Detector, 0, len(detectorRegistry))
	for _, d := range detectorRegistry {
		if len(want) == 0 {
			out = append(out, d)
			continue
		}
		if _, ok := want[strings.ToLower(d.Category)]; ok {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LookupDetector returns the detector with the given name. Used by
// the analyze layer when fanning out a curated set; returns nil when
// the name is unknown so callers can decide between skip and error.
func LookupDetector(name string) *Detector {
	d, _ := lookupDetector(name)
	return d
}

// DetectorInfo is the read-only projection used by the MCP layer.
type DetectorInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Severity    string   `json:"severity"`
	Category    string   `json:"category,omitempty"`
	CWE         string   `json:"cwe,omitempty"`
	OWASP       string   `json:"owasp,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	References  []string `json:"references,omitempty"`
	Languages   []string `json:"languages"`
}

func (d *Detector) normalise() {
	d.Name = strings.TrimSpace(d.Name)
	if d.Severity == "" {
		d.Severity = "warning"
	}
	// Normalise language keys to the lowercase, hyphen-free form
	// the engine and the graph use.
	if len(d.Languages) > 0 {
		fixed := make(map[string]string, len(d.Languages))
		for k, v := range d.Languages {
			fixed[strings.ToLower(strings.TrimSpace(k))] = v
		}
		d.Languages = fixed
	}
	// (Tests-exclusion default lives in the engine — see
	// buildPlan; detectors don't need to flip a bit on every
	// entry.)
}
