package savings

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Event is a single source-reading observation. The dashboard reads the
// event history (Store.EventsSince) to compute the Today / Last 7 days
// buckets the cumulative totals can't reconstruct on their own.
//
// Fields use compact JSON keys — they are also the line schema of the
// flat-file era's savings.jsonl log, which LoadEvents still parses for
// the one-shot legacy import.
type Event struct {
	TS        time.Time `json:"ts"`
	SessionID string    `json:"session,omitempty"`
	Repo      string    `json:"repo,omitempty"`
	Language  string    `json:"lang,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	Returned  int64     `json:"returned"`
	Saved     int64     `json:"saved"`
}

// LoadEvents reads a flat-file era JSONL log at path and returns events
// with ts >= since. since=zero returns everything. Returned events are in
// file order (oldest first). Malformed lines are skipped silently — they
// only happen when a previous gortex process crashed mid-write and the
// legacy import should keep working anyway.
func LoadEvents(path string, since time.Time) ([]Event, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open events log: %w", err)
	}
	defer func() { _ = f.Close() }()

	out := make([]Event, 0, 64)
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			// Strip trailing newline before unmarshal; tolerate
			// CRLF too in case the file was edited on Windows.
			line = trimNewline(line)
			if len(line) > 0 {
				var ev Event
				if jerr := json.Unmarshal(line, &ev); jerr == nil {
					if since.IsZero() || !ev.TS.Before(since) {
						out = append(out, ev)
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return out, fmt.Errorf("read events log: %w", err)
		}
	}
	return out, nil
}

// trimNewline strips at most one trailing \n and one trailing \r so
// parsers see the bare JSON object.
func trimNewline(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	return b
}

// Bucket is a windowed roll-up of events: top-line totals plus an optional
// per-tool breakdown sorted by tokens-saved descending. Used by the
// `gortex savings` dashboard.
type Bucket struct {
	Label   string
	Totals  Totals
	PerTool []ToolTotal // nil when no events fell in this bucket
}

// ToolTotal is one row of the per-tool breakdown.
type ToolTotal struct {
	Tool string
	Totals
}

// AggregateByTool folds events into a top-line Totals + a sorted per-tool
// breakdown. Tool keys are normalized: empty tool names group under
// "(unknown)" so they're still visible in --verbose.
func AggregateByTool(events []Event) (Totals, []ToolTotal) {
	var total Totals
	per := make(map[string]*Totals)
	for _, ev := range events {
		total.TokensSaved += ev.Saved
		total.TokensReturned += ev.Returned
		total.CallsCounted++
		name := ev.Tool
		if name == "" {
			name = "(unknown)"
		}
		t := per[name]
		if t == nil {
			t = &Totals{}
			per[name] = t
		}
		t.TokensSaved += ev.Saved
		t.TokensReturned += ev.Returned
		t.CallsCounted++
	}
	rows := make([]ToolTotal, 0, len(per))
	for name, t := range per {
		rows = append(rows, ToolTotal{Tool: name, Totals: *t})
	}
	sort.Slice(rows, func(i, j int) bool {
		if a, b := rows[i].TokensSaved, rows[j].TokensSaved; a != b {
			return a > b
		}
		return rows[i].Tool < rows[j].Tool
	})
	return total, rows
}

// FilterSince returns the subset of events whose TS is >= since.
func FilterSince(events []Event, since time.Time) []Event {
	if since.IsZero() {
		return events
	}
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		if !ev.TS.Before(since) {
			out = append(out, ev)
		}
	}
	return out
}

// FilterDay returns events whose TS falls on the given calendar day in loc.
func FilterDay(events []Event, day time.Time, loc *time.Location) []Event {
	if loc == nil {
		loc = time.UTC
	}
	y, m, d := day.In(loc).Date()
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		ey, em, ed := ev.TS.In(loc).Date()
		if ey == y && em == m && ed == d {
			out = append(out, ev)
		}
	}
	return out
}

// BuildDashboard returns the three canonical buckets (Today / Last 7 days /
// All time) from the last week's events (oldest first), using `now` as the
// reference clock and `loc` as the calendar for the "Today" boundary.
// storeAllTime supplies the All-time totals and allPerTool its per-tool
// breakdown — both come from the ledger's aggregates, so callers never
// materialize the full event history just to render the dashboard.
func BuildDashboard(weekEvents []Event, storeAllTime Totals, allPerTool []ToolTotal, now time.Time, loc *time.Location) []Bucket {
	if loc == nil {
		loc = time.Local
	}
	weekEvents = FilterSince(weekEvents, now.Add(-7*24*time.Hour))

	todayEvents := FilterDay(weekEvents, now, loc)
	todayTotals, todayPerTool := AggregateByTool(todayEvents)
	weekTotals, weekPerTool := AggregateByTool(weekEvents)

	return []Bucket{
		{Label: "Today", Totals: todayTotals, PerTool: todayPerTool},
		{Label: "Last 7 days", Totals: weekTotals, PerTool: weekPerTool},
		{Label: "All time", Totals: storeAllTime, PerTool: allPerTool},
	}
}

// SavingsPercent returns the percentage of "full file size" tokens that
// were avoided, clamped to [0, 100]. A bucket with no calls returns 0.
func SavingsPercent(t Totals) float64 {
	denom := t.TokensSaved + t.TokensReturned
	if denom <= 0 {
		return 0
	}
	pct := float64(t.TokensSaved) / float64(denom) * 100.0
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// BarString renders a 16-cell █/░ bar for pct in [0, 100]. The cell count
// is configurable so tests and future widths don't hardcode 16.
func BarString(pct float64, cells int) string {
	if cells <= 0 {
		cells = 16
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := min(int(pct/100.0*float64(cells)+0.5), cells)
	var sb strings.Builder
	sb.Grow(cells * 3)
	for range filled {
		sb.WriteString("█")
	}
	for range cells - filled {
		sb.WriteString("░")
	}
	return sb.String()
}
