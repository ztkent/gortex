package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// federationReadTools is the allowlist of read traversal tools eligible
// for remote fan-out. Anything not here (and everything in MutatingTools)
// is never federated.
var federationReadTools = map[string]bool{
	"find_usages":          true,
	"get_callers":          true,
	"get_call_chain":       true,
	"find_implementations": true,
	"get_dependents":       true,
	"search_symbols":       true,
	"smart_context":        true,
}

// localSchemaMajor is this daemon's graph-schema major version. A remote
// advertising an incompatible major is refused (never federated). Kept in
// sync with the value /v1/health advertises (server.SchemaVersion).
const localSchemaMajor = 1

// FederationConfig carries the tunable knobs (from .gortex.yaml's
// federation: block). Zero values fall back to sane defaults.
type FederationConfig struct {
	PerRemoteTimeout time.Duration
	Budget           time.Duration
	BreakerThreshold int
	BreakerCooldown  time.Duration
	HealthTTL        time.Duration
	NameKeyedFallback bool
}

func (c FederationConfig) withDefaults() FederationConfig {
	if c.PerRemoteTimeout <= 0 {
		c.PerRemoteTimeout = 2 * time.Second
	}
	if c.Budget <= 0 {
		c.Budget = 3 * time.Second
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = 3
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = 30 * time.Second
	}
	if c.HealthTTL <= 0 {
		c.HealthTTL = 30 * time.Second
	}
	return c
}

// Federator fans an allowlisted read tool out to enabled remotes after
// the local result is in hand, and merges the responses with provenance.
// It NEVER mutates a stored *graph.Node — it works on serialized bytes,
// so json.Unmarshal already yields detached copies; provenance lives only
// in the response-side origins map, never on a node.
type Federator struct {
	cfg       FederationConfig
	clientFor func(ServerEntry) (*ServerClient, error)
	breaker   *circuitBreaker
	health    *healthCache
	logger    *zap.Logger
}

// NewFederator builds a Federator. clientFor reuses the router's client
// cache so connections are shared.
func NewFederator(cfg FederationConfig, clientFor func(ServerEntry) (*ServerClient, error), logger *zap.Logger) *Federator {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Federator{
		cfg:       cfg,
		clientFor: clientFor,
		breaker:   newCircuitBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown),
		health:    newHealthCache(cfg.HealthTTL),
		logger:    logger,
	}
}

// FederationMeta is the response-side provenance block (JSON-only in v1).
type FederationMeta struct {
	RemotesQueried    []string          `json:"remotes_queried"`
	RemotesFailed     []RemoteFailure   `json:"remotes_failed,omitempty"`
	Degraded          bool              `json:"degraded"`
	NamespaceRewrites []string          `json:"namespace_rewrites,omitempty"`
	Origins           map[string]string `json:"origins,omitempty"`
	Note              string            `json:"note,omitempty"`
}

// RemoteFailure records why a remote was not merged.
type RemoteFailure struct {
	Slug   string `json:"slug"`
	Reason string `json:"reason"`
}

type remoteResult struct {
	slug     string
	toolJSON []byte
}

// Augment runs AFTER the local tool result is in hand. It fans the same
// tool+args out to each enabled remote under a bounded deadline, merges
// the responses by per-tool shape, and returns the merged bytes carrying
// a sibling federation{} block + origins map. It NEVER blocks past the
// budget and never lets one remote's failure drop another's results or
// the local result.
func (f *Federator) Augment(ctx context.Context, tool string, body, localResult []byte, remotes []ServerEntry) []byte {
	// Gate: only an allowlisted read tool with at least one enabled
	// remote is federated. With no enabled remotes the local result is
	// returned byte-for-byte — a pure-local install (or an all-disabled
	// roster) is unaffected; the federation{} block + origins map are
	// the additive superset that appears only when there is something to
	// federate.
	if !federationReadTools[tool] || MutatingTools[tool] || len(remotes) == 0 {
		return localResult
	}

	localTool, wrapped := unwrapToolJSON(localResult)

	results, meta := f.fanOut(ctx, tool, body, remotes)

	merged, origins := f.merge(tool, localTool, results)
	meta.Origins = origins

	// Opt-in name-keyed fallback (OFF by default): a bare-name
	// search on each remote, rendered in a SEPARATE name_hits section
	// tagged text_matched — never merged into the primary id-keyed
	// results (name hits have different native ids that ID-dedup cannot
	// collapse). Rarity/length-gated so stdlib/builtin or too-short
	// names don't surface plausible-but-wrong cross-repo hits.
	if f.cfg.NameKeyedFallback && idKeyedTools[tool] {
		if name := bareNameFromBody(body); nameEligible(name) {
			if hits := f.nameKeyedFan(ctx, name, remotes); len(hits) > 0 {
				merged = injectField(merged, "name_hits", hits)
			}
		}
	}

	if len(meta.RemotesFailed) > 0 && meta.Note == "" {
		meta.Note = fmt.Sprintf("%d remote(s) did not answer; results are local%s.",
			len(meta.RemotesFailed), remoteOnlyOrPartial(meta))
	}

	mergedWithMeta := attachFederation(merged, meta)
	if !wrapped {
		return mergedWithMeta
	}
	return rewrapToolJSON(localResult, mergedWithMeta)
}

