// Package typescript re-exports the tree-sitter-typescript grammar.
// Smacker's path was `typescript/typescript`; the flat `typescript`
// package here is the shorter equivalent.
package typescript

import (
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_typescript.LanguageTypescript())
}
