# CLI reference

```
gortex install               One-time machine-wide setup (user-level MCP, skills, hooks, daemon wiring)
gortex init [path]           Per-repo setup (.mcp.json, hooks, community routing, per-community SKILL.md)
gortex init doctor           Zero-op drift report across all detected agents (human or --json)
gortex mcp [flags]           Start the MCP stdio server (auto-detects daemon; --no-daemon / --proxy; --server adds HTTP API)
gortex daemon start [flags]  Start the daemon; --http-addr <addr> serves the HTTP/JSON API under /v1/* plus the MCP /mcp transport (--http-auth-token, --cors-origin)
gortex daemon <sub>          start / stop / restart / reload / status / logs / install-service / service-status / uninstall-service / server (multi-server roster)
gortex eval <sub>            Retrieval + token benchmarks — recall / embedders / swebench / tokens / baselines / quality (substrate; prefer `gortex bench` for the user-facing surface)
gortex eval-server [flags]   HTTP server used by the swebench harness
gortex bench <sub>           User-facing benchmark suite — recall / tokens / tokens-efficiency / embedders / perf / daemon-latency / swebench / all
gortex audit [flags]         A-F repo health grade derived from per-symbol complexity-axis health score
gortex gain [flags]          Forward-looking per-call USD savings projection from the latest bench tokens output
gortex context [flags]       Generate portable context briefing for a task
gortex savings [flags]       Token-savings dashboard (Today / Last 7 days / All time + USD avoided)
gortex index [path...]       Index one or more repositories and print stats
gortex status [flags]        Show index status (per-repo and per-project in multi-repo mode)
gortex repos [--json]        List every tracked repo with git head-commit SHA, last-indexed time, and a staleness flag
gortex track <path>          Add a repository to the tracked workspace
gortex untrack <path>        Remove a repository from the tracked workspace
gortex workspace <sub>       list [--json] / set / set-all — manage workspace + project slugs across tracked repos
gortex config exclude ...    add / list / remove entries in the effective ignore list
gortex query <sub>           Query the knowledge graph from the CLI
gortex prs [number]          List open PRs with a one-shot review-state classification, or deep-dive one PR's blast radius (gortex prs bundle <n> writes a reviewer graph bundle)
gortex review [path]         Review a changeset and print line-anchored inline comments + a BLOCK/REVIEW/APPROVE verdict (--diff, --base, --audience, --post)
gortex wiki [path]           Generate a multi-page markdown wiki (per-community + processes + analysis)
gortex docs [path]           Generate a "living docs" bundle (recent changes + ownership + stale + blame)
gortex export [path]         Export the graph to Cypher, GraphML, or Mermaid (--format mermaid --scope all)
gortex githook <sub>         install / uninstall / status — manage the post-commit hook
gortex clean                 Remove Gortex files from a project
gortex version               Print version
```

## One-time machine setup

```bash
gortex install                      # interactive-free: MCP + skills + slash commands + sub-agents at ~/.claude/
gortex install --start --track      # also spawn the daemon and track the current directory
gortex install --no-hooks           # skip user-level hook installation

# Daemon lifecycle (also spawned by `gortex install --start`):
gortex daemon start --detach        # spawn in background
gortex daemon status                # PID, uptime, memory, tracked repos, sessions, server roster
gortex daemon stop                  # graceful shutdown + final snapshot
gortex daemon restart               # stop + start
gortex daemon reload                # re-read config, pick up new/removed repos
gortex daemon logs -n 50            # tail the log file

# Multi-server roster — let the daemon route to additional Gortex servers (local sockets or remote HTTPS):
gortex daemon server list                                                  # show ~/.gortex/servers.toml
gortex daemon server add work --url https://gortex.work.example --auth-token-env WORK_TOK
gortex daemon server remove work

# Auto-start at login (launchd on macOS, systemd --user on Linux):
gortex daemon install-service
gortex daemon service-status
gortex daemon uninstall-service

# Track / untrack repos (daemon-first dispatch; falls back to config-only when no daemon):
gortex track ~/projects/backend
gortex untrack backend

# Per-repo status + daemon-wide status share the same command — it picks:
gortex status
```

## Per-repo setup

```bash
cd ~/projects/myapp
gortex init                             # writes .mcp.json, .claude/settings.*, CLAUDE.md with community routing
gortex init --analyze                   # also index first for a richer CLAUDE.md overview
gortex init --no-skills                 # skip community-routing generation
gortex init --skills-min-size 5 --skills-max 10   # tune the generator
gortex init --hooks-only                # (re)install repo-local hooks only, skip everything else
gortex init --no-hooks                  # full init but skip hook installation

# Run the MCP server standalone (auto-detects daemon via stdio; --no-daemon forces embedded):
gortex mcp --index /path/to/repo --watch
gortex mcp --no-daemon --watch          # explicit embedded mode
```

