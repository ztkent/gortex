package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// indirectField returns true if some method named `caller` has an indirect
// accesses_field write edge to a field named `field` with the given `via`.
func indirectField(g *graph.Graph, caller, field, via string) bool {
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindMethod || n.Name != caller {
			continue
		}
		for _, e := range g.GetOutEdges(n.ID) {
			if e.Kind != graph.EdgeAccessesField || e.Meta == nil {
				continue
			}
			if ind, _ := e.Meta["indirect"].(bool); !ind {
				continue
			}
			fn := g.GetNode(e.To)
			if fn == nil || fn.Name != field {
				continue
			}
			if via == "" || e.Meta["via"] == via {
				return true
			}
		}
	}
	return false
}

func TestIndirectMutation_FieldMethodCall(t *testing.T) {
	g := indexFixture(t, map[string]string{
		"m.go": `package main

type Counter struct{ n int }

func (c *Counter) Increment() { c.n++ }

type Server struct {
	counter Counter
}

func (s *Server) Tick() { s.counter.Increment() }
`,
	})
	if !indirectField(g, "Tick", "counter", "Increment") {
		t.Errorf("expected Tick to indirectly mutate counter via Increment")
	}
}

func TestIndirectMutation_SiblingTransitive(t *testing.T) {
	// gograph explicitly defers this transitive case; gortex ships it.
	g := indexFixture(t, map[string]string{
		"m.go": `package main

type Server struct {
	running int
}

func (s *Server) doWork() { s.running = 1 }

func (s *Server) Run() { s.doWork() }
`,
	})
	if !indirectField(g, "Run", "running", "doWork") {
		t.Errorf("expected Run to transitively mutate running via doWork (sibling call)")
	}
}

func TestIndirectMutation_TransitiveFieldChain(t *testing.T) {
	// Increment mutates inner via Bump (field-method), so a caller that does
	// s.counter.Increment() must still see counter as mutated — the fixpoint
	// must mark Increment a mutator transitively.
	g := indexFixture(t, map[string]string{
		"m.go": `package main

type Leaf struct{ v int }

func (l *Leaf) Bump() { l.v++ }

type Counter struct{ inner Leaf }

func (c *Counter) Increment() { c.inner.Bump() }

type Server struct{ counter Counter }

func (s *Server) Tick() { s.counter.Increment() }
`,
	})
	if !indirectField(g, "Increment", "inner", "Bump") {
		t.Errorf("expected Increment to mutate inner via Bump")
	}
	if !indirectField(g, "Tick", "counter", "Increment") {
		t.Errorf("expected Tick to transitively mutate counter via Increment")
	}
}

func TestIndirectMutation_StdlibAllowlist(t *testing.T) {
	g := indexFixture(t, map[string]string{
		"m.go": `package main

import "sync/atomic"

type Server struct {
	running atomic.Bool
}

func (s *Server) Start() { s.running.Store(true) }
`,
	})
	if !indirectField(g, "Start", "running", "Store") {
		t.Errorf("expected Start to indirectly mutate running via Store (stdlib allowlist)")
	}
}

func TestIndirectMutation_NegativeLocalReceiver(t *testing.T) {
	// A call on a local variable (not the receiver) must NOT be attributed to
	// a receiver field.
	g := indexFixture(t, map[string]string{
		"m.go": `package main

type Counter struct{ n int }

func (c *Counter) Increment() { c.n++ }

type Server struct{ counter Counter }

func (s *Server) Stray() {
	local := Counter{}
	local.Increment()
}
`,
	})
	if indirectField(g, "Stray", "counter", "") {
		t.Errorf("local.Increment() must not be attributed to the receiver field counter")
	}
}
