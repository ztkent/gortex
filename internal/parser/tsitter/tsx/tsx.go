// Package tsx re-exports the TSX variant of tree-sitter-typescript
// for .tsx files.
package tsx

import (
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_typescript.LanguageTSX())
}