## Query subcommands

```
gortex query symbol <name>              Find symbols matching name
gortex query deps <id>                  Show dependencies
gortex query dependents <id>            Show blast radius
gortex query callers <func-id>          Show who calls a function
gortex query calls <func-id>            Show what a function calls
gortex query implementations <iface>    Show interface implementations
gortex query usages <id>                Show all usages
gortex query stats                      Show graph statistics
```

All query commands support `--format text|json|dot` (DOT output for Graphviz visualization).

## Pull-request review

```bash
# Triage the review queue — open PRs with CI rollup, review decision, age, and a
# one-shot review-state label (DRAFT / BASE_MISMATCH / CHANGES_REQUESTED / APPROVED
# / STALE / READY). Needs a GitHub token (GH_TOKEN / GITHUB_TOKEN).
gortex prs
gortex prs --worktrees                  # flag PRs whose head branch is checked out locally
gortex prs --base main --format json    # override the base branch; machine-readable output

# Deep-dive one PR: join its changed files against the graph for blast radius + risk.
gortex prs 1234                          # needs a running daemon that tracks the repo

# Write a reviewer graph bundle (impact + privacy-safe receipt + ranked reviewers).
gortex prs bundle 1234 -o pr-1234.json   # deterministic for an unchanged PR — diffable in CI

# Review a changeset → verdict (BLOCK / REVIEW / APPROVE) + line-anchored inline comments.
gortex review                            # unstaged changes (default scope)
gortex review --base main                # compare HEAD against a ref
gortex review --diff - < patch.diff      # review a pasted unified diff from stdin
gortex review --use-llm                  # fold in LLM-found findings (needs a configured provider)
gortex review --audience agent           # terse machine-first packet (vs the default human render)
gortex review --base main --post --pr 1234   # post the gated findings as inline PR comments (secrets redacted)
```

The deterministic correctness rulepack always runs (graph-grounded to drop false positives); `--use-llm` adds LLM findings relocated to exact lines. Posting to a public / fork PR is opt-in via `--confirm-public`; `--dry-run` prints the already-redacted payloads without any network call. The same surface is exposed to agents over MCP — see [mcp.md](mcp.md#pr-review).

## Other commands

```bash
gortex track . && gortex daemon start --http-addr 127.0.0.1:7411  # HTTP/JSON API on :7411 (/v1/* + /mcp). UI lives at github.com/gortexhq/web.
gortex savings [--verbose] [--json]      # Today / Last 7 days / All time bar-chart dashboard + $ avoided
gortex bench <sub>                       # user-facing benchmark suite (recall / tokens / tokens-efficiency / perf / daemon-latency / embedders / swebench / all)
gortex audit [--badge|--format svg|json|text]  # A-F repo health grade + README-ready SVG shield
gortex gain [--since 7d]                 # forward-looking per-call USD savings + optional history slice
gortex version
```

## Generated wiki + living diagrams

Run `gortex wiki .` to produce a Markdown wiki under `wiki/<repo-slug>/`:

```
wiki/
  index.md                    # top-level (single repo today, multi-repo extension point)
  <repo>/
    index.md                  # community navigation
    architecture.md           # community-level system overview
    communities/<n>-<slug>.md # one page per detected community
    processes/<slug>.md       # one page per discovered execution flow (Mermaid sequenceDiagram)
    contracts/api-surface.md  # HTTP / gRPC / GraphQL contracts
    analysis/{hotspots,cycles,semantic}.md
    _assets/community-graph.mermaid
  _workspace/                 # reserved for multi-repo pages
```

Pair with `gortex githook install post-commit --regen-mermaid --regen-wiki` to keep diagrams and docs in sync after every commit. The hook is idempotent and preserves any non-gortex content in the existing hook file.

For CI, drop `examples/.github/workflows/gortex-architecture.yml` into your repo: it re-runs `gortex export --format mermaid --scope all` on every push and opens a PR when the diagrams drift.

`gortex wiki --enhance` enables LLM-augmented narrative summaries via the configured `llm.provider` (claudecli for MVP — uses your local Claude Code subscription). Results are cached by `(node, content_hash)` so re-runs on unchanged inputs produce byte-identical output without re-invoking the LLM.
