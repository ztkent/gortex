package mcp

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// Structured MCP error responses.
//
// MCP tool handlers historically returned errors as text content with
// no machine-readable code, leaving agents to regex-parse English
// strings to decide whether to retry, reauth, or surface to the user.
// Iteration 1 introduces a small set of stable error codes for the
// failure modes the workspace boundary checks introduce, plus the
// multi-server routing that can fail in distinct ways the agent
// should react to differently.
//
// The codes live here (one place to grep) so cross-package code that
// surfaces MCP errors can refer to a single canonical list. The
// payload is JSON-encoded into a TextContent block so existing
// transports keep working — the `is_error` flag on CallToolResult
// signals the structured shape, and a leading `{"error_code": ...}`
// is what a smart client decodes.

// ErrorCode is the stable identifier surfaced under
// `error_code` in tool error payloads. Adding new codes is OK; renaming
// or removing an existing one is a wire-contract break.
type ErrorCode string

const (
	// ErrCodeWorkspaceUnknown — the requested workspace doesn't
	// exist on this server. Routing guidance: the daemon should fall
	// back to the "default" server, surface a hint to the user, or
	// fail.
	ErrCodeWorkspaceUnknown ErrorCode = "workspace_unknown"

	// ErrCodeProjectUnknown — workspace exists but the requested
	// project slug doesn't (e.g. monorepo missing the named project).
	ErrCodeProjectUnknown ErrorCode = "project_unknown"

	// ErrCodeCrossWorkspaceDenied — the source workspace's
	// `cross_workspace_deps` doesn't declare the target workspace
	// (or the import path doesn't match a declared module). The
	// query is refused at the matcher / resolver boundary.
	ErrCodeCrossWorkspaceDenied ErrorCode = "cross_workspace_denied"

	// ErrCodeRepoNotTracked — the cwd wasn't found in any tracked
	// repo's root tree. Used by the daemon's MCP front-door.
	ErrCodeRepoNotTracked ErrorCode = "repo_not_tracked"

	// ErrCodeRouteUnresolved — the multi-server router couldn't
	// pick a server for the request (no servers.toml entry covers
	// the workspace, no roster claims it, no default). Retriable
	// after the user adjusts servers.toml.
	ErrCodeRouteUnresolved ErrorCode = "route_unresolved"

	// ErrCodeProxyUpstream — the router proxied to a remote server
	// and the upstream returned a non-2xx. error.data carries
	// `upstream_status` and `upstream_body` for debugging.
	ErrCodeProxyUpstream ErrorCode = "proxy_upstream"

	// ErrCodeOverlayDrift — an overlay push carried a BaseSHA that
	// disagrees with the file's current on-disk SHA. The client
	// should re-read the file and resubmit a fresh overlay.
	ErrCodeOverlayDrift ErrorCode = "overlay_drift"

	// ErrCodeOverlaySessionUnknown — the overlay session ID isn't
	// registered (expired, dropped, or never existed).
	ErrCodeOverlaySessionUnknown ErrorCode = "overlay_session_unknown"

	// ErrCodeInvalidArgument — the tool args failed validation.
	// Generic catch-all so per-tool handlers don't have to invent
	// their own code for "you passed nonsense".
	ErrCodeInvalidArgument ErrorCode = "invalid_argument"
)

// StructuredError is the JSON shape encoded into the TextContent
// block of an MCP error. Smart clients parse the leading `{` to read
// it; older clients see the human-readable Message field as a
// fallback.
type StructuredError struct {
	ErrorCode ErrorCode      `json:"error_code"`
	Message   string         `json:"message"`
	Retriable bool           `json:"retriable,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// NewStructuredErrorResult returns a CallToolResult whose IsError is
// true and whose single TextContent block is the JSON form of err.
// Use this from any MCP handler that wants to surface a typed
// failure instead of a free-form string.
func NewStructuredErrorResult(err StructuredError) *mcp.CallToolResult {
	if err.ErrorCode == "" {
		err.ErrorCode = ErrCodeInvalidArgument
	}
	if err.Message == "" {
		err.Message = string(err.ErrorCode)
	}
	body, mErr := json.Marshal(err)
	if mErr != nil {
		// Fallback to plain text — never let a marshal failure mask
		// the actual error.
		return mcp.NewToolResultError(fmt.Sprintf("%s: %s", err.ErrorCode, err.Message))
	}
	res := mcp.NewToolResultError(string(body))
	res.IsError = true
	return res
}

// Common constructors for the codes above so handlers can call
// `mcp.WorkspaceUnknownError(slug)` instead of building structs by
// hand.

func WorkspaceUnknownError(workspace string) *mcp.CallToolResult {
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeWorkspaceUnknown,
		Message:   fmt.Sprintf("workspace %q is not known to this server", workspace),
		Retriable: false,
		Data:      map[string]any{"workspace": workspace},
	})
}

func ProjectUnknownError(workspace, project string) *mcp.CallToolResult {
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeProjectUnknown,
		Message:   fmt.Sprintf("project %q does not exist in workspace %q", project, workspace),
		Retriable: false,
		Data:      map[string]any{"workspace": workspace, "project": project},
	})
}

func CrossWorkspaceDeniedError(source, target, importPath string) *mcp.CallToolResult {
	msg := fmt.Sprintf("cross-workspace access from %q to %q is not declared in cross_workspace_deps", source, target)
	if importPath != "" {
		msg = fmt.Sprintf("%s (import path %q)", msg, importPath)
	}
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeCrossWorkspaceDenied,
		Message:   msg,
		Retriable: false,
		Data: map[string]any{
			"source_workspace": source,
			"target_workspace": target,
			"import_path":      importPath,
		},
	})
}

// AsStructuredError unwraps a typed error from the package's known
// sentinel set, returning a CallToolResult on hit. Returns (nil,
// false) when err isn't one of the recognised sentinels — caller
// falls back to its own error handling.
func AsStructuredError(err error) (*mcp.CallToolResult, bool) {
	if err == nil {
		return nil, false
	}
	// Future: extend with errors.Is checks for daemon.Err* sentinels
	// once the daemon's errors are imported here. For now we only
	// match generic shapes used by handlers.
	switch {
	case errors.Is(err, errInvalidArgument):
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   err.Error(),
		}), true
	}
	return nil, false
}

// errInvalidArgument is the canonical sentinel a tool can return when
// its args fail validation; AsStructuredError converts it to the
// structured form. Wrapping (`fmt.Errorf("%w: ...", errInvalidArgument)`)
// is supported via errors.Is.
var errInvalidArgument = errors.New("invalid argument")
