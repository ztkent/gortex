// Package javascript re-exports the tree-sitter-javascript grammar.
package javascript

import (
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_javascript.Language())
}
