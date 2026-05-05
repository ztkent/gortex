package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/progress"
)

// writeIndexTextSummary emits one line per path with file/node/edge totals
// and duration; if the result has errors, they're listed indented below in
// red (or plain text on non-TTY stdout — lipgloss strips colors when the
// output isn't a terminal).
func writeIndexTextSummary(w io.Writer, path string, r *indexer.IndexResult) {
	stats := []string{
		progress.Stat(humanizeInt(r.FileCount), "files", progress.StatNeutral),
		progress.Stat(humanizeInt(r.NodeCount), "nodes", progress.StatNeutral),
		progress.Stat(humanizeInt(r.EdgeCount), "edges", progress.StatNeutral),
		progress.Stat(strconv.FormatInt(r.DurationMs, 10)+"ms", "", progress.StatGood),
	}
	if len(r.Errors) > 0 {
		stats = append(stats, progress.Stat(humanizeInt(len(r.Errors)), "errors", progress.StatBad))
	}

	_, _ = fmt.Fprintf(w, "  %s   %s\n", progress.Row(path, "", 4), progress.StatStrip(stats...))
	for _, e := range r.Errors {
		_, _ = fmt.Fprintf(w, "      %s\n", progress.Caption(fmt.Sprintf("%s: %s", e.FilePath, e.Error)))
	}
}

// humanizeInt returns the integer with thousands separators ("1234" → "1,234").
// Works for any integer-like value.
func humanizeInt[T int | int32 | int64 | uint32 | uint64](v T) string {
	s := strconv.FormatInt(int64(v), 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}
	for i := rem; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
