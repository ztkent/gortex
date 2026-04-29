package contracts

// Cross-workspace contract-extraction must NOT pair across the
// workspace hard boundary.
//
// The design problem made concrete:
//
// Two unrelated services — say `tuck/api` and an unrelated personal
// project — both implement `POST /api/auth/login` with similar request
// shapes (because both copied the same auth template). Without
// workspace bucketing, the matcher pairs them as producer/consumer
// because the contract ID (`http::POST::/api/auth/login`) is keyed
// only on (kind, identifier). With a workspace-keyed contract
// registry and Match boundary enforcement the matcher buckets
// contracts by (workspace_id, project_id) before pairing, so the two
// workspaces stay isolated.

import (
	"strings"
	"testing"
)

// fixtureRegistry builds two workspaces' worth of contracts. Each
// workspace declares its own WorkspaceID slug so the matcher's
// boundary check kicks in. Inside `tuck` the API repo and the app
// repo legitimately pair (one CrossRepo link, both within
// WorkspaceID="tuck"). Inside `personal` same. Across the two — never.
func fixtureRegistry(t *testing.T) *Registry {
	t.Helper()

	reg := NewRegistry()

	// Workspace A: tuck. Provider + consumer (legitimately matched
	// inside its own workspace).
	reg.Add(Contract{
		ID:          "http::POST::/api/auth/login",
		Type:        ContractHTTP,
		Role:        RoleProvider,
		SymbolID:    "tuck-api/handlers/auth.go::Login",
		FilePath:    "tuck-api/handlers/auth.go",
		RepoPrefix:  "tuck-api",
		WorkspaceID: "tuck",
		ProjectID:   "tuck",
		Confidence:  0.9,
	})
	reg.Add(Contract{
		ID:          "http::POST::/api/auth/login",
		Type:        ContractHTTP,
		Role:        RoleConsumer,
		SymbolID:    "tuck-app/api/client.go::DoLogin",
		FilePath:    "tuck-app/api/client.go",
		RepoPrefix:  "tuck-app",
		WorkspaceID: "tuck",
		ProjectID:   "tuck",
		Confidence:  0.9,
	})

	// Workspace B: personal. Provider + consumer that should NEVER
	// be paired with the tuck pair.
	reg.Add(Contract{
		ID:          "http::POST::/api/auth/login",
		Type:        ContractHTTP,
		Role:        RoleProvider,
		SymbolID:    "personal-app/server.go::HandleLogin",
		FilePath:    "personal-app/server.go",
		RepoPrefix:  "personal-app",
		WorkspaceID: "personal",
		ProjectID:   "personal",
		Confidence:  0.9,
	})
	reg.Add(Contract{
		ID:          "http::POST::/api/auth/login",
		Type:        ContractHTTP,
		Role:        RoleConsumer,
		SymbolID:    "personal-cli/main.go::login",
		FilePath:    "personal-cli/main.go",
		RepoPrefix:  "personal-cli",
		WorkspaceID: "personal",
		ProjectID:   "personal",
		Confidence:  0.9,
	})

	return reg
}

// TestCrossWorkspaceContractMatching pins the boundary: `gortex
// contracts check` for `tuck` and `personal` must report orphans,
// never pair them. We expect exactly 2 matches (one per workspace,
// each cross-repo within its own workspace) and zero spurious
// cross-workspace pairs.
func TestCrossWorkspaceContractMatching(t *testing.T) {
	reg := fixtureRegistry(t)
	result := Match(reg)

	if len(result.Matched) != 2 {
		t.Fatalf("expected 2 matches (tuck↔tuck, personal↔personal); got %d. pairs: %s",
			len(result.Matched), summarisePairs(result))
	}
	for _, m := range result.Matched {
		if isCrossFixtureWorkspace(m.Provider.RepoPrefix, m.Consumer.RepoPrefix) {
			t.Fatalf("unexpected cross-workspace pair: provider=%s consumer=%s",
				m.Provider.RepoPrefix, m.Consumer.RepoPrefix)
		}
		if m.Provider.WorkspaceID != m.Consumer.WorkspaceID {
			t.Fatalf("matched pair has mismatched WorkspaceIDs: provider=%q consumer=%q",
				m.Provider.WorkspaceID, m.Consumer.WorkspaceID)
		}
		if !m.CrossRepo {
			// Within each workspace the provider and consumer come
			// from different repos, so CrossRepo must be true.
			t.Fatalf("expected CrossRepo=true within a workspace; got %+v", m)
		}
	}
}

// TestCrossWorkspaceContractOrphansArePerWorkspace pins down the
// negative side of the boundary: the `contracts check` orphans report
// must isolate workspaces. Removing both consumers leaves both
// providers as orphans, but each orphan must remain attributable to
// its own workspace — not silently lumped into a global list that a
// `tuck` operator can't filter.
func TestCrossWorkspaceContractOrphansArePerWorkspace(t *testing.T) {
	reg := NewRegistry()

	reg.Add(Contract{
		ID:          "http::POST::/api/auth/login",
		Role:        RoleProvider,
		SymbolID:    "tuck-api::Login",
		FilePath:    "tuck-api/auth.go",
		RepoPrefix:  "tuck-api",
		WorkspaceID: "tuck",
		ProjectID:   "tuck",
	})
	reg.Add(Contract{
		ID:          "http::POST::/api/auth/login",
		Role:        RoleProvider,
		SymbolID:    "personal::Login",
		FilePath:    "personal/auth.go",
		RepoPrefix:  "personal",
		WorkspaceID: "personal",
		ProjectID:   "personal",
	})

	result := Match(reg)
	if len(result.Matched) != 0 {
		t.Fatalf("expected zero matches when no consumer exists; got %d (%s)",
			len(result.Matched), summarisePairs(result))
	}
	if len(result.OrphanProviders) != 2 {
		t.Fatalf("expected 2 orphan providers, got %d", len(result.OrphanProviders))
	}
	wsSeen := make(map[string]bool)
	for _, op := range result.OrphanProviders {
		wsSeen[op.WorkspaceID] = true
	}
	if !wsSeen["tuck"] || !wsSeen["personal"] {
		t.Fatalf("expected orphan providers preserved for both workspaces; got %+v", wsSeen)
	}
}

func isCrossFixtureWorkspace(a, b string) bool {
	return fixtureWorkspace(a) != fixtureWorkspace(b)
}

func fixtureWorkspace(repoPrefix string) string {
	switch {
	case strings.HasPrefix(repoPrefix, "tuck-"):
		return "tuck"
	case strings.HasPrefix(repoPrefix, "personal-"):
		return "personal"
	default:
		return repoPrefix
	}
}

func summarisePairs(r MatchResult) string {
	var parts []string
	for _, m := range r.Matched {
		parts = append(parts, m.Provider.RepoPrefix+"→"+m.Consumer.RepoPrefix)
	}
	return strings.Join(parts, ", ")
}
