package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// newPackFixture builds a synthetic changeset for the review-pack tiers:
//
//   - app/svc.go::Handle   — the CHANGED symbol (lines 1..6), diff hunk in tier 1
//   - app/svc.go::Caller   — a d=1 caller (lines 8..12), full source in tier 2
//   - app/api.go::Outer    — a d=2 symbol (lines 1..4), outline-only in tier 3
//
// The on-disk files under repoRoot back the tier-2 full-source read; the diff
// text backs the tier-1 hunk slicing.
func newPackFixture(t *testing.T) (graph.Store, *ChangeView, *analysis.DiffResult, *analysis.ImpactResult, string) {
	t.Helper()
	repoRoot := t.TempDir()

	svc := "package svc\n\nfunc Handle() {\n\tdoChanged()\n}\n\nfunc Caller() {\n\tHandle()\n\tlogIt()\n}\n"
	api := "package api\n\nfunc Outer() {\n\tCaller()\n}\n"
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, "app"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "app/svc.go"), []byte(svc), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "app/api.go"), []byte(api), 0o644))

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "app/svc.go::Handle", Kind: graph.KindFunction, Name: "Handle",
		FilePath: "app/svc.go", Language: "go", StartLine: 3, EndLine: 5,
		Meta: map[string]any{"signature": "func Handle()"},
	})
	g.AddNode(&graph.Node{
		ID: "app/svc.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: "app/svc.go", Language: "go", StartLine: 7, EndLine: 10,
		Meta: map[string]any{"signature": "func Caller()"},
	})
	g.AddNode(&graph.Node{
		ID: "app/api.go::Outer", Kind: graph.KindFunction, Name: "Outer",
		FilePath: "app/api.go", Language: "go", StartLine: 3, EndLine: 5,
		Meta: map[string]any{"signature": "func Outer()"},
	})

	// A unified diff that changes the body of Handle (lines 3..5 new-side).
	diffText := strings.Join([]string{
		"diff --git a/app/svc.go b/app/svc.go",
		"--- a/app/svc.go",
		"+++ b/app/svc.go",
		"@@ -3,3 +3,3 @@ package svc",
		" func Handle() {",
		"-\tdoOld()",
		"+\tdoChanged()",
		" }",
		"",
	}, "\n")
	view := ChangeViewFromDiff(repoRoot, diffText)

	diff := &analysis.DiffResult{
		ChangedSymbols: []analysis.ChangedSymbol{
			{ID: "app/svc.go::Handle", Name: "Handle", Kind: "function", FilePath: "app/svc.go", Line: 3},
		},
		ChangedFiles: []string{"app/svc.go"},
	}

	impact := &analysis.ImpactResult{
		ByDepth: map[int][]analysis.ImpactEntry{
			1: {{ID: "app/svc.go::Caller", Name: "Caller", Kind: "function", FilePath: "app/svc.go", Line: 7}},
			2: {{ID: "app/api.go::Outer", Name: "Outer", Kind: "function", FilePath: "app/api.go", Line: 3}},
		},
	}

	return g, view, diff, impact, repoRoot
}

func TestBuildReviewPack_Tiers(t *testing.T) {
	g, view, diff, impact, _ := newPackFixture(t)

	pack := BuildReviewPack(g, view, diff, impact, 0) // no budget — every tier kept

	// Tier 1: the changed symbol, as diff-hunk text.
	require.Len(t, pack.Changed, 1)
	c := pack.Changed[0]
	require.Equal(t, "app/svc.go::Handle", c.ID)
	require.Equal(t, TierChanged, c.Tier)
	require.Contains(t, c.Diff, "+\tdoChanged()", "tier-1 entry must carry the added line")
	require.Contains(t, c.Diff, "-\tdoOld()", "tier-1 entry must carry the removed line")
	require.Empty(t, c.Source, "tier-1 entry must not carry full source")

	// Tier 2: the d=1 caller, as FULL source (not a hunk, not an outline).
	require.Len(t, pack.Callers, 1)
	cl := pack.Callers[0]
	require.Equal(t, "app/svc.go::Caller", cl.ID)
	require.Equal(t, TierCaller, cl.Tier)
	require.Contains(t, cl.Source, "func Caller()", "tier-2 entry must carry full source")
	require.Contains(t, cl.Source, "Handle()", "tier-2 full source spans the caller body")
	require.Empty(t, cl.Diff)

	// Tier 3: the d=2 symbol, as an OUTLINE (signature only).
	require.Len(t, pack.Outline, 1)
	o := pack.Outline[0]
	require.Equal(t, "app/api.go::Outer", o.ID)
	require.Equal(t, TierOutline, o.Tier)
	require.Equal(t, "func Outer()", o.Signature, "tier-3 entry must carry the signature")
	require.Empty(t, o.Source)
	require.Empty(t, o.Diff)

	require.False(t, pack.Truncated, "an unbounded pack is never truncated")
}

