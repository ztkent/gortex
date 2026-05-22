package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/search/trigram"
)

// enrichedTextMatch is a trigram literal-search hit decorated with the
// graph symbol that encloses the matching line. symbol_id /
// symbol_name are empty for a match in a file-level region with no
// enclosing function / method / type.
type enrichedTextMatch struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Text       string `json:"text"`
	SymbolID   string `json:"symbol_id,omitempty"`
	SymbolName string `json:"symbol_name,omitempty"`
}

// handleSearchText runs a trigram-accelerated literal code search
// across the indexed repository -- the alt grep backbone. A trigram
// index narrows the file set, then each candidate is scanned to
// confirm the match, so a repo-wide substring search costs roughly
// the size of the matching files rather than the whole tree.
//
// Each hit is enriched with the enclosing graph symbol so an agent
// can see *which function / method* a literal match landed in
// without a follow-up get_symbol call.
func (s *Server) handleSearchText(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("search_text: query is required"), nil
	}
	if s.indexer == nil {
		return mcp.NewToolResultError("search_text: no indexer available"), nil
	}

	limit := req.GetInt("limit", 100)
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	matches := s.indexer.GrepText(query, limit)
	enriched := s.enrichTextMatches(matches)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"query":   query,
		"matches": enriched,
		"count":   len(enriched),
	})
}

// enrichTextMatches decorates every trigram match with its enclosing
// graph symbol. It builds one per-file symbol index for the set of
// matched files, then resolves each match's line through it.
func (s *Server) enrichTextMatches(matches []trigram.Match) []enrichedTextMatch {
	out := make([]enrichedTextMatch, 0, len(matches))
	paths := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		paths[m.Path] = struct{}{}
	}
	idx := s.buildFileSymbolIndexForPaths(paths)
	for _, m := range matches {
		em := enrichedTextMatch{Path: m.Path, Line: m.Line, Text: m.Text}
		if fi := idx[m.Path]; fi != nil {
			em.SymbolID, em.SymbolName = fi.find(m.Line)
		}
		out = append(out, em)
	}
	return out
}
