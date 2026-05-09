package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

// registerLSPTools wires the H4 LSP-action MCP surface:
//
//	get_diagnostics   — most recent publishDiagnostics for a file
//	get_code_actions  — textDocument/codeAction for a file (and optional range)
//	apply_code_action — apply one CodeAction → WorkspaceEdit on disk
//	fix_all_in_file   — quickfix + organizeImports loop (cap configurable)
//
// All four take a `path` (repo-relative or absolute). The right LSP
// provider is resolved by file extension via the daemon's spec
// registry. Each tool no-ops cleanly when no LSP server is registered
// for that language — callers get a structured "no_lsp_for" error
// payload instead of a hard failure.
func (s *Server) registerLSPTools() {
	s.mcpServer.AddTool(
		mcp.NewTool("get_diagnostics",
			mcp.WithDescription("Returns the most recent LSP diagnostics for a file. The file is auto-opened on the server if it isn't already, and the call optionally waits for the next publishDiagnostics burst."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Repo-relative or absolute file path")),
			mcp.WithBoolean("wait", mcp.Description("Block for the next publishDiagnostics (default: false)")),
			mcp.WithNumber("timeout_ms", mcp.Description("Wait timeout in milliseconds when wait=true (default: 5000)")),
			mcp.WithString("repo", mcp.Description("Filter to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter to repositories in a specific project")),
		),
		s.handleGetDiagnostics,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_code_actions",
			mcp.WithDescription("Returns LSP code actions (quickfix / organizeImports / refactor.* / source.*) available at a file location. Pass `only` to restrict the kinds returned."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Repo-relative or absolute file path")),
			mcp.WithNumber("start_line", mcp.Description("0-based start line of the range (default: 0)")),
			mcp.WithNumber("start_char", mcp.Description("0-based start character (default: 0)")),
			mcp.WithNumber("end_line", mcp.Description("0-based end line (default: same as start_line + 1)")),
			mcp.WithNumber("end_char", mcp.Description("0-based end character (default: 0)")),
			mcp.WithString("only", mcp.Description("Comma-separated kinds to return (e.g. quickfix,source.organizeImports)")),
			mcp.WithString("repo", mcp.Description("Filter to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter to repositories in a specific project")),
		),
		s.handleGetCodeActions,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("apply_code_action",
			mcp.WithDescription("Applies a single LSP code action to disk. Pass the action's `index` from a previous get_code_actions call, plus the same `path`/`start_line`/`only` tuple to re-resolve the action list."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Repo-relative or absolute file path")),
			mcp.WithNumber("index", mcp.Required(), mcp.Description("Index into the action list returned by get_code_actions")),
			mcp.WithNumber("start_line", mcp.Description("Same start_line passed to get_code_actions")),
			mcp.WithNumber("start_char", mcp.Description("Same start_char passed to get_code_actions")),
			mcp.WithNumber("end_line", mcp.Description("Same end_line")),
			mcp.WithNumber("end_char", mcp.Description("Same end_char")),
			mcp.WithString("only", mcp.Description("Same comma-separated kinds")),
			mcp.WithString("repo", mcp.Description("Filter to a specific repository prefix")),
		),
		s.handleApplyCodeAction,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("fix_all_in_file",
			mcp.WithDescription("Loops codeAction → apply → re-collect-diagnostics until convergence. Defaults to {quickfix, source.organizeImports}; pass `kinds` to override. Returns iteration count, total actions applied, files touched, and final diagnostics."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Repo-relative or absolute file path")),
			mcp.WithString("kinds", mcp.Description("Comma-separated CodeAction kinds. Default: quickfix,source.organizeImports")),
			mcp.WithNumber("max_iterations", mcp.Description("Cap on apply/re-diagnose cycles (default: 5)")),
			mcp.WithNumber("max_actions_per_iter", mcp.Description("Cap on actions applied per iteration (default: 50)")),
			mcp.WithNumber("diagnostic_timeout_ms", mcp.Description("How long to wait for refreshed diagnostics each iteration (default: 5000)")),
			mcp.WithString("repo", mcp.Description("Filter to a specific repository prefix")),
		),
		s.handleFixAllInFile,
	)
}

// handleGetDiagnostics implements the `get_diagnostics` tool.
func (s *Server) handleGetDiagnostics(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	wait := req.GetBool("wait", false)
	timeoutMs := req.GetInt("timeout_ms", 5000)

	provider, absPath, err := s.lspProviderForPath(path)
	if err != nil {
		return s.lspNoProviderResult(path, err), nil
	}

	if err := provider.EnsureFileOpen(filepath.Dir(absPath), filepath.Base(absPath)); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("open document: %v", err)), nil
	}

	var diags []lsp.Diagnostic
	if wait {
		diags = provider.WaitForDiagnostics(absPath, time.Duration(timeoutMs)*time.Millisecond)
	} else {
		d, _ := provider.LastDiagnostics(absPath)
		diags = d
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"path":        path,
		"diagnostics": diagsToWire(diags),
		"total":       len(diags),
		"provider":    provider.Name(),
	})
}

