package parser

import (
	"context"
	"fmt"
	"sync"
	"time"

	ts "github.com/tree-sitter/go-tree-sitter"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

const parseTimeout = 5 * time.Second

// NB: a sync.Pool of *Parser was attempted under the smacker bindings
// and tripped cross-call state bugs. The official bindings expose a
// clean Reset(), but until we audit whether grammar switches behave
// correctly, stay on fresh-per-call parsers — cold indexing throughput
// was never parser-alloc-bound after we precompiled queries.

// CapturedNode holds information about a single captured tree-sitter node.
type CapturedNode struct {
	Text      string
	StartLine int // 0-based (tree-sitter native)
	EndLine   int // 0-based
	StartCol  int
	EndCol    int
	Node      *sitter.Node
}

// QueryResult represents a single match from a tree-sitter query.
type QueryResult struct {
	Captures map[string]*CapturedNode
}

// ParseFile parses source bytes with the given language and returns the tree.
// The caller must call tree.Close() when done.
func ParseFile(src []byte, lang *sitter.Language) (*sitter.Tree, error) {
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(lang)

	ctx, cancel := context.WithTimeout(context.Background(), parseTimeout)
	defer cancel()

	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil {
		return nil, fmt.Errorf("tree-sitter parse: %w", err)
	}
	return tree, nil
}

// PreparedQuery is a compiled tree-sitter query safe to reuse across
// many Parse calls. Compile once at extractor init and hang on to it —
// queries are thread-safe for read-only use and avoid the per-call
// CGO compile that dominated large-repo indexing.
type PreparedQuery struct {
	q *sitter.Query
}

// NewPreparedQuery compiles a tree-sitter query pattern for the given
// language. The returned *PreparedQuery is safe for concurrent use by
// many goroutines running queries via a pooled QueryCursor.
func NewPreparedQuery(pattern string, lang *sitter.Language) (*PreparedQuery, error) {
	q, err := sitter.NewQuery([]byte(pattern), lang)
	if err != nil {
		return nil, fmt.Errorf("tree-sitter query compile: %w", err)
	}
	return &PreparedQuery{q: q}, nil
}

// MustPreparedQuery is NewPreparedQuery that panics on compile error.
// Use for extractor-internal queries that are compile-time constants:
// an error is a bug in the extractor, not runtime data, so crashing
// loud at init is the right behavior.
func MustPreparedQuery(pattern string, lang *sitter.Language) *PreparedQuery {
	q, err := NewPreparedQuery(pattern, lang)
	if err != nil {
		panic(err)
	}
	return q
}

// Close releases the underlying query. After Close the PreparedQuery
// must not be used.
func (pq *PreparedQuery) Close() {
	if pq != nil && pq.q != nil {
		pq.q.Close()
		pq.q = nil
	}
}

// cursorPool reuses *ts.QueryCursor across query runs. The new
// QueryCursor is stateless across Matches() calls — each call starts
// fresh iteration — so pooling is safe.
var cursorPool = sync.Pool{
	New: func() any { return ts.NewQueryCursor() },
}

func getCursor() *ts.QueryCursor { return cursorPool.Get().(*ts.QueryCursor) }
func putCursor(c *ts.QueryCursor) { cursorPool.Put(c) }

// RunQuery executes a tree-sitter S-expression query against a node and
// returns all matches with their captures. The query is compiled on
// every call — use RunPrepared with a precompiled query in hot paths.
func RunQuery(pattern string, lang *sitter.Language, node *sitter.Node, src []byte) ([]QueryResult, error) {
	q, err := sitter.NewQuery([]byte(pattern), lang)
	if err != nil {
		return nil, fmt.Errorf("tree-sitter query compile: %w", err)
	}
	defer q.Close()
	return runQuery(q, node, src), nil
}

// RunPrepared executes a precompiled query against a node and returns
// all matches with their captures.
func RunPrepared(pq *PreparedQuery, node *sitter.Node, src []byte) []QueryResult {
	if pq == nil || pq.q == nil {
		return nil
	}
	return runQuery(pq.q, node, src)
}

// runQuery is the hot iterator: it drives the cursor, copies captures
// out of the cursor's reusable buffer before calling Next() again, and
// assembles QueryResult values the extractors expect.
func runQuery(q *sitter.Query, node *sitter.Node, src []byte) []QueryResult {
	if node == nil || node.Inner() == nil {
		return nil
	}
	cursor := getCursor()
	defer putCursor(cursor)

	iter := cursor.Matches(q.Inner(), node.Inner(), src)
	var results []QueryResult
	for {
		match := iter.Next()
		if match == nil {
			break
		}
		if len(match.Captures) == 0 {
			continue
		}
		qr := QueryResult{Captures: make(map[string]*CapturedNode, len(match.Captures))}
		for _, c := range match.Captures {
			// c.Node is a value; copying it detaches from the cursor's
			// per-match buffer so the pointer stays valid after Next().
			nodeCopy := c.Node
			name := q.CaptureNameForId(c.Index)
			sp := nodeCopy.StartPosition()
			ep := nodeCopy.EndPosition()
			qr.Captures[name] = &CapturedNode{
				Text:      nodeCopy.Utf8Text(src),
				StartLine: int(sp.Row),
				EndLine:   int(ep.Row),
				StartCol:  int(sp.Column),
				EndCol:    int(ep.Column),
				Node:      sitter.WrapNode(nodeCopy),
			}
		}
		results = append(results, qr)
	}
	return results
}

// EachMatch runs a prepared query and invokes fn for each match. The
// captures map passed to fn is freshly allocated per match (safe to
// retain), but on hot paths the caller should avoid retaining it and
// copy out only the fields it needs. EachMatch avoids the []QueryResult
// slice allocation that RunPrepared incurs — relevant when a query
// fires thousands of times on a large file.
func EachMatch(pq *PreparedQuery, node *sitter.Node, src []byte, fn func(QueryResult)) {
	if pq == nil || pq.q == nil {
		return
	}
	if node == nil || node.Inner() == nil {
		return
	}
	cursor := getCursor()
	defer putCursor(cursor)

	iter := cursor.Matches(pq.q.Inner(), node.Inner(), src)
	for {
		match := iter.Next()
		if match == nil {
			break
		}
		if len(match.Captures) == 0 {
			continue
		}
		qr := QueryResult{Captures: make(map[string]*CapturedNode, len(match.Captures))}
		for _, c := range match.Captures {
			nodeCopy := c.Node
			name := pq.q.CaptureNameForId(c.Index)
			sp := nodeCopy.StartPosition()
			ep := nodeCopy.EndPosition()
			qr.Captures[name] = &CapturedNode{
				Text:      nodeCopy.Utf8Text(src),
				StartLine: int(sp.Row),
				EndLine:   int(ep.Row),
				StartCol:  int(sp.Column),
				EndCol:    int(ep.Column),
				Node:      sitter.WrapNode(nodeCopy),
			}
		}
		fn(qr)
	}
}

// NodeText extracts the text content of a tree-sitter node from source bytes.
func NodeText(node *sitter.Node, src []byte) string {
	return node.Content(src)
}
