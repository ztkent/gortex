package elide

import (
	"strings"
	"testing"
)

// TestCompressWith_DecideCompressVerdict asserts that a Decide function
// returning FidelityCompress reproduces the legacy body-stub behaviour
// for every declaration.
func TestCompressWith_DecideCompressVerdict(t *testing.T) {
	all := func(Decl) Fidelity { return FidelityCompress }
	out, err := CompressStringWith(keepFixtureGo, "go", Options{Decide: all})
	if err != nil {
		t.Fatalf("CompressStringWith: %v", err)
	}
	plain, err := CompressString(keepFixtureGo, "go")
	if err != nil {
		t.Fatalf("CompressString: %v", err)
	}
	if out != plain {
		t.Errorf("Decide=>FidelityCompress must reproduce Compress.\nDecide:\n%s\nCompress:\n%s", out, plain)
	}
	checkContains(t, out, []string{
		"func Alpha(x int) int",
		"func Beta(x int) int",
		"func Gamma() string",
		"lines elided",
	}, []string{
		"y := x + 1",
		"z := x * 2",
		`"gamma-body"`,
	})
}

// TestCompressWith_DecideFullVerdict asserts that a Decide function
// returning FidelityFull for one declaration leaves its whole body
// verbatim while every other body is still stubbed — i.e. Decide=>Full
// matches a Keep predicate.
func TestCompressWith_DecideFullVerdict(t *testing.T) {
	decide := func(d Decl) Fidelity {
		if d.Name == "Beta" {
			return FidelityFull
		}
		return FidelityCompress
	}
	out, err := CompressStringWith(keepFixtureGo, "go", Options{Decide: decide})
	if err != nil {
		t.Fatalf("CompressStringWith: %v", err)
	}
	checkContains(t, out, []string{
		"func Beta(x int) int",
		"z := x * 2", // Beta kept verbatim
		"return z",
		"lines elided", // Alpha + Gamma stubbed
	}, []string{
		"y := x + 1",   // Alpha elided
		`"gamma-body"`, // Gamma elided
	})
}

// TestCompressWith_DecideOmitVerdict asserts that a Decide function
// returning FidelityOmit removes the whole declaration — signature and
// body both — behind a one-line `// <name> omitted` marker, while the
// other declarations are still present (and body-stubbed).
func TestCompressWith_DecideOmitVerdict(t *testing.T) {
	decide := func(d Decl) Fidelity {
		if d.Name == "Alpha" {
			return FidelityOmit
		}
		return FidelityCompress
	}
	out, err := CompressStringWith(keepFixtureGo, "go", Options{Decide: decide})
	if err != nil {
		t.Fatalf("CompressStringWith: %v", err)
	}
	checkContains(t, out, []string{
		"// Alpha omitted",          // omit marker present
		"func Beta(x int) int",      // Beta signature survives
		"func Gamma() string",       // Gamma signature survives
		"lines elided",              // Beta/Gamma bodies stubbed
	}, []string{
		"func Alpha(x int) int", // Alpha signature gone
		"y := x + 1",            // Alpha body gone
		"return y",              // Alpha body gone
	})
	// The marker must be a real comment so the output still parses.
	if !strings.Contains(out, "// Alpha omitted") {
		t.Fatalf("expected omit marker, got:\n%s", out)
	}
}

// TestCompressWith_KeepOverridesDecide asserts the back-compat layering:
// a Keep predicate that fires forces FidelityFull even when Decide
// would have omitted or compressed the same declaration.
func TestCompressWith_KeepOverridesDecide(t *testing.T) {
	out, err := CompressStringWith(keepFixtureGo, "go", Options{
		Keep:   KeepNames([]string{"Alpha"}),
		Decide: func(Decl) Fidelity { return FidelityOmit },
	})
	if err != nil {
		t.Fatalf("CompressStringWith: %v", err)
	}
	checkContains(t, out, []string{
		"func Alpha(x int) int", // Keep wins -> full, not omitted
		"y := x + 1",
		"return y",
		"// Beta omitted",  // Beta has no Keep -> Decide omits it
		"// Gamma omitted", // Gamma has no Keep -> Decide omits it
	}, []string{
		"// Alpha omitted", // Alpha was NOT omitted
	})
}

// TestCompressWith_OmitMarkerCommentStyle asserts the omit marker uses
// the host language's line-comment syntax: `#` for Ruby/Python rather
// than `//`.
func TestCompressWith_OmitMarkerCommentStyle(t *testing.T) {
	const rubySrc = "class Svc\n  def alpha(x)\n    y = x + 1\n    y\n  end\n\n  def beta(x)\n    z = x * 2\n    z\n  end\nend\n"
	decide := func(d Decl) Fidelity {
		if d.Name == "alpha" {
			return FidelityOmit
		}
		return FidelityCompress
	}
	out, err := CompressStringWith(rubySrc, "ruby", Options{Decide: decide})
	if err != nil {
		t.Fatalf("CompressStringWith ruby: %v", err)
	}
	if !strings.Contains(out, "# alpha omitted") {
		t.Fatalf("expected ruby '#' omit marker, got:\n%s", out)
	}
	if strings.Contains(out, "// alpha omitted") {
		t.Fatalf("ruby omit marker must use '#', not '//':\n%s", out)
	}
	if strings.Contains(out, "y = x + 1") {
		t.Fatalf("omitted ruby method body should be gone:\n%s", out)
	}
	// Beta survives, compressed.
	if !strings.Contains(out, "def beta(x)") {
		t.Fatalf("beta should survive:\n%s", out)
	}
}
