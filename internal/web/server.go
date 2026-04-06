package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/web/hub"
	"github.com/zzet/gortex/internal/web/static"
)

// Server serves the web visualization UI and REST API.
type Server struct {
	graph      *graph.Graph
	engine     *query.Engine
	hub        *hub.Hub // nil when watch mode is off
	logger     *zap.Logger
	httpServer *http.Server
}

// NewServer creates a web visualization server.
func NewServer(g *graph.Graph, eng *query.Engine, h *hub.Hub, logger *zap.Logger) *Server {
	return &Server{
		graph:  g,
		engine: eng,
		hub:    h,
		logger: logger,
	}
}

// Start begins serving HTTP on the given address (e.g. ":8765").
func (s *Server) Start(addr string) error {
	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	// Embedded static files
	staticFS, _ := fs.Sub(static.Assets, ".")
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/static/", http.StripPrefix("/static/", fileServer))

	// Index page
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := static.Assets.ReadFile("index.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	// API endpoints
	mux.HandleFunc("/api/graph", s.handleGetGraph)
	mux.HandleFunc("/api/graph/stats", s.handleGetStats)
	mux.HandleFunc("/api/graph/file", s.handleGetFileGraph)
	mux.HandleFunc("/api/graph/cluster", s.handleGetCluster)
	mux.HandleFunc("/api/graph/search", s.handleSearch)
	mux.HandleFunc("/api/events", s.handleSSE)

	return mux
}

// graphResponse is the full graph payload for initial load.
type graphResponse struct {
	Nodes []*graph.Node    `json:"nodes"`
	Edges []*graph.Edge    `json:"edges"`
	Stats graph.GraphStats `json:"stats"`
}

func (s *Server) handleGetGraph(w http.ResponseWriter, r *http.Request) {
	nodes := s.graph.AllNodes()
	edges := s.graph.AllEdges()
	stats := s.graph.Stats()

	// Strip heavy fields for web transport
	briefNodes := make([]*graph.Node, len(nodes))
	for i, n := range nodes {
		briefNodes[i] = &graph.Node{
			ID:        n.ID,
			Kind:      n.Kind,
			Name:      n.Name,
			FilePath:  n.FilePath,
			StartLine: n.StartLine,
			Language:  n.Language,
		}
	}

	writeJSON(w, graphResponse{Nodes: briefNodes, Edges: edges, Stats: stats})
}

func (s *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.graph.Stats())
}

func (s *Server) handleGetFileGraph(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}

	sub := s.engine.GetFileSymbols(path)
	writeJSON(w, sub)
}

func (s *Server) handleGetCluster(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id parameter", http.StatusBadRequest)
		return
	}

	radius := 2
	if rs := r.URL.Query().Get("radius"); rs != "" {
		if v, err := strconv.Atoi(rs); err == nil && v > 0 {
			radius = v
		}
	}

	opts := query.QueryOptions{Depth: radius, Limit: 100, Detail: "brief"}
	sub := s.engine.GetCluster(id, opts)
	writeJSON(w, sub)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, []*graph.Node{})
		return
	}

	// Search across all nodes by name prefix/substring
	allNodes := s.graph.AllNodes()
	var results []*graph.Node
	lower := strings.ToLower(q)
	for _, n := range allNodes {
		if strings.Contains(strings.ToLower(n.Name), lower) {
			results = append(results, n)
			if len(results) >= 50 {
				break
			}
		}
	}
	writeJSON(w, results)
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	if s.hub == nil {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		fmt.Fprintf(w, ": watch mode not active\n\n")
		flusher.Flush()
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	subID, ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(subID)

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: graph_change\nid: %d\ndata: %s\n\n",
				ev.Timestamp.UnixMilli(), string(data))
			flusher.Flush()

		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()

		case <-ctx.Done():
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
