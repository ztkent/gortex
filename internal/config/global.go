package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/llm"
)

var (
	// globalConfigMu serialises Save() so concurrent writers can't
	// truncate each other's payload.
	globalConfigMu sync.Mutex

	// projectNameRe matches valid project names: alphanumeric, hyphens, underscores.
	projectNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

// RepoEntry defines a repository in the config.
type RepoEntry struct {
	Path string `mapstructure:"path" yaml:"path"`
	Name string `mapstructure:"name" yaml:"name,omitempty"`
	Ref  string `mapstructure:"ref"  yaml:"ref,omitempty"`
	// Exclude adds repo-specific ignore patterns layered on top of the
	// global Exclude list (gitignore semantics).
	Exclude []string `mapstructure:"exclude" yaml:"exclude,omitempty"`
	// Workspace is an optional override for the workspace slug, set in
	// the user's global config when the repo itself has no
	// `.gortex.yaml::workspace` (or when the user wants to override
	// one — e.g. tracking an OSS repo without leaving an artifact in
	// it). Wins over `.gortex.yaml::workspace`. Falls through to the
	// default (repo prefix) when both are empty.
	Workspace string `mapstructure:"workspace" yaml:"workspace,omitempty"`
	// Project is the matching override for the project slug. Same
	// precedence rules as Workspace.
	Project string `mapstructure:"project" yaml:"project,omitempty"`
}

// ProjectConfig defines a named project grouping repos.
type ProjectConfig struct {
	Repos []RepoEntry `mapstructure:"repos" yaml:"repos"`
}

// GlobalConfig is the user-level config at ~/.config/gortex/config.yaml.
type GlobalConfig struct {
	Projects      map[string]ProjectConfig `mapstructure:"projects"       yaml:"projects,omitempty"`
	Repos         []RepoEntry              `mapstructure:"repos"          yaml:"repos,omitempty"`
	ActiveProject string                   `mapstructure:"active_project" yaml:"active_project,omitempty"`
	// Exclude is the user-level ignore list layered above the builtin
	// baseline and below per-RepoEntry / workspace lists.
	Exclude []string `mapstructure:"exclude" yaml:"exclude,omitempty"`

	// LLM is the user-level local-LLM service config (`llm.model:` etc.).
	// Merged into the repo-local Config.LLM at daemon startup via
	// MergeLLMInto — local non-zero fields win, global fills the rest.
	// Lets users keep model paths and tuning in one place across repos
	// without duplicating an `llm:` block in every `.gortex.yaml`.
	LLM llm.Config `mapstructure:"llm" yaml:"llm,omitempty"`

	// configPath stores the file path used for Save(). Set by LoadGlobal or SetConfigPath.
	configPath string `yaml:"-"`
}

// MergeLLMInto layers a repo-local llm.Config over the global user
// config: each zero-valued field of local is filled from gc.LLM,
// per provider sub-block. Local non-zero values always win — including
// an explicit per-repo override of an inherited global model path.
// Safe to call on a nil receiver (returns local unchanged), so daemon
// startup paths don't need separate nil-checks for the global config.
//
// The local provider's model path additionally gets `~/` expanded
// against $HOME so users can write portable paths in either config.
func (gc *GlobalConfig) MergeLLMInto(local llm.Config) llm.Config {
	if gc != nil {
		local = local.MergedWith(gc.LLM)
	}
	local.Local.Model = expandHome(local.Local.Model)
	return local
}

// expandHome resolves a leading `~/` in a path against $HOME so users
// can write portable model paths in their global config. No-op when
// the path is empty, absolute without `~`, or `~` is not the first
// character. Returns the input unchanged on any os.UserHomeDir error.
func expandHome(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// DefaultGlobalConfigPath returns the default path: ~/.config/gortex/config.yaml.
//
// Resolved fresh on every call so HOME changes (notably t.Setenv in tests)
// take effect. A previous version cached this with sync.Once, which made
// the first caller win for the lifetime of the process — any subsequent
// test that flipped HOME silently ended up writing into the developer's
// real config.
func DefaultGlobalConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "gortex", "config.yaml")
}