// handleGetCodeActions implements the `get_code_actions` tool.
func (s *Server) handleGetCodeActions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	rng := rangeFromRequest(req)
	only := splitCSV(req.GetString("only", ""))

	provider, absPath, err := s.lspProviderForPath(path)
	if err != nil {
		return s.lspNoProviderResult(path, err), nil
	}
	if err := provider.EnsureFileOpen(filepath.Dir(absPath), filepath.Base(absPath)); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("open document: %v", err)), nil
	}
	diags, _ := provider.LastDiagnostics(absPath)

	actions, err := provider.GetCodeActions(lsp.CodeActionsRequest{
		AbsPath:     absPath,
		Range:       rng,
		Diagnostics: diags,
		Only:        only,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("code action request failed: %v", err)), nil
	}

	wire := make([]map[string]any, 0, len(actions))
	for i, a := range actions {
		entry := map[string]any{
			"index":        i,
			"title":        a.Title,
			"kind":         a.Kind,
			"is_preferred": a.IsPreferred,
			"has_edit":     a.Edit != nil,
			"has_command":  a.Command != "",
		}
		if a.Disabled != nil {
			entry["disabled_reason"] = a.Disabled.Reason
		}
		wire = append(wire, entry)
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"path":     path,
		"provider": provider.Name(),
		"actions":  wire,
		"total":    len(wire),
	})
}

// handleApplyCodeAction implements the `apply_code_action` tool.
func (s *Server) handleApplyCodeAction(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	idx := req.GetInt("index", -1)
	if idx < 0 {
		return mcp.NewToolResultError("index is required"), nil
	}
	rng := rangeFromRequest(req)
	only := splitCSV(req.GetString("only", ""))

	provider, absPath, err := s.lspProviderForPath(path)
	if err != nil {
		return s.lspNoProviderResult(path, err), nil
	}
	if err := provider.EnsureFileOpen(filepath.Dir(absPath), filepath.Base(absPath)); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("open document: %v", err)), nil
	}
	diags, _ := provider.LastDiagnostics(absPath)
	actions, err := provider.GetCodeActions(lsp.CodeActionsRequest{
		AbsPath:     absPath,
		Range:       rng,
		Diagnostics: diags,
		Only:        only,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("code action request failed: %v", err)), nil
	}
	if idx >= len(actions) {
		return mcp.NewToolResultError(fmt.Sprintf("index %d out of range (have %d actions)", idx, len(actions))), nil
	}
	chosen := actions[idx]
	files, err := provider.ApplyCodeAction(chosen)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("apply failed: %v", err)), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"path":          path,
		"provider":      provider.Name(),
		"applied":       chosen.Title,
		"kind":          chosen.Kind,
		"files_touched": files,
	})
}

// handleFixAllInFile implements the `fix_all_in_file` tool.
func (s *Server) handleFixAllInFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	provider, absPath, err := s.lspProviderForPath(path)
	if err != nil {
		return s.lspNoProviderResult(path, err), nil
	}
	if err := provider.EnsureFileOpen(filepath.Dir(absPath), filepath.Base(absPath)); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("open document: %v", err)), nil
	}
	opts := lsp.FixAllOptions{
		AbsPath:           absPath,
		Kinds:             splitCSV(req.GetString("kinds", "")),
		MaxIterations:     req.GetInt("max_iterations", 5),
		MaxActionsPerIter: req.GetInt("max_actions_per_iter", 50),
		DiagnosticTimeout: time.Duration(req.GetInt("diagnostic_timeout_ms", 5000)) * time.Millisecond,
	}
	res, err := provider.FixAllInFile(opts)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fix-all failed: %v", err)), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"path":              path,
		"provider":          provider.Name(),
		"iterations":        res.Iterations,
		"applied_actions":   res.AppliedActions,
		"files_touched":     res.FilesTouched,
		"final_diagnostics": diagsToWire(res.FinalDiagnostics),
	})
}

