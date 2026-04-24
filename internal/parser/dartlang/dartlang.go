// Package dartlang provides Go bindings for the tree-sitter-dart
// grammar. It vendors the C parser from
// github.com/UserNobody14/tree-sitter-dart (ABI v14) and compiles it
// directly through CGO. The exposed GetLanguage() returns a
// tsitter.Language pointer usable with the gortex shim.
package dartlang

//#cgo CFLAGS: -std=c11 -fPIC
//#include "tree_sitter/parser.h"
//TSLanguage *tree_sitter_dart(void);
import "C"
import (
	"unsafe"

	"github.com/zzet/gortex/internal/parser/tsitter"
)

func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(unsafe.Pointer(C.tree_sitter_dart()))
}
