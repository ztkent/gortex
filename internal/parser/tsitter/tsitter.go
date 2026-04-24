// Package tsitter is a thin compatibility shim over
// github.com/tree-sitter/go-tree-sitter. Its surface intentionally
// mirrors the smacker/go-tree-sitter API so that the ~90 language
// extractors in gortex can be migrated by changing only import paths —
// method names and signatures stay the same.
//
// The shim wraps native tree-sitter types (Node, Tree, Parser, Query)
// and adapts:
//   - Type() → Kind()
//   - Content(src) → Utf8Text(src)
//   - StartPoint/EndPoint → StartPosition/EndPosition (uint32 Row/Column)
//   - int-indexed children → uint-indexed (internally converted)
//   - ParseCtx(ctx, old, src) built on top of ParseWithOptions
//
// The cursor-based query iteration is not re-exposed here: it lives in
// the parent parser package, which uses the official API directly.
package tsitter

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// Language is a parser language. Exposed as an alias so grammar
// sub-packages can return *ts.Language directly.
type Language = ts.Language

// NewLanguage constructs a Language from a grammar's raw C pointer —
// used by the per-language shim sub-packages.
func NewLanguage(ptr unsafe.Pointer) *Language { return ts.NewLanguage(ptr) }

// Point mirrors the smacker Point layout (uint32 row/column).
type Point struct {
	Row    uint32
	Column uint32
}

func fromTSPoint(p ts.Point) Point {
	return Point{Row: uint32(p.Row), Column: uint32(p.Column)}
}

// Node wraps *ts.Node with smacker-compatible method names. Nodes are
// valid for the lifetime of their Tree; copying by value is cheap
// (single C struct field).
type Node struct {
	inner ts.Node
	// set true when constructed; distinguishes the zero value from a real node.
	valid bool
}

// WrapNode wraps a value Node from the new API into our shim.
func WrapNode(n ts.Node) *Node { return &Node{inner: n, valid: true} }

// wrapPtr wraps a nullable *ts.Node, returning nil for nil input.
func wrapPtr(n *ts.Node) *Node {
	if n == nil {
		return nil
	}
	return &Node{inner: *n, valid: true}
}

// Inner returns a pointer to the underlying ts.Node. Internal use by
// the parser package's query runners.
func (n *Node) Inner() *ts.Node {
	if n == nil || !n.valid {
		return nil
	}
	return &n.inner
}

// Type returns the node kind string ("identifier", "function_declaration", …).
func (n *Node) Type() string { return n.inner.Kind() }

// Content returns the UTF-8 text of the node as a slice of src.
func (n *Node) Content(src []byte) string { return n.inner.Utf8Text(src) }

// StartPoint returns the (row, column) position of the node start.
func (n *Node) StartPoint() Point { return fromTSPoint(n.inner.StartPosition()) }

// EndPoint returns the (row, column) position one past the node end.
func (n *Node) EndPoint() Point { return fromTSPoint(n.inner.EndPosition()) }

// StartByte returns the byte offset of the node start.
func (n *Node) StartByte() uint32 { return uint32(n.inner.StartByte()) }

// EndByte returns the byte offset one past the node end.
func (n *Node) EndByte() uint32 { return uint32(n.inner.EndByte()) }

// ChildCount returns the number of children (named + anonymous).
func (n *Node) ChildCount() uint32 { return uint32(n.inner.ChildCount()) }

// NamedChildCount returns the number of named children.
func (n *Node) NamedChildCount() uint32 { return uint32(n.inner.NamedChildCount()) }

// Child returns the i-th child (named or anonymous) or nil.
func (n *Node) Child(i int) *Node {
	if i < 0 {
		return nil
	}
	return wrapPtr(n.inner.Child(uint(i)))
}

// NamedChild returns the i-th named child or nil.
func (n *Node) NamedChild(i int) *Node {
	if i < 0 {
		return nil
	}
	return wrapPtr(n.inner.NamedChild(uint(i)))
}

// ChildByFieldName returns the first child with the given field name or nil.
func (n *Node) ChildByFieldName(name string) *Node {
	return wrapPtr(n.inner.ChildByFieldName(name))
}

// FieldNameForChild returns the field name of the i-th child, or "" if none.
func (n *Node) FieldNameForChild(i int) string {
	if i < 0 {
		return ""
	}
	return n.inner.FieldNameForChild(uint32(i))
}

// Parent returns the parent node or nil for the root.
func (n *Node) Parent() *Node { return wrapPtr(n.inner.Parent()) }

// NextSibling returns the next sibling (named or anonymous) or nil.
func (n *Node) NextSibling() *Node { return wrapPtr(n.inner.NextSibling()) }

// PrevSibling returns the previous sibling (named or anonymous) or nil.
func (n *Node) PrevSibling() *Node { return wrapPtr(n.inner.PrevSibling()) }

// NextNamedSibling returns the next named sibling or nil.
func (n *Node) NextNamedSibling() *Node { return wrapPtr(n.inner.NextNamedSibling()) }

