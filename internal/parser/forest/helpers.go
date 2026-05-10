package forest

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// funcRange is one definition's line span, used to attribute call
// references to their enclosing function.
type funcRange struct {
	id        string
	startLine int
	endLine   int
}

// buildFuncRanges walks the already-emitted nodes and returns a
// flat slice of every function/method's span. Linear scan is fine —
// even large files emit only hundreds of definitions, and the
// per-call lookup walks this slice in O(N).
func buildFuncRanges(result *parser.ExtractionResult) []funcRange {
	if result == nil {
		return nil
	}
	var ranges []funcRange
	for _, n := range result.Nodes {
		if n == nil {
			continue
		}
		// Forest defs that can host a call: anything code-bearing.
		switch n.Kind {
		case graph.KindFunction, graph.KindMethod:
			ranges = append(ranges, funcRange{
				id: n.ID, startLine: n.StartLine, endLine: n.EndLine,
			})
		}
	}
	return ranges
}

// findEnclosingFunc returns the most-tightly-enclosing function ID
// for a given 1-based line, or "" if no def covers the line. When
// definitions nest (e.g. a closure inside a function), the
// inner-most range wins because we prefer the smallest covering
// span.
func findEnclosingFunc(ranges []funcRange, line int) string {
	bestID := ""
	bestSpan := 0
	for _, r := range ranges {
		if line < r.startLine || line > r.endLine {
			continue
		}
		span := r.endLine - r.startLine
		if bestID == "" || span < bestSpan {
			bestID = r.id
			bestSpan = span
		}
	}
	return bestID
}
