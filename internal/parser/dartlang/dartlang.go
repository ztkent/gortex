// Package dartlang provides Go bindings for the tree-sitter-dart grammar.
// It vendors the C parser from github.com/UserNobody14/tree-sitter-dart (ABI v14).
//
// The parser.c and scanner.c files are compiled directly by CGO.
package dartlang

//#cgo CFLAGS: -std=c11 -fPIC
//#include "tree_sitter/parser.h"
//TSLanguage *tree_sitter_dart(void);
import "C"
import (
	"unsafe"

	sitter "github.com/smacker/go-tree-sitter"
)

func GetLanguage() *sitter.Language {
	return sitter.NewLanguage(unsafe.Pointer(C.tree_sitter_dart()))
}
