package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRunPreToolUse_EnrichModeDowngradesDeny verifies that in ModeEnrich
// the same input that produces a deny in ModeDeny is downgraded to an
// additionalContext payload — the agent's call is never blocked, but
// the deny rationale still surfaces so the agent learns the graph
// alternative.
func TestRunPreToolUse_EnrichModeDowngradesDeny(t *testing.T) {
	withEditBlocking(t, true)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/repo/handler.go"}}`)

	t.Run("deny mode → permission decision = deny", func(t *testing.T) {
		out := captureStdout(t, func() { runPreToolUse(payload, port, ModeDeny) })
		if out == "" {
			t.Fatal("expected JSON output")
		}
		var dec HookOutput
		if err := json.Unmarshal([]byte(out), &dec); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		if dec.HookSpecificOutput == nil {
			t.Fatal("missing hookSpecificOutput")
		}
		if dec.HookSpecificOutput.PermissionDecision != "deny" {
			t.Errorf("expected deny, got: %q", dec.HookSpecificOutput.PermissionDecision)
		}
		if dec.HookSpecificOutput.AdditionalContext != "" {
			t.Errorf("deny mode must NOT also emit additionalContext, got: %q", dec.HookSpecificOutput.AdditionalContext)
		}
	})

	t.Run("enrich mode → additionalContext, no deny", func(t *testing.T) {
		out := captureStdout(t, func() { runPreToolUse(payload, port, ModeEnrich) })
		if out == "" {
			t.Fatal("expected JSON output")
		}
		var dec HookOutput
		if err := json.Unmarshal([]byte(out), &dec); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		if dec.HookSpecificOutput == nil {
			t.Fatal("missing hookSpecificOutput")
		}
		if dec.HookSpecificOutput.PermissionDecision == "deny" {
			t.Errorf("enrich mode must NEVER deny; got: %+v", dec.HookSpecificOutput)
		}
		if dec.HookSpecificOutput.AdditionalContext == "" {
			t.Error("enrich mode should surface the deny rationale as additionalContext")
		}
		// The downgraded message should still reference the graph
		// alternative (edit_symbol/edit_file/etc) so the agent learns
		// without being blocked.
		if !strings.Contains(dec.HookSpecificOutput.AdditionalContext, "edit_symbol") {
			t.Errorf("downgraded context should retain graph alternative hints, got:\n%s",
				dec.HookSpecificOutput.AdditionalContext)
		}
	})
}

// TestRunPreToolUse_EnrichModePreservesSoftContext makes sure non-deny
// soft guidance (the "PREFER graph tools" tip on unindexed source) is
// unchanged in ModeEnrich — only deny responses are downgraded.
func TestRunPreToolUse_EnrichModePreservesSoftContext(t *testing.T) {
	port := fakeIndexedBridge(t, map[string]bool{}) // nothing indexed → soft path

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"/repo/unindexed.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeEnrich) })
	if out == "" {
		t.Fatal("expected JSON output for soft context")
	}
	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if dec.HookSpecificOutput == nil || dec.HookSpecificOutput.AdditionalContext == "" {
		t.Fatal("expected soft additionalContext to survive ModeEnrich")
	}
	if dec.HookSpecificOutput.PermissionDecision == "deny" {
		t.Errorf("soft path must not become a deny — got: %q",
			dec.HookSpecificOutput.PermissionDecision)
	}
}

// TestParseMode covers every supported input plus the fallback for
// unknown / empty values.
func TestParseMode(t *testing.T) {
	cases := map[string]Mode{
		"":         ModeDeny,
		"deny":     ModeDeny,
		"DENY":     ModeDeny,
		"  deny  ": ModeDeny,
		"enrich":   ModeEnrich,
		"ENRICH":   ModeEnrich,
		"unknown":  ModeDeny,
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			if got := ParseMode(input); got != want {
				t.Errorf("ParseMode(%q) = %v, want %v", input, got, want)
			}
		})
	}
}

func TestModeString(t *testing.T) {
	if ModeDeny.String() != "deny" {
		t.Errorf("ModeDeny.String() = %q, want \"deny\"", ModeDeny.String())
	}
	if ModeEnrich.String() != "enrich" {
		t.Errorf("ModeEnrich.String() = %q, want \"enrich\"", ModeEnrich.String())
	}
}
