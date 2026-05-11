//go:build llama

package agent

import (
	"context"
	"encoding/json"
	"fmt"

	llm "github.com/zzet/gortex/internal/llm"
)

// GortexChainTools returns GortexTools plus contract / dependency
// navigation tools needed for cross-system call-chain tracing. Used
// for multi-repo questions where the agent must follow a producer →
// consumer → downstream call path.
//
// Note: contract / dependency tools ignore the scope argument and
// search across all indexed repos — chain tracing inherently crosses
// repo boundaries.
func GortexChainTools(b llm.Backend, scope llm.Scope) []Tool {
	base := GortexTools(b, scope)
	return append(base,
		Tool{
			Name:        "contracts",
			Description: `Find HTTP/gRPC/topic API contracts across all repos. args: {"role":"consumer|provider","method":"GET|POST|...","path":"/v1/stats"} — at least one arg required. Returns {"contracts":[{"type":...,"role":...,"repo":...,"method":...,"path":...,"file":...,"symbol_id":...}]}. Use to find producer↔consumer pairs by matching the same path.`,
			Run: func(args map[string]any) (string, error) {
				role, _ := args["role"].(string)
				typ, _ := args["type"].(string)
				method, _ := args["method"].(string)
				path, _ := args["path"].(string)
				if role == "" && typ == "" && method == "" && path == "" {
					return "", fmt.Errorf("contracts: need at least one of role/type/method/path")
				}
				cs, err := b.ListContracts(context.Background(), llm.ContractFilter{
					Role: role, Type: typ, Method: method, Path: path, Limit: 200,
				}, llm.Scope{}) // chain trace: span all repos
				if err != nil {
					return "", err
				}
				out, _ := json.Marshal(map[string]any{"contracts": cs})
				return string(out), nil
			},
		},
		Tool{
			Name:        "get_dependencies",
			Description: `Find what a symbol calls / imports / references (forward). args: {"id":"<symbol id>"}. Returns {"deps":[{"id":...,"kind":"calls|imports|references","file":...,"repo":...}]}. Use after picking a handler symbol to find downstream functions across repos.`,
			Run: func(args map[string]any) (string, error) {
				id, _ := args["id"].(string)
				if id == "" {
					return "", fmt.Errorf(`get_dependencies: missing "id"`)
				}
				deps, err := b.GetDependencies(context.Background(), id, llm.Scope{}, 2, 30)
				if err != nil {
					return "", err
				}
				out, _ := json.Marshal(map[string]any{"deps": deps})
				return string(out), nil
			},
		},
	)
}

// GortexTools returns the standard set of gortex-shaped tools, wired
// to the given backend and scoped to the given repo/project/ref. Arg
// names ("query", "id") match the real gortex MCP tool surface so the
// agent prompt and grammar transfer 1:1 between mock and daemon
// backends — only the data source changes.
func GortexTools(b llm.Backend, scope llm.Scope) []Tool {
	return []Tool{
		{
			Name:        "search_symbols",
			Description: `Fuzzy-find symbols across the indexed graph. args: {"query":"<symbol name or keywords>"}. Returns {"matches":[{"id":...,"kind":...,"path":...,"line":...,"repo":...}]}.`,
			Run: func(args map[string]any) (string, error) {
				q, _ := args["query"].(string)
				if q == "" {
					if n, ok := args["name"].(string); ok {
						q = n // tolerate the legacy mock arg name
					}
				}
				if q == "" {
					return "", fmt.Errorf(`search_symbols: missing "query"`)
				}
				ms, err := b.SearchSymbols(context.Background(), q, scope, 20)
				if err != nil {
					return "", err
				}
				out, _ := json.Marshal(map[string]any{"matches": ms})
				return string(out), nil
			},
		},
		{
			Name:        "get_callers",
			Description: `List functions that call a symbol id. args: {"id":"<symbol id from search_symbols>"}. Returns {"callers":[{"id":...,"file":...,"repo":...}]}.`,
			Run: func(args map[string]any) (string, error) {
				id, _ := args["id"].(string)
				if id == "" {
					return "", fmt.Errorf(`get_callers: missing "id"`)
				}
				cs, err := b.GetCallers(context.Background(), id, scope, 50)
				if err != nil {
					return "", err
				}
				out, _ := json.Marshal(map[string]any{"callers": cs})
				return string(out), nil
			},
		},
	}
}
