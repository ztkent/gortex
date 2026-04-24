// Package python re-exports the tree-sitter-python grammar.
package python

import (
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_python.Language())
}