// PrevNamedSibling returns the previous named sibling or nil.
func (n *Node) PrevNamedSibling() *Node { return wrapPtr(n.inner.PrevNamedSibling()) }

// IsNamed reports whether the node corresponds to a named grammar rule.
func (n *Node) IsNamed() bool { return n.inner.IsNamed() }

// IsMissing reports whether the parser inserted this node to recover from an error.
func (n *Node) IsMissing() bool { return n.inner.IsMissing() }

// IsError reports whether this is a synthetic ERROR node.
func (n *Node) IsError() bool { return n.inner.IsError() }

// HasError reports whether the subtree under this node contains any ERROR nodes.
func (n *Node) HasError() bool { return n.inner.HasError() }

// String returns the s-expression representation of the node.
func (n *Node) String() string { return n.inner.ToSexp() }

// Id returns a stable numeric identity for the underlying node. Safe
// to use as a map key; equal across multiple wrappers of the same
// tree-sitter node. (Required because our shim creates a fresh *Node
// on every traversal, so pointer identity is not meaningful.)
func (n *Node) Id() uintptr {
	if n == nil {
		return 0
	}
	return n.inner.Id()
}

// Equal reports whether two shim Nodes wrap the same underlying
// tree-sitter node. Prefer this to `==` pointer comparison — our
// wrappers are freshly allocated on every navigation.
func (n *Node) Equal(other *Node) bool {
	if n == nil || other == nil {
		return n == other
	}
	return n.inner.Equals(other.inner)
}

// Tree wraps *ts.Tree.
type Tree struct {
	inner *ts.Tree
}

// WrapTree wraps a *ts.Tree for internal use by the parser package.
func WrapTree(t *ts.Tree) *Tree { return &Tree{inner: t} }

// Inner exposes the underlying *ts.Tree for internal use.
func (t *Tree) Inner() *ts.Tree { return t.inner }

// RootNode returns the root node of the parse tree.
func (t *Tree) RootNode() *Node { return wrapPtr(t.inner.RootNode()) }

// Close releases the tree's C resources.
func (t *Tree) Close() {
	if t != nil && t.inner != nil {
		t.inner.Close()
		t.inner = nil
	}
}

// Parser wraps *ts.Parser with a ParseCtx that honours ctx cancellation
// via the new API's progress callback hook.
type Parser struct {
	inner *ts.Parser
}

// NewParser allocates a fresh parser. The caller must Close it.
func NewParser() *Parser { return &Parser{inner: ts.NewParser()} }

// Close releases the parser's C resources.
func (p *Parser) Close() {
	if p != nil && p.inner != nil {
		p.inner.Close()
		p.inner = nil
	}
}

// SetLanguage binds a grammar to the parser. Errors from the new API
// (incompatible ABI versions) are swallowed to keep the smacker-style
// void return; callers trust build-time grammar selection.
func (p *Parser) SetLanguage(lang *Language) { _ = p.inner.SetLanguage(lang) }

// ParseCtx parses src under ctx's deadline, returning a *Tree the
// caller must Close. Cancellation is polled via a ProgressCallback;
// exact-to-the-byte interruption isn't guaranteed — tree-sitter calls
// the callback at its own cadence.
func (p *Parser) ParseCtx(ctx context.Context, old *Tree, src []byte) (*Tree, error) {
	var oldTree *ts.Tree
	if old != nil {
		oldTree = old.inner
	}
	cancelled := false
	opts := &ts.ParseOptions{
		ProgressCallback: func(_ ts.ParseState) bool {
			if ctx.Err() != nil {
				cancelled = true
				return true // true aborts the parse
			}
			return false
		},
	}
	tree := p.inner.ParseWithOptions(func(offset int, _ ts.Point) []byte {
		if offset >= len(src) {
			return nil
		}
		return src[offset:]
	}, oldTree, opts)
	if tree == nil {
		if cancelled {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("tree-sitter: parse cancelled")
		}
		return nil, fmt.Errorf("tree-sitter: parse returned nil")
	}
	return &Tree{inner: tree}, nil
}

// Query is a compiled tree-sitter query. It caches CaptureNames so
// capture-id → name lookups are O(1) and don't cross into CGO.
type Query struct {
	inner *ts.Query
	names []string
}

// NewQuery compiles a query pattern against a language. Signature
// matches smacker's (pattern, lang) order, which is the argument order
// most of our language adapters expect.
func NewQuery(pattern []byte, lang *Language) (*Query, error) {
	q, qerr := ts.NewQuery(lang, string(pattern))
	if qerr != nil {
		return nil, errors.New(qerr.Error())
	}
	return &Query{inner: q, names: q.CaptureNames()}, nil
}

// Inner exposes the underlying *ts.Query for internal query runners.
func (q *Query) Inner() *ts.Query { return q.inner }

// Close releases the query's C resources.
func (q *Query) Close() {
	if q != nil && q.inner != nil {
		q.inner.Close()
		q.inner = nil
	}
}

// CaptureNameForId returns the capture name for a capture index.
func (q *Query) CaptureNameForId(id uint32) string {
	if int(id) >= len(q.names) {
		return ""
	}
	return q.names[id]
}
