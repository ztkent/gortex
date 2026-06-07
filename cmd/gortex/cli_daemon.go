package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/daemon"
)

// ErrNoExecutor signals that no warm daemon owns the repo and no explicit
// daemonless path (--oneshot) was selected — the caller decides whether to
// fall back (Stage 1) or refuse (Stage 3).
var ErrNoExecutor = errors.New("no warm daemon and --oneshot not set")

// ErrRepoNotTracked is the typed form of the daemon's repo_not_tracked
// refusal, distinguished so a CLI command can fall back rather than treat
// it as a hard error.
var ErrRepoNotTracked = errors.New("repository not tracked by the daemon")

// cliExecutor runs a registered MCP tool by name and returns its raw
// result JSON (the same payload the MCP server returns).
type cliExecutor interface {
	CallTool(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error)
	Close() error
}

// daemonExecutor relays a one-shot tools/call over the daemon's AF_UNIX
// ModeMCP channel — the same warm graph the editor proxies hit, no cold
// index. It pins the JSON wire format so per-tool decoding is stable.
type daemonExecutor struct {
	client *daemon.Client
	nextID int
}

func (d *daemonExecutor) CallTool(_ context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	d.nextID++
	frame, err := buildToolCallFrame(d.nextID, tool, args)
	if err != nil {
		return nil, err
	}
	if err := d.client.WriteMCPFrame(frame); err != nil {
		return nil, err
	}
	resp, err := d.client.ReadMCPFrame()
	if err != nil {
		return nil, err
	}
	return extractToolResult(resp)
}

// buildToolCallFrame constructs the JSON-RPC tools/call frame, pinning the
// JSON wire format so the daemon's per-client GCX/TOON auto-selection does
// not defeat the per-tool decode.
func buildToolCallFrame(id int, tool string, args map[string]any) ([]byte, error) {
	if args == nil {
		args = map[string]any{}
	}
	// Default to JSON, but honour a caller-provided format (e.g.
	// mermaid / dot for diagram output) so the CLI can request the
	// daemon's other renderers.
	if _, ok := args["format"]; !ok {
		args["format"] = "json"
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
}

func (d *daemonExecutor) Close() error { return d.client.Close() }

// extractToolResult unwraps a JSON-RPC tools/call response: a
// repo_not_tracked error maps to the typed sentinel, any other error is
// surfaced verbatim, and a success returns the tool's JSON payload (the
// text of the first content block).
func extractToolResult(resp []byte) (json.RawMessage, error) {
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				ErrorCode string `json:"error_code"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &rpc); err != nil {
		return nil, fmt.Errorf("decode daemon response: %w", err)
	}
	if rpc.Error != nil {
		if rpc.Error.Data.ErrorCode == "repo_not_tracked" {
			return nil, ErrRepoNotTracked
		}
		return nil, errors.New(rpc.Error.Message)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(rpc.Result, &res); err != nil || len(res.Content) == 0 {
		return rpc.Result, nil
	}
	payload := json.RawMessage(res.Content[0].Text)
	if res.IsError {
		return nil, errors.New(res.Content[0].Text)
	}
	return payload, nil
}

// resolveExecutor decides where a CLI graph query runs. This Stage-1 slice
// covers the daemon-first case (a warm daemon that owns the repo) and the
// no-executor case; --oneshot and autostart land with the shared
// constructor and the autostart primitive.
func resolveExecutor(repoPath string) (cliExecutor, error) {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	if !daemon.IsRunning() {
		return nil, ErrNoExecutor
	}
	if !daemonOwnsRepo(abs) {
		return nil, ErrNoExecutor
	}
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeMCP, ClientName: "cli", CWD: abs})
	if err != nil {
		return nil, ErrNoExecutor
	}
	return &daemonExecutor{client: c}, nil
}

// daemonOwnsRepo reports whether the running daemon tracks a repo that
// covers abs (so a daemon query won't answer empty for an untracked path).
func daemonOwnsRepo(abs string) bool {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return false
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Control(daemon.ControlStatus, nil)
	if err != nil || !resp.OK {
		return false
	}
	var st daemon.StatusResponse
	if err := json.Unmarshal(resp.Result, &st); err != nil {
		return false
	}
	for _, repo := range st.TrackedRepos {
		if repo.Path == "" {
			continue
		}
		if abs == repo.Path || strings.HasPrefix(abs, repo.Path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
