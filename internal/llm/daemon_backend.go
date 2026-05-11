package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// DaemonBackend speaks MCP over stdio to a long-running `gortex mcp`
// subprocess. The subprocess itself proxies to the running daemon
// (cross-repo state, watchers, snapshot) — so this backend's
// responses reflect the real graph the user has indexed.
//
// One subprocess per backend; requests are sequential (the agent
// loop only issues one tool call at a time). Concurrency is not
// supported; callers should hold one DaemonBackend per goroutine if
// they need it.
type DaemonBackend struct {
	cmd  *exec.Cmd
	stdin io.WriteCloser
	out  *bufio.Reader
	mu   sync.Mutex
	id   int
}

// NewDaemonBackend spawns `gortex mcp` and performs the MCP handshake.
// gortexBin is the path/name of the gortex binary; empty falls back to
// "gortex" on PATH. extraEnv augments the subprocess environment.
//
// The subprocess is bound to the directory the calling process is in
// — make sure that directory is inside a workspace gortex knows about
// (or pass GORTEX_INDEX via extraEnv) or the handshake will fail.
func NewDaemonBackend(gortexBin string, extraEnv []string) (*DaemonBackend, error) {
	if gortexBin == "" {
		gortexBin = "gortex"
	}
	cmd := exec.Command(gortexBin, "mcp")
	cmd.Env = append(os.Environ(), extraEnv...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	// Drain stderr to /dev/null but keep it readable so the
	// subprocess never blocks on a full pipe.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start gortex mcp: %w", err)
	}

	b := &DaemonBackend{
		cmd:   cmd,
		stdin: stdin,
		out:   bufio.NewReaderSize(stdout, 1<<20),
	}

	// MCP initialise handshake — initialize → initialized notification.
	if _, err := b.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "gortex-llm", "version": "0.1"},
	}); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("mcp initialize: %w", err)
	}
	if err := b.notify("notifications/initialized", map[string]any{}); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("mcp initialized notification: %w", err)
	}
	return b, nil
}

// Close terminates the subprocess. Safe to call more than once.
func (b *DaemonBackend) Close() error {
	if b == nil || b.cmd == nil || b.cmd.Process == nil {
		return nil
	}
	_ = b.stdin.Close()
	_ = b.cmd.Process.Kill()
	_, _ = b.cmd.Process.Wait()
	b.cmd = nil
	return nil
}

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// call sends a JSON-RPC request and waits for a matching response.
// Skips notifications and server-initiated requests on the wire.
func (b *DaemonBackend) call(method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.id++
	id := b.id

	req := jsonrpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	buf, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", method, err)
	}
	buf = append(buf, '\n')
	if _, err := b.stdin.Write(buf); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	for {
		line, err := b.out.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", method, err)
		}
		var resp jsonrpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Non-JSON line (banner?) — ignore.
			continue
		}
		if len(resp.ID) == 0 {
			// Notification or server request — skip.
			continue
		}
		var gotID int
		if err := json.Unmarshal(resp.ID, &gotID); err != nil {
			// Non-numeric id (server request) — skip.
			continue
		}
		if gotID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		return resp.Result, nil
	}
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (b *DaemonBackend) notify(method string, params any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	req := jsonrpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	buf, err := json.Marshal(req)
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	_, err = b.stdin.Write(buf)
	return err
}

// callTool invokes one MCP tool by name and returns the text content
// of the first content block (gortex tools always emit one).
func (b *DaemonBackend) callTool(ctx context.Context, name string, args map[string]any) ([]byte, error) {
	_ = ctx // not honored for cancellation in this minimal stdio path
	args["format"] = "json"
	raw, err := b.call("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, err
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("%s: parse envelope: %w", name, err)
	}
	if result.IsError {
		var msg string
		if len(result.Content) > 0 {
			msg = result.Content[0].Text
		}
		return nil, fmt.Errorf("%s: tool error: %s", name, msg)
	}
	if len(result.Content) == 0 {
		return nil, nil
	}
	return []byte(result.Content[0].Text), nil
}

