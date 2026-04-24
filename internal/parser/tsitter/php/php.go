// Package php re-exports the tree-sitter-php grammar (full-mode PHP
// with HTML framing; matches smacker's default php binding).
package php

import (
	tree_sitter_php "github.com/tree-sitter/tree-sitter-php/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_php.LanguagePHP())
}
