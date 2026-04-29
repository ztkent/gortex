package contracts

import "testing"

func TestMatch_ProviderConsumerPairing(t *testing.T) {
	reg := NewRegistry()

	// Two repos, ONE workspace — the model for legitimate cross-repo
	// pairing (microservices behind a shared gateway). Both contracts
	// declare WorkspaceID="acme" so the matcher's boundary check pairs
	// them as a CrossRepo link. Without a shared workspace the
	// boundary would (correctly) treat them as orphans.
	reg.Add(Contract{
		ID:          "http::GET::/api/users",
		Type:        ContractHTTP,
		Role:        RoleProvider,
		SymbolID:    "svc-a::listUsers",
		FilePath:    "routes.go",
		RepoPrefix:  "svc-a",
		WorkspaceID: "acme",
		ProjectID:   "users",
		Confidence:  0.9,
	})
	reg.Add(Contract{
		ID:          "http::GET::/api/users",
		Type:        ContractHTTP,
		Role:        RoleConsumer,
		SymbolID:    "svc-b::fetchUsers",
		FilePath:    "client.go",
		RepoPrefix:  "svc-b",
		WorkspaceID: "acme",
		ProjectID:   "users",
		Confidence:  0.9,
	})

	result := Match(reg)

	if len(result.Matched) != 1 {
		t.Fatalf("expected 1 match, got %d", len(result.Matched))
	}

	m := result.Matched[0]
	if m.ContractID != "http::GET::/api/users" {
		t.Errorf("wrong contract ID: %s", m.ContractID)
	}
	if m.Provider.SymbolID != "svc-a::listUsers" {
		t.Errorf("wrong provider: %s", m.Provider.SymbolID)
	}
	if m.Consumer.SymbolID != "svc-b::fetchUsers" {
		t.Errorf("wrong consumer: %s", m.Consumer.SymbolID)
	}
	if !m.CrossRepo {
		t.Error("expected cross-repo match")
	}

	if len(result.OrphanProviders) != 0 {
		t.Errorf("expected 0 orphan providers, got %d", len(result.OrphanProviders))
	}
	if len(result.OrphanConsumers) != 0 {
		t.Errorf("expected 0 orphan consumers, got %d", len(result.OrphanConsumers))
	}
}

func TestMatch_OrphanProvider(t *testing.T) {
	reg := NewRegistry()

	reg.Add(Contract{
		ID:         "http::POST::/api/orders",
		Type:       ContractHTTP,
		Role:       RoleProvider,
		SymbolID:   "svc-a::createOrder",
		FilePath:   "routes.go",
		RepoPrefix: "svc-a",
	})

	result := Match(reg)

	if len(result.Matched) != 0 {
		t.Errorf("expected 0 matches, got %d", len(result.Matched))
	}
	if len(result.OrphanProviders) != 1 {
		t.Fatalf("expected 1 orphan provider, got %d", len(result.OrphanProviders))
	}
	if result.OrphanProviders[0].SymbolID != "svc-a::createOrder" {
		t.Errorf("wrong orphan provider: %s", result.OrphanProviders[0].SymbolID)
	}
}

func TestMatch_OrphanConsumer(t *testing.T) {
	reg := NewRegistry()

	reg.Add(Contract{
		ID:         "http::GET::/api/missing",
		Type:       ContractHTTP,
		Role:       RoleConsumer,
		SymbolID:   "svc-b::callMissing",
		FilePath:   "client.go",
		RepoPrefix: "svc-b",
	})

	result := Match(reg)

	if len(result.Matched) != 0 {
		t.Errorf("expected 0 matches, got %d", len(result.Matched))
	}
	if len(result.OrphanConsumers) != 1 {
		t.Fatalf("expected 1 orphan consumer, got %d", len(result.OrphanConsumers))
	}
}

