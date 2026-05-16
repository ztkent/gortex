package claudecode

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/agents"
)

// agentsApplyOptsZero is the zero value of agents.ApplyOpts. Helper so
// the mode-switching tests don't drag the agents import into every
// call site.
func agentsApplyOptsZero() agents.ApplyOpts { return agents.ApplyOpts{} }

// readSettingsHooks reads settings.local.json from path and returns
// the hooks map (or fails the test if the file is missing / malformed).
// The mode-switching tests assert on hook structure between InstallHook
// calls, so we centralise the read-and-shape-check here.
func readSettingsHooks(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read %s", path)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	hooks, ok := settings["hooks"].(map[string]any)
	require.True(t, ok, "settings.hooks missing or wrong type")
	return hooks
}

// TestHookCommandPathIsEphemeral covers every branch of the
// ephemeral-path detector because this function's output directly
// decides whether a user's settings.local.json gets rewritten on
// re-run. A false positive here would thrash the user's hook
// config; a false negative leaves them with stale /tmp paths.
func TestHookCommandPathIsEphemeral(t *testing.T) {
	// /bin/sh is a stable POSIX path, not under any ephemeral root.
	// os.Executable() would land in /private/var/folders under go
	// test, which is itself ephemeral — hence hard-coding.
	const existing = "/bin/sh"
	if _, err := os.Stat(existing); err != nil {
		t.Skipf("test fixture %s not present: %v", existing, err)
	}

	missing := filepath.Join("/nonexistent-root-for-gortex-test", "ghost-binary")

	cases := []struct {
		name    string
		cmd     string
		want    bool
		comment string
	}{
		{"empty", "", false, "no fields to inspect"},
		{"bareName", "gortex hook", false, "PATH lookup happens at fire time"},
		{"relative", "./gortex hook", false, "relative paths are user choice, not ephemeral"},
		{"tmp", "/tmp/gortex-hook-fix hook", true, "/tmp is wiped between sessions"},
		{"varFolders", "/var/folders/x/y/z/gortex hook", true, "macOS go-build cache"},
		{"privateTmp", "/private/tmp/gortex hook", true, "macOS resolves /tmp via /private/tmp"},
		{"privateVarFolders", "/private/var/folders/x/y/z/gortex hook", true, "fully resolved go-build cache"},
		{"missingAbsolute", missing + " hook", true, "absolute path that no longer exists"},
		{"healthyAbsolute", existing + " hook", false, "absolute path that exists on disk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HookCommandPathIsEphemeral(tc.cmd)
			assert.Equal(t, tc.want, got, tc.comment)
		})
	}
}

func TestHealStaleHookCommands(t *testing.T) {
	const newCmd = "/opt/homebrew/bin/gortex hook"

	t.Run("emptyHooks", func(t *testing.T) {
		hooks := map[string]any{}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 0, got)
	})

	t.Run("noGortexEntries", func(t *testing.T) {
		hooks := map[string]any{
			"PreToolUse": []any{
				makeHookEntry("Read", "/usr/local/bin/some-other-tool run"),
			},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 0, got)
		entries := hooks["PreToolUse"].([]any)
		inner := entries[0].(map[string]any)["hooks"].([]any)
		cmd := inner[0].(map[string]any)["command"].(string)
		assert.Equal(t, "/usr/local/bin/some-other-tool run", cmd)
	})

	t.Run("healthyGortexEntryUntouched", func(t *testing.T) {
		hooks := map[string]any{
			"PreToolUse": []any{makeHookEntry("Read", "./gortex hook")},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 0, got)
		assert.Equal(t, "./gortex hook", extractCmd(t, hooks, "PreToolUse", 0))
	})

	t.Run("staleEntryRewritten", func(t *testing.T) {
		hooks := map[string]any{
			"Stop": []any{makeHookEntry("", "/tmp/gortex-hook-fix hook")},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 1, got)
		assert.Equal(t, newCmd, extractCmd(t, hooks, "Stop", 0))
	})

	t.Run("multipleEventsAndMixed", func(t *testing.T) {
		hooks := map[string]any{
			"PreToolUse": []any{
				makeHookEntry("Read", "./gortex hook"),
				makeHookEntry("Read", "/usr/local/bin/lint --strict"),
			},
			"PreCompact": []any{makeHookEntry("", "/tmp/gortex-hook-fix hook")},
			"Stop":       []any{makeHookEntry("", "/tmp/gortex-hook-fix hook")},
		}
		got := healStaleHookCommands(hooks, newCmd)
		assert.Equal(t, 2, got)
		assert.Equal(t, "./gortex hook", extractCmd(t, hooks, "PreToolUse", 0))
		assert.Equal(t, "/usr/local/bin/lint --strict", extractCmd(t, hooks, "PreToolUse", 1))
		assert.Equal(t, newCmd, extractCmd(t, hooks, "PreCompact", 0))
		assert.Equal(t, newCmd, extractCmd(t, hooks, "Stop", 0))
	})
}

