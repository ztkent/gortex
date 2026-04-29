package contracts

import (
	"fmt"
	"regexp"
	"strings"
)

// ContractType identifies the protocol or mechanism of a contract.
type ContractType string

const (
	ContractHTTP       ContractType = "http"
	ContractGRPC       ContractType = "grpc"
	ContractGraphQL    ContractType = "graphql"
	ContractTopic      ContractType = "topic"
	ContractWS         ContractType = "ws"
	ContractEnv        ContractType = "env"
	ContractOpenAPI    ContractType = "openapi"
	ContractDependency ContractType = "dependency"
	// ContractDI covers NestJS-style dependency-injection bindings
	// derived from the EdgeProvides / EdgeConsumes edges the
	// TypeScript extractor emits for @Module providers and @Inject
	// consumers. A matched pair has the same `di::<token>` ID on
	// both sides so orphan detection works via the standard matcher.
	ContractDI ContractType = "di"
)

// Role indicates whether a symbol provides or consumes a contract.
type Role string

const (
	RoleProvider Role = "provider"
	RoleConsumer Role = "consumer"
)

// Contract represents a detected API contract (e.g., an HTTP route) attached
// to a symbol in the graph.
type Contract struct {
	ID         string       `json:"id"`
	Type       ContractType `json:"type"`
	Role       Role         `json:"role"`
	SymbolID   string       `json:"symbol_id"`
	FilePath   string       `json:"file_path"`
	Line       int          `json:"line"`
	RepoPrefix string       `json:"repo_prefix,omitempty"`
	// WorkspaceID is the hard-boundary slug. The matcher pairs
	// providers and consumers only inside the same (workspace,
	// project) tuple — across-workspace contracts never pair. Empty
	// falls back to RepoPrefix (the default).
	WorkspaceID string `json:"workspace_id,omitempty"`
	// ProjectID is the soft sub-boundary inside a workspace. Across-
	// project (same-workspace) contracts become orphans rather than
	// paired matches. Empty falls back to RepoPrefix.
	ProjectID  string         `json:"project_id,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
	Confidence float64        `json:"confidence"`
}

// EffectiveWorkspace returns the workspace slug that participates in
// the matcher's boundary check. WorkspaceID wins when set; otherwise
// the default is RepoPrefix (one workspace per repo). Callers
// shouldn't reason about empty strings — always go through this
// helper so the default rule lives in one place.
func (c Contract) EffectiveWorkspace() string {
	if c.WorkspaceID != "" {
		return c.WorkspaceID
	}
	return c.RepoPrefix
}

// EffectiveProject returns the project slug. Same default rule as
// EffectiveWorkspace: ProjectID wins, otherwise RepoPrefix.
func (c Contract) EffectiveProject() string {
	if c.ProjectID != "" {
		return c.ProjectID
	}
	return c.RepoPrefix
}

// paramPatterns matches common path parameter styles and normalises them to {param}.
var paramPatterns = regexp.MustCompile(`:(\w+)|<(\w+(?::\w+)?)>|\{(\w+)\}`)

// tplBasePrefix matches a leading JS/TS template-literal placeholder
// optionally preceded by "/" — e.g. ${API_URL}, /${TUCK_API_URL},
// ${process.env.HOST} — that a consumer glues onto the front of an
// endpoint path to produce the full URL. Stripping it lets consumer
// contracts match providers' canonical "/v1/..." paths.
var tplBasePrefix = regexp.MustCompile(`^/?\$\{[^}]+\}`)

// tplInlineParam matches any remaining inline placeholders in path
// segments — both ${name} (JS/TS, and Dart's braced form) and $name
// (Dart's bare form, e.g. /v1/tucks/$id). Both collapse to {name}
// so consumer paths align with provider route declarations.
var tplInlineParam = regexp.MustCompile(`\$\{([^}]+)\}|\$([a-zA-Z_][a-zA-Z0-9_]*)`)

// NormalizeHTTPPath converts path parameters from various frameworks into the
// canonical {param} form.  Examples:
//
//	/users/:id             -> /users/{id}
//	/users/<int:id>        -> /users/{id}
//	/users/{id}            -> /users/{id}  (no change)
//	${API_URL}/users       -> /users
//	/${TUCK_API_URL}/users -> /users
//	/users/${id}           -> /users/{id}
func NormalizeHTTPPath(path string) string {
	// Strip leading/trailing whitespace and quotes.
	path = strings.Trim(path, " \t\"'`")

	// Strip scheme + authority so a consumer URL like
	// "http://api.example.com/v1/users" matches a provider route like
	// "/v1/users". Without this, the consumer's Contract.ID includes the
	// host and never pairs with the provider's, so cross-service traversal
	// stops at the HTTP call site.
	if idx := strings.Index(path, "://"); idx >= 0 {
		rest := path[idx+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			path = rest[slash:]
		} else {
			path = "/"
		}
	}

	// Strip a leading template-literal placeholder (with optional leading
	// slash) — the base-URL slot that a consumer interpolates. After this
	// the path is the same shape as the provider's route declaration.
	path = tplBasePrefix.ReplaceAllString(path, "")

	// Any remaining inline placeholders are path parameters. Both ${name}
	// (group 1) and Dart-style $name (group 2) collapse to {name} so the
	// canonical param normaliser below treats them uniformly.
	path = tplInlineParam.ReplaceAllStringFunc(path, func(m string) string {
		sub := tplInlineParam.FindStringSubmatch(m)
		name := sub[1]
		if name == "" {
			name = sub[2]
		}
		return "{" + name + "}"
	})

	// Normalise parameter placeholders to positional names — {p1}, {p2}, …
	// HTTP routing identity is positional, not name-based: a provider
	// declaring `DELETE /v1/workspaces/{wid}/tags/{id}` and a consumer
	// calling `DELETE /v1/workspaces/{workspaceId}/tags/{id}` describe
	// the same route, and must hash to the same Contract.ID for
	// cross-repo matching (`contracts check` / `validate`) to work.
	// Keeping the user-written name in the ID is a common source of
	// false orphans across services whose provider and consumer teams
	// chose different names for the same slot.
	var paramCounter int
	path = paramPatterns.ReplaceAllStringFunc(path, func(m string) string {
		paramCounter++
		return fmt.Sprintf("{p%d}", paramCounter)
	})

	// Ensure leading slash.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// Remove trailing slash (except for root).
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}

	return path
}
