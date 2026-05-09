package mcp

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
)

// TestDefaultFormatForClient verifies the GCX-capable client allowlist.
// The list is the contract: clients in it get gcx by default; everything
// else falls back to JSON. Drift in this list silently changes the wire
// format every shipping client receives, so the test pins it explicitly.
func TestDefaultFormatForClient(t *testing.T) {
	cases := []struct {
		client string
		want   string
	}{
		// GCX-capable: every client whose plugin/CLI ships a GCX1 decoder.
		{"claude-code", "gcx"},
		{"Claude-Code", "gcx"}, // case-insensitive
		{"  claude-code  ", "gcx"}, // trimmed
		{"cursor", "gcx"},
		{"vscode", "gcx"},
		{"zed", "gcx"},
		{"aider", "gcx"},
		{"kilocode", "gcx"},
		{"opencode", "gcx"},
		{"openclaw", "gcx"},
		{"codex", "gcx"},

		// Unknown / unset → JSON fallback.
		{"", ""},
		{"some-other-client", ""},
		{"unknown", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, defaultFormatForClient(tc.client),
			"defaultFormatForClient(%q)", tc.client)
	}
}

// TestResolveSessionFormat_NoSession returns "" for a bare context — no
// session means no client identity, which means no default format.
func TestResolveSessionFormat_NoSession(t *testing.T) {
	srv, _ := setupTestServer(t)
	assert.Equal(t, "", srv.resolveSessionFormat(context.Background()))
}

// TestResolveSessionFormat_KnownClient verifies the full per-session
// path: NoteSessionClient stores the client name, and resolveSessionFormat
// reads it back through sessionFor + defaultFormatForClient.
func TestResolveSessionFormat_KnownClient(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.NoteSessionClient("session_X", "claude-code", "1.0.42")
	ctx := WithSessionID(context.Background(), "session_X")
	assert.Equal(t, "gcx", srv.resolveSessionFormat(ctx))
}

func TestResolveSessionFormat_UnknownClient(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.NoteSessionClient("session_X", "some-bespoke-client", "0.1")
	ctx := WithSessionID(context.Background(), "session_X")
	assert.Equal(t, "", srv.resolveSessionFormat(ctx),
		"unknown client must fall back to JSON (empty string)")
}

// TestNoteSessionClient_NilSafe ensures NoteSessionClient never panics
// when called on a nil *Server or with empty inputs — both are normal
// during boot races / embedded-mode tests.
func TestNoteSessionClient_NilSafe(t *testing.T) {
	var srv *Server
	srv.NoteSessionClient("sess", "claude-code", "1.0")

	srv2, _ := setupTestServer(t)
	srv2.NoteSessionClient("", "claude-code", "1.0") // empty session id → no-op
	srv2.NoteSessionClient("sess", "", "1.0")        // empty client → no-op
}

// TestNoteSessionClient_IsolatedPerSession verifies two sessions get
// independent client-name state. This is the core invariant that lets
// the daemon serve multiple proxies through one shared *Server.
func TestNoteSessionClient_IsolatedPerSession(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.NoteSessionClient("sess_A", "claude-code", "1.0")
	srv.NoteSessionClient("sess_B", "some-bespoke-client", "0.1")

	ctxA := WithSessionID(context.Background(), "sess_A")
	ctxB := WithSessionID(context.Background(), "sess_B")
	assert.Equal(t, "gcx", srv.resolveSessionFormat(ctxA))
	assert.Equal(t, "", srv.resolveSessionFormat(ctxB))
}

// TestIsGCX_ExplicitFormatWins verifies that an explicit `format` arg
// overrides the per-session default in either direction.
func TestIsGCX_ExplicitFormatWins(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.NoteSessionClient("sess_A", "claude-code", "1.0") // session default = gcx
	ctx := WithSessionID(context.Background(), "sess_A")

	// Explicit "json" must override the session default.
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"format": "json"}
	assert.False(t, srv.isGCX(ctx, req),
		"explicit format=json must defeat session-default gcx")

	// Explicit "gcx" stays gcx.
	req2 := mcp.CallToolRequest{}
	req2.Params.Arguments = map[string]any{"format": "gcx"}
	assert.True(t, srv.isGCX(ctx, req2))
}

// TestIsGCX_SessionDefaultApplies verifies that a session whose client
// is GCX-capable picks gcx when the request omits `format`.
func TestIsGCX_SessionDefaultApplies(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.NoteSessionClient("sess_A", "claude-code", "1.0")
	ctx := WithSessionID(context.Background(), "sess_A")

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{} // no format
	assert.True(t, srv.isGCX(ctx, req),
		"claude-code session with no explicit format must default to gcx")
}

// TestIsGCX_NoSession_NoFormat returns false — the legacy default is
// JSON, and absent both an explicit format and a known client we must
// preserve that.
func TestIsGCX_NoSession_NoFormat(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	assert.False(t, srv.isGCX(context.Background(), req))
}

// TestIsTOON_ExplicitFormatWins verifies that an explicit `format=toon`
// trips isTOON regardless of session default.
func TestIsTOON_ExplicitFormatWins(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.NoteSessionClient("sess_A", "claude-code", "1.0") // gcx by default
	ctx := WithSessionID(context.Background(), "sess_A")

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"format": "toon"}
	assert.True(t, srv.isTOON(ctx, req))
	assert.False(t, srv.isGCX(ctx, req),
		"format=toon must not also trigger gcx")
}

// TestRespondJSONOrTOON_RoutesByFormat pins the helper that 14
// list-shaped tools share. With explicit format=toon the payload comes
// back as TOON-marshalled text; with no format and an unknown client
// it falls back to JSON. This is the single decision point most tools
// route through, so a regression here would silently flip every
// downstream consumer to the wrong format.
func TestRespondJSONOrTOON_RoutesByFormat(t *testing.T) {
	srv, _ := setupTestServer(t)
	payload := map[string]any{"x": 1, "y": "two"}

	// format=toon → TOON text result.
	reqTOON := mcp.CallToolRequest{}
	reqTOON.Params.Arguments = map[string]any{"format": "toon"}
	res, err := srv.respondJSONOrTOON(context.Background(), reqTOON, payload)
	assert.NoError(t, err)
	assert.False(t, res.IsError)
	tc, ok := res.Content[0].(mcp.TextContent)
	assert.True(t, ok, "expected TextContent for TOON result")
	// TOON encodes scalar map values with `key: value` lines; an empty
	// or JSON-shaped payload would not contain that exact prefix.
	assert.Contains(t, tc.Text, "x: 1")
	assert.NotContains(t, tc.Text, "{")

	// no format, unknown client → JSON fallback.
	reqJSON := mcp.CallToolRequest{}
	reqJSON.Params.Arguments = map[string]any{}
	res, err = srv.respondJSONOrTOON(context.Background(), reqJSON, payload)
	assert.NoError(t, err)
	tc, ok = res.Content[0].(mcp.TextContent)
	assert.True(t, ok)
	assert.Contains(t, tc.Text, "{") // JSON object braces

	// format=json overrides session default → JSON.
	srv.NoteSessionClient("sess_T", "claude-code", "1.0") // session default would be gcx, not toon
	ctx := WithSessionID(context.Background(), "sess_T")
	reqExplicitJSON := mcp.CallToolRequest{}
	reqExplicitJSON.Params.Arguments = map[string]any{"format": "json"}
	res, err = srv.respondJSONOrTOON(ctx, reqExplicitJSON, payload)
	assert.NoError(t, err)
	tc, ok = res.Content[0].(mcp.TextContent)
	assert.True(t, ok)
	assert.Contains(t, tc.Text, "{")
}
