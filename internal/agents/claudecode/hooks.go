package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/agents"
)

// CurrentPreToolUseMatcher is the canonical matcher pattern we bake
// into Claude Code's PreToolUse hook. Older versions used
// "Read|Grep", "Read|Grep|Glob", "Read|Grep|Glob|Task", or
// "Read|Grep|Glob|Task|Bash"; upgradeGortexMatcher rewrites those in
// place. Edit and Write are included so the hook can redirect
// whole-file rewrites of indexed source to the Gortex MCP edit
// tools (gated by GORTEX_HOOK_BLOCK_EDIT in the hook itself).
const CurrentPreToolUseMatcher = "Read|Grep|Glob|Task|Bash|Edit|Write"

// CurrentPostToolUseMatcher names the tools whose response the
// PostToolUse hook augments. Only the read-shaped tools have an obvious
// "enrich this output with graph context" payload — Bash / Edit / Write
// don't benefit from a post-call graph snapshot, so they're omitted.
const CurrentPostToolUseMatcher = "Read|Grep|Glob"

// HookModeDeny / HookModeEnrich / HookModeConsultUnlock /
// HookModeAdaptiveNudge are the posture strings the installer accepts.
// They mirror hooks.Mode without importing it (the claudecode package
// is a leaf of the agents adapter tree and must stay import-free of
// hooks).
const (
	HookModeDeny          = "deny"
	HookModeEnrich        = "enrich"
	HookModeConsultUnlock = "consult-unlock"
	HookModeAdaptiveNudge = "nudge"
)

// normalizeHookMode maps user input to a canonical mode. Empty or
// unknown values fall through to deny so existing installs and shell
// typos preserve the original behavior. "adaptive-nudge" is accepted
// as an alias for the canonical "nudge".
func normalizeHookMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case HookModeEnrich:
		return HookModeEnrich
	case HookModeConsultUnlock:
		return HookModeConsultUnlock
	case HookModeAdaptiveNudge, "adaptive-nudge":
		return HookModeAdaptiveNudge
	default:
		return HookModeDeny
	}
}

// hookCommandWithMode appends `--mode=<mode>` to the base hook command
// when mode is non-default. The deny mode is the historical default —
// emitting it bare keeps existing settings.json diffs minimal during an
// upgrade. Every other posture is emitted explicitly so the installed
// command unambiguously declares itself.
func hookCommandWithMode(base, mode string) string {
	switch normalizeHookMode(mode) {
	case HookModeEnrich:
		return base + " --mode=enrich"
	case HookModeConsultUnlock:
		return base + " --mode=consult-unlock"
	case HookModeAdaptiveNudge:
		return base + " --mode=nudge"
	default:
		return base
	}
}

// ResolveHookCommand returns the shell command to bake into Claude
// Code's hook config. It prefers the `gortex` binary on PATH so
// installers (brew, `go install`) get a stable absolute path; falls
// back to bare "gortex hook" when no installed binary is found.
//
// A warning is written to w because the fallback relies on PATH
// resolution at hook-fire time — fragile when the user's shell
// environment differs between Claude Code and a terminal.
func ResolveHookCommand(w io.Writer) string {
	if path, err := exec.LookPath("gortex"); err == nil {
		return shellSafeHookBinary(path) + " hook"
	}
	if w != nil {
		_, _ = fmt.Fprintln(w,
			"[gortex init] warning: `gortex` not found on PATH; "+
				"writing bare \"gortex hook\" into settings — install gortex to PATH for a stable hook command")
	}
	return "gortex hook"
}

// shellSafeHookBinary normalizes a resolved binary path into a form
// safe to embed in a shell-executed hook command. Claude Code runs
// hooks through a shell; on Windows that shell is Git Bash, which
// treats backslashes as escape characters — a native path like
// C:\Users\me\gortex.exe is mangled to C:Usersmegortex.exe (\U, \m, …
// swallowed) and the hook fails with "command not found". Forward
// slashes survive: Git Bash maps C:/Users/... back to a native path
// when it spawns the .exe. The replacement is unconditional rather
// than Windows-guarded — any backslash in an unquoted shell command is
// a bug regardless of OS, and a real Unix binary path never contains
// one as a separator.
func shellSafeHookBinary(path string) string {
	return strings.ReplaceAll(path, "\\", "/")
}

