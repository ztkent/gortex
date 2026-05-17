package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// PluginBundleSpec describes one generation of the marketplace
// plugin bundle. The same emitter drives the Anthropic
// Plugin Marketplace artifact today; the Cursor variant in a follow-up
// will use the same struct with a different LayoutVariant.
type PluginBundleSpec struct {
	// TargetDir is the directory to emit into. The directory is created
	// if it does not exist; existing files within it are overwritten.
	TargetDir string

	// Version is the gortex SemVer string to bake into plugin.json
	// (e.g. "0.18.2"). Marketplace entries pin a release ref; this
	// string identifies what's inside.
	Version string

	// LayoutVariant chooses the output shape. Today only
	// LayoutVariantAnthropic is implemented; the Cursor variant will
	// arrive when its marketplace schema settles.
	LayoutVariant LayoutVariant
}

// LayoutVariant enumerates the marketplace layouts the emitter knows
// how to write.
type LayoutVariant string

const (
	// LayoutVariantAnthropic targets the Anthropic Plugin Marketplace
	// schema (https://anthropic.com/claude-code/marketplace.schema.json
	// and the per-plugin layout discovered via the example-plugin
	// reference shipped in claude-plugins-official).
	LayoutVariantAnthropic LayoutVariant = "anthropic"
)

// pluginAuthorName / pluginAuthorEmail / pluginHomepage are the
// fields baked into every plugin.json. They are package-level so the
// marketplace.json submission script can read the same values
// without re-deriving them.
const (
	pluginName        = "gortex"
	pluginDescription = "Your AI's live map of your codebase — every call, dependency, and contract indexed across your repos, shared across every running agent via a local daemon. Gives Claude Code, Cursor, and any MCP-aware client 52 graph-aware tools so they answer \"what calls this?\", \"what breaks if I rename UserStore?\", or \"which services consume this endpoint?\" in one call instead of grepping for minutes. Real AST parsing across 92 languages — deterministic, zero LLM calls during indexing. Useful before refactoring, when tracing bugs across services, exploring unfamiliar code, or any time your agent reaches for Grep or Read. Returns precise answers tagged with confidence tiers (compiler-grade vs heuristic). Local-first, source-available, free under defined thresholds."
	pluginAuthorName  = "Andrey Kumanyaev"
	pluginAuthorEmail = "support@gortex.dev"
	pluginHomepage    = "https://gortex.dev"
)

// pluginMCPJSON is the .mcp.json content for the marketplace plugin.
// Stdio form with `gortex mcp --proxy`: the marketplace user installs
// the plugin, the binary is on PATH (via the get.gortex.dev installer
// or Homebrew), and `--proxy` auto-detects the daemon and falls back
// to spawning a local MCPServer when no daemon is reachable.
//
// Indentation matches Claude Code's reference plugins (2 spaces, no
// trailing newline beyond the final brace's).
const pluginMCPJSON = `{
  "gortex": {
    "command": "gortex",
    "args": ["mcp", "--proxy"]
  }
}
`

// pluginHooksJSON is the hooks/hooks.json content for the
// marketplace plugin. Four event bindings (PreToolUse, PreCompact,
// Stop, SessionStart) all dispatch through ${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh
// — a thin wrapper that locates the gortex binary and invokes
// `gortex hook`. The wrapper's job is:
//
//   - Fail soft when gortex is missing (print to stderr, exit 0) so
//     a marketplace user without the binary doesn't see hard errors
//     on every Claude Code action.
//   - Forward stdin / stderr / argv unchanged so all today's
//     `gortex hook` event-dispatcher logic in internal/hooks/dispatch.go
//     keeps working byte-for-byte.
const pluginHooksJSON = `{
  "description": "Gortex graph-aware hooks: PreToolUse routing, PreCompact orientation, post-task diagnostics, SessionStart cold briefing.",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Read|Grep|Glob|Task|Bash|Edit|Write",
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 3000,
            "statusMessage": "Enriching with Gortex graph context..."
          }
        ]
      }
    ],
    "PreCompact": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 3000,
            "statusMessage": "Injecting Gortex orientation snapshot..."
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 5000,
            "statusMessage": "Running Gortex post-task diagnostics..."
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash \"${CLAUDE_PLUGIN_ROOT}/hooks-handlers/gortex-hook.sh\"",
            "timeout": 3000,
            "statusMessage": "Loading Gortex graph orientation..."
          }
        ]
      }
    ]
  }
}
`

// pluginHookHandler is the bash wrapper script that every hook event
// invokes. It locates the gortex binary on PATH, runs `gortex hook`
// with stdin/stderr/argv forwarded, and degrades gracefully when the
// binary is missing — print one warning to stderr and exit 0 so a
// missing binary never blocks the user's Claude Code session.
const pluginHookHandler = `#!/usr/bin/env bash
# gortex marketplace-plugin hook wrapper.
# Locates the gortex binary on PATH and forwards the Claude Code hook
# event to ` + "`gortex hook`" + `. Falls back to a soft-fail (warn-and-exit-0)
# when gortex is not installed, so a missing binary never blocks the
# user's session — the marketplace plugin can always be installed in
# advance of the binary.
#
# Install gortex via:  curl -fsSL https://get.gortex.dev | sh
set -u

if ! command -v gortex >/dev/null 2>&1; then
  echo "gortex binary not found on PATH — install via 'curl -fsSL https://get.gortex.dev | sh' to enable graph-aware hooks." >&2
  exit 0
fi

exec gortex hook "$@"
`

