package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// goWSUpgradeRe matches the server-side WebSocket upgrade calls of the
// common Go libraries: gorilla/websocket `upgrader.Upgrade(...)`,
// gobwas/ws `ws.UpgradeHTTP(...)`, and coder/nhooyr `websocket.Accept(...)`.
var goWSUpgradeRe = regexp.MustCompile(`\.Upgrade\s*\(|\.UpgradeHTTP\s*\(|websocket\.Accept\s*\(`)

// emitGoWebSocketEdges marks each function that performs a WebSocket
// upgrade as a real-time endpoint: it emits an EdgeListensOn from the
// handler to a per-handler websocket KindEvent node, so `analyze
// kind=pubsub` / architecture queries surface the WebSocket surface that
// the call graph cannot see. Gated on the file importing a websocket
// package to avoid matching unrelated `.Upgrade(` methods.
func emitGoWebSocketEdges(src []byte, filePath string, result *parser.ExtractionResult) {
	s := string(src)
	if !strings.Contains(s, "websocket") && !strings.Contains(s, "gobwas/ws") {
		return
	}
	locs := goWSUpgradeRe.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return
	}
	funcRanges := buildFuncRanges(result)
	seenNode := map[string]bool{}
	for _, m := range locs {
		line := lineAt(src, m[0])
		callerID := findEnclosingFunc(funcRanges, line)
		if callerID == "" {
			continue
		}
		topic := callerID
		if i := strings.LastIndexAny(topic, ":./"); i >= 0 {
			topic = topic[i+1:]
		}
		nodeID := "event::pubsub::" + transportWebSocket + "::" + topic
		if !seenNode[nodeID] {
			seenNode[nodeID] = true
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: nodeID, Kind: graph.KindEvent, Name: topic,
				FilePath: filePath, Language: "go",
				Meta: map[string]any{"event_kind": "pubsub", "transport": transportWebSocket, "name": topic},
			})
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: nodeID, Kind: graph.EdgeListensOn,
			FilePath: filePath, Line: line, Origin: graph.OriginASTInferred,
			Meta: map[string]any{"method": "upgrade", "transport": transportWebSocket},
		})
	}
}
