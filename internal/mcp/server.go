package mcp

import (
	"math"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/mark3labs/mcp-go/server"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/web/hub"
)

// Version is set at build time.
var Version = "dev"

// SymbolModification records a single modification event for a symbol.
type SymbolModification struct {
	Timestamp        time.Time `json:"timestamp"`
	SignatureChanged bool      `json:"signature_changed"`
}

// symbolHistory tracks symbol modifications during the current session.
type symbolHistory struct {
	mu      sync.Mutex
	entries map[string][]SymbolModification // symbolID → modifications
}

// Record adds a modification entry for the given symbol.
func (sh *symbolHistory) Record(symbolID string, signatureChanged bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.entries[symbolID] = append(sh.entries[symbolID], SymbolModification{
		Timestamp:        time.Now(),
		SignatureChanged: signatureChanged,
	})
}

// Get returns the modification history for a specific symbol.
func (sh *symbolHistory) Get(symbolID string) []SymbolModification {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	mods := sh.entries[symbolID]
	out := make([]SymbolModification, len(mods))
	copy(out, mods)
	return out
}

// All returns a copy of the entire modification history.
func (sh *symbolHistory) All() map[string][]SymbolModification {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	out := make(map[string][]SymbolModification, len(sh.entries))
	for k, v := range sh.entries {
		cp := make([]SymbolModification, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// Server wraps the MCP server with Gortex-specific tools.
type Server struct {
	mcpServer    *server.MCPServer
	engine       *query.Engine
	graph        *graph.Graph
	indexer      *indexer.Indexer
	watcher      *indexer.Watcher
	multiIndexer *indexer.MultiIndexer
	configManager *config.ConfigManager
	activeProject string
	logger       *zap.Logger
	communities  *analysis.CommunityResult
	processes    *analysis.ProcessResult
	analysisMu   sync.RWMutex
	session      *sessionState
	symHistory   *symbolHistory
	tokenStats   *tokenStats
	guardRules       []config.GuardRule
	contractRegistry *contracts.Registry
}

// sessionState tracks recent agent activity for context recovery after compaction.
type sessionState struct {
	mu             sync.Mutex
	viewedSymbols  []string // recently viewed symbol IDs (most recent first)
	viewedFiles    []string // recently viewed file paths
	modifiedFiles  []string // files modified via edit_symbol
	recentSearches []string // recent search queries
}

// tokenStats tracks estimated token savings for the current session.
type tokenStats struct {
	mu             sync.Mutex
	tokensSaved    int64 // cumulative tokens saved vs reading full files
	tokensReturned int64 // cumulative tokens actually returned
	callCount      int64 // number of source-reading tool invocations
}

// record adds a single savings observation.
// returned and fullFile are token estimates (chars / 4).
func (ts *tokenStats) record(returned, fullFile int64) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	saved := fullFile - returned
	if saved < 0 {
		saved = 0
	}
	ts.tokensSaved += saved
	ts.tokensReturned += returned
	ts.callCount++
}

// snapshot returns a copy of the current counters for inclusion in responses.
func (ts *tokenStats) snapshot() map[string]any {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ratio := 0.0
	if ts.tokensReturned > 0 {
		ratio = float64(ts.tokensSaved+ts.tokensReturned) / float64(ts.tokensReturned)
	}
	return map[string]any{
		"tokens_saved":     ts.tokensSaved,
		"tokens_returned":  ts.tokensReturned,
		"calls_counted":    ts.callCount,
		"efficiency_ratio": math.Round(ratio*10) / 10,
	}
}

const maxSessionItems = 20

func newSessionState() *sessionState {
	return &sessionState{}
}

func (ss *sessionState) recordSymbol(id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.viewedSymbols = prependUnique(ss.viewedSymbols, id, maxSessionItems)
}

func (ss *sessionState) recordFile(path string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.viewedFiles = prependUnique(ss.viewedFiles, path, maxSessionItems)
}

func (ss *sessionState) recordModified(path string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.modifiedFiles = prependUnique(ss.modifiedFiles, path, maxSessionItems)
}

func (ss *sessionState) recordSearch(query string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.recentSearches = prependUnique(ss.recentSearches, query, 10)
}

func (ss *sessionState) snapshot() map[string]any {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return map[string]any{
		"viewed_symbols":  ss.viewedSymbols,
		"viewed_files":    ss.viewedFiles,
		"modified_files":  ss.modifiedFiles,
		"recent_searches": ss.recentSearches,
	}
}

func prependUnique(slice []string, item string, maxLen int) []string {
	// Remove existing occurrence.
	for i, s := range slice {
		if s == item {
			slice = append(slice[:i], slice[i+1:]...)
			break
		}
	}
	// Prepend.
	slice = append([]string{item}, slice...)
	if len(slice) > maxLen {
		slice = slice[:maxLen]
	}
	return slice
}

// MultiRepoOptions holds optional multi-repo components for the Server.
// When nil or zero-valued, the server operates in single-repo mode.
type MultiRepoOptions struct {
	MultiIndexer  *indexer.MultiIndexer
	ConfigManager *config.ConfigManager
	ActiveProject string
}

// NewServer creates an MCP server with all Gortex tools registered.
func NewServer(engine *query.Engine, g *graph.Graph, idx *indexer.Indexer, watcher *indexer.Watcher, logger *zap.Logger, guardRules []config.GuardRule, opts ...MultiRepoOptions) *Server {
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
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{
			entries: make(map[string][]SymbolModification),
		},
		guardRules: guardRules,
	}

	// Apply multi-repo options if provided.
	if len(opts) > 0 {
		o := opts[0]
		s.multiIndexer = o.MultiIndexer
		s.configManager = o.ConfigManager
		s.activeProject = o.ActiveProject
	}

	s.registerCoreTools()
	s.registerCodingTools()
	s.registerAnalysisTools()
	s.registerEnhancementTools()
	s.registerResources()
	s.registerPrompts()

	// Register multi-repo tools when multi-repo components are available.
	if s.multiIndexer != nil || s.configManager != nil {
		s.registerMultiRepoTools()
	}

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

// WatchForReanalysis subscribes to hub events and re-runs analysis after
// a debounce period of inactivity. It runs in a background goroutine.
func (s *Server) WatchForReanalysis(h *hub.Hub, debounceMs int) {
	subID, events := h.Subscribe()
	debounce := time.Duration(debounceMs) * time.Millisecond

	go func() {
		var timer *time.Timer
		for ev := range events {
			_ = ev // any event triggers reanalysis
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, func() {
				s.logger.Info("re-running analysis after graph change")
				s.RunAnalysis()
			})
		}
		// Channel closed — hub is shutting down.
		if timer != nil {
			timer.Stop()
		}
		_ = subID
	}()
}

// ServeStdio starts the MCP server on stdin/stdout.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcpServer)
}

