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

	"github.com/zzet/gortex/internal/daemon"
)

// HookInput is the JSON structure Claude Code sends to PreToolUse hooks via stdin.
type HookInput struct {
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	CWD           string         `json:"cwd"`
	// SessionID identifies the Claude Code session. Used to key the
	// per-session state store (consult-unlock marker, nudge streak).
	SessionID string `json:"session_id"`
	// PermissionMode is the host's active permission posture
	// ("default" / "acceptEdits" / "plan" / "bypassPermissions" / "auto").
	// Drives the auto-approve branch for Gortex's own MCP tools.
	PermissionMode string `json:"permission_mode"`
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

// RunPreToolUse reads a PreToolUse hook payload from stdin and handles it
// in the legacy deny posture. Kept as a public entry point for
// backward compatibility; new callers should use Run which dispatches
// based on hook_event_name and respects the configured Mode.
func RunPreToolUse(gortexPort int) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	runPreToolUse(data, gortexPort, ModeDeny)
}

// gortexMCPToolPrefix is the namespace Claude Code gives Gortex's own
// MCP tools (server name "gortex"). A tool call whose name starts with
// this prefix is a graph query — the in-process hook sees it like any
// other tool call, which is what lets the consult-unlock handshake and
// the adaptive-nudge streak reset work without an external signal.
const gortexMCPToolPrefix = "mcp__gortex__"

// runPreToolUse is the bytes-accepting helper used by both RunPreToolUse and
// the generic Run dispatcher. In ModeEnrich the deny branch is downgraded
// to an additionalContext message — the agent is informed about the graph
// alternative but the original call still runs and PostToolUse can layer
// graph context on the actual output.
func runPreToolUse(data []byte, gortexPort int, mode Mode) {
	var input HookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}

	if input.HookEventName != "PreToolUse" {
		return
	}

	isGortexMCP := strings.HasPrefix(input.ToolName, gortexMCPToolPrefix)

	// Consult-unlock handshake: any Gortex MCP tool call records that
	// the agent has consulted the graph this session. The hook sees the
	// MCP call in-process, so the marker is fully self-contained. The
	// call itself is a no-op pass-through — nothing to enrich.
	if mode == ModeConsultUnlock && isGortexMCP {
		st := loadSessionState(input.SessionID)
		if !st.GraphConsulted {
			st.GraphConsulted = true
			saveSessionState(input.SessionID, st)
		}
		return
	}

	result := enrich(input, gortexPort)

	// In enrich mode no PreToolUse call is ever denied. The agent
	// keeps making whatever tool call it intended; the deny rationale
	// becomes a soft tip surfaced via additionalContext, and the
	// actual graph value lands in the PostToolUse handler that sees
	// the tool's response.
	if mode == ModeEnrich && result.deny {
		result = enrichResult{context: downgradeReason(result)}
	}

	// Consult-unlock mode: a deny stays hard until the agent has
	// queried the graph at least once this session. After the first
	// mcp__gortex__* call the marker is set and the deny is downgraded
	// to soft context (same shape as ModeEnrich). Before that the deny
	// holds, but its reason explains exactly how to unlock fallback.
	if mode == ModeConsultUnlock && result.deny {
		if loadSessionState(input.SessionID).GraphConsulted {
			result = enrichResult{context: downgradeReason(result)}
		} else {
			result.reason = consultUnlockReason(result.reason)
		}
	}

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

	emitPreToolUse(output)
}