// HookCommandPathIsEphemeral reports whether cmd's binary path lives
// in a location that is wiped between sessions (system tmpdirs, the
// macOS go-build cache) or no longer exists on disk. Used by
// healStaleHookCommands to detect settings.json entries that
// outlived their backing binary.
func HookCommandPathIsEphemeral(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	bin := fields[0]
	ephemeralPrefixes := []string{"/tmp/", "/var/folders/", "/private/tmp/", "/private/var/folders/"}
	for _, p := range ephemeralPrefixes {
		if strings.HasPrefix(bin, p) {
			return true
		}
	}
	// An absolute path that no longer resolves to a file is also stale.
	if filepath.IsAbs(bin) {
		if _, err := os.Stat(bin); err != nil {
			return true
		}
	}
	return false
}

// healStaleHookCommands rewrites Gortex hook entries whose command
// points at an ephemeral or missing binary path. Returns the number
// of entries rewritten. Non-Gortex entries are left alone; Gortex
// entries whose path is healthy are also left alone.
func healStaleHookCommands(hooks map[string]any, newCommand string) int {
	healed := 0
	for _, event := range []string{"PreToolUse", "PreCompact", "Stop", "SessionStart"} {
		list, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		for _, h := range list {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			inner, ok := hm["hooks"].([]any)
			if !ok {
				continue
			}
			for _, e := range inner {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				cmd, _ := em["command"].(string)
				if !commandInvokesGortexHook(cmd) {
					continue
				}
				if !HookCommandPathIsEphemeral(cmd) {
					continue
				}
				em["command"] = newCommand
				healed++
			}
		}
	}
	return healed
}

func appendHookEntry(hooks map[string]any, event string, entry map[string]any) {
	if _, ok := hooks[event]; !ok {
		hooks[event] = []any{}
	}
	list := hooks[event].([]any)
	hooks[event] = append(list, entry)
}

// upgradeGortexMatcher rewrites older PreToolUse matchers to the
// current CurrentPreToolUseMatcher. Returns true when a change was
// made. Handles every historical matcher we've shipped; anything
// not in that set is left alone.
func upgradeGortexMatcher(hooks map[string]any) bool {
	pre, ok := hooks["PreToolUse"].([]any)
	if !ok {
		return false
	}
	legacyMatchers := map[string]bool{
		"Read|Grep":                true,
		"Read|Grep|Glob":           true,
		"Read|Grep|Glob|Task":      true,
		"Read|Grep|Glob|Task|Bash": true,
	}
	upgraded := false
	for _, h := range pre {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		matcher, _ := hm["matcher"].(string)
		if !legacyMatchers[matcher] {
			continue
		}
		if !entryInvokesGortexHook(hm) {
			continue
		}
		hm["matcher"] = CurrentPreToolUseMatcher
		upgraded = true
	}
	return upgraded
}

// entryInvokesGortexHook returns true when any hooks[*].command
// looks like a Gortex hook invocation.
func entryInvokesGortexHook(entry map[string]any) bool {
	inner, ok := entry["hooks"].([]any)
	if !ok {
		return false
	}
	for _, e := range inner {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := em["command"].(string)
		if commandInvokesGortexHook(cmd) {
			return true
		}
	}
	return false
}

// dedupGortexEntries collapses duplicate Gortex hook entries inside
// hooks[event] down to the first one. Non-Gortex entries are
// preserved in order. Returns the number of duplicates removed.
func dedupGortexEntries(hooks map[string]any, event string) int {
	list, ok := hooks[event].([]any)
	if !ok {
		return 0
	}
	seenGortex := false
	kept := make([]any, 0, len(list))
	removed := 0
	for _, h := range list {
		hm, ok := h.(map[string]any)
		if !ok {
			kept = append(kept, h)
			continue
		}
		if !entryInvokesGortexHook(hm) {
			kept = append(kept, h)
			continue
		}
		if seenGortex {
			removed++
			continue
		}
		seenGortex = true
		kept = append(kept, h)
	}
	if removed > 0 {
		hooks[event] = kept
	}
	return removed
}

// commandInvokesGortexHook returns true when cmd is a Gortex hook
// invocation. Splits on whitespace and checks that "hook" is a
// standalone token and that "gortex" appears in the binary path
// component.
func commandInvokesGortexHook(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return false
	}
	if !strings.Contains(strings.ToLower(fields[0]), "gortex") {
		return false
	}
	return slices.Contains(fields[1:], "hook")
}

// rewriteGortexHookMode rewrites every Gortex hook entry's command
// across all events so it matches newCommand. Used when the install
// posture changes (deny ↔ enrich) — the existing entries already
// invoke `gortex hook` but with the wrong `--mode=...` suffix; we
// re-stamp them in place instead of removing + re-adding so user-added
// fields (timeout, statusMessage) are preserved. Returns the count of
// rewritten entries.
func rewriteGortexHookMode(hooks map[string]any, newCommand string) int {
	rewritten := 0
	for _, event := range []string{"PreToolUse", "PostToolUse", "PreCompact", "Stop", "SessionStart"} {
		list, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		for _, h := range list {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			inner, ok := hm["hooks"].([]any)
			if !ok {
				continue
			}
			for _, e := range inner {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				cmd, _ := em["command"].(string)
				if !commandInvokesGortexHook(cmd) {
					continue
				}
				if cmd == newCommand {
					continue
				}
				em["command"] = newCommand
				rewritten++
			}
		}
	}
	return rewritten
}

