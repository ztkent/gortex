package mcp

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/semantic/lsp"
)

// fakeNotificationBroadcaster captures every SendNotificationToAllClients
// call so the test can assert what got broadcast.
type fakeNotificationBroadcaster struct {
	mu    sync.Mutex
	calls []fakeNotificationCall
}

type fakeNotificationCall struct {
	method string
	params map[string]any
}

func (f *fakeNotificationBroadcaster) SendNotificationToAllClients(method string, params map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeNotificationCall{method: method, params: params})
}

func (f *fakeNotificationBroadcaster) snapshot() []fakeNotificationCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeNotificationCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestDiagnosticsBroadcaster_NoSubscribers — without a subscribed
// session the broadcaster must not call SendNotificationToAllClients,
// but it MUST still update the lastHash so a late-arriving subscriber
// doesn't get an immediate replay of the previous burst.
func TestDiagnosticsBroadcaster_NoSubscribers(t *testing.T) {
	fake := &fakeNotificationBroadcaster{}
	b := newDiagnosticsBroadcaster(fake, zap.NewNop())

	diags := []lsp.Diagnostic{{Message: "missing semicolon"}}
	b.publish("gopls", "/abs/path/main.go", diags)

	assert.Empty(t, fake.snapshot(), "no subscribers — no broadcast")
	// Subscribe AFTER the silent publish — same payload should still be
	// suppressed (no replay), proving the hash was updated.
	b.subscribe("session-A")
	b.publish("gopls", "/abs/path/main.go", diags)
	assert.Empty(t, fake.snapshot(), "subscriber added after — duplicate payload still suppressed")
}

// TestDiagnosticsBroadcaster_DeltaFilter — identical re-publishes are
// suppressed; a payload change produces a new broadcast.
func TestDiagnosticsBroadcaster_DeltaFilter(t *testing.T) {
	fake := &fakeNotificationBroadcaster{}
	b := newDiagnosticsBroadcaster(fake, zap.NewNop())
	b.subscribe("session-A")

	first := []lsp.Diagnostic{{Message: "one"}}
	second := []lsp.Diagnostic{{Message: "two"}}

	b.publish("gopls", "/abs/path/main.go", first)
	b.publish("gopls", "/abs/path/main.go", first) // duplicate — suppressed
	b.publish("gopls", "/abs/path/main.go", second)
	b.publish("gopls", "/abs/path/main.go", second) // duplicate — suppressed

	require.Len(t, fake.snapshot(), 2, "exactly two broadcasts after delta filter")
}

// TestDiagnosticsBroadcaster_PayloadShape — the broadcast carries the
// expected MCP shape: method = notifications/diagnostics, params =
// {uri, path, server, diagnostics}.
func TestDiagnosticsBroadcaster_PayloadShape(t *testing.T) {
	fake := &fakeNotificationBroadcaster{}
	b := newDiagnosticsBroadcaster(fake, zap.NewNop())
	b.subscribe("session-A")

	diags := []lsp.Diagnostic{{Message: "boom", Severity: 1}}
	b.publish("gopls", "/work/main.go", diags)

	calls := fake.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "notifications/diagnostics", calls[0].method)
	assert.Equal(t, "file:///work/main.go", calls[0].params["uri"])
	assert.Equal(t, "/work/main.go", calls[0].params["path"])
	assert.Equal(t, "gopls", calls[0].params["server"])
	wire, ok := calls[0].params["diagnostics"].([]map[string]any)
	require.True(t, ok, "diagnostics should be the wire-form slice")
	assert.Len(t, wire, 1)
}

// TestDiagnosticsBroadcaster_Unsubscribe — after unsubscribe the
// session stops receiving broadcasts. With zero subscribers the
// broadcaster goes silent again.
func TestDiagnosticsBroadcaster_Unsubscribe(t *testing.T) {
	fake := &fakeNotificationBroadcaster{}
	b := newDiagnosticsBroadcaster(fake, zap.NewNop())

	b.subscribe("session-A")
	b.publish("gopls", "/work/a.go", []lsp.Diagnostic{{Message: "a"}})
	require.Len(t, fake.snapshot(), 1)

	b.unsubscribe("session-A")
	b.publish("gopls", "/work/b.go", []lsp.Diagnostic{{Message: "b"}})
	require.Len(t, fake.snapshot(), 1, "no subscribers — no broadcast")

	assert.Equal(t, 0, b.subscriberCount())
}

// TestDiagnosticsBroadcaster_SubscribeIdempotent — subscribing the
// same session twice doesn't create duplicate entries.
func TestDiagnosticsBroadcaster_SubscribeIdempotent(t *testing.T) {
	fake := &fakeNotificationBroadcaster{}
	b := newDiagnosticsBroadcaster(fake, zap.NewNop())
	b.subscribe("session-A")
	b.subscribe("session-A")
	assert.Equal(t, 1, b.subscriberCount())
}

// TestDiagnosticsBroadcaster_NilBroadcaster — publish on a broadcaster
// with no underlying server is a safe no-op (defensive — boot order
// can produce this transiently).
func TestDiagnosticsBroadcaster_NilBroadcaster(t *testing.T) {
	b := newDiagnosticsBroadcaster(nil, zap.NewNop())
	b.subscribe("session-A")
	// Should not panic.
	b.publish("gopls", "/abs/path/main.go", []lsp.Diagnostic{{Message: "x"}})
}

// TestPathToFileURI_Absolute — POSIX path round-trips into the
// expected file:// URI.
func TestPathToFileURI_Absolute(t *testing.T) {
	assert.Equal(t, "file:///work/main.go", pathToFileURI("/work/main.go"))
	assert.Equal(t, "", pathToFileURI(""))
}

// TestHashDiagnostics_Empty / Stable — empty list maps to a fixed
// sentinel; identical content hashes match.
func TestHashDiagnostics_Stable(t *testing.T) {
	a := []lsp.Diagnostic{{Message: "x", Severity: 2}}
	b := []lsp.Diagnostic{{Message: "x", Severity: 2}}
	assert.Equal(t, hashDiagnostics(a), hashDiagnostics(b))

	c := []lsp.Diagnostic{{Message: "y", Severity: 2}}
	assert.NotEqual(t, hashDiagnostics(a), hashDiagnostics(c))

	assert.Equal(t, "empty", hashDiagnostics(nil))
}
