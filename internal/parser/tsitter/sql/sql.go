// Package sql vendors DerekStride/tree-sitter-sql. Upstream
// gitignores parser.c (regenerated via tree-sitter-cli at install
// time), so we generated it once and vendor it here so Go users can
// import without needing tree-sitter-cli locally.
package sql

// #cgo CFLAGS: -I${SRCDIR}/src -std=c11 -fPIC
// #include "src/parser.c"
// #include "src/scanner.c"
import "C"
import (
	"unsafe"

	"github.com/zzet/gortex/internal/parser/tsitter"
)

// GetLanguage returns the compiled SQL language.
func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(unsafe.Pointer(C.tree_sitter_sql()))
}
