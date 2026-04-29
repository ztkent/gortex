package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"
)

// Router is the "hybrid-read query router". It takes a tool
// invocation plus an optional scope override, decides
// whether the request should run locally or be proxied to a remote
// server, and returns the result bytes the daemon hands back to its
// MCP client.
//
// The router is created once at daemon construction with three
// dependencies:
//
//   - cfg: the parsed `~/.gortex/servers.toml`. nil when the daemon
//     was started without a multi-server config; in that case
//     RouteToolCall always uses the local executor.
//   - rosters: the WorkspaceRosterCache. Populated lazily as
//     workspaces are first looked up; survives across requests so we
//     don't roundtrip on every query.
//   - localSlug: the slug this daemon hosts itself. When the
//     resolved server matches localSlug, the router calls localExec
//     instead of proxying — this is the "hybrid" part. Empty disables
//     the local-fast path; useful for tests and for daemons that only
//     proxy.
//
// Server clients are created lazily on first use and cached; HTTP
// keep-alive in net/http reuses connections across calls.
type Router struct {
	cfg          *ServersConfig
	rosters      *WorkspaceRosterCache
	resolveCwd   CwdResolver
	localSlug    string
	logger       *zap.Logger
	clientsMu    sync.Mutex
	clients      map[string]*ServerClient
	localExecute LocalExecutor
}

// LocalExecutor runs a tool against the local server. The daemon
// supplies one wrapping its in-process MCP server. Returning bytes
// (not a decoded struct) lets this stay independent of the mcp-go
// types and matches what ProxyTool yields, so the caller treats both
// paths identically.
type LocalExecutor func(ctx context.Context, toolName string, body []byte) ([]byte, int, error)

// RouterConfig bundles construction inputs.
type RouterConfig struct {
	Servers      *ServersConfig
	Rosters      *WorkspaceRosterCache
	CwdResolver  CwdResolver
	LocalSlug    string
	LocalExecute LocalExecutor
	Logger       *zap.Logger
}

// NewRouter constructs a Router with the given dependencies. nil
// Logger is replaced with a no-op to keep call sites tidy. nil
// CwdResolver defaults to DefaultCwdResolver.
func NewRouter(rc RouterConfig) *Router {
	r := &Router{
		cfg:          rc.Servers,
		rosters:      rc.Rosters,
		resolveCwd:   rc.CwdResolver,
		localSlug:    rc.LocalSlug,
		logger:       rc.Logger,
		clients:      make(map[string]*ServerClient),
		localExecute: rc.LocalExecute,
	}
	if r.logger == nil {
		r.logger = zap.NewNop()
	}
	if r.resolveCwd == nil {
		r.resolveCwd = DefaultCwdResolver
	}
	return r
}

// RouteContext is the per-call routing input. cwd flows from the
// MCP client (typically the user's pwd at the time of the
// invocation); scopeOverride is what the tool args carried (e.g.
// `workspace: "tuck"`). Either may be empty.
type RouteContext struct {
	Cwd           string
	ScopeOverride string
}

// ErrRouteUnresolved is returned when no server can be picked for a
// request. The caller may surface this as a user-visible "no
// workspace declared and no default server set" error.
var ErrRouteUnresolved = errors.New("no server resolves for this request")

