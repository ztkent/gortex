// Package ocaml re-exports the tree-sitter-ocaml grammar (OCaml
// implementation variant, .ml files). Interface (.mli) and type
// bindings exist in sister packages but gortex only indexes .ml.
package ocaml

import (
	tree_sitter_ocaml "github.com/tree-sitter/tree-sitter-ocaml/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_ocaml.LanguageOCaml())
}
