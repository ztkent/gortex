package trigram

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func mk(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func equalIDs(got, want []uint32) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestIndex_Candidates(t *testing.T) {
	ix := New()
	ix.Add(0, []byte("func validateToken() error"))
	ix.Add(1, []byte("func parseConfig() Config"))
	ix.Add(2, []byte("type Token struct{}"))

	if c := ix.Candidates("Token"); !equalIDs(c, []uint32{0, 2}) {
		t.Errorf("Candidates(Token) = %v, want [0 2]", c)
	}
	if c := ix.Candidates("parseConfig"); !equalIDs(c, []uint32{1}) {
		t.Errorf("Candidates(parseConfig) = %v, want [1]", c)
	}
	if c := ix.Candidates("zzzqqq"); len(c) != 0 {
		t.Errorf("Candidates(zzzqqq) = %v, want empty", c)
	}
}

func TestIndex_ShortQueryReturnsAll(t *testing.T) {
	ix := New()
	ix.Add(0, []byte("alpha"))
	ix.Add(1, []byte("beta"))
	// "ab" is shorter than a trigram — every doc is a candidate.
	if c := ix.Candidates("ab"); !equalIDs(c, []uint32{0, 1}) {
		t.Errorf("short-query candidates = %v, want [0 1]", c)
	}
}

func TestIndex_Remove(t *testing.T) {
	ix := New()
	ix.Add(0, []byte("func handler()"))
	ix.Add(1, []byte("func handler()"))
	if c := ix.Candidates("handler"); !equalIDs(c, []uint32{0, 1}) {
		t.Fatalf("before remove = %v", c)
	}
	ix.Remove(0)
	if c := ix.Candidates("handler"); !equalIDs(c, []uint32{1}) {
		t.Errorf("after remove = %v, want [1]", c)
	}
	if ix.DocCount() != 1 {
		t.Errorf("DocCount = %d, want 1", ix.DocCount())
	}
}

func TestIndex_ReAddReplaces(t *testing.T) {
	ix := New()
	ix.Add(0, []byte("original content here"))
	ix.Add(0, []byte("replacement payload")) // re-add the same docID

	if c := ix.Candidates("original"); len(c) != 0 {
		t.Errorf("stale content still matched: %v", c)
	}
	if c := ix.Candidates("replacement"); !equalIDs(c, []uint32{0}) {
		t.Errorf("new content not matched: %v", c)
	}
	if ix.DocCount() != 1 {
		t.Errorf("DocCount = %d, want 1 (re-add must not duplicate)", ix.DocCount())
	}
}

func TestSearcher_Grep(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a.go", "package a\n\nfunc Alpha() {}\n")
	mk(t, root, "sub/b.go", "package sub\n\nfunc Beta() {}\nfunc Alpha() {}\n")
	mk(t, root, "c.go", "package c\n\nvar X = 1\n")

	s := Build(root, []string{"a.go", "sub/b.go", "c.go"})
	if s.DocCount() != 3 {
		t.Fatalf("DocCount = %d, want 3", s.DocCount())
	}

	hits := s.Grep("func Alpha", 0)
	if len(hits) != 2 {
		t.Fatalf("Grep(func Alpha) = %d hits, want 2: %+v", len(hits), hits)
	}
	if hits[0].Path != "a.go" || hits[0].Line != 3 {
		t.Errorf("hit[0] = %+v, want a.go:3", hits[0])
	}
	if hits[1].Path != "sub/b.go" || hits[1].Line != 4 {
		t.Errorf("hit[1] = %+v, want sub/b.go:4", hits[1])
	}
}

func TestSearcher_GrepLimit(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "x.go", "match\nmatch\nmatch\nmatch\n")
	s := Build(root, []string{"x.go"})
	if hits := s.Grep("match", 2); len(hits) != 2 {
		t.Errorf("Grep with limit 2 returned %d hits", len(hits))
	}
}

func TestSearcher_GrepVerifiesTrigramCandidate(t *testing.T) {
	// "abcab" carries every trigram of the query "abcabc" (abc, bca,
	// cab), so it survives the trigram filter — the literal scan must
	// still reject it because the substring is not actually present.
	root := t.TempDir()
	mk(t, root, "f.go", "the abcab pattern\n")
	s := Build(root, []string{"f.go"})
	if hits := s.Grep("abcabc", 0); len(hits) != 0 {
		t.Errorf("trigram false positive leaked past the literal scan: %+v", hits)
	}
}

func TestSearcher_GrepRegexp(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a.go", "package a\n\nfunc Alpha() {}\nfunc alphabet() {}\n")
	mk(t, root, "sub/b.go", "package sub\n\nfunc Beta() {}\n")
	mk(t, root, "c.go", "package c\n\nvar Counter = 1\n")
	s := Build(root, []string{"a.go", "sub/b.go", "c.go"})

	re := regexp.MustCompile(`func [A-Z]\w+\(\)`)
	hits := s.GrepRegexp(re, []string{"func "}, "", 0)
	// Matches: Alpha (a.go) and Beta (sub/b.go); alphabet starts lowercase.
	if len(hits) != 2 {
		t.Fatalf("GrepRegexp = %d hits, want 2: %+v", len(hits), hits)
	}
	if hits[0].Path != "a.go" || hits[0].Line != 3 {
		t.Errorf("hit[0] = %+v, want a.go:3", hits[0])
	}
	if hits[1].Path != "sub/b.go" {
		t.Errorf("hit[1] = %+v, want sub/b.go", hits[1])
	}
}

func TestSearcher_GrepRegexp_PathPrefix(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a.go", "func Alpha() {}\n")
	mk(t, root, "sub/b.go", "func AlphaSub() {}\n")
	s := Build(root, []string{"a.go", "sub/b.go"})

	re := regexp.MustCompile(`func Alpha`)
	hits := s.GrepRegexp(re, []string{"func Alpha"}, "sub/", 0)
	if len(hits) != 1 || hits[0].Path != "sub/b.go" {
		t.Fatalf("path-prefixed GrepRegexp = %+v, want only sub/b.go", hits)
	}
}

func TestSearcher_GrepRegexp_NoLiterals(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a.go", "abc\ndef\n")
	s := Build(root, []string{"a.go"})

	// No usable literals — every file is scanned, regex still verifies.
	re := regexp.MustCompile(`^a.c$`)
	hits := s.GrepRegexp(re, nil, "", 0)
	if len(hits) != 1 || hits[0].Text != "abc" {
		t.Fatalf("GrepRegexp with no literals = %+v, want [abc]", hits)
	}
}

func TestSearcher_GrepRegexp_Limit(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "x.go", "match1\nmatch2\nmatch3\n")
	s := Build(root, []string{"x.go"})

	re := regexp.MustCompile(`match\d`)
	if hits := s.GrepRegexp(re, []string{"match"}, "", 2); len(hits) != 2 {
		t.Fatalf("limited GrepRegexp = %d hits, want 2", len(hits))
	}
}

func TestSearcher_GrepRegexp_Nil(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "x.go", "anything\n")
	s := Build(root, []string{"x.go"})
	if hits := s.GrepRegexp(nil, nil, "", 0); hits != nil {
		t.Fatalf("GrepRegexp(nil) = %+v, want nil", hits)
	}
}
