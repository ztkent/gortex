// Package coverage parses Go's cover.out profile format and stamps
// per-function coverage percentages onto graph nodes. The result is
// the per-symbol meta.coverage_pct field that lets agents answer
// "which symbols are untested" with real numbers rather than the
// reverse-edge-empty heuristic the existing get_untested_symbols
// tool relies on.
//
// Profile format (one segment per non-header line):
//
//	mode: set | count | atomic
//	<importpath>/<file.go>:<startLine>.<startCol>,<endLine>.<endCol> <numStmt> <count>
//
// startLine/endLine are 1-based; numStmt is the number of source
// statements in the segment; count is execution count (or 0/1 for
// `mode: set`). To compute a function's coverage we sum numStmt
// over segments that fall fully within the function's line range,
// then sum the same numStmt over segments where count > 0; the
// ratio is the percentage.
package coverage

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Segment is one parsed entry from a cover profile. Lines are
// 1-based; columns are kept verbatim from the file but unused by
// the projection — Go's cover output puts the boundary on a line
// number that always belongs to the enclosing function.
type Segment struct {
	File      string
	StartLine int
	EndLine   int
	NumStmt   int
	Count     int
}

// Parse reads cover-profile content and returns one Segment per
// non-header line. Malformed lines are skipped silently — the
// enrichment pass is best-effort like blame.
func Parse(profile []byte) []Segment {
	var out []Segment
	scanner := bufio.NewScanner(bytes.NewReader(profile))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Skip the mode header (and any other lines that aren't
		// segment shapes).
		if strings.HasPrefix(line, "mode:") {
			continue
		}
		seg, ok := parseSegment(line)
		if !ok {
			continue
		}
		out = append(out, seg)
	}
	return out
}

// ParseFile is a small convenience wrapper that reads the profile
// from disk and delegates to Parse.
func ParseFile(path string) ([]Segment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data), nil
}

// parseSegment splits one profile line into a Segment. Format:
// `<file>:<sl>.<sc>,<el>.<ec> <numStmt> <count>` — the last two
// are simple ints; the file:range prefix is split on `:`,`,` and
// `.` boundaries.
func parseSegment(line string) (Segment, bool) {
	colon := strings.LastIndex(line, ":")
	if colon < 0 {
		return Segment{}, false
	}
	file := line[:colon]
	rest := line[colon+1:]
	// rest is `<sl>.<sc>,<el>.<ec> <numStmt> <count>`.
	fields := strings.Fields(rest)
	if len(fields) != 3 {
		return Segment{}, false
	}
	rng := fields[0]
	comma := strings.Index(rng, ",")
	if comma < 0 {
		return Segment{}, false
	}
	startSpec := rng[:comma]
	endSpec := rng[comma+1:]
	startLine, ok := parseLineCol(startSpec)
	if !ok {
		return Segment{}, false
	}
	endLine, ok := parseLineCol(endSpec)
	if !ok {
		return Segment{}, false
	}
	numStmt, err := strconv.Atoi(fields[1])
	if err != nil {
		return Segment{}, false
	}
	count, err := strconv.Atoi(fields[2])
	if err != nil {
		return Segment{}, false
	}
	return Segment{
		File:      file,
		StartLine: startLine,
		EndLine:   endLine,
		NumStmt:   numStmt,
		Count:     count,
	}, true
}

// parseLineCol returns just the line component of a `<line>.<col>`
// pair. Cover-profile columns aren't useful for the line-range
// projection so we keep the parser focused.
func parseLineCol(spec string) (int, bool) {
	dot := strings.Index(spec, ".")
	if dot < 0 {
		return 0, false
	}
	v, err := strconv.Atoi(spec[:dot])
	if err != nil {
		return 0, false
	}
	return v, true
}

// CoverageStats is the accumulated coverage for one node range.
type CoverageStats struct {
	NumStmt int // total statements counted
	Hit     int // statements with count > 0
}

// Percent returns coverage as a 0–100 float, or -1 when no
// statements were counted (so callers can distinguish "uncovered"
// from "no measurement").
func (s CoverageStats) Percent() float64 {
	if s.NumStmt == 0 {
		return -1
	}
	return float64(s.Hit) / float64(s.NumStmt) * 100
}

