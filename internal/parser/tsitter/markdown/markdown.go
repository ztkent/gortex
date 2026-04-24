// Package markdown vendors the tree-sitter-markdown block-level
// grammar (tree-sitter-grammars/tree-sitter-markdown, split_parser
// branch). No upstream Go module ships this grammar, so the C parser
// is compiled directly through CGO like dartlang.
package markdown

// #cgo CFLAGS: -I${SRCDIR}/src -std=c11 -fPIC
// #include "src/parser.c"
// #include "src/scanner.c"
import "C"
import (
	"unsafe"

	"github.com/zzet/gortex/internal/parser/tsitter"
)

// GetLanguage returns the compiled Markdown language.
func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(unsafe.Pointer(C.tree_sitter_markdown()))
}