func TestResolveHookCommand(t *testing.T) {
	t.Run("foundOnPath", func(t *testing.T) {
		dir := t.TempDir()
		fake := filepath.Join(dir, "gortex")
		require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755))
		t.Setenv("PATH", dir)

		got := ResolveHookCommand(io.Discard)
		assert.Equal(t, fake+" hook", got, "should resolve to absolute path on PATH")
	})

	t.Run("notFoundFallsBackToBare", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		got := ResolveHookCommand(io.Discard)
		assert.Equal(t, "gortex hook", got, "fallback to bare name keeps init working in sandboxes")
	})
}

func makeHookEntry(matcher, command string) map[string]any {
	entry := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
				"timeout": 3000,
			},
		},
	}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	return entry
}

func extractCmd(t *testing.T, hooks map[string]any, event string, idx int) string {
	t.Helper()
	list, ok := hooks[event].([]any)
	require.True(t, ok, "event %q missing", event)
	require.Greater(t, len(list), idx, "event %q has fewer than %d entries", event, idx+1)
	entry, ok := list[idx].(map[string]any)
	require.True(t, ok)
	inner, ok := entry["hooks"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, inner)
	em, ok := inner[0].(map[string]any)
	require.True(t, ok)
	cmd, _ := em["command"].(string)
	return cmd
}

// ---------------------------------------------------------------------------
// Mode parsing + command rendering
// ---------------------------------------------------------------------------

func TestNormalizeHookMode(t *testing.T) {
	cases := map[string]string{
		"":         HookModeDeny,
		"deny":     HookModeDeny,
		"DENY":     HookModeDeny,
		"  deny  ": HookModeDeny,
		"enrich":   HookModeEnrich,
		"Enrich":   HookModeEnrich,
		"unknown":  HookModeDeny, // safe fallback
		"off":      HookModeDeny, // install flag handles disable separately
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			assert.Equal(t, want, normalizeHookMode(input))
		})
	}
}

func TestHookCommandWithMode(t *testing.T) {
	base := "/usr/local/bin/gortex hook"
	assert.Equal(t, base, hookCommandWithMode(base, HookModeDeny),
		"deny mode must NOT append --mode flag (back-compat with pre-N3 settings)")
	assert.Equal(t, base+" --mode=enrich", hookCommandWithMode(base, HookModeEnrich),
		"enrich mode must append --mode=enrich")
	assert.Equal(t, base, hookCommandWithMode(base, ""),
		"empty mode defaults to deny — no flag")
}

// ---------------------------------------------------------------------------
// removeGortexHookEntries — used when switching enrich → deny
// ---------------------------------------------------------------------------

func TestRemoveGortexHookEntries(t *testing.T) {
	hooks := map[string]any{
		"PostToolUse": []any{
			makeHookEntry("Read|Grep|Glob", "/opt/gortex hook --mode=enrich"),
			map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": "/usr/bin/other-tool"},
				},
			},
		},
	}
	removed := removeGortexHookEntries(hooks, "PostToolUse")
	assert.Equal(t, 1, removed)
	list := hooks["PostToolUse"].([]any)
	require.Len(t, list, 1, "non-Gortex entry must be preserved")
	em := list[0].(map[string]any)
	inner := em["hooks"].([]any)
	cmd := inner[0].(map[string]any)["command"]
	assert.Equal(t, "/usr/bin/other-tool", cmd)
}

func TestRemoveGortexHookEntries_DeletesEmptyEvent(t *testing.T) {
	hooks := map[string]any{
		"PostToolUse": []any{
			makeHookEntry("Read|Grep|Glob", "/opt/gortex hook --mode=enrich"),
		},
	}
	removed := removeGortexHookEntries(hooks, "PostToolUse")
	assert.Equal(t, 1, removed)
	_, exists := hooks["PostToolUse"]
	assert.False(t, exists, "empty event should be deleted, not left as []")
}

func TestRemoveGortexHookEntries_NoOpOnMissingEvent(t *testing.T) {
	hooks := map[string]any{}
	assert.Equal(t, 0, removeGortexHookEntries(hooks, "PostToolUse"))
}

// ---------------------------------------------------------------------------
// rewriteGortexHookMode — switches mode without touching user fields
// ---------------------------------------------------------------------------

func TestRewriteGortexHookMode_UpdatesCommandPreservingMeta(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{
			map[string]any{
				"matcher": "Read|Grep",
				"hooks": []any{
					map[string]any{
						"type":          "command",
						"command":       "/opt/gortex hook",
						"timeout":       3000,
						"statusMessage": "custom user-set message",
					},
				},
			},
		},
	}
	rewritten := rewriteGortexHookMode(hooks, "/opt/gortex hook --mode=enrich")
	assert.Equal(t, 1, rewritten)
	list := hooks["PreToolUse"].([]any)
	em := list[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	assert.Equal(t, "/opt/gortex hook --mode=enrich", em["command"])
	assert.Equal(t, "custom user-set message", em["statusMessage"],
		"rewrite must preserve user-added fields like statusMessage")
}