const pluginReadmeBody = `# Gortex — Claude Code Plugin

Gortex is a graph-based code intelligence engine. This plugin gives Claude
Code 52 MCP tools for navigation, refactoring, contracts, impact analysis,
and search across 92 languages — backed by a shared-graph daemon that
keeps multiple agents and editor sessions in sync.

## Install

This plugin assumes the ` + "`gortex`" + ` binary is on PATH. Install it via:

` + "```sh\ncurl -fsSL https://get.gortex.dev | sh\n```" + `

The installer fetches a signed release tarball, verifies cosign + SHA256,
installs to ` + "`~/.local/bin`" + ` (or ` + "`/usr/local/bin`" + `), and runs ` + "`gortex install`" + `
+ ` + "`gortex init`" + `. Homebrew users on macOS can also use:

` + "```sh\nbrew install zzet/tap/gortex\n```" + `

## What this plugin adds

| Surface | What you get |
|---------|--------------|
| **MCP server** | 52 tools (` + "`search_symbols`" + `, ` + "`find_usages`" + `, ` + "`get_call_chain`" + `, ` + "`explain_change_impact`" + `, ` + "`rename_symbol`" + `, ` + "`scaffold`" + `, ` + "`contracts`" + `, …) over stdio via ` + "`gortex mcp --proxy`" + ` |
| **Slash commands** | Discovery: ` + "`/gortex-guide`" + `, ` + "`/gortex-explore`" + `, ` + "`/gortex-debug`" + `, ` + "`/gortex-impact`" + `, ` + "`/gortex-dataflow-trace`" + `, ` + "`/gortex-cross-repo-usage`" + `, ` + "`/gortex-co-change`" + `, ` + "`/gortex-onboarding`" + ` · Edit & refactor: ` + "`/gortex-refactor`" + `, ` + "`/gortex-safe-edit`" + `, ` + "`/gortex-rename`" + `, ` + "`/gortex-extract-function`" + `, ` + "`/gortex-fix-all`" + `, ` + "`/gortex-add-test`" + ` · Review & operate: ` + "`/gortex-pr-review`" + `, ` + "`/gortex-architecture-review`" + `, ` + "`/gortex-quality-audit`" + `, ` + "`/gortex-incident-investigation`" + `, ` + "`/gortex-episode-replay`" + ` |
| **Skills** | Nineteen model-invoked skills that activate by task-shape. Discovery (7): architecture exploration, debugging, blast-radius analysis, dataflow tracing, cross-repo usage, co-change analysis, onboarding tour. Edit & refactor (6): general refactor, safe-edit (` + "`preview_edit`" + ` / ` + "`simulate_chain`" + ` before disk), cross-file rename, LSP-driven extract function, LSP ` + "`source.fixAll`" + `, coverage-led add-test. Review & operate (5): PR review, architecture review, quality audit, incident investigation, episode replay. Plus the tool-reference guide. The edit skills enforce the speculative-execution + LSP code-actions order so agents do not bypass safety steps; the review & operate skills wrap the discovery + impact + memory surfaces into ordered playbooks so postmortems and reviews are graph-grounded. |
| **Hooks** | PreToolUse routing (Read → ` + "`get_symbol_source`" + `, Grep → ` + "`search_symbols`" + ` / ` + "`find_usages`" + `), PreCompact orientation snapshot, Stop post-task diagnostics, SessionStart cold briefing |

## First run

After install, point Claude Code at any code repository and ask a task that
involves understanding code structure ("how does authentication work?",
"what breaks if I rename ` + "`UserStore`" + `?"). The hooks redirect Read/Grep/Glob
toward graph queries; the slash commands give you guided workflows.

If ` + "`gortex daemon`" + ` is running (` + "`gortex daemon start --detach`" + ` to start it),
all your editor sessions share one in-memory graph. Otherwise this plugin
spawns a one-shot MCP server per session — same tools, slower cold start.

## Links

- Homepage: https://gortex.dev
- Source:   https://github.com/zzet/gortex
- License:  https://github.com/zzet/gortex/blob/main/LICENSE.md (source-available; free under defined thresholds)
`

// pluginLicenseBody is the LICENSE shipped inside the plugin
// directory. Plugins under claude-plugins-official tend to ship the
// project's own license rather than a marketplace-imposed one — we
// follow that pattern by emitting a one-line pointer to the canonical
// LICENSE.md in the upstream repo. Keeps the plugin directory small
// and avoids drift from the source-of-truth license.
const pluginLicenseBody = `Gortex is licensed under the terms in LICENSE.md at the root of the
upstream repository: https://github.com/zzet/gortex/blob/main/LICENSE.md

Source-available under PolyForm Small Business 1.0.0 with a
contributor-perks addendum. Free for organisations under defined
size and revenue thresholds; commercial license available for
larger deployments.

Copyright (c) 2024-2026 Andrey Kumanyaev <me@zzet.org>
`

