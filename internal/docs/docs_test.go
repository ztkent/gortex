package docs

import (
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func buildBlamedGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()
	now := time.Now().Unix()
	staleTS := time.Now().Add(-400 * 24 * time.Hour).Unix()

	g.AddNode(&graph.Node{
		ID: "pkg/a.go::Recent", Kind: graph.KindFunction, Name: "Recent",
		FilePath: "pkg/a.go", StartLine: 10, Language: "go",
		Meta: map[string]any{
			"last_authored": map[string]any{
				"email": "alice@example.com", "timestamp": now,
				"commit": "deadbeef1234",
			},
		},
	})
	g.AddNode(&graph.Node{
		ID: "pkg/a.go::Old", Kind: graph.KindFunction, Name: "Old",
		FilePath: "pkg/a.go", StartLine: 30, Language: "go",
		Meta: map[string]any{
			"last_authored": map[string]any{
				"email": "alice@example.com", "timestamp": staleTS,
				"commit": "abcd5678abcd",
			},
		},
	})
	g.AddNode(&graph.Node{
		ID: "pkg/b.go::Bob", Kind: graph.KindMethod, Name: "Bob",
		FilePath: "pkg/b.go", StartLine: 5, Language: "go",
		Meta: map[string]any{
			"last_authored": map[string]any{
				"email": "bob@example.com", "timestamp": now,
				"commit": "01234567",
			},
		},
	})
	return g
}

type fakeHistory struct {
	events []HistoryEvent
}

func (f *fakeHistory) HistorySince(since time.Time) []HistoryEvent {
	var out []HistoryEvent
	for _, e := range f.events {
		if e.Timestamp.After(since) {
			out = append(out, e)
		}
	}
	return out
}

func TestGenerate_AllSections(t *testing.T) {
	g := buildBlamedGraph(t)
	hist := &fakeHistory{events: []HistoryEvent{
		{FilePath: "pkg/a.go", Kind: "modified", NodesAdded: 1, NodesRemoved: 0, Timestamp: time.Now()},
		{FilePath: "pkg/b.go", Kind: "created", NodesAdded: 3, NodesRemoved: 0, Timestamp: time.Now()},
	}}
	bundle, err := Generate(Deps{Graph: g, History: hist}, Options{Since: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(bundle.RecentChanges) != 2 {
		t.Errorf("recent changes = %d, want 2", len(bundle.RecentChanges))
	}
	if len(bundle.OwnershipRows) != 2 {
		t.Errorf("owners = %d, want 2", len(bundle.OwnershipRows))
	}
	if len(bundle.StaleCodeRows) != 1 {
		t.Errorf("stale rows = %d, want 1", len(bundle.StaleCodeRows))
	}
}

func TestGenerate_SectionsFilter(t *testing.T) {
	g := buildBlamedGraph(t)
	bundle, err := Generate(Deps{Graph: g},
		Options{Sections: []string{"stale"}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(bundle.OwnershipRows) != 0 {
		t.Errorf("ownership shouldn't render when not in sections; got %d", len(bundle.OwnershipRows))
	}
	if len(bundle.RecentChanges) != 0 {
		t.Errorf("recent shouldn't render; got %d", len(bundle.RecentChanges))
	}
	if len(bundle.StaleCodeRows) != 1 {
		t.Errorf("stale rows = %d, want 1", len(bundle.StaleCodeRows))
	}
}

func TestRenderMarkdown_HasSections(t *testing.T) {
	g := buildBlamedGraph(t)
	bundle, err := Generate(Deps{Graph: g}, Options{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	md := RenderMarkdown(bundle)
	for _, want := range []string{"# Docs Bundle", "## Recent Changes", "## Ownership", "## Stale Code", "## Blame Enrichment"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestRenderJSON_RoundTrips(t *testing.T) {
	g := buildBlamedGraph(t)
	bundle, err := Generate(Deps{Graph: g}, Options{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	data, err := RenderJSON(bundle)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !strings.Contains(string(data), `"sections"`) {
		t.Error("JSON should include sections field")
	}
}

func TestGenerate_WithBlame(t *testing.T) {
	g := buildBlamedGraph(t)
	called := 0
	bundle, err := Generate(Deps{
		Graph: g,
		Blame: func() (int, map[string]int, error) {
			called++
			return 42, map[string]int{"gortex": 42}, nil
		},
	}, Options{IncludeBlame: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if called != 1 {
		t.Errorf("blame runner called %d times, want 1", called)
	}
	if bundle.Blame == nil || bundle.Blame.Enriched != 42 {
		t.Fatalf("blame summary missing or wrong: %+v", bundle.Blame)
	}
}

func TestGenerate_NilGraphFails(t *testing.T) {
	if _, err := Generate(Deps{}, Options{}); err == nil {
		t.Error("expected error when graph is nil")
	}
}
