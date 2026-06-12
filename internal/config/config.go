package config

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/platform"
)

type GuardRule struct {
	Name    string `mapstructure:"name"    yaml:"name"`
	Kind    string `mapstructure:"kind"    yaml:"kind"`              // "co-change" | "boundary"
	Source  string `mapstructure:"source"  yaml:"source"`            // package/path prefix
	Target  string `mapstructure:"target"  yaml:"target"`            // package/path prefix
	Message string `mapstructure:"message" yaml:"message,omitempty"` // human-readable explanation
}

type GuardsConfig struct {
	Rules []GuardRule `mapstructure:"rules" yaml:"rules,omitempty"`
}

// ArchitectureConfig is the declarative architecture-rules block of
// .gortex.yaml — the top-level `architecture:` key. It promotes the
// flat guards: list into named layers with directional allow/deny
// dependency constraints; check_guards evaluates it on every change
// set. The flat guards: list keeps working unchanged alongside it.
type ArchitectureConfig struct {
	// Layers maps a layer name to its definition. A file belongs to
	// the layer whose Paths globs match it — or, when a layer
	// declares no Paths, whose name appears as a path segment.
	Layers map[string]LayerRule `mapstructure:"layers" yaml:"layers,omitempty"`
	// Rules are per-layer / per-pattern dependency-cone constraints —
	// fan-out caps and caller-boundary restrictions — evaluated on
	// top of the layer allow/deny graph.
	Rules []ArchRule `mapstructure:"rules" yaml:"rules,omitempty"`
}

// ArchRule is one architecture constraint scoped to a layer or a file
// glob: a dependency-cone fan-out limit, a caller-boundary
// restriction, or both.
type ArchRule struct {
	// Name is an optional human label surfaced on violations.
	Name string `mapstructure:"name" yaml:"name,omitempty"`
	// Layer scopes the rule to symbols in this architecture layer.
	Layer string `mapstructure:"layer" yaml:"layer,omitempty"`
	// Pattern scopes the rule to symbols whose file matches this glob.
	// When both Layer and Pattern are set a symbol must match both.
	Pattern string `mapstructure:"pattern" yaml:"pattern,omitempty"`
	// MaxFanOut caps the number of distinct symbols a scoped symbol
	// may call or reference. 0 disables the fan-out check.
	MaxFanOut int `mapstructure:"max_fan_out" yaml:"max_fan_out,omitempty"`
	// DenyCallersOutside restricts who may call into the scoped set:
	// every caller's file must match one of these globs. The scoped
	// set is always allowed to call within itself.
	DenyCallersOutside []string `mapstructure:"deny_callers_outside" yaml:"deny_callers_outside,omitempty"`
	// Message is an optional human-readable explanation.
	Message string `mapstructure:"message" yaml:"message,omitempty"`
}

// ArtifactEntry is one row of the `artifacts:` manifest — a non-code
// knowledge file (DB schema, API spec, infra config, ADR) tracked as
// a first-class KindArtifact graph node.
type ArtifactEntry struct {
	// Path is a file path or glob, repo-relative. ** matches any
	// number of directory segments.
	Path string `mapstructure:"path" yaml:"path"`
	// Kind is schema | api | infra | doc. Auto-detected from the file
	// extension when empty.
	Kind string `mapstructure:"kind" yaml:"kind,omitempty"`
	// Name is an optional display name; defaults to the file's base
	// name.
	Name string `mapstructure:"name" yaml:"name,omitempty"`
}

// NamedQuery is one row of the `queries:` block — a named, reusable
// bundle of structural detectors. Running it (`analyze kind=named
// name=<name>`) fans every selected detector across the codebase and
// aggregates the matches. A detector joins the bundle when it carries
// one of the Tags or its name is listed in Detectors.
type NamedQuery struct {
	// Name is the bundle identifier passed to `analyze kind=named`.
	Name string `mapstructure:"name" yaml:"name"`
	// Description is a human-readable summary of the bundle's intent.
	Description string `mapstructure:"description" yaml:"description,omitempty"`
	// Tags selects every detector carrying any of these tags
	// (e.g. injection, secrets, crypto).
	Tags []string `mapstructure:"tags" yaml:"tags,omitempty"`
	// Detectors selects detectors by exact name — for pulling in a
	// specific rule (including a user rule from rule_files).
	Detectors []string `mapstructure:"detectors" yaml:"detectors,omitempty"`
	// Severity is an optional minimum severity floor — info | warning
	// | error. Detectors below it are dropped from the bundle.
	Severity string `mapstructure:"severity" yaml:"severity,omitempty"`
}

// LayerRule defines one architecture layer: the files that belong to
// it and the layers it may or may not depend on.
type LayerRule struct {
	// Paths are globs (with ** matching any number of path segments)
	// selecting the files that belong to this layer. When empty the
	// layer matches files whose path carries the layer name as a
	// segment — so a terse `domain: {allow: [...]}` still works.
	Paths []string `mapstructure:"paths" yaml:"paths,omitempty"`
	// Allow whitelists the layers this layer may depend on; "*"
	// permits any. A non-empty Allow makes every unlisted layer a
	// violation.
	Allow []string `mapstructure:"allow" yaml:"allow,omitempty"`
	// Deny blacklists layers this layer must not depend on; "*"
	// denies every cross-layer dependency not rescued by Allow.
	Deny []string `mapstructure:"deny" yaml:"deny,omitempty"`
}

// IsEmpty reports whether no architecture rules are configured — the
// signal for check_guards to skip the layered evaluation entirely.
func (a ArchitectureConfig) IsEmpty() bool {
	return len(a.Layers) == 0 && len(a.Rules) == 0
}

// MultiRepoConfig holds workspace-discovery settings used by the
// multi-repo bootstrapper. Carries the (formerly `workspace.auto_detect`)
// flag — moved out from under `workspace:` because that key is now
// reclaimed for the workspace-identity slug.
type MultiRepoConfig struct {
	// AutoDetect — when true, `gortex index <parent-dir>` walks
	// immediate subdirectories looking for `.git/`, treating each
	// match as a tracked repo. The legacy YAML key
	// `workspace.auto_detect: true` is still accepted by the custom
	// Config unmarshaller for one release; the canonical key going
	// forward is `multi.auto_detect`.
	AutoDetect bool `mapstructure:"auto_detect" yaml:"auto_detect,omitempty"`
}

// ProjectGlob declares a project's path-globs inside a monorepo.
//
//	projects:
//	  - name: api
//	    paths: ["services/api/**"]
//	  - name: worker
//	    paths: ["services/worker/**"]
//
// A file is assigned to the first project whose globs match (longest
// glob wins on overlap). Files matching no glob get the workspace-
// default project name with a warning at index time.
type ProjectGlob struct {
	Name  string   `mapstructure:"name"  yaml:"name"`
	Paths []string `mapstructure:"paths" yaml:"paths"`
}

// CrossWorkspaceDep declares an explicit, opt-in dependency from this
// workspace into another.
//
//	cross_workspace_deps:
//	  - workspace: gortex
//	    modules: [github.com/gortexhq/gortex]
//	    mode: read-only
//
// `Mode` must be `read-only` in iteration 1 — any other value is
// rejected at config-load time with a clear error.
type CrossWorkspaceDep struct {
	Workspace string   `mapstructure:"workspace" yaml:"workspace"`
	Modules   []string `mapstructure:"modules"   yaml:"modules"`
	Mode      string   `mapstructure:"mode"      yaml:"mode"`
}

// SemanticConfig holds settings for the semantic enrichment layer.
type SemanticConfig struct {
	Enabled           bool                     `mapstructure:"enabled" yaml:"enabled"`
	TimeoutSeconds    int                      `mapstructure:"timeout_seconds" yaml:"timeout_seconds,omitempty"`
	EnrichOnWatch     bool                     `mapstructure:"enrich_on_watch" yaml:"enrich_on_watch,omitempty"`
	WatchDebounceMs   int                      `mapstructure:"watch_debounce_ms" yaml:"watch_debounce_ms,omitempty"`
	RefuteUnconfirmed bool                     `mapstructure:"refute_unconfirmed" yaml:"refute_unconfirmed,omitempty"`
	Providers         []SemanticProviderConfig `mapstructure:"providers" yaml:"providers,omitempty"`
	// AdditionalWorkspaceFolders are extra directory roots passed to
	// every LSP server's `initialize` request as workspace folders.
	// This is how cross-package resolution works for a TypeScript (or
	// any) project that imports from a sibling package living outside
	// the indexed repo root — the language server is told to treat
	// those folders as part of the same workspace.
	AdditionalWorkspaceFolders []string `mapstructure:"additional_workspace_folders" yaml:"additional_workspace_folders,omitempty"`
	// SkipEmbed lists (language, kind) combinations that should be
	// indexed for graph queries but *not* embedded into the vector
	// search. Design tokens (CSS custom properties), terraform
	// resource blocks, YAML/TOML/shell config variables are usually
	// searched by literal name, so paying the embedding + HNSW cost
	// buys nothing. See excludes.DefaultSkipEmbed for the baseline.
	SkipEmbed []SkipEmbedRule `mapstructure:"skip_embed" yaml:"skip_embed,omitempty"`

	// SkipSearch lists (language, kind) combinations that should be
	// kept in the graph but excluded from the text search index
	// (BM25/Bleve). Same shape as SkipEmbed but targets a different
	// index. The motivating case: a big monorepo with ~135k JSON
	// `variable` nodes (package.json keys, tsconfig entries, etc.)
	// pushed total symbol count over search.AutoThreshold and
	// triggered an auto-upgrade from BM25 (~900 B/doc) to Bleve
	// (~32 KiB/doc). Those config-key nodes aren't useful search
	// targets — users who want to find them by name still can via
	// graph queries. Defaults are a superset of SkipEmbed because
	// anything that isn't worth embedding usually isn't worth
	// full-text-indexing either. See DefaultSkipSearch.
	SkipSearch []SkipEmbedRule `mapstructure:"skip_search" yaml:"skip_search,omitempty"`
}

