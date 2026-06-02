package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// searchCorpus selects which slice of the index search_symbols draws
// from: code symbols, documentation prose, or both.
type searchCorpus int

const (
	// corpusCode is the default -- only code symbols (functions,
	// types, ...), prose-section KindDoc nodes excluded.
	corpusCode searchCorpus = iota
	// corpusDocs returns only KindDoc prose-section nodes.
	corpusDocs
	// corpusAll returns code symbols and prose sections together.
	corpusAll
)

// parseCorpus reads the `corpus` argument. "" / "code" -> corpusCode,
// "docs" / "doc" / "prose" -> corpusDocs, "all" / "both" -> corpusAll.
// An unrecognised value is an error so a typo surfaces clearly rather
// than silently returning the wrong corpus.
func parseCorpus(req mcpgo.CallToolRequest) (searchCorpus, error) {
	switch strings.ToLower(strings.TrimSpace(req.GetString("corpus", ""))) {
	case "", "code":
		return corpusCode, nil
	case "docs", "doc", "prose":
		return corpusDocs, nil
	case "all", "both":
		return corpusAll, nil
	default:
		return corpusCode, fmt.Errorf("invalid corpus: %q (want code, docs, or all)",
			req.GetString("corpus", ""))
	}
}

// includesDocs reports whether the corpus admits prose-section nodes.
func (c searchCorpus) includesDocs() bool { return c == corpusDocs || c == corpusAll }

// includesCode reports whether the corpus admits code-symbol nodes.
func (c searchCorpus) includesCode() bool { return c == corpusCode || c == corpusAll }

// docChannelFetchMultiple widens the limit of the parallel doc-biased
// fetch relative to the primary fetch. Prose sections are a minority
// of the corpus, so a doc that the query genuinely matches can sit
// well past the primary fetchLimit behind code symbols that share its
// tokens. Over-fetching here, then keeping only the KindDoc hits, lets
// those prose sections enter the candidate pool. Bounded so the extra
// fetch stays cheap on a large index.
const docChannelFetchMultiple = 4

// docChannelMaxLimit caps the absolute size of the doc-channel fetch
// so a large `limit` request can't blow the over-fetch up unboundedly.
const docChannelMaxLimit = 200

// mergeDocChannel runs a parallel, wider-limit fetch biased to surface
// prose-section (KindDoc) nodes and merges the new doc hits into the
// existing candidate slice. It is the real retrieval channel behind
// corpus:"docs" / "all": the primary fetch is code-shaped and a
// relevant doc can rank just past its limit, so the corpus post-filter
// would have nothing to keep. By over-fetching and admitting only the
// KindDoc hits not already present, prose competes on its own terms
// before the corpus filter and the rerank run.
//
// Existing candidates keep their position; new doc hits append in
// their fetch order (the rerank pass settles final ranking). Dedup is
// by node ID. When the wider fetch surfaces no new docs the input is
// returned unchanged.
func (s *Server) mergeDocChannel(ctx context.Context, query string, nodes []*graph.Node, fetchLimit int, scope query.QueryOptions, timings *query.SearchTimings) []*graph.Node {
	if strings.TrimSpace(query) == "" {
		return nodes
	}
	docLimit := fetchLimit * docChannelFetchMultiple
	if docLimit > docChannelMaxLimit {
		docLimit = docChannelMaxLimit
	}
	if docLimit <= fetchLimit {
		docLimit = fetchLimit
	}

	start := time.Now()
	wide := s.engineFor(ctx).SearchSymbolsScoped(query, docLimit, scope)
	if timings != nil {
		timings.BM25ExpansionMS += time.Since(start).Milliseconds()
	}
	if len(wide) == 0 {
		return nodes
	}

	seen := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if n != nil {
			seen[n.ID] = struct{}{}
		}
	}
	merged := nodes
	for _, n := range wide {
		if n == nil || n.Kind != graph.KindDoc {
			continue
		}
		if _, dup := seen[n.ID]; dup {
			continue
		}
		seen[n.ID] = struct{}{}
		merged = append(merged, n)
	}
	return merged
}

// filterNodesByCorpus drops nodes that fall outside the selected
// corpus. KindDoc nodes are the "docs" corpus; every other kind is
// "code". corpusAll is a no-op.
func filterNodesByCorpus(nodes []*graph.Node, c searchCorpus) []*graph.Node {
	if c == corpusAll {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		isDoc := n.Kind == graph.KindDoc
		if isDoc && c.includesDocs() {
			out = append(out, n)
		} else if !isDoc && c.includesCode() {
			out = append(out, n)
		}
	}
	return out
}
