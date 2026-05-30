package indexer

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/resolver"
)

// resolveWorkspaceID returns the workspace slug for an indexer
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
//  2. `.gortex.yaml::workspace:` — source of truth when the user is
//     the same person who owns the repo (typical for first-party /
//     monorepo setups).
//  3. Falls back to `fallbackPrefix` (the repo prefix). This is the
//     "files without a workspace declaration get workspace =
//     repo-name" default.
//
// Empty `fallbackPrefix` returns "" — single-repo callers that don't
// pass a prefix accept that the workspace slug stays empty, in which
// case the boundary checks treat the node as "no workspace declared"
// and degrade to the legacy RepoPrefix-keyed comparison.
func resolveWorkspaceID(entry *config.RepoEntry, cfg *config.Config, fallbackPrefix string) string {
	if entry != nil && entry.Workspace != "" {
		return entry.Workspace
	}
	if cfg != nil && cfg.Workspace != "" {
		return cfg.Workspace
	}
	return fallbackPrefix
}

// resolveProjectID returns the project slug for an indexer emitting
// nodes for the repo with the given fallback prefix.
//
// Resolution order mirrors resolveWorkspaceID:
//
//  1. RepoEntry.Project — global config override.
//  2. `.gortex.yaml::project:`.
//  3. Per-file `projects[]` paths-glob lookup is the monorepo case.
//     The current implementation stamps a single project slug per
//     repo; the per-file refinement is a follow-up.
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

// ScopeForCWD resolves a working directory to the (workspace, project)
// scope of the tracked repo that physically contains it. The longest
// matching repo root wins so a repo nested inside another resolves to
// the inner one.
//
// ok is false when cwd lies outside every tracked repo — callers MUST
// fail closed (return no cross-workspace data) rather than widening to
// the global graph.
//
// A repo whose effective WorkspaceID is empty (no `.gortex.yaml::
// workspace:` and no global-config override) is treated as its own
// singleton workspace keyed on the repo prefix — the same fallback
// rule resolveWorkspaceID and QueryOptions.scopeAllows use, so the
// session boundary stays consistent with the node stamps.
func (mi *MultiIndexer) ScopeForCWD(cwd string) (workspaceID, projectID, repoPrefix string, ok bool) {
	if mi == nil || cwd == "" {
		return "", "", "", false
	}
	cwd = filepath.Clean(cwd)

	mi.mu.RLock()
	defer mi.mu.RUnlock()

	var bestRoot, bestPrefix string
	for prefix, meta := range mi.repos {
		if meta == nil {
			continue
		}
		root := filepath.Clean(meta.RootPath)
		if root == "" || root == "." {
			continue
		}
		if cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator)) {
			if len(root) > len(bestRoot) {
				bestRoot, bestPrefix = root, prefix
			}
		}
	}
	if bestPrefix == "" {
		return "", "", "", false
	}

	ws, proj := bestPrefix, bestPrefix
	if idx := mi.indexers[bestPrefix]; idx != nil {
		if v := idx.WorkspaceID(); v != "" {
			ws = v
		}
		if v := idx.ProjectID(); v != "" {
			proj = v
		}
	}
	return ws, proj, bestPrefix, true
}

// ReposInWorkspace returns the set of repo prefixes whose effective
// workspace slug equals workspaceID. It is the complete list of repos
// a workspace-scoped session is permitted to see — used to bound the
// query surface for handlers that filter by repo prefix rather than
// routing through the engine's WorkspaceID-aware traversal.
//
// The effective slug follows the same singleton fallback as
// ScopeForCWD: a repo with no declared workspace is its own workspace
// keyed on the repo prefix.
func (mi *MultiIndexer) ReposInWorkspace(workspaceID string) map[string]bool {
	out := make(map[string]bool)
	if mi == nil || workspaceID == "" {
		return out
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	for prefix := range mi.repos {
		ws := prefix
		if idx := mi.indexers[prefix]; idx != nil {
			if v := idx.WorkspaceID(); v != "" {
				ws = v
			}
		}
		if ws == workspaceID {
			out[prefix] = true
		}
	}
	return out
}

// BackfillWorkspaceSlugs walks every node and contract attached to
// the MultiIndexer's tracked repos and stamps WorkspaceID / ProjectID
// from the per-repo `.gortex.yaml` whenever those fields are empty.
//
// This closes the upgrade gap: a snapshot written by a daemon
// before WorkspaceID existed has WorkspaceID="" everywhere; gob
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
	// carry RepoPrefix but no Workspace/Project slugs.
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
	cr.SetLogger(mi.logger)
	cr.SetCrossWorkspaceDepLookup(mi.crossWorkspaceLookup())
	cr.SetNpmAliasResolver(mi.npmAliasResolver())
	cr.SetWorkspaceMembership(mi.workspaceMembershipResolver())
	cr.ResolveAll()
	mi.ReconcileContractEdges()
}

// crossWorkspaceLookup builds a resolver.CrossWorkspaceDepLookup from
// the MultiIndexer's per-repo configs. The closure captures `mi` so a
// post-construction config reload (`Reload` on the ConfigManager) is
// picked up automatically — each call walks the current per-repo
// configs and aggregates declarations whose source workspace matches.
//
// Why iterate per-repo: the schema places `cross_workspace_deps`
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
