package review

import (
	"testing"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// TestClassifyChange_Classes drives every change class through ClassifyChange,
// asserting the precedence (test/config structural, then fix>refactor>feature on
// the hunk).
func TestClassifyChange_Classes(t *testing.T) {
	cases := []struct {
		name string
		sym  analysis.ChangedSymbol
		hunk string
		want string
	}{
		{
			name: "test file wins over hunk content",
			sym:  analysis.ChangedSymbol{ID: "a_test.go::TestFoo", Name: "TestFoo", Kind: "function", FilePath: "a_test.go"},
			hunk: "+func TestFoo() {}\n",
			want: ClassTest,
		},
		{
			name: "test symbol name wins",
			sym:  analysis.ChangedSymbol{ID: "a.go::TestBar", Name: "TestBar", Kind: "function", FilePath: "a.go"},
			hunk: "+\tx := 1\n",
			want: ClassTest,
		},
		{
			name: "yaml file is config",
			sym:  analysis.ChangedSymbol{ID: "deploy.yaml::x", Name: "x", FilePath: "config/deploy.yaml"},
			hunk: "+replicas: 3\n",
			want: ClassConfig,
		},
		{
			name: "constant node kind is config",
			sym:  analysis.ChangedSymbol{ID: "a.go::MaxN", Name: "MaxN", Kind: "constant", FilePath: "a.go"},
			hunk: "+const MaxN = 5\n",
			want: ClassConfig,
		},
		{
			name: "added error handling is a fix",
			sym:  analysis.ChangedSymbol{ID: "a.go::Load", Name: "Load", Kind: "function", FilePath: "internal/svc/a.go"},
			hunk: "+\tif err != nil {\n+\t\treturn err\n+\t}\n",
			want: ClassFix,
		},
		{
			name: "equal add/remove is a refactor",
			sym:  analysis.ChangedSymbol{ID: "a.go::Sum", Name: "Sum", Kind: "function", FilePath: "a.go"},
			hunk: "-\treturn a + b\n+\ttotal := a + b\n",
			want: ClassRefactor,
		},
		{
			name: "net-new lines is a feature",
			sym:  analysis.ChangedSymbol{ID: "a.go::NewThing", Name: "NewThing", Kind: "function", FilePath: "a.go"},
			hunk: "+func NewThing() *Thing {\n+\treturn &Thing{}\n+}\n",
			want: ClassFeature,
		},
		{
			name: "no hunk falls back to feature",
			sym:  analysis.ChangedSymbol{ID: "a.go::Plain", Name: "Plain", Kind: "function", FilePath: "a.go"},
			hunk: "",
			want: ClassFeature,
		},
	}
	g := graph.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyChange(g, tc.sym, tc.hunk)
			if got != tc.want {
				t.Fatalf("ClassifyChange(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestVerificationCommand covers the per-toolchain command derivation and the
// whole-tree fallback.
func TestVerificationCommand(t *testing.T) {
	cases := []struct {
		name    string
		targets []string
		lang    string
		want    string
	}{
		{
			name:    "go targets collapse to package dirs",
			targets: []string{"internal/svc/handler_test.go", "internal/svc/other_test.go"},
			lang:    "go",
			want:    "go test ./internal/svc/...",
		},
		{
			name:    "go multiple dirs sorted",
			targets: []string{"internal/b/b_test.go", "internal/a/a_test.go"},
			lang:    "go",
			want:    "go test ./internal/a/... ./internal/b/...",
		},
		{
			name:    "go no targets is whole module",
			targets: nil,
			lang:    "go",
			want:    "go test ./...",
		},
		{
			name:    "lang inferred from extension",
			targets: []string{"pkg/x/x_test.go"},
			lang:    "",
			want:    "go test ./pkg/x/...",
		},
		{
			name:    "python pytest",
			targets: []string{"tests/test_foo.py"},
			lang:    "python",
			want:    "pytest tests/test_foo.py",
		},
		{
			name:    "typescript npm test",
			targets: []string{"src/x.test.ts"},
			lang:    "",
			want:    "npm test",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VerificationCommand(tc.targets, tc.lang)
			if got != tc.want {
				t.Fatalf("VerificationCommand(%v,%q) = %q, want %q", tc.targets, tc.lang, got, tc.want)
			}
		})
	}
}
