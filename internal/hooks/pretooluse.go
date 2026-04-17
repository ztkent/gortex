// Package hooks provides Claude Code hook handlers for Gortex.
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// HookInput is the JSON structure Claude Code sends to PreToolUse hooks via stdin.
type HookInput struct {
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	CWD           string         `json:"cwd"`
}

// HookOutput is the JSON structure the hook writes to stdout.
type HookOutput struct {
	HookSpecificOutput *HookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

// HookSpecificOutput carries the permission decision and/or additional context.
type HookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

// enrichResult carries both the context text and whether the call should be blocked.
type enrichResult struct {
	context string
	deny    bool
	reason  string
}

// RunPreToolUse reads a PreToolUse hook payload from stdin and handles it.
// Kept as a public entry point for backward compatibility; new callers should
// use Run which dispatches based on hook_event_name.
func RunPreToolUse(gortexPort int) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	runPreToolUse(data, gortexPort)
}

// runPreToolUse is the bytes-accepting helper used by both RunPreToolUse and
// the generic Run dispatcher.
func runPreToolUse(data []byte, gortexPort int) {
	var input HookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}

	if input.HookEventName != "PreToolUse" {
		return
	}

	result := enrich(input, gortexPort)
	if result.context == "" && !result.deny {
		return
	}

	output := HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName: "PreToolUse",
		},
	}

	if result.deny {
		output.HookSpecificOutput.PermissionDecision = "deny"
		output.HookSpecificOutput.PermissionDecisionReason = result.reason
	} else {
		output.HookSpecificOutput.AdditionalContext = result.context
	}

	out, err := json.Marshal(output)
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

func enrich(input HookInput, port int) enrichResult {
	switch input.ToolName {
	case "Read":
		return enrichRead(input.ToolInput, port)
	case "Grep":
		return enrichGrep(input.ToolInput, port)
	case "Glob":
		return enrichGlob(input.ToolInput)
	case "Task":
		return enrichTask(input.ToolInput, port)
	default:
		return enrichResult{}
	}
}

// enrichRead blocks whole-file reads of indexed source files and suggests graph tools.
// Narrow reads (with offset+limit for editing) are allowed through with advisory context.
func enrichRead(toolInput map[string]any, port int) enrichResult {
	filePath, ok := toolInput["file_path"].(string)
	if !ok || filePath == "" {
		return enrichResult{}
	}

	// Skip non-source files — allow reading .md, .yaml, .json, etc.
	if !looksLikeSourceFile(filePath) {
		return enrichResult{}
	}

	// Detect narrow reads (offset+limit for editing). These are legitimate
	// and should pass through — the agent already knows what it needs.
	if isNarrowRead(toolInput) {
		return enrichResult{}
	}

	// Check if Gortex has this file indexed (bridge must be running).
	fileIndexed := false
	symbolCount := 0
	resp, err := queryGortex(port, "/api/graph/file?path="+url.QueryEscape(filePath))
	if err == nil && resp != "" {
		var result struct {
			Nodes []any `json:"nodes"`
		}
		if json.Unmarshal([]byte(resp), &result) == nil && len(result.Nodes) > 1 {
			fileIndexed = true
			symbolCount = len(result.Nodes) - 1 // subtract the file node
		}
	}

	// If the file is indexed, BLOCK the read and provide graph alternatives.
	if fileIndexed {
		var reason strings.Builder
		fmt.Fprintf(&reason, "[Gortex] BLOCKED: Read of %s (%d symbols indexed). Use graph tools instead:\n", filePath, symbolCount)
		reason.WriteString("  - `get_symbol_source` — read one symbol (80%% fewer tokens)\n")
		reason.WriteString("  - `get_editing_context` — full file context before editing\n")
		reason.WriteString("  - `get_file_summary` — all symbols and imports\n")
		reason.WriteString("  - `smart_context` — task-aware minimal context\n")
		reason.WriteString("  - `batch_symbols` — multiple symbols in one call\n")
		reason.WriteString(gcxTip)

		return enrichResult{
			deny:   true,
			reason: reason.String(),
		}
	}

	// File not indexed — allow with advisory.
	var guidance strings.Builder
	guidance.WriteString("[Gortex] PREFER graph tools over Read for source files:\n")
	guidance.WriteString("  - To read one symbol: use `get_symbol_source` (80% fewer tokens)\n")
	guidance.WriteString("  - To understand a file before editing: use `get_editing_context`\n")
	guidance.WriteString("  - To get a file overview: use `get_file_summary`\n")
	guidance.WriteString("  - For task-level context: use `smart_context`\n")
	guidance.WriteString(gcxTip)

	return enrichResult{context: guidance.String()}
}