// EmitPluginBundle writes a marketplace plugin layout under
// spec.TargetDir. The directory is created if missing; existing files
// within it are overwritten so re-runs converge on a deterministic
// output. Returns the list of paths written, relative to TargetDir,
// in stable order.
//
// Single source of truth: skills come from GlobalSkills, slash
// commands from SlashCommands. Both are reused from
// content.go without duplication, so a change to a skill's body
// flows through both the user-level install (~/.claude/skills/) and
// the marketplace plugin (claude-plugin/skills/) on the next emit.
func EmitPluginBundle(spec PluginBundleSpec) ([]string, error) {
	if spec.LayoutVariant == "" {
		spec.LayoutVariant = LayoutVariantAnthropic
	}
	if spec.LayoutVariant != LayoutVariantAnthropic {
		return nil, fmt.Errorf("unsupported plugin layout variant %q (only %q is implemented)", spec.LayoutVariant, LayoutVariantAnthropic)
	}
	if spec.TargetDir == "" {
		return nil, fmt.Errorf("EmitPluginBundle: TargetDir is required")
	}
	if spec.Version == "" {
		return nil, fmt.Errorf("EmitPluginBundle: Version is required (e.g. \"0.18.2\")")
	}

	if err := os.MkdirAll(spec.TargetDir, 0o755); err != nil {
		return nil, fmt.Errorf("create target dir: %w", err)
	}

	written := make([]string, 0, 16)
	write := func(rel string, body []byte, mode os.FileMode) error {
		full := filepath.Join(spec.TargetDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(rel), err)
		}
		if err := os.WriteFile(full, body, mode); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
		written = append(written, rel)
		return nil
	}

	// 1. .claude-plugin/plugin.json
	manifest := map[string]any{
		"name":        pluginName,
		"description": pluginDescription,
		"version":     spec.Version,
		"author": map[string]any{
			"name":  pluginAuthorName,
			"email": pluginAuthorEmail,
		},
		"homepage": pluginHomepage,
	}
	manifestJSON, err := marshalJSONStable(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal plugin.json: %w", err)
	}
	if err := write(".claude-plugin/plugin.json", manifestJSON, 0o644); err != nil {
		return nil, err
	}

	// 2. README.md
	if err := write("README.md", []byte(pluginReadmeBody), 0o644); err != nil {
		return nil, err
	}

	// 3. LICENSE
	if err := write("LICENSE", []byte(pluginLicenseBody), 0o644); err != nil {
		return nil, err
	}

	// 4. .mcp.json
	if err := write(".mcp.json", []byte(pluginMCPJSON), 0o644); err != nil {
		return nil, err
	}

	// 5. commands/gortex-*.md — sorted for deterministic output.
	for _, name := range sortedKeys(SlashCommands) {
		body := SlashCommands[name]
		if err := write(filepath.Join("commands", name), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}

	// 6. skills/<name>/SKILL.md — sorted for deterministic output.
	for _, name := range sortedKeys(GlobalSkills) {
		body := GlobalSkills[name]
		if err := write(filepath.Join("skills", name, "SKILL.md"), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}

	// 7. hooks/hooks.json + hooks-handlers/gortex-hook.sh
	if err := write("hooks/hooks.json", []byte(pluginHooksJSON), 0o644); err != nil {
		return nil, err
	}
	if err := write("hooks-handlers/gortex-hook.sh", []byte(pluginHookHandler), 0o755); err != nil {
		return nil, err
	}

	return written, nil
}

// marshalJSONStable returns indented JSON with sorted keys at every
// level. Two-space indent matches Claude Code's reference plugins;
// trailing newline keeps the file POSIX-text-friendly and avoids
// editor diff noise.
func marshalJSONStable(v any) ([]byte, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// sortedKeys returns the keys of m in lexicographic order. Used so
// the on-disk emit order is independent of map iteration order, which
// keeps the directory diff-stable across Go versions.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PluginManifestPath / PluginMCPPath / PluginHooksPath return the
// in-bundle paths the emitter writes. Exposed so callers (e.g. the
// CI drift guard) can refer to them without hardcoding path strings.
func PluginManifestPath() string { return filepath.Join(".claude-plugin", "plugin.json") }
func PluginMCPPath() string      { return ".mcp.json" }
func PluginHooksPath() string    { return filepath.Join("hooks", "hooks.json") }

// PluginCommandPaths returns the in-bundle paths of all slash command
// files in stable order.
func PluginCommandPaths() []string {
	out := make([]string, 0, len(SlashCommands))
	for _, name := range sortedKeys(SlashCommands) {
		out = append(out, filepath.Join("commands", name))
	}
	return out
}

// PluginSkillPaths returns the in-bundle paths of all SKILL.md files
// in stable order.
func PluginSkillPaths() []string {
	out := make([]string, 0, len(GlobalSkills))
	for _, name := range sortedKeys(GlobalSkills) {
		out = append(out, filepath.Join("skills", name, "SKILL.md"))
	}
	return out
}