// LoadGlobal reads the global config from ~/.config/gortex/config.yaml.
// If the file does not exist, it returns an empty GlobalConfig (no error).
// If configPath is empty, the default path is used.
func LoadGlobal(configPath ...string) (*GlobalConfig, error) {
	path := DefaultGlobalConfigPath()
	if len(configPath) > 0 && configPath[0] != "" {
		path = configPath[0]
	}

	gc := &GlobalConfig{
		configPath: path,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Absent GlobalConfig is not an error — return empty struct.
			return gc, nil
		}
		return nil, fmt.Errorf("reading global config: %w", err)
	}

	if err := yaml.Unmarshal(data, gc); err != nil {
		return nil, fmt.Errorf("parsing global config: %w", err)
	}

	gc.configPath = path
	return gc, nil
}

// SetConfigPath overrides the file path used by Save().
func (gc *GlobalConfig) SetConfigPath(path string) {
	gc.configPath = path
}

// ConfigPath returns the file path used by Save().
func (gc *GlobalConfig) ConfigPath() string {
	if gc.configPath == "" {
		return DefaultGlobalConfigPath()
	}
	return gc.configPath
}

// Save writes the GlobalConfig to disk, creating the directory if needed.
// Uses a file-level mutex to prevent concurrent writes.
func (gc *GlobalConfig) Save() error {
	globalConfigMu.Lock()
	defer globalConfigMu.Unlock()

	path := gc.ConfigPath()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config directory %s: %w", dir, err)
	}

	data, err := yaml.Marshal(gc)
	if err != nil {
		return fmt.Errorf("marshaling global config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing global config to %s: %w", path, err)
	}

	return nil
}

