package analysis

import (
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// fanInGraph builds a hub symbol with `callers` inbound call edges plus a
// covering test, and returns the graph.
func fanInGraph(t *testing.T, hubID, hubFile string, callers int) *graph.Graph {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: hubID, Kind: graph.KindFunction, Name: "Hub", FilePath: hubFile, StartLine: 1, EndLine: 10})
	for i := 0; i < callers; i++ {
		cid := hubFile + "::caller" + itoaTest(i)
		g.AddNode(&graph.Node{ID: cid, Kind: graph.KindFunction, Name: "caller" + itoaTest(i), FilePath: hubFile})
		g.AddEdge(&graph.Edge{From: cid, To: hubID, Kind: graph.EdgeCalls})
	}
	return g
}

func itoaTest(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}

func factorByAxis(factors []PRRiskFactor, axis string) (PRRiskFactor, bool) {
	for _, f := range factors {
		if f.Axis == axis {
			return f, true
		}
	}
	return PRRiskFactor{}, false
}

func TestScorePRRisk_WideFanInDrivesFlow(t *testing.T) {
	hubID := "pkg/hub.go::Hub"
	g := fanInGraph(t, hubID, "pkg/hub.go", 25)

	res := ScorePRRisk(g, PRRiskInput{
		SymbolIDs:    []string{hubID},
		ChangedFiles: []string{"pkg/hub.go"},
	})

	if res.TotalAffected < 20 {
		t.Fatalf("expected wide blast radius, got TotalAffected=%d", res.TotalAffected)
	}
	flow, ok := factorByAxis(res.Factors, "flow")
	if !ok {
		t.Fatal("flow factor missing")
	}
	if flow.Score < 50 {
		t.Fatalf("expected high flow axis for a 25-caller hub, got %.1f", flow.Score)
	}
	callers, ok := factorByAxis(res.Factors, "callers")
	if !ok {
		t.Fatal("callers factor missing")
	}
	if callers.Score < 50 {
		t.Fatalf("expected high caller axis for a 25-caller hub, got %.1f", callers.Score)
	}
	// A 25-caller, untested hub should land at least HIGH.
	if res.Risk != RiskHigh && res.Risk != RiskCritical {
		t.Fatalf("expected >=HIGH for a wide-fan-in untested hub, got %s (score %.1f)", res.Risk, res.Score)
	}
}

func TestScorePRRisk_SecurityKeywordRaisesRisk(t *testing.T) {
	// A small change with no callers, but in an auth path.
	symID := "internal/auth/login.go::ValidateToken"
	g := graph.New()
	g.AddNode(&graph.Node{ID: symID, Kind: graph.KindFunction, Name: "ValidateToken", FilePath: "internal/auth/login.go", StartLine: 1, EndLine: 5})

	res := ScorePRRisk(g, PRRiskInput{
		SymbolIDs:    []string{symID},
		ChangedFiles: []string{"internal/auth/login.go"},
	})

	if len(res.SecurityHits) == 0 {
		t.Fatalf("expected security hits for an auth path, got none")
	}
	sec, ok := factorByAxis(res.Factors, "security")
	if !ok {
		t.Fatal("security factor missing")
	}
	if sec.Score < 70 {
		t.Fatalf("expected high security axis, got %.1f", sec.Score)
	}
	if res.Risk != RiskHigh && res.Risk != RiskCritical {
		t.Fatalf("expected >=HIGH when a changed path is security-sensitive, got %s (score %.1f)", res.Risk, res.Score)
	}
}

func TestScorePRRisk_ZeroImpactIsLow(t *testing.T) {
	// A leaf symbol with no callers, no security keyword, and a covering
	// test should read as LOW.
	symID := "pkg/util.go::Helper"
	g := graph.New()
	g.AddNode(&graph.Node{ID: symID, Kind: graph.KindFunction, Name: "Helper", FilePath: "pkg/util.go", StartLine: 1, EndLine: 5})
	g.AddNode(&graph.Node{ID: "pkg/util_test.go::TestHelper", Kind: graph.KindFunction, Name: "TestHelper", FilePath: "pkg/util_test.go"})
	g.AddEdge(&graph.Edge{From: "pkg/util_test.go::TestHelper", To: symID, Kind: graph.EdgeTests})

	res := ScorePRRisk(g, PRRiskInput{
		SymbolIDs:    []string{symID},
		ChangedFiles: []string{"pkg/util.go"},
	})

	if res.Risk != RiskLow {
		t.Fatalf("expected LOW for a covered leaf change, got %s (score %.1f)", res.Risk, res.Score)
	}
	if res.UncoveredSymbols != 0 {
		t.Fatalf("expected 0 uncovered symbols, got %d", res.UncoveredSymbols)
	}
}

func TestScorePRRisk_FactorsNonEmptyAndSortedDesc(t *testing.T) {
	hubID := "pkg/hub.go::Hub"
	g := fanInGraph(t, hubID, "internal/auth/hub.go", 10)

	res := ScorePRRisk(g, PRRiskInput{
		SymbolIDs:    []string{hubID},
		ChangedFiles: []string{"internal/auth/hub.go"},
	})

	if len(res.Factors) == 0 {
		t.Fatal("review_priorities must be non-empty")
	}
	if !sort.SliceIsSorted(res.Factors, func(i, j int) bool {
		return res.Factors[i].Score > res.Factors[j].Score
	}) {
		t.Fatalf("review_priorities must be sorted descending by score: %+v", res.Factors)
	}
	// Every factor must carry a non-empty reason.
	for _, f := range res.Factors {
		if f.Reason == "" {
			t.Fatalf("factor %s has empty reason", f.Axis)
		}
	}
}

func TestScorePRRisk_UncoveredCounted(t *testing.T) {
	covered := "pkg/a.go::Covered"
	uncovered := "pkg/b.go::Uncovered"
	g := graph.New()
	g.AddNode(&graph.Node{ID: covered, Kind: graph.KindFunction, Name: "Covered", FilePath: "pkg/a.go"})
	g.AddNode(&graph.Node{ID: uncovered, Kind: graph.KindFunction, Name: "Uncovered", FilePath: "pkg/b.go"})
	g.AddNode(&graph.Node{ID: "pkg/a_test.go::TestCovered", Kind: graph.KindFunction, Name: "TestCovered", FilePath: "pkg/a_test.go"})
	g.AddEdge(&graph.Edge{From: "pkg/a_test.go::TestCovered", To: covered, Kind: graph.EdgeTests})

	res := ScorePRRisk(g, PRRiskInput{
		SymbolIDs:    []string{covered, uncovered},
		ChangedFiles: []string{"pkg/a.go", "pkg/b.go"},
	})
	if res.UncoveredSymbols != 1 {
		t.Fatalf("expected exactly 1 uncovered symbol, got %d", res.UncoveredSymbols)
	}
}

func TestSecurityKeywordHits(t *testing.T) {
	hits := securityKeywordHits(
		[]string{"internal/auth/session.go", "pkg/util.go"},
		[]string{"EncryptPayload", "PlainHelper"},
	)
	got := map[string]bool{}
	for _, h := range hits {
		got[h] = true
	}
	for _, want := range []string{"auth", "session", "encrypt"} {
		if !got[want] {
			t.Fatalf("expected hit %q in %v", want, hits)
		}
	}
	// No false positives from a clean path/name.
	if len(securityKeywordHits([]string{"pkg/util.go"}, []string{"Helper"})) != 0 {
		t.Fatal("expected no hits for clean inputs")
	}
}
