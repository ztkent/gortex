package indexer

import (
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/resolver"
)

// resolveWorkspaceID returns the §4.2 workspace slug for an indexer
// emitting nodes for the repo with the given fallback prefix.
//
// Resolution order (highest priority first):
//
//  1. RepoEntry.Workspace — user-level override in
//     `~/.config/gortex/config.yaml`. Lets users pin OSS / read-only
//     repos to a workspace without leaving a `.gortex.yaml` artifact
//     in the repo itself, and lets users override a workspace the
//     repo author chose (the OSS author's slug shouldn't pollute the
//     user's local graph identity).
//  2. `.gortex.yaml::workspace:` — spec-launch.md §4.2 / Step E.
//     Source of truth when the user is the same person who owns the
//     repo (typical for first-party / monorepo setups).
//  3. Falls back to `fallbackPrefix` (the repo prefix). This is the
//     §4.4 "files without a workspace declaration get workspace =
//     repo-name" default.
//
// Empty `fallbackPrefix` returns "" — single-repo callers that don't
// pass a prefix accept that the workspace slug stays empty, in which
// case the §4 boundary checks treat the node as "no workspace
// declared" and degrade to the legacy RepoPrefix-keyed comparison.
func resolveWorkspaceID(entry *config.RepoEntry, cfg *config.Config, fallbackPrefix string) string {
	if entry != nil && entry.Workspace != "" {
		return entry.Workspace
	}
	if cfg != nil && cfg.Workspace != "" {
		return cfg.Workspace
	}
	return fallbackPrefix
}

// resolveProjectID returns the §4.2 project slug for an indexer
// emitting nodes for the repo with the given fallback prefix.
//
// Resolution order mirrors resolveWorkspaceID:
//
//  1. RepoEntry.Project — global config override.
//  2. `.gortex.yaml::project:` — spec-launch.md §4.2 / Step E.
//  3. Per-file `projects[]` paths-glob lookup is the monorepo case
//     (§4.2 example). The current implementation stamps a single
//     project slug per repo; the per-file refinement is a follow-up.
//  4. Falls back to `fallbackPrefix` (the repo prefix). Same
//     "missing → repo-name" rule as workspace.
func resolveProjectID(entry *config.RepoEntry, cfg *config.Config, fallbackPrefix string) string {
	if entry != nil && entry.Project != "" {
		return entry.Project
	}
	if cfg != nil && cfg.Project != "" {
		return cfg.Project
	}
	return fallbackPrefix
}

// BackfillWorkspaceSlugs walks every node and contract attached to
// the MultiIndexer's tracked repos and stamps WorkspaceID / ProjectID
// from the per-repo `.gortex.yaml` whenever those fields are empty.
//
// This closes the upgrade gap: a snapshot written by a pre-§4 daemon
// has WorkspaceID="" everywhere because the field didn't exist; gob
// decodes additive fields as zero. Without backfill, EffectiveWorkspace
// falls back to RepoPrefix and explicit shared-workspace setups
// (multiple repos declaring `workspace: shared`) silently lose
// identity until every file is touched. This pass re-stamps them
// once at warmup.
//
// Returns the count of nodes and contracts updated for telemetry.
// Idempotent: re-running on an already-stamped graph is a no-op.
func (mi *MultiIndexer) BackfillWorkspaceSlugs() (nodesStamped, contractsStamped int) {
	if mi == nil || mi.graph == nil || mi.configMgr == nil {
		return 0, 0
	}
	mi.mu.RLock()
	repoMeta := make(map[string]string, len(mi.repos))
	for prefix, meta := range mi.repos {
		if meta != nil {
			repoMeta[prefix] = meta.RootPath
		}
	}
	mi.mu.RUnlock()

	type slugs struct{ ws, proj string }
	bySlug := make(map[string]slugs, len(repoMeta))
	// Map each repo prefix to its global-config RepoEntry so we honour
	// the user-level workspace/project override even on backfill.
	entryByPrefix := make(map[string]config.RepoEntry, len(repoMeta))
	if mi.configMgr.Global() != nil {
		for _, e := range mi.configMgr.Global().Repos {
			p := config.ResolvePrefix(e)
			if p == "" || p == "." {
				continue
			}
			entryByPrefix[p] = e
		}
	}
	for prefix, root := range repoMeta {
		// Make sure the per-repo `.gortex.yaml` is loaded — at warmup
		// time TrackRepoCtx/ReconcileRepoCtx already calls this, but
		// run defensively in case BackfillWorkspaceSlugs is invoked
		// on a path that didn't.
		mi.configMgr.LoadWorkspaceConfig(prefix, root)
		cfg := mi.configMgr.GetRepoConfig(prefix)
		var entryPtr *config.RepoEntry
		if e, ok := entryByPrefix[prefix]; ok {
			entryCopy := e
			entryPtr = &entryCopy
		}
		bySlug[prefix] = slugs{
			ws:   resolveWorkspaceID(entryPtr, cfg, prefix),
			proj: resolveProjectID(entryPtr, cfg, prefix),
		}
	}

	for _, n := range mi.graph.AllNodes() {
		s, ok := bySlug[n.RepoPrefix]
		if !ok {
			continue
		}
		stamped := false
		if n.WorkspaceID == "" && s.ws != "" {
			n.WorkspaceID = s.ws
			stamped = true
		}
		if n.ProjectID == "" && s.proj != "" {
			n.ProjectID = s.proj
			stamped = true
		}
		if stamped {
			nodesStamped++
		}
	}

	// Per-repo contract registries: rehydrated from snapshot they
	// carry RepoPrefix but no Workspace/Project slugs (pre-§4).
	mi.mu.RLock()
	indexers := make(map[string]*Indexer, len(mi.indexers))
	for k, v := range mi.indexers {
		indexers[k] = v
	}
	mi.mu.RUnlock()
	for prefix, idx := range indexers {
		s, ok := bySlug[prefix]
		if !ok {
			continue
		}
		reg := idx.ContractRegistry()
		if reg == nil {
			continue
		}
		all := reg.All()
		fresh := make([]contracts.Contract, 0, len(all))
		dirty := false
		for _, c := range all {
			if (c.WorkspaceID == "" && s.ws != "") || (c.ProjectID == "" && s.proj != "") {
				if c.WorkspaceID == "" {
					c.WorkspaceID = s.ws
				}
				if c.ProjectID == "" {
					c.ProjectID = s.proj
				}
				dirty = true
				contractsStamped++
			}
			fresh = append(fresh, c)
		}
		if dirty {
			reg.Clear()
			for _, c := range fresh {
				reg.Add(c)
			}
		}
	}
	return nodesStamped, contractsStamped
}

