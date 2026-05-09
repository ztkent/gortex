package lsp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// Provider uses an LSP server for on-demand semantic queries.
type Provider struct {
	command     string
	args        []string
	languages   []string
	daemon      bool
	maxParallel int
	logger      *zap.Logger
	// spec is the ServerSpec this provider was built from (when the
	// caller used NewProviderFromSpec). nil for legacy NewProvider
	// invocations — those fall back to single-language routing.
	spec *ServerSpec

	client *Client

	// sourceCache holds file contents read by openDocument so the
	// per-symbol column-resolution lookups don't reread the file
	// for every hover / references / implementation query. Keyed
	// by absolute path. Eviction is not implemented — the cache
	// lives only for the duration of one Enrich pass.
	sourceCache map[string][]byte

	// docMu guards docVersions / openDocs / lastDiag so concurrent
	// callers (LSP push notifications + MCP request goroutines) can
	// share one client safely.
	docMu       sync.RWMutex
	docVersions map[string]int          // absPath → most-recent didOpen / didChange version
	openDocs    map[string]bool         // absPath → already opened
	lastDiag    map[string][]Diagnostic // absPath → most recent diagnostics from publishDiagnostics

	// diagWaitersMu guards diagWaiters which lets sync code wait for
	// the next publishDiagnostics for a given file (e.g. fix-all
	// loops re-collecting diagnostics after each apply).
	diagWaitersMu sync.Mutex
	diagWaiters   map[string][]chan []Diagnostic

	// diagHookMu guards diagHook — a single persistent subscriber the
	// router (or any caller) can install to be notified on every
	// publishDiagnostics. The hook MUST be non-blocking; it runs on
	// the LSP client's message-pump goroutine.
	diagHookMu sync.RWMutex
	diagHook   func(absPath string, diags []Diagnostic)
}

// NewProvider creates an LSP provider.
func NewProvider(command string, args []string, languages []string, daemon bool, maxParallel int, logger *zap.Logger) *Provider {
	if maxParallel <= 0 {
		maxParallel = 10
	}
	return &Provider{
		command:     command,
		args:        args,
		languages:   languages,
		daemon:      daemon,
		maxParallel: maxParallel,
		logger:      logger,
		docVersions: map[string]int{},
		openDocs:    map[string]bool{},
		lastDiag:    map[string][]Diagnostic{},
		diagWaiters: map[string][]chan []Diagnostic{},
	}
}

// NewProviderFromSpec builds a Provider directly from a ServerSpec.
// Mostly equivalent to NewProvider but lets the runtime router resolve
// the right `languageId` per file extension and pick the first
// available command from the spec's alternatives.
func NewProviderFromSpec(spec *ServerSpec, logger *zap.Logger) *Provider {
	cmd := spec.Command
	args := spec.Args
	if _, err := exec.LookPath(cmd); err != nil {
		for _, alt := range spec.AlternativeCommands {
			if _, err := exec.LookPath(alt.Command); err == nil {
				cmd = alt.Command
				args = alt.Args
				break
			}
		}
	}
	maxParallel := spec.MaxParallel
	if maxParallel <= 0 {
		maxParallel = 10
	}
	p := &Provider{
		command:     cmd,
		args:        args,
		languages:   spec.Languages,
		daemon:      spec.Daemon,
		maxParallel: maxParallel,
		logger:      logger,
		spec:        spec,
		docVersions: map[string]int{},
		openDocs:    map[string]bool{},
		lastDiag:    map[string][]Diagnostic{},
		diagWaiters: map[string][]chan []Diagnostic{},
	}
	return p
}

func (p *Provider) Name() string        { return "lsp-" + p.command }
func (p *Provider) Languages() []string { return p.languages }

func (p *Provider) Available() bool {
	_, err := exec.LookPath(p.command)
	return err == nil
}

func (p *Provider) Close() error {
	if p.client != nil {
		return p.client.Shutdown()
	}
	return nil
}