// Validate checks the GlobalConfig for:
// - Duplicate Repo_Prefix values across top-level repos and all projects
// - Invalid project name format
// - Conflicting name overrides for shared repos across projects
func (gc *GlobalConfig) Validate() error {
	var errs []string

	// Check project name format.
	for name := range gc.Projects {
		if !projectNameRe.MatchString(name) {
			errs = append(errs, fmt.Sprintf(
				"invalid project name %q: must contain only alphanumeric characters, hyphens, or underscores", name))
		}
	}

	// Check duplicate prefixes across top-level repos.
	if dupErrs := checkDuplicatePrefixes(gc.Repos); len(dupErrs) > 0 {
		errs = append(errs, dupErrs...)
	}

	// Check duplicate prefixes within each project.
	for projName, proj := range gc.Projects {
		if dupErrs := checkDuplicatePrefixes(proj.Repos); len(dupErrs) > 0 {
			for _, e := range dupErrs {
				errs = append(errs, fmt.Sprintf("project %q: %s", projName, e))
			}
		}
	}

	// Check conflicting name overrides for shared repos across projects.
	if conflictErrs := checkConflictingNameOverrides(gc.Projects); len(conflictErrs) > 0 {
		errs = append(errs, conflictErrs...)
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// ResolvePrefix returns the effective Repo_Prefix for a RepoEntry.
// If Name is set, it is used directly. Otherwise, the prefix is derived
// from the last path component.
func ResolvePrefix(entry RepoEntry) string {
	if entry.Name != "" {
		return entry.Name
	}
	return filepath.Base(entry.Path)
}

// FindRepoByPrefix searches top-level Repos and all Projects for an entry
// whose ResolvePrefix matches. Returns nil when no entry matches.
func (gc *GlobalConfig) FindRepoByPrefix(prefix string) *RepoEntry {
	if gc == nil {
		return nil
	}
	for i := range gc.Repos {
		if ResolvePrefix(gc.Repos[i]) == prefix {
			return &gc.Repos[i]
		}
	}
	for _, proj := range gc.Projects {
		for i := range proj.Repos {
			if ResolvePrefix(proj.Repos[i]) == prefix {
				return &proj.Repos[i]
			}
		}
	}
	return nil
}

// checkDuplicatePrefixes returns errors for any duplicate Repo_Prefix values.
func checkDuplicatePrefixes(entries []RepoEntry) []string {
	seen := make(map[string]string) // prefix → first path
	var errs []string
	for _, e := range entries {
		prefix := ResolvePrefix(e)
		if firstPath, ok := seen[prefix]; ok {
			errs = append(errs, fmt.Sprintf(
				"duplicate repo prefix %q: %s and %s", prefix, firstPath, e.Path))
		} else {
			seen[prefix] = e.Path
		}
	}
	return errs
}

// checkConflictingNameOverrides checks that shared repos (same absolute path)
// across different projects don't have conflicting name overrides.
func checkConflictingNameOverrides(projects map[string]ProjectConfig) []string {
	// Map: absolute path → map[project name] → name override
	type nameInfo struct {
		project string
		name    string
	}
	pathNames := make(map[string][]nameInfo)

	for projName, proj := range projects {
		for _, entry := range proj.Repos {
			absPath := normalizePath(entry.Path)
			pathNames[absPath] = append(pathNames[absPath], nameInfo{
				project: projName,
				name:    ResolvePrefix(entry),
			})
		}
	}

	var errs []string
	for absPath, infos := range pathNames {
		if len(infos) < 2 {
			continue
		}
		// Check if all resolved names are the same.
		firstName := infos[0].name
		for _, info := range infos[1:] {
			if info.name != firstName {
				errs = append(errs, fmt.Sprintf(
					"conflicting name overrides for repo %s: project %q uses %q, project %q uses %q",
					absPath, infos[0].project, firstName, info.project, info.name))
			}
		}
	}
	return errs
}

// normalizePath attempts to resolve a path to absolute for comparison.
// If resolution fails, the original path is returned.
func normalizePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// AddRepo adds a repository entry to the top-level repos list.
// Relative paths are resolved to absolute. Duplicate paths are skipped.
func (gc *GlobalConfig) AddRepo(entry RepoEntry) error {
	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		return fmt.Errorf("resolving path %s: %w", entry.Path, err)
	}
	entry.Path = absPath

	// Check for duplicate path.
	for _, existing := range gc.Repos {
		existingAbs := normalizePath(existing.Path)
		if existingAbs == absPath {
			return nil // already tracked, skip
		}
	}

	gc.Repos = append(gc.Repos, entry)
	return nil
}

// RemoveRepo removes a repository entry from the top-level repos list by path.
// The path is resolved to absolute for comparison.
func (gc *GlobalConfig) RemoveRepo(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolving path %s: %w", path, err)
	}

	for i, entry := range gc.Repos {
		entryAbs := normalizePath(entry.Path)
		if entryAbs == absPath {
			gc.Repos = append(gc.Repos[:i], gc.Repos[i+1:]...)
			return nil
		}
	}

	return fmt.Errorf("repository not found: %s", path)
}

// ResolveRepos returns the effective repo list for a given project name.
// If projectName is empty, it returns the top-level repos list.
// Otherwise it tries two complementary resolution strategies in order:
//
//  1. Top-level Projects map: gc.Projects[projectName].Repos. This is the
//     "explicit project definition" form.
//  2. Per-repo annotations: every gc.Repos[i] whose Project field equals
//     projectName. This is the "flat repos list with project: <slug> tags"
//     form, which is what users get when they run `gortex track --project`.
//
// Strategy 1 wins when both are populated for the same name, preserving
// the existing precedence so users who define a project explicitly are
// not surprised by per-repo annotations bleeding in.
//
// Returns the "project not found" error only when both strategies yield
// zero entries.
func (gc *GlobalConfig) ResolveRepos(projectName string) ([]RepoEntry, error) {
	if projectName == "" {
		return gc.Repos, nil
	}

	if proj, ok := gc.Projects[projectName]; ok {
		return proj.Repos, nil
	}

	// Fall back to per-repo Project annotations on the flat repos list.
	var tagged []RepoEntry
	for _, entry := range gc.Repos {
		if entry.Project == projectName {
			tagged = append(tagged, entry)
		}
	}
	if len(tagged) > 0 {
		return tagged, nil
	}

	available := make([]string, 0, len(gc.Projects))
	for name := range gc.Projects {
		available = append(available, name)
	}
	return nil, fmt.Errorf("project not found: %q (available: %s)",
		projectName, strings.Join(available, ", "))
}
