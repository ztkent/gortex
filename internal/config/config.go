package config

import (
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/viper"
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
		// Markdown variables are headings captured by the parser —
		// heading text already lives in the graph as file structure;
		// full-text-indexing it adds noise without recall.
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
}

type Config struct {
	// Exclude is the unified ignore list (gitignore semantics) used by
	// both indexing and watching. Workspace-level patterns are appended
	// to builtin + global + per-RepoEntry layers; use `!pattern` to
	// re-include something an outer layer excluded.
	Exclude []string `mapstructure:"exclude" yaml:"exclude,omitempty"`

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

	Index    IndexConfig     `mapstructure:"index"    yaml:"index,omitempty"`
	Watch    WatchConfig     `mapstructure:"watch"    yaml:"watch,omitempty"`
	Query    QueryConfig     `mapstructure:"query"    yaml:"query,omitempty"`
	MCP      MCPConfig       `mapstructure:"mcp"      yaml:"mcp,omitempty"`
	Guards   GuardsConfig    `mapstructure:"guards"   yaml:"guards,omitempty"`
	Multi    MultiRepoConfig `mapstructure:"multi"    yaml:"multi,omitempty"`
	Semantic SemanticConfig  `mapstructure:"semantic" yaml:"semantic,omitempty"`
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
	// MaxFileSize skips files larger than this during indexing. Zero
	// (the default) disables the cap — full coverage is preferred so
	// generated code like `*.pb.go`, schema files, and large data
	// constants stay queryable. Users with very heavy generated /
	// minified files that dominate parse time can set a cap (e.g.
	// 2 MiB) via `.gortex.yaml` to trade coverage for speed. A cap
	// that drops real symbols silently is a worse default than a
	// slightly slower full index.
	MaxFileSize int64 `mapstructure:"max_file_size" yaml:"max_file_size,omitempty"`
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
		v.AddConfigPath("$HOME/.config/gortex")
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
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}
