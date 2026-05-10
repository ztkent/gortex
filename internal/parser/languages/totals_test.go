package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/parser"
)

// TestRegisterAll_Count is a guard against silent regressions: it
// pins the total number of registered languages so a future PR can't
// drop a registration unnoticed. Update the lower bound when langs
// are added; lower it only when you're explicitly removing one.
func TestRegisterAll_Count(t *testing.T) {
	reg := parser.NewRegistry()
	RegisterAll(reg)

	langs := reg.SupportedLanguages()
	assert.GreaterOrEqual(t, len(langs), 200,
		"language count regressed: got %d, want >= 200", len(langs))
	t.Logf("RegisterAll registered %d languages", len(langs))
}
