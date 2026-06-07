package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestFanOut_AuditsAndLogsFailures asserts the read-only fan-out emits a
// structured audit line per remote-routed call AND names a failing remote
// in the logs (not only in the JSON federation block).
func TestFanOut_AuditsAndLogsFailures(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)

	good := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{"nodes":[],"edges":[]}`})

	// A remote that negotiates fine but 500s on the tool call -> degraded.
	badMux := http.NewServeMux()
	badMux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "indexed": true,
			"schema_version": localSchemaMajor, "api_version": 1,
		})
	})
	badMux.HandleFunc("/v1/tools/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	bad := httptest.NewServer(badMux)
	t.Cleanup(bad.Close)

	fed := NewFederator(FederationConfig{
		PerRemoteTimeout: 250 * time.Millisecond,
		Budget:           2 * time.Second,
		HealthTTL:        time.Millisecond,
	}, func(e ServerEntry) (*ServerClient, error) { return NewServerClient(e) }, zap.New(core))

	local := envelope(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0}`)
	ctx := withAuditInfo(context.Background(), "/repo", "sess-9")
	fed.Augment(ctx, "find_usages", []byte(`{}`), local,
		[]ServerEntry{{Slug: "good", URL: good.URL}, {Slug: "bad", URL: bad.URL}})

	// Audit: one remote-routed call line per remote carrying the full
	// {session_id, cwd, tool, target_slug} tuple, tagged via=fan-out.
	audit := logs.FilterMessage("federation: remote-routed call").All()
	if len(audit) != 2 {
		t.Fatalf("want 2 fan-out audit lines, got %d", len(audit))
	}
	seen := map[string]bool{}
	for _, e := range audit {
		f := e.ContextMap()
		if f["via"] != "fan-out" || f["tool"] != "find_usages" {
			t.Errorf("audit line missing via/tool: %v", f)
		}
		if f["cwd"] != "/repo" || f["session_id"] != "sess-9" {
			t.Errorf("audit line missing cwd/session_id tuple: %v", f)
		}
		seen[f["target_slug"].(string)] = true
	}
	if !seen["good"] || !seen["bad"] {
		t.Errorf("both remotes must be audited; got %v", seen)
	}

	// Degraded: the failing remote is named in the logs with a reason.
	degraded := logs.FilterMessage("federation: remote degraded").All()
	if len(degraded) == 0 {
		t.Fatal("a degraded fan-out must emit a warning naming the remote")
	}
	found := false
	for _, e := range degraded {
		f := e.ContextMap()
		if f["target_slug"] == "bad" && f["reason"] != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("the failing remote 'bad' must be named with a reason; got %d degraded lines", len(degraded))
	}
}