func (b *DaemonBackend) SearchSymbols(ctx context.Context, query string, scope Scope, limit int) ([]Match, error) {
	args := map[string]any{"query": query}
	if limit > 0 {
		args["limit"] = limit
	}
	applyScope(args, scope)
	raw, err := b.callTool(ctx, "search_symbols", args)
	if err != nil {
		return nil, err
	}
	// gortex search_symbols json: {"results":[{node…}], …} OR
	// {"nodes":[…]} OR (older) a top-level array of nodes.
	var wrap struct {
		Results []rawGraphNode `json:"results"`
		Nodes   []rawGraphNode `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &wrap); err == nil && (wrap.Results != nil || wrap.Nodes != nil) {
		src := wrap.Results
		if len(src) == 0 {
			src = wrap.Nodes
		}
		out := make([]Match, len(src))
		for i, r := range src {
			out[i] = r.match()
		}
		return out, nil
	}
	var arr []rawGraphNode
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]Match, len(arr))
		for i, r := range arr {
			out[i] = r.match()
		}
		return out, nil
	}
	return nil, fmt.Errorf("search_symbols: unrecognised payload shape: %s", truncate(string(raw), 200))
}

// rawGraphNode is the gortex graph-tool node payload. Field names
// follow the canonical gortex shape (file_path, repo_prefix, start_line)
// with fallbacks to older variants (path, file, repo, line) and a
// nested meta sub-doc some versions wrap things in.
type rawGraphNode struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	FilePath   string `json:"file_path"`
	Path       string `json:"path"`
	File       string `json:"file"`
	RepoPrefix string `json:"repo_prefix"`
	Repo       string `json:"repo"`
	StartLine  int    `json:"start_line"`
	Line       int    `json:"line"`
	Meta       struct {
		FilePath   string `json:"file_path"`
		Path       string `json:"path"`
		File       string `json:"file"`
		RepoPrefix string `json:"repo_prefix"`
		Repo       string `json:"repo"`
	} `json:"meta"`
}

func (n rawGraphNode) file() string {
	for _, s := range []string{n.FilePath, n.Path, n.File, n.Meta.FilePath, n.Meta.Path, n.Meta.File} {
		if s != "" {
			return s
		}
	}
	return ""
}

func (n rawGraphNode) repo() string {
	for _, s := range []string{n.RepoPrefix, n.Repo, n.Meta.RepoPrefix, n.Meta.Repo} {
		if s != "" {
			return s
		}
	}
	return ""
}

func (n rawGraphNode) caller() Caller {
	return Caller{ID: n.ID, File: n.file(), Repo: n.repo()}
}

func (n rawGraphNode) match() Match {
	line := n.StartLine
	if line == 0 {
		line = n.Line
	}
	return Match{
		ID:   n.ID,
		Name: n.Name,
		Kind: n.Kind,
		Path: n.file(),
		Line: line,
		Repo: n.repo(),
	}
}

type rawGraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

func (b *DaemonBackend) GetCallers(ctx context.Context, id string, scope Scope, limit int) ([]Caller, error) {
	args := map[string]any{"id": id}
	if limit > 0 {
		args["limit"] = limit
	}
	applyScope(args, scope)
	raw, err := b.callTool(ctx, "get_callers", args)
	if err != nil {
		return nil, err
	}
	// Canonical gortex graph-tool payload: { "nodes":[...], "edges":[...],
	// "total_nodes":N, "total_edges":M, ...}. Each edge has from→to;
	// callers are the nodes that originate edges pointing to `id`.
	var wrap struct {
		Nodes []rawGraphNode `json:"nodes"`
		Edges []rawGraphEdge `json:"edges"`
		// legacy shapes
		Callers []rawGraphNode `json:"callers"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("get_callers: parse: %w", err)
	}

	// Build node lookup for the from→Caller rendering step.
	byID := make(map[string]rawGraphNode, len(wrap.Nodes))
	for _, n := range wrap.Nodes {
		byID[n.ID] = n
	}

	var callerIDs []string
	if len(wrap.Edges) > 0 {
		// Edges from the get_callers tool point caller→callee; the
		// callee is the queried `id`, the caller is `from`.
		seen := make(map[string]struct{})
		for _, e := range wrap.Edges {
			if e.To != id && e.From == id {
				// Some tool versions invert the direction; fall back.
				if _, ok := seen[e.To]; ok {
					continue
				}
				seen[e.To] = struct{}{}
				callerIDs = append(callerIDs, e.To)
				continue
			}
			if e.To != id {
				continue
			}
			if _, ok := seen[e.From]; ok {
				continue
			}
			seen[e.From] = struct{}{}
			callerIDs = append(callerIDs, e.From)
		}
	} else if len(wrap.Callers) > 0 {
		out := make([]Caller, len(wrap.Callers))
		for i, n := range wrap.Callers {
			out[i] = n.caller()
		}
		return out, nil
	} else {
		// No edges payload AND no legacy callers — return every node
		// in the response except the queried one. Conservative
		// fallback for tool versions that just list neighbours.
		for _, n := range wrap.Nodes {
			if n.ID == id {
				continue
			}
			callerIDs = append(callerIDs, n.ID)
		}
	}

	out := make([]Caller, 0, len(callerIDs))
	for _, cid := range callerIDs {
		if n, ok := byID[cid]; ok {
			out = append(out, n.caller())
			continue
		}
		out = append(out, Caller{ID: cid})
	}
	return out, nil
}

