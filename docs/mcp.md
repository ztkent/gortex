# MCP surface

Gortex exposes a knowledge-graph query surface over the [Model Context Protocol](https://modelcontextprotocol.io): **100+ tools, 16 resources, 3 prompts**. Agents call the same surface from stdio, the daemon Unix socket, or the MCP 2026 Streamable HTTP endpoint.

- [Tool discovery (lazy mode)](#tool-discovery-lazy-mode)
- [Core navigation](#core-navigation)
- [Graph traversal](#graph-traversal)
- [Search & traversal extensions](#search--traversal-extensions)
- [Dataflow (CPG-lite)](#dataflow-cpg-lite)
- [Structural search](#structural-search)
- [Diagnostics & code actions](#diagnostics--code-actions)
- [Proactive notifications](#proactive-notifications)
- [Coding workflow](#coding-workflow)
- [Agent-optimized (token efficiency)](#agent-optimized-token-efficiency)
- [Response re-cutting](#response-re-cutting)
- [Analysis](#analysis)
- [Proactive safety](#proactive-safety)
- [Code quality](#code-quality)
- [Code generation](#code-generation)
- [PR review](#pr-review)
- [Multi-repo management](#multi-repo-management)
- [Live editor buffers (overlay sessions)](#live-editor-buffers-overlay-sessions)
- [Speculative execution](#speculative-execution)
- [MCP resources (16)](#mcp-resources-16)
- [MCP prompts (3)](#mcp-prompts-3)

## Tool discovery (lazy mode)

By default the server publishes the entire tool surface in the initial `tools/list` payload. The lazy/discovery flow — where only a hot set of ~25 tools ships eagerly and the rest are fetched on demand through `tools_search` — is opt-in via `GORTEX_LAZY_TOOLS=1`. The opt-in default exists because dominant stdio MCP hosts (Claude Code among them) don't re-fetch `tools/list` when the server fires `notifications/tools/list_changed`, leaving every post-promotion tool unreachable; eager registration sidesteps that.

```jsonc
// With GORTEX_LAZY_TOOLS=1 set:
// Browse — list deferred tool names without schemas.
{"name":"tools_search","arguments":{}}

// Fetch schemas for specific tools by name (auto-promotes them into tools/list).
{"name":"tools_search","arguments":{"query":"select:flow_between,taint_paths,find_clones"}}

// Keyword search with required-token filter, ranked, capped at max_results.
{"name":"tools_search","arguments":{"query":"+overlay drop","max_results":5}}

// Fuzzy keyword match across name + description.
{"name":"tools_search","arguments":{"query":"memories invariants"}}
```

Returned tools are auto-promoted (`promote:false` opts out) and the server fires `notifications/tools/list_changed`. The `tool_profile` tool reports the active surface — which tools are live vs. deferred, their scopes, and (with a `tool` argument) a single tool's enabled status.

**Prompt-injection screening.** Every tool call is screened by middleware that scans arguments and result text for injection patterns. On a hit it attaches a non-blocking `_meta.gortex_security` advisory — the call still succeeds and the result body is never mutated. Disable with `GORTEX_MCP_SANITIZE=0`.

## Core navigation

| Tool | Description |
|------|-------------|
| `graph_stats` | Node/edge counts by kind, language, per-repo stats, session token savings, and an `edge_identity_revisions` counter (edges re-keyed when their provenance changed) |
| `search_symbols` | Find symbols by name (replaces Grep). Inline `kind:`/`lang:`/`path:` field clauses + `query_class` / `max_per_file` tuning; accepts `repo`, `project`, `ref`, `scope` params. `corpus: code\|docs\|all` selects the corpus (`docs` has its own retrieval channel + prose-tuned ranking); `vocab_anchored: true` constrains LLM expansion to the repo's own vocabulary; a zero-result identifier query is auto-decomposed into leaf terms (`decomposed: true`) |
| `search_text` | Trigram-accelerated literal (or `regexp: true`) code search across the repo — the alt grep backbone. Returns file/line/text rows, each carrying the enclosing symbol (`symbol_id` / `symbol_name`) |
| `winnow_symbols` | Structured constraint-chain retrieval — `kind`, `language`, `community`, `path_prefix`, `min_fan_in`, `min_fan_out`, `min_churn`, `text_match` with per-axis score contributions |
| `get_symbol` | Symbol location and signature (replaces Read). Accepts `repo`, `project`, `ref` params |
| `get_file_summary` | All symbols and imports in a file. Accepts `repo`, `project`, `ref`, `max_bytes` / `max_tokens` budget caps |
| `get_editing_context` | **Primary pre-edit tool** — symbols, signatures, callers, callees. Accepts `max_bytes` / `max_tokens` budget caps; `compress_bodies` stubs bodies, and `fidelity_globs` (e.g. `internal/**:full,*_test.go:omit,vendor/**:compress`) sets a per-glob full/compress/omit tier |
| `get_repo_outline` | Narrative single-call repo overview — top languages, communities, hotspots, most-imported files, entry points |
| `plan_turn` | Opening-move router — returns ranked next calls with pre-filled args for a task description (~200 tokens) |

## Graph traversal

| Tool | Description |
|------|-------------|
| `get_dependencies` | What a symbol depends on |
| `get_dependents` | What depends on a symbol (blast radius) |
| `get_call_chain` | Forward call graph. Accepts `max_bytes` / `max_tokens` budget caps |
| `get_callers` | Reverse call graph. A zero-caller result carries a caveat distinguishing "likely unused" from "possible extraction gap" so a pre-edit safety check isn't silently disarmed |
| `find_usages` | Every reference to a symbol. Each usage carries its reference `context` (parameter_type / return_type / field / value / type / attribute / call); pass `context:` to filter (e.g. "where is this type used as a parameter?"). Accepts `max_bytes` / `max_tokens` budget caps. Zero-edge results carry the same "likely unused" vs "possible extraction gap" caveat |
| `find_implementations` | Types implementing an interface |
| `find_overrides` | Methods that override (children) or are overridden by (parents) a method — backed by `EdgeOverrides` |
| `get_class_hierarchy` | Multi-hop inheritance subgraph around a type, interface, or method. Walks `EdgeExtends` + `EdgeImplements` + `EdgeComposes` (type nodes) and `EdgeOverrides` (method nodes); `direction` ∈ up / down / both, `include_methods` pulls members + their override chain |
| `get_cluster` | Bidirectional neighborhood |

## Search & traversal extensions

| Tool | Description |
|------|-------------|
| `find_declaration` | Use-site → declaration resolver. Accepts a literal substring or (with `regex: true`) a regex matching a use site like `fooBar(`; returns the declaration node plus the matching use locations. Trigram-prefiltered. Optional `path_prefix` / `kind` filters |
| `walk_graph` | Token-budgeted free-form graph traversal — walks arbitrary `edge_kinds` (CSV) outward / inward / both from a starting symbol; auto-stops at `token_budget`. Surfaces `budget_hit` / `stopped_at_depth` on the response. `community` (ID or label) confines the walk to a detected community |
| `context_closure` | Dependency-closure context selection — given a set of seed files / symbols, walks the transitive import / dependency closure and packs it under one `token_budget` (reusing the graded-manifest tiers), ranked by graph distance from the nearest seed or, with `rank: "proximity"`, by seeded random-walk proximity |
| `graph_query` | Ad-hoc graph-query escape hatch — small read-only DSL with `nodes` / `traverse` / `filter` stages joined by `\|`, e.g. `nodes kind=interface name~Handler \| traverse implements in \| filter path=internal/mcp/`. Bounded by `limit` and a five-stage cap |
| `nav` | Per-session symbol cursor — verb-dispatched via `action`: `goto` / `into` (a callee) / `up` (a caller) / `sibling` / `back` / `where` / `read`. Adjacency preview rides on every response; the cursor lives in session state and resets on disconnect |

## Dataflow (CPG-lite)

| Tool | Description |
|------|-------------|
| `flow_between` | Ranked dataflow paths between two symbols — walks `value_flow` / `arg_of` / `returns_to` edges |
| `taint_paths` | Pattern-driven source→sink dataflow sweep for security and architecture audits |

## Structural search

| Tool | Description |
|------|-------------|
| `search_ast` | Cross-language structural search by AST shape — raw tree-sitter S-expression `pattern` or a bundled `detector` (e.g. `sql-string-concat`, `weak-crypto`, `hardcoded-secret`) |

## Diagnostics & code actions

Wired across every running language server (gopls, tsserver, pyright, rust-analyzer, …). Server-driven capability registration via `client/registerCapability` / `client/unregisterCapability` is honoured live, so servers (jdtls, tsserver, rust-analyzer) that announce features *after* `initialize` no longer return empty results.

| Tool | Description |
|------|-------------|
| `subscribe_diagnostics` | Opt the session into push `notifications/diagnostics`; initial state replays immediately, deltas thereafter. Filter by `min_severity` / `path_prefix` |
| `unsubscribe_diagnostics` | Opt back out — idempotent, fires automatically on session disconnect |
| `get_diagnostics` | Latest stored diagnostics for a file; `wait: true` blocks on the first publish |
| `get_code_actions` | LSP code actions (quickfix / organizeImports / refactor / source) at a file location |
| `apply_code_action` | Apply a single code action to disk — atomic temp+rename |
| `fix_all_in_file` | Loop codeAction → apply → re-collect until convergence over the whole file |

## Proactive notifications

Four additional push channels modeled on `subscribe_diagnostics` — per-session opt-in, delta-filtered, initial replay, auto-cleanup on disconnect.

| Tool | Description |
|------|-------------|
| `subscribe_workspace_readiness` | `notifications/workspace_readiness` — daemon warmup phase transitions (snapshot_loaded → parallel_parse → deferred_passes_all → global_resolve → end_batch → watcher_started → ready). Last-known phase replayed to late subscribers. A graph tool *called during warmup* does not need this subscription to cope: it returns an in-band `warming` block plus best-effort partial results instead of blocking or erroring |
| `unsubscribe_workspace_readiness` | Opt back out — idempotent |
| `subscribe_daemon_health` | `notifications/daemon_health` — periodic ticker (default 15 s, `interval_ms` clamped to 1 s..5 min) snapshots uptime, alloc/sys/heap, num_goroutine, num_gc, tracked_repos, sessions, lsp_alive, graph nodes/edges. Ticker only runs while ≥1 subscriber is attached |
| `unsubscribe_daemon_health` | Opt back out — idempotent |
| `subscribe_stale_refs` | `notifications/stale_refs` — per-session intersect of watcher symbol-change events against the session's viewed/modified working set. Fires only when a change actually touches what *this* session has consumed |
| `unsubscribe_stale_refs` | Opt back out — idempotent |
| `subscribe_graph_invalidated` | `notifications/graph_invalidated` — coarse "the graph was rebuilt, drop cached results" signal. `{node_count, edge_count, reason, ts}`; unfiltered |
| `unsubscribe_graph_invalidated` | Opt back out — idempotent |

## Coding workflow

| Tool | Description |
|------|-------------|
| `get_symbol_source` | Source code of a single symbol (80% fewer tokens than Read). Returns `tokens_saved` per call. `compress_bodies` stubs bodies (with an optional `keep` subset); `max_lines` salience-truncates to a control-flow skeleton |
| `batch_symbols` | Multiple symbols with source/callers/callees in one call |
| `find_import_path` | Correct import path for a symbol |
| `explain_change_impact` | Risk-tiered blast radius with affected processes. A zero-edge target carries the "likely unused" vs "possible extraction gap" caveat |
| `get_recent_changes` | Files/symbols changed since timestamp |
| `edit_symbol` | Edit a symbol's source directly by ID — no Read needed. Line-ending tolerant: an LF-authored `old_source` matches a CRLF file (and vice versa) and the replacement adopts the file's endings (`eol_normalized: true` rides on the response). Optional `base_sha` content-hash guard refuses the write when the on-disk SHA has drifted; every success carries `new_sha` so the next edit can pipeline without re-reading |
| `edit_file` | Edit any file (markdown, config, spec, template, source) by exact string replacement — accepts absolute paths or repo-rooted paths. Line-ending tolerant: an LF-authored `old_string` matches a CRLF file (and vice versa) and the replacement is written with the file's own endings (`eol_normalized: true` rides on the response). Same `base_sha` / `new_sha` drift guard. Kills Read-before-Edit for files not in the graph |
| `write_file` | Create or overwrite any file — atomic temp+rename, re-indexes on write. Same `base_sha` / `new_sha` drift guard |
| `rename_symbol` | Coordinated multi-file rename with all references |
| `move_symbol` | Relocate a function / method / type / variable / const to another file. Cross-package moves rewrite every qualified reference, drop the source import, add the target import, synthesise the target file if missing. Go for now |
| `inline_symbol` | Replace every callsite of a trivial single-statement / single-expression callee with the body — refuses cleanly on defer, spawn, close-over-scope, multi-return, or side-effecting arg. `delete_after: true` removes the declaration. Go for now |
| `safe_delete_symbol` | Atomic dead-code removal with a graph-aware safety gate. A `cascade` parameter (`off` / `preview` / `apply`) drives a fixed-point orphan-propagation pass; cross-workspace and out-of-closure callers (and, by default, test-only callers) disqualify a candidate |
| `set_planning_mode` | Switch the session between a guaranteed no-writes planning phase and editing mode |
| `workflow` | Drive a phase-enforcement state machine (explore → implement → verify) — editing tools are gated until the implement phase |

## Agent-optimized (token efficiency)

| Tool | Description |
|------|-------------|
| `smart_context` | Task-aware minimal context — replaces 5-10 exploration calls. The working set is ranked through the full rerank pipeline. Always emits a `blast_radius` block (callers grouped by file + covering tests + a `no covering tests found` warning) and a file-clustered `working_set`; seed count and `token_budget` scale with graph size when unset. `fidelity: "graded"` returns a graph-distance-tiered `context_manifest` (large interchangeable symbol families are skeletonized to one representative) under one `token_budget`; `estimate: true` projects token cost without fetching; `if_none_match` dedups an unchanged pack to `not_modified` |
| `get_edit_plan` | Dependency-ordered edit sequence for multi-file refactors |
| `get_test_targets` | Maps changed symbols to test files and run commands |
| `get_untested_symbols` | Inverse of `get_test_targets` — functions/methods not reached from any test file, ranked by fan-in |
| `suggest_pattern` | Extracts code pattern from an example — source, registration, tests |
| `export_context` | Portable markdown/JSON context briefing for sharing outside MCP |
| `feedback` | `action: "record"`: report useful/missing symbols. `action: "query"`: aggregated stats — most useful, most missed, accuracy metrics |
| `ask` | Optional in-process LLM research agent (`-tags llama` + `llm.model`) — navigates the graph and returns a synthesized answer; `chain: true` for cross-repo call-chain tracing |

## Response re-cutting

Gortex captures every large tool response into a bounded per-session ring; these tools re-cut a captured response without re-issuing the original query.

| Tool | Description |
|------|-------------|
| `ctx_stats` | List the session's buffered responses — handles, tools, line / byte / token counts |
| `ctx_grep` / `grep_results` | Regex (or literal) search over a buffered response — structured `matches[]` plus a grep-style block with `-A`/`-B`/`-C` context |
| `ctx_slice` | An explicit line range of a buffered response |
| `ctx_peek` | Head + tail preview of a buffered response |
| `head_results` | The first N lines of a buffered response |

## Analysis

| Tool | Description |
|------|-------------|
| `get_communities` | Functional clusters (Louvain). Without `id`: list all. With `id`: members and cohesion for one community |
| `get_processes` | Discovered execution flows. Without `id`: list all. With `id`: step-by-step trace |
| `detect_changes` | Git diff mapped to affected symbols |
| `index_repository` | Index or re-index a repository path |
| `reindex_repository` | Incrementally re-index a tracked repository — whole-root, or scoped to an optional `paths` subset. Multi-repo aware |
| `contracts` | API contracts. `action: "list"` (default): detected HTTP/gRPC/GraphQL/topics/WebSocket/env/OpenAPI. `action: "check"`: orphan providers/consumers |
| `find_co_changing_symbols` | Ranked git co-change neighbours for a symbol — over the mined cosine-weighted `co_change` edge layer |
| `search_artifacts` | Full-text search over the context-artifacts manifest — DB schemas, API specs, infra configs, ADRs registered via `.gortex.yaml::artifacts` |
| `get_artifact` | Fetch one context artifact by id, with its content and the symbols it references |

## Proactive safety

| Tool | Description |
|------|-------------|
| `verify_change` | Check proposed signature changes against all callers and interface implementors |
| `check_guards` | Evaluate project guard rules (`.gortex.yaml`) against changed symbols |
| `audit_agent_config` | Scan CLAUDE.md / AGENTS.md / `.cursor/rules` / `.github/copilot-instructions.md` / `.windsurf/rules` / `.antigravity/rules` for stale symbol references, dead file paths, and bloat — validated against the live graph |

## Code quality

| Tool | Description |
|------|-------------|
| `analyze` | Unified graph analysis dispatcher. `kind` ∈ `dead_code`, `hotspots`, `cycles`, `would_create_cycle`, `connectivity_health`, `todos`, `blame`, `coverage`, `coverage_gaps`, `coverage_summary`, `stale_code`, `stale_flags`, `ownership`, `releases`, `cgo_users`, `wasm_users`, `orphan_tables`, `unreferenced_tables`, `channel_ops`, `goroutine_spawns`, `field_writers`, `race_writes`, `unclosed_channels`, `unsafe_patterns`, `health_score`, `impact`, `annotation_users`, `config_readers`, `env_var_users`, `sql_call_sites`, `fixes_history`, `edge_audit`, `domain`, `named`, `tests_as_edges`, `clusters`, `event_emitters`, `pubsub`, `string_emitters`, `error_surface`, `log_events`, `sql_rebuild`, `external_calls`, `routes`, `models`, `components`, `k8s_resources`, `images`, `kustomize`, `cross_repo`, `dbt_models`, `synthesizers`, `resolution_outcomes`. `clusters` takes an `algorithm` arg (`leiden` / `louvain` / `spectral`). `synthesizers` rolls up every framework-dispatch-synthesized edge by the pass that produced it; `resolution_outcomes` classifies unresolved call/reference edges by why the resolver gave up (`ambiguous_multi_match` / `candidate_out_of_scope` / `cross_language_only` / `stub_only` / `no_definition`) |
| `find_clones` | Near-duplicate function/method clusters from the MinHash + LSH `similar_to` layer; `dead_only: true` finds dead duplicates of live code |
| `index_health` | Health score, parse failures, stale files, language coverage |
| `get_symbol_history` | Symbols modified this session with counts; flags churning (3+ edits) |

## Code generation

| Tool | Description |
|------|-------------|
| `scaffold` | Generate code, registration wiring, and test stubs from an example symbol |
| `batch_edit` | Apply multiple edits in dependency order, re-index between steps |
| `diff_context` | Git diff enriched with callers, callees, community, processes, per-file risk |
| `prefetch_context` | Predict needed symbols from task description and recent activity. Accepts `max_bytes` / `max_tokens` budget caps |

## PR review

A graph-grounded pull-request review surface. The forge-data tools self-serve PR data via the daemon's own forge client (needs `GH_TOKEN` / `GITHUB_TOKEN` in the daemon environment), or accept caller-supplied data to skip the network; all are read-only. The review gate is AST-grounded — the deterministic correctness rulepack runs over the changeset and a graph-grounding pass drops false positives, with an opt-in LLM fold-in. The CLI exposes the same surface as `gortex prs` / `gortex review` ([cli.md](cli.md#pull-request-review)).

| Tool | Description |
|------|-------------|
| `list_prs` | List a repo's PRs with a one-shot review-state classification — a state label (DRAFT / BASE_MISMATCH / CHANGES_REQUESTED / APPROVED / STALE / READY), a normalized CI rollup (NONE / FAILURE / PENDING / SUCCESS), and merge blockers. Pass `prs` to classify an already-fetched set with no network call |
| `get_pr_impact` | Graph-joined blast radius + risk score for one PR — maps the PR's changed files to symbols, scores five risk axes (blast-radius flow, caller fan-in, coverage gap, security keywords, community span), groups the affected surface by community and caller/test file. `receipt: true` emits a privacy-safe review receipt |
| `triage_prs` | Rank a repo's open PRs by graph-derived review priority — `get_pr_impact` per PR ordered by composite risk (deterministic; `use_llm` re-ranks with one compact LLM pass + per-PR rationale). Decides which PR to review first |
| `pr_risk` | PR-level composite risk score for a set of changed symbols — five 0-100 axes into one score + a LOW/MEDIUM/HIGH/CRITICAL level and an ordered `review_priorities` list. Pass `ids` (mapped symbol IDs) or `base` (a git ref — changed set from the diff) |
| `conflicts_prs` | Surface merge-order conflict risk — maps each open PR to the graph communities it touches and reports the communities touched by more than one PR, with colliding PR numbers, a suggested safe merge order, and a conflict-risk score. Plan a merge train that minimises rebases |
| `suggest_reviewers` | Rank the people / teams best placed to review a changeset — blends CODEOWNERS matches, recent authorship of the changed symbols, and co-change experts into one ranked list with per-reviewer reasons. Pass `ids`, `base`, or `number` |
| `suggested_review_questions` | Prioritised, symbol-anchored review questions mapping the changeset to graph anomalies — bridge / hub_risk / surprising / thin_community / untested_hotspot — each tied to a symbol id + file + line with a HIGH/MEDIUM/LOW severity |
| `pr_review_context` | Deterministic, LLM-free PR-review rollup in one call — composes `diff_context`, `verify_change`, `simulate_chain` (gated on an explicit overlay session), and `audit_agent_config` into a composite PASS / WARN / BLOCK verdict. The cheap counterpart to `review_pack` |
| `sibling_diff_context` | Raw unified diff of the OTHER changed files in a changeset — the sibling changes a per-symbol / per-file review view filters out, ranked by relatedness to the focus (shared community/process → co-change → directory proximity) |
| `review` | Review a changeset → line-anchored inline comments + a BLOCK/REVIEW/APPROVE verdict. Runs the deterministic correctness rulepack (graph-grounded to drop false positives) over the changeset (`base` / `scope`, or a pasted `diff`); `use_llm` folds in LLM findings relocated to exact lines |
| `review_pack` | The single AST-grounded PR-review entrypoint — folds the graph-grounded review, per-symbol semantic classification, per-file risk, contract-impact + guard/architecture checks, and impacted test targets into one envelope, with a derived `verification_command` and a privacy-safe receipt |
| `critique_review` | Second, adversarial self-critique pass over a prior review's findings — asks the LLM (grounded in the diff) which findings are genuine vs false positives, returns the kept set, the dropped set each with a reason, and a revised verdict. Conservative: a disabled LLM keeps everything |
| `post_review` | Post review findings as inline comments on a GitHub PR / GitLab MR — each anchored to its file + line, batched into one review. Every body is secret-redacted before any payload is built; public / fork PRs require `confirm_public: true`; `dry_run: true` returns the would-post payloads with no network call |
| `suppress_finding` | Durably silence a review finding as a false positive (or `list` / `remove`) for the current repo — keyed over rule / category / symbol / file / source text so it survives the finding shifting lines. A permanent per-repo never-flag-again list (sidecar-backed) |

`analyze` also takes `kind: "review"` — the idiomatic/correctness rulepack (NPE / thread-safety check-then-act / N+1 / logic-error, Go + Python) with the same graph-grounded false-positive-reduction post-pass that backs the `review` tool.

## Multi-repo management

| Tool | Description |
|------|-------------|
| `track_repository` | Add a repo at runtime — indexes immediately, persists to global config |
| `untrack_repository` | Remove a repo — evicts nodes/edges, persists to global config |
| `set_active_project` | Switch active project scope for all subsequent queries |
| `get_active_project` | Return current project name and its member repositories |
| `list_repos` | List every project/repo in the active workspace |
| `workspace_info` | Workspace identity — bind mode, root directory, marker contents, discovered member set |
| `query_project` | Search symbols in another project or repo without a `set_active_project` switch — read-only cross-project lookup |
| `save_scope` | Save a named, reusable set of repository prefixes — accepted by `search_symbols` / `smart_context` via `scope` |
| `list_scopes` | List every saved repository scope |
| `delete_scope` | Delete a saved repository scope by name |

## Live editor buffers (overlay sessions)

Editor extensions push in-flight (unsaved) buffers as **overlays**. Gortex composes a per-request **shadow view** on top of the immutable base graph and threads it through the tool dispatch context — every subsequent `tools/call` from the same MCP session reads through the shadow. Graph-walking tools (`find_usages`, `get_call_chain`, `analyze`, …) and source-reading tools (`get_symbol_source`, `get_editing_context`, …) all see the editor-buffer state without per-tool changes.

**Base is never mutated by overlay flow.** Concurrent sessions each see their own view; the file watcher's reindex passes don't race with overlay queries; cross-file edges from non-overlaid files into overlaid symbols are preserved.

| Tool | Description |
|------|-------------|
| `overlay_register` | Bind an overlay session to the current MCP session ID (idempotent) |
| `overlay_push` | Push (or update) a single file overlay; `base_sha` enables drift detection, `deleted: true` previews a delete |
| `overlay_list` | List every overlay attached to the session — path / size / deleted / base_sha |
| `overlay_delete` | Remove one overlay from the session |
| `overlay_drop` | Tear down the session and discard every overlay |
| `overlay_keepalive` | Refresh the session's idle timer without re-pushing buffer content; cheap option for debugger / wizard pauses |
| `compare_with_overlay` | Run `find_usages` / `get_callers` / `get_call_chain` / `get_dependencies` / `get_dependents` against base AND overlay; returns added / removed / common ID sets |

**Branching — N parallel speculative sessions off one baseline.** Each overlay session carries an active-branch pointer plus a branches map; every legacy overlay tool operates on the active branch, so callers that never touch branches see exactly one implicit `main` branch and behave unchanged. With branches, an agent can hold strategy A and strategy B simultaneously off the same baseline, evaluate each, and merge the winner.

| Tool | Description |
|------|-------------|
| `overlay_fork` | Clone the active (or named) branch into a new branch; optional `activate: true` flips the session pointer |
| `overlay_branches` | List every branch with active flag, file count, `base_sha` anchor count, parent, and `created_at` |
| `overlay_switch` | Flip the session's active branch |
| `overlay_merge` | Fold one branch into another (default target: `main`) or write the branch to disk through the same atomic-write + `base_sha` drift guard as `edit_file`. Same-path divergent content is refused without `force: true`; `force` resolves last-writer-wins |
| `overlay_drop_branch` | Delete a named branch — refuses to drop the active branch or the implicit `main` |
| `compare_branches` | Run `find_usages` / `get_callers` / `get_call_chain` / `get_dependencies` / `get_dependents` against two branches and report each side plus the delta |

HTTP transport mirrors the surface at `/v1/overlay/sessions/*`; the `/v1/tools/<name>` entry point reads the overlay session from `Mcp-Session-Id` (preferred), `X-Gortex-Overlay-Session`, or `?session_id=`. Overlays are bound to their MCP session — when the session ends the overlay is dropped synchronously. Idle TTL is a fail-safe (default 30 m, configurable via `GORTEX_OVERLAY_IDLE_TTL`); every tool call against a live overlay refreshes it.

## Speculative execution

Built on the same shadow-graph substrate, `preview_edit` and `simulate_chain` answer **"what would change if I applied this WorkspaceEdit?"** without ever touching disk or mutating the base graph. The input is a standard LSP `WorkspaceEdit` (`changes` / `documentChanges`), so any agent that already produces WorkspaceEdits for code actions can speculate on them directly. Per-step impact: touched files, added / removed / renamed symbols (non-trivial-signature rename heuristic), broken callers, broken interface implementors, blast-radius rollup, suggested test targets, and (when an LSP is configured) round-trip diagnostics restored to the on-disk state at simulation end.

| Tool | Description |
|------|-------------|
| `preview_edit` | Single-shot WorkspaceEdit → impact report. Optional `diagnostics: false` skips the LSP round-trip. `inherit_overlay: true` layers on top of the caller's current overlay |
| `simulate_chain` | Ordered sequence of WorkspaceEdits applied in order with per-step impact + cumulative rollup + per-step diagnostics delta. `stop_on_error: true` (default) aborts on the first new ERROR-severity diagnostic. `keep: true` promotes the final simulated state into a real overlay session bound to the caller |

## MCP resources (16)

Read-only, URI-addressable, no args. Clients that speak resources can `resources/subscribe` once and receive `notifications/resources/updated` after each graph re-warm — no polling.

| Resource | Description |
|----------|-------------|
| `gortex://session` | Current session state and activity |
| `gortex://stats` | Graph statistics (node/edge counts) |
| `gortex://schema` | Graph schema reference |
| `gortex://index-health` | Health score, parse failures, stale files |
| `gortex://workspace` | Workspace identity and discovered member set |
| `gortex://repos` | Tracked repo / project list |
| `gortex://active-project` | Active project name and member repos |
| `gortex://communities` | Community list with cohesion scores |
| `gortex://community/{id}` | Single community detail |
| `gortex://processes` | Execution flow list |
| `gortex://process/{id}` | Single process trace |
| `gortex://report` | High-level orientation — graph size, top languages/kinds, hotspot / dead-code / todo counts |
| `gortex://god-nodes` | Top 20 hotspots |
| `gortex://surprises` | Cycles + dead code + cross-community call hubs |
| `gortex://audit` | `audit_agent_config` with discovery defaults |
| `gortex://questions` | TODO / FIXME / XXX / HACK / QUESTION rollup grouped by tag and assignee |

## MCP prompts (3)

| Prompt | Description |
|--------|-------------|
| `pre_commit` | Review uncommitted changes — shows changed symbols, blast radius, risk level, affected tests |
| `orientation` | Orient in an unfamiliar codebase — graph stats, communities, execution flows, key symbols |
| `safe_to_change` | Analyze whether it's safe to change specific symbols — blast radius, edit plan, affected tests |
