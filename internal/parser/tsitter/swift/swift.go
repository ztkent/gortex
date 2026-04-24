// Package swift vendors alex-pinkus/tree-sitter-swift. Upstream
// gitignores the generated parser.c, so the Go module zip ships
// without it and the repo's own bindings/go fails to compile. We
// regenerated parser.c via tree-sitter-cli and vendor it here.
package swift

// #cgo CFLAGS: -I${SRCDIR}/src -std=c11 -fPIC -Wno-macro-redefined
// #include "src/parser.c"
// #include "src/scanner.c"
import "C"
import (
	"unsafe"

	"github.com/zzet/gortex/internal/parser/tsitter"
)

// GetLanguage returns the compiled Swift language.
func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(unsafe.Pointer(C.tree_sitter_swift()))
}
