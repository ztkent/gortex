package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/parser"
)

// TestForestRegistrations_Resolves verifies that every forest-backed
// language declared in forestLanguages is reachable through the
// registry by both language name and (at least one of) its declared
// extensions. Catches typos like a missing extension dot or a
// duplicate registration that silently shadows a previous extractor.
func TestForestRegistrations_Resolves(t *testing.T) {
	reg := parser.NewRegistry()
	registerForestLanguages(reg)

	for _, fl := range forestLanguages {
		fl := fl
		t.Run(fl.name, func(t *testing.T) {
			ext, ok := reg.GetByLanguage(fl.name)
			require.True(t, ok, "language %q not in registry", fl.name)
			assert.Equal(t, fl.name, ext.Language())
			assert.Equal(t, fl.exts, ext.Extensions())

			require.NotEmpty(t, fl.exts, "%s declares no extensions", fl.name)
			byExt, ok := reg.GetByExtension(fl.exts[0])
			require.True(t, ok, "%s ext %q does not resolve", fl.name, fl.exts[0])
			assert.Equal(t, fl.name, byExt.Language())
		})
	}
}

// TestForestRegistrations_Smoke parses a tiny input through every
// forest-backed extractor end-to-end. Confirms the grammar pointer
// is non-nil and the parse pipeline does not panic — empty input is
// universally acceptable across all 510 grammars per spec.
func TestForestRegistrations_Smoke(t *testing.T) {
	reg := parser.NewRegistry()
	registerForestLanguages(reg)

	for _, fl := range forestLanguages {
		fl := fl
		t.Run(fl.name, func(t *testing.T) {
			ext, _ := reg.GetByLanguage(fl.name)
			res, err := ext.Extract("smoke"+fl.exts[0], []byte(""))
			require.NoError(t, err, "%s extract on empty input", fl.name)
			require.NotEmpty(t, res.Nodes, "%s missing file node", fl.name)
		})
	}
}
