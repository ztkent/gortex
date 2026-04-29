package contracts

// CrossLink represents a matched provider-consumer pair, possibly across repos.
type CrossLink struct {
	ContractID string   `json:"contract_id"`
	Provider   Contract `json:"provider"`
	Consumer   Contract `json:"consumer"`
	CrossRepo  bool     `json:"cross_repo"`
}

// MatchResult holds the output of a matching pass.
type MatchResult struct {
	Matched         []CrossLink `json:"matched"`
	OrphanProviders []Contract  `json:"orphan_providers"`
	OrphanConsumers []Contract  `json:"orphan_consumers"`
}

// Match analyses a registry and pairs providers with consumers by
// contract ID, bounded by the (workspace, project) boundary:
//
//   - Providers and consumers in different effective workspaces never
//     pair. Each workspace is matched independently — the across-
//     workspace contracts become orphans on their own side.
//   - Providers and consumers in the same workspace but different
//     projects do not pair either: a project owns its own surface and
//     a sibling project's consumer is treated as an orphan that needs
//     an explicit inter-project import to wire up. Iteration 1 keeps
//     it simple: orphan rather than pair.
//
// "Effective" means: WorkspaceID / ProjectID if set, else RepoPrefix —
// the "missing → repo-name" default. So the previous behaviour (one
// repo = one workspace = one project) still drops out for callers
// that haven't started populating the slugs yet.
//
// The CrossRepo flag stays on a CrossLink whose provider and consumer
// have different RepoPrefixes (legitimately so — two repos belonging
// to one workspace, e.g. `tuck-api` provider matched with `tuck-app`
// consumer when both declare WorkspaceID = "tuck").
func Match(reg *Registry) MatchResult {
	var result MatchResult

	// Collect every contract once (the byID lists already cover all
	// contracts) and bucket them by (effectiveWorkspace,
	// effectiveProject, ID, role). We can't just iterate AllIDs and
	// then split by workspace/project because two providers for the
	// same ID in different projects must be reported as separate
	// orphan groups, not lumped together.
	type bucketKey struct {
		workspace string
		project   string
		id        string
	}
	providers := make(map[bucketKey][]Contract)
	consumers := make(map[bucketKey][]Contract)

	for _, id := range reg.AllIDs() {
		for _, c := range reg.ByID(id) {
			key := bucketKey{
				workspace: c.EffectiveWorkspace(),
				project:   c.EffectiveProject(),
				id:        id,
			}
			switch c.Role {
			case RoleProvider:
				providers[key] = append(providers[key], c)
			case RoleConsumer:
				consumers[key] = append(consumers[key], c)
			}
		}
	}

	// Pair within each bucket; emit matched links plus orphans.
	seen := make(map[bucketKey]struct{})
	for key, provs := range providers {
		seen[key] = struct{}{}
		cons := consumers[key]
		if len(cons) == 0 {
			result.OrphanProviders = append(result.OrphanProviders, provs...)
			continue
		}
		for _, consumer := range cons {
			for _, provider := range provs {
				result.Matched = append(result.Matched, CrossLink{
					ContractID: key.id,
					Provider:   provider,
					Consumer:   consumer,
					CrossRepo:  provider.RepoPrefix != consumer.RepoPrefix,
				})
			}
		}
	}
	for key, cons := range consumers {
		if _, ok := seen[key]; ok {
			continue
		}
		// No provider in this bucket — every consumer is orphaned.
		// Orphan, never pair across the boundary even when an
		// ID-equivalent exists in a sibling workspace.
		result.OrphanConsumers = append(result.OrphanConsumers, cons...)
	}

	return result
}
