package exporter

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func toyGraph() *graph.Graph {
	g := graph.New()
	for _, n := range []*graph.Node{
		{ID: "a/main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "a/main.go", Language: "go"},
		{ID: "a/main.go::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/main.go", Language: "go"},
		{ID: "a/main.go::dispatch", Kind: graph.KindFunction, Name: "dispatch", FilePath: "a/main.go", Language: "go"},
		{ID: "b/store.go::Save", Kind: graph.KindFunction, Name: "Save", FilePath: "b/store.go", Language: "go"},
		{ID: "b/store.go::Load", Kind: graph.KindFunction, Name: "Load", FilePath: "b/store.go", Language: "go"},
	} {
		g.AddNode(n)
	}
	for _, e := range []*graph.Edge{
		{From: "a/main.go::main", To: "a/main.go::run", Kind: graph.EdgeCalls},
		{From: "a/main.go::run", To: "a/main.go::dispatch", Kind: graph.EdgeCalls},
		{From: "a/main.go::dispatch", To: "b/store.go::Save", Kind: graph.EdgeCalls},
		{From: "a/main.go::dispatch", To: "b/store.go::Load", Kind: graph.EdgeCalls},
	} {
		g.AddEdge(e)
	}
	return g
}

func TestWriteMermaid_Architecture(t *testing.T) {
	var buf bytes.Buffer
	stats, err := WriteMermaid(&buf, toyGraph(), MermaidOpts{Scope: "architecture", MinCommunity: 1})
	if err != nil {
		t.Fatalf("WriteMermaid: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "graph TB\n") {
		t.Errorf("expected graph TB header; got %q", out)
	}
	if stats.NodesWritten == 0 {
		t.Errorf("nodes_written = 0; want > 0")
	}
	if stats.BytesWritten == 0 {
		t.Error("bytes_written should be > 0")
	}
}

func TestWriteMermaid_Communities(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteMermaid(&buf, toyGraph(), MermaidOpts{Scope: "communities", MinCommunity: 1}); err != nil {
		t.Fatalf("WriteMermaid: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "graph LR\n") {
		t.Errorf("communities scope should be graph LR; got %q", buf.String())
	}
}

func TestWriteMermaid_Processes(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteMermaid(&buf, toyGraph(), MermaidOpts{Scope: "processes", MinCommunity: 1}); err != nil {
		t.Fatalf("WriteMermaid: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "graph LR\n") {
		t.Errorf("expected graph LR; got %q", out)
	}
	if !strings.Contains(out, "subgraph") {
		t.Errorf("processes scope should emit subgraph blocks; got %q", out)
	}
}

func TestWriteMermaid_All(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteMermaid(&buf, toyGraph(), MermaidOpts{Scope: "all", MinCommunity: 1}); err != nil {
		t.Fatalf("WriteMermaid: %v", err)
	}
	out := buf.String()
	for _, marker := range []string{
		"gortex architecture diagram",
		"gortex communities diagram",
		"gortex processes diagram",
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("expected %q in all-scope output", marker)
		}
	}
}

func TestWriteMermaid_UnknownScope(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteMermaid(&buf, toyGraph(), MermaidOpts{Scope: "nonsense"}); err == nil {
		t.Error("expected error for unknown scope")
	}
}

func TestWriteMermaid_NilGraph(t *testing.T) {
	var buf bytes.Buffer
	stats, err := WriteMermaid(&buf, nil, MermaidOpts{})
	if err != nil {
		t.Fatalf("WriteMermaid: %v", err)
	}
	if !strings.Contains(buf.String(), "no graph") {
		t.Errorf("nil graph should write a placeholder; got %q", buf.String())
	}
	if stats.BytesWritten == 0 {
		t.Error("placeholder should still write bytes")
	}
}
