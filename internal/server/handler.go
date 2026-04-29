// Package server exposes Gortex MCP tools over HTTP/JSON.
// It provides the general-purpose HTTP handler used by both the standalone
// server command and the eval server.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/server/hub"
	"go.uber.org/zap"
)

// Handler wraps an MCP server's tool dispatch as an HTTP handler.
// All routes live under /v1/*:
//
//	GET  /v1/health       status + node/edge counts + uptime
//	GET  /v1/tools        list of available MCP tools
//	POST /v1/tools/{name} invoke a tool with JSON arguments
//	GET  /v1/stats        graph stats by kind/language
//	GET  /v1/graph        full brief-graph dump (nodes+edges+stats)
//	GET  /v1/events       SSE stream of graph-change events
//	GET  /v1/activity     ring buffer of recent graph-change events
//	GET  /v1/caveats      aggregated hotspots/dead-code/cycles/guards
//	GET  /v1/dashboard    bundled snapshot for the dashboard hero
//	GET  /v1/repos        per-repository node/edge/kind breakdown
//	GET  /v1/processes    discovered execution flows
//	GET  /v1/contracts    detected API/event/URL contracts
//	GET  /v1/communities  community detection result
//	GET  /v1/guards       guard rule evaluation status
//
// /v1/graph scoping (?project/?repo) and /v1/events streaming require
// a ConfigManager and an event hub respectively, wired via
// SetConfigManager / SetEventHub after construction.
type Handler struct {
	mcpServer     *mcpserver.MCPServer
	graph         *graph.Graph
	version       string
	logger        *zap.Logger
	mux           *http.ServeMux
	startTime     time.Time
	eventHub      *hub.Hub               // nil when watch mode is off
	configManager *config.ConfigManager  // nil in single-repo mode
	serverID      string                 // UUID; empty until SetServerID wires it
	activity      *activityBuffer        // ring buffer of recent graph events
	overlays      *daemon.OverlayManager // nil when overlay support is off
	router        *daemon.Router         // nil when single-server (no servers.toml)
}

// NewHandler creates an HTTP handler that dispatches to MCP tools.
func NewHandler(mcpServer *mcpserver.MCPServer, g *graph.Graph, version string, logger *zap.Logger) *Handler {
	h := &Handler{
		mcpServer: mcpServer,
		graph:     g,
		version:   version,
		logger:    logger,
		mux:       http.NewServeMux(),
		startTime: time.Now(),
		activity:  newActivityBuffer(100),
	}
	h.registerRoutes()
	return h
}

// Mux returns the underlying ServeMux so sub-handlers can register
// additional routes (e.g. eval-specific /augment endpoint).
func (h *Handler) Mux() *http.ServeMux { return h.mux }

// Graph returns the graph instance for sub-handlers that need direct access.
func (h *Handler) Graph() *graph.Graph { return h.graph }

// SetEventHub wires the watch-mode event hub so /v1/events can stream
// graph-change events to subscribers, and starts the activity-buffer
// collector so /v1/activity can backfill the dashboard feed. When nil,
// /v1/events responds with a single keepalive frame and closes.
func (h *Handler) SetEventHub(h2 *hub.Hub) {
	h.eventHub = h2
	h.startActivityCollector(h2)
}

// SetConfigManager wires the multi-repo config so /v1/graph can scope
// its dump by ?project=<name>. Without it, only ?repo=<name> filtering
// is available.
func (h *Handler) SetConfigManager(cm *config.ConfigManager) { h.configManager = cm }

// SetServerID attaches a stable UUID to /v1/stats responses so daemon
// clients can detect server restarts (and therefore index-restart
// races) by watching for id changes.
func (h *Handler) SetServerID(id string) { h.serverID = id }

