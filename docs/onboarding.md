# Onboarding — First 15 Minutes with Gortex

Just installed Gortex? This is the shortest path from "it's on my machine" to "it's helping my AI agent work faster." Follow it top-to-bottom the first time; skip sections you've already done on return visits.

## Prerequisites

- Gortex is installed (run `gortex version` — if that prints a version, you're good). Not installed? → see [README Installation](../README.md#installation).
- A repository you want to work in. We'll call it `~/projects/myapp` in the examples — substitute your own path.
- An AI coding assistant installed. Gortex auto-integrates with Claude Code, Cursor, Kiro, Windsurf, GitHub Copilot (VS Code), Continue.dev, Cline, OpenCode, and Antigravity.

## Two Setup Modes

**Global (recommended if you work across multiple repos):** one long-living daemon holds the graph for every repo you track. Every Claude Code session reads from the same shared index; cross-repo queries work by default.

**Per-repo (simpler, one index per project):** legacy default. Each MCP client spawns its own indexer.

Just run `gortex init` in your repo — when stdin is a terminal, it asks you which mode you want:

```
$ gortex init

How should Gortex integrate with your AI tools?
  [1] Global daemon (recommended) — one graph across all projects,
      per-client session isolation, live file watching, user-level hooks
  [2] Per-repo — isolated server per project; each Claude Code window
      spawns its own indexer (current default)
Choice [1/2] (default: 1): 1
Track this repository with the daemon now? [Y/n]: y
Start the daemon now (detached)? [Y/n]: y
```

For CI, scripts, or explicit control, pass the flags directly and the wizard is skipped:

```bash
gortex init --global --start --track   # explicit global setup
gortex init                            # in a non-TTY: falls back to per-repo
cd ~/projects/myapp && gortex init     # per-repo (pick [2] at the wizard)
```

Global is the more interesting experience once you have more than one repo. Per-repo is fine for single-project workflows. The rest of this doc covers per-repo in depth; see the [Global mode](#global-mode) section at the bottom for the daemon walkthrough.

## 30-Second Version

```bash
cd ~/projects/myapp
gortex init                   # interactive: pick global or per-repo, optionally start daemon + track repo
```

If you picked global + started the daemon, you're done — Claude Code will find Gortex on its next run. If you picked per-repo, also start the MCP server:

```bash
gortex serve --index . --watch # per-repo only; global mode auto-starts via the daemon
```

Now open your AI assistant in that repo and ask it to do something real. It'll use Gortex tools automatically. If that worked, the rest of this document is optional detail.

## Step-by-Step

### 1. Set up integration for the repo

```bash
cd ~/projects/myapp
gortex init
```

In a terminal this opens the interactive wizard (global vs per-repo, optional daemon start + track). In CI / non-TTY stdin, it silently falls through to per-repo mode — the historical behavior. Pass `--global` or `--per-repo` (via unset) explicitly to skip the prompt.

The per-repo path creates tool-specific config files (auto-detecting which tools you have installed). The output lists them all. Commit them — your teammates get Gortex for free when they pull.

**Key files it creates:**

- `.mcp.json` — tells MCP clients (Claude Code, Cursor, VS Code) how to start the Gortex server
- `CLAUDE.md` — instructions for Claude Code telling it to prefer Gortex tools over `Read` / `Grep`
- `.claude/settings.local.json` — installs three hooks: `PreToolUse` (redirects `Read` / `Grep` / `Glob` / `Task`), `PreCompact` (injects orientation snapshot before context compaction), `Stop` (post-task diagnostics)
- `.cursor/mcp.json`, `.kiro/settings/mcp.json`, `.vscode/mcp.json`, etc. — equivalents for other tools

**If your repo is unfamiliar:**

```bash
gortex init --analyze
```

This indexes the codebase first and prepends a repo-specific overview (language breakdown, entry points, communities, top hotspots) to the `CLAUDE.md` block. The AI will start with real context instead of generic tool instructions.

### 2. Start the MCP server

Two ways — pick whichever fits your workflow.

**Option A — you start it and leave it running.** Useful when multiple AI tools point at the same graph, or when you want the web UI:

```bash
gortex serve --index . --watch
```

`--watch` re-indexes changed files live via fsnotify. `--cache-dir ~/.cache/gortex` (default) saves snapshots between restarts so subsequent starts are ~200ms instead of 3-5s.

To also get the web UI + HTTP bridge:

```bash
gortex bridge --index . --web --watch
```

Open `http://localhost:4747` for the force-directed graph explorer.

**Option B — your IDE starts it automatically.** The `.mcp.json` that `gortex init` created tells the IDE how to spawn `gortex serve`. You don't run anything yourself. Claude Code, Cursor, and VS Code all work this way. Downside: each tool gets its own server process (memory cost scales with number of tools).

If you're unsure, start with Option A. You can always remove the `.mcp.json` → switch to Option B later.

### 3. Verify the integration

Open your AI assistant in the repo. Ask it something concrete that requires understanding the code:

> "What does the authentication flow look like? Trace it from the HTTP handler through to the database."

**What should happen:**

- The assistant calls `graph_stats` or `get_repo_outline` to orient itself
- Then `search_symbols "auth"` or `smart_context "authentication flow"` to find relevant code
- Then `get_call_chain` or `find_usages` on the specific handler
- Finally `get_symbol_source` on the specific functions — not `Read` on whole files

**What should NOT happen:**

- The assistant calls `Read` on 5 files and hunts for auth logic manually. If you see this, the hooks aren't wired up — run `gortex init --hooks-only` to reinstall just the hooks.

**Quick sanity check from the CLI:**

```bash
gortex status --index .
```

Prints node/edge counts, language breakdown, and per-repo stats. If this shows 0 nodes, the index didn't build — check for errors in `gortex serve` output.

### 4. Your first calls (if you're driving Gortex directly)

For debugging, writing custom agents, or working with the bridge HTTP API — the "good first calls" in order:

1. **`get_repo_outline`** — zero-arg narrative overview: primary languages, top communities, load-bearing hotspots, most-imported files, entry points. Takes ~1k tokens, covers "what is this repo?"
2. **`plan_turn` with your task description** — returns ranked recommended next calls. Example:
   ```json
   {"tool": "plan_turn", "args": {"task": "add rate limiting to auth handler"}}
   ```
   You get back a list like "smart_context → get_editing_context → find_usages" with pre-filled args.
3. **`smart_context` with the task** — does what `plan_turn` recommended as step 1, but assembles the actual context (relevant symbols, entry file structure, related tests) rather than just pointing at tools.
4. **Before editing any file — `get_editing_context` on its path.** Returns all symbols, signatures, direct dependencies, immediate callers. You don't need to read the file.

### 5. What the hooks do automatically

Once installed, three things happen without you lifting a finger:

- **PreToolUse on `Read` / `Grep` / `Glob`** — Gortex suggests the right graph tool instead and, for indexed source files, blocks whole-file reads.
- **PreToolUse on `Task`** — spawned subagents get a task-scoped briefing with `smart_context` results + a tool-swap table, so they don't inherit the bad habit of reaching for `Read`.
- **PreCompact** — just before Claude Code compacts the conversation, Gortex injects an orientation snapshot (recent edits, hotspots, feedback-ranked symbols) so the agent survives compaction without re-exploring.
- **Stop** — after the agent finishes a turn, Gortex runs `detect_changes` → `get_test_targets`, `check_guards`, `analyze dead_code`, `contracts check` on the symbols that changed, and feeds the results back so the agent self-corrects before handoff.

All four degrade silently when the bridge is unreachable — they never block your normal flow.

## Troubleshooting

**"Gortex MCP server failed to start" in the IDE.**
Check that `gortex` is on your `PATH` (`which gortex` should resolve). If you installed via Homebrew, restart the IDE — it caches PATH at launch.

**The AI still uses `Read` / `Grep` on source files.**
The hooks didn't install. Re-run `gortex init --hooks-only` and restart the AI tool. On Claude Code, also check that `.claude/settings.local.json` exists and contains `"gortex hook"` invocations under `hooks`.

**`graph_stats` returns `total_nodes: 0`.**
The index is empty. Either `gortex serve` isn't watching the right directory, or `.gortex.yaml` excludes everything. Run `gortex status --index /absolute/path/to/repo` to verify the paths.

**Indexing a big repo takes forever.**
First-time index of a 100k-symbol repo is ~20-30 seconds. On restart, it's ~200ms because the snapshot gets restored and only changed files re-index. Make sure `--cache-dir` isn't being deleted between runs.

**Semantic search isn't working.**
On first use, Gortex downloads the MiniLM-L6-v2 model (~90 MB) to `~/.cache/gortex/models/`. Needs network the first time; after that, fully offline. Check `~/.cache/gortex/models/sentence-transformers_all-MiniLM-L6-v2/` exists.

**"Cannot be opened because Apple cannot check it for malicious software" on macOS.**
You downloaded the binary from GitHub Releases via a browser. Either install via Homebrew (`brew install zzet/tap/gortex`) or run once: `xattr -d com.apple.quarantine /usr/local/bin/gortex`.

## Next Steps

Once the basics are working:

- **Multi-repo workspaces** — index several repos into one graph for cross-repo impact analysis. See [Multi-Repo Workspaces](../README.md#multi-repo-workspaces) in the README.
- **Guard rules** — add `.gortex.yaml` to declare architectural invariants (e.g., "UI must not import DB directly"). `check_guards` enforces them on every change. See `.gortex.yaml` in this repo for an example.
- **Per-community skills** — run `gortex skills .` to generate Claude Code skill files, one per detected subsystem. Each skill auto-activates when the agent asks about that area.
- **Token savings + cost tracking** — `gortex savings` prints cumulative tokens saved + dollars avoided per model across all sessions. Accumulates automatically; no setup.
- **Compact wire format (GCX1)** — every list-shaped tool accepts `format: "gcx"` for a round-trippable compact response. Median **−27.4% tokens** vs JSON on the benchmark, 100% round-trip integrity. Spec: [docs/wire-format.md](wire-format.md). TypeScript decoder on npm: [`@gortex/wire`](https://www.npmjs.com/package/@gortex/wire). Agents pick it up automatically — the PreToolUse and subagent hooks surface the opt-in. Applies to: `search_symbols`, `find_usages`, `analyze`, `contracts`, `batch_symbols`, `get_callers` / `get_call_chain` / `get_dependencies` / `get_dependents` / `find_implementations`, `get_file_summary`, `get_editing_context`, `smart_context`.
- **Feedback loop** — after a successful task, call the `feedback` MCP tool with `action: "record"`. Future `smart_context` results rerank based on what was actually useful.
- **Custom bridge integration** — `gortex bridge --index . --cors-origin '*'` exposes every MCP tool as HTTP. Good for editor plugins, CI hooks, custom dashboards.

## Global Mode

The global mode runs Gortex as a long-living daemon that holds the graph for every tracked repo. All MCP clients (Claude Code windows, Cursor, Kiro, etc.) connect to the same daemon via a Unix socket, so:

- Memory scales with workspace size, not open-editor count — one process instead of one per project.
- Cross-repo queries work by default: an agent in `frontend` can find callers in `backend` without extra config.
- Each session gets isolated per-client state (recent activity, token stats) via handshake-assigned session IDs.

### Setup

The simplest path: just run `gortex init` and pick option 1 at the wizard, saying yes to both follow-ups.

For non-interactive setup (CI, scripts, or just preference):

```bash
# One-time: install user-level Claude Code config + hooks, start the daemon,
# and track the current repo.
gortex init --global --start --track

# Track additional repos any time:
gortex track ~/projects/backend
gortex track ~/projects/shared-lib

# Remove a repo from the workspace:
gortex untrack backend        # by prefix, or by absolute path

# See state:
gortex status                 # tracked repos, node/edge counts, memory, sessions (via daemon if running)
gortex daemon status          # PID, uptime, socket path
gortex savings                # cumulative tokens saved + $ avoided across all sessions
```

### Daemon lifecycle

```bash
gortex daemon start --detach  # spawn in background
gortex daemon stop            # graceful shutdown + final snapshot
gortex daemon restart         # stop + start
gortex daemon reload          # re-read config, pick up new/removed repos
gortex daemon logs -n 50      # tail the log
```

### Auto-start at login (optional)

Let the OS supervise the daemon so it starts at login and restarts on crash. No sudo required — the unit lives under `$HOME`.

```bash
gortex daemon install-service   # launchd (macOS) or systemd --user (Linux)
gortex daemon service-status    # check installed state + active/inactive
gortex daemon uninstall-service # remove unit, stop service
```

On macOS the unit lands at `~/Library/LaunchAgents/com.zzet.gortex.plist`; on Linux at `~/.config/systemd/user/com.zzet.gortex.service`. After `install-service`, plain `gortex daemon start / stop` still work — they just fight the service for socket ownership, so prefer `gortex daemon service-status` and `launchctl` / `systemctl --user` commands for lifecycle.

### How it works

- `gortex serve` (what Claude Code spawns via `.mcp.json`) auto-detects the daemon. If reachable, it acts as a thin stdio ↔ socket proxy (~5 MB per client). If not, it falls back to the embedded server — global mode is never "required."
- Every tracked repo gets its own fsnotify watcher so edits on disk flow into the graph live; no manual reload needed. `gortex track` attaches a watcher as part of the track operation; `gortex untrack` detaches it before evicting nodes.
- Graph state is snapshotted to `~/.cache/gortex/daemon.gob.gz` on shutdown and every 10 minutes. Daemon restarts load it back and re-index only changed files.
- Opening Claude Code in an untracked directory returns a structured `repo_not_tracked` error on every tool call. The agent surfaces it; you run `gortex track .` to include it.
- Per-session state is isolated by a handshake-assigned session ID — two Claude Code windows see their own recent-activity and token-savings counters, not a merged view. Cumulative savings in `~/.cache/gortex/savings.json` are still shared.

### Fallback rules

| Invocation | Daemon running | Daemon not running |
|---|---|---|
| Claude Code spawns `gortex serve` | Proxies through daemon | Embedded server (current behavior) |
| `gortex track /path` | Immediate re-index + watcher attached via daemon | Writes config; takes effect on next daemon/server start |
| `gortex untrack /path` | Immediate graph eviction + watcher detached | Removes from config |
| `gortex status` | Aggregate across tracked repos | One-shot local index |
| `gortex daemon status` | PID, uptime, memory, sessions | "not running" |

Full architectural spec: [`spec-daemon.md`](../spec-daemon.md).

## Getting Help

- File issues and feature requests at [github.com/zzet/gortex/issues](https://github.com/zzet/gortex/issues).
- Full tool reference lives in `CLAUDE.md` (created by `gortex init`). Your AI agent already reads it; you can too.
