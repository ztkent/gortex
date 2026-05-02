package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestGoFlags_LaunchDarklyVariation(t *testing.T) {
	src := `package foo

type LDClient struct{}

func (c *LDClient) BoolVariation(key string, ctx any, def bool) bool { return false }

func Run(ld *LDClient) {
	if ld.BoolVariation("signup_v2", nil, false) {
		_ = "ok"
	}
}
`
	fix := runGoExtract(t, src)

	flags := fix.nodesByKind[graph.KindFlag]
	if len(flags) != 1 {
		t.Fatalf("expected 1 KindFlag, got %d: %+v", len(flags), flags)
	}
	if flags[0].ID != "flag::launchdarkly::signup_v2" {
		t.Errorf("flag id = %q", flags[0].ID)
	}
	if p, _ := flags[0].Meta["provider"].(string); p != "launchdarkly" {
		t.Errorf("provider meta = %q", p)
	}

	toggles := fix.edgesByKind[graph.EdgeTogglesFlag]
	if len(toggles) != 1 {
		t.Fatalf("expected 1 EdgeTogglesFlag, got %d", len(toggles))
	}
	if op, _ := toggles[0].Meta["op"].(string); op != "read" {
		t.Errorf("op meta = %q", op)
	}
}

func TestGoFlags_GrowthBookIsOn(t *testing.T) {
	src := `package foo

type GBClient struct{}

func (c *GBClient) IsOn(key string) bool { return false }

func Run(gb *GBClient) {
	gb.IsOn("new_pricing")
}
`
	fix := runGoExtract(t, src)
	flags := fix.nodesByKind[graph.KindFlag]
	if len(flags) != 1 {
		t.Fatalf("expected 1 flag, got %d", len(flags))
	}
	if flags[0].ID != "flag::growthbook::new_pricing" {
		t.Errorf("id = %q", flags[0].ID)
	}
}

func TestGoFlags_UnleashIsEnabled(t *testing.T) {
	src := `package foo

type UnleashClient struct{}

func (c *UnleashClient) IsEnabled(name string) bool { return false }

func Run(u *UnleashClient) {
	u.IsEnabled("dark_mode")
}
`
	fix := runGoExtract(t, src)
	flags := fix.nodesByKind[graph.KindFlag]
	if len(flags) != 1 {
		t.Fatalf("expected 1 flag, got %d", len(flags))
	}
	if flags[0].ID != "flag::unleash::dark_mode" {
		t.Errorf("id = %q", flags[0].ID)
	}
}

func TestGoFlags_DuplicateNameDeduplicates(t *testing.T) {
	src := `package foo

type GBClient struct{}

func (c *GBClient) IsOn(key string) bool { return false }

func A(gb *GBClient) { gb.IsOn("checkout_v2") }
func B(gb *GBClient) { gb.IsOn("checkout_v2") }
`
	fix := runGoExtract(t, src)
	flags := fix.nodesByKind[graph.KindFlag]
	if len(flags) != 1 {
		t.Errorf("expected 1 deduped flag node, got %d", len(flags))
	}
	if got := len(fix.edgesByKind[graph.EdgeTogglesFlag]); got != 2 {
		t.Errorf("expected 2 toggle edges (one per call site), got %d", got)
	}
}

func TestGoFlags_NonLiteralArgSkipped(t *testing.T) {
	src := `package foo

type GBClient struct{}

func (c *GBClient) IsOn(key string) bool { return false }

func Run(gb *GBClient, name string) {
	gb.IsOn(name)
}
`
	fix := runGoExtract(t, src)
	if got := len(fix.nodesByKind[graph.KindFlag]); got != 0 {
		t.Errorf("dynamic flag name should not produce a node, got %d", got)
	}
}

func TestGoFlags_NonFlagMethodIgnored(t *testing.T) {
	src := `package foo

type Cache struct{}

func (c *Cache) Get(key string) any { return nil }

func Run(c *Cache) {
	_ = c.Get("user_123")
}
`
	fix := runGoExtract(t, src)
	if got := len(fix.nodesByKind[graph.KindFlag]); got != 0 {
		t.Errorf("Cache.Get should not match flag heuristic, got %d", got)
	}
}