// SkipEmbedRule says: when a node's Language matches Language AND its
// Kind is in Kinds, skip it during vector-index construction.
type SkipEmbedRule struct {
	Language string   `mapstructure:"language" yaml:"language"`
	Kinds    []string `mapstructure:"kinds"    yaml:"kinds"`
}

// ShouldSkipEmbed reports whether a node of the given (language, kind)
// falls under any rule in the list. Matching is case-sensitive and
// exact — parser output is canonical already.
func ShouldSkipEmbed(rules []SkipEmbedRule, language, kind string) bool {
	return matchesSkipRule(rules, language, kind)
}

// ShouldSkipSearch reports whether a node of the given (language, kind)
// falls under any text-index skip rule. Same matching semantics as
// ShouldSkipEmbed — kept as a distinct function so callers make the
// embed/search distinction explicit, and so the two defaults can
// diverge over time.
func ShouldSkipSearch(rules []SkipEmbedRule, language, kind string) bool {
	return matchesSkipRule(rules, language, kind)
}

// matchesSkipRule is the shared (language, kind) matcher for SkipEmbed
// and SkipSearch. Case-sensitive and exact; parser output is canonical.
func matchesSkipRule(rules []SkipEmbedRule, language, kind string) bool {
	for _, r := range rules {
		if r.Language == language && slices.Contains(r.Kinds, kind) {
			return true
		}
	}
	return false
}

// DefaultSkipEmbed returns the compiled-in baseline for which node
// kinds skip embedding. Kept as a function (rather than a var) so
// callers who mutate the returned slice don't affect each other.
func DefaultSkipEmbed() []SkipEmbedRule {
	return []SkipEmbedRule{
		// Design tokens — searched by literal name, not concept.
		{Language: "css", Kinds: []string{"variable", "type"}},
		// Terraform resource/locals/variable blocks — searched
		// literally (aws_vpc.main, module.foo).
		{Language: "hcl", Kinds: []string{"type", "variable"}},
		// Config keys — usually not meaningful prose.
		{Language: "yaml", Kinds: []string{"variable"}},
		{Language: "toml", Kinds: []string{"variable"}},
		// Shell variables are nearly always noise for semantic search.
		{Language: "bash", Kinds: []string{"variable"}},
	}
}

// DefaultSkipSearch returns the baseline (language, kind) pairs that
// are kept out of the text search index. Superset of DefaultSkipEmbed:
// if a node isn't worth a vector slot it generally isn't worth a BM25/
// Bleve slot either, and on big monorepos these config-key nodes are
// what pushes the backend into its Bleve auto-upgrade (~32 KiB/doc).
// JSON is the heaviest of the additions — tsconfig / package.json /
// lockfile keys alone can account for >100k variable nodes.
func DefaultSkipSearch() []SkipEmbedRule {
	rules := DefaultSkipEmbed()
	rules = append(rules,
		// Object keys — searched by exact path, not full-text.
		SkipEmbedRule{Language: "json", Kinds: []string{"variable"}},
		// Template/markup variables — too noisy to index by name.
		SkipEmbedRule{Language: "liquid", Kinds: []string{"variable"}},
		SkipEmbedRule{Language: "jinja", Kinds: []string{"variable"}},
		// Markdown code-block `variable` nodes (one per fenced block)
		// are noise -- searched by literal name, never by concept. The
		// rule is scoped to `variable` ONLY: the first-class KindDoc
		// prose-section nodes (kind "doc") are deliberately left IN the
		// search index so a prose query ranks the right section.
		SkipEmbedRule{Language: "markdown", Kinds: []string{"variable"}},
		// Build-system variables (Makefile/Dockerfile ARG/ENV) are
		// typically searched by literal name, not concept.
		SkipEmbedRule{Language: "makefile", Kinds: []string{"variable"}},
		SkipEmbedRule{Language: "dockerfile", Kinds: []string{"variable"}},
	)
	return rules
}

// SemanticProviderConfig configures a single semantic provider.
type SemanticProviderConfig struct {
	Name        string   `mapstructure:"name" yaml:"name"`
	Command     string   `mapstructure:"command" yaml:"command,omitempty"`
	Args        []string `mapstructure:"args" yaml:"args,omitempty"`
	Languages   []string `mapstructure:"languages" yaml:"languages"`
	Priority    int      `mapstructure:"priority" yaml:"priority,omitempty"`
	Enabled     bool     `mapstructure:"enabled" yaml:"enabled"`
	Mode        string   `mapstructure:"mode" yaml:"mode,omitempty"`
	Daemon      bool     `mapstructure:"daemon" yaml:"daemon,omitempty"`
	MaxParallel int      `mapstructure:"max_parallel" yaml:"max_parallel,omitempty"`
	// Env adds KEY=VALUE environment entries to the provider's
	// subprocess. The motivating case is pinning a JRE for the
	// heavyweight jdtls Java server (env: ["JAVA_HOME=/path/to/jdk"]),
	// but it works for any LSP server that needs a tuned environment.
	// Command / Args / Env set here override the built-in spec.
	Env []string `mapstructure:"env" yaml:"env,omitempty"`
	// Connect, when non-nil, switches this provider from spawning its
	// own LSP subprocess to dialing an already-running endpoint —
	// typically the LSP server the user's IDE is already managing.
	// When set, validation requires non-empty network + address; an
	// empty block is rejected with a clear error.
	Connect *SemanticConnectConfig `mapstructure:"connect" yaml:"connect,omitempty"`
}

// SemanticConnectConfig is the YAML-bound passive-attach block under
// `semantic.providers[*].connect`. Carrier is tcp or unix; address is
// host:port (tcp) or socket path (unix). When fallback_spawn is true,
// a failed dial falls back to spawning the configured subprocess.
type SemanticConnectConfig struct {
	Network       string `mapstructure:"network" yaml:"network"`
	Address       string `mapstructure:"address" yaml:"address"`
	FallbackSpawn bool   `mapstructure:"fallback_spawn" yaml:"fallback_spawn,omitempty"`
}

// Validate reports a clear error for a malformed Connect block — both
// fields empty, or an unsupported network. Returns nil for a nil
// receiver (spawn-mode is fine), and nil for a well-formed block.
func (c *SemanticConnectConfig) Validate() error {
	if c == nil {
		return nil
	}
	if strings.TrimSpace(c.Network) == "" && strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("semantic.providers.connect: network and address are required")
	}
	if strings.TrimSpace(c.Network) == "" {
		return fmt.Errorf("semantic.providers.connect: network is required (tcp or unix)")
	}
	if strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("semantic.providers.connect: address is required")
	}
	switch strings.ToLower(strings.TrimSpace(c.Network)) {
	case "tcp", "tcp4", "tcp6", "unix":
	default:
		return fmt.Errorf("semantic.providers.connect: unsupported network %q (want tcp or unix)", c.Network)
	}
	return nil
}

type Config struct {
	// Exclude is the unified ignore list (gitignore semantics) used by
	// both indexing and watching. Workspace-level patterns are appended
	// to builtin + global + per-RepoEntry layers; use `!pattern` to
	// re-include something an outer layer excluded.
	Exclude []string `mapstructure:"exclude" yaml:"exclude,omitempty"`

	// RuleFiles are paths (or globs) to TOML files of user-defined
	// domain-extractor rules. Each rule is a tree-sitter pattern that
	// becomes a registered detector surfaced through `analyze
	// kind=domain` — the pluggable way to extract HTTP routes, CLI
	// commands, feature flags, i18n keys, event-bus topics, etc.
	RuleFiles []string `mapstructure:"rule_files" yaml:"rule_files,omitempty"`

	// RespectGitignore controls whether the repo's `.gitignore` file is
	// read and its patterns added to the effective exclude list. Default
	// is true (respect .gitignore); set `respect_gitignore: false` in
	// `.gortex.yaml` to opt out for repos that rely on indexing
	// otherwise-ignored generated code or vendored sources. Pointer so
	// the loader can distinguish "explicitly false" from "absent".
	RespectGitignore *bool `mapstructure:"respect_gitignore" yaml:"respect_gitignore,omitempty"`

	// Workspace is the hard-boundary slug this repo belongs to.
	// Top-level `workspace: <slug>` in `.gortex.yaml`. Empty → defaults
	// to the repo name (resolved by the indexer; see
	// resolveWorkspaceID). Two repos with different non-empty slugs
	// have their contract surfaces and queries strictly isolated.
	Workspace string `mapstructure:"workspace" yaml:"workspace,omitempty"`

	// Project is the soft sub-boundary slug for single-project repos.
	// Top-level `project: <slug>`. When `Projects[]` is set
	// (monorepo case) this scalar field is ignored — file-to-project
	// mapping comes from the glob list instead.
	Project string `mapstructure:"project" yaml:"project,omitempty"`

	// Projects is the monorepo per-file project mapping. Top-level
	// `projects: [{name, paths: [globs]}]`. Mutually exclusive with
	// `Project` for clarity; when both are set the loader rejects
	// the config.
	Projects []ProjectGlob `mapstructure:"projects" yaml:"projects,omitempty"`

	// CrossWorkspaceDeps declares opt-in dependencies into other
	// workspaces. Only `mode: read-only` is accepted in iteration 1.
	CrossWorkspaceDeps []CrossWorkspaceDep `mapstructure:"cross_workspace_deps" yaml:"cross_workspace_deps,omitempty"`

	Index  IndexConfig  `mapstructure:"index"    yaml:"index,omitempty"`
	Watch  WatchConfig  `mapstructure:"watch"    yaml:"watch,omitempty"`
	Query  QueryConfig  `mapstructure:"query"    yaml:"query,omitempty"`
	Search SearchConfig `mapstructure:"search"   yaml:"search,omitempty"`
	// Embedding configures the semantic-search vector channel: the
	// embedding provider plus the chunking / concurrency knobs the
	// indexer uses to build the vector index. The vector channel is
	// on by default with the zero-download static GloVe provider.
	Embedding EmbeddingConfig `mapstructure:"embedding" yaml:"embedding,omitempty"`
	MCP       MCPConfig       `mapstructure:"mcp"      yaml:"mcp,omitempty"`
	Guards    GuardsConfig    `mapstructure:"guards"   yaml:"guards,omitempty"`
	// Federation tunes the read-only cross-daemon fan-out:
	// per-remote deadline, global budget, circuit breaker, and the
	// opt-in name-keyed fallback. Zero values fall back to defaults.
	Federation FederationConfig `mapstructure:"federation" yaml:"federation,omitempty"`
	// Architecture is the declarative layer / allow-deny DSL. Empty
	// by default; the flat Guards list above keeps working when it is
	// unset.
	Architecture ArchitectureConfig `mapstructure:"architecture" yaml:"architecture,omitempty"`
	// Artifacts is the non-code knowledge manifest — schemas, API
	// specs, infra configs, and ADRs surfaced as KindArtifact nodes.
	Artifacts []ArtifactEntry `mapstructure:"artifacts" yaml:"artifacts,omitempty"`
	// Queries are named, reusable detector bundles runnable via
	// `analyze kind=named`.
	Queries  []NamedQuery    `mapstructure:"queries" yaml:"queries,omitempty"`
	Multi    MultiRepoConfig `mapstructure:"multi"    yaml:"multi,omitempty"`
	Semantic SemanticConfig  `mapstructure:"semantic" yaml:"semantic,omitempty"`
	// LLM configures the LLM service that backs the `ask` MCP tool and
	// the search-assist passes. Empty by default — daemon skips LLM
	// wiring entirely when the active provider has no model configured.
	// The `llm.provider` key selects the backend (local / anthropic /
	// openai / ollama / claudecli); env vars GORTEX_LLM_* override file
	// values; see internal/llm/config.go::Config.MergeEnv.
	LLM llm.Config `mapstructure:"llm" yaml:"llm,omitempty"`
	// Review tunes the PR-review surface: the layered path-glob rule
	// list, the gating thresholds applied to surfaced findings, the
	// depth-selection bounds, and the posting gate. Empty by default;
	// the embedded default rules apply when no rule is configured.
	Review ReviewConfig `mapstructure:"review" yaml:"review,omitempty"`
}

