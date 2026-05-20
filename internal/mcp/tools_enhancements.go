package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/audit"
	"github.com/zzet/gortex/internal/blame"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/coverage"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/releases"
	"github.com/zzet/gortex/internal/tokens"
	"go.uber.org/zap"
)

// ensureFresh checks if any of the given file paths are stale (modified on disk
// since last index) and re-indexes up to 5 of them when watch mode is not active.
// Returns the list of file paths that were refreshed.
func (s *Server) ensureFresh(filePaths []string) []string {
	// Skip when watcher is active — it handles updates.
	if s.watcher != nil {
		return nil
	}
	if s.indexer == nil {
		return nil
	}

	var refreshed []string
	limit := 5
	for _, fp := range filePaths {
		if len(refreshed) >= limit {
			break
		}
		if s.indexer.IsStale(fp) {
			absPath := fp
			if root := s.indexer.RootPath(); root != "" {
				absPath = filepath.Join(root, fp)
			}

			// In multi-repo mode, the file path may be prefixed with a repo name
			// (e.g. "ade/internal/..."). If the resolved path doesn't exist, try
			// resolving via the MultiIndexer which knows each repo's root.
			if _, statErr := os.Stat(absPath); statErr != nil && s.multiIndexer != nil {
				resolved := s.multiIndexer.ResolveFilePath(fp)
				if resolved != "" {
					absPath = resolved
				}
			}

			if err := s.indexer.IndexFile(absPath); err != nil {
				s.logger.Warn("auto re-index failed",
					zap.String("file", fp),
					zap.String("resolved", absPath),
					zap.Error(err))
				continue
			}
			refreshed = append(refreshed, fp)
		}
	}
	return refreshed
}

