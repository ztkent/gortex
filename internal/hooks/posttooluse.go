package hooks

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// postHookInput is the PostToolUse payload Claude Code sends. It differs
// from PreToolUse only by carrying tool_response — the textual output
// the tool produced. We deliberately decode tool_response as `any` because
// each tool returns a different shape (Grep emits text with file:line
// matches, Glob emits a newline-separated file list, Read emits raw file
// content). The shape-specific handlers below normalise to a string.
type postHookInput struct {
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	ToolResponse  any            `json:"tool_response"`
	CWD           string         `json:"cwd"`
}

// runPostToolUse parses the PostToolUse payload and appends graph
// enrichment as additionalContext when the tool was a Grep / Glob / Read
// that touched the indexed graph. Other tools fall through to a no-op —
// PostToolUse must never block the run.
//
// The handler is shape-aware:
//   - Grep: parse "path:line:text" lines, look up the enclosing symbol
//     in the graph for the first few hits, append name + kind + caller
//     count so the agent sees graph-grade follow-ups inline.
//   - Glob: count the matched files, look up symbol counts per file,
//     append a short summary so the agent can pick the right one without
//     a follow-up tool call.
//   - Read: look up the file's symbol count, top callers, and community
//     so the agent knows where the file lives before deciding to act.
func runPostToolUse(data []byte, port int) {
	var input postHookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "PostToolUse" {
		return
	}

	var ctx string
	switch input.ToolName {
	case "Grep":
		ctx = postGrep(input, port)
	case "Glob":
		ctx = postGlob(input, port)
	case "Read":
		ctx = postRead(input, port)
	}
	if ctx == "" {
		return
	}

	output := HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     "PostToolUse",
			AdditionalContext: ctx,
		},
	}
	out, err := json.Marshal(output)
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

// grepHitLineRe matches the leading "<path>:<line>" of a ripgrep-style
// hit (Claude Code's Grep tool uses ripgrep underneath). Captures the
// path and line number; the rest of the line — the matched text — is
// discarded.
var grepHitLineRe = regexp.MustCompile(`^([^:]+):(\d+):`)

// postGrep parses ripgrep-style match lines from tool_response and adds
// "enclosing symbol" lookups for the first few hits so the agent doesn't
// have to follow up with find_usages / get_callers manually.
func postGrep(input postHookInput, port int) string {
	body := responseText(input.ToolResponse)
	if body == "" {
		return ""
	}
	hits := parseGrepHits(body)
	if len(hits) == 0 {
		return ""
	}

	const maxLookup = 5
	enriched := make([]string, 0, maxLookup)
	for _, h := range hits {
		if len(enriched) >= maxLookup {
			break
		}
		sym := lookupEnclosingSymbol(port, h.path, h.line)
		if sym == "" {
			continue
		}
		enriched = append(enriched, fmt.Sprintf("  %s:%d → %s", h.path, h.line, sym))
	}
	if len(enriched) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] Graph context for %d of %d Grep hit(s):\n", len(enriched), len(hits))
	for _, line := range enriched {
		b.WriteString(line + "\n")
	}
	b.WriteString("Follow-up: pass any symbol ID to `find_usages` / `get_callers` for blast radius without re-Grep.\n")
	return b.String()
}

// postGlob counts the matched files and looks up symbol counts per file
// so the agent can rank them by relevance without another Read/Grep
// roundtrip. Files that aren't indexed are still counted but only
// reported in aggregate ("12 indexed / 3 unindexed").
func postGlob(input postHookInput, port int) string {
	body := responseText(input.ToolResponse)
	if body == "" {
		return ""
	}
	paths := parseGlobPaths(body)
	if len(paths) == 0 {
		return ""
	}

	type fileSummary struct {
		path    string
		symbols int
	}
	const maxFiles = 8
	indexed := make([]fileSummary, 0, maxFiles)
	unindexed := 0
	for _, p := range paths {
		ok, n := queryFileIndexed(port, p)
		if !ok {
			unindexed++
			continue
		}
		if len(indexed) < maxFiles {
			indexed = append(indexed, fileSummary{path: p, symbols: n})
		}
	}
	if len(indexed) == 0 {
		return ""
	}

	// Sort by symbol count desc — the largest files are usually the
	// most interesting for "where is logic concentrated?" queries.
	sort.Slice(indexed, func(i, j int) bool { return indexed[i].symbols > indexed[j].symbols })

	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] Indexed %d/%d Glob match(es):\n", len(paths)-unindexed, len(paths))
	for _, f := range indexed {
		fmt.Fprintf(&b, "  %s — %d symbol(s)\n", f.path, f.symbols)
	}
	if len(paths)-unindexed > len(indexed) {
		fmt.Fprintf(&b, "  ... and %d more indexed file(s)\n", (len(paths)-unindexed)-len(indexed))
	}
	b.WriteString("Follow-up: `get_file_summary` for any single file; `get_repo_outline` for the whole workspace shape.\n")
	return b.String()
}

