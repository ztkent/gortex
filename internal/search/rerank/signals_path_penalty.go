package rerank

import (
	"path"
	"regexp"
	"strings"
)

// PathPenaltySignal applies a multiplicative penalty to candidates
// whose file path falls into one of five "supporting cast" buckets —
// test files, compatibility shims, examples, type declarations, and
// re-export barrels. The intuition: when an agent asks for the
// canonical definition of `validateToken`, the top hit should be the
// real implementation in `auth/token.go`, not the assertion in
// `auth/token_test.go` or the re-export in `index.ts`.
//
// Signals contribute in [0, 1] additively to a candidate's score, so
// "penalty" here is encoded as a smaller positive contribution: an
// uncategorised file gets 1.0, a test file gets 0.3, and so on. The
// pipeline weight scales the spread between buckets; with weight 0.4,
// a test-file penalty costs roughly 0.28 score points relative to a
// neutral file, which is enough to demote on ties but not enough to
// hide a strong BM25 + fan-in match.
//
// Tiers (multiplier returned):
//
//   - Test files       → 0.3 (the heaviest penalty: assertions and
//     fixtures should never outrank real code)
//   - Compatibility    → 0.5 (legacy / polyfill / shim — usually a
//     workaround, not the live implementation)
//   - Examples         → 0.5 (demo code; useful but never the truth)
//   - Type declarations → 0.7 (`.d.ts`, `.pyi`, `.h` headers — the
//     interface, not the implementation)
//   - Re-export barrels → 0.7 (`index.ts`, `__init__.py`, `mod.rs`,
//     `lib.rs` — a forwarding hop, not the source)
//   - Anything else    → 1.0 (no penalty)
//
// When a file matches multiple rubrics the smaller (more aggressive)
// multiplier wins so penalties don't compound silently — a
// `tests/examples/foo.go` reads as a test, not as 0.3 * 0.5.
//
// Path classifications are cached per-Rerank call so the rubric runs
// once per unique file path rather than once per candidate.
type PathPenaltySignal struct{}

// Name returns the canonical signal name registered in DefaultWeights.
func (PathPenaltySignal) Name() string { return SignalPathPenalty }

// Contribute returns the per-candidate path-penalty multiplier in
// [0.3, 1.0]. Returns 1.0 for candidates with no file path so a
// missing path doesn't accidentally crush a candidate.
func (PathPenaltySignal) Contribute(_ string, c *Candidate, ctx *Context) float64 {
	if c == nil || c.Node == nil {
		return 1.0
	}
	fp := c.Node.FilePath
	if fp == "" {
		return 1.0
	}
	if ctx != nil && ctx.pathPenaltyCache != nil {
		if cached, ok := ctx.pathPenaltyCache[fp]; ok {
			return cached
		}
	}
	pen := classifyPathPenalty(fp)
	if ctx != nil && ctx.pathPenaltyCache != nil {
		ctx.pathPenaltyCache[fp] = pen
	}
	return pen
}

// Penalty multiplier constants — exported so config / debug surfaces
// can refer to them without re-deriving the rubric.
const (
	PathPenaltyTest      = 0.3
	PathPenaltyCompat    = 0.5
	PathPenaltyExamples  = 0.5
	PathPenaltyTypeDecl  = 0.7
	PathPenaltyReexport  = 0.7
	PathPenaltyUncatched = 1.0
)

// Pre-compiled patterns. Built at package init so the rubric stays
// allocation-free on the hot path.
var (
	// Test paths across 15+ language ecosystems. Matches any path
	// segment that looks like a test file: Go's _test.go, Python's
	// test_*.py / *_test.py, JS/TS *.spec.{js,ts,tsx,jsx} and
	// *.test.{js,ts,tsx,jsx}, Ruby's *_spec.rb / *_test.rb, Rust's
	// tests/ tree, Swift's *Tests.swift, Java/Kotlin's *Test.{java,kt}.
	// Also any directory literally called test / tests / __tests__ /
	// spec / specs / e2e / fixtures.
	pathRETest = regexp.MustCompile(`(?i)(^|/)((__tests__|tests?|specs?|e2e|fixtures?|testdata)(/|$)|.*(_test|_spec)\.(go|py|rb)$|.*\.(test|spec)\.(js|jsx|ts|tsx|mjs|cjs)$|test_[^/]+\.py$|.*Tests?\.swift$|.*Test\.(java|kt|scala|cs)$)`)

	// Compatibility / shim directories. The heuristic only fires on
	// the directory itself — `compat.go` (single file) is not enough,
	// but `compat/` or `legacy/` is. Polyfill is the dominant JS
	// convention; backport is the dominant Python convention.
	pathRECompat = regexp.MustCompile(`(?i)(^|/)(compat|legacy|polyfill|polyfills|shim|shims|backport|backports|deprecated)(/|$)`)

	// Examples / demo trees. Same dir-level rule: a file called
	// `example.go` (a single module) is not enough; `examples/` or
	// `demo/` directories are.
	pathREExamples = regexp.MustCompile(`(?i)(^|/)(examples?|demos?|samples?|sandbox|playground)(/|$)`)

	// Type declarations — interface files that don't carry the
	// implementation. TS *.d.ts is the canonical case; Python's .pyi
	// stub mirror; C/C++ headers (.h, .hpp, .hh, .hxx) when they're
	// in include/ or directly named like a type-only declaration.
	pathRETypeDecl = regexp.MustCompile(`(?i)\.(d\.ts|d\.cts|d\.mts|pyi|hpp|hxx|hh)$|(^|/)include/.*\.h$`)

	// Re-export filenames — barrels that just forward symbols from
	// other modules. The canonical names across ecosystems.
	reexportNames = map[string]struct{}{
		"index.js":  {},
		"index.jsx": {},
		"index.ts":  {},
		"index.tsx": {},
		"index.mjs": {},
		"index.cjs": {},
		"__init__.py": {},
		"mod.rs": {},
		"lib.rs": {},
	}
)

// classifyPathPenalty applies the rubric in priority order — most
// aggressive penalty wins on overlap. Exported indirectly via the
// signal so tests can pin specific paths.
func classifyPathPenalty(fp string) float64 {
	// Normalise to forward slashes so the regexes are platform-stable.
	norm := strings.ReplaceAll(fp, "\\", "/")
	if pathRETest.MatchString(norm) {
		return PathPenaltyTest
	}
	if pathRECompat.MatchString(norm) {
		return PathPenaltyCompat
	}
	if pathREExamples.MatchString(norm) {
		return PathPenaltyExamples
	}
	if pathRETypeDecl.MatchString(norm) {
		return PathPenaltyTypeDecl
	}
	base := path.Base(norm)
	if _, ok := reexportNames[strings.ToLower(base)]; ok {
		return PathPenaltyReexport
	}
	return PathPenaltyUncatched
}