func (p *Provider) Enrich(g *graph.Graph, repoRoot string) (*semantic.EnrichResult, error) {
	start := time.Now()

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}

	// Start or reuse client.
	if err := p.ensureClient(absRoot); err != nil {
		return nil, fmt.Errorf("start LSP server: %w", err)
	}

	result := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: p.languages[0],
	}

	// Collect nodes that need enrichment (AMBIGUOUS or INFERRED edges).
	type enrichTarget struct {
		node *graph.Node
		edge *graph.Edge
	}

	var targets []enrichTarget
	for _, e := range g.AllEdges() {
		if e.Confidence >= 1.0 {
			continue
		}
		fromNode := g.GetNode(e.From)
		if fromNode == nil {
			continue
		}
		langMatch := false
		for _, lang := range p.languages {
			if fromNode.Language == lang {
				langMatch = true
				break
			}
		}
		if langMatch {
			targets = append(targets, enrichTarget{node: fromNode, edge: e})
		}
	}

	// Count total symbols.
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		for _, lang := range p.languages {
			if n.Language == lang {
				result.SymbolsTotal++
				break
			}
		}
	}

	// Open documents for files that have targets.
	openedFiles := make(map[string]bool)
	for _, t := range targets {
		if !openedFiles[t.node.FilePath] {
			if err := p.openDocument(absRoot, t.node.FilePath); err != nil {
				p.logger.Debug("LSP: failed to open document",
					zap.String("file", t.node.FilePath),
					zap.Error(err),
				)
				continue
			}
			openedFiles[t.node.FilePath] = true
		}
	}

	// Query hover info for nodes to enrich metadata.
	enrichedNodes := make(map[string]bool)
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		langMatch := false
		for _, lang := range p.languages {
			if n.Language == lang {
				langMatch = true
				break
			}
		}
		if !langMatch {
			continue
		}

		if !openedFiles[n.FilePath] {
			if err := p.openDocument(absRoot, n.FilePath); err != nil {
				continue
			}
			openedFiles[n.FilePath] = true
		}

		col := identifierColumn(p.getSource(absRoot, n.FilePath), n.StartLine, n.Name)
		hoverResult, err := p.hover(absRoot, n.FilePath, n.StartLine-1, col)
		if err != nil || hoverResult == nil {
			continue
		}

		typeInfo := extractTypeFromHover(hoverResult.Contents.Value)
		if typeInfo != "" {
			semantic.EnrichNodeMeta(n, "semantic_type", typeInfo, p.Name())
			if !enrichedNodes[n.ID] {
				result.NodesEnriched++
				result.SymbolsCovered++
				enrichedNodes[n.ID] = true
			}
		}
	}

	// Query implementations for interface nodes.
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindInterface {
			continue
		}
		langMatch := false
		for _, lang := range p.languages {
			if n.Language == lang {
				langMatch = true
				break
			}
		}
		if !langMatch {
			continue
		}

		col := identifierColumn(p.getSource(absRoot, n.FilePath), n.StartLine, n.Name)
		impls, err := p.findImplementations(absRoot, n.FilePath, n.StartLine-1, col)
		if err != nil || len(impls) == 0 {
			continue
		}

		for _, loc := range impls {
			implPath := uriToPath(loc.URI, absRoot)
			if implPath == "" {
				continue
			}
			implNode := semantic.MatchNodeByFileLine(g, implPath, loc.Range.Start.Line+1)
			if implNode == nil {
				continue
			}

			existing := semantic.FindMatchingEdge(g, implNode.ID, n.ID, graph.EdgeImplements)
			if existing != nil {
				if existing.Confidence < 1.0 {
					semantic.ConfirmEdge(existing, p.Name())
					result.EdgesConfirmed++
				}
			} else {
				semantic.AddSemanticEdge(g, implNode.ID, n.ID, graph.EdgeImplements,
					implNode.FilePath, implNode.StartLine, p.Name())
				result.EdgesAdded++
			}
		}
	}

	// Call hierarchy: ask gopls/jdtls/rust-analyzer/... for
	// outgoing calls per indexed function and use them to promote
	// existing call edges to lsp_resolved (or add edges that AST
	// extraction missed when the callee is in another file).
	p.enrichCallHierarchy(g, absRoot, result)

	// Type hierarchy: ask the server for super- and sub-types of
	// each indexed type/interface and emit EdgeExtends /
	// EdgeImplements / EdgeComposes — the single biggest non-Go
	// win because the AST extractor handles interface and type
	// inheritance with very low fidelity outside Go.
	p.enrichTypeHierarchy(g, absRoot, result)

	// Query references for AMBIGUOUS edges to confirm/refute.
	for _, t := range targets {
		toNode := g.GetNode(t.edge.To)
		if toNode == nil {
			continue
		}

		col := identifierColumn(p.getSource(absRoot, toNode.FilePath), toNode.StartLine, toNode.Name)
		refs, err := p.findReferences(absRoot, toNode.FilePath, toNode.StartLine-1, col)
		if err != nil || len(refs) == 0 {
			continue
		}

		// Check if any reference matches the caller's location.
		confirmed := false
		for _, ref := range refs {
			refPath := uriToPath(ref.URI, absRoot)
			if refPath == t.node.FilePath &&
				ref.Range.Start.Line+1 >= t.node.StartLine &&
				ref.Range.Start.Line+1 <= t.node.EndLine {
				confirmed = true
				break
			}
		}

		if confirmed {
			semantic.ConfirmEdge(t.edge, p.Name())
			result.EdgesConfirmed++
		}
	}

	if result.SymbolsTotal > 0 {
		result.CoveragePercent = float64(result.SymbolsCovered) / float64(result.SymbolsTotal) * 100
	}

	result.DurationMs = time.Since(start).Milliseconds()
	return result, nil
}

func (p *Provider) EnrichFile(g *graph.Graph, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	// LSP supports incremental updates, but for simplicity we skip it.
	// The full Enrich pass handles this.
	return nil, nil
}