func (b *DaemonBackend) GetDependencies(ctx context.Context, id string, scope Scope, depth, limit int) ([]Dep, error) {
	args := map[string]any{"id": id}
	if depth > 0 {
		args["depth"] = depth
	}
	if limit > 0 {
		args["limit"] = limit
	}
	applyScope(args, scope)
	raw, err := b.callTool(ctx, "get_dependencies", args)
	if err != nil {
		return nil, err
	}
	// Canonical shape: { "nodes":[…], "edges":[{from,to,kind}], … }
	// Edges from the queried `id` to other nodes describe dependencies.
	var wrap struct {
		Nodes []rawGraphNode `json:"nodes"`
		Edges []rawGraphEdge `json:"edges"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("get_dependencies: parse: %w", err)
	}
	byID := make(map[string]rawGraphNode, len(wrap.Nodes))
	for _, n := range wrap.Nodes {
		byID[n.ID] = n
	}
	var depIDs []struct {
		ID   string
		Kind string
	}
	seen := map[string]struct{}{}
	for _, e := range wrap.Edges {
		if e.From != id {
			continue
		}
		if _, ok := seen[e.To]; ok {
			continue
		}
		seen[e.To] = struct{}{}
		depIDs = append(depIDs, struct {
			ID   string
			Kind string
		}{e.To, e.Kind})
	}
	// If the graph payload omitted explicit edges, fall back to
	// every non-self node as an opaque dependency.
	if len(depIDs) == 0 {
		for _, n := range wrap.Nodes {
			if n.ID == id {
				continue
			}
			depIDs = append(depIDs, struct {
				ID   string
				Kind string
			}{n.ID, ""})
		}
	}
	out := make([]Dep, 0, len(depIDs))
	for _, d := range depIDs {
		n := byID[d.ID]
		out = append(out, Dep{ID: d.ID, Kind: d.Kind, File: n.file(), Repo: n.repo()})
	}
	return out, nil
}

// rawContract mirrors a single contract entry in gortex's nested
// by_repo / by_type response shape. Field names match the canonical
// node-style (file_path, repo_prefix) with method/path lifted out of
// the meta sub-doc for HTTP contracts.
type rawContract struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Role       string `json:"role"`
	RepoPrefix string `json:"repo_prefix"`
	Repo       string `json:"repo"`
	FilePath   string `json:"file_path"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	SymbolID   string `json:"symbol_id"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Meta       struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	} `json:"meta"`
}

func (r rawContract) toContract() Contract {
	method := r.Method
	if method == "" {
		method = r.Meta.Method
	}
	path := r.Path
	if path == "" {
		path = r.Meta.Path
	}
	// id format for HTTP is "http::METHOD::/path" — fall back to
	// parsing the id when method/path are absent.
	if (method == "" || path == "") && strings.HasPrefix(r.ID, "http::") {
		parts := strings.SplitN(r.ID, "::", 3)
		if len(parts) == 3 {
			if method == "" {
				method = parts[1]
			}
			if path == "" {
				path = parts[2]
			}
		}
	}
	repo := r.RepoPrefix
	if repo == "" {
		repo = r.Repo
	}
	file := r.FilePath
	if file == "" {
		file = r.File
	}
	return Contract{
		Type: r.Type, Role: r.Role, Repo: repo,
		Method: method, Path: path,
		File: file, Line: r.Line,
		ID: r.ID, SymbolID: r.SymbolID,
	}
}

func (b *DaemonBackend) ListContracts(ctx context.Context, f ContractFilter, scope Scope) ([]Contract, error) {
	args := map[string]any{"action": "list"}
	if f.Role != "" {
		args["role"] = f.Role
	}
	if f.Type != "" {
		args["type"] = f.Type
	}
	if f.Limit > 0 {
		args["limit"] = f.Limit
	}
	// Chain tracing inherently spans repos. Disable the daemon's
	// active-project auto-scope unless the caller pinned a specific
	// repo.
	if scope.Repo == "" && scope.Project == "" {
		args["all_repos"] = true
	}
	applyScope(args, scope)
	raw, err := b.callTool(ctx, "contracts", args)
	if err != nil {
		return nil, err
	}
	// gortex contracts payload (canonical, json format):
	//   { "by_repo": { "<repo>": { "contracts": { "<type>": [contract…] }, "total": N } } }
	// Flatten the nested structure into a single slice, then apply
	// client-side method / path filters.
	var wrap struct {
		ByRepo map[string]struct {
			Contracts map[string][]rawContract `json:"contracts"`
		} `json:"by_repo"`
		// Legacy / non-grouped variant.
		Contracts []rawContract `json:"contracts"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("contracts: parse: %w", err)
	}
	var out []Contract
	add := func(rc rawContract) {
		c := rc.toContract()
		if f.Method != "" && c.Method != f.Method {
			return
		}
		if f.Path != "" && c.Path != f.Path {
			return
		}
		out = append(out, c)
	}
	for _, repo := range wrap.ByRepo {
		for _, list := range repo.Contracts {
			for _, rc := range list {
				add(rc)
			}
		}
	}
	for _, rc := range wrap.Contracts {
		add(rc)
	}
	return out, nil
}

func (b *DaemonBackend) ListRepos(ctx context.Context) ([]Repo, error) {
	raw, err := b.callTool(ctx, "list_repos", map[string]any{})
	if err != nil {
		return nil, err
	}
	// list_repos shape varies a bit too. Look for "repos" key, fall
	// back to top-level array.
	var wrap struct {
		Repos []struct {
			Name  string `json:"name"`
			Root  string `json:"root"`
			Path  string `json:"path"`
			Nodes int    `json:"nodes"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.Repos == nil {
		return nil, fmt.Errorf("list_repos: unrecognised payload shape: %s", truncate(string(raw), 200))
	}
	out := make([]Repo, len(wrap.Repos))
	for i, r := range wrap.Repos {
		root := r.Root
		if root == "" {
			root = r.Path
		}
		out[i] = Repo{Name: r.Name, Root: root, Nodes: r.Nodes}
	}
	return out, nil
}

func applyScope(args map[string]any, s Scope) {
	if s.Repo != "" {
		args["repo"] = s.Repo
	}
	if s.Project != "" {
		args["project"] = s.Project
	}
	if s.Ref != "" {
		args["ref"] = s.Ref
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var _ Backend = (*DaemonBackend)(nil)
