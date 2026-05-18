package rerank

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestClassifyPathPenalty_Tiers(t *testing.T) {
	cases := []struct {
		name string
		path string
		want float64
	}{
		// --- Test tier (×0.3) ---
		{"go _test.go", "auth/token_test.go", PathPenaltyTest},
		{"python test_ prefix", "tests/test_auth.py", PathPenaltyTest},
		{"python _test suffix", "auth_test.py", PathPenaltyTest},
		{"ruby _spec.rb", "auth_spec.rb", PathPenaltyTest},
		{"ruby _test.rb", "auth_test.rb", PathPenaltyTest},
		{"js .spec.js", "src/auth.spec.js", PathPenaltyTest},
		{"ts .test.ts", "src/auth.test.ts", PathPenaltyTest},
		{"tsx .spec.tsx", "src/Comp.spec.tsx", PathPenaltyTest},
		{"swift Tests suffix", "AuthTests.swift", PathPenaltyTest},
		{"java Test suffix", "AuthTest.java", PathPenaltyTest},
		{"kotlin Test suffix", "AuthTest.kt", PathPenaltyTest},
		{"tests dir", "tests/anything.go", PathPenaltyTest},
		{"__tests__ dir (jest)", "src/__tests__/comp.js", PathPenaltyTest},
		{"e2e dir", "e2e/login.ts", PathPenaltyTest},
		{"fixtures dir", "fixtures/seed.go", PathPenaltyTest},
		{"testdata dir", "internal/foo/testdata/x.json", PathPenaltyTest},

		// --- Compatibility tier (×0.5) ---
		{"compat dir", "compat/old_api.go", PathPenaltyCompat},
		{"legacy dir", "src/legacy/router.js", PathPenaltyCompat},
		{"polyfill dir", "polyfill/fetch.js", PathPenaltyCompat},
		{"shim dir", "shim/socket.ts", PathPenaltyCompat},
		{"backport dir", "backport/feature.py", PathPenaltyCompat},
		{"deprecated dir", "deprecated/old.go", PathPenaltyCompat},

		// --- Examples tier (×0.5) ---
		{"examples dir", "examples/quickstart.go", PathPenaltyExamples},
		{"example dir (singular)", "example/main.go", PathPenaltyExamples},
		{"demo dir", "demo/app.go", PathPenaltyExamples},
		{"samples dir", "samples/hello.py", PathPenaltyExamples},
		{"playground dir", "playground/test.ts", PathPenaltyExamples},

		// --- Type declarations tier (×0.7) ---
		{"d.ts", "types/index.d.ts", PathPenaltyTypeDecl},
		{".pyi stub", "auth.pyi", PathPenaltyTypeDecl},
		{"include header", "src/include/foo.h", PathPenaltyTypeDecl},
		{"cpp header .hpp", "core.hpp", PathPenaltyTypeDecl},

		// --- Re-export barrel tier (×0.7) ---
		{"js index", "src/index.js", PathPenaltyReexport},
		{"ts index", "src/index.ts", PathPenaltyReexport},
		{"tsx index", "src/Layout/index.tsx", PathPenaltyReexport},
		{"py __init__", "pkg/__init__.py", PathPenaltyReexport},
		{"rust mod", "src/auth/mod.rs", PathPenaltyReexport},
		{"rust lib", "src/lib.rs", PathPenaltyReexport},

		// --- Uncatched / neutral (×1.0) ---
		{"plain go", "auth/token.go", PathPenaltyUncatched},
		{"plain py", "auth/token.py", PathPenaltyUncatched},
		{"plain rs", "src/auth/token.rs", PathPenaltyUncatched},
		{"empty path", "", PathPenaltyUncatched},
	}
	for _, c := range cases {
		got := classifyPathPenalty(c.path)
		if got != c.want {
			t.Errorf("%s: classifyPathPenalty(%q) = %v, want %v",
				c.name, c.path, got, c.want)
		}
	}
}

func TestClassifyPathPenalty_TestBeatsExample(t *testing.T) {
	// Overlap: a file at `tests/examples/foo.go` is a test fixture, not
	// an example. The most-aggressive penalty must win to avoid silent
	// compounding (×0.3 * ×0.5 = ×0.15 would over-penalize).
	got := classifyPathPenalty("tests/examples/foo.go")
	if got != PathPenaltyTest {
		t.Errorf("test+example overlap got %v, want %v (test wins)", got, PathPenaltyTest)
	}
}

func TestClassifyPathPenalty_NormalizesBackslashes(t *testing.T) {
	// Windows-style separators should still trip the regex.
	got := classifyPathPenalty(`src\__tests__\comp.js`)
	if got != PathPenaltyTest {
		t.Errorf("windows-path test got %v, want %v", got, PathPenaltyTest)
	}
}

func TestPathPenaltySignal_AppliedToCandidate(t *testing.T) {
	g := newTestGraph()
	prod := mustNode(g, "auth/token.go::Validate", "Validate", graph.KindFunction)
	prod.FilePath = "auth/token.go"
	test := mustNode(g, "auth/token_test.go::TestValidate", "TestValidate", graph.KindFunction)
	test.FilePath = "auth/token_test.go"

	cands := []*Candidate{candidateFor(prod, 0, -1), candidateFor(test, 1, -1)}
	ctx := &Context{Graph: g}
	ctx.prepare(cands)
	sig := PathPenaltySignal{}

	if got := sig.Contribute("Validate", cands[0], ctx); got != PathPenaltyUncatched {
		t.Errorf("production file got %v, want %v", got, PathPenaltyUncatched)
	}
	if got := sig.Contribute("Validate", cands[1], ctx); got != PathPenaltyTest {
		t.Errorf("test file got %v, want %v", got, PathPenaltyTest)
	}
}

func TestPathPenaltySignal_CacheReuses(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	a.FilePath = "src/legacy/old.go"
	b := mustNode(g, "f.go::B", "B", graph.KindFunction)
	b.FilePath = "src/legacy/old.go" // same path → cache hit on second call

	cands := []*Candidate{candidateFor(a, 0, -1), candidateFor(b, 1, -1)}
	ctx := &Context{Graph: g}
	ctx.prepare(cands)
	sig := PathPenaltySignal{}
	got1 := sig.Contribute("q", cands[0], ctx)
	got2 := sig.Contribute("q", cands[1], ctx)
	if got1 != PathPenaltyCompat || got2 != PathPenaltyCompat {
		t.Errorf("legacy-dir got %v, %v, want %v for both", got1, got2, PathPenaltyCompat)
	}
	if len(ctx.pathPenaltyCache) != 1 {
		t.Errorf("cache size = %d, want 1 (single distinct path)", len(ctx.pathPenaltyCache))
	}
}

func TestPathPenaltySignal_NilSafety(t *testing.T) {
	sig := PathPenaltySignal{}
	if got := sig.Contribute("q", nil, &Context{}); got != PathPenaltyUncatched {
		t.Errorf("nil candidate got %v, want %v (neutral)", got, PathPenaltyUncatched)
	}
	if got := sig.Contribute("q", &Candidate{}, &Context{}); got != PathPenaltyUncatched {
		t.Errorf("nil node got %v, want %v (neutral)", got, PathPenaltyUncatched)
	}
	if got := sig.Contribute("q", &Candidate{Node: &graph.Node{FilePath: ""}}, nil); got != PathPenaltyUncatched {
		t.Errorf("nil context got %v, want %v (neutral)", got, PathPenaltyUncatched)
	}
}
