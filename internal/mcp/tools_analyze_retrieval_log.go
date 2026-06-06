package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// handleAnalyzeRetrievalLog mines the append-only retrieval query log
// (see query_log.go) for offline recall tuning. It surfaces the
// zero-result queries — the ones BM25/the graph could not answer, the
// highest-signal candidates for synonym expansion, missing index
// coverage, or a feedback force-inject — plus per-tool latency and
// result-size rollups.
//
// Params: limit (records scanned, newest first; default 5000),
// tool (filter), zero_only (bool), since (RFC3339 prefix match),
// top (distinct zero-result questions to surface; default 20),
// include_recent (bool — attach the newest rows).
func (s *Server) handleAnalyzeRetrievalLog(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	limit := intArgOrDefault(args, "limit", 5000)
	if limit <= 0 {
		limit = 5000
	}
	if limit > 200000 {
		limit = 200000
	}
	toolFilter := strings.TrimSpace(stringArg(args, "tool"))
	zeroOnly, _ := args["zero_only"].(bool)
	since := strings.TrimSpace(stringArg(args, "since"))
	top := intArgOrDefault(args, "top", 20)
	if top <= 0 {
		top = 20
	}
	includeRecent, _ := args["include_recent"].(bool)

	path := s.queryLog.Path()
	if path == "" {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"enabled": false,
			"message": "retrieval query log is disabled (GORTEX_QUERY_LOG_DISABLE set, or no writable cache dir)",
		})
	}

	records, scanned, readErr := readQueryLogTail(path, limit)
	if readErr != nil && len(records) == 0 {
		// No log yet is the common cold-start case, not an error.
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"enabled":  true,
			"log_path": path,
			"total":    0,
			"message":  "no query log records yet",
		})
	}

	// Per-tool aggregation.
	type toolAgg struct {
		count     int
		zero      int
		nodesSum  int
		durations []float64
	}
	byTool := map[string]*toolAgg{}
	zeroCounter := map[string]*zeroQ{}
	var filtered []queryLogRecord
	var firstTS, lastTS string

	for _, r := range records {
		if toolFilter != "" && r.Tool != toolFilter {
			continue
		}
		if since != "" && r.TS < since {
			continue
		}
		if zeroOnly && !r.ZeroResult {
			continue
		}
		filtered = append(filtered, r)
		if firstTS == "" || r.TS < firstTS {
			firstTS = r.TS
		}
		if r.TS > lastTS {
			lastTS = r.TS
		}
		a := byTool[r.Tool]
		if a == nil {
			a = &toolAgg{}
			byTool[r.Tool] = a
		}
		a.count++
		a.nodesSum += r.NodesReturned
		a.durations = append(a.durations, r.DurationMS)
		if r.ZeroResult {
			a.zero++
			key := r.Tool + "\x00" + strings.ToLower(strings.TrimSpace(r.Question))
			zq := zeroCounter[key]
			if zq == nil {
				zq = &zeroQ{Tool: r.Tool, Question: r.Question, Corpus: r.Corpus}
				zeroCounter[key] = zq
			}
			zq.Count++
		}
	}

	// Build per-tool rows.
	type toolRow struct {
		Tool      string  `json:"tool"`
		Count     int     `json:"count"`
		Zero      int     `json:"zero_result"`
		ZeroRate  float64 `json:"zero_rate"`
		AvgNodes  float64 `json:"avg_nodes"`
		P50MS     float64 `json:"p50_ms"`
		P95MS     float64 `json:"p95_ms"`
	}
	toolRows := make([]toolRow, 0, len(byTool))
	totalQueries, totalZero := 0, 0
	for name, a := range byTool {
		totalQueries += a.count
		totalZero += a.zero
		sort.Float64s(a.durations)
		row := toolRow{Tool: name, Count: a.count, Zero: a.zero}
		if a.count > 0 {
			row.ZeroRate = round4(float64(a.zero) / float64(a.count))
			row.AvgNodes = round4(float64(a.nodesSum) / float64(a.count))
		}
		row.P50MS = round4(percentile(a.durations, 0.50))
		row.P95MS = round4(percentile(a.durations, 0.95))
		toolRows = append(toolRows, row)
	}
	sort.Slice(toolRows, func(i, j int) bool {
		if toolRows[i].Count != toolRows[j].Count {
			return toolRows[i].Count > toolRows[j].Count
		}
		return toolRows[i].Tool < toolRows[j].Tool
	})

	// Top zero-result questions.
	zeroRows := make([]zeroQ, 0, len(zeroCounter))
	for _, zq := range zeroCounter {
		zeroRows = append(zeroRows, *zq)
	}
	sort.Slice(zeroRows, func(i, j int) bool {
		if zeroRows[i].Count != zeroRows[j].Count {
			return zeroRows[i].Count > zeroRows[j].Count
		}
		return zeroRows[i].Question < zeroRows[j].Question
	})
	if len(zeroRows) > top {
		zeroRows = zeroRows[:top]
	}

	zeroRate := 0.0
	if totalQueries > 0 {
		zeroRate = round4(float64(totalZero) / float64(totalQueries))
	}

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "scanned=%d queries=%d zero=%d (%.1f%%)\n", scanned, totalQueries, totalZero, zeroRate*100)
		for _, t := range toolRows {
			fmt.Fprintf(&b, "%-22s n=%-5d zero=%-4d (%.0f%%) p50=%.1fms p95=%.1fms avg_nodes=%.1f\n",
				t.Tool, t.Count, t.Zero, t.ZeroRate*100, t.P50MS, t.P95MS, t.AvgNodes)
		}
		if len(zeroRows) > 0 {
			b.WriteString("\nzero-result questions:\n")
			for _, z := range zeroRows {
				fmt.Fprintf(&b, "  [%dx] %s: %s\n", z.Count, z.Tool, z.Question)
			}
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	out := map[string]any{
		"enabled":                true,
		"log_path":               path,
		"scanned":                scanned,
		"total":                  totalQueries,
		"zero_results":           totalZero,
		"zero_rate":              zeroRate,
		"first_ts":               firstTS,
		"last_ts":                lastTS,
		"by_tool":                toolRows,
		"zero_result_questions":  zeroRows,
	}
	if includeRecent {
		n := len(filtered)
		start := n - 50
		if start < 0 {
			start = 0
		}
		out["recent"] = filtered[start:]
	}
	return s.respondJSONOrTOON(ctx, req, out)
}

// zeroQ tallies a distinct zero-result (tool, question) pair.
type zeroQ struct {
	Tool     string `json:"tool"`
	Question string `json:"question"`
	Corpus   string `json:"corpus,omitempty"`
	Count    int    `json:"count"`
}

// readQueryLogTail reads the JSONL query log and returns the newest
// `limit` parsed records (in chronological order) plus the total number
// of lines scanned. Malformed lines are skipped.
func readQueryLogTail(path string, limit int) ([]queryLogRecord, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()

	// Ring buffer of the last `limit` raw lines, then parse — bounds
	// memory regardless of log size.
	ring := make([]string, 0, limit)
	scanned := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate large response lines
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		scanned++
		if len(ring) < limit {
			ring = append(ring, line)
		} else {
			copy(ring, ring[1:])
			ring[len(ring)-1] = line
		}
	}
	if err := sc.Err(); err != nil {
		return nil, scanned, err
	}
	records := make([]queryLogRecord, 0, len(ring))
	for _, line := range ring {
		var r queryLogRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			records = append(records, r)
		}
	}
	return records, scanned, nil
}

// percentile returns the nearest-rank percentile of a sorted slice.
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := int(q * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// round4 rounds to 4 decimal places for stable wire output.
func round4(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}