// gcxTip is appended to every Read/Grep/Glob redirect so agents see the
// GCX1 wire-format opt-in at the exact moment they are picking a tool
// call. Kept short — the messages are read under token pressure.
const gcxTip = "  - Tip: pass format:\"gcx\" to any of these for round-trippable compact output (~27% fewer tokens, spec: docs/wire-format.md).\n"

// isNarrowRead returns true if the Read has offset+limit targeting a small range,
// indicating the agent is reading a specific section for editing.
func isNarrowRead(toolInput map[string]any) bool {
	_, hasOffset := toolInput["offset"]
	_, hasLimit := toolInput["limit"]

	if hasOffset && hasLimit {
		// Any offset+limit read is considered narrow (the agent knows what it wants).
		return true
	}

	if hasOffset {
		// Offset alone means "read from this line" — likely targeted.
		return true
	}

	if hasLimit {
		// Limit alone — check if it's a small read.
		if limitVal, ok := toFloat64(toolInput["limit"]); ok && limitVal <= 50 {
			return true
		}
	}

	return false
}

// grepProbeTimeout caps the search_symbols probe so hooks never slow Grep.
const grepProbeTimeout = 200 * time.Millisecond

// grepSymbolHit mirrors daemon.SymbolHit but lives in this package so the
// probe interface can be swapped for tests without dragging the full
// daemon-protocol types into hook unit tests.
type grepSymbolHit struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
}

// errProbeTimeout is the sentinel returned by the probe when the daemon
// didn't reply within grepProbeTimeout. Differentiates from "daemon
// unreachable" / "no hits" so telemetry can record it correctly.
var errProbeTimeout = errors.New("probe timeout")

// errDaemonUnreachable is returned when the daemon socket can't be dialed
// (no daemon, wrong path, permissions). Treated as "no signal" — fall
// through to soft guidance, do not log telemetry.
var errDaemonUnreachable = errors.New("daemon unreachable")

// grepProbeFn is the function the Grep enrichment uses to query the
// graph for symbol matches. Defaults to the daemon-socket implementation;
// tests swap it for a stub.
type grepProbeFn func(pattern string, timeout time.Duration) ([]grepSymbolHit, error)

// grepProbe is the indirection point. Production reads probeViaDaemon;
// tests reassign this var via a t.Cleanup-restored helper.
var grepProbe grepProbeFn = probeViaDaemon

// enrichGrep classifies the Grep pattern and, for symbol-shaped patterns,
// probes the local daemon's search_symbols endpoint. On ≥1 hit the call is
// denied with top matches and a bypass hint; on miss/timeout/non-symbol the
// existing soft guidance is returned so Grep proceeds.
func enrichGrep(toolInput map[string]any, _ int) enrichResult {
	pattern, ok := toolInput["pattern"].(string)
	if !ok || pattern == "" {
		return enrichResult{}
	}

	guidance := defaultGrepGuidance()

	if classifyGrepPattern(pattern) != GrepPatternSymbol {
		if len(pattern) > 2 {
			logHookDecision("Grep", pattern, DecisionSkippedNonSymbol, 0, 0)
			return enrichResult{context: guidance}
		}
		return enrichResult{}
	}

	start := time.Now()
	hits, err := grepProbe(pattern, grepProbeTimeout)
	dur := time.Since(start)
	switch {
	case errors.Is(err, errProbeTimeout):
		logHookDecision("Grep", pattern, DecisionTimedOut, 0, dur)
		return enrichResult{context: guidance}
	case errors.Is(err, errDaemonUnreachable):
		// No daemon = no signal. Don't pollute telemetry with infra noise.
		return enrichResult{context: guidance}
	case err != nil:
		// Other transport/decode failure — treat as miss so we have a record.
		logHookDecision("Grep", pattern, DecisionProbedMiss, 0, dur)
		return enrichResult{context: guidance}
	}

	if len(hits) == 0 {
		logHookDecision("Grep", pattern, DecisionProbedMiss, 0, dur)
		return enrichResult{context: guidance}
	}

	logHookDecision("Grep", pattern, DecisionProbedHit, len(hits), dur)
	return enrichResult{
		deny:   true,
		reason: formatGrepDeny(pattern, hits),
	}
}

