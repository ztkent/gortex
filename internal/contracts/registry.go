package contracts

import (
	"strings"
	"sync"
)

// Registry collects contracts from all repos and provides indexed lookups.
type Registry struct {
	mu          sync.RWMutex
	byID        map[string][]Contract // contractID -> contracts
	byRepo      map[string][]Contract // repoPrefix -> contracts
	bySymbol    map[string][]Contract // symbolID -> contracts
	byFilePath  map[string][]Contract // filePath -> contracts
	byWorkspace map[string][]Contract // workspaceID -> contracts
}

// NewRegistry creates an empty contract registry.
func NewRegistry() *Registry {
	return &Registry{
		byID:        make(map[string][]Contract),
		byRepo:      make(map[string][]Contract),
		bySymbol:    make(map[string][]Contract),
		byFilePath:  make(map[string][]Contract),
		byWorkspace: make(map[string][]Contract),
	}
}

// Add inserts a contract into the registry.
func (r *Registry) Add(c Contract) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[c.ID] = append(r.byID[c.ID], c)
	r.byRepo[c.RepoPrefix] = append(r.byRepo[c.RepoPrefix], c)
	if c.SymbolID != "" {
		r.bySymbol[c.SymbolID] = append(r.bySymbol[c.SymbolID], c)
	}
	r.byFilePath[c.FilePath] = append(r.byFilePath[c.FilePath], c)
	r.byWorkspace[c.EffectiveWorkspace()] = append(r.byWorkspace[c.EffectiveWorkspace()], c)
}

// AddAll inserts multiple contracts, assigning the repo prefix to each.
// Existing WorkspaceID / ProjectID values on the contracts are
// preserved — callers that need to also stamp those slugs should use
// AddAllScoped.
func (r *Registry) AddAll(contracts []Contract, repoPrefix string) {
	for i := range contracts {
		contracts[i].RepoPrefix = repoPrefix
		r.Add(contracts[i])
	}
}

// AddAllScoped inserts multiple contracts, assigning the repo prefix
// and (when non-empty) the WorkspaceID / ProjectID slugs. Mirrors
// the indexer-side stamp applied to graph nodes via Indexer.applyRepoPrefix.
// Empty workspaceID / projectID arguments leave the contract's existing
// values untouched (so a per-file resolver can pre-stamp a stricter
// projectID for monorepo paths-glob matches and the AddAllScoped call
// won't blunt it back to the repo-default).
func (r *Registry) AddAllScoped(contracts []Contract, repoPrefix, workspaceID, projectID string) {
	for i := range contracts {
		contracts[i].RepoPrefix = repoPrefix
		if workspaceID != "" && contracts[i].WorkspaceID == "" {
			contracts[i].WorkspaceID = workspaceID
		}
		if projectID != "" && contracts[i].ProjectID == "" {
			contracts[i].ProjectID = projectID
		}
		r.Add(contracts[i])
	}
}

// ReplaceByID swaps the full list of contracts recorded for an ID.
// Used by post-passes that mutate a contract's meta in place (e.g.
// cross-file handler resolution re-running schema enrichment). The
// per-repo / per-symbol / per-file indexes are rebuilt for the
// affected entries so subsequent lookups stay consistent.
func (r *Registry) ReplaceByID(id string, list []Contract) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Remove every old entry for this ID from the secondary indexes.
	for _, old := range r.byID[id] {
		r.byRepo[old.RepoPrefix] = removeContract(r.byRepo[old.RepoPrefix], old)
		if old.SymbolID != "" {
			r.bySymbol[old.SymbolID] = removeContract(r.bySymbol[old.SymbolID], old)
		}
		r.byFilePath[old.FilePath] = removeContract(r.byFilePath[old.FilePath], old)
		r.byWorkspace[old.EffectiveWorkspace()] = removeContract(r.byWorkspace[old.EffectiveWorkspace()], old)
	}
	r.byID[id] = nil

	// Re-insert.
	for _, c := range list {
		r.byID[id] = append(r.byID[id], c)
		r.byRepo[c.RepoPrefix] = append(r.byRepo[c.RepoPrefix], c)
		if c.SymbolID != "" {
			r.bySymbol[c.SymbolID] = append(r.bySymbol[c.SymbolID], c)
		}
		r.byFilePath[c.FilePath] = append(r.byFilePath[c.FilePath], c)
		r.byWorkspace[c.EffectiveWorkspace()] = append(r.byWorkspace[c.EffectiveWorkspace()], c)
	}
}

// TypeLookup resolves a bare type name to the symbol ID of its
// definition. The optional repoHint lets callers prefer definitions
// from the same repo as the contract itself when multiple repos
// define a type with the same name. An empty return slice means no
// match; a single entry is an unambiguous upgrade; more than one
// entry signals ambiguity and the upgrade pass leaves the bare name
// in place.
type TypeLookup func(name, repoHint string) []string

