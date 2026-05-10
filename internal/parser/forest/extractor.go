// Package forest is the long-tail language adapter. It wraps any
// alexaandru/go-sitter-forest grammar (510+ supported) as a Gortex
// Extractor with signature-only depth: function / method / type /
// interface / variable / constant nodes plus EdgeDefines from the
// file. Calls and imports are best-effort via @reference.call captures
// when the grammar ships a tags.scm.
//
// Top-tier languages (Go, TS, Python, Rust, …) keep their bespoke
// extractors in internal/parser/languages — those have hand-tuned
// queries that emit Gortex-specific edges (ORM, contracts, dataflow)
// the generic walker can't produce. forest is for everything else.
package forest

import (
	"sync"
	"unsafe"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// GetLanguageFn returns the raw C *TSLanguage pointer. Every forest
// language module exposes one as `forestpkg.GetLanguage`.
type GetLanguageFn func() unsafe.Pointer

// GetQueryFn returns the bytes of a named .scm query bundled with the
// grammar. Forest grammars ship a subset of {tags, highlights, locals,
// folds, indents, injections}; we only read tags.scm. The opts
// argument is forest's preference flag (NvimFirst / NativeFirst /
// NvimOnly / NativeOnly). nil is acceptable when the grammar has no
// queries.
type GetQueryFn func(kind string, opts ...byte) []byte

// Extractor is a generic forest-backed signature-only extractor.
// Construct one per language via New() and register it with the
// parser registry exactly like any other Extractor.
type Extractor struct {
	language   string
	extensions []string

	getLang  GetLanguageFn
	getQuery GetQueryFn

	once    sync.Once
	initErr error
	lang    *sitter.Language
	tagsQ   *parser.PreparedQuery // nil when grammar ships no tags.scm
}

// New builds a forest-backed Extractor. getQuery may be nil for
// grammars that do not expose any .scm queries — extraction then
// falls back to the generic node-kind walker.
func New(language string, extensions []string, getLang GetLanguageFn, getQuery GetQueryFn) *Extractor {
	return &Extractor{
		language:   language,
		extensions: extensions,
		getLang:    getLang,
		getQuery:   getQuery,
	}
}

func (e *Extractor) Language() string     { return e.language }
func (e *Extractor) Extensions() []string { return e.extensions }

// init compiles the language pointer + tags.scm query exactly once.
// Errors are sticky: once an init has failed, every subsequent
// Extract returns the same error rather than retrying CGO setup on
// every file (which would slow indexing to a crawl on a broken
// grammar).
func (e *Extractor) init() error {
	e.once.Do(func() {
		ptr := e.getLang()
		if ptr == nil {
			e.initErr = errNilLanguage
			return
		}
		e.lang = sitter.NewLanguage(ptr)

		if e.getQuery != nil {
			pattern := e.getQuery("tags")
			if len(pattern) > 0 {
				q, err := parser.NewPreparedQuery(string(pattern), e.lang)
				if err == nil {
					e.tagsQ = q
				}
				// Compile failure is non-fatal: drop to walker.
			}
		}
	})
	return e.initErr
}

// Extract parses src with the bundled grammar and emits one file
// node, plus one node per detected definition (function / method /
// type / interface / variable / constant / field / module). Edges:
// EdgeDefines from the file to every definition; EdgeCalls
// (unresolved::name) for @reference.call captures when tags.scm is
// present.
func (e *Extractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	if err := e.init(); err != nil {
		return nil, err
	}

	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID:       filePath,
		Kind:     graph.KindFile,
		Name:     filePath,
		FilePath: filePath,
		StartLine: 1,
		EndLine:   int(root.EndPoint().Row) + 1,
		Language: e.language,
	}
	result.Nodes = append(result.Nodes, fileNode)

	if e.tagsQ != nil {
		e.extractByTags(root, src, filePath, fileNode, result)
		return result, nil
	}

	e.extractByWalker(root, src, filePath, fileNode, result)
	return result, nil
}

var _ parser.Extractor = (*Extractor)(nil)
