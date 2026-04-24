// Package ruby re-exports the tree-sitter-ruby grammar.
package ruby

import (
	tree_sitter_ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_ruby.Language())
}
