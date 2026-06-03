package hooks

import (
	"fmt"
	"strings"
)

// The Gortex MCP read tools that return whole-file source. A call to
// either with default arguments hands the agent every function body —
// the exact pattern that burns context in issue #40. They are the one
// gap the native-tool (Read / Grep / Bash) redirects can't cover: once
// the agent is already inside a Gortex tool, nothing else guards how it
// is called.
const (
	gortexReadFileTool       = gortexMCPToolPrefix + "read_file"
	gortexEditingContextTool = gortexMCPToolPrefix + "get_editing_context"
)

// gortexForceCompressEnvVar upgrades the compress-bodies advisory from a
// soft nudge to a hard deny. It mirrors GORTEX_HOOK_BLOCK_EDIT: the
// default posture is a non-blocking reminder — a full-body read is
// sometimes genuinely needed — and a team can flip this on to enforce
// the rule once they trust it.
const gortexForceCompressEnvVar = "GORTEX_HOOK_FORCE_COMPRESS"

// enrichGortexRead nudges (or, when GORTEX_HOOK_FORCE_COMPRESS is set,
// denies) a whole-file read through read_file / get_editing_context
// that omits compress_bodies on a source file. Returns an empty result
// — i.e. silent pass-through — for any call that is already economical
// (compressed, size-capped, or a non-code file where compression is a
// no-op).
func enrichGortexRead(toolName string, toolInput map[string]any) enrichResult {
	msg := gortexReadNudge(toolName, toolInput)
	if msg == "" {
		return enrichResult{}
	}
	if gortexForceCompressEnabled() {
		return enrichResult{deny: true, reason: msg}
	}
	return enrichResult{context: msg}
}

// gortexReadNudge returns the advisory message for a Gortex read-tool
// call that should be nudged, or "" when the call needs no nudge. It is
// pure (no env gate, no daemon round-trip — the decision is made
// entirely from the tool input, so the hook stays sub-millisecond) so
// callers can surface the message either as soft context or as a deny.
func gortexReadNudge(toolName string, toolInput map[string]any) string {
	// Already compressing — nothing to suggest.
	if asBool(toolInput["compress_bodies"]) {
		return ""
	}
	path, _ := toolInput["path"].(string)
	if path == "" {
		return ""
	}
	// compress_bodies only elides code bodies; on prose / config it is a
	// no-op, so don't nag on non-source reads.
	if !looksLikeSourceFile(path) {
		return ""
	}
	// The agent already bounded the read (a slice or a token / byte cap)
	// — it knows what it wants; don't second-guess a constrained call.
	if hasReadSizeCap(toolInput) {
		return ""
	}
	return gortexReadAdvisory(toolName, path)
}

// gortexReadAdvisory builds the reminder shown when a Gortex read tool
// is about to pull full bodies. It names the cheaper paths the reporter
// of issue #40 wished the agent had taken: search_text to locate sites
// without reading bodies, and compress_bodies (+ keep) to read for an
// edit at a fraction of the tokens.
func gortexReadAdvisory(toolName, path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] %s on %s without compress_bodies — a full-body read can dominate context.\n",
		shortGortexToolName(toolName), path)
	b.WriteString("  - Locating specific call sites? `search_text` returns line-precise hits and reads no bodies.\n")
	b.WriteString("  - Reading to edit? Re-call with compress_bodies:true (~30-40% of the tokens; signatures, types, and comments kept).\n")
	b.WriteString("  - Need certain bodies in full? Add keep:\"Name1,Name2\" alongside compress_bodies:true.\n")
	return b.String()
}

// shortGortexToolName strips the mcp__gortex__ namespace so the advisory
// reads "read_file" rather than the fully-qualified tool name.
func shortGortexToolName(toolName string) string {
	return strings.TrimPrefix(toolName, gortexMCPToolPrefix)
}

// hasReadSizeCap reports whether the read already bounds its output via a
// line / byte / token cap. read_file uses max_lines / max_bytes;
// get_editing_context uses max_bytes / max_tokens. A zero or non-numeric
// value is treated as "no cap" so an explicit `max_tokens: 0` opt-out
// still draws the nudge.
func hasReadSizeCap(toolInput map[string]any) bool {
	for _, k := range []string{"max_lines", "max_bytes", "max_tokens"} {
		if n, ok := toFloat64(toolInput[k]); ok && n > 0 {
			return true
		}
	}
	return false
}

// asBool coerces a JSON-decoded tool-input value to bool. Claude Code
// sends booleans as JSON true/false (decoded to Go bool); the string
// fallback covers hosts that stringify tool inputs.
func asBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "1", "yes", "on":
			return true
		}
	}
	return false
}

// gortexForceCompressEnabled reports whether the hard-deny gate is on.
// Same truthiness rules as editBlockingEnabled (see envGateEnabled).
func gortexForceCompressEnabled() bool {
	return envGateEnabled(gortexForceCompressEnvVar)
}
