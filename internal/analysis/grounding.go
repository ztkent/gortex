package analysis

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// LineHit is an exact new-side anchor for a free-text finding: the file, the
// post-change line number, and a confidence in the match (1.0 exact substring,
// 0.7 whitespace-normalized, <0.5 token-overlap fallback).
type LineHit struct {
	File       string  `json:"file"`
	Line       int     `json:"line"`
	Confidence float64 `json:"confidence"`
}

// LineGrounder resolves a free-text snippet inside a changed file (optionally
// scoped to a changed symbol) to an exact new-side line. It is fed the per-file
// new-side HunkLines produced by MapGitDiffWithLines, plus a symbol-span lookup
// derived from the graph so a finding can be confined to the symbol it concerns.
type LineGrounder struct {
	lines      map[string][]HunkLine
	symbolSpan func(symbolID string) (start, end int, ok bool)
}

// NewLineGrounder builds a grounder over the new-side lines of a diff. The graph
// supplies symbol spans (Node.StartLine/EndLine via GetFileNodes) so GroundFinding
// can restrict matching to a single changed symbol when given its id.
func NewLineGrounder(g graph.Store, lines map[string][]HunkLine) *LineGrounder {
	return &LineGrounder{
		lines:      lines,
		symbolSpan: graphSymbolSpan(g),
	}
}

// graphSymbolSpan returns a lookup from a symbol id to its [start,end] line span,
// resolved by scanning the file's nodes for an exact id match.
func graphSymbolSpan(g graph.Store) func(string) (int, int, bool) {
	return func(symbolID string) (int, int, bool) {
		if g == nil || symbolID == "" {
			return 0, 0, false
		}
		file := symbolFile(symbolID)
		if file == "" {
			return 0, 0, false
		}
		for _, n := range g.GetFileNodes(file) {
			if n == nil || n.Kind == graph.KindFile {
				continue
			}
			if n.ID == symbolID {
				end := n.EndLine
				if end < n.StartLine {
					end = n.StartLine
				}
				return n.StartLine, end, true
			}
		}
		return 0, 0, false
	}
}

// symbolFile extracts the file portion of a "<file>::<symbol>" node id.
func symbolFile(symbolID string) string {
	if i := strings.Index(symbolID, "::"); i >= 0 {
		return symbolID[:i]
	}
	return ""
}

// GroundFinding resolves a free-text snippet to an exact new-side line inside the
// given file, optionally confined to symbolID's line span.
//
// Tiers, best first:
//   - exact substring of an added/context line → confidence 1.0
//   - whitespace-normalized substring match    → confidence 0.7
//   - token-overlap best line (fallback)        → confidence <0.5
//
// An empty snippet anchors to the symbol's first added line (or the file's first
// added line when no symbol scope is given). ok is false when no line scopes or
// matches.
func (lg *LineGrounder) GroundFinding(filePath, symbolID, snippet string) (LineHit, bool) {
	if lg == nil {
		return LineHit{}, false
	}
	file := cleanFile(filePath)
	candidates := lg.scopedLines(file, symbolID)
	if len(candidates) == 0 {
		return LineHit{}, false
	}

	snip := strings.TrimSpace(snippet)
	if snip == "" {
		// Anchor to the first added line in scope; fall back to the first line.
		for _, hl := range candidates {
			if hl.Side == "+" {
				return LineHit{File: file, Line: hl.NewLine, Confidence: 1.0}, true
			}
		}
		return LineHit{File: file, Line: candidates[0].NewLine, Confidence: 1.0}, true
	}

	// Tier 1: exact substring.
	for _, hl := range candidates {
		if strings.Contains(hl.Text, snip) {
			return LineHit{File: file, Line: hl.NewLine, Confidence: 1.0}, true
		}
	}

	// Tier 2: whitespace-normalized substring.
	normSnip := normalizeWS(snip)
	if normSnip != "" {
		for _, hl := range candidates {
			if strings.Contains(normalizeWS(hl.Text), normSnip) {
				return LineHit{File: file, Line: hl.NewLine, Confidence: 0.7}, true
			}
		}
	}

	// Tier 3: token-overlap best line (fallback, confidence < 0.5).
	wantTokens := tokenize(snip)
	if len(wantTokens) == 0 {
		return LineHit{}, false
	}
	bestLine := 0
	bestScore := 0.0
	for _, hl := range candidates {
		score := tokenOverlap(wantTokens, tokenize(hl.Text))
		if score > bestScore {
			bestScore = score
			bestLine = hl.NewLine
		}
	}
	if bestScore <= 0 {
		return LineHit{}, false
	}
	// Cap the fallback confidence strictly below the whitespace tier.
	conf := bestScore * 0.49
	if conf >= 0.5 {
		conf = 0.49
	}
	return LineHit{File: file, Line: bestLine, Confidence: conf}, true
}

// scopedLines returns the candidate new-side lines for a file, narrowed to the
// symbol's [start,end] span when symbolID resolves to a known span.
func (lg *LineGrounder) scopedLines(file, symbolID string) []HunkLine {
	all := lg.lines[file]
	if len(all) == 0 {
		return nil
	}
	if symbolID == "" || lg.symbolSpan == nil {
		return all
	}
	start, end, ok := lg.symbolSpan(symbolID)
	if !ok {
		return all
	}
	var scoped []HunkLine
	for _, hl := range all {
		if hl.NewLine >= start && hl.NewLine <= end {
			scoped = append(scoped, hl)
		}
	}
	if len(scoped) == 0 {
		return all
	}
	return scoped
}

// cleanFile normalizes a path the same way DiffHunk.FilePath / parseDiffLines do
// so a caller-supplied path keys into the lines map.
func cleanFile(p string) string {
	return strings.TrimPrefix(p, "./")
}

// normalizeWS collapses every run of whitespace to a single space and trims the
// ends, so indentation and inter-token spacing differences don't defeat a match.
func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// tokenize splits a string into lowercased identifier-ish tokens for the
// overlap fallback.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_'
	})
	return fields
}

// tokenOverlap is the fraction of want's tokens present in have (a directional
// containment score in [0,1]).
func tokenOverlap(want, have []string) float64 {
	if len(want) == 0 {
		return 0
	}
	haveSet := make(map[string]struct{}, len(have))
	for _, t := range have {
		haveSet[t] = struct{}{}
	}
	matched := 0
	for _, t := range want {
		if _, ok := haveSet[t]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(want))
}
