// Package cpp re-exports the tree-sitter-cpp grammar.
package cpp

import (
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_cpp.Language())
}
