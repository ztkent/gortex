package mcp

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// agentTTL is how long an agent survives without a heartbeat before it is
// pruned from the registry as dead.
const agentTTL = 2 * time.Minute

// agentRecord is the live presence of one coding agent in a multi-agent
// session. Presence is volatile (heartbeat-driven), so it lives in this
// in-memory registry rather than the persistent code graph; the graph.KindAgent
// kind names the entity so it reads as first-class in tool output.
type agentRecord struct {
	ID          string
	Name        string
	Cursor      string // symbol ID / file the agent is focused on
	Status      string // free-form: editing, reviewing, idle, ...
	LockedPaths []string
	SessionID   string
	LastSeen    time.Time
}

func (a *agentRecord) clone() *agentRecord {
	cp := *a
	cp.LockedPaths = append([]string(nil), a.LockedPaths...)
	return &cp
}

// pathConflict reports a path an agent tried to lock that another live agent
// already holds.
type pathConflict struct {
	Path   string `json:"path"`
	HeldBy string `json:"held_by"`
}

// agentRegistry is a thread-safe, heartbeat-expiring registry of live agents
// plus their advisory path locks. Locks are cooperative: lock() reports
// conflicts but never blocks — it is up to agents to honour them.
type agentRegistry struct {
	mu     sync.Mutex
	agents map[string]*agentRecord
	ttl    time.Duration
	now    func() time.Time
}

func newAgentRegistry() *agentRegistry {
	return &agentRegistry{
		agents: map[string]*agentRecord{},
		ttl:    agentTTL,
		now:    time.Now,
	}
}

// prune drops agents whose last heartbeat is older than the TTL. Caller holds mu.
func (r *agentRegistry) prune() {
	cutoff := r.now().Add(-r.ttl)
	for id, a := range r.agents {
		if a.LastSeen.Before(cutoff) {
			delete(r.agents, id)
		}
	}
}

func (r *agentRegistry) register(id, name, cursor, status, sessionID string, paths []string) *agentRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	if id == "" {
		if sessionID != "" {
			id = sessionID
		} else {
			id = name
		}
	}
	a := r.agents[id]
	if a == nil {
		a = &agentRecord{ID: id}
		r.agents[id] = a
	}
	if name != "" {
		a.Name = name
	}
	if sessionID != "" {
		a.SessionID = sessionID
	}
	if cursor != "" {
		a.Cursor = cursor
	}
	if status != "" {
		a.Status = status
	}
	for _, p := range paths {
		a.LockedPaths = addPath(a.LockedPaths, normalizeLockPath(p))
	}
	a.LastSeen = r.now()
	return a.clone()
}

func (r *agentRegistry) heartbeat(id, cursor, status string) (*agentRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	a := r.agents[id]
	if a == nil {
		return nil, false
	}
	if cursor != "" {
		a.Cursor = cursor
	}
	if status != "" {
		a.Status = status
	}
	a.LastSeen = r.now()
	return a.clone(), true
}

func (r *agentRegistry) lock(id string, paths []string) (locked []string, conflicts []pathConflict, found bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	a := r.agents[id]
	if a == nil {
		return nil, nil, false
	}
	a.LastSeen = r.now()

	owned := map[string]string{} // path -> other agent id
	for oid, oa := range r.agents {
		if oid == id {
			continue
		}
		for _, p := range oa.LockedPaths {
			owned[p] = oid
		}
	}
	for _, raw := range paths {
		p := normalizeLockPath(raw)
		if p == "" {
			continue
		}
		if other, taken := owned[p]; taken {
			conflicts = append(conflicts, pathConflict{Path: p, HeldBy: other})
			continue
		}
		a.LockedPaths = addPath(a.LockedPaths, p)
		locked = append(locked, p)
	}
	return locked, conflicts, true
}

func (r *agentRegistry) unlock(id string, paths []string) (*agentRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	a := r.agents[id]
	if a == nil {
		return nil, false
	}
	a.LastSeen = r.now()
	if len(paths) == 0 {
		a.LockedPaths = nil
	} else {
		drop := map[string]bool{}
		for _, p := range paths {
			drop[normalizeLockPath(p)] = true
		}
		kept := a.LockedPaths[:0]
		for _, p := range a.LockedPaths {
			if !drop[p] {
				kept = append(kept, p)
			}
		}
		a.LockedPaths = append([]string(nil), kept...)
	}
	return a.clone(), true
}

func (r *agentRegistry) unregister(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	if _, ok := r.agents[id]; !ok {
		return false
	}
	delete(r.agents, id)
	return true
}