// ReviewConfig is the `review:` block. It carries every knob the
// PR-review layers read: the path-glob rule list (resolved by
// review.RuleResolver), the gating thresholds applied to surfaced
// findings, the depth-selection bounds, and the comment-posting gate.
// The whole struct lands here so later layers only read these fields.
type ReviewConfig struct {
	// Rules is the ordered path-glob rule list. The first rule whose
	// glob matches a changed file selects its severity floor and
	// rulepack; an empty list falls through to the embedded defaults.
	Rules []ReviewRule `mapstructure:"rules" yaml:"rules,omitempty"`
	// MinConfidence drops any finding scored below this confidence
	// (0..1). Zero keeps every finding.
	MinConfidence float64 `mapstructure:"min_confidence" yaml:"min_confidence,omitempty"`
	// MinSeverity is the minimum severity floor — info | warning |
	// error | critical. Findings below it are dropped. Empty keeps all.
	MinSeverity string `mapstructure:"min_severity" yaml:"min_severity,omitempty"`
	// Categories, when set, restricts surfaced findings to these
	// categories. Empty keeps every category.
	Categories []string `mapstructure:"categories" yaml:"categories,omitempty"`
	// MaxFindings caps how many findings are surfaced. Zero is no cap.
	MaxFindings int `mapstructure:"max_findings" yaml:"max_findings,omitempty"`
	// QuickMaxLines is the changed-line ceiling under which a review
	// runs in the quick (shallow) mode. Zero uses the built-in default.
	QuickMaxLines int `mapstructure:"quick_max_lines" yaml:"quick_max_lines,omitempty"`
	// DeepMinLines is the changed-line floor at or above which a review
	// escalates to the deep mode. Zero uses the built-in default.
	DeepMinLines int `mapstructure:"deep_min_lines" yaml:"deep_min_lines,omitempty"`
	// DeepMinFiles is the changed-file floor at or above which a review
	// escalates to the deep mode. Zero uses the built-in default.
	DeepMinFiles int `mapstructure:"deep_min_files" yaml:"deep_min_files,omitempty"`
	// Post gates comment posting.
	Post ReviewPostConfig `mapstructure:"post" yaml:"post,omitempty"`
}

// ReviewPostConfig gates how review findings are posted back. Posting
// to a public or forked repository is opt-in.
type ReviewPostConfig struct {
	// AllowPublic permits posting comments on public / fork repos.
	// Off by default so a misconfigured token never leaks comments.
	AllowPublic bool `mapstructure:"allow_public" yaml:"allow_public,omitempty"`
}

// ReviewRule is one path-glob review rule. Path is a gitignore-style
// glob (so `**` works); the first rule whose Path matches a changed
// file selects its Severity floor and Rulepack. A Disabled rule is
// skipped during resolution.
type ReviewRule struct {
	// Name is the human-readable rule identifier.
	Name string `mapstructure:"name" yaml:"name,omitempty"`
	// Path is the gitignore-style glob the changed file is matched
	// against (e.g. `**/*_test.go`, `internal/auth/**`, `**`).
	Path string `mapstructure:"path" yaml:"path,omitempty"`
	// Severity is an optional severity floor for findings grounded to
	// a file this rule selects — info | warning | error | critical.
	Severity string `mapstructure:"severity" yaml:"severity,omitempty"`
	// Rulepack names the detector bundle to run for a matched file.
	Rulepack string `mapstructure:"rulepack" yaml:"rulepack,omitempty"`
	// Disabled skips this rule entirely during resolution.
	Disabled bool `mapstructure:"disabled" yaml:"disabled,omitempty"`
}

