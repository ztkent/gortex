package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fixtureDiff is a unified diff over pkg/foo.go: it adds a new-side line, keeps
// context, and removes an old-side-only line.
const fixtureDiff = `diff --git a/pkg/foo.go b/pkg/foo.go
index 1111111..2222222 100644
--- a/pkg/foo.go
+++ b/pkg/foo.go
@@ -10,7 +10,8 @@ func Foo() {
 	ctx := context.Background()
 	client := newClient()
-	legacyOnly := deprecatedCall()
+	freshValue := computeFreshValue()
+	another := helper(freshValue)
 	return client.Do(ctx)
 }
`

func TestLocateSnippet_Tier1NewSide(t *testing.T) {
	view := ChangeViewFromDiff("", fixtureDiff)

	a := LocateSnippet(view, "pkg/foo.go", "computeFreshValue()")
	if a.Method != AnchorExactHunk {
		t.Fatalf("method = %q, want %q", a.Method, AnchorExactHunk)
	}
	if a.Side != sideRight {
		t.Fatalf("side = %q, want %q", a.Side, sideRight)
	}
	// "+freshValue := computeFreshValue()" is at new-side line 12
	// (@@ +10,8: 10 ctx, 11 ctx, 12 added).
	if a.Line != 12 {
		t.Fatalf("line = %d, want 12", a.Line)
	}
	if a.Confidence < 0.99 {
		t.Fatalf("confidence = %v, want exact (1.0)", a.Confidence)
	}
}

func TestLocateSnippet_Tier2OldSide(t *testing.T) {
	view := ChangeViewFromDiff("", fixtureDiff)

	a := LocateSnippet(view, "pkg/foo.go", "deprecatedCall()")
	if a.Method != AnchorOldSide {
		t.Fatalf("method = %q, want %q (deterministic miss on new-side)", a.Method, AnchorOldSide)
	}
	if a.Side != sideLeft {
		t.Fatalf("side = %q, want %q", a.Side, sideLeft)
	}
	// "-legacyOnly := deprecatedCall()" is at old-side line 12
	// (@@ -10,7: 10 ctx, 11 ctx, 12 removed).
	if a.Line != 12 {
		t.Fatalf("line = %d, want 12", a.Line)
	}
}

func TestLocateSnippet_Tier3FullFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The on-disk file carries a line that the diff never mentions, so only the
	// full-file tier can locate it.
	body := "package pkg\n\nfunc Foo() {\n\tonlyInFile := untouchedHelper()\n\t_ = onlyInFile\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "pkg", "foo.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	view := ChangeViewFromDiff(dir, fixtureDiff)

	a := LocateSnippet(view, "pkg/foo.go", "untouchedHelper()")
	if a.Method != AnchorFullFile {
		t.Fatalf("method = %q, want %q", a.Method, AnchorFullFile)
	}
	if a.Side != sideRight {
		t.Fatalf("side = %q, want %q", a.Side, sideRight)
	}
	if a.Line != 4 { // 1:package 2:blank 3:func 4:onlyInFile
		t.Fatalf("line = %d, want 4", a.Line)
	}
}

func TestLocateSnippet_Tier4LLMSeam(t *testing.T) {
	view := ChangeViewFromDiff("", fixtureDiff) // no RepoRoot → tier 3 cannot fire

	const missing = "totallyAbsentSymbol()"

	// Deterministic tiers all miss.
	if a := LocateSnippet(view, "pkg/foo.go", missing); a.Method != AnchorUnresolved {
		t.Fatalf("deterministic method = %q, want %q", a.Method, AnchorUnresolved)
	}

	// Stubbed LLM seam returns a valid candidate line number → tier 4.
	gen := func(_ context.Context, _ string, _ int) (string, error) {
		return "line 12", nil // 12 is a real new-side candidate
	}
	a := LocateSnippetWithLLM(context.Background(), gen, view, "pkg/foo.go", missing)
	if a.Method != AnchorLLM {
		t.Fatalf("method = %q, want %q", a.Method, AnchorLLM)
	}
	if a.Line != 12 {
		t.Fatalf("line = %d, want 12", a.Line)
	}
	if a.Side != sideRight {
		t.Fatalf("side = %q, want %q", a.Side, sideRight)
	}
}

func TestLocateSnippet_DisabledSeam(t *testing.T) {
	view := ChangeViewFromDiff("", fixtureDiff)

	// Disabled seam (nil gen): tier 4 must yield unresolved, never an error.
	a := LocateSnippetWithLLM(context.Background(), nil, view, "pkg/foo.go", "totallyAbsentSymbol()")
	if a.Method != AnchorUnresolved {
		t.Fatalf("method = %q, want %q", a.Method, AnchorUnresolved)
	}
	if a.Line != 0 {
		t.Fatalf("line = %d, want 0 (caller drops finding)", a.Line)
	}
	if a.Located() {
		t.Fatal("disabled-seam anchor must not report Located")
	}
}

func TestReLocateWithLLM_ErrorYieldsUnresolved(t *testing.T) {
	view := ChangeViewFromDiff("", fixtureDiff)
	gen := func(_ context.Context, _ string, _ int) (string, error) {
		return "", errors.New("model unavailable")
	}
	a := ReLocateWithLLM(context.Background(), gen, view, "pkg/foo.go", "totallyAbsentSymbol()")
	if a.Method != AnchorUnresolved {
		t.Fatalf("method = %q, want %q on gen error", a.Method, AnchorUnresolved)
	}
}

func TestReLocateWithLLM_OutOfRangeYieldsUnresolved(t *testing.T) {
	view := ChangeViewFromDiff("", fixtureDiff)
	gen := func(_ context.Context, _ string, _ int) (string, error) {
		return "9999", nil // not a candidate line
	}
	a := ReLocateWithLLM(context.Background(), gen, view, "pkg/foo.go", "totallyAbsentSymbol()")
	if a.Method != AnchorUnresolved {
		t.Fatalf("method = %q, want %q on out-of-range answer", a.Method, AnchorUnresolved)
	}
}

func TestLocateFinding_PopulatesFindingFields(t *testing.T) {
	view := ChangeViewFromDiff("", fixtureDiff)

	f := &Finding{
		Rule:     "example-rule",
		Severity: SevWarning,
		Category: "review",
		File:     "pkg/foo.go",
		Message:  "freshValue is unchecked",
		Source:   "llm",
	}
	a := LocateFinding(context.Background(), nil, view, f, "computeFreshValue()")
	if !a.Located() {
		t.Fatalf("anchor not located: %+v", a)
	}
	if f.Line != 12 || f.StartLine != 12 || f.EndLine != 12 {
		t.Fatalf("finding lines = (%d,%d,%d), want all 12", f.Line, f.StartLine, f.EndLine)
	}
	if f.Side != sideRight {
		t.Fatalf("finding side = %q, want %q", f.Side, sideRight)
	}
	if f.Anchor != AnchorExactHunk {
		t.Fatalf("finding anchor = %q, want %q", f.Anchor, AnchorExactHunk)
	}
}

func TestLocateSnippet_UnknownFileUnresolved(t *testing.T) {
	view := ChangeViewFromDiff("", fixtureDiff)
	a := LocateSnippet(view, "pkg/nope.go", "anything")
	if a.Method != AnchorUnresolved {
		t.Fatalf("method = %q, want %q for unknown file", a.Method, AnchorUnresolved)
	}
}