// RunGlobalResolve runs a cross-repo + cross-workspace resolution
// pass over the merged graph, then reconciles contract bridge edges.
// Used post-warmup (after BackfillWorkspaceSlugs) and any other time
// the daemon needs to refresh cross-repo edges without re-indexing
// every file. Idempotent — safe to call repeatedly.
func (mi *MultiIndexer) RunGlobalResolve() {
	if mi == nil || mi.graph == nil {
		return
	}
	cr := resolver.NewCrossRepo(mi.graph)
	cr.SetCrossWorkspaceDepLookup(mi.crossWorkspaceLookup())
	cr.ResolveAll()
	mi.ReconcileContractEdges()
}

// crossWorkspaceLookup builds a resolver.CrossWorkspaceDepLookup from
// the MultiIndexer's per-repo configs. The closure captures `mi` so a
// post-construction config reload (`Reload` on the ConfigManager) is
// picked up automatically — each call walks the current per-repo
// configs and aggregates declarations whose source workspace matches.
//
// Why iterate per-repo: the §4.2 schema places `cross_workspace_deps`
// inside a repo's `.gortex.yaml`, keyed implicitly to that repo's
// `workspace`. Two repos can both declare workspace = "tuck" with
// overlapping but possibly extended dep lists; the union forms the
// effective rule set for source workspace "tuck".
func (mi *MultiIndexer) crossWorkspaceLookup() resolver.CrossWorkspaceDepLookup {
	return func(sourceWS string) []resolver.CrossWorkspaceDepRule {
		if mi == nil || mi.configMgr == nil {
			return nil
		}
		var rules []resolver.CrossWorkspaceDepRule
		mi.mu.RLock()
		repoPrefixes := make([]string, 0, len(mi.repos))
		for prefix := range mi.repos {
			repoPrefixes = append(repoPrefixes, prefix)
		}
		mi.mu.RUnlock()
		// Per-repo crossWorkspaceLookup needs the same precedence as the
		// stamp path: a global-config Workspace override changes which
		// repos count as belonging to `sourceWS`.
		entryByPrefix := make(map[string]config.RepoEntry)
		if mi.configMgr.Global() != nil {
			for _, e := range mi.configMgr.Global().Repos {
				p := config.ResolvePrefix(e)
				if p == "" || p == "." {
					continue
				}
				entryByPrefix[p] = e
			}
		}
		for _, prefix := range repoPrefixes {
			cfg := mi.configMgr.GetRepoConfig(prefix)
			if cfg == nil {
				continue
			}
			var entryPtr *config.RepoEntry
			if e, ok := entryByPrefix[prefix]; ok {
				entryCopy := e
				entryPtr = &entryCopy
			}
			repoWS := resolveWorkspaceID(entryPtr, cfg, prefix)
			if repoWS != sourceWS {
				continue
			}
			for _, dep := range cfg.CrossWorkspaceDeps {
				if dep.Workspace == "" {
					continue
				}
				rules = append(rules, resolver.CrossWorkspaceDepRule{
					Workspace: dep.Workspace,
					Modules:   append([]string(nil), dep.Modules...),
				})
			}
		}
		return rules
	}
}
