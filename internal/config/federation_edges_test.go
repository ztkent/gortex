package config

import (
	"testing"
	"time"
)

func TestFederationEdges_DefaultsAndEnv(t *testing.T) {
	var c FederationEdgesConfig

	// Defaults when unset.
	if c.IsEnabled() {
		t.Error("edges must be off by default")
	}
	if c.TTL() != 5*time.Minute {
		t.Errorf("default TTL = %v, want 5m", c.TTL())
	}
	if c.MaxNodes() != 5000 {
		t.Errorf("default MaxNodes = %d, want 5000", c.MaxNodes())
	}
	if c.Depth() != 1 {
		t.Errorf("default Depth = %d, want 1", c.Depth())
	}

	// Explicit config values win.
	c = FederationEdgesConfig{Enabled: true, TTLMs: 1000, MaxProxyNodes: 7, HydrateDepth: 2}
	if !c.IsEnabled() || c.TTL() != time.Second || c.MaxNodes() != 7 || c.Depth() != 2 {
		t.Errorf("explicit config not honoured: %+v", c)
	}

	// Env override flips Enabled regardless of the field.
	t.Setenv("GORTEX_FEDERATION_EDGES", "1")
	if !(FederationEdgesConfig{}).IsEnabled() {
		t.Error("GORTEX_FEDERATION_EDGES=1 must enable edges")
	}
	t.Setenv("GORTEX_FEDERATION_EDGES", "0")
	if (FederationEdgesConfig{Enabled: true}).IsEnabled() {
		t.Error("GORTEX_FEDERATION_EDGES=0 must override an enabled config")
	}
}