// UpgradeBareTypeRefs walks every contract in the registry and
// rewrites Meta["request_type"] / Meta["response_type"] from a bare
// type name to a symbol ID when the lookup yields exactly one match.
// Meta entries that already look like symbol IDs (they contain "::")
// are left alone. This pass is the counterpart to the in-file type
// resolution done during extraction — it upgrades the cases where
// the type is defined in a sibling file and therefore couldn't be
// resolved from the file-scoped node list.
func (r *Registry) UpgradeBareTypeRefs(lookup TypeLookup) {
	r.mu.Lock()
	defer r.mu.Unlock()
	upgrade := func(meta map[string]any, key, repoHint string) {
		v, ok := meta[key].(string)
		if !ok || v == "" || strings.Contains(v, "::") {
			return
		}
		candidates := lookup(v, repoHint)
		if len(candidates) == 1 {
			meta[key] = candidates[0]
		}
	}
	for id, list := range r.byID {
		for i := range list {
			if list[i].Meta == nil {
				continue
			}
			upgrade(list[i].Meta, "request_type", list[i].RepoPrefix)
			upgrade(list[i].Meta, "response_type", list[i].RepoPrefix)
		}
		r.byID[id] = list
	}
	// Mirror the upgrades into the other indexes so lookups stay
	// consistent. The byRepo / bySymbol / byFilePath slices hold
	// value copies of the same Contract, so we re-walk them too.
	syncMeta := func(m map[string][]Contract) {
		for k, list := range m {
			for i := range list {
				if list[i].Meta == nil {
					continue
				}
				upgrade(list[i].Meta, "request_type", list[i].RepoPrefix)
				upgrade(list[i].Meta, "response_type", list[i].RepoPrefix)
			}
			m[k] = list
		}
	}
	syncMeta(r.byRepo)
	syncMeta(r.bySymbol)
	syncMeta(r.byFilePath)
}

// ByID returns all contracts matching the given contract ID.
func (r *Registry) ByID(id string) []Contract {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.byID[id]
	out := make([]Contract, len(src))
	copy(out, src)
	return out
}

// ByRepo returns all contracts belonging to the given repo prefix.
func (r *Registry) ByRepo(repoPrefix string) []Contract {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.byRepo[repoPrefix]
	out := make([]Contract, len(src))
	copy(out, src)
	return out
}

// BySymbol returns all contracts attached to the given symbol.
func (r *Registry) BySymbol(symbolID string) []Contract {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.bySymbol[symbolID]
	out := make([]Contract, len(src))
	copy(out, src)
	return out
}

// ByFile returns all contracts found in the given file.
func (r *Registry) ByFile(filePath string) []Contract {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.byFilePath[filePath]
	out := make([]Contract, len(src))
	copy(out, src)
	return out
}

// ByWorkspace returns every contract whose effective workspace
// (WorkspaceID || RepoPrefix default) equals workspaceID. Used by
// per-workspace `contracts check` calls so each workspace's matcher
// pass sees only its own contracts.
func (r *Registry) ByWorkspace(workspaceID string) []Contract {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.byWorkspace[workspaceID]
	out := make([]Contract, len(src))
	copy(out, src)
	return out
}

// AllWorkspaces returns the deduplicated list of effective workspace
// slugs present in the registry. The matcher uses this to drive its
// per-workspace pairing pass.
func (r *Registry) AllWorkspaces() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byWorkspace))
	for ws := range r.byWorkspace {
		out = append(out, ws)
	}
	return out
}

// AllIDs returns a deduplicated list of contract IDs in the registry.
func (r *Registry) AllIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	return ids
}

// All returns every contract in the registry.
func (r *Registry) All() []Contract {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Contract
	seen := make(map[string]bool)
	for _, contracts := range r.byID {
		for _, c := range contracts {
			key := c.ID + "|" + c.FilePath + "|" + c.SymbolID + "|" + string(c.Role)
			if !seen[key] {
				seen[key] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// Clear removes all contracts from the registry.
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID = make(map[string][]Contract)
	r.byRepo = make(map[string][]Contract)
	r.bySymbol = make(map[string][]Contract)
	r.byFilePath = make(map[string][]Contract)
	r.byWorkspace = make(map[string][]Contract)
}

// EvictRepo removes all contracts belonging to the given repo prefix.
func (r *Registry) EvictRepo(repoPrefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	contracts := r.byRepo[repoPrefix]
	if len(contracts) == 0 {
		return 0
	}

	// Remove from byID index.
	for _, c := range contracts {
		r.byID[c.ID] = removeContract(r.byID[c.ID], c)
		if len(r.byID[c.ID]) == 0 {
			delete(r.byID, c.ID)
		}
	}

	// Remove from bySymbol index.
	for _, c := range contracts {
		if c.SymbolID != "" {
			r.bySymbol[c.SymbolID] = removeContract(r.bySymbol[c.SymbolID], c)
			if len(r.bySymbol[c.SymbolID]) == 0 {
				delete(r.bySymbol, c.SymbolID)
			}
		}
	}

	// Remove from byFilePath index.
	for _, c := range contracts {
		r.byFilePath[c.FilePath] = removeContract(r.byFilePath[c.FilePath], c)
		if len(r.byFilePath[c.FilePath]) == 0 {
			delete(r.byFilePath, c.FilePath)
		}
	}

	// Remove from byWorkspace index. A workspace can span multiple
	// repos by design, so a workspace bucket may still have entries
	// from sibling repos after this evict — we only delete the bucket
	// when it goes empty.
	for _, c := range contracts {
		ws := c.EffectiveWorkspace()
		r.byWorkspace[ws] = removeContract(r.byWorkspace[ws], c)
		if len(r.byWorkspace[ws]) == 0 {
			delete(r.byWorkspace, ws)
		}
	}

	removed := len(contracts)
	delete(r.byRepo, repoPrefix)
	return removed
}

func removeContract(contracts []Contract, target Contract) []Contract {
	out := contracts[:0]
	for _, c := range contracts {
		if c.FilePath == target.FilePath && c.SymbolID == target.SymbolID &&
			c.Role == target.Role && c.ID == target.ID && c.RepoPrefix == target.RepoPrefix {
			continue
		}
		out = append(out, c)
	}
	return out
}
