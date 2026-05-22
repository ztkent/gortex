package mcp

import (
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
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
