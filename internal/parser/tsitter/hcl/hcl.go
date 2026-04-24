// Package hcl re-exports tree-sitter-grammars/tree-sitter-hcl.
package hcl

import (
	tree_sitter_hcl "github.com/tree-sitter-grammars/tree-sitter-hcl/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_hcl.Language())
}