func remoteOnlyOrPartial(meta FederationMeta) string {
	if len(meta.RemotesQueried) > len(meta.RemotesFailed) {
		return " plus the remotes that answered"
	}
	return ""
}

// fanOut queries each enabled remote concurrently under a per-remote
// deadline and a global budget. A plain WaitGroup (not errgroup) is used
// so one remote's error never cancels the others.
func (f *Federator) fanOut(ctx context.Context, tool string, body []byte, remotes []ServerEntry) ([]remoteResult, FederationMeta) {
	meta := FederationMeta{RemotesQueried: []string{}}
	if len(remotes) == 0 {
		return nil, meta
	}
	budgetCtx, cancel := context.WithTimeout(ctx, f.cfg.Budget)
	defer cancel()

	var (
		mu      sync.Mutex
		results []remoteResult
		wg      sync.WaitGroup
	)
	fail := func(slug, reason string) {
		mu.Lock()
		meta.RemotesFailed = append(meta.RemotesFailed, RemoteFailure{Slug: slug, Reason: reason})
		meta.Degraded = true
		mu.Unlock()
		// Name the failing remote in the logs, not only the JSON block,
		// so a degraded fan-out is visible to operators.
		f.logger.Warn("federation: remote degraded",
			zap.String("tool", tool),
			zap.String("target_slug", slug),
			zap.String("reason", reason))
	}

	audit := auditInfoFrom(ctx)
	for _, rem := range remotes {
		rem := rem
		mu.Lock()
		meta.RemotesQueried = append(meta.RemotesQueried, rem.Slug)
		mu.Unlock()
		// Audit every remote-routed fan-out call (cross-daemon access
		// record), carrying the same {session_id, cwd, tool, target_slug}
		// tuple as the single-remote proxy-routing audit line.
		f.logger.Info("federation: remote-routed call",
			zap.String("tool", tool),
			zap.String("target_slug", rem.Slug),
			zap.String("cwd", audit.Cwd),
			zap.String("session_id", audit.SessionID),
			zap.String("via", "fan-out"))

		if f.breaker.isOpen(rem.Slug) {
			fail(rem.Slug, "circuit_open")
			continue
		}
		cli, err := f.clientFor(rem)
		if err != nil {
			fail(rem.Slug, "client_error")
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			// Capability + readiness negotiation (cached), inside the
			// goroutine so remotes negotiate concurrently. An
			// incompatible major schema is refused; a still-warming
			// remote is bucketed rather than counted as empty success.
			if h, herr := f.health.get(budgetCtx, cli, f.cfg.PerRemoteTimeout); herr == nil {
				if h.SchemaVersion != 0 && h.SchemaVersion != localSchemaMajor {
					fail(rem.Slug, "incompatible_schema")
					return
				}
				if !h.Indexed {
					fail(rem.Slug, "warming")
					return
				}
			}

			rctx, rcancel := context.WithTimeout(budgetCtx, f.cfg.PerRemoteTimeout)
			defer rcancel()
			out, status, err := cli.ProxyToolCtx(rctx, tool, body)
			if err != nil {
				f.breaker.fail(rem.Slug)
				fail(rem.Slug, "unreachable")
				return
			}
			if status >= 400 {
				f.breaker.fail(rem.Slug)
				fail(rem.Slug, fmt.Sprintf("status_%d", status))
				return
			}
			f.breaker.success(rem.Slug)
			toolJSON, _ := unwrapToolJSON(out)
			mu.Lock()
			results = append(results, remoteResult{slug: rem.Slug, toolJSON: toolJSON})
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results, meta
}

// merge dispatches to the per-tool adapter and returns the merged tool
// JSON plus the origins map.
func (f *Federator) merge(tool string, local []byte, remotes []remoteResult) ([]byte, map[string]string) {
	switch tool {
	case "find_usages", "get_callers", "get_call_chain", "get_dependents":
		return mergeSubGraph(local, remotes)
	case "search_symbols":
		return mergeKeyedList(local, remotes, "results")
	case "find_implementations":
		return mergeKeyedList(local, remotes, "implementations")
	case "smart_context":
		return mergeSmartContext(local, remotes)
	default:
		return local, map[string]string{}
	}
}

// mergeSubGraph merges query.SubGraph responses: nodes deduped by string
// ID (local wins), edges by (From,To,Kind). Origins keys each node ID to
// "local" or "remote:<slug>".
func mergeSubGraph(local []byte, remotes []remoteResult) ([]byte, map[string]string) {
	origins := map[string]string{}
	var sg query.SubGraph
	if err := json.Unmarshal(local, &sg); err != nil {
		return local, origins
	}
	seen := make(map[string]bool, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n != nil {
			seen[n.ID] = true
			origins[n.ID] = "local"
		}
	}
	edgeSeen := make(map[string]bool, len(sg.Edges))
	for _, e := range sg.Edges {
		if e != nil {
			edgeSeen[edgeKey(e)] = true
		}
	}
	for _, rr := range remotes {
		var rsg query.SubGraph
		if err := json.Unmarshal(rr.toolJSON, &rsg); err != nil {
			continue
		}
		for _, n := range rsg.Nodes {
			if n == nil || seen[n.ID] {
				continue // local wins on collision
			}
			seen[n.ID] = true
			origins[n.ID] = "remote:" + rr.slug
			sg.Nodes = append(sg.Nodes, n)
		}
		for _, e := range rsg.Edges {
			if e == nil {
				continue
			}
			k := edgeKey(e)
			if edgeSeen[k] {
				continue
			}
			edgeSeen[k] = true
			sg.Edges = append(sg.Edges, e)
		}
	}
	sg.TotalNodes = len(sg.Nodes)
	sg.TotalEdges = len(sg.Edges)
	out, err := json.Marshal(sg)
	if err != nil {
		return local, origins
	}
	return out, origins
}

func edgeKey(e *graph.Edge) string {
	return e.From + "\x00" + e.To + "\x00" + string(e.Kind)
}

// idKeyedTools are the SubGraph traversals whose primary query keys on a
// node id; only these get the optional bare-name fallback fan.
var idKeyedTools = map[string]bool{
	"find_usages":    true,
	"get_callers":    true,
	"get_call_chain": true,
}

// commonNames are too-frequent identifiers a bare-name fan would
// mis-match across repos; the fallback skips them even when enabled.
var commonNames = map[string]bool{
	"len": true, "set": true, "get": true, "new": true, "string": true,
	"error": true, "close": true, "read": true, "write": true, "run": true,
	"stop": true, "start": true, "init": true, "name": true, "value": true,
	"key": true, "data": true, "result": true, "next": true, "size": true,
}

// bareNameFromBody extracts the symbol name from the {"arguments":{"id":...}}
// body, stripping the repo/file prefix and the :: separator.
func bareNameFromBody(body []byte) string {
	var b struct {
		Arguments struct {
			ID string `json:"id"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return ""
	}
	id := b.Arguments.ID
	if i := strings.LastIndex(id, "::"); i >= 0 {
		id = id[i+2:]
	}
	return id
}

// nameEligible gates the bare-name fallback on rarity/length so a common
// or short identifier never fans out.
func nameEligible(name string) bool {
	if len(name) < 4 {
		return false
	}
	return !commonNames[strings.ToLower(name)]
}

// nameKeyedFan issues a per-remote search_symbols query for the bare
// name, capping each remote's contribution and tagging every hit with
// its origin + the text_matched confidence tier.
func (f *Federator) nameKeyedFan(ctx context.Context, name string, remotes []ServerEntry) []any {
	const perRemoteCap = 5
	body, _ := json.Marshal(map[string]any{
		"arguments": map[string]any{"query": name, "limit": perRemoteCap, "format": "json"},
	})
	budgetCtx, cancel := context.WithTimeout(ctx, f.cfg.Budget)
	defer cancel()
	var (
		mu   sync.Mutex
		hits []any
		wg   sync.WaitGroup
	)
	for _, rem := range remotes {
		rem := rem
		if f.breaker.isOpen(rem.Slug) {
			continue
		}
		cli, err := f.clientFor(rem)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rctx, rcancel := context.WithTimeout(budgetCtx, f.cfg.PerRemoteTimeout)
			defer rcancel()
			out, status, err := cli.ProxyToolCtx(rctx, "search_symbols", body)
			if err != nil || status >= 400 {
				return
			}
			tj, _ := unwrapToolJSON(out)
			var rp map[string]any
			if json.Unmarshal(tj, &rp) != nil {
				return
			}
			results, _ := rp["results"].([]any)
			mu.Lock()
			for i, r := range results {
				if i >= perRemoteCap {
					break
				}
				if m, ok := r.(map[string]any); ok {
					m["origin"] = "remote:" + rem.Slug
					m["confidence"] = "text_matched"
					hits = append(hits, m)
				}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return hits
}

// injectField adds a top-level key to a JSON object payload.
func injectField(b []byte, key string, value any) []byte {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return b
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	return out
}

// mergeKeyedList merges a {<key>:[{id,...}], total} payload (search_symbols
// results / find_implementations implementations): concat, dedup by native
// id (local wins), sum total.
func mergeKeyedList(local []byte, remotes []remoteResult, key string) ([]byte, map[string]string) {
	origins := map[string]string{}
	var payload map[string]any
	if err := json.Unmarshal(local, &payload); err != nil {
		return local, origins
	}
	items, _ := payload[key].([]any)
	seen := map[string]bool{}
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if id, ok := m["id"].(string); ok && id != "" {
				seen[id] = true
				origins[id] = "local"
			}
		}
	}
	total := toInt(payload["total"])
	for _, rr := range remotes {
		var rp map[string]any
		if err := json.Unmarshal(rr.toolJSON, &rp); err != nil {
			continue
		}
		ritems, _ := rp[key].([]any)
		for _, it := range ritems {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			if id != "" && seen[id] {
				continue
			}
			if id != "" {
				seen[id] = true
				origins[id] = "remote:" + rr.slug
			}
			items = append(items, m)
		}
		total += toInt(rp["total"])
	}
	payload[key] = items
	payload["total"] = total
	out, err := json.Marshal(payload)
	if err != nil {
		return local, origins
	}
	return out, origins
}

// mergeSmartContext keeps the local manifest authoritative and folds each
// remote's contribution into a SEPARATE remote_context section under its
// slug. The edit_plan is NEVER federated (edits are local-only).
func mergeSmartContext(local []byte, remotes []remoteResult) ([]byte, map[string]string) {
	origins := map[string]string{}
	var payload map[string]any
	if err := json.Unmarshal(local, &payload); err != nil {
		return local, origins
	}
	var sections []any
	for _, rr := range remotes {
		var rp map[string]any
		if err := json.Unmarshal(rr.toolJSON, &rp); err != nil {
			continue
		}
		section := map[string]any{"slug": rr.slug}
		// Carry the remote's relevant symbols / manifest, never its
		// edit_plan.
		for _, k := range []string{"relevant_symbols", "context_manifest", "symbols"} {
			if v, ok := rp[k]; ok {
				section[k] = v
			}
		}
		sections = append(sections, section)
	}
	if len(sections) > 0 {
		payload["remote_context"] = sections
	}
	delete(payload, "")
	out, err := json.Marshal(payload)
	if err != nil {
		return local, origins
	}
	return out, origins
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// attachFederation adds the federation{} block + origins map as siblings
// on a JSON-object tool response. A non-object payload is returned
// unchanged (no place to attach).
func attachFederation(toolJSON []byte, meta FederationMeta) []byte {
	var m map[string]any
	if err := json.Unmarshal(toolJSON, &m); err != nil {
		return toolJSON
	}
	fed := map[string]any{
		"remotes_queried": meta.RemotesQueried,
		"degraded":        meta.Degraded,
	}
	if len(meta.RemotesFailed) > 0 {
		fed["remotes_failed"] = meta.RemotesFailed
	}
	if len(meta.NamespaceRewrites) > 0 {
		fed["namespace_rewrites"] = meta.NamespaceRewrites
	}
	if meta.Note != "" {
		fed["note"] = meta.Note
	}
	m["federation"] = fed
	if len(meta.Origins) > 0 {
		m["origins"] = meta.Origins
	}
	out, err := json.Marshal(m)
	if err != nil {
		return toolJSON
	}
	return out
}

// unwrapToolJSON extracts the tool's JSON payload from the MCP result
// envelope ({content:[{type:text,text:<json>}]}). When the bytes are not
// that envelope, they are returned as-is with wrapped=false.
func unwrapToolJSON(b []byte) (toolJSON []byte, wrapped bool) {
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &env); err != nil || len(env.Content) == 0 {
		return b, false
	}
	if env.Content[0].Type != "text" || env.Content[0].Text == "" {
		return b, false
	}
	return []byte(env.Content[0].Text), true
}

// rewrapToolJSON replaces the text payload of an MCP result envelope with
// newToolJSON, preserving the envelope's other fields (e.g. is_error).
func rewrapToolJSON(envelope, newToolJSON []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(envelope, &m); err != nil {
		return newToolJSON
	}
	content, ok := m["content"].([]any)
	if !ok || len(content) == 0 {
		return newToolJSON
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		return newToolJSON
	}
	first["text"] = string(newToolJSON)
	content[0] = first
	m["content"] = content
	out, err := json.Marshal(m)
	if err != nil {
		return newToolJSON
	}
	return out
}
