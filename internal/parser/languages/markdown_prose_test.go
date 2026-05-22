package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

const proseDoc = "# Project\n" +
	"\n" +
	"An intro paragraph about the project.\n" +
	"\n" +
	"## Setup\n" +
	"\n" +
	"Run the installer before anything else.\n" +
	"\n" +
	"### Build\n" +
	"\n" +
	"Compile with the standard toolchain and link the binary.\n" +
	"\n" +
	"## Usage\n" +
	"\n" +
	"Invoke the command with a config file.\n"

func docNodes(nodes []*graph.Node) []*graph.Node {
	var out []*graph.Node
	for _, n := range nodes {
		if n.Kind == graph.KindDoc {
			out = append(out, n)
		}
	}
	return out
}

func TestMarkdownProse_SectionExtraction(t *testing.T) {
	e := NewMarkdownExtractor()
	result, err := e.Extract("README.md", []byte(proseDoc))
	require.NoError(t, err)

	docs := docNodes(result.Nodes)
	require.NotEmpty(t, docs, "prose extraction should emit KindDoc nodes")

	byName := map[string]*graph.Node{}
	for _, d := range docs {
		byName[d.Name] = d
	}

	// Heading-path breadcrumb names.
	require.Contains(t, byName, "README.md > Project")
	require.Contains(t, byName, "README.md > Project > Setup")
	require.Contains(t, byName, "README.md > Project > Setup > Build")
	require.Contains(t, byName, "README.md > Project > Usage")

	// The Build section's body is the section paragraph, markdown
	// stripped.
	build := byName["README.md > Project > Setup > Build"]
	bodyText, _ := build.Meta["section_text"].(string)
	assert.Contains(t, bodyText, "Compile with the standard toolchain")
	// The deeper Build heading must NOT absorb the parent Setup prose.
	assert.NotContains(t, bodyText, "Run the installer")

	// heading_path meta is the crumb slice.
	hp, _ := build.Meta["heading_path"].([]string)
	assert.Equal(t, []string{"Project", "Setup", "Build"}, hp)

	// Line ranges are populated and sane.
	assert.Greater(t, build.StartLine, 0)
	assert.GreaterOrEqual(t, build.EndLine, build.StartLine)
}

func TestMarkdownProse_StableIDs(t *testing.T) {
	e := NewMarkdownExtractor()

	r1, err := e.Extract("README.md", []byte(proseDoc))
	require.NoError(t, err)

	// Re-extract the SAME doc with extra blank lines inserted at the
	// top -- every section shifts down, but the IDs (keyed on the
	// heading path, not line numbers) must be identical.
	shifted := "\n\n\n" + proseDoc
	r2, err := e.Extract("README.md", []byte(shifted))
	require.NoError(t, err)

	ids := func(nodes []*graph.Node) map[string]bool {
		m := map[string]bool{}
		for _, n := range docNodes(nodes) {
			m[n.ID] = true
		}
		return m
	}
	id1, id2 := ids(r1.Nodes), ids(r2.Nodes)
	require.Equal(t, id1, id2, "section IDs must be stable across a line-shifting edit")

	// IDs are heading-path derived, file-prefixed.
	for id := range id1 {
		assert.Contains(t, id, "README.md::doc:")
	}
}

func TestMarkdownProse_PreambleCaptured(t *testing.T) {
	// Prose before the first heading is not lost.
	e := NewMarkdownExtractor()
	doc := "Leading prose with no heading above it.\n\n# Title\n\nBody.\n"
	result, err := e.Extract("notes.md", []byte(doc))
	require.NoError(t, err)
	var preamble *graph.Node
	for _, d := range docNodes(result.Nodes) {
		if txt, _ := d.Meta["section_text"].(string); txt == "Leading prose with no heading above it." {
			preamble = d
		}
	}
	require.NotNil(t, preamble, "leading prose should be captured in a preamble section")
}

func TestMarkdownProse_EmptySectionsSkipped(t *testing.T) {
	// A heading with no prose body carries no search signal -- it
	// should not produce a KindDoc node.
	e := NewMarkdownExtractor()
	doc := "# Only A Heading\n\n## Another Bare Heading\n"
	result, err := e.Extract("bare.md", []byte(doc))
	require.NoError(t, err)
	assert.Empty(t, docNodes(result.Nodes),
		"headings with no prose body should not emit KindDoc nodes")
}

func TestMarkdownProse_InlineSyntaxStripped(t *testing.T) {
	e := NewMarkdownExtractor()
	doc := "# Doc\n\nSee the [install guide](setup.md) and run `make build` for **details**.\n"
	result, err := e.Extract("d.md", []byte(doc))
	require.NoError(t, err)
	docs := docNodes(result.Nodes)
	require.Len(t, docs, 1)
	body, _ := docs[0].Meta["section_text"].(string)
	// Link text kept, URL dropped; inline code unwrapped; emphasis
	// markers removed.
	assert.Contains(t, body, "install guide")
	assert.NotContains(t, body, "setup.md")
	assert.NotContains(t, body, "`")
	assert.NotContains(t, body, "**")
	assert.Contains(t, body, "make build")
}
