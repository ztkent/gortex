// Package yaml re-exports tree-sitter-grammars/tree-sitter-yaml.
package yaml

import (
	tree_sitter_yaml "github.com/tree-sitter-grammars/tree-sitter-yaml/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_yaml.Language())
}