func TestMatch_SameRepoNotCrossRepo(t *testing.T) {
	reg := NewRegistry()

	reg.Add(Contract{
		ID:         "http::GET::/api/health",
		Type:       ContractHTTP,
		Role:       RoleProvider,
		SymbolID:   "svc::healthHandler",
		FilePath:   "health.go",
		RepoPrefix: "svc",
	})
	reg.Add(Contract{
		ID:         "http::GET::/api/health",
		Type:       ContractHTTP,
		Role:       RoleConsumer,
		SymbolID:   "svc::selfCheck",
		FilePath:   "check.go",
		RepoPrefix: "svc",
	})

	result := Match(reg)

	if len(result.Matched) != 1 {
		t.Fatalf("expected 1 match, got %d", len(result.Matched))
	}
	if result.Matched[0].CrossRepo {
		t.Error("expected same-repo match (not cross-repo)")
	}
}

func TestMatch_MultipleProvidersSingleConsumer(t *testing.T) {
	reg := NewRegistry()

	// Two providers for the same route (e.g., two microservices behind
	// a gateway). All three repos declare the same WorkspaceID="acme"
	// so the boundary lets the matcher pair them.
	reg.Add(Contract{
		ID:          "http::GET::/api/users",
		Type:        ContractHTTP,
		Role:        RoleProvider,
		SymbolID:    "svc-a::listUsers",
		FilePath:    "a.go",
		RepoPrefix:  "svc-a",
		WorkspaceID: "acme",
		ProjectID:   "users",
	})
	reg.Add(Contract{
		ID:          "http::GET::/api/users",
		Type:        ContractHTTP,
		Role:        RoleProvider,
		SymbolID:    "svc-c::listUsers",
		FilePath:    "c.go",
		RepoPrefix:  "svc-c",
		WorkspaceID: "acme",
		ProjectID:   "users",
	})
	reg.Add(Contract{
		ID:          "http::GET::/api/users",
		Type:        ContractHTTP,
		Role:        RoleConsumer,
		SymbolID:    "svc-b::fetchUsers",
		FilePath:    "b.go",
		RepoPrefix:  "svc-b",
		WorkspaceID: "acme",
		ProjectID:   "users",
	})

	result := Match(reg)

	// Consumer should be paired with each provider.
	if len(result.Matched) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(result.Matched))
	}
	for _, m := range result.Matched {
		if !m.CrossRepo {
			t.Error("expected cross-repo match")
		}
	}
}

func TestMatch_EmptyRegistry(t *testing.T) {
	reg := NewRegistry()
	result := Match(reg)

	if len(result.Matched) != 0 {
		t.Errorf("expected 0 matches, got %d", len(result.Matched))
	}
	if len(result.OrphanProviders) != 0 {
		t.Errorf("expected 0 orphan providers, got %d", len(result.OrphanProviders))
	}
	if len(result.OrphanConsumers) != 0 {
		t.Errorf("expected 0 orphan consumers, got %d", len(result.OrphanConsumers))
	}
}

func TestRegistry_AddAll(t *testing.T) {
	reg := NewRegistry()

	contracts := []Contract{
		{ID: "http::GET::/a", Role: RoleProvider, FilePath: "a.go"},
		{ID: "http::GET::/b", Role: RoleProvider, FilePath: "b.go"},
	}

	reg.AddAll(contracts, "myrepo")

	byRepo := reg.ByRepo("myrepo")
	if len(byRepo) != 2 {
		t.Fatalf("expected 2 contracts in repo, got %d", len(byRepo))
	}
	for _, c := range byRepo {
		if c.RepoPrefix != "myrepo" {
			t.Errorf("expected repo prefix myrepo, got %s", c.RepoPrefix)
		}
	}
}

func TestRegistry_EvictRepo(t *testing.T) {
	reg := NewRegistry()

	reg.Add(Contract{ID: "http::GET::/a", Role: RoleProvider, FilePath: "a.go", RepoPrefix: "svc-a", SymbolID: "fn1"})
	reg.Add(Contract{ID: "http::GET::/a", Role: RoleConsumer, FilePath: "b.go", RepoPrefix: "svc-b", SymbolID: "fn2"})

	removed := reg.EvictRepo("svc-a")
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}

	byID := reg.ByID("http::GET::/a")
	if len(byID) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(byID))
	}
	if byID[0].RepoPrefix != "svc-b" {
		t.Errorf("wrong remaining contract: %s", byID[0].RepoPrefix)
	}
}