func defaultGrepGuidance() string {
	var b strings.Builder
	b.WriteString("[Gortex] PREFER graph tools over Grep:\n")
	b.WriteString("  - To find a symbol by name: use `search_symbols` (BM25 + camelCase-aware)\n")
	b.WriteString("  - To find all references: use `find_usages` (zero false positives)\n")
	b.WriteString("  - To find callers: use `get_callers`\n")
	b.WriteString("  - To find implementations: use `find_implementations`\n")
	b.WriteString(gcxTip)
	return b.String()
}

func formatGrepDeny(pattern string, hits []grepSymbolHit) string {
	const maxShown = 5
	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] BLOCKED: \"%s\" matches %d symbol(s) in the knowledge graph. Use `search_symbols` or `find_usages` instead:\n\n", pattern, len(hits))
	shown := min(len(hits), maxShown)
	for i := range shown {
		h := hits[i]
		kind := h.Kind
		if kind == "" {
			kind = "symbol"
		}
		fmt.Fprintf(&b, "  %s — %s:%d (%s)\n", h.Name, h.FilePath, h.Line, kind)
	}
	if len(hits) > maxShown {
		fmt.Fprintf(&b, "  ... and %d more\n", len(hits)-maxShown)
	}
	b.WriteString("\n")
	b.WriteString(gcxTip)
	b.WriteString("To force text search, add a regex metachar (e.g. \\b) or quote the pattern.")
	return b.String()
}

// enrichGlob suggests graph alternatives for file discovery.
// Glob is not blocked — it's needed for file pattern matching.
func enrichGlob(toolInput map[string]any) enrichResult {
	pattern, ok := toolInput["pattern"].(string)
	if !ok || pattern == "" {
		return enrichResult{}
	}

	// Only intervene for source file patterns.
	sourceExts := []string{
		".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java",
		".kt", ".scala", ".swift", ".php", ".rb", ".ex", ".c", ".cpp",
		".cs", ".dart", ".lua", ".zig", ".ml", ".hs",
	}
	isSourceGlob := false
	lower := strings.ToLower(pattern)
	for _, ext := range sourceExts {
		if strings.HasSuffix(lower, ext) {
			isSourceGlob = true
			break
		}
	}
	if !isSourceGlob {
		return enrichResult{}
	}

	return enrichResult{
		context: "[Gortex] PREFER graph tools over Glob for source files:\n" +
			"  - To find a symbol by name: use `search_symbols`\n" +
			"  - To find files containing a symbol: use `search_symbols` (returns file paths)\n" +
			"  - To understand file structure: use `get_file_summary`\n" +
			"  - For task-level file discovery: use `smart_context`\n" +
			gcxTip,
	}
}

func queryGortex(port int, path string) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d%s", port, path))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func looksLikeSourceFile(path string) bool {
	sourceExts := []string{
		".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java",
		".kt", ".scala", ".swift", ".php", ".rb", ".ex", ".exs",
		".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".cs",
		".sql", ".proto", ".sh", ".bash",
	}
	lower := strings.ToLower(path)
	for _, ext := range sourceExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// toFloat64 attempts to convert an any value to float64.
// JSON numbers are decoded as float64 by encoding/json.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
