// Package llm — backend abstraction.
//
// Backend isolates the agent loop from where graph data actually comes
// from. Two implementations ship: a Mock with canned responses (for
// hermetic tests and the bench) and a Daemon client that speaks
// HTTP-over-UDS to the running gortex daemon. Same interface, swap
// freely.
//
// Field names on Match / Caller match the JSON keys the real gortex
// MCP tools return — the agent's tool wrappers serialise these back
// to JSON and feed the result to the model, so the model sees the
// same shape regardless of which backend is in use.
package llm

import "context"

// Scope narrows a query to a subset of the multi-repo graph. Empty
// fields mean "no filter" — i.e., the daemon's default scope.
type Scope struct {
	Repo    string `json:"repo,omitempty"`
	Project string `json:"project,omitempty"`
	Ref     string `json:"ref,omitempty"`
}

// Match is one hit from search_symbols.
type Match struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
	Path string `json:"path,omitempty"`
	Line int    `json:"line,omitempty"`
	Repo string `json:"repo,omitempty"`
}

// Caller is one entry from get_callers.
type Caller struct {
	ID   string `json:"id"`
	File string `json:"file,omitempty"`
	Repo string `json:"repo,omitempty"`
}

// Repo is one entry from list_repos.
type Repo struct {
	Name  string `json:"name"`
	Root  string `json:"root,omitempty"`
	Nodes int    `json:"nodes,omitempty"`
}

// Contract is one HTTP/gRPC/topic/etc producer or consumer site.
// Field set mirrors the `contracts` MCP tool's response shape.
type Contract struct {
	Type     string `json:"type"`           // http | grpc | graphql | topic | ws | …
	Role     string `json:"role"`           // provider | consumer
	Repo     string `json:"repo,omitempty"`
	Method   string `json:"method,omitempty"`
	Path     string `json:"path,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	ID       string `json:"id,omitempty"`        // contract id (e.g. "http::GET::/v1/stats")
	SymbolID string `json:"symbol_id,omitempty"` // the handler / caller node id
}

// ContractFilter narrows ListContracts. Empty fields = no filter.
type ContractFilter struct {
	Role    string // provider | consumer
	Type    string // http | grpc | …
	Method  string
	Path    string
	Limit   int
}

// Dep is one outgoing dependency (call/import/reference) of a symbol.
type Dep struct {
	ID   string `json:"id"`
	Kind string `json:"kind,omitempty"` // calls | imports | references
	File string `json:"file,omitempty"`
	Repo string `json:"repo,omitempty"`
}

// Backend is the surface area the agent's gortex-shaped tools need.
// Keep it tight: every method here corresponds to a real gortex MCP
// tool, with arg names matching the MCP tool's arg names.
type Backend interface {
	SearchSymbols(ctx context.Context, query string, scope Scope, limit int) ([]Match, error)
	GetCallers(ctx context.Context, id string, scope Scope, limit int) ([]Caller, error)
	GetDependencies(ctx context.Context, id string, scope Scope, depth, limit int) ([]Dep, error)
	ListContracts(ctx context.Context, filter ContractFilter, scope Scope) ([]Contract, error)
	ListRepos(ctx context.Context) ([]Repo, error)
}
