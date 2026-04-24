// Package html re-exports the tree-sitter-html grammar.
package html

import (
	tree_sitter_html "github.com/tree-sitter/tree-sitter-html/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_html.Language())
}
