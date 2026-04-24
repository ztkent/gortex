// Package golang re-exports the tree-sitter-go grammar.
// The package name mirrors smacker's `golang` so existing extractor
// imports only need a path change.
package golang

import (
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_go.Language())
}
