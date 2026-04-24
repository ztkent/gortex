// Package css re-exports the tree-sitter-css grammar.
package css

import (
	tree_sitter_css "github.com/tree-sitter/tree-sitter-css/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_css.Language())
}