// ensureClient starts the LSP server if not already running.
func (p *Provider) ensureClient(workspaceRoot string) error {
	if p.client != nil {
		return nil
	}

	client, err := NewClient(p.command, p.args, workspaceRoot, p.logger)
	if err != nil {
		return err
	}

	// Wire diagnostic + reverse-RPC handlers before initialize so we
	// don't lose the first publishDiagnostics burst that some servers
	// emit during workspace warmup.
	client.OnNotification("textDocument/publishDiagnostics",
		func(_ string, params json.RawMessage) {
			var pd PublishDiagnosticsParams
			if err := json.Unmarshal(params, &pd); err != nil {
				return
			}
			abs := uriToAbsPath(pd.URI)
			if abs == "" {
				return
			}
			p.docMu.Lock()
			p.lastDiag[abs] = pd.Diagnostics
			p.docMu.Unlock()
			p.fanoutDiagnostics(abs, pd.Diagnostics)
		})
	// Some servers (rust-analyzer, jdtls) emit progress / log messages
	// — silently swallow them; they're not actionable for the indexer.
	for _, m := range []string{
		"$/progress", "window/logMessage", "window/showMessage",
		"telemetry/event", "$/cancelRequest",
	} {
		client.OnNotification(m, func(_ string, _ json.RawMessage) {})
	}

	// Reply OK to common reverse-RPC requests so servers don't stall.
	// We never *need* to mutate workspace settings — saying "applied"
	// to applyEdit when we're an indexer is wrong, so we say no by
	// default and let H4's apply path opt in explicitly.
	client.OnRequest("workspace/configuration",
		func(_ string, _ json.RawMessage) (any, *jsonRPCError) {
			// Reply with one nil per requested item — servers that ask
			// for configuration treat null as "use defaults".
			return []any{nil}, nil
		})
	client.OnRequest("client/registerCapability",
		func(_ string, _ json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	client.OnRequest("client/unregisterCapability",
		func(_ string, _ json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	client.OnRequest("workspace/applyEdit",
		func(_ string, _ json.RawMessage) (any, *jsonRPCError) {
			// Default: refuse. The H4 apply path swaps this handler
			// before issuing executeCommand so server-driven applies
			// land on disk via WriteWorkspaceEdit.
			return ApplyWorkspaceEditResponse{Applied: false, FailureReason: "applies are routed through gortex"}, nil
		})

	initParams := InitializeParams{
		ProcessID: os.Getpid(),
		RootURI:   pathToURI(workspaceRoot),
		Capabilities: ClientCapabilities{
			Workspace: &WorkspaceClientCapabilities{
				ApplyEdit: true,
				WorkspaceEdit: &WorkspaceEditClientCapabilities{
					DocumentChanges:    true,
					ResourceOperations: []string{"create", "rename", "delete"},
				},
				ExecuteCommand:   &ExecuteCommandCapability{DynamicRegistration: true},
				WorkspaceFolders: true,
				Configuration:    true,
			},
			TextDocument: TextDocumentClientCapabilities{
				Synchronization: &SynchronizationCapability{DynamicRegistration: true},
				Implementation:  &ImplementationCapability{DynamicRegistration: true},
				References:      &ReferencesCapability{DynamicRegistration: true},
				Definition:      &DefinitionCapability{DynamicRegistration: true},
				Hover:           &HoverCapability{ContentFormat: []string{"plaintext"}},
				CallHierarchy:   &CallHierarchyCapability{DynamicRegistration: true},
				TypeHierarchy:   &TypeHierarchyCapability{DynamicRegistration: true},
				CodeAction: &CodeActionCapability{
					DynamicRegistration: true,
					CodeActionLiteralSupport: &CodeActionLiteralSupport{
						CodeActionKind: CodeActionKindCapability{
							ValueSet: []string{
								CodeActionKindEmpty,
								CodeActionKindQuickFix,
								CodeActionKindRefactor,
								CodeActionKindRefactorExtract,
								CodeActionKindRefactorInline,
								CodeActionKindRefactorRewrite,
								CodeActionKindSource,
								CodeActionKindSourceOrganizeImports,
								CodeActionKindSourceFixAll,
							},
						},
					},
					IsPreferredSupport: true,
				},
				PublishDiagnostics: &PublishDiagnosticsCapability{
					RelatedInformation: true,
					VersionSupport:     true,
				},
			},
		},
	}

	var initResult InitializeResult
	if err := client.Call("initialize", initParams, &initResult); err != nil {
		_ = client.Shutdown()
		return fmt.Errorf("initialize: %w", err)
	}

	// Send initialized notification.
	if err := client.Notify("initialized", struct{}{}); err != nil {
		_ = client.Shutdown()
		return fmt.Errorf("initialized: %w", err)
	}

	p.client = client
	return nil
}

// fanoutDiagnostics wakes everyone who called WaitForDiagnostics for
// this absPath AND invokes the persistent hook installed via
// SetDiagnosticsHook (if any). Runs with no provider lock held.
//
// The hook MUST NOT block — this method runs on the LSP client's
// message-pump goroutine. The MCP-level wiring uses
// `SendNotificationToAllClients` which is non-blocking by design (the
// SDK drops to an error hook when a session's notification channel is
// full).
func (p *Provider) fanoutDiagnostics(absPath string, diags []Diagnostic) {
	p.diagWaitersMu.Lock()
	waiters := p.diagWaiters[absPath]
	delete(p.diagWaiters, absPath)
	p.diagWaitersMu.Unlock()
	for _, ch := range waiters {
		select {
		case ch <- diags:
		default:
		}
	}
	p.diagHookMu.RLock()
	hook := p.diagHook
	p.diagHookMu.RUnlock()
	if hook != nil {
		hook(absPath, diags)
	}
}

// SetDiagnosticsHook installs a persistent callback invoked for every
// `textDocument/publishDiagnostics` the LSP server emits for this
// provider. Pass nil to detach. The Router uses this to forward LSP
// diagnostics to MCP clients via `notifications/diagnostics`.
//
// The hook MUST NOT block — see fanoutDiagnostics doc.
func (p *Provider) SetDiagnosticsHook(hook func(absPath string, diags []Diagnostic)) {
	p.diagHookMu.Lock()
	p.diagHook = hook
	p.diagHookMu.Unlock()
}

// uriToAbsPath converts a file:// URI to an absolute filesystem path.
// Returns "" for non-file URIs or malformed input.
func uriToAbsPath(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	if parsed.Scheme != "" && parsed.Scheme != "file" {
		return ""
	}
	return parsed.Path
}

// openDocument sends textDocument/didOpen for a file. Tracks version
// 1 in docVersions so a later didChange can monotonically bump it.
// Idempotent — a second call to openDocument with the same path is a
// no-op.
func (p *Provider) openDocument(repoRoot, relPath string) error {
	absPath := filepath.Join(repoRoot, relPath)
	p.docMu.Lock()
	if p.openDocs[absPath] {
		p.docMu.Unlock()
		return nil
	}
	p.docMu.Unlock()

	content, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	if p.sourceCache == nil {
		p.sourceCache = map[string][]byte{}
	}
	p.sourceCache[absPath] = content

	langID := p.languageIDFor(absPath)

	if err := p.client.Notify("textDocument/didOpen", DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:        pathToURI(absPath),
			LanguageID: langID,
			Version:    1,
			Text:       string(content),
		},
	}); err != nil {
		return err
	}
	p.docMu.Lock()
	p.openDocs[absPath] = true
	p.docVersions[absPath] = 1
	p.docMu.Unlock()
	return nil
}

// languageIDFor picks the LSP `languageId` to send in didOpen. When
// the provider was built from a ServerSpec, the spec's per-extension
// table wins; otherwise we fall back to the first configured language.
// Final fallback is the file's extension stripped of its leading dot.
func (p *Provider) languageIDFor(absPath string) string {
	if p.spec != nil {
		ext := strings.ToLower(filepath.Ext(absPath))
		if id, ok := p.spec.LanguageIDs[ext]; ok && id != "" {
			return id
		}
	}
	if len(p.languages) > 0 {
		return p.languages[0]
	}
	if ext := strings.ToLower(filepath.Ext(absPath)); ext != "" {
		return strings.TrimPrefix(ext, ".")
	}
	return ""
}

// changeDocument sends textDocument/didChange with a full-text replace
// and bumps the document's version monotonically.
func (p *Provider) changeDocument(absPath, newText string) error {
	p.docMu.Lock()
	v := p.docVersions[absPath] + 1
	p.docVersions[absPath] = v
	p.docMu.Unlock()
	if p.sourceCache == nil {
		p.sourceCache = map[string][]byte{}
	}
	p.sourceCache[absPath] = []byte(newText)
	return p.client.Notify("textDocument/didChange", DidChangeTextDocumentParams{
		TextDocument: VersionedTextDocumentIdentifier{
			URI:     pathToURI(absPath),
			Version: v,
		},
		ContentChanges: []TextDocumentContentChangeEvent{{Text: newText}},
	})
}

// closeDocument sends textDocument/didClose. Idempotent.
func (p *Provider) closeDocument(absPath string) error {
	p.docMu.Lock()
	if !p.openDocs[absPath] {
		p.docMu.Unlock()
		return nil
	}
	delete(p.openDocs, absPath)
	delete(p.docVersions, absPath)
	p.docMu.Unlock()
	return p.client.Notify("textDocument/didClose", DidCloseTextDocumentParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
	})
}

// LastDiagnostics returns the most recent diagnostics published for a
// file. Returns nil + false when the server has not (yet) emitted
// diagnostics for that path.
func (p *Provider) LastDiagnostics(absPath string) ([]Diagnostic, bool) {
	p.docMu.RLock()
	defer p.docMu.RUnlock()
	d, ok := p.lastDiag[absPath]
	if !ok {
		return nil, false
	}
	out := make([]Diagnostic, len(d))
	copy(out, d)
	return out, true
}

// WaitForDiagnostics blocks until the server publishes the next
// publishDiagnostics for absPath, or the timeout elapses (returning the
// last known diagnostics if any). Callers register their interest
// before triggering the change that will cause the publish, otherwise
// they may miss the event.
func (p *Provider) WaitForDiagnostics(absPath string, timeout time.Duration) []Diagnostic {
	ch := make(chan []Diagnostic, 1)
	p.diagWaitersMu.Lock()
	p.diagWaiters[absPath] = append(p.diagWaiters[absPath], ch)
	p.diagWaitersMu.Unlock()
	select {
	case d := <-ch:
		return d
	case <-time.After(timeout):
		// Drain & remove our waiter so we don't leak.
		p.diagWaitersMu.Lock()
		var kept []chan []Diagnostic
		for _, w := range p.diagWaiters[absPath] {
			if w != ch {
				kept = append(kept, w)
			}
		}
		p.diagWaiters[absPath] = kept
		p.diagWaitersMu.Unlock()
		if d, ok := p.LastDiagnostics(absPath); ok {
			return d
		}
		return nil
	}
}

// Client exposes the underlying LSP client for advanced callers (e.g.
// the daemon router). Returns nil before ensureClient succeeds.
func (p *Provider) Client() *Client { return p.client }

// EnsureClient is the exported form of ensureClient — it spawns the
// LSP server (idempotent) so callers that want diagnostics or code
// actions outside an Enrich pass can prime the connection on demand.
func (p *Provider) EnsureClient(workspaceRoot string) error {
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return err
	}
	return p.ensureClient(abs)
}

