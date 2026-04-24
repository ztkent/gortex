// Package dockerfile vendors camdencheek/tree-sitter-dockerfile. The
// upstream go.mod declares a module path that clashes with the parent
// repo, so Go's module system can't import it as a submodule; we
// compile the C grammar directly through CGO instead.
package dockerfile

// #cgo CFLAGS: -I${SRCDIR}/src -std=c11 -fPIC
// #include "src/parser.c"
// #include "src/scanner.c"
import "C"
import (
	"unsafe"

	"github.com/zzet/gortex/internal/parser/tsitter"
)

// GetLanguage returns the compiled Dockerfile language.
func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(unsafe.Pointer(C.tree_sitter_dockerfile()))
}