func (s *Server) registerEnhancementTools() {
	// verify_change
	s.addTool(
		mcp.NewTool("verify_change",
			mcp.WithDescription("Given proposed signature changes, checks all callers and interface implementors for contract violations. Use before refactoring to catch breaking changes."),
			mcp.WithString("changes", mcp.Required(), mcp.Description("JSON array of {symbol_id, new_signature} objects")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-violation text output")),
		),
		s.handleVerifyChange,
	)

	// check_guards
	s.addTool(
		mcp.NewTool("check_guards",
			mcp.WithDescription("Evaluates project-specific guard rules against a set of changed symbols. Reports co-change and boundary violations."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of changed symbol IDs")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-rule text output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleCheckGuards,
	)

	// prefetch_context
	s.addTool(
		mcp.NewTool("prefetch_context",
			mcp.WithDescription("Predicts what context you will need next based on recent activity and a task description. Returns ranked symbols with relevance reasons."),
			mcp.WithString("task", mcp.Description("Natural language task description")),
			mcp.WithString("recent_symbols", mcp.Description("Comma-separated list of recently viewed symbol IDs")),
			mcp.WithBoolean("include_source", mcp.Description("Include source code for top 5 candidates")),
			mcp.WithNumber("limit", mcp.Description("Max candidates to return (default: 10)")),
			mcp.WithString("cursor", mcp.Description("Opaque pagination cursor from a previous `next_cursor` to fetch the next page.")),
			mcp.WithBoolean("paginate", mcp.Description("When true, the server caps each page at the project default budget and returns `next_cursor` for any tail.")),
			mcp.WithString("fields", mcp.Description("Comma-separated list of fields to keep on each candidate (e.g. \"id,confidence,reason\").")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description(tokenBudgetParamDescription)),
		),
		s.handlePrefetchContext,
	)

	// analyze — unified graph analysis tool (dead_code, hotspots, cycles, would_create_cycle)
	s.addTool(
		mcp.NewTool("analyze",
			mcp.WithDescription("Unified graph analysis. kind=dead_code: symbols with zero incoming edges. kind=hotspots: high-complexity symbols by fan-in/out. kind=cycles: circular dependency chains. kind=would_create_cycle: check if a new edge would form a cycle (requires from_id, to_id). kind=todos: list KindTodo nodes with optional tag/assignee/ticket/has_assignee filters. kind=blame: run `git blame` against the indexed repo and stamp meta.last_authored on every symbol-level node. kind=coverage: parse a Go cover.out profile (path via `profile` arg) and stamp meta.coverage_pct on every executable symbol. kind=stale_code: list symbols whose meta.last_authored is older than the threshold (requires blame-enriched graph). kind=ownership: group blame metadata by author email — symbol count, files touched, oldest/newest timestamps; supports path_prefix scoping (requires blame-enriched graph). kind=coverage_gaps: list symbols whose meta.coverage_pct falls in [min_pct, max_pct) — sorted ascending so the most undertested code surfaces first (requires coverage-enriched graph). kind=unsafe_patterns: bundled scan for panic-prone / undefined-behaviour primitives across every supported language — Go panic, Rust .unwrap/.expect/panic!/todo!/unimplemented!/unreachable!/assert!/unsafe blocks, Python assert, JS/TS throw — aggregated into one row-per-site response with a per-detector summary. Filters: language, detector, severity, path_prefix, limit, exclude_tests. kind=sast / kind=hygiene: Bandit-parity SAST rule library — 190+ structural rules across Python / Go / JS+TS / Java / Ruby / PHP / Rust, each carrying CWE + OWASP + tags metadata. Per-detector summary + per-CWE rollup. Filters: language, detector, severity, cwe, tag, path_prefix, limit, exclude_tests, kinds_only. kind=health_score: composite per-symbol health value (0..100) + A..F grade aggregated from coverage_pct, complexity (fan-in/out + community-crossings), recency (last_authored), and session churn. Per-axis breakdown surfaced on every row; missing axes are skipped (not zero-imputed). Always returns a population distribution (mean / median / std_dev / Gini coefficient over inequality of risk + per-grade counts). Pass roll_up='file' or 'repo' for per-file / per-repo averages with min/max bands and per-grade counts. Filters: path_prefix, kinds, grade, min_score, max_score, min_axes, limit, roll_up. Sorted ascending so worst symbols surface first. kind=impact: composite per-symbol change-impact score (0..100, higher = more impactful) plus a risk label, ranking symbols by blast radius from five axes — PageRank centrality, transitive reach, cyclomatic complexity, git co-change coupling, and community span. Per-axis breakdown on every row. Filters: ids, path_prefix, kinds, min_score, max_score, limit. kind=named: run a named query bundle — a reusable, named selection of structural detectors. Pass name=<bundle> to fan every selected detector across the codebase and aggregate matches; omit name to list every bundle. Ten bundles ship built-in (sql-injection, command-injection, hardcoded-secrets, weak-crypto, xss, unsafe-deserialization, path-traversal, ssrf, xxe, debug-leftovers); a repo defines its own in .gortex.yaml::queries. Filters: name, language, severity, path_prefix, limit, exclude_tests."),
			mcp.WithString("kind", mcp.Required(), mcp.Description("Analysis kind: dead_code | hotspots | cycles | would_create_cycle | todos | blame | coverage | stale_code | ownership | coverage_gaps | stale_flags | releases | cgo_users | wasm_users | orphan_tables | unreferenced_tables | coverage_summary | channel_ops | goroutine_spawns | field_writers | race_writes | unclosed_channels | unsafe_patterns | sast | hygiene | health_score | annotation_users | config_readers | event_emitters | pubsub | string_emitters | error_surface | log_events | sql_rebuild | external_calls | routes | models | components | k8s_resources | images | kustomize | cross_repo | dbt_models | impact | named")),
			mcp.WithString("framework", mcp.Description("(dbt_models) Filter to one transformation framework — dbt or sqlmesh")),
			mcp.WithString("materialized", mcp.Description("(dbt_models) Substring match on the model materialization — table, view, incremental, …")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-result text output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format, per-kind hand-tuned encoder), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithBoolean("include_variables", mcp.Description("(dead_code) Include variable nodes (default false — usually false positives without data-flow analysis)")),
			mcp.WithBoolean("include_fields", mcp.Description("(dead_code) Include struct/class field nodes (default false — graph can't always pick a candidate for intra-function field reads, so fields look dead even when used)")),
			mcp.WithBoolean("include_constants", mcp.Description("(dead_code) Include constant nodes (default false — same caveat as variables)")),
			mcp.WithBoolean("include_cgo_exports", mcp.Description("(dead_code) Include functions annotated //export — default false; CGo exports have no Go-level callers")),
			mcp.WithBoolean("include_linkname_targets", mcp.Description("(dead_code) Include //go:linkname targets — default false; they are linked by name from outside the package")),
			mcp.WithBoolean("skip_cross_repo_nodes", mcp.Description("(dead_code) Drop nodes whose RepoPrefix is set — useful when cross-repo linking is incomplete")),
			mcp.WithNumber("threshold", mcp.Description("(hotspots) Complexity score threshold (default: mean + 2σ)")),
			mcp.WithString("scope", mcp.Description("(cycles) File path or package prefix to limit scope")),
			mcp.WithString("from_id", mcp.Description("(would_create_cycle) Source symbol ID")),
			mcp.WithString("to_id", mcp.Description("(would_create_cycle) Target symbol ID")),
			mcp.WithString("profile", mcp.Description("(coverage) Path to a Go cover.out profile, absolute or relative to the indexed repo root")),
			mcp.WithNumber("older_than", mcp.Description("(stale_code) Symbols last touched more than this many days ago — default 365")),
			mcp.WithString("email", mcp.Description("(stale_code) Filter to a single author email")),
			mcp.WithString("kinds", mcp.Description("(stale_code, ownership) Comma-separated kinds — default function,method; pass 'all' for every blame-eligible kind")),
			mcp.WithNumber("min_symbols", mcp.Description("(ownership) Drop authors with fewer than this many symbols — default 1")),
			mcp.WithString("path_prefix", mcp.Description("(ownership, coverage_gaps) Scope to nodes under this file-path prefix — e.g. 'internal/auth/'")),
			mcp.WithNumber("min_pct", mcp.Description("(coverage_gaps) Lower-inclusive coverage threshold — default 0")),
			mcp.WithNumber("max_pct", mcp.Description("(coverage_gaps) Upper-exclusive coverage threshold — default 100, i.e. anything not fully covered")),
			mcp.WithString("provider", mcp.Description("(stale_flags) Filter to a single provider — launchdarkly, growthbook, unleash, internal")),
			mcp.WithString("tag", mcp.Description("(todos) Filter by tag — TODO / FIXME / HACK / XXX / NOTE — case-insensitive")),
			mcp.WithString("assignee", mcp.Description("(todos) Filter by exact assignee — case-sensitive")),
			mcp.WithString("ticket", mcp.Description("(todos) Filter by exact ticket reference — e.g. PROJ-42")),
			mcp.WithBoolean("has_assignee", mcp.Description("(todos) Keep only TODOs that have an assignee set")),
			mcp.WithString("repo", mcp.Description("(cross_repo) Scope to repo-boundary dependencies touching this repository prefix on either side")),
			mcp.WithString("base_kind", mcp.Description("(cross_repo) Scope to one base relation — calls, implements, or extends")),
			mcp.WithNumber("limit", mcp.Description("(cross_repo, error_surface, unsafe_patterns) Cap the number of rows returned — default 200")),
			mcp.WithString("language", mcp.Description("(unsafe_patterns, sast, hygiene) Comma-separated subset of languages to keep — rust, python, javascript, typescript, go, java, ruby, php")),
			mcp.WithString("detector", mcp.Description("(unsafe_patterns, sast, hygiene) Comma-separated subset of bundled detector names. The full catalog is available via the search_ast tool with no args; for SAST the names follow `py-*` / `go-*` / `js-*` / `java-*` / `ruby-*` / `php-*` / `rust-*` / `hygiene-*` conventions.")),
			mcp.WithString("severity", mcp.Description("(unsafe_patterns, sast, hygiene) Comma-separated subset of severity labels to keep — error, warning, info")),
			mcp.WithBoolean("exclude_tests", mcp.Description("(unsafe_patterns, sast, hygiene) Override the per-detector default (defaults to true — test-only matches are dropped)")),
			mcp.WithString("cwe", mcp.Description("(sast) Comma-separated subset of MITRE CWE identifiers to keep — e.g. 'CWE-78,CWE-89'")),
			mcp.WithBoolean("kinds_only", mcp.Description("(sast) Return only the per-detector + per-CWE breakdown; omit per-site `matches` rows. Use for a SAST surface snapshot without paying row bytes.")),
			mcp.WithString("grade", mcp.Description("(health_score) Comma-separated A..F subset to keep — e.g. 'd,f' for the worst-scoring symbols only")),
			mcp.WithNumber("min_score", mcp.Description("(health_score) Drop rows whose composite score is below this (0..100)")),
			mcp.WithNumber("max_score", mcp.Description("(health_score) Drop rows whose composite score is above this (0..100)")),
			mcp.WithNumber("min_axes", mcp.Description("(health_score) Require at least this many populated axes per row (default 1; raise to demand multi-signal confidence)")),
			mcp.WithString("roll_up", mcp.Description("(health_score) Aggregate per-symbol scores up to a coarser scope — 'file' (per-file average + per-grade counts) or 'repo' (per-repo). Omit for per-symbol rows.")),
			mcp.WithString("ids", mcp.Description("(impact) Comma-separated symbol IDs — score only these, the blast radius of changing specific symbols.")),
			mcp.WithString("name", mcp.Description("(named) The query bundle to run. Omit to list every available bundle.")),
		),
		s.handleAnalyze,
	)

	// winnow_symbols — multi-axis constraint-chain retrieval
	s.addTool(
		mcp.NewTool("winnow_symbols",
			mcp.WithDescription("Structured constraint-chain retrieval. Combines BM25 text matching with structural filters (kind, language, fan-in/out, community, path prefix, churn, test classification) and returns a ranked list with per-axis score contributions. Use when search_symbols' free-text-only query is too coarse — e.g. 'methods in the auth community with fan-in >= 5 touching handlers/' or 'production functions only, no tests'."),
			mcp.WithString("kind", mcp.Description("Comma-separated node kinds to keep (function, method, type, interface, variable, contract)")),
			mcp.WithString("language", mcp.Description("Filter to a single language (go, typescript, python, ...)")),
			mcp.WithString("path_prefix", mcp.Description("Comma-separated file path prefixes — any match passes")),
			mcp.WithString("community", mcp.Description("Community ID (community-0) or label to scope to a functional cluster")),
			mcp.WithString("text_match", mcp.Description("BM25 text query; when absent ranking is purely structural")),
			mcp.WithNumber("min_fan_in", mcp.Description("Minimum incoming calls+references (default: 0)")),
			mcp.WithNumber("min_fan_out", mcp.Description("Minimum outgoing calls (default: 0)")),
			mcp.WithNumber("min_churn", mcp.Description("Minimum session modification count (default: 0)")),
			mcp.WithBoolean("is_test", mcp.Description("Tri-state test filter: true keeps only test symbols, false keeps only production symbols. Omit for no constraint.")),
			mcp.WithString("test_role", mcp.Description("Comma-separated test roles to keep: test, benchmark, fuzz, example")),
			mcp.WithNumber("limit", mcp.Description("Max results (default: 20)")),
			mcp.WithString("cursor", mcp.Description("Opaque pagination cursor from a previous `next_cursor` to fetch the next page.")),
			mcp.WithBoolean("paginate", mcp.Description("When true, the server caps each page at the project default budget and returns `next_cursor` for any tail.")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response.")),
			mcp.WithString("fields", mcp.Description("Comma-separated list of fields to keep on each result (e.g. \"id,score,fan_in\").")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-result text output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
		),
		s.handleWinnowSymbols,
	)

	// scaffold
	s.addTool(
		mcp.NewTool("scaffold",
			mcp.WithDescription("Generates code scaffolding from an existing symbol pattern, including registration wiring and test stubs."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol ID to use as the pattern example")),
			mcp.WithString("new_name", mcp.Required(), mcp.Description("Name for the new symbol")),
			mcp.WithBoolean("dry_run", mcp.Description("Return scaffold without writing files (default: true)")),
			mcp.WithBoolean("compact", mcp.Description("Compact text output")),
		),
		s.handleScaffold,
	)

	// diff_context
	s.addTool(
		mcp.NewTool("diff_context",
			mcp.WithDescription("Returns graph-enriched context for symbols affected by a git diff: source, callers, callees, community, processes, and per-file risk."),
			mcp.WithString("scope", mcp.Description("unstaged (default), staged, all, or compare")),
			mcp.WithString("base_ref", mcp.Description("Branch/commit for compare scope (default: main)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol condensed output")),
		),
		s.handleDiffContext,
	)

	// index_health
	s.addTool(
		mcp.NewTool("index_health",
			mcp.WithDescription("Reports the health and completeness of the Gortex index: parse failures, stale files, language coverage, and health score."),
			mcp.WithBoolean("compact", mcp.Description("Single-line summary output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleIndexHealth,
	)

	// get_symbol_history
	s.addTool(
		mcp.NewTool("get_symbol_history",
			mcp.WithDescription("Returns symbols modified during the current session with modification counts. Flags churning symbols (modified 3+ times)."),
			mcp.WithString("id", mcp.Description("Specific symbol ID (omit for all)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output")),
		),
		s.handleGetSymbolHistory,
	)

	// batch_edit
	s.addTool(
		mcp.NewTool("batch_edit",
			mcp.WithDescription("Applies multiple symbol edits in dependency order. Re-indexes after each edit. Stops on failure and reports status."),
			mcp.WithString("edits", mcp.Required(), mcp.Description("JSON array of {id, old_source, new_source} objects")),
			mcp.WithBoolean("dry_run", mcp.Description("Return dependency-ordered plan without applying changes")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-edit summary")),
		),
		s.handleBatchEdit,
	)

	// contracts — unified contracts tool (list + check + validate)
	s.addTool(
		mcp.NewTool("contracts",
			mcp.WithDescription("API contracts tool. action=list (default): lists detected contracts (HTTP, gRPC, GraphQL, topics, WebSocket, env, OpenAPI). action=check: detects orphan providers/consumers across repos. action=validate: diffs provider↔consumer request/response shapes and flags breaking/warning/info issues.\n\nDEFAULT SCOPE for list: auto-scopes to the active project's repos and hides dependency-origin contracts (type=dependency, vendored paths like vendor/, node_modules/). The response reports other_repos (count of contracts filtered out of scope) and dependencies_skipped (count of dep contracts hidden). To widen scope, pass repo=<prefix>, project=<name>, ref=<tag>, or all_repos=true. To include dependency contracts, pass include_deps=true."),
			mcp.WithString("action", mcp.Description("list (default), check, or validate")),
			mcp.WithString("repo", mcp.Description("Filter by repository prefix")),
			mcp.WithString("project", mcp.Description("Filter to repositories in a specific project (resolves to the project's repo set)")),
			mcp.WithString("ref", mcp.Description("Filter to repositories tagged with this ref")),
			mcp.WithBoolean("all_repos", mcp.Description("(list) Disable active-project auto-scope; return contracts from every indexed repo. Default false.")),
			mcp.WithBoolean("include_deps", mcp.Description("(list) Include type=dependency contracts and contracts from vendored paths (vendor/, node_modules/, Pods/, .venv/). Default false.")),
			mcp.WithString("type", mcp.Description("(list) Filter by type: http, grpc, graphql, topic, ws, env, openapi, dependency")),
			mcp.WithString("role", mcp.Description("(list) Filter by role: provider or consumer")),
			mcp.WithNumber("limit", mcp.Description("(list) Max contracts per page (default: 200)")),
			mcp.WithString("cursor", mcp.Description("(list) Opaque pagination cursor from a previous `next_cursor` to fetch the next page.")),
			mcp.WithBoolean("paginate", mcp.Description("(list) When true, caps each page at the project default budget and returns `next_cursor` for any tail.")),
			mcp.WithNumber("max_bytes", mcp.Description("(list) Cap the marshaled response at this many bytes; the longest list is trimmed with truncation metadata.")),
			mcp.WithString("fields", mcp.Description("(list) Comma-separated list of fields to keep on each contract (e.g. \"type,role,id\").")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-contract text output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleContracts,
	)

	// feedback — unified feedback tool (record + query)
	s.addTool(
		mcp.NewTool("feedback",
			mcp.WithDescription("Agent learning feedback. action=record: report which symbols from smart_context/prefetch_context were useful, not_needed, or missing (improves future context). action=query: aggregated stats — most useful, most missed, accuracy."),
			mcp.WithString("action", mcp.Required(), mcp.Description("record or query")),
			mcp.WithString("task", mcp.Description("(record) The task description used in the original context call")),
			mcp.WithString("useful", mcp.Description("(record) Comma-separated symbol IDs that were useful")),
			mcp.WithString("not_needed", mcp.Description("(record) Comma-separated symbol IDs that were returned but not needed")),
			mcp.WithString("missing", mcp.Description("(record) Comma-separated symbol IDs that should have been included")),
			mcp.WithString("tool_source", mcp.Description("Which tool produced the context: smart_context or prefetch_context (default: smart_context). For query: filter by source or 'all'")),
			mcp.WithNumber("top_n", mcp.Description("(query) Number of top symbols to return per category (default: 10)")),
			mcp.WithBoolean("compact", mcp.Description("(query) One-line-per-symbol text output")),
			mcp.WithString("format", mcp.Description("(query) Output format: json (default), gcx (GCX1 compact wire format), or toon")),
		),
		s.handleFeedback,
	)

	// export_context
	s.addTool(
		mcp.NewTool("export_context",
			mcp.WithDescription("Generates a portable context briefing for a task as self-contained markdown or JSON. Use for sharing context outside MCP — paste into Slack, PRs, docs, or non-MCP AI tools."),
			mcp.WithString("task", mcp.Required(), mcp.Description("Natural language task description")),
			mcp.WithString("entry_point", mcp.Description("Optional symbol ID or file path to start from")),
			mcp.WithNumber("max_symbols", mcp.Description("Max symbols to include (default: 5)")),
			mcp.WithString("format", mcp.Description("Output format: markdown (default) or json")),
			mcp.WithNumber("token_budget", mcp.Description("Approximate token budget for output (default: 2000, max: 8000)")),
		),
		s.handleExportContext,
	)

	// audit_agent_config
	s.addTool(
		mcp.NewTool("audit_agent_config",
			mcp.WithDescription("Scans agent config files (CLAUDE.md, AGENTS.md, .cursor/rules, .github/copilot-instructions.md, etc.) for stale symbol references, dead file paths, and bloat — validated against the Gortex graph."),
			mcp.WithString("files", mcp.Description("Optional comma-separated file paths to audit (relative to repo root). If omitted, auto-discovers known agent config files.")),
			mcp.WithString("root", mcp.Description("Optional repo root override. Defaults to the indexer's root.")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-finding text output")),
		),
		s.handleAuditAgentConfig,
	)
}

// ---------------------------------------------------------------------------
// 10.2 handleVerifyChange
// ---------------------------------------------------------------------------

func (s *Server) handleVerifyChange(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	changesStr, err := req.RequireString("changes")
	if err != nil {
		return mcp.NewToolResultError("changes is required"), nil
	}

	var changes []analysis.SignatureChange
	if err := json.Unmarshal([]byte(changesStr), &changes); err != nil {
		return mcp.NewToolResultError("invalid changes JSON: " + err.Error()), nil
	}
	if len(changes) == 0 {
		return mcp.NewToolResultError("changes array is empty"), nil
	}

	result := analysis.VerifyChanges(s.graph, s.engine, changes)

	if isCompact(req) {
		var b strings.Builder
		for _, v := range result.Violations {
			fmt.Fprintf(&b, "%s %s %s:%d %s\n", v.Kind, v.SymbolID, v.FilePath, v.Line, v.Description)
		}
		if result.Clean {
			fmt.Fprintf(&b, "clean: checked %d callers, %d implementors\n", result.CheckedCallers, result.CheckedImpls)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, result)
}

// ---------------------------------------------------------------------------
// 10.3 handleCheckGuards
// ---------------------------------------------------------------------------

func (s *Server) handleCheckGuards(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("ids")
	if err != nil {
		return mcp.NewToolResultError("ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	if len(s.guardRules) == 0 && s.architecture.IsEmpty() {
		if s.isGCX(ctx, req) {
			return s.gcxResponseWithBudget(req)(encodeCheckGuards(nil, true))
		}
		empty := map[string]any{
			"violations": []any{},
			"message":    "no guard rules configured",
		}
		if s.isTOON(ctx, req) {
			return returnTOON(empty)
		}
		return s.respondJSONOrTOON(ctx, req, empty)
	}

	violations := analysis.EvaluateGuards(s.graph, s.guardRules, ids)
	violations = append(violations, analysis.EvaluateArchitecture(s.graph, s.architecture, ids)...)

	if isCompact(req) {
		var b strings.Builder
		for _, v := range violations {
			fmt.Fprintf(&b, "%s %s %s\n", v.Kind, v.RuleName, v.Description)
		}
		if len(violations) == 0 {
			b.WriteString("no guard rule violations\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeCheckGuards(violations, false))
	}

	result := map[string]any{
		"violations": violations,
		"total":      len(violations),
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// ---------------------------------------------------------------------------
// 10.4 handlePrefetchContext
// ---------------------------------------------------------------------------

// prefetchCandidate holds a scored symbol for prefetch ranking.
type prefetchCandidate struct {
	Node            *graph.Node `json:"-"`
	ID              string      `json:"id"`
	Kind            string      `json:"kind"`
	FilePath        string      `json:"file_path"`
	StartLine       int         `json:"start_line"`
	Reason          string      `json:"reason"`
	Confidence      float64     `json:"confidence"`
	SearchRelevance float64     `json:"-"`
	GraphProximity  float64     `json:"-"`
	CommunityBonus  float64     `json:"-"`
	Source          string      `json:"source,omitempty"`
}

func (s *Server) handlePrefetchContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task := req.GetString("task", "")
	recentStr := req.GetString("recent_symbols", "")
	includeSource := false
	if v, ok := req.GetArguments()["include_source"].(bool); ok {
		includeSource = v
	}

	// Gather recent symbols from parameter or session state.
	var recentIDs []string
	if recentStr != "" {
		for _, id := range strings.Split(recentStr, ",") {
			recentIDs = append(recentIDs, strings.TrimSpace(id))
		}
	}
	if len(recentIDs) == 0 {
		sess := s.sessionFor(ctx)
		sess.mu.Lock()
		recentIDs = append(recentIDs, sess.viewedSymbols...)
		sess.mu.Unlock()
	}

	if task == "" && len(recentIDs) == 0 {
		return mcp.NewToolResultError("insufficient context for prefetch: provide a task description or recent_symbols"), nil
	}

	// Score map: symbolID → scores
	type scores struct {
		search    float64
		proximity float64
		community float64
		feedback  float64
		reason    string
		node      *graph.Node
	}
	scoreMap := make(map[string]*scores)

	getOrCreate := func(n *graph.Node) *scores {
		if sc, ok := scoreMap[n.ID]; ok {
			return sc
		}
		sc := &scores{node: n}
		scoreMap[n.ID] = sc
		return sc
	}

	// 1. BM25 search on task description (weight 0.4)
	if task != "" {
		searchResults := s.scopedNodeSlice(ctx, s.engineFor(ctx).SearchSymbols(task, 30))
		maxScore := 1.0
		for i, n := range searchResults {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sc := getOrCreate(n)
			// Decay score by rank position
			relevance := 1.0 / float64(i+1)
			if relevance > maxScore {
				maxScore = relevance
			}
			sc.search = relevance
			if sc.reason == "" {
				sc.reason = "matches task keyword"
			}
		}
		// Normalize search scores
		if maxScore > 0 {
			for _, sc := range scoreMap {
				sc.search = sc.search / maxScore
			}
		}
	}

	// 2. Graph proximity from recent symbols (weight 0.4)
	communities := s.getCommunities()
	recentCommSet := make(map[string]bool)

	for _, rid := range recentIDs {
		if communities != nil {
			if cid, ok := communities.NodeToComm[rid]; ok {
				recentCommSet[cid] = true
			}
		}
		// Get neighbors at depth 1-2
		sg := s.engineFor(ctx).GetDependencies(rid, query.QueryOptions{Depth: 2, Limit: 30, Detail: "brief"})
		for _, n := range sg.Nodes {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sc := getOrCreate(n)
			// Closer = higher score
			proximity := 0.5 // depth 2
			// Check if depth 1
			for _, e := range sg.Edges {
				if (e.From == rid && e.To == n.ID) || (e.To == rid && e.From == n.ID) {
					proximity = 1.0
					break
				}
			}
			if proximity > sc.proximity {
				sc.proximity = proximity
				sc.reason = fmt.Sprintf("graph neighbor of %s", rid)
			}
		}
		// Also check dependents (callers)
		callers := s.engineFor(ctx).GetCallers(rid, query.QueryOptions{Depth: 1, Limit: 20, Detail: "brief"})
		for _, n := range callers.Nodes {
			if n.ID == rid || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sc := getOrCreate(n)
			if 1.0 > sc.proximity {
				sc.proximity = 1.0
				sc.reason = fmt.Sprintf("caller of %s", rid)
			}
		}
	}

	// 3. Community bonus (weight 0.2)
	if communities != nil && len(recentCommSet) > 0 {
		for _, sc := range scoreMap {
			if cid, ok := communities.NodeToComm[sc.node.ID]; ok {
				if recentCommSet[cid] {
					sc.community = 1.0
					if sc.reason == "" {
						sc.reason = "same community as recent activity"
					}
				}
			}
		}
	}

	// 4. Feedback signal (weight 0.15 when data exists, else use original 3-signal weights).
	hasFeedback := s.feedback != nil && s.feedback.HasData()
	if hasFeedback {
		for _, sc := range scoreMap {
			fbScore := s.feedback.GetSymbolScore(sc.node.ID)
			// Normalize from [-1, 1] to [0, 1].
			sc.feedback = (fbScore + 1.0) / 2.0
		}
	}

	// Compute combined scores and build candidates
	var candidates []prefetchCandidate
	for id, sc := range scoreMap {
		// Exclude recently viewed symbols themselves
		isRecent := false
		for _, rid := range recentIDs {
			if id == rid {
				isRecent = true
				break
			}
		}
		if isRecent {
			continue
		}

		var combined float64
		if hasFeedback {
			combined = 0.35*sc.search + 0.35*sc.proximity + 0.15*sc.community + 0.15*sc.feedback
		} else {
			combined = 0.4*sc.search + 0.4*sc.proximity + 0.2*sc.community
		}
		if combined <= 0 {
			continue
		}
		// Clamp confidence to [0, 1]
		confidence := math.Min(combined, 1.0)

		candidates = append(candidates, prefetchCandidate{
			Node:            sc.node,
			ID:              id,
			Kind:            string(sc.node.Kind),
			FilePath:        sc.node.FilePath,
			StartLine:       sc.node.StartLine,
			Reason:          sc.reason,
			Confidence:      math.Round(confidence*1000) / 1000,
			SearchRelevance: sc.search,
			GraphProximity:  sc.proximity,
			CommunityBonus:  sc.community,
		})
	}

	// Sort by confidence descending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Confidence > candidates[j].Confidence
	})

	// Default page size 10, capped at totalCount. The cursor opens the
	// rare "I want more than the top 10" path without making it the
	// default — agents that don't paginate get the same first page
	// they always got.
	totalCount := len(candidates)
	limit := req.GetInt("limit", 10)
	if limit <= 0 {
		limit = 10
	}
	offset := decodeCursor(req.GetString("cursor", ""))
	if offset > totalCount {
		offset = totalCount
	}
	endIdx := offset + limit
	if endIdx > totalCount {
		endIdx = totalCount
	}
	candidates = candidates[offset:endIdx]
	truncated := endIdx < totalCount
	nextCursor := ""
	if truncated {
		nextCursor = encodeCursor(endIdx)
	}

	// Include source for top 5 if requested
	if includeSource {
		for i := range candidates {
			if i >= 5 {
				break
			}
			n := candidates[i].Node
			if n.StartLine > 0 && n.EndLine > 0 {
				if absPath, err := s.resolveNodePath(n); err == nil {
					if source, _, _, err := readLines(absPath, n.StartLine, n.EndLine, 0); err == nil {
						candidates[i].Source = source
					}
				}
			}
		}
	}

	if isCompact(req) {
		var b strings.Builder
		for _, c := range candidates {
			fmt.Fprintf(&b, "%s %s %s:%d %.3f %s\n", c.Kind, c.ID, c.FilePath, c.StartLine, c.Confidence, c.Reason)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total)\n", totalCount)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodePrefetchContext(candidates, totalCount, truncated, includeSource))
	}

	result := map[string]any{
		"candidates": candidates,
		"total":      totalCount,
		"truncated":  truncated,
	}
	if nextCursor != "" {
		result["next_cursor"] = nextCursor
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// ---------------------------------------------------------------------------
// handleAnalyze — unified dispatcher for graph analysis (replaces 4 tools)
// ---------------------------------------------------------------------------

func (s *Server) handleAnalyze(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	kind, err := req.RequireString("kind")
	if err != nil {
		return mcp.NewToolResultError("kind is required (one of: dead_code, hotspots, cycles, would_create_cycle, todos, blame, coverage, stale_code, ownership, coverage_gaps, stale_flags, releases, cgo_users, wasm_users, orphan_tables, unreferenced_tables, coverage_summary, channel_ops, goroutine_spawns, field_writers, race_writes, unclosed_channels, unsafe_patterns, health_score, annotation_users, config_readers, event_emitters, pubsub, string_emitters, error_surface, log_events, sql_rebuild, external_calls, routes, models, components, k8s_resources, images, kustomize, cross_repo, impact, named)"), nil
	}
	switch kind {
	case "dead_code":
		return s.handleFindDeadCode(ctx, req)
	case "hotspots":
		return s.handleFindHotspots(ctx, req)
	case "cycles":
		return s.handleFindCycles(ctx, req)
	case "would_create_cycle":
		return s.handleWouldCreateCycle(ctx, req)
	case "todos":
		return s.handleAnalyzeTodos(ctx, req)
	case "blame":
		return s.handleAnalyzeBlame(ctx, req)
	case "coverage":
		return s.handleAnalyzeCoverage(ctx, req)
	case "stale_code":
		return s.handleAnalyzeStaleCode(ctx, req)
	case "ownership":
		return s.handleAnalyzeOwnership(ctx, req)
	case "coverage_gaps":
		return s.handleAnalyzeCoverageGaps(ctx, req)
	case "stale_flags":
		return s.handleAnalyzeStaleFlags(ctx, req)
	case "releases":
		return s.handleAnalyzeReleases(ctx, req)
	case "cgo_users":
		return s.handleAnalyzeInteropUsers(ctx, req, "uses_cgo", "cgo_users")
	case "wasm_users":
		return s.handleAnalyzeInteropUsers(ctx, req, "uses_wasm_bindgen", "wasm_users")
	case "orphan_tables":
		return s.handleAnalyzeOrphanTables(ctx, req)
	case "unreferenced_tables":
		return s.handleAnalyzeUnreferencedTables(ctx, req)
	case "coverage_summary":
		return s.handleAnalyzeCoverageSummary(ctx, req)
	case "channel_ops":
		return s.handleAnalyzeChannelOps(ctx, req)
	case "goroutine_spawns":
		return s.handleAnalyzeGoroutineSpawns(ctx, req)
	case "field_writers":
		return s.handleAnalyzeFieldWriters(ctx, req)
	case "race_writes":
		return s.handleAnalyzeRaceWrites(ctx, req)
	case "unclosed_channels":
		return s.handleAnalyzeUnclosedChannels(ctx, req)
	case "unsafe_patterns":
		return s.handleAnalyzeUnsafePatterns(ctx, req)
	case "sast", "hygiene":
		return s.handleAnalyzeSAST(ctx, req, kind)
	case "domain":
		return s.handleAnalyzeSAST(ctx, req, "domain")
	case "health_score":
		return s.handleAnalyzeHealthScore(ctx, req)
	case "annotation_users":
		return s.handleAnalyzeAnnotationUsers(ctx, req)
	case "config_readers":
		return s.handleAnalyzeConfigReaders(ctx, req)
	case "env_var_users":
		return s.handleAnalyzeEnvVarUsers(ctx, req)
	case "sql_call_sites":
		return s.handleAnalyzeSQLCallSites(ctx, req)
	case "fixes_history":
		return s.handleAnalyzeFixesHistory(ctx, req)
	case "edge_audit":
		return s.handleAnalyzeEdgeAudit(ctx, req)
	case "event_emitters":
		return s.handleAnalyzeEventEmitters(ctx, req)
	case "pubsub":
		return s.handleAnalyzePubsub(ctx, req)
	case "string_emitters":
		return s.handleAnalyzeStringEmitters(ctx, req)
	case "error_surface":
		return s.handleAnalyzeErrorSurface(ctx, req)
	case "log_events":
		return s.handleAnalyzeLogEvents(ctx, req)
	case "sql_rebuild":
		return s.handleAnalyzeSQLRebuild(ctx, req)
	case "external_calls":
		return s.handleAnalyzeExternalCalls(ctx, req)
	case "routes":
		return s.handleAnalyzeRoutes(ctx, req)
	case "models":
		return s.handleAnalyzeModels(ctx, req)
	case "components":
		return s.handleAnalyzeComponents(ctx, req)
	case "k8s_resources":
		return s.handleAnalyzeK8sResources(ctx, req)
	case "images":
		return s.handleAnalyzeImages(ctx, req)
	case "kustomize":
		return s.handleAnalyzeKustomize(ctx, req)
	case "cross_repo":
		return s.handleAnalyzeCrossRepo(ctx, req)
	case "dbt_models":
		return s.handleAnalyzeDbtModels(ctx, req)
	case "role":
		return s.handleAnalyzeRole(ctx, req)
	case "constructors_missing_fields":
		return s.handleAnalyzeConstructorsMissingFields(ctx, req)
	case "clusters":
		return s.handleAnalyzeClusters(ctx, req)
	case "concepts":
		return s.handleAnalyzeConcepts(ctx, req)
	case "impact":
		return s.handleAnalyzeImpactComposite(ctx, req)
	case "named":
		return s.handleAnalyzeNamed(ctx, req)
	default:
		return mcp.NewToolResultError("unknown analyze kind: " + kind + " (expected: dead_code, hotspots, cycles, would_create_cycle, todos, blame, coverage, stale_code, ownership, coverage_gaps, stale_flags, releases, cgo_users, wasm_users, orphan_tables, unreferenced_tables, coverage_summary, channel_ops, goroutine_spawns, field_writers, race_writes, unclosed_channels, unsafe_patterns, sast, hygiene, health_score, annotation_users, config_readers, env_var_users, sql_call_sites, fixes_history, edge_audit, domain, event_emitters, pubsub, string_emitters, error_surface, log_events, sql_rebuild, external_calls, routes, models, components, k8s_resources, images, kustomize, cross_repo, dbt_models, impact, named)"), nil
	}
}

// ---------------------------------------------------------------------------
// handleAnalyzeTodos — list KindTodo nodes with filters
// ---------------------------------------------------------------------------

// handleAnalyzeTodos enumerates the KindTodo nodes in the graph,
// optionally filtering by tag (TODO/FIXME/HACK/XXX/NOTE), assignee,
// or ticket. Designed for the cleanup-loop workflow: find every
// TODO assigned to me, every FIXME without a ticket, every TODO
// older than the v1.4 release, etc. The temporal filter is left
// for a v2 refinement that consumes git-blame enrichment.
//
// Returns one row per matching todo with file, line, tag,
// assignee, due, ticket, and the truncated text.
func (s *Server) handleAnalyzeTodos(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	tagFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "tag")))
	assigneeFilter := strings.TrimSpace(stringArg(args, "assignee"))
	ticketFilter := strings.TrimSpace(stringArg(args, "ticket"))
	requireAssignee, _ := args["has_assignee"].(bool)

	type todoRow struct {
		ID       string `json:"id"`
		Tag      string `json:"tag"`
		File     string `json:"file"`
		Line     int    `json:"line"`
		Assignee string `json:"assignee,omitempty"`
		Due      string `json:"due,omitempty"`
		Ticket   string `json:"ticket,omitempty"`
		Text     string `json:"text,omitempty"`
	}

	var rows []todoRow
	for _, n := range s.scopedNodes(ctx) {
		if n.Kind != graph.KindTodo {
			continue
		}
		tag, _ := n.Meta["tag"].(string)
		assignee, _ := n.Meta["assignee"].(string)
		ticket, _ := n.Meta["ticket"].(string)
		due, _ := n.Meta["due"].(string)
		text, _ := n.Meta["text"].(string)

		if tagFilter != "" && strings.ToLower(tag) != tagFilter {
			continue
		}
		if assigneeFilter != "" && assignee != assigneeFilter {
			continue
		}
		if ticketFilter != "" && ticket != ticketFilter {
			continue
		}
		if requireAssignee && assignee == "" {
			continue
		}
		rows = append(rows, todoRow{
			ID:       n.ID,
			Tag:      tag,
			File:     n.FilePath,
			Line:     n.StartLine,
			Assignee: assignee,
			Due:      due,
			Ticket:   ticket,
			Text:     text,
		})
	}
	// Stable order: file then line. Predictable diffs across calls
	// matter for cleanup workflows that compare results over time.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s %s:%d", r.Tag, r.File, r.Line)
			if r.Assignee != "" {
				fmt.Fprintf(&b, " @%s", r.Assignee)
			}
			if r.Ticket != "" {
				fmt.Fprintf(&b, " %s", r.Ticket)
			}
			if r.Text != "" {
				fmt.Fprintf(&b, " — %s", r.Text)
			}
			b.WriteByte('\n')
		}
		if len(rows) == 0 {
			b.WriteString("no todos matched\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"todos": rows,
		"total": len(rows),
	})
}

// stringArg returns args[key] as a trimmed string, or "" when the
// key is missing or the value isn't a string.
func stringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// handleAnalyzeCoverage parses a Go cover profile and stamps
// meta.coverage_pct + meta.coverage on every executable symbol it
// can map to a profile segment by line range. Requires a `profile`
// argument with the path to the cover.out file (relative paths
// resolve against the indexed repo root).
//
// Re-runnable: each call re-reads the profile and overwrites
// existing meta — the desired behaviour after a fresh test run.
func (s *Server) handleAnalyzeCoverage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	profileArg := stringArg(req.GetArguments(), "profile")
	if profileArg == "" {
		return mcp.NewToolResultError("coverage enrichment requires a `profile` argument with the cover.out path"), nil
	}
	if s.indexer == nil {
		return mcp.NewToolResultError("coverage enrichment requires an active indexer"), nil
	}
	root := s.indexer.RootPath()
	if !filepath.IsAbs(profileArg) {
		profileArg = filepath.Join(root, profileArg)
	}
	segments, err := coverage.ParseFile(profileArg)
	if err != nil {
		return mcp.NewToolResultError("read profile: " + err.Error()), nil
	}
	modulePath := coverage.ReadModulePath(root)
	count := coverage.EnrichGraph(s.graph, segments, modulePath)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"enriched":    count,
		"segments":    len(segments),
		"profile":     profileArg,
		"module_path": modulePath,
	})
}

// handleAnalyzeStaleCode lists symbols whose meta.last_authored is
// older than the threshold. Requires that blame enrichment has
// already run (either through analyze kind=blame or `gortex enrich
// blame`); symbols without authorship metadata are silently
// skipped — they're either unenriched or hand-authored without git
// history (test fixtures, generated code), and lumping them in
// with "unchanged for ages" would be a lie.
//
// Filters:
//
//   - older_than: days, default 365. Symbols with a last-author
//     timestamp older than now - older_than days are included.
//   - email: exact author email match — useful for "find code
//     authored by someone who has left the team."
//   - kinds: comma-separated list, default function,method. Pass
//     "all" to include every blame-eligible kind.
//
// Sorted oldest-first so the cleanup loop sees the staleness
// gradient at a glance.
func (s *Server) handleAnalyzeStaleCode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	olderThanDays := 365.0
	if v, ok := args["older_than"].(float64); ok && v > 0 {
		olderThanDays = v
	}
	emailFilter := strings.TrimSpace(stringArg(args, "email"))

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	cutoffSec := time.Now().Add(-time.Duration(olderThanDays*24) * time.Hour).Unix()

	type staleRow struct {
		ID        string `json:"id"`
		File      string `json:"file"`
		Line      int    `json:"line"`
		Email     string `json:"email"`
		Commit    string `json:"commit"`
		Timestamp int64  `json:"timestamp"`
		AgeDays   int    `json:"age_days"`
	}
	var rows []staleRow
	for _, n := range s.scopedNodes(ctx) {
		if _, ok := allowedKinds[n.Kind]; !ok {
			continue
		}
		la, ok := n.Meta["last_authored"].(map[string]any)
		if !ok {
			continue
		}
		ts, ok := la["timestamp"].(int64)
		if !ok {
			// JSON unmarshal lands ints as float64 in some paths;
			// accept both shapes so the analyzer works on graphs
			// loaded from snapshots and graphs enriched in-process.
			if f, isFloat := la["timestamp"].(float64); isFloat {
				ts = int64(f)
			} else {
				continue
			}
		}
		if ts > cutoffSec {
			continue
		}
		email, _ := la["email"].(string)
		if emailFilter != "" && email != emailFilter {
			continue
		}
		commit, _ := la["commit"].(string)
		ageSec := time.Now().Unix() - ts
		rows = append(rows, staleRow{
			ID:        n.ID,
			File:      n.FilePath,
			Line:      n.StartLine,
			Email:     email,
			Commit:    commit,
			Timestamp: ts,
			AgeDays:   int(ageSec / (24 * 3600)),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Timestamp < rows[j].Timestamp
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%dd %s:%d", r.AgeDays, r.File, r.Line)
			if r.Email != "" {
				fmt.Fprintf(&b, " @%s", r.Email)
			}
			fmt.Fprintf(&b, " %s\n", r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no stale code matched\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"stale":          rows,
		"total":          len(rows),
		"older_than_day": olderThanDays,
	})
}

// parseAnalyzeKindsFilter parses a comma-separated kinds argument
// into the set used by handleAnalyzeStaleCode. The literal "all"
// returns the broadest blame-eligible kind set so callers can drop
// the default function/method scope when they want types and
// fields included too.
func parseAnalyzeKindsFilter(arg string) map[graph.NodeKind]struct{} {
	out := map[graph.NodeKind]struct{}{}
	for _, k := range strings.Split(arg, ",") {
		k = strings.TrimSpace(strings.ToLower(k))
		if k == "" {
			continue
		}
		if k == "all" {
			return map[graph.NodeKind]struct{}{
				graph.KindFunction:   {},
				graph.KindMethod:     {},
				graph.KindType:       {},
				graph.KindInterface:  {},
				graph.KindField:      {},
				graph.KindVariable:   {},
				graph.KindConstant:   {},
				graph.KindEnumMember: {},
			}
		}
		out[graph.NodeKind(k)] = struct{}{}
	}
	return out
}

// handleAnalyzeOwnership groups blame metadata by author email and
// returns one row per author with the symbol count, files
// touched, and the oldest/newest last-authored timestamp seen.
// Requires a blame-enriched graph (analyze kind=blame or `gortex
// enrich blame`) — symbols without authorship metadata are
// silently skipped, same as handleAnalyzeStaleCode.
//
// Filters:
//
//   - min_symbols: drop authors below this symbol count (default 1).
//     Useful for excluding drive-by contributions on large repos.
//   - kinds: comma-separated kind list, default function,method.
//     Pass "all" to include every blame-eligible kind.
//   - path_prefix: scope to nodes under this file-path prefix —
//     e.g. "internal/auth/" to ask "who owns the auth package".
//
// Sorted descending by symbol count so the top owners appear
// first. The combination (path_prefix + min_symbols + sorted
// output) is the cleanup-loop's "who do I ping for review on
// this area" query without needing a CODEOWNERS file.
func (s *Server) handleAnalyzeOwnership(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	minSymbols := 1
	if v, ok := args["min_symbols"].(float64); ok && v > 0 {
		minSymbols = int(v)
	}
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	type ownerStats struct {
		Email    string `json:"email"`
		Symbols  int    `json:"symbols"`
		Files    int    `json:"files"`
		OldestTS int64  `json:"oldest_timestamp"`
		NewestTS int64  `json:"newest_timestamp"`
		fileSet  map[string]struct{}
	}
	byEmail := map[string]*ownerStats{}

	for _, n := range s.scopedNodes(ctx) {
		if _, ok := allowedKinds[n.Kind]; !ok {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		la, ok := n.Meta["last_authored"].(map[string]any)
		if !ok {
			continue
		}
		email, _ := la["email"].(string)
		if email == "" {
			continue
		}
		ts := tsFromMeta(la["timestamp"])
		if ts == 0 {
			continue
		}
		stats, ok := byEmail[email]
		if !ok {
			stats = &ownerStats{
				Email:    email,
				OldestTS: ts,
				NewestTS: ts,
				fileSet:  map[string]struct{}{},
			}
			byEmail[email] = stats
		}
		stats.Symbols++
		stats.fileSet[n.FilePath] = struct{}{}
		if ts < stats.OldestTS {
			stats.OldestTS = ts
		}
		if ts > stats.NewestTS {
			stats.NewestTS = ts
		}
	}

	rows := make([]*ownerStats, 0, len(byEmail))
	for _, s := range byEmail {
		s.Files = len(s.fileSet)
		s.fileSet = nil // hide from JSON output
		if s.Symbols < minSymbols {
			continue
		}
		rows = append(rows, s)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Symbols != rows[j].Symbols {
			return rows[i].Symbols > rows[j].Symbols
		}
		return rows[i].Email < rows[j].Email
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-5d %-3d %s\n", r.Symbols, r.Files, r.Email)
		}
		if len(rows) == 0 {
			b.WriteString("no owners matched\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"owners": rows,
		"total":  len(rows),
	})
}

// tsFromMeta normalises the timestamp field across the int64
// (in-process enrichment) and float64 (gob-decoded snapshot)
// shapes. Returns 0 when the value is missing or the wrong type
// — callers treat 0 as "skip this node" since blame timestamps
// are always positive.
func tsFromMeta(raw any) int64 {
	switch v := raw.(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

// handleAnalyzeCoverageGaps lists symbols whose meta.coverage_pct
// falls inside [min_pct, max_pct) — the half-open interval lets
// "everything below 50%" be expressed as max_pct=50 without
// dragging in fully-uncovered nodes that callers might want to
// distinguish via a separate query. Requires a coverage-enriched
// graph (analyze kind=coverage or `gortex enrich coverage`).
//
// Symbols without coverage_pct are silently skipped — a node
// could be unmeasured because the profile didn't cover it (real
// gap) or because it's a non-executable kind (no signal at all).
// Lumping the two together would be misleading, and the
// distinction lives in the static dead_code analyzer rather than
// here.
//
// Filters:
//
//   - max_pct: upper exclusive bound (default 100 — i.e. anything
//     not fully covered).
//   - min_pct: lower inclusive bound (default 0 — i.e. include
//     fully-uncovered too). Combine with max_pct to scope to a
//     coverage band: "20-50% coverage" is min_pct=20 max_pct=50.
//   - kinds: same shared kind filter as stale_code/ownership;
//     default function/method.
//   - path_prefix: scope to a directory subtree.
//
// Sorted ascending by coverage_pct so the most-undertested
// symbols surface first.
func (s *Server) handleAnalyzeCoverageGaps(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	maxPct := 100.0
	if v, ok := args["max_pct"].(float64); ok && v > 0 {
		maxPct = v
	}
	minPct := 0.0
	if v, ok := args["min_pct"].(float64); ok && v >= 0 {
		minPct = v
	}
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	type gapRow struct {
		ID      string  `json:"id"`
		File    string  `json:"file"`
		Line    int     `json:"line"`
		Pct     float64 `json:"coverage_pct"`
		NumStmt int     `json:"num_stmt"`
		Hit     int     `json:"hit"`
	}
	var rows []gapRow
	for _, n := range s.scopedNodes(ctx) {
		if _, ok := allowedKinds[n.Kind]; !ok {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		pct, ok := n.Meta["coverage_pct"].(float64)
		if !ok {
			continue
		}
		if pct < minPct || pct >= maxPct {
			continue
		}
		row := gapRow{
			ID:   n.ID,
			File: n.FilePath,
			Line: n.StartLine,
			Pct:  pct,
		}
		if cov, ok := n.Meta["coverage"].(map[string]any); ok {
			if v, ok := cov["num_stmt"].(int); ok {
				row.NumStmt = v
			} else if f, ok := cov["num_stmt"].(float64); ok {
				row.NumStmt = int(f)
			}
			if v, ok := cov["hit"].(int); ok {
				row.Hit = v
			} else if f, ok := cov["hit"].(float64); ok {
				row.Hit = int(f)
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Pct != rows[j].Pct {
			return rows[i].Pct < rows[j].Pct
		}
		// Tie-break by symbol size — bigger gaps surface above
		// smaller ones at the same percentage. NumStmt may be 0
		// when meta.coverage didn't decode cleanly; the secondary
		// fallback to file:line keeps the order stable.
		if rows[i].NumStmt != rows[j].NumStmt {
			return rows[i].NumStmt > rows[j].NumStmt
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%5.1f%% %s:%d  %s\n", r.Pct, r.File, r.Line, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no coverage gaps matched\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"gaps":    rows,
		"total":   len(rows),
		"min_pct": minPct,
		"max_pct": maxPct,
	})
}

// handleAnalyzeStaleFlags lists feature flags whose every toggling
// call site was last touched more than `older_than` days ago. The
// staleness signal is derived: for each KindFlag node we walk its
// incoming EdgeTogglesFlag edges, look up each caller's
// meta.last_authored.timestamp, and take the maximum. If even the
// most-recently-touched check site is older than the cutoff, the
// flag is stale — every check is in code nobody's edited in a
// while, which is the operational signal that the rollout is
// done.
//
// Requires both flag detection (analyze kind=blame is enough to
// populate KindFlag nodes if the repo enables index.coverage.flags)
// AND blame enrichment (analyze kind=blame). Flags whose callers
// don't have blame metadata are silently skipped — without
// authorship data we can't compute the staleness — and reported
// in the response's `unscored` count so the agent can tell the
// difference between "no flags found" and "flags found but
// unscored."
//
// Filters:
//
//   - older_than: days, default 365.
//   - provider: filter to a single provider (launchdarkly,
//     growthbook, unleash, internal).
//
// Sorted oldest-first so cleanup priorities surface at the top.
func (s *Server) handleAnalyzeStaleFlags(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	olderThanDays := 365.0
	if v, ok := args["older_than"].(float64); ok && v > 0 {
		olderThanDays = v
	}
	providerFilter := strings.TrimSpace(stringArg(args, "provider"))
	cutoffSec := time.Now().Add(-time.Duration(olderThanDays*24) * time.Hour).Unix()

	type staleFlag struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Provider     string `json:"provider"`
		Callers      int    `json:"callers"`
		NewestCallTS int64  `json:"newest_call_timestamp"`
		AgeDays      int    `json:"age_days"`
	}
	var rows []staleFlag
	unscored := 0

	for _, n := range s.scopedNodes(ctx) {
		if n.Kind != graph.KindFlag {
			continue
		}
		provider, _ := n.Meta["provider"].(string)
		if providerFilter != "" && provider != providerFilter {
			continue
		}
		// Walk incoming EdgeTogglesFlag edges to collect callers.
		var callerIDs []string
		for _, e := range s.graph.GetInEdges(n.ID) {
			if e.Kind != graph.EdgeTogglesFlag {
				continue
			}
			callerIDs = append(callerIDs, e.From)
		}
		if len(callerIDs) == 0 {
			// Orphan flag — declared but never checked. Treat as
			// stale: a flag with zero call sites is tautologically
			// safe to delete.
			rows = append(rows, staleFlag{
				ID:       n.ID,
				Name:     stringFromMeta(n.Meta, "name"),
				Provider: provider,
				Callers:  0,
				AgeDays:  -1,
			})
			continue
		}
		var newestTS int64
		hasBlame := false
		for _, callerID := range callerIDs {
			caller := s.graph.GetNode(callerID)
			if caller == nil {
				continue
			}
			la, ok := caller.Meta["last_authored"].(map[string]any)
			if !ok {
				continue
			}
			ts := tsFromMeta(la["timestamp"])
			if ts == 0 {
				continue
			}
			hasBlame = true
			if ts > newestTS {
				newestTS = ts
			}
		}
		if !hasBlame {
			unscored++
			continue
		}
		if newestTS > cutoffSec {
			continue // some caller is fresh
		}
		ageSec := time.Now().Unix() - newestTS
		rows = append(rows, staleFlag{
			ID:           n.ID,
			Name:         stringFromMeta(n.Meta, "name"),
			Provider:     provider,
			Callers:      len(callerIDs),
			NewestCallTS: newestTS,
			AgeDays:      int(ageSec / (24 * 3600)),
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		// Orphans (AgeDays = -1) first, then oldest by timestamp.
		if rows[i].AgeDays < 0 && rows[j].AgeDays >= 0 {
			return true
		}
		if rows[j].AgeDays < 0 && rows[i].AgeDays >= 0 {
			return false
		}
		return rows[i].NewestCallTS < rows[j].NewestCallTS
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			if r.AgeDays < 0 {
				fmt.Fprintf(&b, "ORPHAN  %s (%s)\n", r.Name, r.Provider)
				continue
			}
			fmt.Fprintf(&b, "%4dd  %s (%s) — %d callers\n", r.AgeDays, r.Name, r.Provider, r.Callers)
		}
		if len(rows) == 0 {
			b.WriteString("no stale flags matched\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"flags":          rows,
		"total":          len(rows),
		"unscored":       unscored,
		"older_than_day": olderThanDays,
	})
}

// stringFromMeta is a tiny helper for safe meta string extraction.
func stringFromMeta(meta map[string]any, key string) string {
	if v, ok := meta[key].(string); ok {
		return v
	}
	return ""
}

// handleAnalyzeOrphanTables lists tables that are referenced by
// at least one EdgeQueries call site but have no incoming
// EdgeProvides from a migration. Combines the two SQL extraction
// paths (query-string detection + migration-file declaration)
// into a single signal: tables likely missing a migration, or
// pointing at an external/legacy schema the agent should flag.
//
// Returns one row per orphan with the canonical id, table name,
// schema, dialect, and the count of EdgeQueries call sites
// pointing at it. Sorted by query count descending so the most-
// used orphans surface first — those are the highest-priority
// "we should declare this" candidates.
//
// Tables reachable via both EdgeProvides AND EdgeQueries are not
// orphans by definition. Tables with no EdgeQueries either (pure
// declaration with no users) aren't included either — they're
// the inverse problem ("orphan migration") which is a separate
// future analyzer.
func (s *Server) handleAnalyzeOrphanTables(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type orphanRow struct {
		ID         string `json:"id"`
		Table      string `json:"table"`
		Schema     string `json:"schema,omitempty"`
		Dialect    string `json:"dialect"`
		QueryCount int    `json:"query_count"`
	}
	var rows []orphanRow
	for _, n := range s.scopedNodes(ctx) {
		if n.Kind != graph.KindTable {
			continue
		}
		// Walk incoming edges to detect both providers (migrations)
		// and consumers (query call sites).
		hasProvider := false
		queryCount := 0
		for _, e := range s.graph.GetInEdges(n.ID) {
			switch e.Kind {
			case graph.EdgeProvides:
				hasProvider = true
			case graph.EdgeQueries:
				queryCount++
			}
		}
		if hasProvider {
			continue
		}
		if queryCount == 0 {
			continue
		}
		dialect, _ := n.Meta["dialect"].(string)
		schema, _ := n.Meta["schema"].(string)
		table, _ := n.Meta["table"].(string)
		if table == "" {
			table = n.Name
		}
		rows = append(rows, orphanRow{
			ID:         n.ID,
			Table:      table,
			Schema:     schema,
			Dialect:    dialect,
			QueryCount: queryCount,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].QueryCount != rows[j].QueryCount {
			return rows[i].QueryCount > rows[j].QueryCount
		}
		return rows[i].ID < rows[j].ID
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d  %s\n", r.QueryCount, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no orphan tables\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"orphans": rows,
		"total":   len(rows),
	})
}

// handleAnalyzeUnreferencedTables is the inverse of
// orphan_tables: tables that have an incoming EdgeProvides from
// a migration but zero EdgeQueries call sites. Useful for
// "which migrations created tables we don't read or write" —
// dead schema candidates, cleanup signals after a feature
// removal, or tables that exist only for downstream replication.
//
// Returns one row per unreferenced table with the canonical id,
// table name, schema, dialect, and the count of providers
// (typically 1, but a table can appear in multiple migrations).
// Sorted alphabetically by id for diff-able output — there's no
// natural priority ordering for this list the way query_count
// gives orphan_tables.
func (s *Server) handleAnalyzeUnreferencedTables(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type unrefRow struct {
		ID            string `json:"id"`
		Table         string `json:"table"`
		Schema        string `json:"schema,omitempty"`
		Dialect       string `json:"dialect"`
		ProviderCount int    `json:"provider_count"`
	}
	var rows []unrefRow
	for _, n := range s.scopedNodes(ctx) {
		if n.Kind != graph.KindTable {
			continue
		}
		providerCount := 0
		queryCount := 0
		for _, e := range s.graph.GetInEdges(n.ID) {
			switch e.Kind {
			case graph.EdgeProvides:
				providerCount++
			case graph.EdgeQueries:
				queryCount++
			}
		}
		if providerCount == 0 || queryCount > 0 {
			continue
		}
		dialect, _ := n.Meta["dialect"].(string)
		schema, _ := n.Meta["schema"].(string)
		table, _ := n.Meta["table"].(string)
		if table == "" {
			table = n.Name
		}
		rows = append(rows, unrefRow{
			ID:            n.ID,
			Table:         table,
			Schema:        schema,
			Dialect:       dialect,
			ProviderCount: providerCount,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].ID < rows[j].ID
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintln(&b, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no unreferenced tables\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"unreferenced": rows,
		"total":        len(rows),
	})
}

// handleAnalyzeCoverageSummary aggregates meta.coverage_pct per
// directory. Complements coverage_gaps (per-symbol view) with a
// package-level rollup useful for cleanup planning ("which
// directory needs the most test attention"). Each row carries
// the directory path, total measured symbols, average coverage,
// fully-covered count, fully-uncovered count, and partial count
// — the breakdown helps distinguish "package needs more
// branches tested" from "package has no tests at all".
//
// Sorted ascending by avg_pct so worst packages surface first.
// Filters mirror coverage_gaps: kinds (default function/method),
// path_prefix scoping.
func (s *Server) handleAnalyzeCoverageSummary(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	type dirStats struct {
		Dir       string  `json:"dir"`
		Symbols   int     `json:"symbols"`
		AvgPct    float64 `json:"avg_pct"`
		Covered   int     `json:"covered"`
		Partial   int     `json:"partial"`
		Uncovered int     `json:"uncovered"`

		sumPct float64 // running sum, hidden from JSON
	}
	byDir := map[string]*dirStats{}

	for _, n := range s.scopedNodes(ctx) {
		if _, ok := allowedKinds[n.Kind]; !ok {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		pct, ok := n.Meta["coverage_pct"].(float64)
		if !ok {
			continue
		}
		dir := filepath.Dir(n.FilePath)
		ds, ok := byDir[dir]
		if !ok {
			ds = &dirStats{Dir: dir}
			byDir[dir] = ds
		}
		ds.Symbols++
		ds.sumPct += pct
		switch {
		case pct >= 100:
			ds.Covered++
		case pct == 0:
			ds.Uncovered++
		default:
			ds.Partial++
		}
	}

	rows := make([]*dirStats, 0, len(byDir))
	for _, s := range byDir {
		if s.Symbols == 0 {
			continue
		}
		s.AvgPct = roundTwoDecimal(s.sumPct / float64(s.Symbols))
		s.sumPct = 0
		rows = append(rows, s)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AvgPct != rows[j].AvgPct {
			return rows[i].AvgPct < rows[j].AvgPct
		}
		return rows[i].Dir < rows[j].Dir
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%5.1f%% %3d sym (%d cov / %d part / %d unc)  %s\n",
				r.AvgPct, r.Symbols, r.Covered, r.Partial, r.Uncovered, r.Dir)
		}
		if len(rows) == 0 {
			b.WriteString("no coverage data\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"directories": rows,
		"total":       len(rows),
	})
}

// roundTwoDecimal rounds to 2 decimal places, mirroring the
// coverage package's roundTwo. Local helper rather than an import
// dependency on internal/coverage so the mcp tool stays
// self-contained.
func roundTwoDecimal(v float64) float64 {
	if v < 0 {
		return v
	}
	return float64(int64(v*100+0.5)) / 100
}

// handleAnalyzeInteropUsers lists every file with the named
// cross-language interop meta flag set. Currently used for two
// sentinels: meta.uses_cgo (Go files that `import "C"`) and
// meta.uses_wasm_bindgen (Rust files with `#[wasm_bindgen]`).
// Each routes through the same handler with a different
// metaKey + resultKey pair — adding future interop kinds
// (jni, napi, ffi-style imports) is one switch case in the
// dispatcher.
//
// Useful for porting surveys ("how much surface uses cgo?"),
// CI gate questions ("did this PR add a new wasm-bindgen
// boundary?"), and non-interop build planning. Files are
// reported in path order so the result is diff-able across
// runs.
func (s *Server) handleAnalyzeInteropUsers(ctx context.Context, req mcp.CallToolRequest, metaKey, resultKey string) (*mcp.CallToolResult, error) {
	type interopFile struct {
		File string `json:"file"`
		ID   string `json:"id"`
	}
	var rows []interopFile
	for _, n := range s.scopedNodes(ctx) {
		if n.Kind != graph.KindFile {
			continue
		}
		if v, _ := n.Meta[metaKey].(bool); !v {
			continue
		}
		rows = append(rows, interopFile{
			File: n.FilePath,
			ID:   n.ID,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].File < rows[j].File
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			b.WriteString(r.File)
			b.WriteByte('\n')
		}
		if len(rows) == 0 {
			fmt.Fprintf(&b, "no %s\n", resultKey)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		resultKey: rows,
		"total":   len(rows),
	})
}

// handleAnalyzeReleases walks git tags chronologically and stamps
// meta.added_in on every file node with the earliest tag whose
// tree contained that file. Symbols inherit indirectly via their
// owning file — answers "added in v1.4?" with one graph hop from
// any symbol to its file. Re-runnable: each call re-walks tags
// and overwrites existing meta.
func (s *Server) handleAnalyzeReleases(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	roots := s.collectRepoRoots(req.GetString("repo", ""))
	if len(roots) == 0 {
		return mcp.NewToolResultError("releases enrichment requires at least one indexed repo with a root path"), nil
	}
	total := 0
	perRepo := make(map[string]any, len(roots))
	for prefix, root := range roots {
		count, err := releases.EnrichGraphWithRepoPrefix(s.graph, root, prefix)
		if err != nil {
			perRepo[prefix] = map[string]any{"root": root, "error": err.Error()}
			continue
		}
		total += count
		perRepo[prefix] = map[string]any{"root": root, "enriched": count}
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"enriched": total,
		"per_repo": perRepo,
	})
}

// handleAnalyzeBlame runs `git blame -p` against the indexed
// repository and stamps meta.last_authored on each function /
// method / type / interface / field / variable / constant /
// enum_member node it can map to a real source line. Returns the
// number of nodes enriched.
//
// Blocking — large repos can take seconds — but explicit (the
// agent invoked it). Repeat invocations re-run blame and overwrite
// existing meta.last_authored, which is the desired behaviour for
// post-commit refresh.
func (s *Server) handleAnalyzeBlame(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	roots := s.collectRepoRoots(req.GetString("repo", ""))
	if len(roots) == 0 {
		return mcp.NewToolResultError("blame enrichment requires at least one indexed repo with a root path"), nil
	}
	total := 0
	perRepo := make(map[string]any, len(roots))
	for prefix, root := range roots {
		count, err := blame.EnrichGraph(s.graph, root)
		if err != nil {
			perRepo[prefix] = map[string]any{"root": root, "error": err.Error()}
			continue
		}
		total += count
		perRepo[prefix] = map[string]any{"root": root, "enriched": count}
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"enriched": total,
		"per_repo": perRepo,
	})
}

// collectRepoRoots returns the set of repo prefix → root paths to enrich.
// In multi-repo mode iterates every tracked repo (or just the one matching
// `scope` when set). In single-repo mode returns the lone indexer's root
// keyed by an empty prefix. Empty roots are skipped so callers don't have
// to filter them downstream.
func (s *Server) collectRepoRoots(scope string) map[string]string {
	out := make(map[string]string)
	if s.multiIndexer != nil {
		if scope != "" {
			if root, ok := s.multiIndexer.RepoRoot(scope); ok {
				out[scope] = root
			}
			return out
		}
		for prefix, meta := range s.multiIndexer.AllMetadata() {
			if meta == nil || meta.RootPath == "" {
				continue
			}
			out[prefix] = meta.RootPath
		}
		return out
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			out[s.indexer.RepoPrefix()] = root
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 10.5 handleFindDeadCode and handleFindHotspots
// ---------------------------------------------------------------------------

func (s *Server) handleFindDeadCode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	opts := analysis.FindDeadCodeOptions{}
	args := req.GetArguments()
	if v, ok := args["include_variables"].(bool); ok && v {
		opts.IncludeVariables = true
	}
	if v, ok := args["include_fields"].(bool); ok && v {
		opts.IncludeFields = true
	}
	if v, ok := args["include_constants"].(bool); ok && v {
		opts.IncludeConstants = true
	}
	if v, ok := args["include_cgo_exports"].(bool); ok && v {
		opts.IncludeCgoExports = true
	}
	if v, ok := args["include_linkname_targets"].(bool); ok && v {
		opts.IncludeLinknameTargets = true
	}
	if v, ok := args["skip_cross_repo_nodes"].(bool); ok && v {
		opts.SkipCrossRepoNodes = true
	}

	entries := analysis.FindDeadCode(s.graph, s.getProcesses(), nil, opts)

	// Cap response size — large repos surface thousands of dead-code
	// candidates and the default JSON encoding spills past the MCP
	// per-response token cap. Callers that need the full list can
	// raise the limit explicitly.
	limit := 200
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	totalEntries := len(entries)
	deadTruncated := false
	if len(entries) > limit {
		entries = entries[:limit]
		deadTruncated = true
	}

	variablesNote := buildDeadCodeNote(opts)

	if s.isGCX(ctx, req) {
		items := make([]deadCodeItem, 0, len(entries))
		for _, e := range entries {
			items = append(items, deadCodeItem{
				ID:   e.ID,
				Kind: e.Kind,
				Name: e.Name,
				Path: e.FilePath,
				Line: e.Line,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("dead_code", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, e := range entries {
			fmt.Fprintf(&b, "%s %s %s:%d\n", e.Kind, e.ID, e.FilePath, e.Line)
		}
		if len(entries) == 0 {
			b.WriteString("no dead code found\n")
		}
		if variablesNote != "" {
			fmt.Fprintf(&b, "\nnote: %s\n", variablesNote)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	result := map[string]any{
		"dead_code": entries,
		"total":     totalEntries,
		"truncated": deadTruncated,
	}
	if deadTruncated {
		result["limit"] = limit
	}
	if variablesNote != "" {
		result["note"] = variablesNote
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// buildDeadCodeNote summarises which low-signal kinds the analyzer
// dropped by default, so callers know which include_* flag to flip
// if they want to broaden the scan. Returns the empty string when
// every opt-in flag is already set.
func buildDeadCodeNote(opts analysis.FindDeadCodeOptions) string {
	var off []string
	if !opts.IncludeVariables {
		off = append(off, "variables (include_variables=true)")
	}
	if !opts.IncludeFields {
		off = append(off, "fields (include_fields=true)")
	}
	if !opts.IncludeConstants {
		off = append(off, "constants (include_constants=true)")
	}
	if len(off) == 0 {
		return ""
	}
	return "Excluded by default — graph lacks intra-function data flow, so these always look dead: " + strings.Join(off, ", ") + "."
}

func (s *Server) handleFindHotspots(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Check minimum graph size
	if s.graph.NodeCount() < 10 {
		return mcp.NewToolResultError("codebase too small for meaningful hotspot analysis (need at least 10 symbols)"), nil
	}

	threshold := 0.0
	if v, ok := req.GetArguments()["threshold"].(float64); ok {
		threshold = v
	}

	entries := analysis.FindHotspots(s.graph, s.getCommunities(), threshold)

	// K17: optional novelty / directional reranking modes. Default
	// "complexity" preserves the legacy ranking.
	mode := strings.TrimSpace(stringArg(req.GetArguments(), "mode"))
	if mode == "" {
		mode = "complexity"
	}
	if mode != "complexity" {
		windowDays := 30
		if v, ok := req.GetArguments()["window_days"].(float64); ok && v > 0 {
			windowDays = int(v)
		}
		direction := strings.TrimSpace(stringArg(req.GetArguments(), "direction"))
		if direction == "" {
			direction = "adds"
		}
		entries = rerankHotspots(entries, s.graph, mode, direction, windowDays)
	}

	// Truncate to top 20
	totalCount := len(entries)
	truncated := false
	if len(entries) > 20 {
		entries = entries[:20]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		items := make([]hotspotItem, 0, len(entries))
		for _, e := range entries {
			items = append(items, hotspotItem{
				ID:             e.ID,
				Name:           e.Name,
				Path:           e.FilePath,
				Line:           e.Line,
				FanIn:          e.FanIn,
				FanOut:         e.FanOut,
				CrossCommunity: e.CommunityCrossings,
				Score:          e.ComplexityScore,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("hotspots", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, e := range entries {
			fmt.Fprintf(&b, "%s %s %s:%d score=%.1f fan_in=%d fan_out=%d crossings=%d\n",
				e.Kind, e.ID, e.FilePath, e.Line, e.ComplexityScore, e.FanIn, e.FanOut, e.CommunityCrossings)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total)\n", totalCount)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"hotspots":  entries,
		"total":     totalCount,
		"truncated": truncated,
	})
}

// ---------------------------------------------------------------------------
// 10.6 handleScaffold
// ---------------------------------------------------------------------------

// scaffoldReader bridges Server to analysis.SourceReader so scaffolding can
// resolve file paths through the multi-repo aware Server.resolveGraphPath
// instead of relying on a single Indexer.RootPath which is empty in
// multi-repo mode.
type scaffoldReader struct{ s *Server }

func (r scaffoldReader) Graph() *graph.Graph { return r.s.graph }
func (r scaffoldReader) ResolveFilePath(graphPath string) string {
	abs, err := r.s.resolveGraphPath(graphPath)
	if err != nil {
		return ""
	}
	return abs
}

func (s *Server) handleScaffold(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	exampleID, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	newName, err := req.RequireString("new_name")
	if err != nil {
		return mcp.NewToolResultError("new_name is required"), nil
	}

	// dry_run defaults to true (scaffold never writes by default)
	dryRun := true
	if v, ok := req.GetArguments()["dry_run"].(bool); ok {
		dryRun = v
	}

	result, err := analysis.GenerateScaffold(s.engine, scaffoldReader{s}, exampleID, newName)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	resp := map[string]any{
		"edits":   result.Edits,
		"notes":   result.Notes,
		"dry_run": dryRun,
	}

	if !dryRun && s.indexer != nil {
		// Apply edits by writing files
		for _, edit := range result.Edits {
			absPath := edit.FilePath
			if root := s.indexer.RootPath(); root != "" {
				absPath = filepath.Join(root, edit.FilePath)
			}
			content, readErr := os.ReadFile(absPath)
			if readErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("could not read %s: %v", edit.FilePath, readErr)), nil
			}
			lines := strings.Split(string(content), "\n")
			insertIdx := edit.InsertionLine - 1
			if insertIdx < 0 {
				insertIdx = 0
			}
			if insertIdx > len(lines) {
				insertIdx = len(lines)
			}
			newLines := make([]string, 0, len(lines)+strings.Count(edit.Code, "\n")+2)
			newLines = append(newLines, lines[:insertIdx]...)
			newLines = append(newLines, "")
			newLines = append(newLines, edit.Code)
			newLines = append(newLines, lines[insertIdx:]...)
			if writeErr := os.WriteFile(absPath, []byte(strings.Join(newLines, "\n")), 0o644); writeErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("could not write %s: %v", edit.FilePath, writeErr)), nil
			}
		}
		resp["applied"] = true
	}

	return s.respondJSONOrTOON(ctx, req, resp)
}

// ---------------------------------------------------------------------------
// 10.7 handleFindCycles and handleWouldCreateCycle
// ---------------------------------------------------------------------------

func (s *Server) handleFindCycles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "")

	cycles := analysis.DetectCycles(s.graph, s.getCommunities(), scope)

	if s.isGCX(ctx, req) {
		items := make([]cycleItem, 0, len(cycles))
		for _, c := range cycles {
			items = append(items, cycleItem{
				Size:     len(c.Path),
				Severity: c.Kind,
				Nodes:    c.Path,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("cycles", items))
	}

	if len(cycles) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"cycles":  []any{},
			"message": "no dependency cycles detected",
		})
	}

	// Truncate to 20 highest-severity (already sorted by severity desc)
	totalCount := len(cycles)
	truncated := false
	if len(cycles) > 20 {
		cycles = cycles[:20]
		truncated = true
	}

	if isCompact(req) {
		var b strings.Builder
		for _, c := range cycles {
			fmt.Fprintf(&b, "%s severity=%d %s\n", c.Kind, c.Severity, strings.Join(c.Path, " → "))
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total)\n", totalCount)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"cycles":    cycles,
		"total":     totalCount,
		"truncated": truncated,
	})
}

func (s *Server) handleWouldCreateCycle(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fromID, err := req.RequireString("from_id")
	if err != nil {
		return mcp.NewToolResultError("from_id is required"), nil
	}
	toID, err := req.RequireString("to_id")
	if err != nil {
		return mcp.NewToolResultError("to_id is required"), nil
	}

	// Validate both symbols exist
	if s.graph.GetNode(fromID) == nil {
		return mcp.NewToolResultError("symbol not found: " + fromID), nil
	}
	if s.graph.GetNode(toID) == nil {
		return mcp.NewToolResultError("symbol not found: " + toID), nil
	}

	wouldCycle, path := analysis.WouldCreateCycle(s.graph, fromID, toID)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("would_create_cycle", map[string]any{
			"would_cycle": wouldCycle,
			"path":        path,
		}))
	}

	if isCompact(req) {
		if wouldCycle {
			return mcp.NewToolResultText(fmt.Sprintf("would_cycle=true %s\n", strings.Join(path, " → "))), nil
		}
		return mcp.NewToolResultText("would_cycle=false\n"), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"would_cycle": wouldCycle,
		"path":        path,
	})
}

// ---------------------------------------------------------------------------
// 10.8 handleDiffContext
// ---------------------------------------------------------------------------

// diffFileGroup groups changed symbols by file with risk assessment.
type diffFileGroup struct {
	FilePath string           `json:"file_path"`
	Risk     string           `json:"risk"`
	Symbols  []diffSymbolInfo `json:"symbols"`
}

// diffSymbolInfo holds enriched context for a single changed symbol.
type diffSymbolInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	StartLine int      `json:"start_line"`
	Signature string   `json:"signature,omitempty"`
	Source    string   `json:"source,omitempty"`
	Callers   []string `json:"callers,omitempty"`
	Callees   []string `json:"callees,omitempty"`
	Community string   `json:"community,omitempty"`
	Processes []string `json:"processes,omitempty"`
}

func (s *Server) handleDiffContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "unstaged")
	baseRef := req.GetString("base_ref", "main")

	repoRoot := "."
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			repoRoot = root
		}
	}

	diff, err := analysis.MapGitDiff(s.graph, repoRoot, scope, baseRef)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if len(diff.ChangedSymbols) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"files":   []any{},
			"message": "no changes detected",
		})
	}

	communities := s.getCommunities()
	processes := s.getProcesses()

	// Build enriched symbol info
	var allSymbols []diffSymbolInfo
	for _, cs := range diff.ChangedSymbols {
		node := s.graph.GetNode(cs.ID)
		if node == nil {
			continue
		}

		info := diffSymbolInfo{
			ID:        cs.ID,
			Name:      cs.Name,
			Kind:      cs.Kind,
			StartLine: cs.Line,
		}

		// Signature
		if sig, ok := node.Meta["signature"].(string); ok {
			info.Signature = sig
		}

		// Source
		if node.StartLine > 0 && node.EndLine > 0 {
			if absPath, err := s.resolveNodePath(node); err == nil {
				if source, _, _, readErr := readLines(absPath, node.StartLine, node.EndLine, 0); readErr == nil {
					info.Source = source
				}
			}
		}

		// Callers (depth 1)
		callers := s.engineFor(ctx).GetCallers(cs.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if cn.ID != cs.ID {
				info.Callers = append(info.Callers, cn.ID)
			}
		}

		// Callees (depth 1)
		callees := s.engineFor(ctx).GetCallChain(cs.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
		for _, cn := range callees.Nodes {
			if cn.ID != cs.ID {
				info.Callees = append(info.Callees, cn.ID)
			}
		}

		// Community
		if communities != nil {
			if cid, ok := communities.NodeToComm[cs.ID]; ok {
				info.Community = cid
			}
		}

		// Processes
		if processes != nil {
			info.Processes = processes.NodeToProcs[cs.ID]
		}

		allSymbols = append(allSymbols, info)
	}

	// Group by file
	fileMap := make(map[string][]diffSymbolInfo)
	for _, sym := range allSymbols {
		fp := ""
		if n := s.graph.GetNode(sym.ID); n != nil {
			fp = n.FilePath
		}
		if fp == "" {
			continue
		}
		fileMap[fp] = append(fileMap[fp], sym)
	}

	// Compute per-file risk
	var groups []diffFileGroup
	for fp, syms := range fileMap {
		// Compute risk based on blast radius of symbols in this file
		symbolIDs := make([]string, len(syms))
		for i, sym := range syms {
			symbolIDs[i] = sym.ID
		}
		impact := analysis.AnalyzeImpact(s.graph, symbolIDs, communities, processes)

		groups = append(groups, diffFileGroup{
			FilePath: fp,
			Risk:     string(impact.Risk),
			Symbols:  syms,
		})
	}

	// Sort by risk (CRITICAL > HIGH > MEDIUM > LOW)
	riskOrder := map[string]int{"CRITICAL": 0, "HIGH": 1, "MEDIUM": 2, "LOW": 3}
	sort.Slice(groups, func(i, j int) bool {
		ri := riskOrder[groups[i].Risk]
		rj := riskOrder[groups[j].Risk]
		if ri != rj {
			return ri < rj
		}
		return groups[i].FilePath < groups[j].FilePath
	})

	// Truncate to 50 symbols total
	totalSymbols := 0
	for _, g := range groups {
		totalSymbols += len(g.Symbols)
	}
	truncated := false
	if totalSymbols > 50 {
		truncated = true
		count := 0
		var truncGroups []diffFileGroup
		for _, g := range groups {
			if count >= 50 {
				break
			}
			remaining := 50 - count
			if len(g.Symbols) > remaining {
				g.Symbols = g.Symbols[:remaining]
			}
			truncGroups = append(truncGroups, g)
			count += len(g.Symbols)
		}
		groups = truncGroups
	}

	if isCompact(req) {
		var b strings.Builder
		for _, g := range groups {
			for _, sym := range g.Symbols {
				fmt.Fprintf(&b, "%s %s %s callers=%d callees=%d\n",
					sym.ID, sym.Kind, g.Risk, len(sym.Callers), len(sym.Callees))
			}
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total symbols)\n", totalSymbols)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"files":         groups,
		"total_symbols": totalSymbols,
		"total_files":   len(groups),
		"truncated":     truncated,
	})
}

// ---------------------------------------------------------------------------
// 10.9 handleIndexHealth
// ---------------------------------------------------------------------------

func (s *Server) handleIndexHealth(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.indexer == nil {
		return mcp.NewToolResultError("no indexer available"), nil
	}
	result := s.buildIndexHealthPayload()

	if isCompact(req) {
		stale, _ := result["stale_files"].([]string)
		failures, _ := result["parse_failures"].(map[string]string)
		line := fmt.Sprintf("health=%.1f%% nodes=%d stale=%d failures=%d",
			result["health_score"], result["node_count"], len(stale), len(failures))
		if _, ok := result["recommendation"]; ok {
			line += " [needs re-index]"
		}
		return mcp.NewToolResultText(line + "\n"), nil
	}

	return s.respondJSONOrTOON(ctx, req, result)
}

// buildIndexHealthPayload returns the same data the `index_health`
// tool emits. Shared with the `gortex://index-health` resource.
// Returns nil when no indexer is wired.
func (s *Server) buildIndexHealthPayload() map[string]any {
	if s.indexer == nil {
		return nil
	}

	totalDetected := s.indexer.TotalDetected()
	parseErrors := s.indexer.ParseErrors()

	// When totalDetected is 0 (e.g., graph restored from cache without a full re-index),
	// fall back to counting file nodes in the graph.
	if totalDetected == 0 {
		stats := s.graph.Stats()
		if fileCount, ok := stats.ByKind[string(graph.KindFile)]; ok {
			totalDetected = fileCount
		}
	}

	successfullyIndexed := totalDetected - len(parseErrors)
	if successfullyIndexed < 0 {
		successfullyIndexed = 0
	}

	var healthScore float64
	if totalDetected > 0 {
		healthScore = math.Round(float64(successfullyIndexed)/float64(totalDetected)*1000) / 10
	}

	var staleFiles []string
	mtimes := s.indexer.FileMtimes()
	for relPath := range mtimes {
		if s.indexer.IsStale(relPath) {
			staleFiles = append(staleFiles, relPath)
		}
	}

	stats := s.graph.Stats()
	langCoverage := make(map[string]bool)
	for lang := range stats.ByLanguage {
		langCoverage[lang] = true
	}

	lastIndexTime := s.indexer.LastIndexTime()
	lastIndexStr := ""
	if !lastIndexTime.IsZero() {
		lastIndexStr = lastIndexTime.Format("2006-01-02T15:04:05Z07:00")
	}

	var recommendation string
	if healthScore < 80 {
		recommendation = "Health score below 80%. Run index_repository with path \".\" to re-index the codebase."
	}

	// Edgeless-index sanity check: a populated graph with files and
	// symbol nodes but zero edges means edge extraction failed
	// wholesale (a broken grammar, an aborted reindex). Even a single
	// one-function file yields containment edges, so this only trips on
	// a real regression.
	edgesOK := totalDetected <= 0 || stats.TotalNodes <= 0 || stats.TotalEdges != 0
	if !edgesOK {
		msg := "Index has files and symbol nodes but zero edges — edge extraction failed. Re-index with index_repository path \".\"; if it persists the language grammar may be broken."
		if recommendation == "" {
			recommendation = msg
		} else {
			recommendation = msg + " " + recommendation
		}
	}

	result := map[string]any{
		"health_score":         healthScore,
		"total_detected":       totalDetected,
		"successfully_indexed": successfullyIndexed,
		"language_coverage":    langCoverage,
		"last_index_time":      lastIndexStr,
		"node_count":           stats.TotalNodes,
		"edge_count":           stats.TotalEdges,
		"edges_ok":             edgesOK,
	}
	if len(parseErrors) > 0 {
		result["parse_failures"] = parseErrors
	}
	if len(staleFiles) > 0 {
		result["stale_files"] = staleFiles
	}
	if recommendation != "" {
		result["recommendation"] = recommendation
	}

	return result
}

// ---------------------------------------------------------------------------
// 10.10 handleGetSymbolHistory
// ---------------------------------------------------------------------------

func (s *Server) handleGetSymbolHistory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbolID := req.GetString("id", "")

	if symbolID != "" {
		// Single symbol history
		mods := s.symHistory.Get(symbolID)
		if len(mods) == 0 {
			return s.respondJSONOrTOON(ctx, req, map[string]any{
				"symbol_id":     symbolID,
				"modifications": []any{},
				"message":       "no modifications recorded for this symbol",
			})
		}

		churning := len(mods) >= 3

		if isCompact(req) {
			churnFlag := ""
			if churning {
				churnFlag = " [churning]"
			}
			return mcp.NewToolResultText(fmt.Sprintf("%s count=%d%s\n", symbolID, len(mods), churnFlag)), nil
		}

		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"symbol_id":     symbolID,
			"count":         len(mods),
			"modifications": mods,
			"churning":      churning,
		})
	}

	// All symbols, sorted by count descending
	all := s.symHistory.All()
	if len(all) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"symbols": []any{},
			"message": "no modifications recorded this session",
		})
	}

	type symbolEntry struct {
		SymbolID      string               `json:"symbol_id"`
		Count         int                  `json:"count"`
		Churning      bool                 `json:"churning"`
		Modifications []SymbolModification `json:"modifications"`
	}

	var entries []symbolEntry
	for id, mods := range all {
		entries = append(entries, symbolEntry{
			SymbolID:      id,
			Count:         len(mods),
			Churning:      len(mods) >= 3,
			Modifications: mods,
		})
	}

	// Sort by count descending
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Count > entries[j].Count
	})

	if isCompact(req) {
		var b strings.Builder
		for _, e := range entries {
			churnFlag := ""
			if e.Churning {
				churnFlag = " [churning]"
			}
			fmt.Fprintf(&b, "%s count=%d%s\n", e.SymbolID, e.Count, churnFlag)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"symbols": entries,
		"total":   len(entries),
	})
}

// ---------------------------------------------------------------------------
// 10.11 handleBatchEdit
// ---------------------------------------------------------------------------

// batchEditItem represents a single edit in a batch.
type batchEditItem struct {
	SymbolID  string `json:"id"`
	OldSource string `json:"old_source"`
	NewSource string `json:"new_source"`
}

// batchEditResult represents the outcome of a single edit in the batch.
type batchEditResult struct {
	SymbolID string `json:"id"`
	FilePath string `json:"path"`
	Status   string `json:"status"` // "applied", "failed", "skipped"
	Error    string `json:"error,omitempty"`
}

func (s *Server) handleBatchEdit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	editsStr, err := req.RequireString("edits")
	if err != nil {
		return mcp.NewToolResultError("edits is required"), nil
	}

	var edits []batchEditItem
	if err := json.Unmarshal([]byte(editsStr), &edits); err != nil {
		return mcp.NewToolResultError("invalid edits JSON: " + err.Error()), nil
	}
	if len(edits) == 0 {
		return mcp.NewToolResultError("edits array is empty"), nil
	}

	dryRun := false
	if v, ok := req.GetArguments()["dry_run"].(bool); ok {
		dryRun = v
	}

	// Sort edits in dependency order using get_edit_plan logic:
	// Definitions first, then implementations, then callers.
	type editWithOrder struct {
		edit  batchEditItem
		order int
		file  string
	}

	var ordered []editWithOrder
	for _, edit := range edits {
		node := s.engineFor(ctx).GetSymbol(edit.SymbolID)
		order := 50 // default middle priority
		filePath := ""
		if node != nil {
			filePath = node.FilePath
			// Definitions/interfaces get lowest order (edited first)
			if node.Kind == graph.KindInterface || node.Kind == graph.KindType {
				order = 0
			} else if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
				// Check if this symbol is depended on by other edits
				for _, other := range edits {
					if other.SymbolID == edit.SymbolID {
						continue
					}
					// Check if other calls this symbol
					callers := s.engineFor(ctx).GetCallers(edit.SymbolID, query.QueryOptions{Depth: 1, Limit: 100, Detail: "brief"})
					for _, cn := range callers.Nodes {
						if cn.ID == other.SymbolID {
							order = 10 // this is a dependency — edit first
							break
						}
					}
				}
				if order == 50 {
					order = 20 // regular function
				}
			}
		}
		ordered = append(ordered, editWithOrder{edit: edit, order: order, file: filePath})
	}

	// Sort by order ascending (lowest = edit first)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].order != ordered[j].order {
			return ordered[i].order < ordered[j].order
		}
		return ordered[i].file < ordered[j].file
	})

	if dryRun {
		var plan []map[string]any
		for i, o := range ordered {
			entry := map[string]any{
				"order":  i + 1,
				"id":     o.edit.SymbolID,
				"path":   o.file,
				"status": "planned",
			}
			plan = append(plan, entry)
		}

		if isCompact(req) {
			var b strings.Builder
			for _, p := range plan {
				fmt.Fprintf(&b, "%s %s planned\n", p["id"], p["path"])
			}
			return mcp.NewToolResultText(b.String()), nil
		}

		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"plan":    plan,
			"dry_run": true,
			"total":   len(plan),
		})
	}

	// Apply edits sequentially
	var results []batchEditResult
	failed := false

	for _, o := range ordered {
		if failed {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: o.file,
				Status:   "skipped",
			})
			continue
		}

		node := s.engineFor(ctx).GetSymbol(o.edit.SymbolID)
		if node == nil {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: o.file,
				Status:   "failed",
				Error:    "symbol not found: " + o.edit.SymbolID,
			})
			failed = true
			continue
		}

		if node.StartLine == 0 || node.EndLine == 0 {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    "symbol has no line range",
			})
			failed = true
			continue
		}

		// Resolve file path
		absPath, resolveErr := s.resolveNodePath(node)
		if resolveErr != nil {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    resolveErr.Error(),
			})
			failed = true
			continue
		}

		// Read file
		content, readErr := os.ReadFile(absPath)
		if readErr != nil {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    fmt.Sprintf("could not read file: %v", readErr),
			})
			failed = true
			continue
		}

		fileStr := string(content)
		lines := strings.Split(fileStr, "\n")

		if node.StartLine > len(lines) || node.EndLine > len(lines) {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    "symbol line range exceeds file length",
			})
			failed = true
			continue
		}

		symbolSource := strings.Join(lines[node.StartLine-1:node.EndLine], "\n")
		if !strings.Contains(symbolSource, o.edit.OldSource) {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    "old_source not found within symbol",
			})
			failed = true
			continue
		}

		// Compute offset within file
		symbolStart := 0
		for i := 0; i < node.StartLine-1 && i < len(lines); i++ {
			symbolStart += len(lines[i]) + 1
		}
		symbolEnd := symbolStart + len(symbolSource)
		if symbolEnd > len(fileStr) {
			symbolEnd = len(fileStr)
		}

		offset := strings.Index(fileStr[symbolStart:symbolEnd], o.edit.OldSource)
		if offset < 0 {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    "old_source not found in symbol region",
			})
			failed = true
			continue
		}

		editStart := symbolStart + offset
		editEnd := editStart + len(o.edit.OldSource)
		newContent := fileStr[:editStart] + o.edit.NewSource + fileStr[editEnd:]

		if writeErr := os.WriteFile(absPath, []byte(newContent), 0o644); writeErr != nil {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    fmt.Sprintf("could not write file: %v", writeErr),
			})
			failed = true
			continue
		}

		// Re-index the file after edit
		if s.indexer != nil {
			_ = s.indexer.IndexFile(absPath)
		}

		results = append(results, batchEditResult{
			SymbolID: o.edit.SymbolID,
			FilePath: node.FilePath,
			Status:   "applied",
		})
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range results {
			fmt.Fprintf(&b, "%s %s %s\n", r.SymbolID, r.FilePath, r.Status)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	// Count statuses
	applied, failedCount, skipped := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "applied":
			applied++
		case "failed":
			failedCount++
		case "skipped":
			skipped++
		}
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"results": results,
		"summary": map[string]int{
			"applied": applied,
			"failed":  failedCount,
			"skipped": skipped,
			"total":   len(results),
		},
	})
}

