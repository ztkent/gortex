package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/excludes"
)

// ConfigManager merges GlobalConfig + per-repo WorkspaceConfig.
// It loads the GlobalConfig once at construction and caches workspace
// configs (per-repo .gortex.yaml) on demand with a sync.RWMutex.
type ConfigManager struct {
	global    *GlobalConfig
	workspace map[string]*Config // repoPrefix → workspace config
	mu        sync.RWMutex
	logger    *zap.Logger
}

// NewConfigManager creates a ConfigManager by loading the GlobalConfig
// from the given path. If globalPath is empty, the default path is used.
// A missing GlobalConfig file is not an error (returns empty config).
func NewConfigManager(globalPath string) (*ConfigManager, error) {
	var gc *GlobalConfig
	var err error
	if globalPath != "" {
		gc, err = LoadGlobal(globalPath)
	} else {
		gc, err = LoadGlobal()
	}
	if err != nil {
		return nil, fmt.Errorf("loading global config: %w", err)
	}

	return &ConfigManager{
		global:    gc,
		workspace: make(map[string]*Config),
		logger:    zap.NewNop(),
	}, nil
}

// SetLogger sets the logger for the ConfigManager.
func (cm *ConfigManager) SetLogger(logger *zap.Logger) {
	if logger != nil {
		cm.logger = logger
	}
}

// Global returns the underlying GlobalConfig.
func (cm *ConfigManager) Global() *GlobalConfig {
	return cm.global
}

// Reload re-reads the GlobalConfig from disk, keeping the same config
// path. Workspace caches are preserved — individual `.gortex.yaml`
// files are re-read lazily on demand. Used by the daemon's `reload`
// control RPC to pick up manual edits to the global config without a
// full process restart.
func (cm *ConfigManager) Reload() error {
	cm.mu.Lock()
	path := cm.global.ConfigPath()
	cm.mu.Unlock()

	var fresh *GlobalConfig
	var err error
	if path != "" {
		fresh, err = LoadGlobal(path)
	} else {
		fresh, err = LoadGlobal()
	}
	if err != nil {
		return fmt.Errorf("reload global config: %w", err)
	}

	cm.mu.Lock()
	cm.global = fresh
	// Drop workspace cache so stale per-repo overrides don't linger;
	// they'll be reloaded on the next LoadWorkspaceConfig call.
	cm.workspace = make(map[string]*Config)
	cm.mu.Unlock()
	return nil
}

// LoadWorkspaceConfig loads a .gortex.yaml from the given repo root
// and caches it under the given repoPrefix. If the file is missing,
// no entry is cached (global defaults will apply). If the file is
// malformed, a warning is logged and no entry is cached.
func (cm *ConfigManager) LoadWorkspaceConfig(repoPrefix, repoPath string) {
	configPath := filepath.Join(repoPath, ".gortex.yaml")

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No workspace config — global defaults will apply.
			return
		}
		cm.logger.Warn("failed to read workspace config",
			zap.String("repo", repoPrefix),
			zap.String("path", configPath),
			zap.Error(err))
		return
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		// Malformed workspace config — log warning, return global defaults.
		cm.logger.Warn("malformed workspace config, using global defaults",
			zap.String("repo", repoPrefix),
			zap.String("path", configPath),
			zap.Error(err))
		return
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.workspace[repoPrefix] = &cfg
}

// getWorkspaceConfig returns the cached workspace config for a repo, or nil.
func (cm *ConfigManager) getWorkspaceConfig(repoPrefix string) *Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.workspace[repoPrefix]
}

// GetRepoConfig returns the merged config for a repository. The returned
// Config has Index.Exclude and Watch.Exclude populated with the full
// layered exclude list from EffectiveExclude, so callers passing
// cfg.Index into indexer.New automatically receive the effective patterns.
func (cm *ConfigManager) GetRepoConfig(repoPrefix string) *Config {
	var out *Config
	if ws := cm.getWorkspaceConfig(repoPrefix); ws != nil {
		dup := *ws
		out = &dup
	} else {
		out = Default()
	}
	effective := cm.EffectiveExclude(repoPrefix)
	out.Exclude = effective
	out.Index.Exclude = effective
	out.Watch.Exclude = effective
	// Plumb semantic.skip_embed through to the indexer's config so the
	// embedder can filter without a new setter. Workspace > compiled
	// defaults.
	if len(out.Semantic.SkipEmbed) > 0 {
		out.Index.SkipEmbed = out.Semantic.SkipEmbed
	} else {
		out.Index.SkipEmbed = DefaultSkipEmbed()
	}
	return out
}

// EffectiveExclude returns the effective ignore patterns for a repo,
// layered in precedence order (later layers can re-include via !pattern):
//
//  1. Builtin baseline (excludes.Builtin)
//  2. Global Exclude from ~/.config/gortex/config.yaml
//  3. Matching RepoEntry.Exclude (first match in Repos, then Projects)
//  4. Workspace .gortex.yaml top-level Exclude
//  5. Legacy workspace Index.Exclude / Watch.Exclude (deprecated)
func (cm *ConfigManager) EffectiveExclude(repoPrefix string) []string {
	cm.mu.RLock()
	gc := cm.global
	ws := cm.workspace[repoPrefix]
	cm.mu.RUnlock()

	out := make([]string, 0, 32)
	out = append(out, excludes.Builtin...)
	if gc != nil {
		out = append(out, gc.Exclude...)
		if entry := gc.FindRepoByPrefix(repoPrefix); entry != nil {
			out = append(out, entry.Exclude...)
		}
	}
	if ws != nil {
		out = append(out, ws.Exclude...)
		// Legacy fallback: older configs put patterns under index.exclude
		// or watch.exclude. Fold them in so nothing silently breaks.
		if len(ws.Exclude) == 0 {
			out = append(out, ws.Index.Exclude...)
			out = append(out, ws.Watch.Exclude...)
		}
	}
	return out
}

// EffectiveGuardRules returns the effective guard rules for a repo.
// Workspace config wins when present; otherwise global defaults apply.
func (cm *ConfigManager) EffectiveGuardRules(repoPrefix string) []GuardRule {
	ws := cm.getWorkspaceConfig(repoPrefix)
	if ws != nil && len(ws.Guards.Rules) > 0 {
		return ws.Guards.Rules
	}
	return Default().Guards.Rules
}

// ActiveRepos returns the repos for the active project, or the top-level
// repos if no active project is set.
func (cm *ConfigManager) ActiveRepos() []RepoEntry {
	if cm.global.ActiveProject != "" {
		repos, err := cm.global.ResolveRepos(cm.global.ActiveProject)
		if err == nil {
			return repos
		}
		// If the active project is invalid, fall through to top-level repos.
		cm.logger.Warn("active project not found, falling back to top-level repos",
			zap.String("project", cm.global.ActiveProject),
			zap.Error(err))
	}
	return cm.global.Repos
}
