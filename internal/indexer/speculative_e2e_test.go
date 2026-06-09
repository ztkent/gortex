package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func speculativeCallExists(g *graph.Graph, callerName, targetName string) bool {
	for _, n := range g.AllNodes() {
		if n.Name != callerName {
			continue
		}
		for _, e := range g.GetOutEdges(n.ID) {
			if e.Kind != graph.EdgeCalls || !e.IsSpeculative() {
				continue
			}
			if tn := g.GetNode(e.To); tn != nil && tn.Name == targetName {
				return true
			}
		}
	}
	return false
}

// TestSpeculativeDispatch_EndToEnd indexes a Python computed-member dispatch
// (obj["run"]()) with speculative synthesis enabled and asserts a best-guess
// edge is minted to the same-name method — and that it is NOT minted when the
// feature is off (default).
func TestSpeculativeDispatch_EndToEnd(t *testing.T) {
	fixture := map[string]string{
		"m.py": `class Worker:
    def run(self):
        pass

def dispatch(table):
    table["run"]()
`,
	}

	// Off by default → no speculative edge.
	t.Run("default_off", func(t *testing.T) {
		g := indexFixture(t, fixture)
		if speculativeCallExists(g, "dispatch", "run") {
			t.Errorf("speculative edge must NOT exist when synthesis is disabled (default)")
		}
	})

	// Enabled via env override → speculative edge minted.
	t.Run("enabled", func(t *testing.T) {
		t.Setenv("GORTEX_SYNTH_SPECULATIVE", "1")
		g := indexFixture(t, fixture)
		if !speculativeCallExists(g, "dispatch", "run") {
			t.Errorf("expected a speculative dispatch->run edge when synthesis is enabled")
		}
	})
}
