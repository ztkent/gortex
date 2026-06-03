package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// realtimeChannels returns transport→[]topic for every pub/sub event node
// reachable via a listen/emit edge (the websocket/sse channels live
// alongside broker topics under the same KindEvent model).
func realtimeChannels(nodes []*graph.Node) map[string][]string {
	out := map[string][]string{}
	for _, n := range nodes {
		if n.Kind != graph.KindEvent || n.Meta == nil {
			continue
		}
		t, _ := n.Meta["transport"].(string)
		out[t] = append(out[t], n.Name)
	}
	return out
}

func TestJSExtract_WebSocketAndSSE(t *testing.T) {
	src := `function connect() {
  const ws = new WebSocket('wss://api.example.com/ws');
  const es = new EventSource('/events/stream');
  ws.send('hi');
}`
	r, err := NewJavaScriptExtractor().Extract("rt.js", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	ch := realtimeChannels(r.Nodes)
	if got := ch[transportWebSocket]; len(got) != 1 || got[0] != "wss://api.example.com/ws" {
		t.Errorf("websocket channels = %v, want [wss://api.example.com/ws]", got)
	}
	if got := ch[transportSSE]; len(got) != 1 || got[0] != "/events/stream" {
		t.Errorf("sse channels = %v, want [/events/stream]", got)
	}
	// Each constructor produced a listen edge from connect().
	listens := 0
	for _, e := range r.Edges {
		if e.Kind == graph.EdgeListensOn && e.From == "rt.js::connect" {
			listens++
		}
	}
	if listens < 2 {
		t.Errorf("want >=2 listen edges from connect(), got %d", listens)
	}
}

func TestTSExtract_EventSource(t *testing.T) {
	src := `export function sub() {
  const src = new EventSource('/sse');
  src.onmessage = (e) => console.log(e);
}`
	r, err := NewTypeScriptExtractor().Extract("sub.ts", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got := realtimeChannels(r.Nodes)[transportSSE]; len(got) != 1 || got[0] != "/sse" {
		t.Errorf("sse channels = %v, want [/sse]", got)
	}
}

func TestGoExtract_WebSocketUpgrade(t *testing.T) {
	src := `package server

import "github.com/gorilla/websocket"

var upgrader = websocket.Upgrader{}

func handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	_ = conn
}`
	res, err := NewGoExtractor().Extract("ws.go", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	found := false
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeListensOn && e.From == "ws.go::handleWS" && e.Meta != nil {
			if e.Meta["transport"] == transportWebSocket {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("handleWS must produce a websocket listen edge")
	}
}

func TestGoExtract_NonWebSocketUpgradeIgnored(t *testing.T) {
	// A .Upgrade( call in a file with no websocket import must not be
	// treated as a WebSocket endpoint.
	src := `package db
func migrate() { schema.Upgrade(ctx) }`
	res, err := NewGoExtractor().Extract("db.go", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindEvent {
			t.Errorf("unexpected websocket event node %q in non-websocket file", n.ID)
		}
	}
}