type IndexConfig struct {
	Languages []string `mapstructure:"languages" yaml:"languages,omitempty"`
	// Exclude is deprecated — use top-level Config.Exclude instead.
	// Still read for one release so existing .gortex.yaml files don't
	// silently stop working; merged into the unified list by ConfigManager.
	Exclude []string `mapstructure:"exclude" yaml:"exclude,omitempty"`
	Workers int      `mapstructure:"workers" yaml:"workers,omitempty"`
	// SkipEmbed is the effective skip-embedding rules resolved from
	// Semantic.SkipEmbed. Not part of the on-disk YAML schema — it's
	// populated by ConfigManager.GetRepoConfig so the indexer gets it
	// through the same struct it already receives. Surface it to users
	// under semantic.skip_embed, not under index.
	SkipEmbed []SkipEmbedRule `mapstructure:"-" yaml:"-"`
	// SkipSearch is the effective text-index skip rules resolved from
	// Semantic.SkipSearch, same propagation pattern as SkipEmbed.
	// Users configure this under semantic.skip_search; the indexer
	// reads it here. Controls what goes into BM25/Bleve — unlike
	// SkipEmbed it doesn't affect the graph or vector index.
	SkipSearch []SkipEmbedRule `mapstructure:"-" yaml:"-"`
	// IndexProse is the effective prose-indexing toggle resolved from
	// Search.IndexProse -- same `-` (not on-disk) propagation pattern
	// as SkipSearch. When false, Markdown KindDoc prose-section nodes
	// are kept out of the BM25 search index. Defaults to true.
	IndexProse bool `mapstructure:"-" yaml:"-"`
	// MaxFileSize skips files larger than this during indexing. Zero
	// (the default) disables the cap — full coverage is preferred so
	// generated code like `*.pb.go`, schema files, and large data
	// constants stay queryable. Users with very heavy generated /
	// minified files that dominate parse time can set a cap (e.g.
	// 2 MiB) via `.gortex.yaml` to trade coverage for speed. A cap
	// that drops real symbols silently is a worse default than a
	// slightly slower full index.
	MaxFileSize int64 `mapstructure:"max_file_size" yaml:"max_file_size,omitempty"`
	// IndexMinified controls whether bundled / minified build
	// artifacts are indexed. Off by default: a minified bundle or a
	// sourcemap is synthetic source — it has no meaningful symbols and
	// only pollutes the graph and search results — so it is detected by
	// content and skipped with a synthetic telemetry node. Set true to
	// index such files anyway. Configured under `index.index_minified`.
	IndexMinified bool `mapstructure:"index_minified" yaml:"index_minified,omitempty"`
	// CrashIsolation runs tree-sitter extraction in worker
	// subprocesses so a grammar SIGSEGV / OOM / hang on one
	// pathological file is contained: the bad file is quarantined
	// with Meta["parse_error"] and the index pass still completes.
	// Off by default — the subprocess round-trip adds measurable
	// overhead and an in-process index is the common case. Opt in via
	// `.gortex.yaml::index::crash_isolation: true` or the
	// GORTEX_PARSER_ISOLATION=1 environment override.
	CrashIsolation bool `mapstructure:"crash_isolation" yaml:"crash_isolation,omitempty"`
	// Merkle switches incremental re-index change detection from
	// per-file mtime comparison to a BLAKE3 Merkle tree. The tree is
	// content-addressed, so a file touched without a content change is
	// not re-indexed, and an unchanged subtree is skipped wholesale.
	// Off by default — mtime detection is cheaper when most touches
	// are real edits. Opt in via `.gortex.yaml::index::merkle: true`
	// or the GORTEX_MERKLE=1 environment override.
	Merkle bool `mapstructure:"merkle" yaml:"merkle,omitempty"`
	// MaxExtractMillis caps how long a single file's extraction may
	// run. A file that exceeds the budget is skipped with a synthetic
	// node carrying Meta["skipped_due_to_timeout"] instead of stalling
	// the pass — the microsoft/TypeScript reallyLargeFile.ts class of
	// pathological input. Zero (the default) disables the cap, for the
	// same full-coverage reason MaxFileSize defaults off. Set
	// generously (e.g. 10000) so only genuinely pathological files
	// trip it. In crash-isolation mode it also bounds the worker
	// round-trip.
	MaxExtractMillis int `mapstructure:"max_extract_millis" yaml:"max_extract_millis,omitempty"`
	// SynthesizeExternalCalls turns a call into an un-indexed third
	// party (an npm/pip/cargo/maven/nuget package, or a sibling
	// microservice) into an explicit graph terminal. Without it such a
	// call edge stays an `unresolved::` placeholder with no destination
	// node, and a call-chain walk silently stops there — losing the fact
	// that the function reaches out to an external system at all. When
	// on, the resolver synthesises a clearly-marked synthetic node per
	// (ecosystem, import path) and retargets the call edge to it, so
	// `get_call_chain` / `get_callers` keep the external hop visible and
	// every call into the same package shares one stable, cross-repo
	// identity node — `analyze kind=external_calls` then aggregates a
	// service's whole external surface across repositories.
	//
	// Tri-state, default ON: a nil pointer (the key absent) resolves to
	// enabled via ExternalCallSynthesisEnabledOrDefault, so external-call
	// qualification is automatic for every language whose extractor lands
	// calls on a dep/stdlib/external terminal (Go/Rust/Java/Python/C#/TS).
	// Affordable by default because synthesis is incremental on the hot
	// reindex path (only the edited file's terminals are re-materialised)
	// and the disk backend pushes candidate-edge selection down to a
	// partial-indexed query. Opt out per-repo with
	// `.gortex.yaml::index::synthesize_external_calls: false` or the
	// GORTEX_SYNTH_EXTERNAL_CALLS=0 environment override; language /
	// stdlib calls are filtered out regardless so the synthetic nodes
	// only ever name genuine third-party packages.
	SynthesizeExternalCalls *bool `mapstructure:"synthesize_external_calls" yaml:"synthesize_external_calls,omitempty"`
	// SynthesizeSpeculativeDispatch mints opt-in, best-guess `calls` edges for
	// dynamic-dispatch blind spots (computed-member calls obj["foo"](),
	// getattr, decorator registries). Unlike external-call synthesis it
	// DEFAULTS OFF (tri-state, nil = off) because the edges are heuristic
	// fan-outs; when on they ride a strictly-lower confidence tier and are
	// hidden from every default query (opt in per query with
	// include_speculative, or audit via `analyze kind=speculative`). Enable
	// per-repo with `.gortex.yaml::index::synthesize_speculative_dispatch:
	// true` or the GORTEX_SYNTH_SPECULATIVE=1 environment override.
	SynthesizeSpeculativeDispatch *bool `mapstructure:"synthesize_speculative_dispatch" yaml:"synthesize_speculative_dispatch,omitempty"`
	// ScopedGlobalPasses scopes the global inference passes (InferImplements /
	// InferOverrides) on incremental reindex to only the types/interfaces a
	// change can affect, instead of re-running the whole-graph type×interface
	// cross product on every single-file edit. Add-parity with the full pass
	// (it re-derives exactly the edges eviction dropped). Tri-state, DEFAULT
	// ON. Set false (or GORTEX_INDEX_SCOPED_GLOBAL_PASSES=0) to restore the
	// whole-graph behaviour.
	ScopedGlobalPasses *bool `mapstructure:"scoped_global_passes" yaml:"scoped_global_passes,omitempty"`
	// Transforms are pluggable pre-ingestion content processors. Each
	// rewrites a matching file's bytes before the parser sees them —
	// expanding minified bundles, normalising SVG/TOON, converting a
	// PDF to markdown, etc. Empty by default.
	Transforms []TransformRule `mapstructure:"transforms" yaml:"transforms,omitempty"`
	// Grammars registers user-supplied tree-sitter grammars — drop-in
	// languages Gortex was not compiled with. Each entry points at a
	// compiled grammar shared object (.so / .dylib / .dll); see
	// GrammarSpec. Empty by default. Configured under `index.grammars`
	// in .gortex.yaml.
	Grammars []GrammarSpec `mapstructure:"grammars" yaml:"grammars,omitempty"`
	// Coverage gates the per-domain coverage extractors (todos,
	// licenses, ownership, function shape, etc.). Each sub-block has
	// its own default; an empty Coverage block means "use the
	// documented per-domain defaults" — cheap structural domains on,
	// expensive ones off.
	Coverage CoverageConfig `mapstructure:"coverage" yaml:"coverage,omitempty"`
	// HTTPClientAliases names project-defined wrapped HTTP-client
	// functions so calls to them are recognised as HTTP consumer
	// contracts (RoleConsumer) — without relying on the type-driven
	// fetch/axios heuristics. Each entry is a bare function name (or
	// receiver.method) that forwards a string-literal path to an
	// underlying client, e.g. `apiGet`, `apiPost`, `client.request`.
	// The contracts HTTP extractor mints an `http::METHOD::/path`
	// consumer contract for every call to one of these names. The
	// method is taken from the name's verb suffix (`apiGet` → GET,
	// `apiPost` → POST, ...) when present, falling back to the first
	// string-literal argument when the call passes the method
	// explicitly (`apiCall('GET', '/users')`); the path is the first
	// path-shaped string literal. Empty by default. Configured under
	// `index.http_client_aliases` in .gortex.yaml.
	HTTPClientAliases []string `mapstructure:"http_client_aliases" yaml:"http_client_aliases,omitempty"`
	// ExtractorPlugins registers external post-parse extractor passes —
	// the "add a custom extractor without forking Gortex" entry point.
	// Each spec names a language + file extensions and an external
	// command; matching files are piped (content on stdin,
	// GORTEX_FILE_PATH in the env) to that command, which emits a JSON
	// document of nodes and edges that Gortex folds into the graph. See
	// ExtractorPluginSpec. Empty by default. Configured under
	// `index.extractor_plugins` in .gortex.yaml.
	ExtractorPlugins []ExtractorPluginSpec `mapstructure:"extractor_plugins" yaml:"extractor_plugins,omitempty"`
	// FallbackChunkers registers declarative regex chunkers for
	// languages Gortex has no tree-sitter grammar for — the "give a
	// grammar-less language a coarse outline without writing Go" entry
	// point. Each spec names a language + file extensions and a list of
	// regex patterns; matching files yield one node per pattern match
	// plus an EdgeDefines from the file. See FallbackChunkerSpec. Empty
	// by default. Configured under `index.fallback_chunkers` in
	// .gortex.yaml.
	FallbackChunkers []FallbackChunkerSpec `mapstructure:"fallback_chunkers" yaml:"fallback_chunkers,omitempty"`

	// EventBus declares producer/consumer boundaries for a bespoke event
	// bus / SSE flow purely in config — no code change. Each spec matches
	// call sites / decorators / interface bases and Gortex synthesizes real
	// traversable provider↔consumer contract pairs (with dispatch guards)
	// for the bus, so find_usages / trace_path / analyze pubsub all work on
	// it. Empty by default. Configured under `index.event_bus` in
	// .gortex.yaml; the CODEGRAPH_EVENT_CONFIG env var (a JSON list)
	// overrides it for drop-in compatibility with pygraph/tsgraph.
	EventBus []EventBusBoundarySpec `mapstructure:"event_bus" yaml:"event_bus,omitempty"`
}

// EventBusBoundarySpec declares one producer or consumer boundary of a
// user-defined event bus / SSE flow. The schema is language-neutral and
// drop-in compatible with pygraph/tsgraph's CODEGRAPH_EVENT_CONFIG entries.
type EventBusBoundarySpec struct {
	// Name groups producers and consumers of the same logical bus. A
	// producer and consumer with the same Name + topic pair into an edge.
	Name string `mapstructure:"name" yaml:"name"`
	// Type is "producer" or "consumer".
	Type string `mapstructure:"type" yaml:"type"`
	// Callee is a dotted call suffix matched against producer/consumer call
	// sites (e.g. "bus.publish", "producer.send"). Matched as a suffix so
	// "self.producer.send(" matches "producer.send".
	Callee string `mapstructure:"callee" yaml:"callee,omitempty"`
	// CalleePattern is a looser substring match on the callee for SSE / hook
	// forms (e.g. "EventSource", "useEventStream"). Alias: HookPattern.
	CalleePattern string `mapstructure:"callee_pattern" yaml:"callee_pattern,omitempty"`
	// HookPattern is an alias for CalleePattern (tsgraph compatibility).
	HookPattern string `mapstructure:"hook_pattern" yaml:"hook_pattern,omitempty"`
	// Decorator names a decorator that marks the decorated function as a
	// consumer (e.g. "kafka_consumer"); the topic is read from its args.
	Decorator string `mapstructure:"decorator" yaml:"decorator,omitempty"`
	// Interface names a class base; every method of a class extending it is
	// a consumer (paired on the bus wildcard topic).
	Interface string `mapstructure:"interface" yaml:"interface,omitempty"`
	// TopicArg names the argument carrying the topic / url that keys the
	// producer↔consumer join: a positional index ("0") or a kwarg name
	// ("topic"). Defaults to "topic".
	TopicArg string `mapstructure:"topic_arg" yaml:"topic_arg,omitempty"`
	// Guards, when true, extracts the consumer's first if/elif dispatch
	// chain (field==value rules) onto the contract for routing queries.
	Guards bool `mapstructure:"guards" yaml:"guards,omitempty"`
}

// GrammarSpec declares a user-supplied tree-sitter grammar to load at
// startup — a drop-in language Gortex was not compiled with. The
// grammar must be a compiled shared library (.so / .dylib / .dll)
// exporting the standard tree-sitter `tree_sitter_<language>` entry
// point, built against a compatible tree-sitter ABI.
type GrammarSpec struct {
	// Language is the name the extractor registers under. A name that
	// collides with a built-in extractor is skipped — built-ins win.
	Language string `mapstructure:"language" yaml:"language"`
	// Library is the path to the compiled grammar shared object. A
	// relative path resolves against the working directory.
	Library string `mapstructure:"library" yaml:"library"`
	// Symbol overrides the C entry-point symbol. Defaults to
	// `tree_sitter_<language>` with non-alphanumerics folded to `_`.
	Symbol string `mapstructure:"symbol" yaml:"symbol,omitempty"`
	// Extensions are the file extensions (leading dot) the grammar
	// claims, e.g. [".foo", ".foobar"]. An extension already claimed
	// by a built-in extractor is skipped.
	Extensions []string `mapstructure:"extensions" yaml:"extensions"`
}