// EnsureFileOpen makes sure the document is opened on the server (with
// version 1) so request methods that take a position can proceed.
func (p *Provider) EnsureFileOpen(repoRoot, relPath string) error {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return err
	}
	return p.openDocument(abs, relPath)
}

// getSource returns cached file content from the most recent
// openDocument call. Returns nil when not cached — callers fall
// back to col=0 then.
func (p *Provider) getSource(repoRoot, relPath string) []byte {
	if p.sourceCache == nil {
		return nil
	}
	return p.sourceCache[filepath.Join(repoRoot, relPath)]
}

// hover queries hover info for a position.
func (p *Provider) hover(repoRoot, relPath string, line, col int) (*HoverResult, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := HoverParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}

	var result HoverResult
	if err := p.client.Call("textDocument/hover", params, &result); err != nil {
		return nil, err
	}
	if result.Contents.Value == "" {
		return nil, nil
	}
	return &result, nil
}

// findImplementations queries textDocument/implementation.
func (p *Provider) findImplementations(repoRoot, relPath string, line, col int) ([]Location, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := ImplementationParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}

	var locations []Location
	if err := p.client.Call("textDocument/implementation", params, &locations); err != nil {
		return nil, err
	}
	return locations, nil
}

// CodeActionsRequest carries the params for a single
// textDocument/codeAction call.
type CodeActionsRequest struct {
	// AbsPath is the absolute path to the file the cursor is in.
	AbsPath string
	// Range narrows the request. Pass {} for the whole file.
	Range Range
	// Diagnostics is the set of diagnostics the actions should
	// address — typically a recent slice from LastDiagnostics.
	Diagnostics []Diagnostic
	// Only restricts the kind of actions returned (e.g.
	// CodeActionKindQuickFix, CodeActionKindSourceOrganizeImports).
	Only []string
}

