package hooks

import (
	"encoding/json"
	"io"
	"os"
	"strings"
)

// Mode selects the posture of the Claude Code PreToolUse / PostToolUse
// integration. Two modes are supported:
//
//   - ModeDeny (default, "deny"): the PreToolUse hook actively denies
//     whole-file Grep / Glob / Read of indexed source and points the
//     agent at the equivalent Gortex graph tools. The agent learns by
//     friction. No PostToolUse hook is registered.
//
//   - ModeEnrich ("enrich"): the PreToolUse hook never denies — it only
//     emits soft `additionalContext` guidance — and a PostToolUse hook
//     augments the tool's actual output with graph context (enclosing
//     symbols, file metadata, callers). Easier onboarding for teams that
//     don't want their agent stopped mid-flow; the graph value still
//     lands, just adjacent to the raw output rather than as a redirect.
//
// The two modes co-exist by selection — a user picks one per install via
// `gortex install --hook-mode=<mode>`. Switching modes is a single
// re-install; the Claude Code adapter rewrites the hook command and
// adds / removes the PostToolUse entry to match.
type Mode int

const (
	// ModeDeny preserves the legacy "redirect by deny" behavior.
	ModeDeny Mode = iota
	// ModeEnrich augments tool output rather than denying it.
	ModeEnrich
)

// ParseMode resolves the string form ("deny" / "enrich") into a Mode.
// Empty / unknown values fall through to ModeDeny so unset environments
// keep the established behavior.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "enrich":
		return ModeEnrich
	default:
		return ModeDeny
	}
}

// String renders the mode in its canonical lower-case form. Useful for
// logging and for round-tripping the value through the `--mode` flag.
func (m Mode) String() string {
	switch m {
	case ModeEnrich:
		return "enrich"
	default:
		return "deny"
	}
}

// Run reads a single hook payload from stdin, peeks at hook_event_name,
// and dispatches to the right handler. This is the single entry point
// `.claude/settings.local.json` should register via `gortex hook`.
//
// Any error in reading or parsing results in a silent no-op — hooks
// must never block Claude Code's normal flow.
func Run(port int, mode Mode) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}

	var peek struct {
		HookEventName string `json:"hook_event_name"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return
	}

	switch peek.HookEventName {
	case "PreToolUse":
		runPreToolUse(data, port, mode)
	case "PostToolUse":
		runPostToolUse(data, port)
	case "PreCompact":
		runPreCompact(data, port)
	case "Stop":
		runPostTask(data, port)
	case "SessionStart":
		runSessionStart(data)
	}
}
