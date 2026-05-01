package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestOrgModeExtractor_Headings(t *testing.T) {
	src := []byte(`* Getting Started

** Installation

Some text here.

** Usage

More text.

*** Advanced
`)
	e := NewOrgModeExtractor()
	result, err := e.Extract("README.org", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	require.GreaterOrEqual(t, len(vars), 4)

	byName := map[string]*graph.Node{}
	for _, v := range vars {
		byName[v.Name] = v
	}
	require.Contains(t, byName, "Getting Started")
	require.Contains(t, byName, "Installation")
	require.Contains(t, byName, "Usage")
	require.Contains(t, byName, "Advanced")

	assert.Equal(t, 1, byName["Getting Started"].Meta["heading_level"])
	assert.Equal(t, 2, byName["Installation"].Meta["heading_level"])
	assert.Equal(t, 3, byName["Advanced"].Meta["heading_level"])
}

func TestOrgModeExtractor_TodoAndPriority(t *testing.T) {
	src := []byte(`* TODO [#A] Ship the release

* DONE [#C] Update changelog

* Plain heading
`)
	e := NewOrgModeExtractor()
	result, err := e.Extract("todo.org", src)
	require.NoError(t, err)

	byName := map[string]*graph.Node{}
	for _, v := range nodesOfKind(result.Nodes, graph.KindVariable) {
		byName[v.Name] = v
	}

	require.Contains(t, byName, "Ship the release")
	assert.Equal(t, "TODO", byName["Ship the release"].Meta["todo_state"])
	assert.Equal(t, "[#A]", byName["Ship the release"].Meta["priority"])

	require.Contains(t, byName, "Update changelog")
	assert.Equal(t, "DONE", byName["Update changelog"].Meta["todo_state"])

	require.Contains(t, byName, "Plain heading")
	_, hasTodo := byName["Plain heading"].Meta["todo_state"]
	assert.False(t, hasTodo)
}

func TestOrgModeExtractor_Links(t *testing.T) {
	src := []byte(`* Docs

See [[file:CONTRIBUTING.org][contributing]] for guidelines.

Check [[docs/config.org]] and [[./docs/api.org][API docs]].

External link [[https://example.com][site]] should be skipped.

Anchor link [[#section]] should be skipped.
`)
	e := NewOrgModeExtractor()
	result, err := e.Extract("README.org", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	targets := map[string]bool{}
	for _, edge := range imports {
		targets[edge.To] = true
	}
	assert.Contains(t, targets, "unresolved::import::CONTRIBUTING.org")
	assert.Contains(t, targets, "unresolved::import::docs/config.org")
	assert.Contains(t, targets, "unresolved::import::./docs/api.org")
	for target := range targets {
		assert.NotContains(t, target, "example.com")
		assert.NotContains(t, target, "#section")
	}
}

func TestOrgModeExtractor_SrcBlocks(t *testing.T) {
	src := []byte(`* Examples

#+BEGIN_SRC go
func main() {}
#+END_SRC

#+BEGIN_SRC bash
echo hi
#+END_SRC

#+BEGIN_QUOTE
A quote.
#+END_QUOTE
`)
	e := NewOrgModeExtractor()
	result, err := e.Extract("examples.org", src)
	require.NoError(t, err)

	codeLangs := []string{}
	blockTypes := []string{}
	for _, v := range nodesOfKind(result.Nodes, graph.KindVariable) {
		if lang, ok := v.Meta["code_language"].(string); ok {
			codeLangs = append(codeLangs, lang)
		}
		if t, ok := v.Meta["block_type"].(string); ok {
			blockTypes = append(blockTypes, t)
		}
	}
	assert.ElementsMatch(t, []string{"go", "bash"}, codeLangs)
	assert.Contains(t, blockTypes, "src")
	assert.Contains(t, blockTypes, "quote")
}

func TestOrgModeExtractor_TitleKeyword(t *testing.T) {
	src := []byte(`#+TITLE: My Document
#+AUTHOR: Andrey

* First heading
`)
	e := NewOrgModeExtractor()
	result, err := e.Extract("doc.org", src)
	require.NoError(t, err)

	files := nodesOfKind(result.Nodes, graph.KindFile)
	require.Len(t, files, 1)
	assert.Equal(t, "My Document", files[0].Meta["title"])
	assert.Equal(t, "Andrey", files[0].Meta["author"])
}

func TestOrgModeExtractor_FileNode(t *testing.T) {
	src := []byte("* Heading\n")
	e := NewOrgModeExtractor()
	result, err := e.Extract("doc.org", src)
	require.NoError(t, err)

	files := nodesOfKind(result.Nodes, graph.KindFile)
	require.Len(t, files, 1)
	assert.Equal(t, "doc.org", files[0].Name)
	assert.Equal(t, "orgmode", files[0].Language)
}

func TestOrgModeExtractor_Extensions(t *testing.T) {
	e := NewOrgModeExtractor()
	assert.Equal(t, "orgmode", e.Language())
	assert.Equal(t, []string{".org"}, e.Extensions())
}