// GetCodeActions issues textDocument/codeAction and returns a unified
// list of CodeActionOrCommand. The provider must already have opened
// the document via EnsureFileOpen before calling this.
func (p *Provider) GetCodeActions(req CodeActionsRequest) ([]CodeActionOrCommand, error) {
	if p.client == nil {
		return nil, fmt.Errorf("LSP client not initialized")
	}
	params := CodeActionParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(req.AbsPath)},
		Range:        req.Range,
		Context: CodeActionContext{
			Diagnostics: req.Diagnostics,
			Only:        req.Only,
			TriggerKind: 1, // Invoked.
		},
	}
	var raw []json.RawMessage
	if err := p.client.Call("textDocument/codeAction", params, &raw); err != nil {
		return nil, err
	}
	out := make([]CodeActionOrCommand, 0, len(raw))
	for _, item := range raw {
		var u CodeActionOrCommand
		if err := json.Unmarshal(item, &u); err != nil {
			continue
		}
		// Legacy Command form has the shape {title, command, arguments}.
		// CodeAction literal has {title, kind?, edit?, command?, ...}.
		// json.Unmarshal handles both with the unified struct above.
		out = append(out, u)
	}
	return out, nil
}

// ResolveCodeAction calls codeAction/resolve. Some servers (rust-
// analyzer, jdtls) defer the heavy WorkspaceEdit computation until
// resolve to keep the initial codeAction call cheap.
func (p *Provider) ResolveCodeAction(action CodeActionOrCommand) (CodeActionOrCommand, error) {
	if p.client == nil {
		return action, fmt.Errorf("LSP client not initialized")
	}
	var resolved CodeActionOrCommand
	if err := p.client.Call("codeAction/resolve", action, &resolved); err != nil {
		return action, err
	}
	return resolved, nil
}

