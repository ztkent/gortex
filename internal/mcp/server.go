package mcp

import (
	"sync"

	"go.uber.org/zap"

	"github.com/mark3labs/mcp-go/server"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// Version is set at build time.
var Version = "dev"

// Server wraps the MCP server with Gortex-specific tools.
type Server struct {
	mcpServer   *server.MCPServer
	engine      *query.Engine
	graph       *graph.Graph
	indexer     *indexer.Indexer
	watcher     *indexer.Watcher
	logger      *zap.Logger
	communities *analysis.CommunityResult
	processes   *analysis.ProcessResult
	analysisMu  sync.RWMutex
}

// NewServer creates an MCP server with all Gortex tools registered.
func NewServer(engine *query.Engine, g *graph.Graph, idx *indexer.Indexer, watcher *indexer.Watcher, logger *zap.Logger) *Server {
	s := &Server{
		mcpServer: server.NewMCPServer("gortex", Version,
			server.WithToolCapabilities(false),
			server.WithRecovery(),
		),
		engine:  engine,
		graph:   g,
		indexer: idx,
		watcher: watcher,
		logger:  logger,
	}
	s.registerCoreTools()
	s.registerCodingTools()
	s.registerAnalysisTools()
	return s
}

// RunAnalysis performs community detection and process discovery on the current graph.
func (s *Server) RunAnalysis() {
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()
	s.communities = analysis.DetectCommunities(s.graph)
	s.processes = analysis.DiscoverProcesses(s.graph)
}

func (s *Server) getCommunities() *analysis.CommunityResult {
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	return s.communities
}

func (s *Server) getProcesses() *analysis.ProcessResult {
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	return s.processes
}

// ServeStdio starts the MCP server on stdin/stdout.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcpServer)
}