// ServeHTTP implements http.Handler with panic recovery middleware.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			stack := debug.Stack()
			h.logger.Error("panic recovered in HTTP handler",
				zap.Any("panic", rec),
				zap.String("stack", string(stack)),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
			)
			WriteJSONError(w, http.StatusInternalServerError, "internal server error")
		}
	}()
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) registerRoutes() {
	h.mux.HandleFunc("GET /v1/health", h.handleHealth)
	h.mux.HandleFunc("GET /v1/tools", h.handleListTools)
	h.mux.HandleFunc("POST /v1/tools/", h.handleToolCall)
	h.mux.HandleFunc("GET /v1/stats", h.handleStats)
	h.mux.HandleFunc("GET /v1/graph", h.handleGetGraph)
	h.mux.HandleFunc("GET /v1/events", h.handleEvents)
	h.mux.HandleFunc("GET /v1/activity", h.handleActivity)
	h.mux.HandleFunc("GET /v1/caveats", h.handleCaveats)
	h.mux.HandleFunc("GET /v1/dashboard", h.handleDashboard)
	h.mux.HandleFunc("GET /v1/repos", h.handleRepos)
	h.mux.HandleFunc("GET /v1/processes", h.handleProcesses)
	h.mux.HandleFunc("GET /v1/contracts", h.handleContracts)
	h.mux.HandleFunc("GET /v1/contracts/validate", h.handleContractsValidate)
	h.mux.HandleFunc("GET /v1/communities", h.handleCommunities)
	h.mux.HandleFunc("GET /v1/guards", h.handleGuards)
	// Workspace roster discovery. The daemon side calls this when it
	// doesn't yet know which server owns a given workspace; the
	// response lets the daemon's lookup path skip a roundtrip on every
	// subsequent query against the workspace.
	h.mux.HandleFunc("GET /v1/workspaces/{ws}/repos", h.handleWorkspaceRoster)
	// Editor overlay sessions. Clients register a session, push file
	// overlays for in-flight edits, and the server merges them on top
	// of the indexed graph for the duration of the session. The actual
	// merge is the daemon's responsibility (router); these endpoints
	// just expose the OverlayManager to MCP clients.
	h.mux.HandleFunc("POST /v1/overlay/sessions", h.handleOverlayRegister)
	h.mux.HandleFunc("DELETE /v1/overlay/sessions/{id}", h.handleOverlayDrop)
	h.mux.HandleFunc("PUT /v1/overlay/sessions/{id}/files", h.handleOverlayPush)
	h.mux.HandleFunc("DELETE /v1/overlay/sessions/{id}/files", h.handleOverlayDelete)
	h.mux.HandleFunc("GET /v1/overlay/sessions/{id}/files", h.handleOverlayList)
}

// SetOverlayManager wires an OverlayManager into the handler so the
// /v1/overlay/* endpoints become live. Called by the server / daemon
// during construction; nil disables those endpoints (they return 503).
func (h *Handler) SetOverlayManager(m *daemon.OverlayManager) { h.overlays = m }

// SetRouter wires the hybrid-read query router. When set,
// /v1/tools/<name> calls flow through the router;
// remote workspaces proxy via daemon.ServerClient.ProxyTool, local
// ones fall through to the in-process MCP tool dispatch. Nil
// disables routing (the legacy single-server behaviour).
func (h *Handler) SetRouter(r *daemon.Router) { h.router = r }

// peekRouteContext sniffs the `workspace` / `cwd` arg overrides out
// of an MCP tool-call body without disturbing it. The body is left
// available for the local executor to re-parse; we only read enough
// to make a routing decision. Both nested-args (`{"arguments":
// {"workspace": "..."}}`) and flat-args (`{"workspace": "..."}`)
// shapes are handled — the local handler tolerates both, so the
// router does too.
func (h *Handler) peekRouteContext(body []byte, r *http.Request) (scope, cwd string) {
	if len(body) > 0 {
		var nested struct {
			Arguments struct {
				Workspace string `json:"workspace"`
				Cwd       string `json:"cwd"`
			} `json:"arguments"`
			Workspace string `json:"workspace"`
			Cwd       string `json:"cwd"`
		}
		if err := json.Unmarshal(body, &nested); err == nil {
			if nested.Arguments.Workspace != "" {
				scope = nested.Arguments.Workspace
			} else if nested.Workspace != "" {
				scope = nested.Workspace
			}
			if nested.Arguments.Cwd != "" {
				cwd = nested.Arguments.Cwd
			} else if nested.Cwd != "" {
				cwd = nested.Cwd
			}
		}
	}
	if cwd == "" {
		// HTTP clients without an explicit cwd in the body can pass
		// it via header — matches the daemon's session-cwd plumbing.
		cwd = r.Header.Get("X-Gortex-Cwd")
	}
	return scope, cwd
}

// --- /health ---

// HealthResponse is the JSON structure for the /health endpoint.
type HealthResponse struct {
	Status        string  `json:"status"`
	Indexed       bool    `json:"indexed"`
	Nodes         int     `json:"nodes"`
	Edges         int     `json:"edges"`
	Version       string  `json:"version"`
	UptimeSeconds float64 `json:"uptime_seconds"`
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	stats := h.graph.Stats()
	resp := HealthResponse{
		Status:        "ok",
		Indexed:       stats.TotalNodes > 0,
		Nodes:         stats.TotalNodes,
		Edges:         stats.TotalEdges,
		Version:       h.version,
		UptimeSeconds: time.Since(h.startTime).Seconds(),
	}
	WriteJSON(w, http.StatusOK, resp)
}

// --- /tools ---

type toolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (h *Handler) handleListTools(w http.ResponseWriter, _ *http.Request) {
	tools := h.mcpServer.ListTools()
	result := make([]toolInfo, 0, len(tools))
	for name, t := range tools {
		result = append(result, toolInfo{
			Name:        name,
			Description: t.Tool.Description,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	WriteJSON(w, http.StatusOK, result)
}

// --- /tool/{name} ---

// ToolRequest is the expected JSON body for POST /v1/tools/{tool_name}.
// Format is a convenience top-level alias for arguments["format"],
// merged into Arguments before the tool is invoked.
type ToolRequest struct {
	Arguments map[string]any `json:"arguments"`
	Format    string         `json:"format,omitempty"`
}

// ToolResponse wraps the MCP tool call result for JSON serialization.
type ToolResponse struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolContent is a simplified content item from the MCP tool result.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (h *Handler) handleToolCall(w http.ResponseWriter, r *http.Request) {
	toolName := strings.TrimPrefix(r.URL.Path, "/v1/tools/")
	if toolName == "" {
		WriteJSONError(w, http.StatusBadRequest, "missing tool name in path")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		WriteJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	// If a Router is wired, peek the body for `workspace` / `cwd`
	// overrides and let the router
	// decide local vs remote. Local path falls through to the
	// existing in-process tool dispatch below; remote path returns
	// the proxied response verbatim. Only the proxy short-circuits
	// — local routing reuses the legacy code so downstream features
	// (combo / frecency / session state) keep working unchanged.
	if h.router != nil {
		scope, cwd := h.peekRouteContext(body, r)
		out, status, rerr := h.router.RouteToolCall(r.Context(), toolName, body, daemon.RouteContext{
			ScopeOverride: scope,
			Cwd:           cwd,
		})
		if rerr == nil && status > 0 {
			// Proxied to a remote server; relay the upstream
			// response.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(out)
			return
		}
		if rerr != nil && !errors.Is(rerr, daemon.ErrRouteUnresolved) {
			h.logger.Warn("router: proxy failed, falling back to local",
				zap.String("tool", toolName),
				zap.Error(rerr))
		}
		// Either ErrRouteUnresolved (no remote claims this scope) or
		// the local-fast path — both fall through to the in-process
		// dispatch below.
	}

	tool := h.mcpServer.GetTool(toolName)
	if tool == nil {
		available := h.availableToolNames()
		WriteJSON(w, http.StatusNotFound, map[string]any{
			"error":           "tool_not_found",
			"message":         fmt.Sprintf("tool '%s' not found", toolName),
			"available_tools": available,
		})
		return
	}

	var args map[string]any
	var bodyFormat string
	if len(body) > 0 {
		var req ToolRequest
		if err := json.Unmarshal(body, &req); err != nil {
			if err2 := json.Unmarshal(body, &args); err2 != nil {
				WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("malformed JSON: %s", err.Error()))
				return
			}
		} else {
			args = req.Arguments
			bodyFormat = req.Format
			if args == nil {
				_ = json.Unmarshal(body, &args)
			}
		}
	}

	// Merge ?format=<fmt> query param or body-level "format" into the
	// arguments map so tools that understand the argument (gcx, toon,
	// compact, ...) honor it without callers having to nest it under
	// "arguments". Explicit arguments.format still wins.
	if format := firstNonEmpty(r.URL.Query().Get("format"), bodyFormat); format != "" {
		if args == nil {
			args = make(map[string]any)
		}
		if _, ok := args["format"]; !ok {
			args["format"] = format
		}
	}

	mcpReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: args,
		},
	}

	result, err := tool.Handler(r.Context(), mcpReq)
	if err != nil {
		h.logger.Error("tool call failed",
			zap.String("tool", toolName),
			zap.Error(err),
		)
		WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "tool_error",
			"message": err.Error(),
		})
		return
	}

	resp := ToolResponse{
		IsError: result.IsError,
	}
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			resp.Content = append(resp.Content, ToolContent{
				Type: "text",
				Text: tc.Text,
			})
		}
	}

	WriteJSON(w, http.StatusOK, resp)
}

// --- /stats ---

