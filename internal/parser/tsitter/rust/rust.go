// Package rust re-exports the tree-sitter-rust grammar.
package rust

import (
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_rust.Language())
}