// TestBuildReviewPack_BudgetDemotesOutlineFirst proves the budget fills tier 1,
// then tier 2, then tier 3 — and demotes tier 3 before tier 2 when tight.
func TestBuildReviewPack_BudgetDemotesOutlineFirst(t *testing.T) {
	g, view, diff, impact, _ := newPackFixture(t)

	full := BuildReviewPack(g, view, diff, impact, 0)
	changedCost := 0
	for _, e := range full.Changed {
		changedCost += entryTokens(e)
	}
	callerCost := 0
	for _, e := range full.Callers {
		callerCost += entryTokens(e)
	}

	// A budget that fits tier 1 + tier 2 but leaves no room for tier 3.
	budget := changedCost + callerCost + 1
	pack := BuildReviewPack(g, view, diff, impact, budget)

	require.Len(t, pack.Changed, 1, "tier 1 always survives")
	require.Len(t, pack.Callers, 1, "tier 2 fits and survives")
	require.Empty(t, pack.Outline, "tier 3 is demoted first when the budget is tight")
	require.True(t, pack.Truncated, "dropping any entry sets truncated")
}

// TestBuildReviewPack_BudgetDemotesCallerNext proves tier 2 is demoted after
// tier 3 but before tier 1: a budget that only fits tier 1 keeps the change and
// drops both callers and outline.
func TestBuildReviewPack_BudgetDemotesCallerNext(t *testing.T) {
	g, view, diff, impact, _ := newPackFixture(t)

	full := BuildReviewPack(g, view, diff, impact, 0)
	changedCost := 0
	for _, e := range full.Changed {
		changedCost += entryTokens(e)
	}

	// A budget that fits only tier 1.
	budget := changedCost + 1
	pack := BuildReviewPack(g, view, diff, impact, budget)

	require.Len(t, pack.Changed, 1, "tier 1 always survives")
	require.Empty(t, pack.Callers, "tier 2 is demoted when only tier 1 fits")
	require.Empty(t, pack.Outline, "tier 3 is demoted first")
	require.True(t, pack.Truncated)
}

// TestBuildReviewPack_ChangedSurvivesTinyBudget proves the change itself is never
// dropped, even when a single changed entry overruns the whole budget.
func TestBuildReviewPack_ChangedSurvivesTinyBudget(t *testing.T) {
	g, view, diff, impact, _ := newPackFixture(t)

	pack := BuildReviewPack(g, view, diff, impact, 1) // budget smaller than any entry

	require.Len(t, pack.Changed, 1, "the change is kept even over budget")
	require.Empty(t, pack.Callers)
	require.Empty(t, pack.Outline)
	require.True(t, pack.Truncated)
}

// TestBuildReviewPack_RenderDeterministic proves the rendered pack is byte-stable
// across builds — entries are pre-sorted by id.
func TestBuildReviewPack_RenderDeterministic(t *testing.T) {
	g, view, diff, impact, _ := newPackFixture(t)

	a := BuildReviewPack(g, view, diff, impact, 0).Render()
	b := BuildReviewPack(g, view, diff, impact, 0).Render()
	require.Equal(t, a, b, "render must be deterministic")

	// The render places the tiers in order with their headers.
	require.Contains(t, a, "## changed (diff)")
	require.Contains(t, a, "## callers (full source)")
	require.Contains(t, a, "## outline (signatures)")
	require.True(t,
		strings.Index(a, "## changed") < strings.Index(a, "## callers") &&
			strings.Index(a, "## callers") < strings.Index(a, "## outline"),
		"tiers render in changed→callers→outline order")
}

// TestBuildReviewPack_OutlineSortedDeterministic proves multiple tier-3 entries
// are emitted in a stable id order regardless of impact-map iteration order.
func TestBuildReviewPack_OutlineSortedDeterministic(t *testing.T) {
	g, view, diff, _, _ := newPackFixture(t)

	g.AddNode(&graph.Node{
		ID: "app/api.go::Alpha", Kind: graph.KindFunction, Name: "Alpha",
		FilePath: "app/api.go", Language: "go", StartLine: 7, EndLine: 8,
		Meta: map[string]any{"signature": "func Alpha()"},
	})

	impact := &analysis.ImpactResult{
		ByDepth: map[int][]analysis.ImpactEntry{
			2: {
				{ID: "app/api.go::Outer", Name: "Outer", FilePath: "app/api.go", Line: 3},
				{ID: "app/api.go::Alpha", Name: "Alpha", FilePath: "app/api.go", Line: 7},
			},
		},
	}

	pack := BuildReviewPack(g, view, diff, impact, 0)
	require.Len(t, pack.Outline, 2)
	require.Equal(t, "app/api.go::Alpha", pack.Outline[0].ID, "outline sorted by id")
	require.Equal(t, "app/api.go::Outer", pack.Outline[1].ID)
}

// TestBuildReviewPack_FallbackHunkWhenNoView proves a changed symbol still yields
// a +/- diff (a pure-addition unified diff) when the ChangeView has no lines for
// it — the off-disk / no-hunk fallback.
func TestBuildReviewPack_FallbackHunkWhenNoView(t *testing.T) {
	g, _, diff, impact, repoRoot := newPackFixture(t)

	// A view rooted at the real repo but with NO recorded diff lines for the
	// changed file forces the unified-diff fallback over the symbol's source.
	view := &ChangeView{RepoRoot: repoRoot, ByFile: map[string]*FileChange{}}

	pack := BuildReviewPack(g, view, diff, impact, 0)
	require.Len(t, pack.Changed, 1)
	require.NotEmpty(t, pack.Changed[0].Diff, "fallback must still produce hunk text")
	require.Contains(t, pack.Changed[0].Diff, "+func Handle()", "fallback renders the new source as additions")
}
