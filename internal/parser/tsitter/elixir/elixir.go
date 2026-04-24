// Package elixir re-exports tree-sitter-elixir.
// The upstream repo declares its module path as
// github.com/tree-sitter/tree-sitter-elixir (it was donated from
// elixir-lang); go.mod pins a replace directive to the elixir-lang
// origin so this import resolves.
package elixir

import (
	tree_sitter_elixir "github.com/tree-sitter/tree-sitter-elixir/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_elixir.Language())
}
