package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/web/hub"
)

func setupTestServer(t *testing.T) (*Server, *hub.Hub) {
	t.Helper()
	g := graph.New()

	// Add some test nodes and edges
	g.AddNode(&graph.Node{ID: "main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go", StartLine: 5})
	g.AddNode(&graph.Node{ID: "main.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "main.go", Language: "go", StartLine: 15})
	g.AddEdge(&graph.Edge{From: "main.go", To: "main.go::main", Kind: graph.EdgeDefines, FilePath: "main.go", Line: 5})
	g.AddEdge(&graph.Edge{From: "main.go::main", To: "main.go::Helper", Kind: graph.EdgeCalls, FilePath: "main.go", Line: 8})

	eng := query.NewEngine(g)
	h := hub.New()
	logger, _ := zap.NewDevelopment()
	srv := NewServer(g, eng, h, logger)

	return srv, h
}

func TestHandleGetGraph(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/graph", nil)
	w := httptest.NewRecorder()
	srv.handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")

	var resp graphResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, 3, len(resp.Nodes))
	assert.Equal(t, 2, len(resp.Edges))
	assert.Equal(t, 3, resp.Stats.TotalNodes)

	// Verify brief mode: no meta or qual_name
	for _, n := range resp.Nodes {
		assert.Empty(t, n.QualName)
		assert.Nil(t, n.Meta)
	}
}

func TestHandleGetStats(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/graph/stats", nil)
	w := httptest.NewRecorder()
	srv.handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var stats graph.GraphStats
	err := json.Unmarshal(w.Body.Bytes(), &stats)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.TotalNodes)
	assert.Equal(t, 2, stats.TotalEdges)
}

func TestHandleGetFileGraph(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/graph/file?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var sub query.SubGraph
	err := json.Unmarshal(w.Body.Bytes(), &sub)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(sub.Nodes), 2) // at least the functions
}

func TestHandleGetFileGraph_MissingParam(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/graph/file", nil)
	w := httptest.NewRecorder()
	srv.handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleSearch(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/graph/search?q=main", nil)
	w := httptest.NewRecorder()
	srv.handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var nodes []*graph.Node
	err := json.Unmarshal(w.Body.Bytes(), &nodes)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(nodes), 1)
}

func TestHandleSSE(t *testing.T) {
	srv, h := setupTestServer(t)

	events := make(chan indexer.GraphChangeEvent, 8)
	go h.Run(events)
	defer h.Stop()

	// Use a real test server to avoid data races with httptest.ResponseRecorder
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/events")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	// Send an event
	events <- indexer.GraphChangeEvent{
		FilePath:   "test.go",
		Kind:       indexer.ChangeModified,
		NodesAdded: 1,
		Timestamp:  time.Now(),
	}

	// Read from the SSE stream
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	require.NoError(t, err)
	body := string(buf[:n])
	assert.Contains(t, body, "graph_change")
	assert.Contains(t, body, "test.go")
}

func TestHandleIndex(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), "Gortex")
}

func TestHandleSSE_NilHub(t *testing.T) {
	g := graph.New()
	eng := query.NewEngine(g)
	logger, _ := zap.NewDevelopment()
	srv := NewServer(g, eng, nil, logger)

	req := httptest.NewRequest("GET", "/api/events", nil)
	w := httptest.NewRecorder()
	srv.handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/event-stream")
	assert.Contains(t, w.Body.String(), "watch mode not active")
}