// ---------------------------------------------------------------------------
// handleContracts — unified dispatcher for contracts (replaces 2 tools)
// ---------------------------------------------------------------------------

func (s *Server) handleContracts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action := req.GetString("action", "list")
	switch action {
	case "list", "":
		return s.handleGetContracts(ctx, req)
	case "check":
		return s.handleCheckContracts(ctx, req)
	case "validate":
		return s.handleValidateContracts(ctx, req)
	default:
		return mcp.NewToolResultError("unknown contracts action: " + action + " (expected: list, check, or validate)"), nil
	}
}

// ---------------------------------------------------------------------------
// handleGetContracts
// ---------------------------------------------------------------------------

func (s *Server) handleGetContracts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	registry := s.effectiveContractRegistry()
	if registry == nil {
		return mcp.NewToolResultError("no contract registry available — index a repository first"), nil
	}

	contractType := req.GetString("type", "")
	role := req.GetString("role", "")

	args := req.GetArguments()
	allRepos := false
	if v, ok := args["all_repos"].(bool); ok {
		allRepos = v
	}
	includeDeps := false
	if v, ok := args["include_deps"].(bool); ok {
		includeDeps = v
	}

	// resolveRepoFilter unifies repo/project/ref into a single allow-set
	// and falls back to the active project when no axis is given — same
	// default scoping every other query tool uses. all_repos=true opts out.
	var allowed map[string]bool
	if !allRepos {
		var err error
		allowed, err = s.resolveRepoFilter(ctx, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	all := registry.All()

	// Apply filters.
	var filtered []contracts.Contract
	otherRepos := make(map[string]int)
	depsSkipped := 0
	for _, c := range all {
		isDep := c.Type == contracts.ContractDependency || excludes.IsVendored(c.FilePath)

		if allowed != nil && c.RepoPrefix != "" && !allowed[c.RepoPrefix] {
			if includeDeps || !isDep {
				otherRepos[c.RepoPrefix]++
			}
			continue
		}
		if contractType != "" && string(c.Type) != contractType {
			continue
		}
		if role != "" && string(c.Role) != role {
			continue
		}
		if !includeDeps && isDep {
			depsSkipped++
			continue
		}
		filtered = append(filtered, c)
	}

	otherReposTotal := 0
	for _, n := range otherRepos {
		otherReposTotal += n
	}

	// Cap response — every per-contract row carries handler trails and
	// schema metadata, so a few hundred contracts blows past the MCP
	// per-response token cap. Default 200 surfaces enough to be useful;
	// callers that need every contract pass `limit` explicitly. With
	// pagination on, the contract list is sliced [offset, offset+limit)
	// with a `next_cursor` returned when the tail is unread.
	contractsLimit := 200
	if v, ok := args["limit"].(float64); ok && v > 0 {
		contractsLimit = int(v)
	}
	contractsOffset := decodeCursor(req.GetString("cursor", ""))
	contractsTotal := len(filtered)
	if contractsOffset > contractsTotal {
		contractsOffset = contractsTotal
	}
	contractsEnd := contractsOffset + contractsLimit
	if contractsEnd > contractsTotal {
		contractsEnd = contractsTotal
	}
	filtered = filtered[contractsOffset:contractsEnd]
	contractsTruncated := contractsEnd < contractsTotal
	contractsNextCursor := ""
	if contractsTruncated {
		contractsNextCursor = encodeCursor(contractsEnd)
	}

	if isCompact(req) {
		var b strings.Builder
		// Group by repo for readability in multi-repo mode.
		byRepo := make(map[string][]contracts.Contract)
		for _, c := range filtered {
			repo := c.RepoPrefix
			if repo == "" {
				repo = "(default)"
			}
			byRepo[repo] = append(byRepo[repo], c)
		}
		for repoName, items := range byRepo {
			if len(byRepo) > 1 {
				fmt.Fprintf(&b, "\n[%s] (%d contracts)\n", repoName, len(items))
			}
			for _, c := range items {
				fmt.Fprintf(&b, "%s %s %s %s:%d\n", c.Type, c.Role, c.ID, c.FilePath, c.Line)
			}
		}
		if len(filtered) == 0 {
			b.WriteString("no contracts found\n")
		}
		fmt.Fprintf(&b, "total: %d contracts\n", len(filtered))
		if otherReposTotal > 0 {
			fmt.Fprintf(&b, "other_repos: %d contracts in %d repo(s) (pass all_repos=true or repo=<prefix> to include)\n", otherReposTotal, len(otherRepos))
		}
		if depsSkipped > 0 {
			fmt.Fprintf(&b, "dependencies_skipped: %d (pass include_deps=true to include)\n", depsSkipped)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	if s.isGCX(ctx, req) {
		extra := []string{}
		if otherReposTotal > 0 {
			extra = append(extra, "other_repos_contracts", fmt.Sprintf("%d", otherReposTotal),
				"other_repos", fmt.Sprintf("%d", len(otherRepos)))
		}
		if depsSkipped > 0 {
			extra = append(extra, "dependencies_skipped", fmt.Sprintf("%d", depsSkipped))
		}
		return s.gcxResponseWithBudget(req)(encodeContractsList(filtered, len(filtered), extra...))
	}

	// Group by repo, then by type for structured output.
	type repoGroup struct {
		Contracts map[string][]contracts.Contract `json:"contracts"`
		Total     int                             `json:"total"`
	}
	byRepo := make(map[string]*repoGroup)
	for _, c := range filtered {
		repo := c.RepoPrefix
		if repo == "" {
			repo = "(default)"
		}
		if byRepo[repo] == nil {
			byRepo[repo] = &repoGroup{Contracts: make(map[string][]contracts.Contract)}
		}
		byRepo[repo].Contracts[string(c.Type)] = append(byRepo[repo].Contracts[string(c.Type)], c)
		byRepo[repo].Total++
	}

	payload := map[string]any{
		"by_repo":   byRepo,
		"total":     contractsTotal,
		"truncated": contractsTruncated,
	}
	if contractsTruncated {
		payload["limit"] = contractsLimit
	}
	if contractsNextCursor != "" {
		payload["next_cursor"] = contractsNextCursor
	}
	if otherReposTotal > 0 {
		payload["other_repos"] = map[string]any{
			"total":      otherReposTotal,
			"repo_count": len(otherRepos),
			"by_repo":    otherRepos,
			"hint":       "pass all_repos=true or repo=<prefix>/project=<name> to include these",
		}
	}
	if depsSkipped > 0 {
		payload["dependencies_skipped"] = map[string]any{
			"total": depsSkipped,
			"hint":  "pass include_deps=true to include type=dependency and vendor-pathed contracts",
		}
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// ---------------------------------------------------------------------------
// handleCheckContracts
// ---------------------------------------------------------------------------

func (s *Server) handleCheckContracts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	registry := s.effectiveContractRegistry()
	if registry == nil {
		return mcp.NewToolResultError("no contract registry available — index a repository first"), nil
	}

	// resolveRepoFilter folds repo/project/ref into one allow-set so
	// `contracts check` can answer "does project X match" without the
	// caller having to list its repos by hand. A nil allow-set means
	// "all tracked repos" and keeps the original single-registry fast
	// path — avoids a pointless copy of the full registry.
	allowed, err := s.resolveRepoFilter(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	reg := registry
	if allowed != nil {
		reg = contracts.NewRegistry()
		for _, c := range registry.All() {
			if c.RepoPrefix != "" && !allowed[c.RepoPrefix] {
				continue
			}
			reg.Add(c)
		}
	}

	result := contracts.Match(reg)

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "matched: %d pairs\n", len(result.Matched))
		for _, m := range result.Matched {
			cross := ""
			if m.CrossRepo {
				cross = " [cross-repo]"
			}
			provRepo := m.Provider.RepoPrefix
			consRepo := m.Consumer.RepoPrefix
			if provRepo == "" {
				provRepo = "(default)"
			}
			if consRepo == "" {
				consRepo = "(default)"
			}
			fmt.Fprintf(&b, "  %s: [%s] %s:%d -> [%s] %s:%d%s\n",
				m.ContractID,
				provRepo, m.Provider.FilePath, m.Provider.Line,
				consRepo, m.Consumer.FilePath, m.Consumer.Line,
				cross)
		}
		fmt.Fprintf(&b, "orphan providers: %d\n", len(result.OrphanProviders))
		for _, o := range result.OrphanProviders {
			repoLabel := o.RepoPrefix
			if repoLabel == "" {
				repoLabel = "(default)"
			}
			fmt.Fprintf(&b, "  [%s] %s %s:%d\n", repoLabel, o.ID, o.FilePath, o.Line)
		}
		fmt.Fprintf(&b, "orphan consumers: %d\n", len(result.OrphanConsumers))
		for _, o := range result.OrphanConsumers {
			repoLabel := o.RepoPrefix
			if repoLabel == "" {
				repoLabel = "(default)"
			}
			fmt.Fprintf(&b, "  [%s] %s %s:%d\n", repoLabel, o.ID, o.FilePath, o.Line)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeContractsCheck(result))
	}

	payload := map[string]any{
		"matched":          result.Matched,
		"orphan_providers": result.OrphanProviders,
		"orphan_consumers": result.OrphanConsumers,
		"summary": map[string]int{
			"matched_pairs":    len(result.Matched),
			"orphan_providers": len(result.OrphanProviders),
			"orphan_consumers": len(result.OrphanConsumers),
		},
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// ---------------------------------------------------------------------------
// handleValidateContracts
// ---------------------------------------------------------------------------
//
// Pairs each contract's provider and consumer sides, diffs their
// request/response shapes (populated by the Stage 2 snapshotting
// pass), and returns a list of issues classified as breaking,
// warning, or info. Accepts the same repo/project/ref scoping
// parameters as `check` so callers can limit the diff to one project.

func (s *Server) handleValidateContracts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	registry := s.effectiveContractRegistry()
	if registry == nil {
		return mcp.NewToolResultError("no contract registry available — index a repository first"), nil
	}

	allowed, err := s.resolveRepoFilter(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	reg := registry
	if allowed != nil {
		reg = contracts.NewRegistry()
		for _, c := range registry.All() {
			if c.RepoPrefix != "" && !allowed[c.RepoPrefix] {
				continue
			}
			reg.Add(c)
		}
	}

	// Shape lookup pulls Shape out of the type node's meta — the
	// indexer attaches it during commitContracts (see
	// snapshotContractShapes in internal/indexer/indexer.go).
	lookup := contracts.ShapeLookup(func(symbolID string) *contracts.Shape {
		n := s.graph.GetNode(symbolID)
		if n == nil || n.Meta == nil {
			return nil
		}
		switch v := n.Meta["shape"].(type) {
		case *contracts.Shape:
			return v
		case contracts.Shape:
			return &v
		}
		return nil
	})

	issues := contracts.Validate(reg, lookup)

	// Severity rollup for easy at-a-glance counts.
	summary := map[string]int{"breaking": 0, "warning": 0, "info": 0, "total": len(issues)}
	for _, is := range issues {
		switch is.Severity {
		case contracts.SeverityBreaking:
			summary["breaking"]++
		case contracts.SeverityWarning:
			summary["warning"]++
		case contracts.SeverityInfo:
			summary["info"]++
		}
	}

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "issues: %d (breaking=%d warning=%d info=%d)\n",
			summary["total"], summary["breaking"], summary["warning"], summary["info"])
		for _, is := range issues {
			field := is.Field
			if field == "" {
				field = "-"
			}
			fmt.Fprintf(&b, "  [%s] %s %s field=%s prov=%s cons=%s %s\n",
				is.Severity, is.ContractID, is.Kind, field, is.Provider, is.Consumer, is.Details)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	payload := map[string]any{
		"issues":  issues,
		"summary": summary,
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// ---------------------------------------------------------------------------
// handleFeedback — unified dispatcher for feedback (replaces 2 tools)
// ---------------------------------------------------------------------------

func (s *Server) handleFeedback(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action, err := req.RequireString("action")
	if err != nil {
		return mcp.NewToolResultError("action is required (one of: record, query)"), nil
	}
	switch action {
	case "record":
		return s.handleRecordFeedback(ctx, req)
	case "query":
		return s.handleQueryFeedback(ctx, req)
	default:
		return mcp.NewToolResultError("unknown feedback action: " + action + " (expected: record or query)"), nil
	}
}

// ---------------------------------------------------------------------------
// 12.1 handleRecordFeedback / handleQueryFeedback
// ---------------------------------------------------------------------------

func (s *Server) handleRecordFeedback(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task := req.GetString("task", "")
	if task == "" {
		return mcp.NewToolResultError("task is required"), nil
	}

	useful := splitCSV(req.GetString("useful", ""))
	notNeeded := splitCSV(req.GetString("not_needed", ""))
	missing := splitCSV(req.GetString("missing", ""))

	if len(useful) == 0 && len(notNeeded) == 0 && len(missing) == 0 {
		return mcp.NewToolResultError("at least one of useful, not_needed, or missing must be provided"), nil
	}

	source := req.GetString("tool_source", "smart_context")

	entry := persistence.FeedbackEntry{
		Task:      task,
		Useful:    useful,
		NotNeeded: notNeeded,
		Missing:   missing,
		Source:    source,
	}

	if s.feedback == nil {
		return mcp.NewToolResultError("feedback storage not initialized (no cache directory)"), nil
	}

	if err := s.feedback.Record(entry); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to record feedback: %v", err)), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"recorded":         true,
		"useful_count":     len(useful),
		"not_needed_count": len(notNeeded),
		"missing_count":    len(missing),
	})
}

func (s *Server) handleQueryFeedback(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.feedback == nil || !s.feedback.HasData() {
		empty := map[string]any{
			"total_entries": 0,
			"accuracy":      0.0,
			"most_useful":   []any{},
			"most_missed":   []any{},
			"most_demoted":  []any{},
		}
		if s.isGCX(ctx, req) {
			return s.gcxResponseWithBudget(req)(encodeFeedbackQuery(empty))
		}
		if s.isTOON(ctx, req) {
			return returnTOON(empty)
		}
		return s.respondJSONOrTOON(ctx, req, empty)
	}

	topN := 10
	if n := req.GetInt("top_n", 0); n > 0 {
		topN = n
	}

	toolSource := req.GetString("tool_source", "all")

	stats := s.feedback.AggregatedStats(toolSource, topN)

	if isCompact(req) {
		var sb strings.Builder
		fmt.Fprintf(&sb, "Feedback: %v entries, %.0f%% accuracy\n",
			stats["total_entries"], stats["accuracy"].(float64)*100)
		return mcp.NewToolResultText(sb.String()), nil
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeFeedbackQuery(stats))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(stats)
	}

	return s.respondJSONOrTOON(ctx, req, stats)
}

// splitCSV splits a comma-separated string into trimmed, non-empty parts.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// 12.2 handleExportContext
// ---------------------------------------------------------------------------

func (s *Server) handleExportContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Delegate to smart_context for the raw data. Force `format: json`
	// on the inner call so the unmarshal below always sees JSON,
	// regardless of the caller's outer format preference or the
	// server's client-aware default (which auto-selects GCX1 for
	// known clients and would otherwise blow up our json.Unmarshal
	// with "invalid character 'G'").
	smartReq := req
	args, _ := smartReq.Params.Arguments.(map[string]any)
	innerArgs := make(map[string]any, len(args)+1)
	maps.Copy(innerArgs, args)
	innerArgs["format"] = "json"
	smartReq.Params.Arguments = innerArgs

	smartResult, err := s.handleSmartContext(ctx, smartReq)
	if err != nil {
		return nil, err
	}
	// If smart_context returned an error result, pass it through.
	if smartResult.IsError {
		return smartResult, nil
	}

	format := req.GetString("format", "markdown")
	tokenBudget := req.GetInt("token_budget", 2000)
	if tokenBudget <= 0 {
		tokenBudget = 2000
	}
	if tokenBudget > 8000 {
		tokenBudget = 8000
	}

	// Extract the JSON data from smart_context result.
	var data map[string]any
	for _, content := range smartResult.Content {
		if textContent, ok := content.(mcp.TextContent); ok {
			if jsonErr := json.Unmarshal([]byte(textContent.Text), &data); jsonErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to parse smart_context output: %v", jsonErr)), nil
			}
			break
		}
	}
	if data == nil {
		return mcp.NewToolResultError("no data from smart_context"), nil
	}

	if format == "json" {
		return s.respondJSONOrTOON(ctx, req, data)
	}

	// Render as markdown briefing.
	md := renderContextMarkdown(data, tokenBudget)
	return mcp.NewToolResultText(md), nil
}

// renderContextMarkdown converts smart_context JSON output into a self-contained
// markdown briefing suitable for sharing outside MCP.
func renderContextMarkdown(data map[string]any, tokenBudget int) string {
	var sb strings.Builder
	// Conservative char budget calibrated for cl100k_base on code-heavy input.
	charBudget := tokens.TokensToChars(tokenBudget)

	// Header.
	task, _ := data["task"].(string)
	sb.WriteString("# Context Briefing\n\n")
	fmt.Fprintf(&sb, "**Task:** %s\n\n", task)

	// Keywords.
	if kws, ok := data["keywords"].([]any); ok && len(kws) > 0 {
		sb.WriteString("**Keywords:** ")
		for i, kw := range kws {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "`%v`", kw)
		}
		sb.WriteString("\n\n")
	}

	// Key symbols.
	if symbols, ok := data["relevant_symbols"].([]any); ok && len(symbols) > 0 {
		sb.WriteString("## Key Symbols\n\n")
		for _, sym := range symbols {
			symMap, ok := sym.(map[string]any)
			if !ok {
				continue
			}
			name, _ := symMap["name"].(string)
			kind, _ := symMap["kind"].(string)
			id, _ := symMap["id"].(string)
			filePath, _ := symMap["file_path"].(string)
			startLine, _ := symMap["start_line"].(float64)

			fmt.Fprintf(&sb, "### `%s` (%s)\n\n", name, kind)
			fmt.Fprintf(&sb, "- **ID:** `%s`\n", id)
			fmt.Fprintf(&sb, "- **File:** `%s:%d`\n", filePath, int(startLine))

			if sig, ok := symMap["signature"].(string); ok && sig != "" {
				fmt.Fprintf(&sb, "- **Signature:** `%s`\n", sig)
			}

			// Include source if within budget.
			if source, ok := symMap["source"].(string); ok && source != "" {
				if sb.Len()+len(source) < charBudget {
					sb.WriteString("\n```go\n")
					sb.WriteString(source)
					sb.WriteString("\n```\n")
				} else {
					sb.WriteString("- *(source omitted — token budget exceeded)*\n")
				}
			}
			sb.WriteString("\n")
		}
	}

	// Callers and callees.
	if callers, ok := data["callers"].([]any); ok && len(callers) > 0 {
		sb.WriteString("## Callers\n\n")
		for _, c := range callers {
			fmt.Fprintf(&sb, "- `%v`\n", c)
		}
		sb.WriteString("\n")
	}

	if callees, ok := data["callees"].([]any); ok && len(callees) > 0 {
		sb.WriteString("## Callees\n\n")
		for _, c := range callees {
			fmt.Fprintf(&sb, "- `%v`\n", c)
		}
		sb.WriteString("\n")
	}

	// Cross-repo dependencies.
	if crossDeps, ok := data["cross_repo_dependencies"].([]any); ok && len(crossDeps) > 0 {
		sb.WriteString("## Cross-Repo Dependencies\n\n")
		for _, dep := range crossDeps {
			depMap, ok := dep.(map[string]any)
			if !ok {
				continue
			}
			name, _ := depMap["name"].(string)
			repo, _ := depMap["repo_prefix"].(string)
			edgeKind, _ := depMap["edge_kind"].(string)
			fmt.Fprintf(&sb, "- `%s` (repo: %s, %s)\n", name, repo, edgeKind)
		}
		sb.WriteString("\n")
	}

	// Test files.
	if tests, ok := data["related_test_files"].([]any); ok && len(tests) > 0 {
		sb.WriteString("## Related Tests\n\n")
		for _, t := range tests {
			fmt.Fprintf(&sb, "- `%v`\n", t)
		}
		sb.WriteString("\n")
	}

	// Files to edit.
	if files, ok := data["files_to_edit"].([]any); ok && len(files) > 0 {
		sb.WriteString("## Files to Edit\n\n")
		for _, f := range files {
			fmt.Fprintf(&sb, "- `%v`\n", f)
		}
		sb.WriteString("\n")
	}

	// Footer.
	sb.WriteString("---\n*Generated by `gortex export_context`*\n")

	return sb.String()
}

// ---------------------------------------------------------------------------
// 13.2 handleAuditAgentConfig — scans CLAUDE.md / AGENTS.md / Cursor rules /
// Copilot instructions for stale symbol refs, dead file paths, and bloat.
// ---------------------------------------------------------------------------

func (s *Server) handleAuditAgentConfig(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	root := req.GetString("root", "")
	if root == "" {
		if s.indexer != nil {
			root = s.indexer.RootPath()
		}
	}
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	if root == "" {
		return mcp.NewToolResultError("could not determine repo root — pass 'root' argument"), nil
	}

	var files []string
	if filesArg := req.GetString("files", ""); filesArg != "" {
		for _, f := range strings.Split(filesArg, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				files = append(files, f)
			}
		}
	} else {
		files = audit.DiscoverConfigFiles(root)
	}

	if len(files) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"files_scanned": 0,
			"message":       "no agent config files found",
		})
	}

	report := audit.Audit(s.graph, root, files)

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "scanned=%d stale=%d dead=%d bloat=%d\n",
			report.FilesScanned, len(report.StaleRefs), len(report.DeadPaths), report.BloatScore)
		for _, r := range report.StaleRefs {
			fmt.Fprintf(&b, "stale %s:%d `%s`\n", r.File, r.Line, r.Token)
		}
		for _, d := range report.DeadPaths {
			fmt.Fprintf(&b, "dead %s:%d `%s`\n", d.File, d.Line, d.Path)
		}
		for _, f := range report.Files {
			if f.Bloat.Score >= 40 {
				fmt.Fprintf(&b, "bloat %s score=%d lines=%d dup=%d\n",
					f.File, f.Bloat.Score, f.Bloat.Lines, f.Bloat.Duplicates)
			}
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, report)
}
