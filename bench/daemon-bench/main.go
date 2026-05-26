// daemon-bench: drives the gortex daemon's MCP-over-HTTP transport
// (POST /mcp) through a fixed tool battery and emits per-call wall
// clock + a one-shot health snapshot. Used to compare backends
// (memory vs ladybug) under identical workload from a separate
// process — no in-process shortcuts.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const sessionHeader = "Mcp-Session-Id"

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError,omitempty"`
}

type client struct {
	base    string
	token   string
	session string
	http    *http.Client
	id      int
}

func newClient(base, token string) *client {
	return &client{
		base:  base,
		token: token,
		http:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *client) nextID() int {
	c.id++
	return c.id
}

func (c *client) post(body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", c.base+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.session != "" {
		req.Header.Set(sessionHeader, c.session)
	}
	return c.http.Do(req)
}

func (c *client) call(method string, params any) (*rpcResp, error) {
	body, err := json.Marshal(rpcReq{JSONRPC: "2.0", ID: c.nextID(), Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	resp, err := c.post(body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if sid := resp.Header.Get(sessionHeader); sid != "" {
		c.session = sid
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var r rpcResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode: %w (body=%s)", err, string(raw))
	}
	if r.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", r.Error.Code, r.Error.Message)
	}
	return &r, nil
}

func (c *client) initialize() error {
	_, err := c.call("initialize", map[string]any{
		"protocolVersion": "2026-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "daemon-bench", "version": "1.0.0"},
	})
	if err != nil {
		return err
	}
	return nil
}

type callRecord struct {
	Label       string `json:"label"`
	Tool        string `json:"tool"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	OutputBytes int    `json:"output_bytes"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

type benchCase struct {
	Label string
	Tool  string
	Args  map[string]any
}

func (c *client) tool(tc benchCase) callRecord {
	rec := callRecord{Label: tc.Label, Tool: tc.Tool}
	start := time.Now()
	resp, err := c.call("tools/call", map[string]any{"name": tc.Tool, "arguments": tc.Args})
	rec.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	rec.OK = true
	rec.OutputBytes = len(resp.Result)
	// Decode the tool-call body so we can summarise.
	var tr toolCallResult
	if err := json.Unmarshal(resp.Result, &tr); err == nil {
		if len(tr.Content) > 0 {
			s := tr.Content[0].Text
			if len(s) > 160 {
				s = s[:160] + "…"
			}
			rec.Summary = s
		}
		if tr.IsError {
			rec.OK = false
			rec.Error = "tool returned isError=true"
		}
	}
	return rec
}

func main() {
	addr := flag.String("addr", "http://127.0.0.1:7090", "daemon HTTP base URL")
	token := flag.String("token", "x", "bearer auth token")
	label := flag.String("label", "memory", "tag the run with this backend label")
	jsonOut := flag.String("json", "", "write JSON record to this path")
	flag.Parse()

	c := newClient(*addr, *token)

	if err := c.initialize(); err != nil {
		fmt.Fprintf(os.Stderr, "initialize: %v\n", err)
		os.Exit(2)
	}

	cases := []benchCase{
		{Label: "graph_stats", Tool: "graph_stats", Args: map[string]any{}},
		{Label: "list_repos", Tool: "list_repos", Args: map[string]any{}},
		{Label: "get_repo_outline", Tool: "get_repo_outline", Args: map[string]any{}},
		{Label: "search_symbols(NewServer)", Tool: "search_symbols", Args: map[string]any{"query": "NewServer", "limit": 10}},
		{Label: "search_symbols(handleStreamable)", Tool: "search_symbols", Args: map[string]any{"query": "handleStreamable", "limit": 5}},
		{Label: "search_symbols(daemon controller)", Tool: "search_symbols", Args: map[string]any{"query": "daemon controller", "limit": 8}},
		{Label: "search_text(buildDaemonStreamable)", Tool: "search_text", Args: map[string]any{"query": "buildDaemonStreamableHandler", "limit": 5}},
		{Label: "find_usages(Indexer.RepoPrefix)", Tool: "find_usages", Args: map[string]any{"symbol_id": "internal/indexer/indexer.go::Indexer::RepoPrefix"}},
		{Label: "get_callers(MultiIndexer.IndexAll)", Tool: "get_callers", Args: map[string]any{"symbol_id": "internal/indexer/multi.go::MultiIndexer::IndexAll"}},
		{Label: "get_symbol_source(NewServer)", Tool: "get_symbol_source", Args: map[string]any{"symbol_id": "internal/mcp/server.go::NewServer"}},
		{Label: "get_file_summary(daemon.go)", Tool: "get_file_summary", Args: map[string]any{"path": "cmd/gortex/daemon.go"}},
		{Label: "get_editing_context(server.go)", Tool: "get_editing_context", Args: map[string]any{"path": "cmd/gortex/server.go"}},
		{Label: "smart_context(daemon http transport)", Tool: "smart_context", Args: map[string]any{"task": "wire daemon http auth", "limit": 8}},
		{Label: "analyze(hotspots)", Tool: "analyze", Args: map[string]any{"kind": "hotspots", "limit": 10}},
		{Label: "analyze(pagerank)", Tool: "analyze", Args: map[string]any{"kind": "pagerank", "limit": 10}},
		{Label: "analyze(louvain)", Tool: "analyze", Args: map[string]any{"kind": "louvain", "limit": 10}},
		{Label: "analyze(wcc)", Tool: "analyze", Args: map[string]any{"kind": "wcc", "limit": 10}},
		{Label: "analyze(scc)", Tool: "analyze", Args: map[string]any{"kind": "scc", "limit": 10}},
		{Label: "analyze(kcore)", Tool: "analyze", Args: map[string]any{"kind": "kcore", "limit": 10}},
	}

	total := time.Now()
	out := struct {
		Label   string       `json:"label"`
		Started string       `json:"started"`
		Records []callRecord `json:"records"`
		TotalMS int64        `json:"total_ms"`
	}{Label: *label, Started: time.Now().Format(time.RFC3339)}

	fmt.Printf("== bench: %s (target=%s) ==\n", *label, *addr)
	fmt.Printf("%-44s %10s %10s %s\n", "label", "ms", "bytes", "summary")
	for _, tc := range cases {
		rec := c.tool(tc)
		out.Records = append(out.Records, rec)
		status := "ok"
		if !rec.OK {
			status = "ERR"
		}
		fmt.Printf("%-44s %10d %10d [%s] %s\n", rec.Label, rec.ElapsedMS, rec.OutputBytes, status, rec.Summary)
		if !rec.OK {
			fmt.Printf("    ↳ error: %s\n", rec.Error)
		}
	}
	out.TotalMS = time.Since(total).Milliseconds()
	fmt.Printf("\ntotal_wall_ms=%d  successes=%d/%d\n", out.TotalMS, countOK(out.Records), len(out.Records))

	if *jsonOut != "" {
		body, _ := json.MarshalIndent(out, "", "  ")
		if err := os.WriteFile(*jsonOut, body, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *jsonOut, err)
		}
	}
}

func countOK(rs []callRecord) int {
	n := 0
	for _, r := range rs {
		if r.OK {
			n++
		}
	}
	return n
}