// ExecuteCommand issues workspace/executeCommand. Used by the H4 apply
// path when a CodeAction has only a Command (legacy) form.
func (p *Provider) ExecuteCommand(cmd Command) (json.RawMessage, error) {
	if p.client == nil {
		return nil, fmt.Errorf("LSP client not initialized")
	}
	params := ExecuteCommandParams{Command: cmd.Command, Arguments: cmd.Arguments}
	var result json.RawMessage
	if err := p.client.Call("workspace/executeCommand", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// findReferences queries textDocument/references.
func (p *Provider) findReferences(repoRoot, relPath string, line, col int) ([]Location, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := ReferenceParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
		Context: ReferenceContext{IncludeDeclaration: false},
	}

	var locations []Location
	if err := p.client.Call("textDocument/references", params, &locations); err != nil {
		return nil, err
	}
	return locations, nil
}

// enrichCallHierarchy walks every function/method node in p.languages
// and uses callHierarchy/{prepare, outgoingCalls} to either promote a
// matching ast_inferred / text_matched EdgeCalls to lsp_resolved, or
// add a fresh EdgeCalls when the AST extractor missed the link
// (cross-file calls in languages without compile-unit info).
func (p *Provider) enrichCallHierarchy(g *graph.Graph, absRoot string, result *semantic.EnrichResult) {
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if !p.languageMatches(n.Language) {
			continue
		}
		col := identifierColumn(p.getSource(absRoot, n.FilePath), n.StartLine, n.Name)
		items, err := p.prepareCallHierarchy(absRoot, n.FilePath, n.StartLine-1, col)
		if err != nil || len(items) == 0 {
			continue
		}
		for _, item := range items {
			calls, err := p.outgoingCalls(item)
			if err == nil {
				for _, c := range calls {
					p.recordHierarchyCall(g, absRoot, n, c.To, true, result)
				}
			}
			incoming, err := p.incomingCalls(item)
			if err == nil {
				for _, c := range incoming {
					p.recordHierarchyCall(g, absRoot, n, c.From, false, result)
				}
			}
		}
	}
}

// recordHierarchyCall lands one call-hierarchy hop into the graph.
// asOutgoing=true means "this node calls other"; false means "other
// calls this node" (incoming-calls direction). Existing edges get
// promoted to lsp_resolved; missing edges get added.
func (p *Provider) recordHierarchyCall(g *graph.Graph, absRoot string, n *graph.Node, other CallHierarchyItem, asOutgoing bool, result *semantic.EnrichResult) {
	otherPath := uriToPath(other.URI, absRoot)
	if otherPath == "" {
		return
	}
	otherNode := semantic.MatchNodeByFileLine(g, otherPath,
		other.SelectionRange.Start.Line+1)
	if otherNode == nil {
		return
	}
	from, to := n, otherNode
	if !asOutgoing {
		from, to = otherNode, n
	}
	if from.ID == to.ID {
		return
	}
	existing := semantic.FindMatchingEdge(g, from.ID, to.ID, graph.EdgeCalls)
	if existing != nil {
		if graph.OriginRank(existing.Origin) < graph.OriginRank(graph.OriginLSPResolved) {
			semantic.ConfirmEdge(existing, p.Name())
			existing.Origin = graph.OriginLSPResolved
			result.EdgesConfirmed++
		}
		return
	}
	semantic.AddSemanticEdge(g, from.ID, to.ID, graph.EdgeCalls,
		from.FilePath, from.StartLine, p.Name())
	result.EdgesAdded++
}

// enrichTypeHierarchy walks every type / interface node and uses
// typeHierarchy/{prepare, supertypes, subtypes} to fill EdgeExtends
// / EdgeImplements / EdgeComposes for non-Go languages where AST
// extraction can't follow `extends X` / `implements I` across files.
//
//   - supertypes(T) = the parents T extends/implements. Emits
//     EdgeExtends T → super for class hierarchy and EdgeImplements
//     T → super when the super is an interface kind.
//   - subtypes(T) = the children of T. Emits EdgeImplements child
//     → T when T is an interface; EdgeExtends otherwise.
func (p *Provider) enrichTypeHierarchy(g *graph.Graph, absRoot string, result *semantic.EnrichResult) {
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
			continue
		}
		if !p.languageMatches(n.Language) {
			continue
		}
		col := identifierColumn(p.getSource(absRoot, n.FilePath), n.StartLine, n.Name)
		items, err := p.prepareTypeHierarchy(absRoot, n.FilePath, n.StartLine-1, col)
		if err != nil || len(items) == 0 {
			continue
		}
		for _, item := range items {
			supers, _ := p.supertypes(item)
			for _, s := range supers {
				p.linkTypeHierarchy(g, absRoot, n, s, true, result)
			}
			subs, _ := p.subtypes(item)
			for _, s := range subs {
				p.linkTypeHierarchy(g, absRoot, n, s, false, result)
			}
		}
	}
}