// TransformRule declares a pluggable pre-ingestion content transform.
// Before a matching file is parsed, its bytes are piped through the
// command (file content on stdin, transformed content on stdout). This
// is how non-native inputs reach the graph — minified bundles expanded,
// SVG/TOON normalised, a PDF converted to markdown, and so on.
type TransformRule struct {
	// Name identifies the transform in logs.
	Name string `mapstructure:"name" yaml:"name"`
	// Extensions are the file extensions (with the dot, e.g. ".svg")
	// this transform applies to. Empty means every file.
	Extensions []string `mapstructure:"extensions" yaml:"extensions,omitempty"`
	// Command is the transform program as argv. The file content is
	// written to its stdin; its stdout replaces the content.
	Command []string `mapstructure:"command" yaml:"command"`
	// AsLanguage, when set, indexes matching files as this language
	// even if their extension is not natively recognised — a rule with
	// extensions [".pdf"] + as_language "markdown" lets a PDF→markdown
	// command feed the markdown extractor.
	AsLanguage string `mapstructure:"as_language" yaml:"as_language,omitempty"`
	// TimeoutMillis bounds the transform subprocess; 0 uses a default.
	TimeoutMillis int `mapstructure:"timeout_millis" yaml:"timeout_millis,omitempty"`
}

// ExtractorPluginSpec declares an external post-parse extractor pass —
// the config-driven SPI for adding a custom extractor without forking
// Gortex. Files whose extension matches are piped (content on stdin,
// GORTEX_FILE_PATH carrying the path in the env) to the command, which
// must emit a JSON document of the shape:
//
//	{"nodes":[{"id","kind","name","file_path","start_line","end_line",
//	           "language","meta":{...}}],
//	 "edges":[{"from","to","kind","file_path","line","meta":{...}}]}
//
// Each node whose Kind is a valid graph node kind becomes a graph.Node;
// each edge becomes a graph.Edge. The owning file node is always
// emitted. A non-zero exit, malformed JSON, or a timeout degrades to
// just the file node (the index never fails). A language or extension
// that collides with a built-in extractor is skipped — built-ins win.
type ExtractorPluginSpec struct {
	// Language is the name the extractor registers under. A name that
	// collides with a built-in extractor is skipped.
	Language string `mapstructure:"language" yaml:"language"`
	// Extensions are the file extensions (leading dot) the plugin
	// claims, e.g. [".foo"]. An extension already claimed by a built-in
	// extractor is skipped.
	Extensions []string `mapstructure:"extensions" yaml:"extensions"`
	// Command is the plugin program (argv[0]). The file content is
	// written to its stdin; GORTEX_FILE_PATH names the file in the env.
	Command string `mapstructure:"command" yaml:"command"`
	// Args are the trailing arguments passed to Command.
	Args []string `mapstructure:"args" yaml:"args,omitempty"`
	// TimeoutMs bounds the plugin subprocess; 0 uses a default.
	TimeoutMs int `mapstructure:"timeout_ms" yaml:"timeout_ms,omitempty"`
}

// FallbackChunkerSpec declares a declarative regex chunker for a
// grammar-less language — the config-driven SPI for giving a language
// Gortex has no tree-sitter grammar a coarse symbol outline without
// writing Go. Files whose extension matches are scanned with each
// pattern; every match emits a node of the pattern's kind (validated
// against the graph node kinds) plus an EdgeDefines from the file. A
// language or extension that collides with a built-in extractor is
// skipped — built-ins win.
type FallbackChunkerSpec struct {
	// Language is the name the chunker registers under. A name that
	// collides with a built-in extractor is skipped.
	Language string `mapstructure:"language" yaml:"language"`
	// Extensions are the file extensions (leading dot) the chunker
	// claims. An extension already claimed by a built-in extractor is
	// skipped.
	Extensions []string `mapstructure:"extensions" yaml:"extensions"`
	// Patterns are the regex rules applied to a matching file's content.
	Patterns []ChunkPattern `mapstructure:"patterns" yaml:"patterns"`
}

// ChunkPattern is a single regex rule of a FallbackChunkerSpec. Each
// match yields one node of Kind named by the NameGroup-th capture.
type ChunkPattern struct {
	// Kind is the graph node kind to emit (e.g. "function", "type").
	// A kind that is not a valid graph node kind is skipped.
	Kind string `mapstructure:"kind" yaml:"kind"`
	// Regex is the (Go-syntax) pattern matched against the file content.
	// A pattern that fails to compile skips the whole chunker spec.
	Regex string `mapstructure:"regex" yaml:"regex"`
	// NameGroup is the capture-group index whose text names the node.
	// Zero (the default) uses group 1 — the first parenthesised group.
	NameGroup int `mapstructure:"name_group" yaml:"name_group,omitempty"`
}

// CoverageConfig collects the per-domain coverage extraction gates.
// Each sub-block is independently shippable and gated.
//
// Defaults (per-domain Enabled value when not set in YAML):
//
//   - FunctionShape: true  (params/returns/generics/closures — Phase 1)
//   - Concurrency:   true  (closures already covered by FunctionShape;
//     this gates EdgeSpawns / channel I/O)
//   - Constants:     true  (cheap, structural)
//   - TypeShape:     true  (aliases vs newtypes vs composition)
//   - Codegen:       true  (// Code generated markers)
//   - Todos:         true  (comment scanner)
//   - Ownership:     true  (CODEOWNERS — only emits when file present)
//   - Licenses:      true  (SPDX header scan)
//   - Modules:       true  (lockfile parse — cheap)
//   - Configs:       false (recognizers per provider; opt-in)
//   - Flags:         false (auto-on if a provider client is detected)
//   - Observability: false (logging/metric/trace event names)
//   - Pubsub:        false (event pub/sub publish/subscribe edges)
//   - SQL:           false (Phase 3 — noisy and slow)
//   - Fixtures:      true  (testdata path detection)
//   - CrossLanguage: false (cgo / wasm-bindgen)
//   - Clones:        true  (MinHash + LSH near-duplicate detection)
//
// Setting any leaf Enabled explicitly overrides the default.
type CoverageConfig struct {
	FunctionShape DomainToggle `mapstructure:"function_shape" yaml:"function_shape,omitempty"`
	Concurrency   DomainToggle `mapstructure:"concurrency"    yaml:"concurrency,omitempty"`
	Constants     DomainToggle `mapstructure:"constants"      yaml:"constants,omitempty"`
	TypeShape     DomainToggle `mapstructure:"type_shape"     yaml:"type_shape,omitempty"`
	Codegen       DomainToggle `mapstructure:"codegen"        yaml:"codegen,omitempty"`
	Todos         TodoConfig   `mapstructure:"todos"          yaml:"todos,omitempty"`
	Ownership     DomainToggle `mapstructure:"ownership"      yaml:"ownership,omitempty"`
	Licenses      DomainToggle `mapstructure:"licenses"       yaml:"licenses,omitempty"`
	Modules       DomainToggle `mapstructure:"modules"        yaml:"modules,omitempty"`
	Configs       DomainToggle `mapstructure:"configs"        yaml:"configs,omitempty"`
	Flags         FlagConfig   `mapstructure:"flags"          yaml:"flags,omitempty"`
	Observability DomainToggle `mapstructure:"observability"  yaml:"observability,omitempty"`
	Pubsub        DomainToggle `mapstructure:"pubsub"         yaml:"pubsub,omitempty"`
	SQL           SQLConfig    `mapstructure:"sql"            yaml:"sql,omitempty"`
	Fixtures      DomainToggle `mapstructure:"fixtures"       yaml:"fixtures,omitempty"`
	CrossLanguage DomainToggle `mapstructure:"cross_language" yaml:"cross_language,omitempty"`
	Clones        ClonesConfig `mapstructure:"clones"         yaml:"clones,omitempty"`
}

// ClonesConfig gates MinHash + LSH near-duplicate ("clone") detection.
// When enabled, the indexer stamps a 64-slot MinHash signature on every
// substantial function/method node at parse time and a graph-wide pass
// emits EdgeSimilarTo edges between bodies whose estimated Jaccard
// similarity is at or above Threshold. Default-on: signature
// computation is a cheap per-function tokenise + hash, and the LSH pass
// is near-linear.
type ClonesConfig struct {
	Enabled *bool `mapstructure:"enabled" yaml:"enabled,omitempty"`
	// Threshold is the Jaccard similarity at or above which a candidate
	// pair is recorded as a clone. Zero (unset) falls back to the
	// clones package default (0.82). Values are clamped to (0, 1].
	Threshold float64 `mapstructure:"threshold" yaml:"threshold,omitempty"`
}

// DomainToggle is the minimal config for a domain whose only knob is
// on/off plus an optional language allow-list. The Enabled field uses
// a tri-state pointer so we can distinguish "user set false" from
// "user did not set" (and apply the per-domain default for the
// latter). The language list, when non-empty, restricts extraction to
// the listed source languages.
type DomainToggle struct {
	Enabled   *bool    `mapstructure:"enabled"   yaml:"enabled,omitempty"`
	Languages []string `mapstructure:"languages" yaml:"languages,omitempty"`
}

// TodoConfig configures the TODO scanner. Tags is the set of
// recognised marker tokens (default: TODO, FIXME, HACK, XXX, NOTE);
// MaxText caps the stored text length (default 200) so a wall of
// commented-out code in a TODO doesn't bloat the graph.
type TodoConfig struct {
	Enabled *bool    `mapstructure:"enabled"  yaml:"enabled,omitempty"`
	Tags    []string `mapstructure:"tags"     yaml:"tags,omitempty"`
	MaxText int      `mapstructure:"max_text" yaml:"max_text,omitempty"`
}

