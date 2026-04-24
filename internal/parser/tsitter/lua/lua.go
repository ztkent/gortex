// Package lua re-exports tree-sitter-grammars/tree-sitter-lua.
package lua

import (
	tree_sitter_lua "github.com/tree-sitter-grammars/tree-sitter-lua/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_lua.Language())
}
