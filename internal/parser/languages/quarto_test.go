package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// quartoSample is a representative .qmd document exercising every node
// family: frontmatter keys, a parent/child heading pair, a labelled {r}
// chunk, an unlabelled {python} chunk, and a plain (non-executable)
// fenced code block that must NOT yield a chunk node.
const quartoSample = "---\n" +
	"title: \"My Report\"\n" +
	"author: Jane Doe\n" +
	"format:\n" +
	"  html:\n" +
	"    toc: true\n" + // nested key — must NOT become a frontmatter node
	"---\n" +
	"\n" +
	"# Introduction\n" +
	"\n" +
	"This is the **intro** with a [link](http://x) and `code`.\n" +
	"\n" +
	"## Details\n" +
	"\n" +
	"Some _child_ section prose here.\n" +
	"\n" +
	"```{r}\n" +
	"#| label: fig-plot\n" +
	"plot(cars)\n" +
	"```\n" +
	"\n" +
	"```{python}\n" +
	"import sys\n" +
	"print(sys.version)\n" +
	"```\n" +
	"\n" +
	"```\n" +
	"echo just a plain block, not a chunk\n" +
	"```\n"

func extractQuarto(t *testing.T, path, body string) *parserResultQuarto {
	t.Helper()
	e := NewQuartoExtractor()
	res, err := e.Extract(path, []byte(body))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return &parserResultQuarto{
		nodesByID: indexNodes(res.Nodes),
		nodes:     res.Nodes,
		defines:   countDefines(res.Edges),
		edges:     res.Edges,
	}
}

type parserResultQuarto struct {
	nodesByID map[string]*graph.Node
	nodes     []*graph.Node
	defines   int
	edges     []*graph.Edge
}

func indexNodes(nodes []*graph.Node) map[string]*graph.Node {
	m := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		m[n.ID] = n
	}
	return m
}

func countDefines(edges []*graph.Edge) int {
	n := 0
	for _, e := range edges {
		if e.Kind == graph.EdgeDefines {
			n++
		}
	}
	return n
}

func TestQuartoLanguageAndExtensions(t *testing.T) {
	e := NewQuartoExtractor()
	if got := e.Language(); got != "quarto" {
		t.Fatalf("Language() = %q, want quarto", got)
	}
	exts := e.Extensions()
	if len(exts) != 1 || exts[0] != ".qmd" {
		t.Fatalf("Extensions() = %v, want [.qmd]", exts)
	}
}