// FlagConfig configures feature-flag recognition. Recognizers is a
// list of (provider, function-name) pairs to treat as flag checks;
// the built-in recognizers cover GrowthBook, LaunchDarkly, Unleash by
// default.
type FlagConfig struct {
	Enabled     *bool            `mapstructure:"enabled"     yaml:"enabled,omitempty"`
	Recognizers []FlagRecognizer `mapstructure:"recognizers" yaml:"recognizers,omitempty"`
}

// FlagRecognizer maps a fully-qualified function name to a flag
// provider and operation. Op ∈ read|write|register.
type FlagRecognizer struct {
	Provider string `mapstructure:"provider" yaml:"provider"`
	Func     string `mapstructure:"func"     yaml:"func"`
	Op       string `mapstructure:"op"       yaml:"op,omitempty"`
}

// SQLConfig gates SQL schema extraction. Default-off because SQL
// parsing is slow and string-literal SQL produces false positives.
type SQLConfig struct {
	Enabled        *bool         `mapstructure:"enabled"         yaml:"enabled,omitempty"`
	Dialect        string        `mapstructure:"dialect"         yaml:"dialect,omitempty"`
	Migrations     SQLMigrations `mapstructure:"migrations"      yaml:"migrations,omitempty"`
	ORM            SQLOrm        `mapstructure:"orm"             yaml:"orm,omitempty"`
	StringLiterals SQLStringLits `mapstructure:"string_literals" yaml:"string_literals,omitempty"`
}

type SQLMigrations struct {
	Paths  []string `mapstructure:"paths"  yaml:"paths,omitempty"`
	Format string   `mapstructure:"format" yaml:"format,omitempty"`
}

type SQLOrm struct {
	Detect []string `mapstructure:"detect" yaml:"detect,omitempty"`
}

type SQLStringLits struct {
	Enabled       *bool  `mapstructure:"enabled"        yaml:"enabled,omitempty"`
	MinConfidence string `mapstructure:"min_confidence" yaml:"min_confidence,omitempty"`
}

// coverageDomainDefault is the per-domain default that applies when a
// user has not explicitly set Enabled in their YAML. Cheap structural
// domains default on; expensive or noisy domains default off. See the
// CoverageConfig doc comment for the canonical list.
var coverageDomainDefault = map[string]bool{
	"function_shape": true,
	"concurrency":    true,
	"constants":      true,
	"type_shape":     true,
	"codegen":        true,
	"todos":          true,
	"ownership":      true,
	"licenses":       true,
	"modules":        true,
	"configs":        false,
	"flags":          false,
	"observability":  false,
	"pubsub":         false,
	"sql":            false,
	"fixtures":       true,
	"cross_language": false,
	"clones":         true,
}

// resolveDomainEnabled returns the effective Enabled flag for a
// domain — explicit YAML setting if present, else the documented
// default. Used by IsCoverageEnabled and the per-domain accessors.
func resolveDomainEnabled(explicit *bool, domain string) bool {
	if explicit != nil {
		return *explicit
	}
	return coverageDomainDefault[domain]
}

// IsCoverageEnabled reports whether a coverage domain's extractor
// should run for this Config. Pass a domain string from the
// CoverageConfig YAML field names: "function_shape", "todos",
// "licenses", etc. Unknown domain strings return false.
func (c *Config) IsCoverageEnabled(domain string) bool {
	return c.Index.Coverage.IsEnabled(domain)
}

// IsEnabled is the IndexConfig-level form of IsCoverageEnabled. The
// indexer holds an IndexConfig directly (not the parent Config) so
// the gate accessor lives here too — keeps the call-site short and
// avoids passing the whole Config around for one boolean check.
func (cov CoverageConfig) IsEnabled(domain string) bool {
	switch domain {
	case "function_shape":
		return resolveDomainEnabled(cov.FunctionShape.Enabled, domain)
	case "concurrency":
		return resolveDomainEnabled(cov.Concurrency.Enabled, domain)
	case "constants":
		return resolveDomainEnabled(cov.Constants.Enabled, domain)
	case "type_shape":
		return resolveDomainEnabled(cov.TypeShape.Enabled, domain)
	case "codegen":
		return resolveDomainEnabled(cov.Codegen.Enabled, domain)
	case "todos":
		return resolveDomainEnabled(cov.Todos.Enabled, domain)
	case "ownership":
		return resolveDomainEnabled(cov.Ownership.Enabled, domain)
	case "licenses":
		return resolveDomainEnabled(cov.Licenses.Enabled, domain)
	case "modules":
		return resolveDomainEnabled(cov.Modules.Enabled, domain)
	case "configs":
		return resolveDomainEnabled(cov.Configs.Enabled, domain)
	case "flags":
		return resolveDomainEnabled(cov.Flags.Enabled, domain)
	case "observability":
		return resolveDomainEnabled(cov.Observability.Enabled, domain)
	case "pubsub":
		return resolveDomainEnabled(cov.Pubsub.Enabled, domain)
	case "sql":
		return resolveDomainEnabled(cov.SQL.Enabled, domain)
	case "fixtures":
		return resolveDomainEnabled(cov.Fixtures.Enabled, domain)
	case "cross_language":
		return resolveDomainEnabled(cov.CrossLanguage.Enabled, domain)
	case "clones":
		return resolveDomainEnabled(cov.Clones.Enabled, domain)
	}
	return false
}

// ClonesThreshold returns the configured Jaccard similarity threshold
// for clone detection, or 0 when the user has not set one (callers
// then fall back to the clones package default). Out-of-range values
// are clamped to (0, 1].
func (cov CoverageConfig) ClonesThreshold() float64 {
	t := cov.Clones.Threshold
	if t <= 0 {
		return 0
	}
	if t > 1 {
		return 1
	}
	return t
}

// TodoTags returns the configured TODO tag set, or the default set
// when the user has not provided one. The defaults match the spec
// (TODO, FIXME, HACK, XXX, NOTE).
func (c *Config) TodoTags() []string {
	if tags := c.Index.Coverage.Todos.Tags; len(tags) > 0 {
		return tags
	}
	return []string{"TODO", "FIXME", "HACK", "XXX", "NOTE"}
}

// TodoMaxText returns the configured cap on stored TODO text, or the
// default of 200 characters.
func (c *Config) TodoMaxText() int {
	if n := c.Index.Coverage.Todos.MaxText; n > 0 {
		return n
	}
	return 200
}

type WatchConfig struct {
	Enabled    bool     `mapstructure:"enabled"     yaml:"enabled,omitempty"`
	Paths      []string `mapstructure:"paths"       yaml:"paths,omitempty"`
	DebounceMs int      `mapstructure:"debounce_ms" yaml:"debounce_ms,omitempty"`
	// Exclude is deprecated — use top-level Config.Exclude instead.
	// Kept for one release as a fallback merged into the unified list.
	Exclude []string `mapstructure:"exclude" yaml:"exclude,omitempty"`

	// StormThreshold — when more than this many events arrive within
	// StormWindowMs, the watcher switches from per-file debounced
	// patching to a batched reconcile that defers resolver + search
	// rebuild until a quiet period has passed. Protects against event
	// floods from bulk operations: `rsync`, `npm install`, branch
	// checkout, bulk format-on-save, find-and-replace across a repo.
	// Zero disables storm mode (pure per-file behaviour).
	StormThreshold int `mapstructure:"storm_threshold" yaml:"storm_threshold,omitempty"`
	// StormWindowMs is the sliding window over which events are counted
	// against StormThreshold. Defaults to 500.
	StormWindowMs int `mapstructure:"storm_window_ms" yaml:"storm_window_ms,omitempty"`
	// StormQuietPeriodMs is how long the watcher waits for no events
	// before draining the batch. Defaults to 500.
	StormQuietPeriodMs int `mapstructure:"storm_quiet_period_ms" yaml:"storm_quiet_period_ms,omitempty"`
}

type QueryConfig struct {
	DefaultDepth int `mapstructure:"default_depth" yaml:"default_depth,omitempty"`
	MaxDepth     int `mapstructure:"max_depth"     yaml:"max_depth,omitempty"`
}

// SearchConfig configures the I13 11-signal rerank pipeline that
// orders `search_symbols` / `winnow_symbols` results. The Weights
// map is keyed by canonical signal name (rerank.SignalBM25,
// rerank.SignalSemantic, …) and overrides the tuned defaults
// returned by rerank.DefaultWeights(). Missing keys keep the
// default weight; setting a key to 0 disables that signal.
// FederationConfig is the .gortex.yaml `federation:` block. All knobs are
// optional; an unset field uses the daemon's built-in default.
type FederationConfig struct {
	PerRemoteTimeoutMs int  `mapstructure:"per_remote_timeout_ms" yaml:"per_remote_timeout_ms,omitempty"`
	BudgetMs           int  `mapstructure:"budget_ms" yaml:"budget_ms,omitempty"`
	BreakerThreshold   int  `mapstructure:"breaker_threshold" yaml:"breaker_threshold,omitempty"`
	BreakerCooldownMs  int  `mapstructure:"breaker_cooldown_ms" yaml:"breaker_cooldown_ms,omitempty"`
	NameKeyedFallback  bool `mapstructure:"name_keyed_fallback" yaml:"name_keyed_fallback,omitempty"`
	// Edges is the cross-daemon proxy-edge minting block. Off by
	// default — federation stays read-only fan-out only.
	Edges FederationEdgesConfig `mapstructure:"edges" yaml:"edges,omitempty"`
}

// FederationEdgesConfig is the `federation.edges` block — the
// cross-daemon proxy-node edge feature. Research-grade and
// off by default; none of the mint/hydrate paths run unless IsEnabled().
type FederationEdgesConfig struct {
	// Enabled turns on proxy-node minting + lazy hydration.
	Enabled bool `mapstructure:"enabled" yaml:"enabled,omitempty"`
	// TTLMs is the proxy-node neighbour-cache TTL in ms (default 5m).
	TTLMs int `mapstructure:"ttl_ms" yaml:"ttl_ms,omitempty"`
	// MaxProxyNodes is the hard heap bound across all remotes (default
	// 5000); overflow refuses the mint.
	MaxProxyNodes int `mapstructure:"max_proxy_nodes" yaml:"max_proxy_nodes,omitempty"`
	// HydrateDepth is the neighbour rings pulled per access (default 1).
	HydrateDepth int `mapstructure:"hydrate_depth" yaml:"hydrate_depth,omitempty"`
}

