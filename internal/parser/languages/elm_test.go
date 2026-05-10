package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestElmExtractor_Basics(t *testing.T) {
	src := []byte(`module Main exposing (..)

import Html exposing (Html, text)
import Json.Decode as Decode

type Model
    = Loading
    | Loaded String
    | Failed

type alias Person =
    { name : String
    , age : Int
    }

main : Html msg
main =
    text "hello"

view : Model -> Html msg
view model =
    case model of
        Loading -> text "..."
        Loaded s -> text s
        Failed -> text "err"
`)

	e := NewElmExtractor()
	require.Equal(t, "elm", e.Language())
	require.Equal(t, []string{".elm"}, e.Extensions())

	res, err := e.Extract("Main.elm", src)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotEmpty(t, res.Nodes, "elm parse should produce at least the file node")

	// File node always first.
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
	assert.Equal(t, "elm", res.Nodes[0].Language)

	// Elm's tags.scm captures function declarations
	// (@definition.function), `type` declarations
	// (@definition.type), union variants (@definition.union), and the
	// module name (@definition.module). Type aliases are not
	// captured upstream — that's a tags.scm limitation, not a bug.
	gotMain, gotView, gotModel, gotMainMod := false, false, false, false
	for _, n := range res.Nodes {
		switch n.Name {
		case "main":
			gotMain = n.Kind == graph.KindFunction
		case "view":
			gotView = n.Kind == graph.KindFunction
		case "Model":
			gotModel = n.Kind == graph.KindType
		case "Main":
			gotMainMod = n.Kind == graph.KindPackage
		}
	}
	assert.True(t, gotMain, "expected `main` extracted as function")
	assert.True(t, gotView, "expected `view` extracted as function")
	assert.True(t, gotModel, "expected `Model` extracted as type")
	assert.True(t, gotMainMod, "expected `Main` module extracted as package")

	// EdgeDefines must wire every definition back to the file node.
	defines := 0
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeDefines {
			defines++
		}
	}
	assert.GreaterOrEqual(t, defines, 4, "expected at least 4 EdgeDefines")
}

func TestElmExtractor_EmptyInput(t *testing.T) {
	res, err := NewElmExtractor().Extract("e.elm", []byte(""))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}
