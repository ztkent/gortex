// Package c re-exports the tree-sitter-c grammar.
package c

import (
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_c.Language())
}