// StatsResponse is the JSON structure for the /v1/stats endpoint.
// ServerID is a per-machine UUID that changes on server restart so
// daemon clients can detect reconnects; StartedAt is the wall-clock
// time of this process start.
type StatsResponse struct {
	ServerID   string         `json:"server_id,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	TotalNodes int            `json:"total_nodes"`
	TotalEdges int            `json:"total_edges"`
	ByKind     map[string]int `json:"by_kind"`
	ByLanguage map[string]int `json:"by_language"`
}

func (h *Handler) handleStats(w http.ResponseWriter, _ *http.Request) {
	stats := h.graph.Stats()
	resp := StatsResponse{
		ServerID:   h.serverID,
		StartedAt:  h.startTime,
		TotalNodes: stats.TotalNodes,
		TotalEdges: stats.TotalEdges,
		ByKind:     stats.ByKind,
		ByLanguage: stats.ByLanguage,
	}
	WriteJSON(w, http.StatusOK, resp)
}

// --- Tool invocation helper ---

// CallTool invokes an MCP tool by name and returns the concatenated text content.
// Returns empty string on error or if the tool is not found.
func (h *Handler) CallTool(ctx context.Context, toolName string, args map[string]any) string {
	tool := h.mcpServer.GetTool(toolName)
	if tool == nil {
		return ""
	}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: args,
		},
	}

	result, err := tool.Handler(ctx, req)
	if err != nil {
		h.logger.Debug("internal tool call failed",
			zap.String("tool", toolName),
			zap.Error(err),
		)
		return ""
	}

	var sb strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// --- Helpers ---

func (h *Handler) availableToolNames() []string {
	tools := h.mcpServer.ListTools()
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// WriteJSON writes a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteJSONError writes a JSON error response.
func WriteJSONError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{
		"error":   http.StatusText(status),
		"message": message,
	})
}

// --- /v1/graph ---

// GraphResponse is the full brief-graph dump returned by /v1/graph.
// Nodes carry only the fields needed for force-directed rendering;
// heavy fields (Meta, QualName, EndLine) are stripped.
type GraphResponse struct {
	Nodes []*graph.Node    `json:"nodes"`
	Edges []*graph.Edge    `json:"edges"`
	Stats graph.GraphStats `json:"stats"`
}

func (h *Handler) handleGetGraph(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))

	allowedPrefixes, err := h.resolveRepoFilter(project, repo)
	if err != nil {
		WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	nodes := h.graph.AllNodes()
	edges := h.graph.AllEdges()

	briefNodes := make([]*graph.Node, 0, len(nodes))
	keptIDs := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if allowedPrefixes != nil {
			if _, ok := allowedPrefixes[n.RepoPrefix]; !ok {
				continue
			}
		}
		briefNodes = append(briefNodes, &graph.Node{
			ID:         n.ID,
			Kind:       n.Kind,
			Name:       n.Name,
			FilePath:   n.FilePath,
			StartLine:  n.StartLine,
			Language:   n.Language,
			RepoPrefix: n.RepoPrefix,
		})
		keptIDs[n.ID] = struct{}{}
	}

	var filteredEdges []*graph.Edge
	if allowedPrefixes == nil {
		filteredEdges = edges
	} else {
		filteredEdges = make([]*graph.Edge, 0, len(edges))
		for _, e := range edges {
			if _, ok := keptIDs[e.From]; !ok {
				continue
			}
			if _, ok := keptIDs[e.To]; !ok {
				continue
			}
			filteredEdges = append(filteredEdges, e)
		}
	}

	// When unfiltered, report full graph stats; otherwise return zero
	// stats — the UI can derive counts from the nodes/edges arrays.
	var stats graph.GraphStats
	if allowedPrefixes == nil {
		stats = h.graph.Stats()
	}

	WriteJSON(w, http.StatusOK, GraphResponse{
		Nodes: briefNodes,
		Edges: filteredEdges,
		Stats: stats,
	})
}

// resolveRepoFilter returns a set of allowed RepoPrefix values based on
// the ?project / ?repo query parameters. Returns nil when no filter
// was requested (meaning "return everything").
func (h *Handler) resolveRepoFilter(project, repo string) (map[string]struct{}, error) {
	if project == "" && repo == "" {
		return nil, nil
	}
	allowed := make(map[string]struct{})
	if project != "" {
		if h.configManager == nil {
			return nil, fmt.Errorf("?project= requires multi-repo config, none loaded")
		}
		repos, err := h.configManager.Global().ResolveRepos(project)
		if err != nil {
			return nil, err
		}
		for _, entry := range repos {
			allowed[filepath.Base(entry.Path)] = struct{}{}
		}
	}
	if repo != "" {
		allowed[repo] = struct{}{}
	}
	return allowed, nil
}

// --- /v1/events (SSE) ---

func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Without a hub (watch mode off), emit a single comment frame and
	// close so clients can distinguish "no events ever" from "stream
	// dropped mid-session".
	if h.eventHub == nil {
		_, _ = fmt.Fprintf(w, ": watch mode not active\n\n")
		flusher.Flush()
		return
	}

	flusher.Flush()

	subID, ch := h.eventHub.Subscribe()
	defer h.eventHub.Unsubscribe(subID)

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "event: graph_change\nid: %d\ndata: %s\n\n",
				ev.Timestamp.UnixMilli(), string(data))
			flusher.Flush()

		case <-keepalive.C:
			_, _ = fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()

		case <-ctx.Done():
			return
		}
	}
}
