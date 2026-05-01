// Package orgmode re-exports the tree-sitter-org-mode grammar. The C
// parser lives in the sibling github.com/gortexhq/tree-sitter-org-mode
// module (a vendored fork of zac-garby/tree-sitter-org-mode); this file
// is the thin shim that bridges the upstream binding into gortex's
// *tsitter.Language type.
package orgmode

import (
	tree_sitter_orgmode "github.com/gortexhq/tree-sitter-org-mode/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

// GetLanguage returns the compiled Org-mode language.
func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_orgmode.Language())
}
