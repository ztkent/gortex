package parser

import "github.com/zzet/gortex/internal/graph"

// Extractor extracts graph nodes and edges from a single source file.
type Extractor interface {
	Language() string
	Extensions() []string
	Extract(filePath string, src []byte) (*ExtractionResult, error)
}

// ExtractionResult holds the nodes and edges extracted from a single
// file, plus an optional handle to the parse tree the extractor used.
//
// When Tree is non-nil the indexer is responsible for releasing it
// after every per-file consumer (contract extractors, body-fact
// resolvers) has run. Languages whose extractor doesn't have a
// downstream consumer for the tree leave Tree as nil and close their
// own trees internally — the contract pipeline degrades to its regex
// fallback for those languages.
type ExtractionResult struct {
	Nodes []*graph.Node
	Edges []*graph.Edge
	Tree  *ParseTree
}
