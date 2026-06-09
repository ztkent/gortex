package review

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFindingUnionFields constructs a Finding with every union field set,
// proving the single type compiles with the full field set at this commit.
func TestFindingUnionFields(t *testing.T) {
	f := Finding{
		ID:          "f1",
		IdentityKey: "k1",
		Rule:        "nil-deref",
		Severity:    SevCritical,
		Category:    "nil-deref",
		CWE:         "CWE-476",
		Confidence:  0.9,
		Source:      "rulepack",
		SymbolID:    "pkg/x.go::Foo",
		File:        "pkg/x.go",
		Line:        14,
		StartLine:   14,
		EndLine:     16,
		Side:        "RIGHT",
		Anchor:      "exact_hunk",
		Message:     "possible nil dereference",
		Body:        "## Finding\nfull markdown body",
		Suggestion:  "guard with `if p != nil`",
		GenTokens:   42,
	}

	if f.Rule != "nil-deref" || f.Severity != SevCritical {
		t.Fatalf("field readback mismatch: %+v", f)
	}
}

// TestFindingSnakeCaseJSON asserts the JSON tags are snake_case and round-trip
// losslessly.
func TestFindingSnakeCaseJSON(t *testing.T) {
	f := Finding{
		ID:          "f1",
		IdentityKey: "k1",
		Rule:        "nil-deref",
		Severity:    SevError,
		Category:    "nil-deref",
		CWE:         "CWE-476",
		Confidence:  0.75,
		Source:      "llm",
		SymbolID:    "pkg/x.go::Foo",
		File:        "pkg/x.go",
		Line:        14,
		StartLine:   14,
		EndLine:     16,
		Side:        "RIGHT",
		Anchor:      "exact_hunk",
		Message:     "headline",
		Body:        "body",
		Suggestion:  "fix it",
		GenTokens:   7,
	}

	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)

	for _, want := range []string{
		`"identity_key":`, `"symbol_id":`, `"start_line":`, `"end_line":`,
		`"anchor_method":`, `"gen_tokens":`, `"cwe":`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected snake_case key %s in JSON, got %s", want, s)
		}
	}
	// camelCase / PascalCase keys must NOT appear.
	for _, bad := range []string{
		`"identityKey":`, `"symbolID":`, `"SymbolID":`, `"startLine":`, `"StartLine":`,
	} {
		if strings.Contains(s, bad) {
			t.Errorf("unexpected non-snake_case key %s in JSON: %s", bad, s)
		}
	}

	var back Finding
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != f {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", back, f)
	}
}

// TestFindingOmitempty confirms optional fields drop out of the JSON when zero.
func TestFindingOmitempty(t *testing.T) {
	f := Finding{
		Rule:     "r",
		Severity: SevInfo,
		Category: "c",
		SymbolID: "s",
		File:     "f.go",
		Line:     3,
		Message:  "m",
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, omitted := range []string{
		`"id":`, `"identity_key":`, `"cwe":`, `"source":`, `"start_line":`,
		`"end_line":`, `"side":`, `"anchor_method":`, `"body":`, `"suggestion":`,
		`"gen_tokens":`,
	} {
		if strings.Contains(s, omitted) {
			t.Errorf("expected omitempty key %s to be absent: %s", omitted, s)
		}
	}
	// Required keys always present.
	for _, present := range []string{
		`"rule":`, `"severity":`, `"category":`, `"confidence":`,
		`"symbol_id":`, `"file":`, `"line":`, `"message":`,
	} {
		if !strings.Contains(s, present) {
			t.Errorf("expected key %s present: %s", present, s)
		}
	}
}