// RouteToolCall implements the priority chain:
//
//  1. If the resolved server slug is empty, the daemon has no remote
//     servers configured: fall back to localExecute.
//  2. If the resolved slug equals localSlug, run locally.
//  3. Otherwise proxy to the remote server.
//
// On a cache miss with a non-empty workspace slug, the router also
// triggers a one-shot FetchWorkspaceRoster call so subsequent
// requests for the same workspace skip the discovery roundtrip.
//
// The body bytes passed through are whatever the caller wants to
// send to the tool — typically the JSON the MCP client posted to
// /v1/tools/<name>. The router does NOT re-marshal them.
func (r *Router) RouteToolCall(ctx context.Context, toolName string, body []byte, route RouteContext) ([]byte, int, error) {
	if r == nil || r.cfg == nil {
		// No multi-server config: only local makes sense.
		return r.callLocal(ctx, toolName, body)
	}
	lookup := RouteForCwd(r.cfg, r.rosters, r.resolveCwd, route.Cwd, route.ScopeOverride)
	r.logger.Debug("router: resolve",
		zap.String("tool", toolName),
		zap.String("workspace", lookup.Workspace),
		zap.String("source", lookup.Source))
	if lookup.Server == nil {
		// We have a workspace but no server for it: try a roster
		// fetch across every configured server so the next call
		// resolves. If none claim it, fall through to local —
		// better than failing outright when localSlug is the only
		// runtime option.
		if lookup.Workspace != "" && r.rosters != nil {
			r.discoverRoster(lookup.Workspace)
			lookup = RouteForCwd(r.cfg, r.rosters, r.resolveCwd, route.Cwd, route.ScopeOverride)
		}
		if lookup.Server == nil {
			if r.localExecute != nil {
				return r.callLocal(ctx, toolName, body)
			}
			return nil, 0, fmt.Errorf("%w: workspace=%q", ErrRouteUnresolved, lookup.Workspace)
		}
	}
	if lookup.Server.Slug == r.localSlug || r.localSlug == "" && lookup.Source == "default" && r.localExecute != nil {
		// Local fast path. Empty localSlug + default-server source
		// also resolves to local because there's no other server we
		// could reach.
		return r.callLocal(ctx, toolName, body)
	}
	if lookup.Server.Slug == r.localSlug {
		return r.callLocal(ctx, toolName, body)
	}
	cli, err := r.clientFor(*lookup.Server)
	if err != nil {
		return nil, 0, err
	}
	return cli.ProxyTool(toolName, body)
}

// callLocal invokes the local executor or returns an error if the
// router was constructed without one.
func (r *Router) callLocal(_ context.Context, toolName string, body []byte) ([]byte, int, error) {
	if r.localExecute == nil {
		return nil, 0, fmt.Errorf("%w: no local executor wired", ErrRouteUnresolved)
	}
	return r.localExecute(context.Background(), toolName, body)
}

// clientFor returns the ServerClient for the given entry, building
// it on first use. The cache key is the slug; entries that share a
// slug are not allowed by ServersConfig.Validate so this is safe.
func (r *Router) clientFor(entry ServerEntry) (*ServerClient, error) {
	r.clientsMu.Lock()
	defer r.clientsMu.Unlock()
	if c, ok := r.clients[entry.Slug]; ok {
		return c, nil
	}
	c, err := NewServerClient(entry)
	if err != nil {
		return nil, err
	}
	r.clients[entry.Slug] = c
	return c, nil
}

// discoverRoster walks every configured server and asks who hosts
// `workspace`. Caches the answer so the next routing call hits the
// cache. Errors are logged but not surfaced — the caller's lookup
// will simply not find a server and the fallback path takes over.
func (r *Router) discoverRoster(workspace string) {
	if r.cfg == nil || r.rosters == nil {
		return
	}
	for _, slug := range r.cfg.AllSlugs() {
		entry := r.cfg.FindBySlug(slug)
		if entry == nil {
			continue
		}
		cli, err := r.clientFor(*entry)
		if err != nil {
			r.logger.Warn("router: build client failed",
				zap.String("slug", slug),
				zap.Error(err))
			continue
		}
		repos, err := cli.FetchWorkspaceRoster(workspace)
		if err != nil {
			if errors.Is(err, ErrWorkspaceNotFound) {
				r.rosters.SetNotFound(slug, workspace)
				continue
			}
			r.logger.Debug("router: roster fetch failed",
				zap.String("slug", slug),
				zap.String("workspace", workspace),
				zap.Error(err))
			continue
		}
		r.rosters.Set(slug, workspace, repos)
		return
	}
}

// EncodeJSON is a small helper for callers that want to marshal a
// scope override + tool args into the body bytes RouteToolCall
// expects. Round-trippable on the proxy side because the local
// server's POST /v1/tools/<name> handler is the same Mux route the
// MCP client originally hit.
func EncodeJSON(v any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(v)
}

// LookupForCwd exposes RouteForCwd against the router's own
// servers.toml + roster cache + cwd resolver. Callers (notably the
// daemon's MCP dispatcher) use this to decide whether a session's
// cwd has any chance of being routable — locally OR remotely —
// before applying their own "is this cwd tracked?" gate. Returns a
// zero LookupResult when the router has no servers.toml.
func (r *Router) LookupForCwd(cwd, scopeOverride string) LookupResult {
	if r == nil || r.cfg == nil {
		return LookupResult{}
	}
	return RouteForCwd(r.cfg, r.rosters, r.resolveCwd, cwd, scopeOverride)
}