func TestQuartoExtract(t *testing.T) {
	const path = "report.qmd"
	r := extractQuarto(t, path, quartoSample)

	t.Run("file_node", func(t *testing.T) {
		f := r.nodesByID[path]
		if f == nil {
			t.Fatalf("missing file node %q", path)
		}
		if f.Kind != graph.KindFile {
			t.Fatalf("file node kind = %q, want %q", f.Kind, graph.KindFile)
		}
		if f.Language != "quarto" {
			t.Fatalf("file node language = %q, want quarto", f.Language)
		}
		if f.StartLine != 1 {
			t.Fatalf("file node StartLine = %d, want 1", f.StartLine)
		}
	})

	t.Run("frontmatter_keys", func(t *testing.T) {
		cases := []string{"title", "author", "format"}
		for _, key := range cases {
			id := path + "::frontmatter::" + key
			n := r.nodesByID[id]
			if n == nil {
				t.Fatalf("missing frontmatter node %q", id)
			}
			if n.Kind != graph.KindConfigKey {
				t.Fatalf("frontmatter %q kind = %q, want %q", key, n.Kind, graph.KindConfigKey)
			}
			if n.Name != key {
				t.Fatalf("frontmatter node Name = %q, want %q", n.Name, key)
			}
			if n.Meta["source"] != "quarto_frontmatter" {
				t.Fatalf("frontmatter %q meta source = %v, want quarto_frontmatter", key, n.Meta["source"])
			}
		}
		// Nested keys (`html`, `toc`) must NOT be emitted.
		for _, key := range []string{"html", "toc"} {
			if n := r.nodesByID[path+"::frontmatter::"+key]; n != nil {
				t.Fatalf("nested key %q wrongly emitted as a frontmatter node", key)
			}
		}
	})

	t.Run("prose_sections_breadcrumb_and_text", func(t *testing.T) {
		var parent, child *graph.Node
		for _, n := range r.nodes {
			if n.Kind != graph.KindDoc {
				continue
			}
			switch n.Name {
			case "report.qmd > Introduction":
				parent = n
			case "report.qmd > Introduction > Details":
				child = n
			}
		}
		if parent == nil {
			t.Fatalf("missing parent doc section 'report.qmd > Introduction'")
		}
		if child == nil {
			t.Fatalf("missing child doc section 'report.qmd > Introduction > Details'")
		}
		// section_text has markdown inline syntax stripped.
		pt, _ := parent.Meta["section_text"].(string)
		if !strings.Contains(pt, "intro") || strings.Contains(pt, "**") || strings.Contains(pt, "[link]") {
			t.Fatalf("parent section_text not stripped: %q", pt)
		}
		if strings.Contains(pt, "http://x") {
			t.Fatalf("parent section_text kept link URL: %q", pt)
		}
		ct, _ := child.Meta["section_text"].(string)
		if !strings.Contains(ct, "child section prose") {
			t.Fatalf("child section_text missing prose: %q", ct)
		}
		if strings.Contains(ct, "_") {
			t.Fatalf("child section_text kept emphasis markers: %q", ct)
		}
		// Stable, heading-derived IDs (not line-based).
		if parent.ID != path+"::doc:report-qmd-introduction" {
			t.Fatalf("parent doc ID = %q", parent.ID)
		}
		if child.ID != path+"::doc:report-qmd-introduction-details" {
			t.Fatalf("child doc ID = %q", child.ID)
		}
	})

	t.Run("labelled_r_chunk", func(t *testing.T) {
		id := path + "::chunk:fig-plot"
		n := r.nodesByID[id]
		if n == nil {
			t.Fatalf("missing labelled chunk node %q", id)
		}
		if n.Kind != graph.KindFunction {
			t.Fatalf("chunk kind = %q, want %q", n.Kind, graph.KindFunction)
		}
		if n.Name != "fig-plot" {
			t.Fatalf("chunk Name = %q, want fig-plot", n.Name)
		}
		if n.Meta["chunk_language"] != "r" {
			t.Fatalf("chunk_language = %v, want r", n.Meta["chunk_language"])
		}
	})

	t.Run("unlabelled_python_chunk", func(t *testing.T) {
		id := path + "::chunk:chunk-python-1"
		n := r.nodesByID[id]
		if n == nil {
			t.Fatalf("missing generated-name chunk node %q", id)
		}
		if n.Kind != graph.KindFunction {
			t.Fatalf("chunk kind = %q, want %q", n.Kind, graph.KindFunction)
		}
		if n.Name != "chunk-python-1" {
			t.Fatalf("chunk Name = %q, want chunk-python-1", n.Name)
		}
		if n.Meta["chunk_language"] != "python" {
			t.Fatalf("chunk_language = %v, want python", n.Meta["chunk_language"])
		}
	})

	t.Run("plain_block_is_not_a_chunk", func(t *testing.T) {
		for _, n := range r.nodes {
			if n.Kind == graph.KindFunction && strings.Contains(n.Name, "echo") {
				t.Fatalf("plain fenced block wrongly produced a chunk node: %q", n.ID)
			}
		}
		// Exactly two chunk nodes (the {r} and {python} blocks).
		chunks := 0
		for _, n := range r.nodes {
			if n.Kind == graph.KindFunction {
				chunks++
			}
		}
		if chunks != 2 {
			t.Fatalf("chunk node count = %d, want 2", chunks)
		}
	})

	t.Run("edge_defines_count_and_kinds", func(t *testing.T) {
		// One EdgeDefines per non-file node: 3 frontmatter + 2 doc + 2 chunk = 7.
		nonFile := 0
		for _, n := range r.nodes {
			if n.Kind != graph.KindFile {
				nonFile++
			}
		}
		if nonFile != 7 {
			t.Fatalf("non-file node count = %d, want 7", nonFile)
		}
		if r.defines != nonFile {
			t.Fatalf("EdgeDefines count = %d, want %d", r.defines, nonFile)
		}
		// Every define edge originates at the file node.
		for _, e := range r.edges {
			if e.Kind == graph.EdgeDefines && e.From != path {
				t.Fatalf("EdgeDefines from %q, want file node %q", e.From, path)
			}
		}
		// All node kinds are the expected ones (no new kinds introduced).
		allowed := map[graph.NodeKind]bool{
			graph.KindFile: true, graph.KindConfigKey: true,
			graph.KindDoc: true, graph.KindFunction: true,
		}
		for _, n := range r.nodes {
			if !allowed[n.Kind] {
				t.Fatalf("unexpected node kind %q on %q", n.Kind, n.ID)
			}
		}
	})
}

func TestQuartoCRLF(t *testing.T) {
	// CRLF must scan identically to LF.
	crlf := strings.ReplaceAll(quartoSample, "\n", "\r\n")
	r := extractQuarto(t, "report.qmd", crlf)
	if r.nodesByID["report.qmd::chunk:fig-plot"] == nil {
		t.Fatalf("CRLF: missing labelled chunk node")
	}
	if r.nodesByID["report.qmd::frontmatter::title"] == nil {
		t.Fatalf("CRLF: missing frontmatter title node")
	}
}

func TestQuartoTildeFenceAndBraceLabel(t *testing.T) {
	// ~~~ fences and an inline brace label.
	doc := "~~~{r, label=\"setup\"}\n" +
		"library(ggplot2)\n" +
		"~~~\n"
	r := extractQuarto(t, "t.qmd", doc)
	n := r.nodesByID["t.qmd::chunk:setup"]
	if n == nil {
		t.Fatalf("missing tilde-fence chunk with brace label")
	}
	if n.Meta["chunk_language"] != "r" {
		t.Fatalf("chunk_language = %v, want r", n.Meta["chunk_language"])
	}
}
