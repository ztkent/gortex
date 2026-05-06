package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestGoStrings_StatsdMetricsExtracted(t *testing.T) {
	src := `package foo

type StatsdClient struct{}

func (c *StatsdClient) Increment(name string, tags []string, rate float64) error { return nil }
func (c *StatsdClient) Gauge(name string, value float64, tags []string, rate float64) error { return nil }

func Run(c *StatsdClient) {
	c.Increment("orders.checkout.success", nil, 1)
	c.Gauge("server.memory.bytes", 12345, nil, 1)
}
`
	fix := runGoExtract(t, src)

	strs := fix.nodesByKind[graph.KindString]
	if len(strs) != 2 {
		t.Fatalf("expected 2 KindString, got %d: %+v", len(strs), strs)
	}
	gotByValue := map[string]*graph.Node{}
	for _, n := range strs {
		gotByValue[n.Name] = n
		if ctx, _ := n.Meta["context"].(string); ctx != "metric" {
			t.Errorf("node %q: context = %q, want metric", n.Name, ctx)
		}
		want := "string::metric::" + n.Name
		if n.ID != want {
			t.Errorf("id = %q, want %q", n.ID, want)
		}
	}
	if gotByValue["orders.checkout.success"] == nil || gotByValue["server.memory.bytes"] == nil {
		t.Errorf("missing expected metric names: %v", gotByValue)
	}

	emits := fix.edgesByKind[graph.EdgeEmits]
	if len(emits) != 2 {
		t.Errorf("expected 2 EdgeEmits, got %d", len(emits))
	}
	for _, e := range emits {
		if e.From != "pkg/foo.go::Run" {
			t.Errorf("emit from = %q, want pkg/foo.go::Run", e.From)
		}
		if ctx, _ := e.Meta["context"].(string); ctx != "metric" {
			t.Errorf("emit context = %q", ctx)
		}
	}
}

func TestGoStrings_GenericMethodNamesIgnored(t *testing.T) {
	// Set / Inc / Add are too generic — would create false positives
	// against map and counter types — and are deliberately excluded.
	src := `package foo

type M struct{}
func (m M) Set(k, v string)  {}
func (m M) Inc(k string)     {}
func (m M) Add(k string, n int) {}

func Run(m M) {
	m.Set("magic", "value")
	m.Inc("counter")
	m.Add("tally", 1)
}
`
	fix := runGoExtract(t, src)
	if got := fix.nodesByKind[graph.KindString]; len(got) != 0 {
		t.Errorf("expected no metric strings, got %d: %+v", len(got), got)
	}
}

func TestGoStrings_ErrorsNewAndFmtErrorf(t *testing.T) {
	src := `package foo

import (
	"errors"
	"fmt"
)

func A() error { return errors.New("user not found") }
func B(id int) error { return fmt.Errorf("invalid id %d", id) }
`
	fix := runGoExtract(t, src)

	strs := fix.nodesByKind[graph.KindString]
	if len(strs) != 2 {
		t.Fatalf("expected 2 KindString, got %d: %+v", len(strs), strs)
	}
	gotByValue := map[string]*graph.Node{}
	for _, n := range strs {
		gotByValue[n.Name] = n
		if ctx, _ := n.Meta["context"].(string); ctx != "error_msg" {
			t.Errorf("node %q: context = %q, want error_msg", n.Name, ctx)
		}
	}
	if gotByValue["user not found"] == nil {
		t.Errorf("missing 'user not found': %v", gotByValue)
	}
	if gotByValue["invalid id %d"] == nil {
		t.Errorf("missing 'invalid id %%d': %v", gotByValue)
	}
}

func TestGoStrings_HTTPRoutesExtracted(t *testing.T) {
	src := `package foo

type Mux struct{}
func (m *Mux) HandleFunc(pattern string, h func()) {}
func (m *Mux) Get(pattern string, h func())        {}

func Wire(m *Mux) {
	m.HandleFunc("/api/v1/users", nil)
	m.Get("/health", nil)
	m.HandleFunc("GET /api/v1/orders", nil)
}
`
	fix := runGoExtract(t, src)

	strs := fix.nodesByKind[graph.KindString]
	if len(strs) != 3 {
		t.Fatalf("expected 3 KindString routes, got %d: %+v", len(strs), strs)
	}
	got := map[string]bool{}
	for _, n := range strs {
		got[n.Name] = true
		if ctx, _ := n.Meta["context"].(string); ctx != "route" {
			t.Errorf("node %q: context = %q, want route", n.Name, ctx)
		}
	}
	for _, want := range []string{"/api/v1/users", "/health", "GET /api/v1/orders"} {
		if !got[want] {
			t.Errorf("missing route %q", want)
		}
	}
}

func TestGoStrings_NonRouteStringSkipped(t *testing.T) {
	// Get/Post on map-like types — the looksLikeRoute filter should
	// keep these out of the route bucket.
	src := `package foo

type Cache struct{}
func (c *Cache) Get(key string) string { return "" }

func Run(c *Cache) {
	_ = c.Get("user:42:profile")
}
`
	fix := runGoExtract(t, src)
	if got := fix.nodesByKind[graph.KindString]; len(got) != 0 {
		t.Errorf("expected no routes for cache.Get, got: %+v", got)
	}
}

func TestGoStrings_SameMetricAcrossCallsDeduplicates(t *testing.T) {
	src := `package foo

type Client struct{}
func (c *Client) Increment(name string) {}

func A(c *Client) { c.Increment("orders.success") }
func B(c *Client) { c.Increment("orders.success") }
`
	fix := runGoExtract(t, src)

	strs := fix.nodesByKind[graph.KindString]
	if len(strs) != 1 {
		t.Fatalf("expected 1 KindString (deduplicated), got %d", len(strs))
	}
	emits := fix.edgesByKind[graph.EdgeEmits]
	if len(emits) != 2 {
		t.Errorf("expected 2 EdgeEmits (one per caller), got %d", len(emits))
	}
}

func TestGoStrings_NodeIDForLongValueIsHashed(t *testing.T) {
	// 250 chars > 200-char threshold — should hash to keep IDs sane.
	long := strings.Repeat("a", 250)
	got := goStringNodeID(stringCtxErrorMsg, long)
	want := "string::error_msg::sha1:"
	if got[:len(want)] != want {
		t.Errorf("long-value id = %q, want prefix %q", got, want)
	}
	short := goStringNodeID(stringCtxMetric, "ok.short")
	if short != "string::metric::ok.short" {
		t.Errorf("short-value id = %q", short)
	}
}

func TestLooksLikeRoute(t *testing.T) {
	cases := map[string]bool{
		"/users":                true,
		"/api/v1/orders":        true,
		"GET /foo":              true,
		"POST /bar":             true,
		"":                      false,
		"plain":                 false,
		"user:42:profile":       false,
		"orders.checkout.event": false,
	}
	for in, want := range cases {
		if got := looksLikeRoute(in); got != want {
			t.Errorf("looksLikeRoute(%q) = %v, want %v", in, got, want)
		}
	}
}
