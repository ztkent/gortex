package languages

import (
	"github.com/alexaandru/go-sitter-forest/elm"

	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/forest"
)

// NewElmExtractor returns a forest-backed signature-only extractor
// for Elm. Forest ships a tags.scm for elm so we get function /
// type / module captures without writing a query. Brand-new
// language — replaces nothing.
func NewElmExtractor() parser.Extractor {
	return forest.New("elm", []string{".elm"}, elm.GetLanguage, elm.GetQuery)
}