// IsEnabled reports whether proxy edges are on, honouring the
// GORTEX_FEDERATION_EDGES env override (1/true) over the config field.
func (c FederationEdgesConfig) IsEnabled() bool {
	if v := strings.TrimSpace(os.Getenv("GORTEX_FEDERATION_EDGES")); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return c.Enabled
}

// TTL is the proxy-node neighbour-cache TTL (default 5m).
func (c FederationEdgesConfig) TTL() time.Duration {
	if c.TTLMs > 0 {
		return time.Duration(c.TTLMs) * time.Millisecond
	}
	return 5 * time.Minute
}

// MaxNodes is the proxy-node heap bound across all remotes (default 5000).
func (c FederationEdgesConfig) MaxNodes() int {
	if c.MaxProxyNodes > 0 {
		return c.MaxProxyNodes
	}
	return 5000
}

// Depth is the neighbour rings pulled per hydration (default 1).
func (c FederationEdgesConfig) Depth() int {
	if c.HydrateDepth > 0 {
		return c.HydrateDepth
	}
	return 1
}

type SearchConfig struct {
	// Weights overrides per-signal scoring weights. Canonical names:
	// bm25, semantic, fan_in, hits, fan_out, churn, co_change,
	// community, minhash, api_signature, type_signature, recency,
	// feedback, file_coherence, path_penalty, definition_bias,
	// source_bias, provenance, proximity.
	Weights map[string]float64 `mapstructure:"weights" yaml:"weights,omitempty"`

	// KeywordSoupRewrite controls how `search_symbols` handles a
	// degenerate boolean / OR-soup query (`A OR B OR 'no access'`).
	//   - "split" (default): split the soup on its boolean operators
	//     and run a BM25 OR-merge over the disjuncts; LLM expansion is
	//     suppressed and a `query_advice` nudge rides on the response.
	//   - "nudge": leave retrieval unchanged but still attach the
	//     `query_advice` nudge.
	//   - "off": disable soup handling entirely.
	// An empty value is treated as "split".
	KeywordSoupRewrite string `mapstructure:"keyword_soup_rewrite" yaml:"keyword_soup_rewrite,omitempty"`

	// EquivalenceClasses enables deterministic, LLM-free query
	// expansion through the curated software-concept synonym table
	// plus the per-repo auto-mined concept vocabulary. On by default
	// (the pointer is nil-checked: nil means "on"). Set it false to
	// disable equivalence expansion entirely.
	EquivalenceClasses *bool `mapstructure:"equivalence_classes" yaml:"equivalence_classes,omitempty"`

	// EquivalenceExtra adds repo-custom synonym classes to the curated
	// equivalence table. Each entry maps a class label to its member
	// words; the label itself joins the class. A label that names a
	// curated word extends that curated class rather than forking it.
	EquivalenceExtra map[string][]string `mapstructure:"equivalence_extra" yaml:"equivalence_extra,omitempty"`

	// IndexProse controls whether Markdown prose-section (KindDoc)
	// nodes are fed into the BM25 search index, so search_symbols
	// with corpus:"docs" / "all" can return documentation hits. On
	// by default (a nil pointer means "on"). Set false to keep prose
	// out of the index entirely.
	IndexProse *bool `mapstructure:"index_prose" yaml:"index_prose,omitempty"`

	// VocabAnchoredExpansion sets the server-wide default for the
	// search_symbols `vocab_anchored` argument: when on, the LLM
	// query-expander's freely-invented synonyms are post-filtered to
	// the words that actually appear in this repo's symbol names
	// before they feed the BM25 OR-merge, so a model that hallucinates
	// a plausible-but-absent term can't dilute the candidate pool. Off
	// by default (a nil pointer means "off") -- the per-call argument
	// is the primary control; this only changes the default when the
	// argument is omitted. Degrades to unconstrained expansion when
	// the repo's mined vocabulary is empty.
	VocabAnchoredExpansion *bool `mapstructure:"vocab_anchored_expansion" yaml:"vocab_anchored_expansion,omitempty"`

	// CosineRerank enables the post-rerank exact-cosine refinement
	// stage: after the 11-signal rerank produces the ranked order,
	// the top results are re-scored by exact cosine similarity
	// between the query's embedding and each candidate's stored
	// embedding, recovering the precise semantic distance the
	// rank-based SemanticSignal discards. On by default (a nil
	// pointer means "on"); the stage is a no-op whenever the vector
	// channel is inactive (no embedder, no stored vectors, or the
	// query fails to embed), so enabling it can never regress a
	// text-only search. Set false to keep the pure rank-fusion order.
	CosineRerank *bool `mapstructure:"cosine_rerank" yaml:"cosine_rerank,omitempty"`
}

// Keyword-soup rewrite modes for SearchConfig.KeywordSoupRewrite.
const (
	KeywordSoupSplit = "split"
	KeywordSoupNudge = "nudge"
	KeywordSoupOff   = "off"
)

// EquivalenceClassesEnabled reports whether deterministic
// equivalence-class query expansion is on. It defaults to true --
// a nil EquivalenceClasses pointer (the unset state) means enabled.
func (c SearchConfig) EquivalenceClassesEnabled() bool {
	return c.EquivalenceClasses == nil || *c.EquivalenceClasses
}

// IndexProseEnabled reports whether Markdown prose-section nodes are
// indexed for search. Defaults to true -- a nil IndexProse pointer
// (the unset state) means enabled.
func (c SearchConfig) IndexProseEnabled() bool {
	return c.IndexProse == nil || *c.IndexProse
}

// CosineRerankEnabled reports whether the post-rerank exact-cosine
// refinement stage is on. Defaults to true -- a nil CosineRerank
// pointer (the unset state) means enabled. The stage still no-ops
// when the vector channel is unavailable, so "enabled" is a
// permission, not a guarantee that it runs.
func (c SearchConfig) CosineRerankEnabled() bool {
	return c.CosineRerank == nil || *c.CosineRerank
}

// VocabAnchoredExpansionDefault reports the server-wide default for
// the search_symbols `vocab_anchored` argument when the caller omits
// it. Defaults to false -- a nil VocabAnchoredExpansion pointer (the
// unset state) means off, so expansion stays unconstrained unless a
// caller (or this config) opts in.
func (c SearchConfig) VocabAnchoredExpansionDefault() bool {
	return c.VocabAnchoredExpansion != nil && *c.VocabAnchoredExpansion
}

// EffectiveKeywordSoupRewrite folds the empty default into the
// canonical "split" mode and lower-cases the value, so callers can
// switch on it without re-normalising.
func (c SearchConfig) EffectiveKeywordSoupRewrite() string {
	switch v := strings.ToLower(strings.TrimSpace(c.KeywordSoupRewrite)); v {
	case "", KeywordSoupSplit:
		return KeywordSoupSplit
	case KeywordSoupNudge:
		return KeywordSoupNudge
	case KeywordSoupOff:
		return KeywordSoupOff
	default:
		return KeywordSoupSplit
	}
}

// EmbeddingConfig controls the semantic-search vector channel: which
// embedding provider builds the per-symbol vectors, and the chunking /
// concurrency knobs the indexer uses while building the vector index.
//
// Semantic search is a *fusion signal* layered alongside BM25 — the
// HybridBackend down-weights the vector channel for identifier-shaped
// queries — never a replacement for text search. The default provider
// is `static` (baked GloVe word vectors): it needs zero download and
// is CPU-only, so semantic search is on by default at no setup cost.
type EmbeddingConfig struct {
	// Enabled is tri-state. A nil pointer means "not configured" —
	// the caller falls back to the default (semantic search ON with
	// the static provider). An explicit `embedding.enabled: false`
	// turns the vector channel off; `true` forces it on. Pointer so
	// the loader can tell "absent" from "explicitly false", mirroring
	// RespectGitignore.
	Enabled *bool `mapstructure:"enabled" yaml:"enabled,omitempty"`

	// Provider selects the embedding backend: `static` (baked GloVe,
	// the default), `local` (best available transformer — Hugot
	// MiniLM, auto-downloads ~87 MB on first use), or `api` (an
	// external Ollama / OpenAI-compatible embedding endpoint). An
	// empty value is treated as `static`.
	Provider string `mapstructure:"provider" yaml:"provider,omitempty"`

	// APIURL is the embedding endpoint base URL, used only when
	// Provider is `api`. Ollama vs OpenAI wire format is auto-detected
	// from the URL.
	APIURL string `mapstructure:"api_url" yaml:"api_url,omitempty"`

	// APIModel is the model name passed to the `api` provider. Empty
	// lets the provider pick its format-appropriate default
	// (nomic-embed-text for Ollama, text-embedding-3-small for OpenAI).
	APIModel string `mapstructure:"api_model" yaml:"api_model,omitempty"`

	// MaxSymbols overrides the indexer's hard cap on how many symbols
	// are embedded into the vector index. Zero keeps the built-in
	// default. Above the cap the vector channel is skipped and BM25
	// alone serves search — an OOM during index is worse than a
	// missing semantic boost.
	MaxSymbols int `mapstructure:"max_symbols" yaml:"max_symbols,omitempty"`

	// ChunkThresholdLines is the source-span line count above which a
	// symbol is split into AST windows before embedding instead of
	// being embedded as a single metadata-only vector. Zero keeps the
	// built-in default (~60).
	ChunkThresholdLines int `mapstructure:"chunk_threshold_lines" yaml:"chunk_threshold_lines,omitempty"`

	// ChunkWindowLines caps the line span of each AST window emitted
	// by the sub-chunker. Zero keeps the built-in default (~40).
	ChunkWindowLines int `mapstructure:"chunk_window_lines" yaml:"chunk_window_lines,omitempty"`

	// APIConcurrency bounds how many embedding requests the indexer
	// issues in parallel against an `api` provider. Zero keeps the
	// built-in default (4). Only the `api` provider runs concurrently —
	// in-process transformer backends serialize on an inference mutex,
	// so the pool would give them no speedup.
	APIConcurrency int `mapstructure:"api_concurrency" yaml:"api_concurrency,omitempty"`

	// Variant names a specific local transformer model to load when
	// Provider is `local` -- a key from embedding.KnownHugotVariants
	// (e.g. `fp32`, `bge_small`, `jina_code`). A non-empty Variant
	// pins that exact model instead of the auto-selected default
	// backend. Empty preserves the current default-selection
	// behaviour. Ignored for the `static` and `api` providers.
	// A variant change alters the embedding dimension, which the
	// indexer's dims-mismatch guard detects -- stale vectors of the
	// old width are discarded and the corpus is re-embedded.
	Variant string `mapstructure:"variant" yaml:"variant,omitempty"`
}