// EnrichGraph projects parsed segments onto every function /
// method / closure / generic_param node by line range and stamps
// meta.coverage_pct (rounded to 2 decimals) plus meta.coverage =
// {num_stmt, hit}. Returns the number of nodes that received a
// measurement (segments with NumStmt > 0).
//
// modulePath is the Go module path of the indexed repo (read from
// go.mod) — needed because cover-profile file paths are
// module-prefixed (`github.com/foo/bar/pkg/file.go`) while graph
// file paths are repo-relative (`pkg/file.go`). Pass "" to skip
// the prefix-strip, useful when the profile was generated against
// raw paths.
func EnrichGraph(g graph.Store, segments []Segment, modulePath string) int {
	if g == nil || len(segments) == 0 {
		return 0
	}
	// Group segments by repo-relative file path so each file is
	// projected once even when the profile lists thousands of
	// segments per package.
	byFile := make(map[string][]Segment)
	for _, s := range segments {
		path := stripModulePrefix(s.File, modulePath)
		byFile[path] = append(byFile[path], s)
	}

	enriched := 0
	for _, n := range g.AllNodes() {
		if !shouldEnrichCoverage(n.Kind) {
			continue
		}
		if n.FilePath == "" || n.StartLine == 0 {
			continue
		}
		segs, ok := byFile[n.FilePath]
		if !ok {
			continue
		}
		stats := projectStats(segs, n.StartLine, n.EndLine)
		if stats.NumStmt == 0 {
			continue
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		pct := roundTwo(stats.Percent())
		n.Meta["coverage_pct"] = pct
		n.Meta["coverage"] = map[string]any{
			"num_stmt": stats.NumStmt,
			"hit":      stats.Hit,
		}
		enriched++

		// EdgeCoveredBy: invert each EdgeTests pointing at this
		// node so agents can ask "which tests cover X" with the
		// coverage metric attached, without re-deriving it from
		// meta.coverage_pct + a second EdgeTests walk. Skip when
		// pct == 0 — uncovered code has no test relation worth
		// advertising. Dedup on the test ID because the same test
		// may call the subject multiple times.
		if pct == 0 {
			continue
		}
		seen := map[string]bool{}
		for _, in := range g.GetInEdges(n.ID) {
			if in == nil || in.Kind != graph.EdgeTests {
				continue
			}
			if seen[in.From] {
				continue
			}
			seen[in.From] = true
			g.AddEdge(&graph.Edge{
				From:     n.ID,
				To:       in.From,
				Kind:     graph.EdgeCoveredBy,
				FilePath: n.FilePath,
				Line:     n.StartLine,
				Origin:   graph.OriginASTInferred,
				Meta: map[string]any{
					"coverage_pct": pct,
				},
			})
		}
	}
	return enriched
}

// projectStats sums numStmt and hit-count for segments whose start
// line falls inside [startLine, endLine] inclusive. Using the
// segment start-line as the inclusion test matches the way Go's
// cover tool reports per-function counts via `go tool cover -func`
// — segments are scoped to the immediately enclosing block, so a
// start-line containment check is the correct projection.
func projectStats(segments []Segment, startLine, endLine int) CoverageStats {
	if endLine < startLine {
		endLine = startLine
	}
	var stats CoverageStats
	for _, s := range segments {
		if s.StartLine < startLine || s.StartLine > endLine {
			continue
		}
		stats.NumStmt += s.NumStmt
		if s.Count > 0 {
			stats.Hit += s.NumStmt
		}
	}
	return stats
}

// shouldEnrichCoverage limits enrichment to executable-symbol
// kinds. Variables, fields, types — the structural kinds — have
// no coverage signal of their own.
func shouldEnrichCoverage(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindFunction, graph.KindMethod, graph.KindClosure:
		return true
	}
	return false
}

// stripModulePrefix turns `github.com/foo/bar/pkg/file.go` into
// `pkg/file.go` when modulePath is `github.com/foo/bar`. Always
// strips a leading `./` regardless of modulePath — some profiles
// (notably ones generated outside a module-aware build) emit
// relative paths.
func stripModulePrefix(file, modulePath string) string {
	file = strings.TrimPrefix(file, "./")
	if modulePath == "" {
		return file
	}
	prefix := modulePath + "/"
	if strings.HasPrefix(file, prefix) {
		return file[len(prefix):]
	}
	return file
}

// ReadModulePath extracts the module path declared by go.mod at
// repoRoot. Returns "" when go.mod is missing or malformed —
// EnrichGraph treats "" as "skip prefix-strip", which still
// produces correct output for profiles generated against raw
// paths.
func ReadModulePath(repoRoot string) string {
	data, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// roundTwo rounds a float to 2 decimal places. Used for the
// coverage_pct field to keep the meta payload tidy in JSON
// responses.
func roundTwo(v float64) float64 {
	if v < 0 {
		return v
	}
	scaled := int64(v*100 + 0.5)
	return float64(scaled) / 100
}
