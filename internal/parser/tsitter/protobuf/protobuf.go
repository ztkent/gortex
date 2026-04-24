// Package protobuf vendors the tree-sitter-proto grammar
// (mitchellh/tree-sitter-proto, main branch). The repo ships only the
// generated parser.c with no Go bindings, so we compile it directly
// through CGO. There's no external scanner for this grammar.
package protobuf

// #cgo CFLAGS: -I${SRCDIR}/src -std=c11 -fPIC
// #include "src/parser.c"
import "C"
import (
	"unsafe"

	"github.com/zzet/gortex/internal/parser/tsitter"
)

// GetLanguage returns the compiled Protobuf language.
func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(unsafe.Pointer(C.tree_sitter_proto()))
}