func (r *agentRegistry) list() []*agentRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	out := make([]*agentRecord, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a.clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func addPath(paths []string, p string) []string {
	if p == "" {
		return paths
	}
	for _, e := range paths {
		if e == p {
			return paths
		}
	}
	return append(paths, p)
}

func normalizeLockPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// view renders an agent as a graph.KindAgent-shaped node for tool output, so
// agents read as first-class graph entities.
func (r *agentRegistry) view(a *agentRecord, now time.Time) map[string]any {
	return map[string]any{
		"id":           "agent::" + a.ID,
		"kind":         string(graph.KindAgent),
		"agent_id":     a.ID,
		"name":         a.Name,
		"cursor":       a.Cursor,
		"status":       a.Status,
		"locked_paths": a.LockedPaths,
		"session_id":   a.SessionID,
		"last_seen":    a.LastSeen.UTC().Format(time.RFC3339),
		"age_seconds":  int(now.Sub(a.LastSeen).Seconds()),
	}
}

func (s *Server) registerAgentRegistryTools() {
	s.addTool(
		mcp.NewTool("agent_registry",
			mcp.WithDescription("Multi-agent coordination registry. Several agents working the same repo announce presence here, see each other's focus (cursor), and claim advisory locks on paths to avoid concurrent-edit conflicts. Agents are first-class graph entities (kind=agent) and expire after a heartbeat TTL. Actions:\n  • list (default): all live agents with cursor + locked paths.\n  • register: announce/refresh this agent (name, optional cursor/status/paths).\n  • heartbeat: keep this agent alive and update its cursor/status.\n  • lock: claim advisory locks on paths; returns any conflicts (paths another live agent already holds).\n  • unlock: release paths (or all when none given).\n  • unregister: leave the session."),
			mcp.WithString("action", mcp.Description("list (default), register, heartbeat, lock, unlock, or unregister")),
			mcp.WithString("id", mcp.Description("Agent ID. Defaults to the MCP session ID on register; required for heartbeat/lock/unlock/unregister.")),
			mcp.WithString("name", mcp.Description("(register) Human-readable agent name.")),
			mcp.WithString("cursor", mcp.Description("(register/heartbeat) Symbol ID or file the agent is currently focused on.")),
			mcp.WithString("status", mcp.Description("(register/heartbeat) Free-form status, e.g. editing / reviewing / idle.")),
			mcp.WithString("paths", mcp.Description("(register/lock/unlock) Comma-separated paths to lock or release.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleAgentRegistry,
	)
}

func (s *Server) handleAgentRegistry(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.agentReg == nil {
		s.agentReg = newAgentRegistry()
	}
	action := strings.ToLower(strings.TrimSpace(req.GetString("action", "list")))
	id := strings.TrimSpace(req.GetString("id", ""))
	paths := splitPathList(req.GetString("paths", ""))
	now := s.agentReg.now()

	switch action {
	case "", "list":
		agents := s.agentReg.list()
		views := make([]map[string]any, 0, len(agents))
		for _, a := range agents {
			views = append(views, s.agentReg.view(a, now))
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"agents": views,
			"total":  len(views),
		})

	case "register":
		name := strings.TrimSpace(req.GetString("name", ""))
		if id == "" {
			id = SessionIDFromContext(ctx)
		}
		a := s.agentReg.register(id, name, req.GetString("cursor", ""), req.GetString("status", ""), SessionIDFromContext(ctx), paths)
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status": "registered",
			"agent":  s.agentReg.view(a, now),
		})

	case "heartbeat":
		if id == "" {
			id = SessionIDFromContext(ctx)
		}
		a, ok := s.agentReg.heartbeat(id, req.GetString("cursor", ""), req.GetString("status", ""))
		if !ok {
			return mcp.NewToolResultError("unknown agent: " + id + " — call action=register first"), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status": "ok",
			"agent":  s.agentReg.view(a, now),
		})

	case "lock":
		if id == "" {
			id = SessionIDFromContext(ctx)
		}
		if len(paths) == 0 {
			return mcp.NewToolResultError("lock requires paths"), nil
		}
		locked, conflicts, ok := s.agentReg.lock(id, paths)
		if !ok {
			return mcp.NewToolResultError("unknown agent: " + id + " — call action=register first"), nil
		}
		if conflicts == nil {
			conflicts = []pathConflict{}
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status":    "ok",
			"locked":    locked,
			"conflicts": conflicts,
		})

	case "unlock":
		if id == "" {
			id = SessionIDFromContext(ctx)
		}
		a, ok := s.agentReg.unlock(id, paths)
		if !ok {
			return mcp.NewToolResultError("unknown agent: " + id), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status": "ok",
			"agent":  s.agentReg.view(a, now),
		})

	case "unregister":
		if id == "" {
			id = SessionIDFromContext(ctx)
		}
		if !s.agentReg.unregister(id) {
			return mcp.NewToolResultError("unknown agent: " + id), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{"status": "unregistered", "id": id})

	default:
		return mcp.NewToolResultError("unknown action: " + action + " (want list/register/heartbeat/lock/unlock/unregister)"), nil
	}
}

func splitPathList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
