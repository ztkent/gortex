// Package kotlin re-exports fwcd/tree-sitter-kotlin. The
// tree-sitter-grammars/tree-sitter-kotlin v1.x grammar renamed many
// node kinds (e.g. dropped the `type_identifier` alias), which would
// force a substantial rewrite of our Kotlin extractor. fwcd's grammar
// matches the node vocabulary smacker shipped, so we keep that.
package kotlin

import (
	tree_sitter_kotlin "github.com/fwcd/tree-sitter-kotlin/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_kotlin.Language())
}