func TestRewriteGortexHookMode_NoOpOnIdenticalCommand(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{makeHookEntry("Read|Grep|Glob", "/opt/gortex hook")},
	}
	assert.Equal(t, 0, rewriteGortexHookMode(hooks, "/opt/gortex hook"))
}

func TestRewriteGortexHookMode_IgnoresNonGortexEntries(t *testing.T) {
	hooks := map[string]any{
		"PreToolUse": []any{makeHookEntry("Bash", "/usr/bin/other --thing")},
	}
	assert.Equal(t, 0, rewriteGortexHookMode(hooks, "/opt/gortex hook --mode=enrich"))
	cmd := extractCmd(t, hooks, "PreToolUse", 0)
	assert.Equal(t, "/usr/bin/other --thing", cmd,
		"non-Gortex entries must NOT be touched even when we're rewriting")
}

// ---------------------------------------------------------------------------
// InstallHookWithMode — end-to-end: deny → enrich → deny round-trip
// ---------------------------------------------------------------------------

func TestInstallHookWithMode_DenyMode(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir()) // force fallback to bare "gortex hook"

	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)

	hooks := readSettingsHooks(t, settingsPath)
	assert.NotContains(t, hooks, "PostToolUse",
		"deny mode must NOT install a PostToolUse entry")
	cmd := extractCmd(t, hooks, "PreToolUse", 0)
	assert.Equal(t, "gortex hook", cmd,
		"deny mode keeps the bare command — no --mode flag")
}

func TestInstallHookWithMode_EnrichMode(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir())

	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeEnrich, agentsApplyOptsZero())
	require.NoError(t, err)

	hooks := readSettingsHooks(t, settingsPath)
	require.Contains(t, hooks, "PostToolUse")
	postCmd := extractCmd(t, hooks, "PostToolUse", 0)
	assert.Equal(t, "gortex hook --mode=enrich", postCmd)

	preCmd := extractCmd(t, hooks, "PreToolUse", 0)
	assert.Equal(t, "gortex hook --mode=enrich", preCmd,
		"PreToolUse must use the same --mode=enrich command so it falls back to soft-context")

	// PostToolUse uses the matcher restricted to read-shaped tools.
	post := hooks["PostToolUse"].([]any)[0].(map[string]any)
	assert.Equal(t, CurrentPostToolUseMatcher, post["matcher"])
}

func TestInstallHookWithMode_DenyToEnrichRoundTrip(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir())

	// Initial deny install.
	_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)
	hooks := readSettingsHooks(t, settingsPath)
	require.NotContains(t, hooks, "PostToolUse")
	assert.Equal(t, "gortex hook", extractCmd(t, hooks, "PreToolUse", 0))

	// Switch to enrich.
	_, err = InstallHookWithMode(io.Discard, settingsPath, HookModeEnrich, agentsApplyOptsZero())
	require.NoError(t, err)
	hooks = readSettingsHooks(t, settingsPath)
	require.Contains(t, hooks, "PostToolUse", "enrich install must add PostToolUse")
	assert.Equal(t, "gortex hook --mode=enrich", extractCmd(t, hooks, "PreToolUse", 0),
		"PreToolUse command must be rewritten on mode switch")
	assert.Equal(t, "gortex hook --mode=enrich", extractCmd(t, hooks, "PostToolUse", 0))

	// Switch back to deny — PostToolUse must be removed, PreToolUse rewritten.
	_, err = InstallHookWithMode(io.Discard, settingsPath, HookModeDeny, agentsApplyOptsZero())
	require.NoError(t, err)
	hooks = readSettingsHooks(t, settingsPath)
	assert.NotContains(t, hooks, "PostToolUse", "switch back to deny must drop PostToolUse")
	assert.Equal(t, "gortex hook", extractCmd(t, hooks, "PreToolUse", 0),
		"switch back to deny must restore bare command on PreToolUse")
}

func TestInstallHookWithMode_EnrichIdempotent(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir())

	for i := range 3 {
		_, err := InstallHookWithMode(io.Discard, settingsPath, HookModeEnrich, agentsApplyOptsZero())
		require.NoError(t, err, "iteration %d", i)
	}
	hooks := readSettingsHooks(t, settingsPath)
	// PostToolUse should have exactly one entry — re-running install
	// must not append duplicates.
	post := hooks["PostToolUse"].([]any)
	assert.Len(t, post, 1, "repeated install must not duplicate the PostToolUse entry")
}

// InstallHook (the back-compat wrapper) must default to deny mode.
func TestInstallHook_DefaultsToDeny(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.local.json")
	t.Setenv("PATH", t.TempDir())

	_, err := InstallHook(io.Discard, settingsPath, agentsApplyOptsZero())
	require.NoError(t, err)
	hooks := readSettingsHooks(t, settingsPath)
	assert.NotContains(t, hooks, "PostToolUse",
		"InstallHook (no mode) must behave as deny — no PostToolUse entry")
}
