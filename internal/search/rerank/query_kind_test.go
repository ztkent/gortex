package rerank

import "testing"

func TestIsSymbolQuery_PositiveCases(t *testing.T) {
	cases := []string{
		// CamelCase / PascalCase
		"FooBar",
		"validateToken",
		"HTTPServer",
		// snake_case
		"validate_user_token",
		"my_var",
		// Namespace qualifiers
		"pkg.Type",
		"Module::Symbol",
		"path/to/file.go",
		"path\\to\\file",
		// All-uppercase acronyms (length >= 2)
		"URL",
		"JWT",
		"API",
	}
	for _, q := range cases {
		if !IsSymbolQuery(q) {
			t.Errorf("IsSymbolQuery(%q) = false, want true", q)
		}
	}
}

func TestIsSymbolQuery_NegativeCases(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"validate user token",  // multi-word NL
		"the user manager",     // multi-word NL
		"what does this do",    // multi-word NL
		"http server",          // multi-word despite identifier-shaped tokens
		"single",               // single lowercase word — too ambiguous
		"a",                    // single letter
		"A",                    // single uppercase letter (still too ambiguous)
		"42",                   // pure digits
		"...",                  // pure punctuation
	}
	for _, q := range cases {
		if IsSymbolQuery(q) {
			t.Errorf("IsSymbolQuery(%q) = true, want false", q)
		}
	}
}

func TestAlphaFor_SymbolVsNL(t *testing.T) {
	if got := AlphaFor("FooBar"); got != AlphaSymbol {
		t.Errorf("AlphaFor(symbol) = %v, want %v", got, AlphaSymbol)
	}
	if got := AlphaFor("what does this do"); got != AlphaNL {
		t.Errorf("AlphaFor(NL) = %v, want %v", got, AlphaNL)
	}
	// Empty falls through to NL — defensible default for nil queries.
	if got := AlphaFor(""); got != AlphaNL {
		t.Errorf("AlphaFor(empty) = %v, want %v", got, AlphaNL)
	}
}

func TestAlphaSymbolLessThanAlphaNL(t *testing.T) {
	// Sanity: symbol α MUST be smaller than NL α so the auto-blend
	// actually leans toward BM25 for identifier queries.
	if !(AlphaSymbol < AlphaNL) {
		t.Fatalf("AlphaSymbol (%v) must be less than AlphaNL (%v)", AlphaSymbol, AlphaNL)
	}
}
