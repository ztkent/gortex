package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestInitialize_ReturnsInstructions drives a real `initialize`
// JSON-RPC frame through HandleMessage and asserts the server-level
// `instructions` field is populated — the field MCP clients surface
// to the agent as "how to drive this server".
func TestInitialize_ReturnsInstructions(t *testing.T) {
	srv := newFullTestServer(t)
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)

	reply := srv.MCPServer().HandleMessage(context.Background(), frame)
	if reply == nil {
		t.Fatal("HandleMessage returned nil for initialize")
	}
	out, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	var parsed struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if strings.TrimSpace(parsed.Result.Instructions) == "" {
		t.Fatalf("initialize response carried no instructions field; got: %s", out)
	}
	for _, want := range []string{"smart_context", "search_symbols", "tools_search"} {
		if !strings.Contains(parsed.Result.Instructions, want) {
			t.Errorf("instructions should mention %q; got: %q", want, parsed.Result.Instructions)
		}
	}
}

func TestServerInstructions_NonEmpty(t *testing.T) {
	if strings.TrimSpace(serverInstructions) == "" {
		t.Fatal("serverInstructions constant is empty")
	}
	if scrubControlChars(serverInstructions) != serverInstructions {
		t.Error("serverInstructions carries control characters")
	}
}