// postRead enriches a Read by reporting the file's graph footprint —
// symbol count, top community, top callers — so the agent sees where
// the file sits in the codebase. Files outside the graph are silently
// skipped.
func postRead(input postHookInput, port int) string {
	filePath, _ := input.ToolInput["file_path"].(string)
	if filePath == "" {
		return ""
	}
	ok, symbolCount := queryFileIndexed(port, filePath)
	if !ok {
		return ""
	}

	callers := lookupFileCallerCount(port, filePath)

	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] Graph footprint for %s:\n", filePath)
	fmt.Fprintf(&b, "  %d indexed symbol(s)\n", symbolCount)
	if callers > 0 {
		fmt.Fprintf(&b, "  %d unique external caller(s)\n", callers)
	}
	b.WriteString("Follow-up: `get_file_summary` / `get_editing_context` returns the same info plus signatures, no re-Read needed.\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// Response normalisation
// ---------------------------------------------------------------------------

// responseText extracts a plain string from tool_response regardless of
// the shape Claude Code sent: bare string, {"content": "..."}, or
// {"output": "..."}. Unknown shapes return "" — the handler then no-ops.
func responseText(v any) string {
	switch r := v.(type) {
	case string:
		return r
	case map[string]any:
		for _, k := range []string{"content", "output", "stdout", "text"} {
			if s, ok := r[k].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// parseGrepHits scans the response for "<path>:<line>:" prefixes. Lines
// that don't match the shape are ignored — Claude Code's Grep tool can
// emit summary lines (e.g. "Found 4 files") that aren't hits.
type grepHit struct {
	path string
	line int
}

func parseGrepHits(body string) []grepHit {
	var out []grepHit
	seen := make(map[string]bool)
	for ln := range strings.SplitSeq(body, "\n") {
		m := grepHitLineRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		path := m[1]
		lineNum, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		key := path + ":" + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, grepHit{path: path, line: lineNum})
	}
	return out
}

// parseGlobPaths splits the response on newlines, trims whitespace, and
// keeps non-empty entries that look like file paths (skip summary
// strings). The Glob tool emits one path per line.
func parseGlobPaths(body string) []string {
	var out []string
	for ln := range strings.SplitSeq(body, "\n") {
		p := strings.TrimSpace(ln)
		if p == "" {
			continue
		}
		// Skip "(no matches)" / "Found N files" preambles.
		if strings.HasPrefix(p, "(") || strings.HasPrefix(p, "Found ") {
			continue
		}
		// Must look like a path (have an extension or a slash). This
		// is a conservative filter — anything else gets dropped so we
		// don't try to graph-lookup commentary lines.
		if !strings.Contains(p, "/") && !strings.Contains(p, ".") {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ---------------------------------------------------------------------------
// Graph lookups
// ---------------------------------------------------------------------------

// lookupEnclosingSymbol asks the daemon for the symbol containing the
// given path:line position. Returns a one-line label suitable for
// inclusion in additionalContext, or "" on any failure / miss. Failures
// are silent — PostToolUse must never block the agent.
func lookupEnclosingSymbol(port int, path string, line int) string {
	q := fmt.Sprintf("/api/graph/symbol-at?path=%s&line=%d",
		url.QueryEscape(path), line)
	resp, err := queryGortex(port, q)
	if err != nil || resp == "" {
		return ""
	}
	var result struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		return ""
	}
	if result.Name == "" {
		return ""
	}
	if result.Kind == "" {
		return result.Name
	}
	return fmt.Sprintf("%s %s", result.Kind, result.Name)
}

// lookupFileCallerCount asks the daemon how many unique callers
// reference symbols in the given file from outside the file. Used by
// postRead to surface "this file is load-bearing — N external callers"
// without forcing the agent into a follow-up get_dependents call. A
// zero / error result returns 0; postRead then omits the line.
func lookupFileCallerCount(port int, filePath string) int {
	q := "/api/graph/file-callers?path=" + url.QueryEscape(filePath)
	resp, err := queryGortex(port, q)
	if err != nil || resp == "" {
		return 0
	}
	var result struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		return 0
	}
	return result.Count
}