// MCPServer returns the underlying MCP server instance.
// This is used by the eval-server to wire tool dispatch into an HTTP handler.
func (s *Server) MCPServer() *server.MCPServer {
	return s.mcpServer
}

// SetContractRegistry sets the contract registry for the MCP server.
func (s *Server) SetContractRegistry(r *contracts.Registry) {
	s.contractRegistry = r
}

// SetWatcher sets the watcher after background initialization and registers
// a symbol change callback to record modifications in symbolHistory.
func (s *Server) SetWatcher(w *indexer.Watcher) {
	s.watcher = w

	// Register callback to track symbol modifications for get_symbol_history.
	w.OnSymbolChange(func(filePath string, oldSymbols, newSymbols []*graph.Node) {
		oldMap := make(map[string]string, len(oldSymbols)) // ID → signature
		for _, n := range oldSymbols {
			sig, _ := n.Meta["signature"].(string)
			oldMap[n.ID] = sig
		}

		newMap := make(map[string]string, len(newSymbols)) // ID → signature
		for _, n := range newSymbols {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sig, _ := n.Meta["signature"].(string)
			newMap[n.ID] = sig
		}

		// Detect modified symbols (present in both old and new with changed signature).
		for id, oldSig := range oldMap {
			if newSig, exists := newMap[id]; exists {
				sigChanged := oldSig != newSig
				s.symHistory.Record(id, sigChanged)
			}
		}

		// Detect removed symbols (in old but not in new).
		for id := range oldMap {
			if _, exists := newMap[id]; !exists {
				s.symHistory.Record(id, true)
			}
		}

		// Detect added symbols (in new but not in old).
		for id := range newMap {
			if _, exists := oldMap[id]; !exists {
				s.symHistory.Record(id, false)
			}
		}
	})
}
