package review

import "testing"

// TestIdentityKeyStableAcrossLineShift proves the identity key is invariant to
// line-number drift: the same flagged code at line 10 and line 40 yields the
// same key, so a dismissed finding stays dismissed after the file shifts.
func TestIdentityKeyStableAcrossLineShift(t *testing.T) {
	a := Finding{
		Rule:       "nil-deref",
		Category:   "nil-deref",
		File:       "pkg/x.go",
		SymbolID:   "pkg/x.go::Foo",
		Line:       10,
		SourceLine: "\tresult := obj.Field()",
	}
	b := a
	b.Line = 40 // only the line moved

	ka, kb := IdentityKey(a), IdentityKey(b)
	if ka != kb {
		t.Fatalf("identity must survive line drift: %s != %s", ka, kb)
	}

	// Whitespace / indentation reflow of the same statement keeps the key.
	c := a
	c.Line = 99
	c.SourceLine = "  result :=   obj.Field()  "
	if kc := IdentityKey(c); kc != ka {
		t.Fatalf("identity must survive whitespace reflow: %s != %s", kc, ka)
	}
}

// TestIdentityKeyDiffersOnContent proves the key changes when rule, category,
// symbol, file, or the flagged source text differs.
func TestIdentityKeyDiffersOnContent(t *testing.T) {
	base := Finding{
		Rule:       "nil-deref",
		Category:   "nil-deref",
		File:       "pkg/x.go",
		SymbolID:   "pkg/x.go::Foo",
		Line:       10,
		SourceLine: "result := obj.Field()",
	}
	baseKey := IdentityKey(base)

	cases := map[string]func(*Finding){
		"different rule":     func(f *Finding) { f.Rule = "check-then-act" },
		"different category": func(f *Finding) { f.Category = "concurrency" },
		"different symbol":   func(f *Finding) { f.SymbolID = "pkg/x.go::Bar" },
		"different file":     func(f *Finding) { f.File = "pkg/y.go" },
		"different source":   func(f *Finding) { f.SourceLine = "other := thing.Call()" },
	}
	for name, mutate := range cases {
		f := base
		mutate(&f)
		if k := IdentityKey(f); k == baseKey {
			t.Fatalf("%s: key must differ from base, both %s", name, k)
		}
	}
}

// TestIdentityKeyPathNormalization proves equivalent path spellings collapse to
// one key (so "./pkg/x.go" and "pkg/x.go" do not produce two suppressions).
func TestIdentityKeyPathNormalization(t *testing.T) {
	a := Finding{Rule: "r", Category: "c", File: "pkg/x.go", SymbolID: "s", SourceLine: "x"}
	b := a
	b.File = "./pkg/x.go"
	if IdentityKey(a) != IdentityKey(b) {
		t.Fatalf("equivalent path spellings must produce one identity key")
	}
}

// TestIdentityKeyFallbackWithoutSource proves a finding with no source line
// still gets a stable key derived from rule + category + symbol + file, and that
// two findings differing only in (absent) source share the coarse fallback key.
func TestIdentityKeyFallbackWithoutSource(t *testing.T) {
	a := Finding{Rule: "r", Category: "c", File: "pkg/x.go", SymbolID: "pkg/x.go::Foo"}
	k1 := IdentityKey(a)
	if k1 == "" {
		t.Fatal("fallback key must be non-empty")
	}
	// Deterministic: recomputing yields the same key.
	if IdentityKey(a) != k1 {
		t.Fatal("fallback key must be deterministic")
	}
	// A different symbol still separates the coarse keys.
	b := a
	b.SymbolID = "pkg/x.go::Bar"
	if IdentityKey(b) == k1 {
		t.Fatal("fallback key must still separate distinct symbols")
	}
}