// removeGortexHookEntries drops every entry under hooks[event] that
// invokes `gortex hook`, preserving entries owned by other tools.
// Returns the number of entries removed. Used to clean up PostToolUse
// when the installer switches back from enrich to deny mode.
func removeGortexHookEntries(hooks map[string]any, event string) int {
	list, ok := hooks[event].([]any)
	if !ok {
		return 0
	}
	removed := 0
	kept := make([]any, 0, len(list))
	for _, h := range list {
		hm, ok := h.(map[string]any)
		if !ok {
			kept = append(kept, h)
			continue
		}
		if entryInvokesGortexHook(hm) {
			removed++
			continue
		}
		kept = append(kept, h)
	}
	if removed > 0 {
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	return removed
}

// hasGortexHookEntry returns true when the given event already has a
// hook entry that invokes `gortex hook`.
func hasGortexHookEntry(hooks map[string]any, event string) bool {
	existing, ok := hooks[event].([]any)
	if !ok {
		return false
	}
	for _, h := range existing {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if entryInvokesGortexHook(hm) {
			return true
		}
	}
	return false
}

// InstallHook is the top-level "make settings.local.json hooks
// match the current Gortex config" operation. It reads the file,
// heals stale commands, upgrades old matchers, dedupes repeat
// entries, then installs any missing Gortex hooks (PreToolUse,
// PreCompact, Stop, SessionStart, and — in enrich mode — PostToolUse).
// Writes back atomically via the shared helper.
//
// This function intentionally accepts a plain filesystem path
// rather than an Env — the same helper is used for project-level
// (.claude/settings.local.json) and user-level (~/.claude/…) files.
//
// Delegates to InstallHookWithMode with the deny posture for callers
// that don't care about hook mode (mostly tests and back-compat paths).
func InstallHook(w io.Writer, settingsPath string, opts agents.ApplyOpts) (agents.FileAction, error) {
	return InstallHookWithMode(w, settingsPath, HookModeDeny, opts)
}

// InstallHookWithMode is the mode-aware variant. mode is one of
// HookModeDeny (default — install PreToolUse with deny behavior, no
// PostToolUse) or HookModeEnrich (install PreToolUse in soft-context
// mode plus a PostToolUse entry that augments tool output with graph
// context). Switching modes between installs rewrites the existing
// Gortex hook command in place and adds or removes the PostToolUse
// entry to match.
func InstallHookWithMode(w io.Writer, settingsPath string, mode string, opts agents.ApplyOpts) (agents.FileAction, error) {
	mode = normalizeHookMode(mode)
	var settings map[string]any
	existed := false
	if data, err := os.ReadFile(settingsPath); err == nil {
		existed = true
		if err := json.Unmarshal(data, &settings); err != nil {
			settings = make(map[string]any)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return agents.FileAction{}, fmt.Errorf("read %s: %w", settingsPath, err)
	} else {
		settings = make(map[string]any)
	}

	baseCommand := ResolveHookCommand(w)
	hookCommand := hookCommandWithMode(baseCommand, mode)

	if _, ok := settings["hooks"]; !ok {
		settings["hooks"] = make(map[string]any)
	}
	hooks := settings["hooks"].(map[string]any)

	healedCount := healStaleHookCommands(hooks, hookCommand)
	matcherUpgraded := upgradeGortexMatcher(hooks)
	modeRewriteCount := rewriteGortexHookMode(hooks, hookCommand)
	dedupedCount := dedupGortexEntries(hooks, "PreToolUse") +
		dedupGortexEntries(hooks, "PreCompact") +
		dedupGortexEntries(hooks, "PostToolUse") +
		dedupGortexEntries(hooks, "Stop") +
		dedupGortexEntries(hooks, "SessionStart")

	// PostToolUse is only present when mode=enrich. Removing it on a
	// switch to any other posture keeps settings.json clean — agents
	// won't fire a no-op hook on every Read/Grep response.
	postToolUseRemoved := 0
	if mode != HookModeEnrich {
		postToolUseRemoved = removeGortexHookEntries(hooks, "PostToolUse")
	}

	preToolUseInstalled := hasGortexHookEntry(hooks, "PreToolUse")
	preCompactInstalled := hasGortexHookEntry(hooks, "PreCompact")
	stopInstalled := hasGortexHookEntry(hooks, "Stop")
	sessionStartInstalled := hasGortexHookEntry(hooks, "SessionStart")
	postToolUseInstalled := hasGortexHookEntry(hooks, "PostToolUse")

	if !preToolUseInstalled {
		appendHookEntry(hooks, "PreToolUse", map[string]any{
			"matcher": CurrentPreToolUseMatcher,
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       3000,
					"statusMessage": "Enriching with Gortex graph context...",
				},
			},
		})
	}
	if !preCompactInstalled {
		appendHookEntry(hooks, "PreCompact", map[string]any{
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       3000,
					"statusMessage": "Injecting Gortex orientation snapshot...",
				},
			},
		})
	}
	if !stopInstalled {
		appendHookEntry(hooks, "Stop", map[string]any{
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       5000,
					"statusMessage": "Running Gortex post-task diagnostics...",
				},
			},
		})
	}
	if !sessionStartInstalled {
		// SessionStart fires at the start of a new or resumed session
		// — a perfect moment to inject the Gortex orientation snapshot
		// so the first turn doesn't have to call graph_stats. It
		// complements PreCompact (which fires on summary boundaries).
		appendHookEntry(hooks, "SessionStart", map[string]any{
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       3000,
					"statusMessage": "Loading Gortex graph orientation...",
				},
			},
		})
	}
	// PostToolUse is only installed in enrich mode. It augments
	// Grep / Glob / Read responses with graph context (enclosing
	// symbols, file footprints) so the agent sees the graph value
	// adjacent to the raw output instead of via a deny redirect.
	postToolUseAdded := false
	if mode == HookModeEnrich && !postToolUseInstalled {
		appendHookEntry(hooks, "PostToolUse", map[string]any{
			"matcher": CurrentPostToolUseMatcher,
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       hookCommand,
					"timeout":       3000,
					"statusMessage": "Layering Gortex graph context onto tool output...",
				},
			},
		})
		postToolUseAdded = true
	}

	allPresent := preToolUseInstalled && preCompactInstalled && stopInstalled && sessionStartInstalled &&
		(mode != HookModeEnrich || postToolUseInstalled)
	noChanges := allPresent && !matcherUpgraded && dedupedCount == 0 && healedCount == 0 &&
		modeRewriteCount == 0 && postToolUseRemoved == 0 && !postToolUseAdded
	if noChanges {
		if w != nil {
			_, _ = fmt.Fprintf(w, "[gortex init] all hooks already present in %s\n", settingsPath)
		}
		return agents.FileAction{Path: settingsPath, Action: agents.ActionSkip, Reason: "already-configured"}, nil
	}

	if opts.DryRun {
		action := agents.ActionWouldCreate
		if existed {
			action = agents.ActionWouldMerge
		}
		return agents.FileAction{Path: settingsPath, Action: action, Keys: []string{"hooks"}}, nil
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return agents.FileAction{}, err
	}
	if err := agents.AtomicWriteFile(settingsPath, data, 0o644); err != nil {
		return agents.FileAction{}, err
	}

	// Report exactly what changed — helpful for the doctor subcommand
	// and for reassuring users during `gortex init` re-runs.
	var changes []string
	if matcherUpgraded {
		changes = append(changes, "upgraded PreToolUse matcher")
	}
	if dedupedCount > 0 {
		changes = append(changes, fmt.Sprintf("removed %d duplicate entries", dedupedCount))
	}
	if healedCount > 0 {
		changes = append(changes, fmt.Sprintf("rewrote %d stale hook path(s)", healedCount))
	}
	if !preToolUseInstalled {
		changes = append(changes, "installed PreToolUse")
	}
	if !preCompactInstalled {
		changes = append(changes, "installed PreCompact")
	}
	if !stopInstalled {
		changes = append(changes, "installed Stop")
	}
	if !sessionStartInstalled {
		changes = append(changes, "installed SessionStart")
	}
	if postToolUseAdded {
		changes = append(changes, "installed PostToolUse (enrich mode)")
	}
	if postToolUseRemoved > 0 {
		changes = append(changes, fmt.Sprintf("removed PostToolUse (switched to %s mode)", mode))
	}
	if modeRewriteCount > 0 {
		changes = append(changes, fmt.Sprintf("rewrote %d hook command(s) for mode=%s", modeRewriteCount, mode))
	}
	if w != nil {
		_, _ = fmt.Fprintf(w, "[gortex init] %s in %s\n", strings.Join(changes, ", "), settingsPath)
	}
	action := agents.ActionCreate
	if existed {
		action = agents.ActionMerge
	}
	return agents.FileAction{Path: settingsPath, Action: action, Keys: []string{"hooks"}}, nil
}
