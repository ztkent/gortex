package forest

import (
	"testing"
	"unsafe"

	"github.com/alexaandru/go-sitter-forest/elm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestExtractor_TagsPath(t *testing.T) {
	// Elm ships a tags.scm — exercises the tags-driven path.
	e := New("elm", []string{".elm"}, elm.GetLanguage, elm.GetQuery)

	require.Equal(t, "elm", e.Language())
	require.Equal(t, []string{".elm"}, e.Extensions())

	src := []byte(`module M exposing (..)

type Color = Red | Green | Blue

double : Int -> Int
double n = n * 2
`)

	res, err := e.Extract("M.elm", src)
	require.NoError(t, err)
	require.NotEmpty(t, res.Nodes)

	gotFile := false
	gotDouble := false
	gotColor := false
	for _, n := range res.Nodes {
		switch n.Kind {
		case graph.KindFile:
			gotFile = n.FilePath == "M.elm"
		case graph.KindFunction:
			if n.Name == "double" {
				gotDouble = true
			}
		case graph.KindType:
			if n.Name == "Color" {
				gotColor = true
			}
		}
	}
	assert.True(t, gotFile, "missing file node")
	assert.True(t, gotDouble, "missing `double` function")
	assert.True(t, gotColor, "missing `Color` type")
}

func TestExtractor_ParseError(t *testing.T) {
	// Garbage input still returns a tree (forest grammars produce a
	// best-effort tree with ERROR nodes), so Extract should succeed
	// with at least the file node and not panic.
	e := New("elm", []string{".elm"}, elm.GetLanguage, elm.GetQuery)
	res, err := e.Extract("bad.elm", []byte("@@@@@@@@@"))
	require.NoError(t, err)
	require.NotEmpty(t, res.Nodes)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}

func TestExtractor_CallEdgesParentToEnclosingFunc(t *testing.T) {
	// Two functions; calls inside `caller`'s body should land on
	// `M.elm::caller`, not on the file. Confirms the second-pass
	// attribution from buildFuncRanges/findEnclosingFunc.
	//
	// Note: elm's tags.scm also captures type-annotation references
	// (`caller : Int -> Int`) as @reference.function — those land
	// outside any function range and parent to the file, which is
	// the documented module-level fallback. We only assert the
	// in-body case here.
	e := New("elm", []string{".elm"}, elm.GetLanguage, elm.GetQuery)

	src := []byte(`module M exposing (..)

helper : Int -> Int
helper n = n + 1

caller : Int -> Int
caller x =
    helper (helper x)
`)

	res, err := e.Extract("M.elm", src)
	require.NoError(t, err)

	bodyCallToHelper := false
	for _, ed := range res.Edges {
		if ed.Kind != graph.EdgeCalls {
			continue
		}
		if ed.To == "unresolved::helper" && ed.From == "M.elm::caller" {
			bodyCallToHelper = true
			break
		}
	}
	assert.True(t, bodyCallToHelper,
		"expected a call edge from M.elm::caller to unresolved::helper; got %+v",
		callEdgeSummary(res.Edges))
}

// callEdgeSummary builds a compact list of (from, to) pairs for
// every EdgeCalls edge — only used when an assertion fails to give
// the failure message something diagnostic to chew on.
func callEdgeSummary(edges []*graph.Edge) []string {
	var out []string
	for _, ed := range edges {
		if ed.Kind == graph.EdgeCalls {
			out = append(out, ed.From+" -> "+ed.To)
		}
	}
	return out
}

func TestExtractor_ModuleLevelCallsParentToFile(t *testing.T) {
	// A call at module top-level (no enclosing function) should
	// fall back to the file node — better than dropping the edge.
	e := New("elm", []string{".elm"}, elm.GetLanguage, elm.GetQuery)

	src := []byte(`module M exposing (..)

import X

result =
    foo bar
`)
	res, err := e.Extract("M.elm", src)
	require.NoError(t, err)

	// `result = foo bar` is a value declaration, treated as a function
	// in elm tags.scm — so its calls would parent to it. We pick a
	// file-level expression test that's harder: the assignment itself
	// is the def, but if any @reference.call is captured outside any
	// function, it should land on the file.
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			assert.True(t,
				ed.From == "M.elm" || ed.From == "M.elm::result",
				"unexpected call parent %q", ed.From)
		}
	}
}

func TestExtractor_NilGetLanguage(t *testing.T) {
	// Sticky-error path: a grammar that returns a nil pointer fails
	// the first call and every subsequent call (cached error)
	// without panicking.
	e := New("broken", []string{".x"}, func() unsafe.Pointer { return nil }, nil)

	_, err := e.Extract("a.x", []byte("anything"))
	require.Error(t, err)
	require.ErrorIs(t, err, errNilLanguage)

	// Sticky cache — second call returns same error without
	// re-invoking the grammar.
	_, err2 := e.Extract("b.x", []byte("again"))
	require.ErrorIs(t, err2, errNilLanguage)
}