// emitPreToolUse marshals a PreToolUse HookOutput to stdout. A marshal
// failure is swallowed — a hook must never block Claude Code's flow.
func emitPreToolUse(output HookOutput) {
	out, err := json.Marshal(output)
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

// downgradeReason picks the human text to surface when a deny is
// softened to additionalContext: the deny reason if present, else the
// advisory context. Shared by ModeEnrich and ModeConsultUnlock.
func downgradeReason(result enrichResult) string {
	if result.reason != "" {
		return result.reason
	}
	return result.context
}

// consultUnlockReason augments a hard deny reason with the one-line
// instruction for unlocking fallback file reads under ModeConsultUnlock.
func consultUnlockReason(reason string) string {
	const unlock = "\n[Gortex] consult-unlock: query the Gortex graph once (any mcp__gortex__ tool) to unlock fallback file reads for the rest of this session."
	if reason == "" {
		return strings.TrimPrefix(unlock, "\n")
	}
	return reason + unlock
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
	case "Bash":
		return enrichBash(input.ToolInput, port)
	case "Edit":
		return enrichEdit(input.ToolInput, port)
	case "Write":
		return enrichWrite(input.ToolInput, port)
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

	fileIndexed, symbolCount := queryFileIndexed(port, filePath)

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
	pattern, _ := toolInput["pattern"].(string)
	return probeSymbolPattern("Grep", pattern, defaultGrepGuidance())
}

// probeSymbolPattern is the shared body of enrichGrep and the grep-/find-like
// branches of enrichBash. Given a pattern, it gates on symbol-shape, probes
// the daemon, and returns deny-with-hits or soft guidance. Telemetry is
// attributed to the `tool` label so Grep- vs Bash-sourced probes stay
// distinguishable in `hook-decisions.jsonl`.
func probeSymbolPattern(tool, pattern, guidance string) enrichResult {
	if pattern == "" {
		return enrichResult{}
	}

	if classifyGrepPattern(pattern) != GrepPatternSymbol {
		if len(pattern) > 2 {
			logHookDecision(tool, pattern, DecisionSkippedNonSymbol, 0, 0)
			return enrichResult{context: guidance}
		}
		return enrichResult{}
	}

	start := time.Now()
	hits, err := grepProbe(pattern, grepProbeTimeout)
	dur := time.Since(start)
	switch {
	case errors.Is(err, errProbeTimeout):
		logHookDecision(tool, pattern, DecisionTimedOut, 0, dur)
		return enrichResult{context: guidance}
	case errors.Is(err, errDaemonUnreachable):
		// No daemon = no signal. Don't pollute telemetry with infra noise.
		return enrichResult{context: guidance}
	case err != nil:
		// Other transport/decode failure — treat as miss so we have a record.
		logHookDecision(tool, pattern, DecisionProbedMiss, 0, dur)
		return enrichResult{context: guidance}
	}

	if len(hits) == 0 {
		logHookDecision(tool, pattern, DecisionProbedMiss, 0, dur)
		return enrichResult{context: guidance}
	}

	logHookDecision(tool, pattern, DecisionProbedHit, len(hits), dur)
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
	b.WriteString("  - For TODO / FIXME / HACK / XXX / NOTE patterns: use `analyze kind=todos` (filter by tag/assignee/ticket)\n")
	b.WriteString("  - For HTTP route / handler patterns (e.g. `app.get`, `func.*Handler`, `@RequestMapping`): use `contracts` (action=list to enumerate, action=check to match cross-repo)\n")
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

// queryFileIndexed asks the local bridge whether the file at filePath is
// indexed, returning the symbol count when it is. Shared by enrichRead and
// enrichBash. A zero return (false, 0) is the "no signal" case — daemon
// unreachable, malformed response, or file genuinely not indexed; callers
// treat all three the same (fall through to soft guidance).
func queryFileIndexed(port int, filePath string) (bool, int) {
	resp, err := queryGortex(port, "/api/graph/file?path="+url.QueryEscape(filePath))
	if err != nil || resp == "" {
		return false, 0
	}
	var result struct {
		Nodes []any `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		return false, 0
	}
	if len(result.Nodes) <= 1 {
		return false, 0
	}
	return true, len(result.Nodes) - 1 // subtract the file node itself
}

// enrichBash classifies the Bash command and routes codebase-search shapes
// through the same graph probes the Grep and Read enrichments use. Anything
// not recognised as a codebase search passes through silently — false-deny
// is more disruptive than a miss, so the classifier only flags primary
// grep/rg/find-name/cat-source invocations.
func enrichBash(toolInput map[string]any, port int) enrichResult {
	cmd, _ := toolInput["command"].(string)
	if cmd == "" {
		return enrichResult{}
	}

	c := classifyBashCommand(cmd)
	switch c.Action {
	case BashActionGrepLike:
		return probeSymbolPattern("Bash", c.Pattern, defaultGrepGuidance())

	case BashActionFindName:
		// find -name values often include `*` globs; the classifier has
		// already stripped wildcards, but the residue may still be
		// non-symbol-shaped (e.g. ".go" from `-name "*.go"`) — let
		// probeSymbolPattern decide.
		return probeSymbolPattern("Bash", c.Pattern, defaultGrepGuidance())

	case BashActionReadSource:
		indexed, symbolCount := queryFileIndexed(port, c.Path)
		if indexed {
			var reason strings.Builder
			fmt.Fprintf(&reason,
				"[Gortex] BLOCKED: Bash `%s %s` reads indexed source (%d symbols). Use graph tools instead:\n",
				c.Primary, c.Path, symbolCount)
			reason.WriteString("  - `get_symbol_source` — one symbol (80% fewer tokens)\n")
			reason.WriteString("  - `get_file_summary` — all symbols and imports\n")
			reason.WriteString("  - `get_editing_context` — full file context before editing\n")
			reason.WriteString(gcxTip)
			return enrichResult{deny: true, reason: reason.String()}
		}
		// Not indexed — soft guidance so Bash proceeds.
		var g strings.Builder
		g.WriteString("[Gortex] PREFER graph tools over Bash cat/head/tail for source files:\n")
		g.WriteString("  - To read one symbol: use `get_symbol_source` (80% fewer tokens)\n")
		g.WriteString("  - To get a file overview: use `get_file_summary`\n")
		g.WriteString("  - To understand a file before editing: use `get_editing_context`\n")
		g.WriteString(gcxTip)
		return enrichResult{context: g.String()}
	}

	return enrichResult{}
}

// daemonReachableFn is the seam tests use to fake daemon availability
// without a real socket. Production reads daemon.IsRunning.
var daemonReachableFn = daemon.IsRunning

// enrichGlob denies "list all source files of extension X" patterns
// when the daemon is reachable — those are exactly the queries the
// graph already answers (via `get_repo_outline` / `search_symbols`).
// Name-based patterns (e.g. `**/handler*.go`, `*test*.ts`) get soft
// guidance only because grep-style filename search has no clean
// graph equivalent. When the daemon is unreachable, every shape
// degrades to soft guidance — no daemon means no enforcement.
func enrichGlob(toolInput map[string]any) enrichResult {
	pattern, ok := toolInput["pattern"].(string)
	if !ok || pattern == "" {
		return enrichResult{}
	}
	if !looksLikeSourceFile(pattern) {
		return enrichResult{}
	}

	guidance := defaultGlobGuidance()

	// Greedy source-ext patterns (`**/*.go`, `*.ts`) are the
	// "enumerate every source file" shape. Hard-deny only when the
	// daemon is up — we can't redirect to graph tools that aren't
	// answering.
	if isGreedySourceGlob(pattern) && daemonReachableFn() {
		var b strings.Builder
		fmt.Fprintf(&b, "[Gortex] BLOCKED: Glob `%s` enumerates source files. The graph already indexes them — use:\n", pattern)
		b.WriteString("  - `get_repo_outline` — every file with symbol counts\n")
		b.WriteString("  - `search_symbols` — name-based lookup that returns file paths\n")
		b.WriteString("  - `get_file_summary` — when you have a specific file in mind\n")
		b.WriteString(gcxTip)
		b.WriteString("If you genuinely need a file-system listing, run `find` or `ls` via Bash with a specific filename component — Glob deny only triggers on bare extension wildcards.")
		return enrichResult{deny: true, reason: b.String()}
	}

	return enrichResult{context: guidance}
}

// defaultGlobGuidance is the soft-guidance message returned when a
// Glob pattern targets source files but isn't a greedy "all of this
// extension" pattern, or when the daemon is unreachable.
func defaultGlobGuidance() string {
	return "[Gortex] PREFER graph tools over Glob for source files:\n" +
		"  - To find a symbol by name: use `search_symbols`\n" +
		"  - To find files containing a symbol: use `search_symbols` (returns file paths)\n" +
		"  - To understand file structure: use `get_file_summary`\n" +
		"  - For task-level file discovery: use `smart_context`\n" +
		"  - For migration / SQL globs (`db/migrations/*.sql`, `**/*.sql`): use `analyze kind=orphan_tables` and `kind=unreferenced_tables` to find queried-but-undeclared and provided-but-unused tables\n" +
		gcxTip
}

// isGreedySourceGlob returns true when the pattern is a bare
// extension wildcard like `*.go`, `**/*.ts`, `src/**/*.tsx`. The
// classifier looks at the segment between the last `/` and the
// extension: if it's just `*` (or `**` collapsed), the agent is
// asking for "every source file of this kind" — exactly the shape
// `get_repo_outline` answers. Anything else (a literal filename, a
// substring wildcard like `*test*.go`) is treated as name-based
// search and not denied.
func isGreedySourceGlob(pattern string) bool {
	last := pattern
	if idx := strings.LastIndex(pattern, "/"); idx >= 0 {
		last = pattern[idx+1:]
	}
	dot := strings.LastIndex(last, ".")
	if dot <= 0 {
		return false
	}
	stem := last[:dot]
	// Bare wildcard stems indicate "all files of this extension".
	return stem == "*" || stem == "**"
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

// editBlockingEnvVar gates Edit/Write enforcement. We ship behind a
// flag because Edit/Write redirects are higher-blast-radius than
// Read/Grep — false positives stop the agent from making any
// progress at all. Once we have field telemetry showing the
// classifier is reliable, the gate can flip default-on or be
// removed.
const editBlockingEnvVar = "GORTEX_HOOK_BLOCK_EDIT"

// editBlockingEnabled reports whether the env-gated Edit/Write
// redirect is on. Anything besides empty/"0"/"false"/"no"/"off"
// enables.
func editBlockingEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(editBlockingEnvVar)))
	switch v {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// enrichEdit redirects whole-file edits of indexed source to the
// Gortex MCP edit tools. Behind GORTEX_HOOK_BLOCK_EDIT until the
// classifier is proven; without it the hook is a no-op so Edit
// behaves exactly as it did pre-feature.
func enrichEdit(toolInput map[string]any, port int) enrichResult {
	if !editBlockingEnabled() {
		return enrichResult{}
	}
	filePath, ok := toolInput["file_path"].(string)
	if !ok || filePath == "" {
		return enrichResult{}
	}
	if !looksLikeSourceFile(filePath) {
		return enrichResult{}
	}
	indexed, _ := queryFileIndexed(port, filePath)
	if !indexed {
		return enrichResult{}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] BLOCKED: Edit of %s (indexed source). Use Gortex MCP edit tools — they don't require a prior Read and update the graph atomically:\n", filePath)
	b.WriteString("  - `edit_symbol` — change one symbol's body by ID (cleanest for one-function changes)\n")
	b.WriteString("  - `edit_file` — whole-file replace, no Read precondition\n")
	b.WriteString("  - `rename_symbol` — coordinated rename across all references\n")
	b.WriteString("  - `batch_edit` — multi-file edits in dependency order\n\n")
	b.WriteString("To bypass this redirect: unset GORTEX_HOOK_BLOCK_EDIT, or target a file outside the tracked repos.\n")
	return enrichResult{deny: true, reason: b.String()}
}

// enrichWrite mirrors enrichEdit for whole-file Write. New files
// (not yet indexed) pass through; rewrites of existing indexed
// files are redirected to `edit_file` / `write_file`.
func enrichWrite(toolInput map[string]any, port int) enrichResult {
	if !editBlockingEnabled() {
		return enrichResult{}
	}
	filePath, ok := toolInput["file_path"].(string)
	if !ok || filePath == "" {
		return enrichResult{}
	}
	if !looksLikeSourceFile(filePath) {
		return enrichResult{}
	}
	indexed, _ := queryFileIndexed(port, filePath)
	if !indexed {
		return enrichResult{}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] BLOCKED: Write of %s (indexed source — would overwrite existing tracked file). Use:\n", filePath)
	b.WriteString("  - `write_file` — whole-file write through Gortex (re-indexes after)\n")
	b.WriteString("  - `edit_file` — when you want a delta-style replace\n\n")
	b.WriteString("To bypass: unset GORTEX_HOOK_BLOCK_EDIT, or target a path outside tracked repos.\n")
	return enrichResult{deny: true, reason: b.String()}
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