// linkTypeHierarchy emits the right edge kind for one super/subtype
// hop. When asSupertype=true, the hop is `cur → other` (cur extends
// or implements other). When false, the hop is `other → cur`.
//
// Beyond the type-level edge, it also walks the methods of the child
// type (the `from` side) and emits EdgeOverrides for every method
// whose name matches a method on the parent — closing the
// method-level half of I2 (Joern calls these CONTAINS + OVERRIDES).
func (p *Provider) linkTypeHierarchy(g *graph.Graph, absRoot string, cur *graph.Node, other TypeHierarchyItem, asSupertype bool, result *semantic.EnrichResult) {
	otherPath := uriToPath(other.URI, absRoot)
	if otherPath == "" {
		return
	}
	otherNode := semantic.MatchNodeByFileLine(g, otherPath, other.SelectionRange.Start.Line+1)
	if otherNode == nil {
		return
	}
	from, to := cur, otherNode
	if !asSupertype {
		from, to = otherNode, cur
	}
	kind := graph.EdgeExtends
	if to.Kind == graph.KindInterface {
		kind = graph.EdgeImplements
	}
	if from.ID == to.ID {
		return
	}
	existing := semantic.FindMatchingEdge(g, from.ID, to.ID, kind)
	if existing != nil {
		if graph.OriginRank(existing.Origin) < graph.OriginRank(graph.OriginLSPResolved) {
			semantic.ConfirmEdge(existing, p.Name())
			existing.Origin = graph.OriginLSPResolved
			result.EdgesConfirmed++
		}
	} else {
		semantic.AddSemanticEdge(g, from.ID, to.ID, kind, from.FilePath, from.StartLine, p.Name())
		result.EdgesAdded++
	}

	// Method-level override edges: child methods that share a name
	// with parent methods.
	addOverrideEdges(g, from, to, p.Name(), graph.OriginLSPDispatch, result)
}

// addOverrideEdges emits EdgeOverrides from each method of child to
// the matching method of parent (matched by name). Parent methods are
// resolved via EdgeMemberOf (`m -member_of-> parent`) so the routine
// works regardless of language as long as the AST extractor recorded
// member_of for methods.
//
// origin lets the caller stamp the edges with lsp_dispatch (LSP-
// confirmed parent), ast_resolved (AST-confirmed parent in the same
// compilation unit), or ast_inferred (parent is a heuristic match).
func addOverrideEdges(g *graph.Graph, child, parent *graph.Node, provider, origin string, result *semantic.EnrichResult) {
	if child == nil || parent == nil || child.ID == parent.ID {
		return
	}
	parentMethods := map[string]*graph.Node{}
	for _, e := range g.GetInEdges(parent.ID) {
		if e.Kind != graph.EdgeMemberOf {
			continue
		}
		m := g.GetNode(e.From)
		if m == nil || m.Kind != graph.KindMethod {
			continue
		}
		parentMethods[m.Name] = m
	}
	if len(parentMethods) == 0 {
		return
	}
	for _, e := range g.GetInEdges(child.ID) {
		if e.Kind != graph.EdgeMemberOf {
			continue
		}
		m := g.GetNode(e.From)
		if m == nil || m.Kind != graph.KindMethod {
			continue
		}
		pm, ok := parentMethods[m.Name]
		if !ok || pm.ID == m.ID {
			continue
		}
		existing := semantic.FindMatchingEdge(g, m.ID, pm.ID, graph.EdgeOverrides)
		if existing != nil {
			if graph.OriginRank(existing.Origin) < graph.OriginRank(origin) {
				semantic.ConfirmEdge(existing, provider)
				existing.Origin = origin
				if result != nil {
					result.EdgesConfirmed++
				}
			}
			continue
		}
		ed := semantic.AddSemanticEdge(g, m.ID, pm.ID, graph.EdgeOverrides, m.FilePath, m.StartLine, provider)
		if ed != nil {
			ed.Origin = origin
		}
		if result != nil {
			result.EdgesAdded++
		}
	}
}

// languageMatches returns true when n.Language is one of the
// languages this provider serves.
func (p *Provider) languageMatches(lang string) bool {
	for _, l := range p.languages {
		if l == lang {
			return true
		}
	}
	return false
}

