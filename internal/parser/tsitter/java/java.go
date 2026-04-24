// Package java re-exports the tree-sitter-java grammar.
package java

import (
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_java.Language())
}
