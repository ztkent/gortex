// Package scala re-exports the tree-sitter-scala grammar.
package scala

import (
	tree_sitter_scala "github.com/tree-sitter/tree-sitter-scala/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_scala.Language())
}
