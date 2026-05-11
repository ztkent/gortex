package llm

import "context"

// MockBackend serves canned responses keyed on input. Used by the
// bench so the only variable across runs is the model, not real graph
// data. Add cases here as new questions are added.
type MockBackend struct{}

func (MockBackend) SearchSymbols(_ context.Context, query string, _ Scope, _ int) ([]Match, error) {
	switch query {
	case "LoadConfig":
		return []Match{
			{ID: "internal/config.LoadConfig", Kind: "function", Path: "internal/config/config.go"},
			{ID: "internal/config.LoadConfigFromEnv", Kind: "function", Path: "internal/config/env.go"},
		}, nil
	case "RunServer":
		return []Match{
			{ID: "cmd/gortex/server.RunServer", Kind: "function", Path: "cmd/gortex/server/run.go"},
		}, nil
	}
	return nil, nil
}

func (MockBackend) GetCallers(_ context.Context, id string, _ Scope, _ int) ([]Caller, error) {
	switch id {
	case "internal/config.LoadConfig":
		return []Caller{
			{ID: "cmd/gortex.main", File: "cmd/gortex/main.go"},
			{ID: "internal/daemon.Bootstrap", File: "internal/daemon/bootstrap.go"},
		}, nil
	case "cmd/gortex/server.RunServer":
		return []Caller{
			{ID: "cmd/gortex.main", File: "cmd/gortex/main.go"},
		}, nil
	}
	return nil, nil
}

func (MockBackend) ListRepos(_ context.Context) ([]Repo, error) {
	return []Repo{
		{Name: "gortex", Root: "/Users/x/code/gortex", Nodes: 21609},
	}, nil
}

func (MockBackend) GetDependencies(_ context.Context, id string, _ Scope, _, _ int) ([]Dep, error) {
	// MockBackend doesn't synthesise dependencies; cross-system chain
	// tests are daemon-backend-only.
	return nil, nil
}

func (MockBackend) ListContracts(_ context.Context, _ ContractFilter, _ Scope) ([]Contract, error) {
	return nil, nil
}