// lspProviderForPath resolves the right *lsp.Provider for the given
// file path. The path may be repo-relative or absolute. Returns the
// *lsp.Provider, the absolute path it routed to, and an error when no
// provider applies.
//
// Resolution order: when SemanticManager().LSPRouter() returns a
// concrete *lsp.Router, it wins (lazy spawn + idle reaper + LRU
// eviction). Otherwise we fall back to scanning eagerly-registered
// Providers via Manager.AllProviders() — the legacy path that still
// covers user-defined daemons (`pc.Daemon` without a registry spec).
func (s *Server) lspProviderForPath(path string) (*lsp.Provider, string, error) {
	if s.semanticMgr == nil {
		return nil, "", errors.New("semantic manager not configured")
	}
	abs, err := s.absolutePath(path)
	if err != nil {
		return nil, "", err
	}
	spec := lsp.SpecForPath(abs)
	if spec == nil {
		return nil, abs, fmt.Errorf("no LSP server registered for %s", filepath.Ext(abs))
	}

	// Router-managed lifecycle wins when configured. The router
	// returns a *lsp.Provider directly; the LSP-action surface needs
	// the concrete type for LastDiagnostics / GetCodeActions / etc.
	if r := s.lspRouter(); r != nil {
		// Router only knows about specs the user enabled via config
		// — SpecAvailable filters to "enabled AND on PATH" without
		// spawning. Probe before ForSpec so an opt-out spec or a
		// missing binary returns a clean no_lsp_for error.
		if r.SpecAvailable(spec.Name) {
			// Pass the per-file workspace root so the cache key
			// is (spec, workspace) — separate subprocess per repo
			// in a multi-repo daemon, each rooted correctly. A
			// shared (spec, defaultWorkspace) reuse would corrupt
			// rootURI for files outside the first-spawned repo.
			root := s.workspaceRootFor(abs)
			lp, err := r.ForSpecWorkspace(spec, root)
			if err != nil {
				return nil, abs, fmt.Errorf("router spawn %s: %w", spec.Name, err)
			}
			return lp, abs, nil
		}
		// Fall through to legacy scan — Router is in charge but the
		// user may have an out-of-registry custom daemon registered
		// the old way.
	}

	for _, p := range s.semanticMgr.AllProviders() {
		lp, ok := p.(*lsp.Provider)
		if !ok {
			continue
		}
		if !providerCovers(lp, spec) {
			continue
		}
		if !lp.Available() {
			continue
		}
		// Lazy spawn — Provider.EnsureClient is idempotent.
		root := s.workspaceRootFor(abs)
		if err := lp.EnsureClient(root); err != nil {
			return nil, abs, fmt.Errorf("ensure client %s: %w", spec.Name, err)
		}
		return lp, abs, nil
	}
	return nil, abs, fmt.Errorf("no LSP provider registered for languages %v", spec.Languages)
}

// lspRouter returns the semantic manager's LSP router as a concrete
// *lsp.Router for use with the LSP-action MCP surface, or nil when no
// router is wired (legacy boot paths, tests). Centralised here so the
// type assertion lives in one place.
func (s *Server) lspRouter() *lsp.Router {
	if s.semanticMgr == nil {
		return nil
	}
	r, _ := s.semanticMgr.LSPRouter().(*lsp.Router)
	return r
}

// providerCovers reports whether the provider serves at least one
// language listed by the given spec.
func providerCovers(p *lsp.Provider, spec *lsp.ServerSpec) bool {
	pls := p.Languages()
	for _, l := range pls {
		for _, sl := range spec.Languages {
			if l == sl {
				return true
			}
		}
	}
	return false
}

// absolutePath resolves a (possibly repo-relative) path to an
// absolute filesystem path, using the indexer / multi-indexer roots
// when available.
func (s *Server) absolutePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	if s.multiIndexer != nil {
		if abs := s.multiIndexer.ResolveFilePath(path); abs != "" {
			return abs, nil
		}
	}
	if s.indexer != nil {
		root := s.indexer.RootPath()
		if root != "" {
			return filepath.Join(root, path), nil
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs, nil
	}
	return path, nil
}

// workspaceRootFor returns the workspace root the LSP server should
// be initialised with for the given file. Falls back to the file's
// directory when no indexer root is available.
func (s *Server) workspaceRootFor(absPath string) string {
	if s.indexer != nil {
		root := s.indexer.RootPath()
		if root != "" && strings.HasPrefix(absPath, root) {
			return root
		}
	}
	if s.multiIndexer != nil {
		if prefix := s.multiIndexer.RepoForFile(absPath); prefix != "" {
			if meta := s.multiIndexer.GetMetadata(prefix); meta != nil {
				return meta.RootPath
			}
		}
	}
	return filepath.Dir(absPath)
}

// lspNoProviderResult returns a structured error for the "no LSP
// available" common case so callers can branch on a stable
// `no_lsp_for` field instead of grepping the error string.
func (s *Server) lspNoProviderResult(path string, err error) *mcp.CallToolResult {
	res, _ := mcp.NewToolResultJSON(map[string]any{
		"path":       path,
		"no_lsp_for": filepath.Ext(path),
		"reason":     err.Error(),
		"error":      true,
	})
	return res
}

func rangeFromRequest(req mcp.CallToolRequest) lsp.Range {
	startLine := req.GetInt("start_line", 0)
	startChar := req.GetInt("start_char", 0)
	endLine := req.GetInt("end_line", startLine+1)
	endChar := req.GetInt("end_char", 0)
	return lsp.Range{
		Start: lsp.Position{Line: startLine, Character: startChar},
		End:   lsp.Position{Line: endLine, Character: endChar},
	}
}

func diagsToWire(diags []lsp.Diagnostic) []map[string]any {
	out := make([]map[string]any, 0, len(diags))
	for _, d := range diags {
		out = append(out, map[string]any{
			"severity":   d.Severity,
			"source":     d.Source,
			"message":    d.Message,
			"start_line": d.Range.Start.Line,
			"start_char": d.Range.Start.Character,
			"end_line":   d.Range.End.Line,
			"end_char":   d.Range.End.Character,
		})
	}
	return out
}

// AllProviders is exposed for the LSP-action service.
var _ = semantic.Manager{}
