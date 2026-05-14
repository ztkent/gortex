package mcp

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/svc"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/savings"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/server/hub"
	"github.com/zzet/gortex/internal/workspace"
)

// Version is set at build time.
var Version = "dev"

// SymbolModification records a single modification event for a symbol.
type SymbolModification struct {
	Timestamp        time.Time `json:"timestamp"`
	SignatureChanged bool      `json:"signature_changed"`
}

// symbolHistory tracks symbol modifications during the current session.
type symbolHistory struct {
	mu      sync.Mutex
	entries map[string][]SymbolModification // symbolID → modifications
}

// Record adds a modification entry for the given symbol.
func (sh *symbolHistory) Record(symbolID string, signatureChanged bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.entries[symbolID] = append(sh.entries[symbolID], SymbolModification{
		Timestamp:        time.Now(),
		SignatureChanged: signatureChanged,
	})
}

// Get returns the modification history for a specific symbol.
func (sh *symbolHistory) Get(symbolID string) []SymbolModification {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	mods := sh.entries[symbolID]
	out := make([]SymbolModification, len(mods))
	copy(out, mods)
	return out
}

// All returns a copy of the entire modification history.
func (sh *symbolHistory) All() map[string][]SymbolModification {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	out := make(map[string][]SymbolModification, len(sh.entries))
	for k, v := range sh.entries {
		cp := make([]SymbolModification, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// Server wraps the MCP server with Gortex-specific tools.
type Server struct {
	mcpServer     *server.MCPServer
	engine        *query.Engine
	graph         *graph.Graph
	indexer       *indexer.Indexer
	watcher       *indexer.Watcher
	multiIndexer  *indexer.MultiIndexer
	configManager *config.ConfigManager
	activeProject string
	// scopeWorkspace / scopeProject default-scope every query at this
	// server instance to a single (workspace, project) tuple. Set by
	// `gortex server --workspace <slug> [--scope-project <slug>]`.
	// Tool handlers consult these via the QueryScope() helper rather
	// than reading the fields directly.
	scopeWorkspace string
	scopeProject   string
	logger         *zap.Logger
	communities    *analysis.CommunityResult
	processes      *analysis.ProcessResult
	analysisMu     sync.RWMutex

	// session / symHistory / tokenStats are the shared-default per-client
	// state for the embedded stdio path (one implicit client per process).
	// Tool handlers reach per-session activity via sessionFor(ctx); that
	// helper returns this default when ctx carries no session ID.
	session    *sessionState
	symHistory *symbolHistory
	tokenStats *tokenStats

	// sessions multiplexes per-client sessionLocal for the daemon
	// transport. When ctx carries a session ID (WithSessionID), handlers
	// resolve through this map; otherwise the shared fields above are
	// used.
	sessions *sessionMap

	guardRules       []config.GuardRule
	contractRegistry *contracts.Registry
	semanticMgr      *semantic.Manager
	feedback         *feedbackManager
	combo            *comboManager
	frecency         *frecencyTracker

	// diagBroadcaster forwards LSP `publishDiagnostics` payloads to
	// MCP clients as `notifications/diagnostics`. Lazy-initialised by
	// SetLSPDiagnosticsBroadcasting; nil until then.
	diagBroadcaster *diagnosticsBroadcaster

	// llmService is the optional in-process LLM agent service backing
	// the `ask` MCP tool (plus future internal callers like wiki/doc
	// generation). nil until SetLLMService is called by the daemon
	// entrypoint with a real service instance — in which case the
	// `ask` tool is registered. In builds without `-tags llama`, the
	// service is a stub that returns errServiceUnavailable.
	llmService *svc.Service

	// resourcesNotifier overrides the live mcpServer when pushing
	// `notifications/resources/updated`. Test-only: production code
	// leaves it nil so the live server is used.
	resourcesNotifier resourcesUpdatedNotifier

	// bind is the active two-entry-point handshake result.
	// nil when no handshake has been performed (legacy callers, tests
	// that construct the server directly). Tool handlers consult it
	// via Bind(); the per-tool scope dispatcher consults it via
	// resolveScope.
	bind *workspace.Bind

	// toolScopes is the per-Server tool-name → ToolScope registry.
	// Populated by registerToolWithScope as tools are added; consulted
	// by ResolveToolScope before each handler runs.
	toolScopes *scopeRegistry
}

// sessionFor returns the session-scoped state for the current request.
// If ctx was wrapped with WithSessionID, the per-session entry is used
// (created on first access). Otherwise the shared default is returned,
// preserving embedded-mode behavior exactly.
//
// Never returns nil — callers can chain `.recordFile(...)` etc.
// unconditionally.
func (s *Server) sessionFor(ctx context.Context) *sessionState {
	id := SessionIDFromContext(ctx)
	if id == "" || s.sessions == nil {
		return s.session
	}
	return s.sessions.get(id).session
}

// ReleaseSession drops per-session state for id. Called by the daemon
// when a proxy disconnects, so idle entries don't accumulate for the
// lifetime of the daemon process. Cascades into the diagnostics
// broadcaster so a disconnecting subscriber's slot is reclaimed.
func (s *Server) ReleaseSession(id string) {
	if id == "" {
		return
	}
	if s.sessions != nil {
		s.sessions.release(id)
	}
	if s.diagBroadcaster != nil {
		s.diagBroadcaster.unsubscribe(id)
	}
}

// sessionState tracks recent agent activity for context recovery after compaction.
type sessionState struct {
	mu             sync.Mutex
	viewedSymbols  []string // recently viewed symbol IDs (most recent first)
	viewedFiles    []string // recently viewed file paths
	modifiedFiles  []string // files modified via edit_symbol
	recentSearches []string // recent search queries
	// clientName is the MCP client identifier (claude-code / cursor / vscode /
	// zed / …) captured from the protocol's `initialize.clientInfo.name`
	// field by the daemon dispatcher. Drives the per-session default
	// wire format: known-decoder clients get `gcx` by default, others
	// fall back to JSON. Empty until the dispatcher sees the
	// `initialize` frame.
	clientName string
	// lastSearch captures the most recent search_symbols call so that a
	// subsequent get_symbol_source / get_editing_context on one of its
	// results can be attributed back to the query — this is the raw input
	// to the combo tracker. Reset on every search.
	lastSearch lastSearchState

	// Workspace scope for this session, resolved lazily from the
	// session cwd on first query and cached here. scopeResolved
	// guards the one-time resolution. scopeBound is true when the
	// session has a cwd: query handlers then confine every result to
	// scopeWorkspaceID — the hard, un-widenable workspace boundary.
	// When scopeBound is true and scopeWorkspaceID names no real
	// workspace, the cwd matched no tracked repo and handlers fail
	// closed rather than widening to the global graph. scopeRepoPrefix
	// / scopeProjectID feed relevance ranking (same-repo > same-project
	// > same-workspace); they never relax the boundary.
	scopeResolved    bool
	scopeBound       bool
	scopeWorkspaceID string
	scopeProjectID   string
	scopeRepoPrefix  string
}

type lastSearchState struct {
	query       string
	returnedIDs map[string]struct{}
	at          time.Time
}

// tokenStats tracks estimated token savings for the current session. When a
// savings.Store is attached, each record() call also increments the persistent
// cumulative totals so "Gortex saved $X this month"-style narratives survive
// server restarts.
type tokenStats struct {
	mu             sync.Mutex
	tokensSaved    int64 // cumulative tokens saved vs reading full files
	tokensReturned int64 // cumulative tokens actually returned
	callCount      int64 // number of source-reading tool invocations
	persistent     *savings.Store
	repoPath       string // forwarded to savings for per-repo aggregation
}

// record adds a single savings observation. node is the symbol whose
// source was returned — its RepoPrefix and Language are folded into the
// per-repo / per-language buckets in the persistent store. node may be
// nil for code paths that don't have a node handle, in which case the
// observation only contributes to top-line totals.
//
// returned and fullFile are token counts (cl100k_base via internal/tokens).
func (ts *tokenStats) record(node *graph.Node, returned, fullFile int64) {
	ts.mu.Lock()
	saved := fullFile - returned
	if saved < 0 {
		saved = 0
	}
	ts.tokensSaved += saved
	ts.tokensReturned += returned
	ts.callCount++
	store := ts.persistent
	fallbackRepo := ts.repoPath
	ts.mu.Unlock()

	// Repo: prefer the node's RepoPrefix so multi-repo daemons attribute
	// correctly to the actual repo the symbol lives in. Fall back to the
	// rootPath captured at InitSavings only when the node has no prefix
	// (single-repo mode).
	repo := fallbackRepo
	var language string
	if node != nil {
		if node.RepoPrefix != "" {
			repo = node.RepoPrefix
		}
		language = node.Language
	}

	// Forward to the persistent store outside our lock — its own mutex guards
	// concurrent writers, and flushing to disk shouldn't block new record()
	// calls on the hot path.
	if store != nil {
		store.AddObservation(repo, language, returned, saved)
	}
}

// snapshot returns a copy of the current counters for inclusion in responses.
func (ts *tokenStats) snapshot() map[string]any {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ratio := 0.0
	if ts.tokensReturned > 0 {
		ratio = float64(ts.tokensSaved+ts.tokensReturned) / float64(ts.tokensReturned)
	}
	return map[string]any{
		"tokens_saved":     ts.tokensSaved,
		"tokens_returned":  ts.tokensReturned,
		"calls_counted":    ts.callCount,
		"efficiency_ratio": math.Round(ratio*10) / 10,
	}
}

const maxSessionItems = 20

func newSessionState() *sessionState {
	return &sessionState{}
}

func (ss *sessionState) recordSymbol(id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.viewedSymbols = prependUnique(ss.viewedSymbols, id, maxSessionItems)
}

func (ss *sessionState) recordFile(path string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.viewedFiles = prependUnique(ss.viewedFiles, path, maxSessionItems)
}

func (ss *sessionState) recordModified(path string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.modifiedFiles = prependUnique(ss.modifiedFiles, path, maxSessionItems)
}

func (ss *sessionState) recordSearch(query string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.recentSearches = prependUnique(ss.recentSearches, query, 10)
}

// recordClientName captures the MCP client name from the `initialize`
// frame. Idempotent — re-init overwrites. Empty input is ignored so a
// late env-var fallback can't clobber a prior authoritative value.
func (ss *sessionState) recordClientName(name string) {
	if name == "" {
		return
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.clientName = name
}

// snapshotClientName returns the captured client name under the
// session lock. Returns empty when the `initialize` frame hasn't
// arrived yet (very early tool calls — rare but possible during
// boot races).
func (ss *sessionState) snapshotClientName() string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.clientName
}

// NoteSessionClient is called by the daemon dispatcher after it
// snoops the MCP `initialize.clientInfo.name` value, so the per-
// session sessionState can default tool wire-format based on the
// client's decoder capability. Idempotent and safe to call before
// the session is registered (no-op until sessionFor materialises
// the entry).
func (s *Server) NoteSessionClient(sessionID, name, version string) {
	if s == nil || sessionID == "" || name == "" {
		return
	}
	if s.sessions == nil {
		// Embedded mode — single shared session; just record on the
		// shared state.
		if s.session != nil {
			s.session.recordClientName(name)
		}
		return
	}
	s.sessions.get(sessionID).session.recordClientName(name)
	_ = version // reserved for per-version capability gates
}

// defaultFormatForClient returns the most-compressed wire format the
// named MCP client is known to decode. Resolution order is gcx >
// toon > json:
//
//   - GCX-capable: claude-code, cursor, vscode (via the @gortex/wire
//     extension that ships with the IDE plugin), zed (gortex-zed
//     plugin links gcx-go), aider, kilocode, opencode, openclaw,
//     codex (Anthropic CLI bundles the gcx decoder).
//   - TOON-capable but no GCX: kept for forward compat; today there
//     is no client we know to be in this bucket. Listed for the
//     mapping shape and as a placeholder — clients can be promoted
//     here when their plugin ships a TOON-only decoder.
//   - Everything else: empty string → JSON (the safe legacy default).
//
// Lower-cased client name is matched. Unknown clients are not a
// failure — they just keep the JSON default until they're added.
func defaultFormatForClient(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude-code",
		"cursor",
		"vscode",
		"zed",
		"aider",
		"kilocode",
		"opencode",
		"openclaw",
		"codex":
		return "gcx"
	}
	return ""
}

// resolveSessionFormat returns the format the current session prefers
// when a tool's `format` arg is absent. Pure read — used by isGCX /
// isTOON when the caller didn't pin a format explicitly.
func (s *Server) resolveSessionFormat(ctx context.Context) string {
	if s == nil {
		return ""
	}
	sess := s.sessionFor(ctx)
	if sess == nil {
		return ""
	}
	return defaultFormatForClient(sess.snapshotClientName())
}

// comboWindow is how long after a search_symbols the session will still
// attribute a consume call (get_symbol_source / get_editing_context) back
// to that search's query for combo tracking. FFF uses a similar concept
// with a T-second window; 5 minutes is long enough for agents that
// interleave many tool calls but short enough that an unrelated later
// consume doesn't get mis-attributed.
const comboWindow = 5 * time.Minute

// recordLastSearch captures the query + the IDs it returned so a later
// consume call can be credited to this query. Truncating to the top N
// results keeps the map small — only symbols the agent can plausibly
// have seen are eligible.
func (ss *sessionState) recordLastSearch(query string, ids []string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	ss.lastSearch = lastSearchState{query: query, returnedIDs: set, at: time.Now()}
}

// attributedQuery returns the query string that should receive credit for
// consuming symbolID, or "" if no recent search eligibly returned it.
// Cleared from the caller's perspective but not from state — a single
// search can legitimately credit several consume calls.
func (ss *sessionState) attributedQuery(symbolID string) string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.lastSearch.query == "" || symbolID == "" {
		return ""
	}
	if time.Since(ss.lastSearch.at) > comboWindow {
		return ""
	}
	if _, ok := ss.lastSearch.returnedIDs[symbolID]; !ok {
		return ""
	}
	return ss.lastSearch.query
}

func (ss *sessionState) snapshot() map[string]any {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return map[string]any{
		"viewed_symbols":  ss.viewedSymbols,
		"viewed_files":    ss.viewedFiles,
		"modified_files":  ss.modifiedFiles,
		"recent_searches": ss.recentSearches,
	}
}

func prependUnique(slice []string, item string, maxLen int) []string {
	// Remove existing occurrence.
	for i, s := range slice {
		if s == item {
			slice = append(slice[:i], slice[i+1:]...)
			break
		}
	}
	// Prepend.
	slice = append([]string{item}, slice...)
	if len(slice) > maxLen {
		slice = slice[:maxLen]
	}
	return slice
}

// MultiRepoOptions holds optional multi-repo components for the Server.
// When nil or zero-valued, the server operates in single-repo mode.
type MultiRepoOptions struct {
	MultiIndexer  *indexer.MultiIndexer
	ConfigManager *config.ConfigManager
	ActiveProject string
	// ScopeWorkspace is the workspace slug filter applied as the
	// default scope on every query. Set by `gortex server --workspace
	// <slug>`. Empty disables the filter.
	ScopeWorkspace string
	// ScopeProject narrows further inside ScopeWorkspace (no effect
	// without it).
	ScopeProject string
}

// NewServer creates an MCP server with all Gortex tools registered.
func NewServer(engine *query.Engine, g *graph.Graph, idx *indexer.Indexer, watcher *indexer.Watcher, logger *zap.Logger, guardRules []config.GuardRule, opts ...MultiRepoOptions) *Server {
	s := &Server{
		mcpServer: server.NewMCPServer("gortex", Version,
			server.WithToolCapabilities(false),
			// subscribe=true lets clients call `resources/subscribe`
			// for bootstrap URIs and receive
			// `notifications/resources/updated` after each graph
			// re-warm. listChanged=false because the resource set is
			// static for the server's lifetime.
			server.WithResourceCapabilities(true, false),
			server.WithRecovery(),
		),
		engine:     engine,
		graph:      g,
		indexer:    idx,
		watcher:    watcher,
		logger:     logger,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{
			entries: make(map[string][]SymbolModification),
		},
		sessions:   newSessionMap(),
		guardRules: guardRules,
		toolScopes: newScopeRegistry(),
	}

	// Apply multi-repo options if provided.
	if len(opts) > 0 {
		o := opts[0]
		s.multiIndexer = o.MultiIndexer
		s.configManager = o.ConfigManager
		s.activeProject = o.ActiveProject
		s.scopeWorkspace = o.ScopeWorkspace
		s.scopeProject = o.ScopeProject
	}

	s.registerCoreTools()
	s.registerCodingTools()
	s.registerAnalysisTools()
	s.registerEnhancementTools()
	s.registerLSPTools()
	s.registerDiagnosticsTools()
	s.registerDataflowTools()
	s.registerASTTools()
	s.registerResources()
	s.registerPrompts()

	// Register multi-repo tools when multi-repo components are available.
	if s.multiIndexer != nil || s.configManager != nil {
		s.registerMultiRepoTools()
	}

	// Workspace-scope bootstrap tools (list_repos, workspace_info).
	// Always registered — they degrade cleanly in single-project mode
	// to a one-member view.
	s.registerWorkspaceTools()

	// LLM-backed tools (`ask`) are NOT registered here — they're
	// gated on SetLLMService being called with an enabled service,
	// which happens post-construction from the daemon entrypoint.

	s.applyDefaultToolScopes()

	return s
}

// SetLLMService attaches the in-process LLM service to the server
// and registers the `ask` MCP tool. Call after NewServer; without
// this, the `ask` MCP tool is not registered (clean degradation for
// builds / deployments without an LLM).
//
// Safe to call with a stub service (built without `-tags llama`) —
// tool registration is a no-op in that case.
//
// Lifecycle: the server does NOT take ownership of the service; the
// daemon entrypoint that constructed the service is responsible for
// calling svc.Close() on shutdown.
func (s *Server) SetLLMService(service *svc.Service) {
	s.llmService = service
	s.registerLLMTools()
}

// SetupLLM is the convenience constructor used by daemon entrypoints.
// It builds an in-process backend wired to this server's engine +
// contract registry, constructs the service from cfg, and attaches
// it. A zero or disabled cfg is a no-op — safe to call
// unconditionally.
//
// Available in both `-tags llama` and pure-Go builds: the stub
// Service is what gets attached without the tag, and the stub
// registerLLMTools then skips registration.
func (s *Server) SetupLLM(cfg llm.Config) {
	cfg = cfg.MergeEnv()
	if !cfg.IsEnabled() {
		return
	}
	backend := svc.NewInProcessBackend(s.engine, s.effectiveContractRegistry)
	s.SetLLMService(svc.NewService(cfg, backend))
}

// InitFeedback initializes the feedback manager for cross-session feedback persistence.
// Call after NewServer with the cache directory and primary repo path.
func (s *Server) InitFeedback(cacheDir, repoPath string) {
	s.feedback = newFeedbackManager(cacheDir, repoPath)
}

// WorkspaceScope returns the default workspace slug filter applied
// to every query (set by `gortex server --workspace`). Empty
// means no scope; tools that consult it should fall back to the
// global multi-workspace view.
func (s *Server) WorkspaceScope() string { return s.scopeWorkspace }

// ProjectScope returns the project slug filter; meaningful only
// when WorkspaceScope() is non-empty.
func (s *Server) ProjectScope() string { return s.scopeProject }

// unresolvedWorkspacePrefix marks a session whose cwd is non-empty but
// resolves to no tracked repo. Used as a QueryOptions.WorkspaceID
// sentinel: it can never equal a real node's WorkspaceID/RepoPrefix,
// so every node is rejected and the session fails closed instead of
// widening to the global graph (SI-3).
const unresolvedWorkspacePrefix = "\x00gortex-unresolved-workspace:"

// sessionScope resolves the workspace/project boundary for the current
// request. When bound is true the session is confined to workspaceID
// and query handlers MUST NOT return data outside it; an empty (or
// sentinel) workspaceID with bound==true means the cwd resolved to no
// tracked repo and handlers fail closed. When bound is false the
// session is unbound (embedded stdio / control client / `gortex
// server --workspace`) and callers fall back to the server-default
// scope.
//
// Resolution happens once per session — derived from the immutable
// session cwd — and is cached on sessionState. repoPrefix is the
// session's home repo, used only for relevance ranking.
func (s *Server) sessionScope(ctx context.Context) (workspaceID, projectID string, bound bool) {
	ss := s.sessionFor(ctx)
	if ss == nil {
		return "", "", false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.scopeResolved {
		return ss.scopeWorkspaceID, ss.scopeProjectID, ss.scopeBound
	}
	ss.scopeResolved = true

	cwd := SessionCWDFromContext(ctx)
	if cwd == "" || s.multiIndexer == nil {
		// No cwd (embedded stdio, control clients) or no multi-repo
		// indexer: unbound — the server-default scope applies.
		return "", "", false
	}

	ss.scopeBound = true
	ws, proj, repoPrefix, ok := s.multiIndexer.ScopeForCWD(cwd)
	if ok {
		ss.scopeWorkspaceID = ws
		ss.scopeProjectID = proj
		ss.scopeRepoPrefix = repoPrefix
	} else {
		// cwd is non-empty but maps to no tracked repo. The daemon
		// dispatcher rejects unreachable cwds before dispatch, so
		// this is defensive: the sentinel matches no node, so the
		// session sees nothing rather than the whole global graph.
		ss.scopeWorkspaceID = unresolvedWorkspacePrefix + cwd
	}
	return ss.scopeWorkspaceID, ss.scopeProjectID, true
}

// sessionLocality returns the session's home repo prefix and project
// slug for relevance ranking — same-repo hits rank above same-project
// above same-workspace. Both empty for unbound sessions.
func (s *Server) sessionLocality(ctx context.Context) (repoPrefix, projectID string) {
	ss := s.sessionFor(ctx)
	if ss == nil {
		return "", ""
	}
	// Ensure scope is resolved (populates scopeRepoPrefix).
	s.sessionScope(ctx)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.scopeRepoPrefix, ss.scopeProjectID
}

// sessionWorkspaceRepos returns {name, path} for every repo in the
// current session's workspace, sorted by name. Empty for an unbound
// session or when the multi-repo indexer is unavailable. Used by the
// introspection tools so they report the session's real boundary.
func (s *Server) sessionWorkspaceRepos(ctx context.Context) []map[string]string {
	sessWS, _, bound := s.sessionScope(ctx)
	if !bound || s.multiIndexer == nil {
		return nil
	}
	prefixes := s.multiIndexer.ReposInWorkspace(sessWS)
	meta := s.multiIndexer.AllMetadata()
	out := make([]map[string]string, 0, len(prefixes))
	for p := range prefixes {
		entry := map[string]string{"name": p}
		if m := meta[p]; m != nil {
			entry["path"] = m.RootPath
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	return out
}

// nodeInSessionScope reports whether a node may be surfaced to the
// current session. For a workspace-bound session only nodes inside
// the session's workspace pass; for an unbound session every node
// passes (the server-default scope applies). This is the universal
// per-node enforcement of the workspace boundary — used by by-id and
// whole-graph handlers that don't route through the engine's scoped
// traversal.
func (s *Server) nodeInSessionScope(ctx context.Context, n *graph.Node) bool {
	sessWS, _, bound := s.sessionScope(ctx)
	if !bound {
		return true
	}
	if n == nil {
		return false
	}
	return query.QueryOptions{WorkspaceID: sessWS}.ScopeAllows(n)
}

// scopedNodes returns the graph nodes visible to the current session:
// every node for an unbound session, only the session workspace's
// nodes for a bound one. Whole-graph handlers (analyze, outline,
// untested, resource rollups, …) iterate this instead of
// graph.AllNodes() so a workspace-bound session can never observe
// another workspace's nodes — not even in aggregate counts.
func (s *Server) scopedNodes(ctx context.Context) []*graph.Node {
	all := s.graph.AllNodes()
	sessWS, _, bound := s.sessionScope(ctx)
	if !bound {
		return all
	}
	opts := query.QueryOptions{WorkspaceID: sessWS}
	out := make([]*graph.Node, 0, len(all))
	for _, n := range all {
		if opts.ScopeAllows(n) {
			out = append(out, n)
		}
	}
	return out
}

// scopedNodeSlice filters an existing node slice to the session's
// workspace. Convenience for handlers that already hold a node list
// (engine list methods that don't take QueryOptions).
func (s *Server) scopedNodeSlice(ctx context.Context, nodes []*graph.Node) []*graph.Node {
	sessWS, _, bound := s.sessionScope(ctx)
	if !bound {
		return nodes
	}
	opts := query.QueryOptions{WorkspaceID: sessWS}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if opts.ScopeAllows(n) {
			out = append(out, n)
		}
	}
	return out
}

// resolveQueryScope resolves the (workspace, project) scope for a
// query. For a workspace-bound session the session's workspace is an
// immovable ceiling: a `workspace` arg can never widen it (cross-
// workspace values are rejected up front in resolveRepoFilter), while
// a `project` arg may narrow within it. For an unbound session it
// merges the server-level default scope with caller-supplied arg
// overrides — the legacy `gortex server --workspace` behaviour.
func (s *Server) resolveQueryScope(ctx context.Context, argWorkspace, argProject string) (workspace, project string) {
	if sessWS, sessProj, bound := s.sessionScope(ctx); bound {
		workspace = sessWS
		project = sessProj
		if argProject != "" {
			project = argProject
		}
		return
	}
	workspace = s.scopeWorkspace
	if argWorkspace != "" {
		workspace = argWorkspace
	}
	project = s.scopeProject
	if argProject != "" {
		project = argProject
	}
	return
}

// scopeFromRequest pulls `workspace` / `project` arg overrides off
// the MCP request and resolves them against the session boundary.
// Convenience wrapper around resolveQueryScope for handlers that take
// the request directly.
func (s *Server) scopeFromRequest(ctx context.Context, req scopeArgGetter) (workspace, project string) {
	return s.resolveQueryScope(ctx, req.GetString("workspace", ""), req.GetString("project", ""))
}

// scopeArgGetter is the minimum interface for reading MCP string
// args; mirrors mcp.CallToolRequest's GetString without forcing
// every caller to import the mcp pkg here.
type scopeArgGetter interface {
	GetString(key, fallback string) string
}

// InitCombo initializes the query→symbol combo tracker. Persists per-repo,
// same cache directory as feedback; zero-effect no-op when either argument
// is empty. mode selects the max-age reap schedule (AI: 7 days, human: 30).
func (s *Server) InitCombo(cacheDir, repoPath string, mode AgentMode) {
	s.combo = newComboManager(cacheDir, repoPath, mode)
}

// InitFrecency initializes the implicit symbol frecency tracker. mode
// selects the decay regime — ModeAI (3-day half-life) for MCP server use;
// ModeHuman (10-day) for interactive sessions.
func (s *Server) InitFrecency(cacheDir, repoPath string, mode AgentMode) {
	s.frecency = newFrecencyTracker(cacheDir, repoPath, mode)
}

// InitSavings wires the persistent token-savings store into tokenStats so
// every source-reading tool call accumulates cumulative totals. Call once
// after NewServer; safe to skip when persistence isn't desired.
//
// Propagates to the sessionMap too so per-session counters (daemon path)
// also flush to the shared persistent store. Without this propagation a
// proxy that connects before InitSavings runs would hold a tokenStats
// with nil persistent and silently drop observations.
func (s *Server) InitSavings(store *savings.Store, repoPath string) {
	if store == nil || s.tokenStats == nil {
		return
	}
	s.tokenStats.mu.Lock()
	s.tokenStats.persistent = store
	s.tokenStats.repoPath = repoPath
	s.tokenStats.mu.Unlock()
	if s.sessions != nil {
		s.sessions.setPersistent(store, repoPath)
	}
}

// tokenStatsFor returns the tokenStats for the current request. Mirrors
// sessionFor: when ctx carries a session ID the per-session counter is
// returned, otherwise the shared default. Per-session counters share
// the same persistent store so disk totals accumulate across clients.
func (s *Server) tokenStatsFor(ctx context.Context) *tokenStats {
	id := SessionIDFromContext(ctx)
	if id == "" || s.sessions == nil {
		return s.tokenStats
	}
	return s.sessions.get(id).tokenStats
}

// FlushSavings forces any buffered savings observations to disk. Called on
// server shutdown to minimize data loss on unclean exits.
func (s *Server) FlushSavings() error {
	store := s.savingsStore()
	if store == nil {
		return nil
	}
	return store.Flush()
}

// StartPeriodicSavingsFlush starts a background ticker that flushes the
// savings store every interval if there are pending observations. Returns
// a stop function for clean shutdown. No-op when persistence isn't wired.
//
// This bounds on-crash data loss to roughly `interval` worth of observations
// even when the call rate is too low to trip the every-N-observations flush.
func (s *Server) StartPeriodicSavingsFlush(interval time.Duration) func() {
	store := s.savingsStore()
	if store == nil {
		return func() {}
	}
	return store.StartPeriodicFlush(interval)
}

// savingsStore extracts the persistent savings store via tokenStats. Returns
// nil when persistence isn't initialized.
func (s *Server) savingsStore() *savings.Store {
	if s == nil || s.tokenStats == nil {
		return nil
	}
	s.tokenStats.mu.Lock()
	store := s.tokenStats.persistent
	s.tokenStats.mu.Unlock()
	return store
}

// cumulativeSavingsSnapshot exposes the persistent savings state for
// inclusion in graph_stats. Returns nil when persistence isn't wired so
// single-shot CLI calls don't emit confusing empty totals.
func (s *Server) cumulativeSavingsSnapshot() map[string]any {
	if s.tokenStats == nil {
		return nil
	}
	s.tokenStats.mu.Lock()
	store := s.tokenStats.persistent
	s.tokenStats.mu.Unlock()
	if store == nil {
		return nil
	}

	snap := store.Snapshot()
	costs := savings.CostAvoidedAll(snap.Totals.TokensSaved)
	return map[string]any{
		"first_seen":       snap.FirstSeen.Format(time.RFC3339),
		"last_updated":     snap.LastUpdated.Format(time.RFC3339),
		"tokens_saved":     snap.Totals.TokensSaved,
		"tokens_returned":  snap.Totals.TokensReturned,
		"calls_counted":    snap.Totals.CallsCounted,
		"cost_avoided_usd": costs,
	}
}

// ExportContext generates a portable context briefing for the given task.
// This is the public API for the CLI command, delegating to the MCP handler.
func (s *Server) ExportContext(ctx context.Context, task, entryPoint, format string, maxSymbols, tokenBudget int) (*mcp.CallToolResult, error) {
	args := map[string]any{
		"task":         task,
		"format":       format,
		"max_symbols":  float64(maxSymbols),
		"token_budget": float64(tokenBudget),
	}
	if entryPoint != "" {
		args["entry_point"] = entryPoint
	}
	argsJSON, _ := json.Marshal(args)
	req := mcp.CallToolRequest{}
	req.Params.Name = "export_context"
	_ = json.Unmarshal(argsJSON, &req.Params.Arguments)
	return s.handleExportContext(ctx, req)
}

// SetBind installs the active workspace bind.
// Called by the `mcp` / `server` / daemon command
// after a successful workspace.Resolve(cwd). nil resets the bind —
// useful for tests.
//
// The bind drives per-tool scope dispatch: every call to a scope: repo
// tool resolves `repo` against bind.Members; every call to a
// scope: workspace tool defaults to the bind's member list;
// scope: fan-out's `["*"]` sentinel expands to bind.Members.
func (s *Server) SetBind(b *workspace.Bind) { s.bind = b }

// Bind returns the active bind, or nil if no handshake has run.
// Exposed for tool handlers that need to enforce the
// workspace-isolation invariant directly (e.g. `list_repos`).
func (s *Server) Bind() *workspace.Bind { return s.bind }

// RegisterToolScope records the ToolScope for toolName so the
// dispatcher can validate `repo` per call. Tools that don't register a
// scope behave as if unscoped — legacy single-repo behavior — until
// every tool is migrated.
func (s *Server) RegisterToolScope(toolName string, scope ToolScope) {
	s.toolScopes.set(toolName, scope)
}

// ToolScope returns the registered scope for toolName and whether one
// has been declared. Used by tests asserting that every tool has a
// scope, and by the dispatcher.
func (s *Server) ToolScope(toolName string) (ToolScope, bool) {
	return s.toolScopes.get(toolName)
}

// ToolScopeMap returns a snapshot of every registered tool name →
// scope-name mapping. Used for diagnostics and for the
// `workspace_info`-style introspection tool (scope: workspace).
func (s *Server) ToolScopeMap() map[string]string {
	return s.toolScopes.snapshot()
}

// RegisteredScopedTools returns the registered tool names sorted
// lexically. Test convenience.
func (s *Server) RegisteredScopedTools() []string {
	return s.toolScopes.allTools()
}

// ResolveToolScope is the public entry point used by the dispatcher:
// looks up the tool's scope, then validates the request's `repo`
// argument against it. Returns either the resolved repo set or a
// structured protocol error suitable for the caller to surface
// verbatim.
//
// When the tool isn't in the registry, returns a nil ScopedRepos and
// nil error — callers treat this as "unscoped, do not enforce" so
// gradual migration doesn't break anything.
func (s *Server) ResolveToolScope(toolName string, repo any) (*ScopedRepos, *mcp.CallToolResult) {
	scope, ok := s.toolScopes.get(toolName)
	if !ok {
		return nil, nil
	}
	return ResolveScopedRepos(scope, s.bind, repo)
}

// RunAnalysis performs community detection and process discovery on
// the current graph, then pushes a `notifications/resources/updated`
// for every bootstrap resource so subscribed clients can refresh
// without polling.
func (s *Server) RunAnalysis() {
	s.analysisMu.Lock()
	s.communities = analysis.DetectCommunities(s.graph)
	s.processes = analysis.DiscoverProcesses(s.graph)
	s.analysisMu.Unlock()

	// Bootstrap-resource payloads (graph_stats, index_health, etc.)
	// can change after re-warm even when the analysis itself didn't
	// — node counts move on every reindex. Fire updates regardless.
	s.notifyBootstrapResourcesUpdated()
}

func (s *Server) getCommunities() *analysis.CommunityResult {
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	return s.communities
}

func (s *Server) getProcesses() *analysis.ProcessResult {
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	return s.processes
}

// WatchForReanalysis subscribes to hub events and re-runs analysis after
// a debounce period of inactivity. It runs in a background goroutine.
func (s *Server) WatchForReanalysis(h *hub.Hub, debounceMs int) {
	subID, events := h.Subscribe()
	debounce := time.Duration(debounceMs) * time.Millisecond

	go func() {
		var timer *time.Timer
		for ev := range events {
			_ = ev // any event triggers reanalysis
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, func() {
				s.logger.Info("re-running analysis after graph change")
				s.RunAnalysis()
			})
		}
		// Channel closed — hub is shutting down.
		if timer != nil {
			timer.Stop()
		}
		_ = subID
	}()
}

// ServeStdio starts the MCP server on stdin/stdout.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcpServer)
}

// MCPServer returns the underlying MCP server instance.
// This is used by the eval-server to wire tool dispatch into an HTTP handler.
func (s *Server) MCPServer() *server.MCPServer {
	return s.mcpServer
}

// SetContractRegistry sets an explicit contract registry override for the MCP
// server. Used by single-indexer callers and tests. In multi-repo mode the
// server prefers a freshly-merged registry from MultiIndexer (see
// effectiveContractRegistry) so that repos tracked or re-indexed at runtime
// are visible immediately.
func (s *Server) SetContractRegistry(r *contracts.Registry) {
	s.contractRegistry = r
}

// effectiveContractRegistry resolves the current contract registry. It prefers
// a live view over any snapshot: in multi-repo mode it re-merges per-repo
// registries on every call so that track_repository / index_repository at
// runtime take effect without a restart. Falls back to the single indexer,
// then to the explicit override.
func (s *Server) effectiveContractRegistry() *contracts.Registry {
	if s.multiIndexer != nil {
		return s.multiIndexer.MergedContractRegistry()
	}
	if s.indexer != nil {
		if cr := s.indexer.ContractRegistry(); cr != nil {
			return cr
		}
	}
	return s.contractRegistry
}

// SetSemanticManager sets the semantic enrichment manager for the MCP server.
func (s *Server) SetSemanticManager(m *semantic.Manager) {
	s.semanticMgr = m
}

// SemanticManager returns the semantic enrichment manager.
func (s *Server) SemanticManager() *semantic.Manager {
	return s.semanticMgr
}

// SetWatcher sets the watcher after background initialization and registers
// a symbol change callback to record modifications in symbolHistory.
func (s *Server) SetWatcher(w *indexer.Watcher) {
	s.watcher = w

	// Register callback to track symbol modifications for get_symbol_history.
	w.OnSymbolChange(func(filePath string, oldSymbols, newSymbols []*graph.Node) {
		oldMap := make(map[string]string, len(oldSymbols)) // ID → signature
		for _, n := range oldSymbols {
			sig, _ := n.Meta["signature"].(string)
			oldMap[n.ID] = sig
		}

		newMap := make(map[string]string, len(newSymbols)) // ID → signature
		for _, n := range newSymbols {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sig, _ := n.Meta["signature"].(string)
			newMap[n.ID] = sig
		}

		// Detect modified symbols (present in both old and new with changed signature).
		for id, oldSig := range oldMap {
			if newSig, exists := newMap[id]; exists {
				sigChanged := oldSig != newSig
				s.symHistory.Record(id, sigChanged)
			}
		}

		// Detect removed symbols (in old but not in new).
		for id := range oldMap {
			if _, exists := newMap[id]; !exists {
				s.symHistory.Record(id, true)
			}
		}

		// Detect added symbols (in new but not in old).
		for id := range newMap {
			if _, exists := oldMap[id]; !exists {
				s.symHistory.Record(id, false)
			}
		}
	})
}
