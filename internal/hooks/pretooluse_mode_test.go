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
		"":                 ModeDeny,
		"deny":             ModeDeny,
		"DENY":             ModeDeny,
		"  deny  ":         ModeDeny,
		"enrich":           ModeEnrich,
		"ENRICH":           ModeEnrich,
		"consult-unlock":   ModeConsultUnlock,
		"Consult-Unlock":   ModeConsultUnlock,
		"  consult-unlock": ModeConsultUnlock,
		"unknown":          ModeDeny,
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
	if ModeConsultUnlock.String() != "consult-unlock" {
		t.Errorf("ModeConsultUnlock.String() = %q, want \"consult-unlock\"", ModeConsultUnlock.String())
	}
}

// decodeHookOutput unmarshals captured stdout into a HookOutput,
// failing the test on malformed JSON.
func decodeHookOutput(t *testing.T, out string) HookOutput {
	t.Helper()
	var dec HookOutput
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	return dec
}

// TestRunPreToolUse_ConsultUnlock_DeniesBeforeConsult verifies that
// under ModeConsultUnlock a fallback Read of indexed source is hard-
// denied while the session has not yet queried the Gortex graph, and
// the deny reason explains how to unlock.
func TestRunPreToolUse_ConsultUnlock_DeniesBeforeConsult(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"cu-1","tool_input":{"file_path":"/repo/handler.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeConsultUnlock) })
	if out == "" {
		t.Fatal("expected JSON output before consult")
	}
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil {
		t.Fatal("missing hookSpecificOutput")
	}
	if dec.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected hard deny before consult, got %q", dec.HookSpecificOutput.PermissionDecision)
	}
	if !strings.Contains(dec.HookSpecificOutput.PermissionDecisionReason, "mcp__gortex__") {
		t.Errorf("deny reason should tell the agent to query the graph, got:\n%s",
			dec.HookSpecificOutput.PermissionDecisionReason)
	}
}

// TestRunPreToolUse_ConsultUnlock_AllowsAfterConsult verifies that once
// a mcp__gortex__* call has recorded the per-session marker, the same
// fallback Read is downgraded from a deny to additionalContext.
func TestRunPreToolUse_ConsultUnlock_AllowsAfterConsult(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	// Step 1: the agent queries the Gortex graph. The hook records the
	// marker and emits nothing (no-op pass-through).
	consult := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__search_symbols","session_id":"cu-2","tool_input":{"query":"Foo"}}`)
	consultOut := captureStdout(t, func() { runPreToolUse(consult, port, ModeConsultUnlock) })
	if consultOut != "" {
		t.Errorf("gortex MCP call should be a silent no-op, got: %q", consultOut)
	}
	if !loadSessionState("cu-2").GraphConsulted {
		t.Fatal("expected GraphConsulted marker to be set after a mcp__gortex__* call")
	}

	// Step 2: the previously-denied fallback Read is now downgraded.
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"cu-2","tool_input":{"file_path":"/repo/handler.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeConsultUnlock) })
	if out == "" {
		t.Fatal("expected JSON output after consult")
	}
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil {
		t.Fatal("missing hookSpecificOutput")
	}
	if dec.HookSpecificOutput.PermissionDecision == "deny" {
		t.Errorf("after consult the deny must be downgraded, got: %+v", dec.HookSpecificOutput)
	}
	if dec.HookSpecificOutput.AdditionalContext == "" {
		t.Error("downgraded result should still surface the graph guidance as additionalContext")
	}
}

// TestRunPreToolUse_ConsultUnlock_MarkerIsPerSession ensures one
// session consulting the graph does not unlock another session.
func TestRunPreToolUse_ConsultUnlock_MarkerIsPerSession(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{"/repo/handler.go": true})

	consult := []byte(`{"hook_event_name":"PreToolUse","tool_name":"mcp__gortex__get_symbol","session_id":"cu-A","tool_input":{}}`)
	_ = captureStdout(t, func() { runPreToolUse(consult, port, ModeConsultUnlock) })

	// A different session has NOT consulted — its fallback Read is
	// still hard-denied.
	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"cu-B","tool_input":{"file_path":"/repo/handler.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeConsultUnlock) })
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil || dec.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("session B never consulted; expected hard deny, got: %+v", dec.HookSpecificOutput)
	}
}

// TestRunPreToolUse_ConsultUnlock_SoftPathUnchanged confirms that an
// unindexed file (soft-guidance, not a deny) is unaffected by the
// consult-unlock posture regardless of marker state.
func TestRunPreToolUse_ConsultUnlock_SoftPathUnchanged(t *testing.T) {
	withSessionDir(t)
	port := fakeIndexedBridge(t, map[string]bool{}) // nothing indexed

	payload := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read","session_id":"cu-soft","tool_input":{"file_path":"/repo/unindexed.go"}}`)
	out := captureStdout(t, func() { runPreToolUse(payload, port, ModeConsultUnlock) })
	dec := decodeHookOutput(t, out)
	if dec.HookSpecificOutput == nil || dec.HookSpecificOutput.AdditionalContext == "" {
		t.Fatal("expected soft additionalContext to survive ModeConsultUnlock")
	}
	if dec.HookSpecificOutput.PermissionDecision == "deny" {
		t.Errorf("soft path must not become a deny, got %q", dec.HookSpecificOutput.PermissionDecision)
	}
}
