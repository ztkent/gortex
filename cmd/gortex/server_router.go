package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

// newLocalToolExecutor builds the daemon.LocalExecutor closure used by
// the multi-server router. It wraps the in-process MCP server's tool
// dispatch with the same body-shape the HTTP handler
// accepts (`{"arguments": {...}}` or flat-args), so a router-routed
// local call returns bytes shaped exactly like a remote-proxied
// response — the caller can't tell the difference and the response
// content type stays JSON.
//
// We re-use the local mcp.Server's MCPServer() rather than calling
// back into the HTTP handler so this path doesn't recursively flow
// through the router again. The router's localExec is the single
// canonical "run this tool here" entrypoint; everything else goes
// through it.
func newLocalToolExecutor(srv *gortexmcp.Server, logger *zap.Logger) daemon.LocalExecutor {
	if srv == nil || srv.MCPServer() == nil {
		return func(_ context.Context, _ string, _ []byte) ([]byte, int, error) {
			return nil, 500, fmt.Errorf("local executor: no MCP server attached")
		}
	}
	return func(ctx context.Context, toolName string, body []byte) ([]byte, int, error) {
		tool := srv.MCPServer().GetTool(toolName)
		if tool == nil {
			payload := map[string]any{
				"error":   "tool_not_found",
				"message": fmt.Sprintf("tool '%s' not found", toolName),
			}
			out, _ := json.Marshal(payload)
			return out, 404, nil
		}

		var args map[string]any
		if len(body) > 0 {
			var nested struct {
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(body, &nested); err == nil && nested.Arguments != nil {
				args = nested.Arguments
			} else {
				_ = json.Unmarshal(body, &args)
			}
		}

		mcpReq := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      toolName,
				Arguments: args,
			},
		}
		result, err := tool.Handler(ctx, mcpReq)
		if err != nil {
			logger.Warn("local executor: tool call error",
				zap.String("tool", toolName), zap.Error(err))
			payload := map[string]any{
				"error":   "tool_error",
				"message": err.Error(),
			}
			out, _ := json.Marshal(payload)
			return out, 500, nil
		}

		// Mirror the same response shape the HTTP handler emits so
		// the proxy and local paths are indistinguishable downstream.
		resp := struct {
			IsError bool             `json:"is_error,omitempty"`
			Content []map[string]any `json:"content,omitempty"`
		}{IsError: result.IsError}
		for _, c := range result.Content {
			if tc, ok := c.(mcp.TextContent); ok {
				resp.Content = append(resp.Content, map[string]any{
					"type": "text",
					"text": tc.Text,
				})
			}
		}
		out, err := json.Marshal(resp)
		if err != nil {
			return nil, 500, err
		}
		return out, 200, nil
	}
}