// prepareCallHierarchy queries textDocument/prepareCallHierarchy and
// returns the items the server resolved at the given position. Empty
// (and nil error) means the server doesn't recognise a function-like
// symbol at that location.
func (p *Provider) prepareCallHierarchy(repoRoot, relPath string, line, col int) ([]CallHierarchyItem, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := CallHierarchyPrepareParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}
	var items []CallHierarchyItem
	if err := p.client.Call("textDocument/prepareCallHierarchy", params, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// outgoingCalls queries callHierarchy/outgoingCalls for one item.
func (p *Provider) outgoingCalls(item CallHierarchyItem) ([]CallHierarchyOutgoingCall, error) {
	var calls []CallHierarchyOutgoingCall
	if err := p.client.Call("callHierarchy/outgoingCalls",
		CallHierarchyOutgoingCallsParams{Item: item}, &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

// incomingCalls queries callHierarchy/incomingCalls for one item.
func (p *Provider) incomingCalls(item CallHierarchyItem) ([]CallHierarchyIncomingCall, error) {
	var calls []CallHierarchyIncomingCall
	if err := p.client.Call("callHierarchy/incomingCalls",
		CallHierarchyIncomingCallsParams{Item: item}, &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

// prepareTypeHierarchy queries textDocument/prepareTypeHierarchy.
func (p *Provider) prepareTypeHierarchy(repoRoot, relPath string, line, col int) ([]TypeHierarchyItem, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := TypeHierarchyPrepareParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}
	var items []TypeHierarchyItem
	if err := p.client.Call("textDocument/prepareTypeHierarchy", params, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// supertypes queries typeHierarchy/supertypes.
func (p *Provider) supertypes(item TypeHierarchyItem) ([]TypeHierarchyItem, error) {
	var items []TypeHierarchyItem
	if err := p.client.Call("typeHierarchy/supertypes",
		TypeHierarchySupertypesParams{Item: item}, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// subtypes queries typeHierarchy/subtypes.
func (p *Provider) subtypes(item TypeHierarchyItem) ([]TypeHierarchyItem, error) {
	var items []TypeHierarchyItem
	if err := p.client.Call("typeHierarchy/subtypes",
		TypeHierarchySubtypesParams{Item: item}, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// pathToURI converts a file path to a file:// URI.
func pathToURI(path string) string {
	absPath, _ := filepath.Abs(path)
	return "file://" + absPath
}

// uriToPath converts a file:// URI to a repo-relative path.
func uriToPath(uri, repoRoot string) string {
	parsed, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	absPath := parsed.Path
	if !strings.HasPrefix(absPath, repoRoot) {
		return ""
	}
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// identifierColumn returns the 0-based column of the first
// occurrence of name on the given 1-based line of src. Returns 0
// when the source doesn't have the line, the name isn't found on
// it, or name is empty — col=0 was the previous unconditional
// default and remains a safe fallback for those edge cases.
//
// Why this matters: most LSP servers (gopls, jdtls, rust-analyzer,
// kotlin-ls, omnisharp, pyright) require the position cursor to be
// _on_ the identifier for textDocument/references and
// textDocument/implementation. Pinning to col=0 silently empty-resulted
// every method declaration in indented contexts (`func (f *Foo) Bar()`
// — col=0 is the `func` keyword, not `Bar`). Resolving to the actual
// identifier column unblocks the bulk of cross-file edge promotion.
func identifierColumn(src []byte, oneBasedLine int, name string) int {
	if name == "" || oneBasedLine <= 0 || len(src) == 0 {
		return 0
	}
	// Walk to the start of the requested line.
	target := oneBasedLine - 1
	lineStart := 0
	cur := 0
	for cur < len(src) && target > 0 {
		if src[cur] == '\n' {
			target--
			lineStart = cur + 1
		}
		cur++
	}
	if target > 0 {
		return 0
	}
	lineEnd := lineStart
	for lineEnd < len(src) && src[lineEnd] != '\n' {
		lineEnd++
	}
	line := string(src[lineStart:lineEnd])
	idx := strings.Index(line, name)
	if idx < 0 {
		return 0
	}
	return idx
}

// extractTypeFromHover extracts type information from hover text.
func extractTypeFromHover(hover string) string {
	// Remove markdown code fences.
	hover = strings.TrimPrefix(hover, "```go\n")
	hover = strings.TrimPrefix(hover, "```\n")
	hover = strings.TrimSuffix(hover, "\n```")
	hover = strings.TrimSpace(hover)

	lines := strings.SplitN(hover, "\n", 2)
	if len(lines) > 0 {
		line := strings.TrimSpace(lines[0])
		if strings.HasPrefix(line, "func ") ||
			strings.HasPrefix(line, "type ") ||
			strings.HasPrefix(line, "var ") ||
			strings.HasPrefix(line, "const ") ||
			strings.HasPrefix(line, "field ") ||
			strings.HasPrefix(line, "package ") {
			return line
		}
		// Short type like "string", "*Foo", "[]byte".
		if !strings.Contains(line, " ") && len(line) > 0 && len(line) < 100 {
			return line
		}
	}
	return ""
}
