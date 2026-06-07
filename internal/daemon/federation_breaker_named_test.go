package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// failingToolRemote serves a healthy /v1/health (indexed, compatible
// schema) but answers every /v1/tools/<name> call with HTTP 500, and
// records how many times its tool endpoint was actually hit. That lets a
// test drive the circuit breaker to the open state and then prove the
// next fan-out makes NO outbound tool request to it.
func failingToolRemote(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var toolCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "indexed": true, "schema_version": localSchemaMajor,
			"api_version": 1, "read_only": true,
		})
	})
	mux.HandleFunc("/v1/tools/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&toolCalls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &toolCalls
}

// breakerFederator builds a Federator with a small, deterministic breaker
// threshold and a long cooldown so the open state does not half-open
// mid-test.
func breakerFederator(threshold int) *Federator {
	return NewFederator(FederationConfig{
		PerRemoteTimeout: 250 * time.Millisecond,
		Budget:           2 * time.Second,
		BreakerThreshold: threshold,
		BreakerCooldown:  30 * time.Second,
		HealthTTL:        time.Millisecond,
	}, func(e ServerEntry) (*ServerClient, error) { return NewServerClient(e) }, nil)
}

// TestFederator_CircuitBreaker asserts that after K consecutive failures
// to a remote, that remote is circuit-broken and SKIPPED on the next
// fan-out with no outbound tool request observed, and that it is bucketed
// in remotes_failed with reason "circuit_open".
func TestFederator_CircuitBreaker(t *testing.T) {
	const threshold = 3
	remote, toolCalls := failingToolRemote(t)
	fed := breakerFederator(threshold)
	local := envelope(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0}`)
	roster := []ServerEntry{{Slug: "flaky", URL: remote.URL}}

	body := []byte(`{}`)

	// Drive exactly K failing fan-outs. Each one issues one tool call that
	// returns 500, recording one breaker failure. After the K-th, the
	// breaker for this remote should be open.
	for i := 0; i < threshold; i++ {
		out := fed.Augment(context.Background(), "find_usages", body, local, roster)
		m := decodeFederated(t, out)
		fed2, _ := m["federation"].(map[string]any)
		if fed2["degraded"] != true {
			t.Fatalf("call %d: a failing remote must mark the response degraded", i+1)
		}
	}

	calledBeforeBreak := atomic.LoadInt32(toolCalls)
	if calledBeforeBreak != threshold {
		t.Fatalf("expected exactly %d outbound tool calls while the breaker was closed, got %d",
			threshold, calledBeforeBreak)
	}

	// Next call: the breaker is open, so fanOut must short-circuit this
	// remote BEFORE any client is built or any HTTP request is made.
	out := fed.Augment(context.Background(), "find_usages", body, local, roster)
	if got := atomic.LoadInt32(toolCalls); got != calledBeforeBreak {
		t.Fatalf("an open circuit must make NO further outbound tool request: tool hit %d times, want it to stay at %d",
			got, calledBeforeBreak)
	}

	m := decodeFederated(t, out)
	fed3, _ := m["federation"].(map[string]any)
	if fed3["degraded"] != true {
		t.Error("a circuit-broken remote must keep the response degraded")
	}
	failed, _ := fed3["remotes_failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("the broken remote must be bucketed in remotes_failed, got %v", fed3["remotes_failed"])
	}
	first, _ := failed[0].(map[string]any)
	if first["reason"] != "circuit_open" {
		t.Errorf("a circuit-broken remote must be reported with reason circuit_open, got %v", failed[0])
	}
	if first["slug"] != "flaky" {
		t.Errorf("the broken remote slug must be reported, got %v", first["slug"])
	}
}