// EmbeddingEnabledOrDefault resolves the tri-state Enabled flag against
// the default-on policy: an unset flag means semantic search is ON.
func (e EmbeddingConfig) EmbeddingEnabledOrDefault() bool {
	if e.Enabled == nil {
		return true
	}
	return *e.Enabled
}

// ExternalCallSynthesisEnabledOrDefault resolves the tri-state
// SynthesizeExternalCalls flag against the default-on policy: an unset
// flag (key absent) means external-package call qualification is ON, so
// every external call gets a stable cross-repo identity node by default.
// Default-on is affordable because the synthesis is incrementalised — a
// single-file reindex re-materialises only that file's external terminals
// (SynthesizeExternalCallsForFiles) instead of recomputing the whole
// graph — and the disk backend selects candidate edges through a pushdown
// (graph.ExternalCallCandidates) served by a partial index. Opt out
// per-repo with the key set to false or GORTEX_SYNTH_EXTERNAL_CALLS=0.
func (i IndexConfig) ExternalCallSynthesisEnabledOrDefault() bool {
	if i.SynthesizeExternalCalls == nil {
		return true
	}
	return *i.SynthesizeExternalCalls
}

// SpeculativeDispatchEnabledOrDefault resolves the tri-state
// SynthesizeSpeculativeDispatch flag against a default-OFF policy: an unset
// flag means speculative dynamic-dispatch synthesis is disabled (the edges are
// heuristic fan-outs, opt-in only). Enable per-repo with the key set to true
// or GORTEX_SYNTH_SPECULATIVE=1.
func (i IndexConfig) SpeculativeDispatchEnabledOrDefault() bool {
	if i.SynthesizeSpeculativeDispatch == nil {
		return false
	}
	return *i.SynthesizeSpeculativeDispatch
}

// ScopedGlobalPassesEnabledOrDefault resolves the tri-state ScopedGlobalPasses
// flag against a default-ON policy.
func (i IndexConfig) ScopedGlobalPassesEnabledOrDefault() bool {
	if i.ScopedGlobalPasses == nil {
		return true
	}
	return *i.ScopedGlobalPasses
}

// EmbeddingProviderOrDefault returns the configured provider name,
// falling back to `static` (the zero-download default) when unset.
func (e EmbeddingConfig) EmbeddingProviderOrDefault() string {
	if e.Provider == "" {
		return "static"
	}
	return e.Provider
}

type MCPConfig struct {
	Transport string `mapstructure:"transport" yaml:"transport,omitempty"`
	Port      int    `mapstructure:"port"      yaml:"port,omitempty"`
}

// Default returns a Config with sensible defaults.
//
// Exclude is intentionally empty here — the builtin baseline lives in
// excludes.Builtin and is layered in by ConfigManager.EffectiveExclude.
// Callers that need the full effective list should go through the
// ConfigManager, not Default().
func Default() *Config {
	return &Config{
		Index: IndexConfig{
			Workers: runtime.NumCPU(),
			// MaxFileSize: 0 = no cap. Opt-in knob for users who want
			// to skip large generated/minified files.
			// Prose indexing is on by default; the ConfigManager
			// re-derives this from search.index_prose.
			IndexProse: true,
		},
		Watch: WatchConfig{
			Enabled:    false,
			Paths:      []string{"."},
			DebounceMs: 150,
		},
		Query: QueryConfig{
			DefaultDepth: 3,
			MaxDepth:     10,
		},
		MCP: MCPConfig{
			Transport: "stdio",
			Port:      8765,
		},
		Multi: MultiRepoConfig{
			AutoDetect: false,
		},
		Semantic: SemanticConfig{
			Enabled:         true,
			TimeoutSeconds:  120,
			EnrichOnWatch:   false,
			WatchDebounceMs: 500,
			SkipEmbed:       DefaultSkipEmbed(),
			SkipSearch:      DefaultSkipSearch(),
		},
	}
}

// Load reads config from file, environment, and returns a merged Config.
// configPath may be empty; in that case only default locations are searched.
//
// Legacy-shape handling: previously the `workspace:` key held a struct
// (`workspace: { auto_detect: true }`). The new schema
// reclaims `workspace:` as a scalar slug. Existing configs are migrated
// in place — `workspace.auto_detect` lifts into `multi.auto_detect`,
// and the loader emits a one-line deprecation note via the returned
// error chain (callers can choose whether to surface or swallow it).
func Load(configPath string) (*Config, error) {
	v := viper.New()
	v.SetConfigName(".gortex")
	v.SetConfigType("yaml")

	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.AddConfigPath(".")
		v.AddConfigPath(platform.ConfigDir())
	}

	v.SetEnvPrefix("GORTEX")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	cfg := Default()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
		// No config file found — use defaults + env.
	}

	// Migrate legacy `workspace:` mapping shape (held a struct with
	// `auto_detect`) into the new `multi:` block so the v.Unmarshal
	// below decodes the new schema cleanly. We do the migration on the
	// viper key map so env-var overrides and viper's own merge logic
	// stay consistent.
	migrateLegacyWorkspaceKey(v)

	if err := v.Unmarshal(cfg); err != nil {
		return nil, err
	}

	if err := cfg.validateWorkspaceSchema(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// migrateLegacyWorkspaceKey rewrites `workspace.auto_detect` → `multi.auto_detect`
// in the viper key store before unmarshal, so a `.gortex.yaml` written
// against the legacy schema still produces a working Config without the
// caller seeing a parse error. The migration is silent — there's no
// global logger here — but the audit step (`gortex audit_agent_config`,
// reserved for a follow-up) can flag the deprecated key.
//
// Only the documented legacy field is migrated. Any other map under
// `workspace:` is rejected by `validateWorkspaceSchema` so unknown
// shapes don't get silently ignored.
func migrateLegacyWorkspaceKey(v *viper.Viper) {
	raw := v.Get("workspace")
	if raw == nil {
		return
	}
	switch t := raw.(type) {
	case string:
		// Already in new shape; nothing to do.
	case map[string]interface{}:
		if ad, ok := t["auto_detect"]; ok {
			// Move to the new home unless `multi.auto_detect`
			// is already set explicitly (caller wins).
			if v.Get("multi.auto_detect") == nil {
				v.Set("multi.auto_detect", ad)
			}
		}
		// The old shape never carried a workspace identity slug,
		// so we clear the polymorphic key so v.Unmarshal doesn't
		// fail trying to coerce a map into a string.
		v.Set("workspace", "")
	case map[interface{}]interface{}:
		// yaml.v2 / older path — same semantics.
		if ad, ok := t["auto_detect"]; ok {
			if v.Get("multi.auto_detect") == nil {
				v.Set("multi.auto_detect", ad)
			}
		}
		v.Set("workspace", "")
	default:
		// Unrecognised shape; downstream coercion will surface
		// a precise error rather than us silently dropping it.
	}
}

// validateWorkspaceSchema enforces the defaults / boundaries that
// can't be expressed via struct tags alone:
//
//   - `Project` and `Projects[]` are mutually exclusive (a repo is
//     either single-project or a monorepo, never both).
//   - Every `CrossWorkspaceDeps[].Mode` must be `read-only`. Iteration 1
//     ships only the read-only mode.
//   - `Workspace` slug, when set, may not be empty after trimming.
//
// Errors are concatenated so a malformed file surfaces every problem
// in one pass rather than one round-trip per fix.
func (c *Config) validateWorkspaceSchema() error {
	var errs []string
	if c.Project != "" && len(c.Projects) > 0 {
		errs = append(errs, "config: 'project' and 'projects' are mutually exclusive — pick one")
	}
	for _, dep := range c.CrossWorkspaceDeps {
		if dep.Workspace == "" {
			errs = append(errs, "config: cross_workspace_deps[].workspace is required")
		}
		if len(dep.Modules) == 0 {
			errs = append(errs, "config: cross_workspace_deps[\""+dep.Workspace+"\"].modules must be non-empty")
		}
		switch dep.Mode {
		case "", "read-only":
			// Empty defaults to read-only (the only iteration-1 mode).
		default:
			errs = append(errs,
				"config: cross_workspace_deps[\""+dep.Workspace+"\"].mode = "+
					strconv.Quote(dep.Mode)+" is unsupported in iteration 1; only \"read-only\" is allowed")
		}
	}
	// Validate passive-attach blocks on each semantic provider. An
	// empty `connect: {}` (both fields blank) is rejected here so a
	// misconfigured YAML produces a clear error at load time rather
	// than silently spawning the subprocess and surprising the user
	// when the IDE coexistence story fails.
	for _, pc := range c.Semantic.Providers {
		if err := pc.Connect.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("config: semantic.providers[%q]: %v", pc.Name, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

// ValidateSemanticConnectForTest exposes the connect-block validation
// loop for tests that build a Config in memory and want to assert the
// loader would reject it. Production callers go through Load() which
// runs the full validateWorkspaceSchema pass; this thin export keeps
// the test surface small without forcing tests to also satisfy the
// cross_workspace_deps preconditions.
func (c *Config) ValidateSemanticConnectForTest() error {
	var errs []string
	for _, pc := range c.Semantic.Providers {
		if err := pc.Connect.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("config: semantic.providers[%q]: %v", pc.Name, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}
