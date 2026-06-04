package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// shFor returns an ExtractorPluginSpec whose command is `sh -c <script>`
// — the script consumes stdin and echoes a fixed JSON document.
func shFor(language, ext, script string) config.ExtractorPluginSpec {
	return config.ExtractorPluginSpec{
		Language:   language,
		Extensions: []string{ext},
		Command:    "sh",
		Args:       []string{"-c", script},
	}
}

func TestSubprocessExtractor_EmitsNodesAndEdges(t *testing.T) {
	// One function node + one EdgeDefines from the file to it. The
	// script drains stdin so the parent's stdin pipe always closes.
	const json = `{"nodes":[` +
		`{"id":"foo.ml::main","kind":"function","name":"main","start_line":3,"end_line":9,"meta":{"k":"v"}}` +
		`],"edges":[` +
		`{"from":"foo.ml","to":"foo.ml::main","kind":"defines","line":3}` +
		`]}`
	ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf '%s' '"+json+"'"), nil)

	res, err := ext.Extract("foo.ml", []byte("let\nx\nmain () = ()\n"))
	require.NoError(t, err)
	require.NotNil(t, res)

	// File node is always emitted first.
	require.GreaterOrEqual(t, len(res.Nodes), 2)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
	assert.Equal(t, "foo.ml", res.Nodes[0].ID)
	assert.Equal(t, "ml", res.Nodes[0].Language)

	fn := res.Nodes[1]
	assert.Equal(t, "foo.ml::main", fn.ID)
	assert.Equal(t, graph.KindFunction, fn.Kind)
	assert.Equal(t, "main", fn.Name)
	assert.Equal(t, 3, fn.StartLine)
	assert.Equal(t, 9, fn.EndLine)
	assert.Equal(t, "ml", fn.Language) // defaulted from extractor language
	assert.Equal(t, "v", fn.Meta["k"])

	require.Len(t, res.Edges, 1)
	assert.Equal(t, "foo.ml", res.Edges[0].From)
	assert.Equal(t, "foo.ml::main", res.Edges[0].To)
	assert.Equal(t, graph.EdgeDefines, res.Edges[0].Kind)
}

func TestSubprocessExtractor_SynthesizesMissingID(t *testing.T) {
	const json = `{"nodes":[{"kind":"type","name":"Widget"}]}`
	ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf '%s' '"+json+"'"), nil)

	res, err := ext.Extract("w.ml", []byte("x"))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 2)
	assert.Equal(t, "w.ml::Widget", res.Nodes[1].ID) // filePath + "::" + name
}

func TestSubprocessExtractor_DropsInvalidKind(t *testing.T) {
	const json = `{"nodes":[` +
		`{"id":"bad","kind":"not_a_real_kind","name":"x"},` +
		`{"id":"good","kind":"function","name":"y"}` +
		`]}`
	ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf '%s' '"+json+"'"), nil)

	res, err := ext.Extract("z.ml", []byte("x"))
	require.NoError(t, err)
	// file node + the one valid node; the invalid-kind node is dropped.
	require.Len(t, res.Nodes, 2)
	assert.Equal(t, "good", res.Nodes[1].ID)
}

func TestSubprocessExtractor_FailingCommandDegradesToFileNode(t *testing.T) {
	ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; exit 3"), nil)

	res, err := ext.Extract("e.ml", []byte("x\ny\n"))
	require.NoError(t, err) // never fails the index
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
	assert.Empty(t, res.Edges)
}

func TestSubprocessExtractor_BadJSONDegradesToFileNode(t *testing.T) {
	ext := NewSubprocessExtractor(shFor("ml", ".ml", "cat >/dev/null; printf 'not json'"), nil)

	res, err := ext.Extract("b.ml", []byte("x"))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}

func TestRegisterExtractorPlugins_SkipsInvalidAndCollisions(t *testing.T) {
	reg := parser.NewRegistry()
	reg.Register(NewGoExtractor()) // claims "go" and ".go"

	RegisterExtractorPlugins(reg, []config.ExtractorPluginSpec{
		{Language: "", Extensions: []string{".x"}, Command: "cat"},        // no language
		{Language: "x", Extensions: nil, Command: "cat"},                  // no extensions
		{Language: "x", Extensions: []string{".x"}, Command: ""},          // no command
		{Language: "go", Extensions: []string{".golang"}, Command: "cat"}, // language collision
		{Language: "mygo", Extensions: []string{".go"}, Command: "cat"},   // extension collision
		{Language: "ml", Extensions: []string{".ml"}, Command: "cat"},     // valid
	}, nil)

	_, hasML := reg.GetByLanguage("ml")
	assert.True(t, hasML)
	_, hasMygo := reg.GetByLanguage("mygo")
	assert.False(t, hasMygo)
	// The built-in Go extractor is untouched.
	e, ok := reg.GetByLanguage("go")
	require.True(t, ok)
	assert.Equal(t, "go", e.Language())
}
