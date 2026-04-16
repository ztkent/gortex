package excludes

import (
	"testing"
)

func TestMatcher_Nil(t *testing.T) {
	var m *Matcher
	if m.MatchRel("anything") {
		t.Fatal("nil matcher should never match")
	}
}

func TestMatcher_Builtin(t *testing.T) {
	m := New(Builtin)
	cases := []struct {
		path string
		want bool
	}{
		{".git/HEAD", true},
		{"pkg/.git/HEAD", true},
		{"src/node_modules/foo/index.js", true},
		{"vendor/lib/x.go", true},
		{"pkg/foo.go", false},
		{"README.md", false},
		{"tmp.tmp", true},
		{"deep/nested/file.swp", true},
	}
	for _, tc := range cases {
		if got := m.MatchRel(tc.path); got != tc.want {
			t.Errorf("MatchRel(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestMatcher_Negation(t *testing.T) {
	// Exclude all of dist, except dist/keep
	m := New([]string{"dist/", "!dist/keep/**"})
	if !m.MatchRel("dist/junk.txt") {
		t.Error("dist/junk.txt should be excluded")
	}
	if m.MatchRel("dist/keep/foo.txt") {
		t.Error("dist/keep/foo.txt should be re-included")
	}
}

func TestMatcher_CommentsAndEmpty(t *testing.T) {
	m := New([]string{"", "# comment", "foo/"})
	pats := m.Patterns()
	if len(pats) != 1 || pats[0] != "foo/" {
		t.Errorf("expected [foo/], got %v", pats)
	}
}
