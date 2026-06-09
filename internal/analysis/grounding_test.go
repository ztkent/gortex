package analysis

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func groundingLines() map[string][]HunkLine {
	return map[string][]HunkLine{
		"pkg/foo.go": {
			{NewLine: 3, Side: " ", Text: "func Foo() int {"},
			{NewLine: 4, Side: "+", Text: "\tx := compute(a, b)"},
			{NewLine: 5, Side: "+", Text: "\treturn x"},
			{NewLine: 6, Side: " ", Text: "}"},
			{NewLine: 10, Side: "+", Text: "var globalCounter int"},
		},
	}
}

func TestGroundFindingExact(t *testing.T) {
	lg := NewLineGrounder(nil, groundingLines())
	hit, ok := lg.GroundFinding("pkg/foo.go", "", "compute(a, b)")
	if !ok {
		t.Fatal("expected a hit for an exact substring")
	}
	if hit.Line != 4 {
		t.Fatalf("expected line 4, got %d", hit.Line)
	}
	if hit.Confidence != 1.0 {
		t.Fatalf("expected confidence 1.0, got %v", hit.Confidence)
	}
	if hit.File != "pkg/foo.go" {
		t.Fatalf("unexpected file %q", hit.File)
	}
}

func TestGroundFindingWhitespaceVariant(t *testing.T) {
	lg := NewLineGrounder(nil, groundingLines())
	// Different inner spacing than the source ("compute(a, b)" vs "compute(a,  b)")
	// — defeats exact match, must hit the whitespace-normalized tier.
	hit, ok := lg.GroundFinding("pkg/foo.go", "", "x   :=   compute(a,  b)")
	if !ok {
		t.Fatal("expected a hit for a whitespace variant")
	}
	if hit.Line != 4 {
		t.Fatalf("expected line 4, got %d", hit.Line)
	}
	if hit.Confidence != 0.7 {
		t.Fatalf("expected confidence 0.7, got %v", hit.Confidence)
	}
}

func TestGroundFindingNonexistent(t *testing.T) {
	lg := NewLineGrounder(nil, groundingLines())
	_, ok := lg.GroundFinding("pkg/foo.go", "", "totally unrelated zzzqqq phrase")
	if ok {
		t.Fatal("expected ok=false for a snippet that matches nothing")
	}
	// Unknown file → also ok=false.
	if _, ok := lg.GroundFinding("pkg/nope.go", "", "anything"); ok {
		t.Fatal("expected ok=false for an unknown file")
	}
}

func TestGroundFindingEmptySnippetFirstAdded(t *testing.T) {
	lg := NewLineGrounder(nil, groundingLines())
	hit, ok := lg.GroundFinding("pkg/foo.go", "", "")
	if !ok {
		t.Fatal("expected a hit for an empty snippet")
	}
	// First added line in the file is line 4 (line 3 is context).
	if hit.Line != 4 {
		t.Fatalf("expected first added line 4, got %d", hit.Line)
	}
}

func TestGroundFindingTokenOverlapFallback(t *testing.T) {
	lg := NewLineGrounder(nil, groundingLines())
	// No exact / whitespace match, but shares the token "compute".
	hit, ok := lg.GroundFinding("pkg/foo.go", "", "result of compute step")
	if !ok {
		t.Fatal("expected a token-overlap fallback hit")
	}
	if hit.Line != 4 {
		t.Fatalf("expected fallback to land on line 4, got %d", hit.Line)
	}
	if hit.Confidence >= 0.5 {
		t.Fatalf("fallback confidence must be < 0.5, got %v", hit.Confidence)
	}
}

func TestGroundFindingSymbolScoped(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "pkg/foo.go::Foo",
		Kind:      graph.KindFunction,
		Name:      "Foo",
		FilePath:  "pkg/foo.go",
		StartLine: 3,
		EndLine:   6,
		Language:  "go",
	})
	lg := NewLineGrounder(g, groundingLines())

	// Empty snippet scoped to Foo (lines 3..6) → first added line in span = 4,
	// NOT the line-10 add outside the symbol.
	hit, ok := lg.GroundFinding("pkg/foo.go", "pkg/foo.go::Foo", "")
	if !ok {
		t.Fatal("expected a hit scoped to Foo")
	}
	if hit.Line != 4 {
		t.Fatalf("expected first added line inside Foo (4), got %d", hit.Line)
	}

	// A snippet that only exists outside the symbol span still anchors (scope
	// falls back to the whole file when nothing in-span matches), but the
	// in-span content is preferred when present.
	hit, ok = lg.GroundFinding("pkg/foo.go", "pkg/foo.go::Foo", "return x")
	if !ok {
		t.Fatal("expected a hit for in-span snippet")
	}
	if hit.Line != 5 {
		t.Fatalf("expected line 5 for 'return x', got %d", hit.Line)
	}
}
