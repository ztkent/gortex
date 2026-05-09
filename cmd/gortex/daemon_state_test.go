package main

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/config"
)

// TestLSPDisabledSet_ConfigOnly — a `semantic.providers` entry with
// `enabled: false` whose name matches a known LSP spec lands in the
// disabled set. Entries with unknown names are ignored (so an
// `enabled: false` for a custom non-registry daemon doesn't shadow
// a same-named LSP).
func TestLSPDisabledSet_ConfigOnly(t *testing.T) {
	got := lspDisabledSet([]config.SemanticProviderConfig{
		{Name: "gopls", Enabled: false},
		{Name: "tsserver", Enabled: true}, // explicitly enabled — must NOT land in disabled
		{Name: "not-a-real-lsp", Enabled: false},
	}, "")
	want := map[string]bool{"gopls": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_EnvOnly — comma-separated names land in the
// disabled set. Whitespace is trimmed; empty entries are skipped.
func TestLSPDisabledSet_EnvOnly(t *testing.T) {
	got := lspDisabledSet(nil, "gopls, tsserver,, ,pyright")
	want := map[string]bool{"gopls": true, "tsserver": true, "pyright": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_EnvAllKillSwitch — the literal value "all" or
// "*" sets the special "__all__" key, signalling callers to skip
// auto-registration entirely.
func TestLSPDisabledSet_EnvAllKillSwitch(t *testing.T) {
	for _, env := range []string{"all", "ALL", "*", " all "} {
		got := lspDisabledSet(nil, env)
		if !got["__all__"] {
			t.Fatalf("env=%q: expected __all__ kill switch, got %v", env, got)
		}
	}
}

// TestLSPDisabledSet_ConfigAndEnvMerge — disables from both sources
// merge cleanly into one map.
func TestLSPDisabledSet_ConfigAndEnvMerge(t *testing.T) {
	got := lspDisabledSet([]config.SemanticProviderConfig{
		{Name: "gopls", Enabled: false},
	}, "tsserver,pyright")
	want := map[string]bool{
		"gopls":    true,
		"tsserver": true,
		"pyright":  true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_Empty — no providers, empty env yields an empty
// map (not nil — callers index into it).
func TestLSPDisabledSet_Empty(t *testing.T) {
	got := lspDisabledSet(nil, "")
	if got == nil {
		t.Fatal("expected non-nil empty map")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}
