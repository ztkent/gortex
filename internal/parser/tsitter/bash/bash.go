// Package bash re-exports the tree-sitter-bash grammar in a
// smacker-compatible shape (GetLanguage() *tsitter.Language).
package bash

import (
	tree_sitter_bash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_bash.Language())
}
