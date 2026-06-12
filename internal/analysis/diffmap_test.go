package analysis

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// sampleDiff is a synthetic unified diff (context width 3) covering two files
// with adds, deletes, and context lines so both the hunk parser and the
// line-carrying parser have something to chew on.
const sampleDiff = `diff --git a/pkg/foo.go b/pkg/foo.go
index 1111111..2222222 100644
--- a/pkg/foo.go
+++ b/pkg/foo.go
@@ -1,6 +1,7 @@
 package foo

 func Foo() int {
-	return 1
+	x := compute()
+	return x
 }

@@ -20,3 +21,3 @@ func Bar() {
 	a := 1
-	b := 2
+	b := 3
 	_ = a
diff --git a/pkg/baz.go b/pkg/baz.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/pkg/baz.go
@@ -0,0 +1,3 @@
+package baz
+
+func Baz() {}
`

func TestParseDiffHunksEqualsInternal(t *testing.T) {
	got := ParseDiffHunks(sampleDiff)
	want := parseDiffHunks(sampleDiff)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseDiffHunks != parseDiffHunks\n got: %#v\nwant: %#v", got, want)
	}
	if len(got) == 0 {
		t.Fatalf("expected some hunks from the sample diff, got none")
	}
}

func TestParseDiffLinesNewSide(t *testing.T) {
	lines := parseDiffLines(sampleDiff)

	foo := lines["pkg/foo.go"]
	if len(foo) == 0 {
		t.Fatalf("expected new-side lines for pkg/foo.go")
	}
	// The first hunk starts at new line 1; the added "x := compute()" lands on
	// line 4 (after package/blank/func-sig context).
	var sawCompute bool
	for _, hl := range foo {
		// No removed lines must ever appear.
		if hl.Side != "+" && hl.Side != " " {
			t.Fatalf("unexpected side %q on %#v", hl.Side, hl)
		}
		if hl.Side == "+" && hl.Text == "\tx := compute()" {
			sawCompute = true
			if hl.NewLine != 4 {
				t.Fatalf("expected 'x := compute()' on new line 4, got %d", hl.NewLine)
			}
		}
		// The removed "return 1" / "b := 2" must not surface.
		if hl.Text == "\treturn 1" && hl.Side != " " {
			t.Fatalf("removed line leaked into new-side lines: %#v", hl)
		}
	}
	if !sawCompute {
		t.Fatalf("added line 'x := compute()' missing from new-side lines: %#v", foo)
	}

	// New-file lines all carry "+", numbered 1..3.
	baz := lines["pkg/baz.go"]
	if len(baz) != 3 {
		t.Fatalf("expected 3 new-side lines for pkg/baz.go, got %d (%#v)", len(baz), baz)
	}
	for i, hl := range baz {
		if hl.Side != "+" {
			t.Fatalf("new file line %d should be an add, got side %q", i, hl.Side)
		}
		if hl.NewLine != i+1 {
			t.Fatalf("new file line %d numbered %d, want %d", i, hl.NewLine, i+1)
		}
	}
}

// newTestRepo creates a throwaway git repo with one committed file, mutates it,
// and returns the repo root. The base commit is on branch the caller diffs
// against via scope "all" (working tree vs HEAD).
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	// Force standard a/ b/ diff prefixes regardless of the developer's global
	// git config (mnemonic/noprefix would otherwise emit c/ w/ and defeat the
	// +++ b/ header match shared by MapGitDiff and parseDiffLines).
	run("config", "diff.mnemonicPrefix", "false")
	run("config", "diff.noprefix", "false")

	src := "package foo\n\nfunc Foo() int {\n\treturn 1\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "base")

	// Modify: add a line inside Foo.
	mutated := "package foo\n\nfunc Foo() int {\n\tx := 1\n\treturn x\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMapGitDiffWithLinesReturnsNewSideLines(t *testing.T) {
	dir := newTestRepo(t)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "foo.go::Foo",
		Kind:      graph.KindFunction,
		Name:      "Foo",
		FilePath:  "foo.go",
		StartLine: 3,
		EndLine:   6,
		Language:  "go",
	})

	res, lines, err := MapGitDiffWithLines(g, dir, "", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiffWithLines: %v", err)
	}
	if res == nil {
		t.Fatal("nil DiffResult")
	}
	foo := lines["foo.go"]
	if len(foo) == 0 {
		t.Fatalf("expected new-side lines for foo.go, got none (%#v)", lines)
	}
	var sawAdd bool
	for _, hl := range foo {
		if hl.Side == "+" {
			sawAdd = true
		}
		if hl.NewLine <= 0 {
			t.Fatalf("non-positive new line: %#v", hl)
		}
	}
	if !sawAdd {
		t.Fatalf("expected at least one added new-side line: %#v", foo)
	}

	// The changed symbol Foo should be detected (overlap logic unchanged).
	var sawFoo bool
	for _, cs := range res.ChangedSymbols {
		if cs.ID == "foo.go::Foo" {
			sawFoo = true
		}
	}
	if !sawFoo {
		t.Fatalf("expected Foo among changed symbols: %#v", res.ChangedSymbols)
	}
}

// TestMapGitDiffRepoPrefixJoin covers the multi-repo daemon shape: indexed
// file paths carry the repo prefix ("myrepo/foo.go") while git emits
// repo-relative hunk paths ("foo.go"). The prefix-aware join must find the
// symbol; ChangedFiles must stay diff-relative so git pathspec re-joins keep
// working.
func TestMapGitDiffRepoPrefixJoin(t *testing.T) {
	dir := newTestRepo(t)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "myrepo/foo.go::Foo",
		Kind:      graph.KindFunction,
		Name:      "Foo",
		FilePath:  "myrepo/foo.go",
		StartLine: 3,
		EndLine:   6,
		Language:  "go",
	})

	res, err := MapGitDiff(g, dir, "myrepo", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	var sawFoo bool
	for _, cs := range res.ChangedSymbols {
		if cs.ID == "myrepo/foo.go::Foo" {
			sawFoo = true
		}
	}
	if !sawFoo {
		t.Fatalf("expected prefixed Foo among changed symbols: %#v", res.ChangedSymbols)
	}
	if len(res.ChangedFiles) != 1 || res.ChangedFiles[0] != "foo.go" {
		t.Fatalf("ChangedFiles must keep diff-relative paths, got %#v", res.ChangedFiles)
	}

	// Without the prefix the join misses — the pre-fix behavior, kept for
	// single-repo graphs whose paths are unprefixed.
	res, err = MapGitDiff(g, dir, "", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiff (no prefix): %v", err)
	}
	if len(res.ChangedSymbols) != 0 {
		t.Fatalf("unprefixed join against a prefixed graph should miss, got %#v", res.ChangedSymbols)
	}
}

// TestMapGitDiffMnemonicPrefixConfig pins the diff header prefixes against
// hostile git config: with diff.mnemonicPrefix=true a worktree diff emits
// "+++ w/..." headers, which the "+++ b/" parser anchor would zero out —
// every diff-driven tool would silently report an empty changeset. The -c
// overrides in GitDiffArgs must win over repo and global config.
func TestMapGitDiffMnemonicPrefixConfig(t *testing.T) {
	dir := newTestRepo(t)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Hostile repo-local config (newTestRepo sets both to false; flip them).
	run("config", "diff.mnemonicPrefix", "true")

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "foo.go::Foo",
		Kind:      graph.KindFunction,
		Name:      "Foo",
		FilePath:  "foo.go",
		StartLine: 3,
		EndLine:   6,
		Language:  "go",
	})

	res, err := MapGitDiff(g, dir, "", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	if len(res.Hunks) == 0 {
		t.Fatalf("expected hunks despite diff.mnemonicPrefix=true, got none")
	}
	var sawFoo bool
	for _, cs := range res.ChangedSymbols {
		if cs.ID == "foo.go::Foo" {
			sawFoo = true
		}
	}
	if !sawFoo {
		t.Fatalf("expected Foo among changed symbols: %#v", res.ChangedSymbols)
	}

	run("config", "diff.noprefix", "true")
	res, err = MapGitDiff(g, dir, "", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiff (noprefix): %v", err)
	}
	if len(res.Hunks) == 0 {
		t.Fatalf("expected hunks despite diff.noprefix=true, got none")
	}
}

func TestJoinFileNodes(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "myrepo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "myrepo/a.go"})
	g.AddNode(&graph.Node{ID: "b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "b.go"})

	// Raw hit wins (single-repo / unprefixed graph).
	if nodes := JoinFileNodes(g, "myrepo", "b.go"); len(nodes) != 1 || nodes[0].ID != "b.go::B" {
		t.Fatalf("raw lookup should win: %#v", nodes)
	}
	// Relative path retries with the prefix.
	if nodes := JoinFileNodes(g, "myrepo", "a.go"); len(nodes) != 1 || nodes[0].ID != "myrepo/a.go::A" {
		t.Fatalf("prefixed retry should hit: %#v", nodes)
	}
	// Already-prefixed input does not double-prefix.
	if nodes := JoinFileNodes(g, "myrepo", "myrepo/a.go"); len(nodes) != 1 || nodes[0].ID != "myrepo/a.go::A" {
		t.Fatalf("already-prefixed input should hit raw: %#v", nodes)
	}
	// No prefix → raw only.
	if nodes := JoinFileNodes(g, "", "a.go"); len(nodes) != 0 {
		t.Fatalf("no-prefix miss should stay a miss: %#v", nodes)
	}
}

func TestJoinFilePath(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "myrepo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "myrepo/a.go"})

	if got := JoinFilePath(g, "myrepo", "a.go"); got != "myrepo/a.go" {
		t.Fatalf("expected prefixed path, got %q", got)
	}
	if got := JoinFilePath(g, "myrepo", "myrepo/a.go"); got != "myrepo/a.go" {
		t.Fatalf("already-prefixed path should pass through, got %q", got)
	}
	if got := JoinFilePath(g, "myrepo", "missing.go"); got != "missing.go" {
		t.Fatalf("unresolvable path should pass through raw, got %q", got)
	}
	if got := JoinFilePath(g, "", "a.go"); got != "a.go" {
		t.Fatalf("no prefix should pass through, got %q", got)
	}
}

// TestMapGitDiffUnchanged asserts the existing --unified=0 path still yields the
// same DiffResult shape (hunks + changed symbols + changed files) it always did,
// independent of the new sibling.
func TestMapGitDiffUnchanged(t *testing.T) {
	dir := newTestRepo(t)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "foo.go::Foo",
		Kind:      graph.KindFunction,
		Name:      "Foo",
		FilePath:  "foo.go",
		StartLine: 3,
		EndLine:   6,
		Language:  "go",
	})

	res, err := MapGitDiff(g, dir, "", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	if len(res.Hunks) == 0 {
		t.Fatalf("expected hunks, got none")
	}
	for _, h := range res.Hunks {
		if h.FilePath != "foo.go" {
			t.Fatalf("unexpected hunk file %q", h.FilePath)
		}
	}
	if len(res.ChangedFiles) != 1 || res.ChangedFiles[0] != "foo.go" {
		t.Fatalf("expected changed files [foo.go], got %#v", res.ChangedFiles)
	}
	var sawFoo bool
	for _, cs := range res.ChangedSymbols {
		if cs.ID == "foo.go::Foo" {
			sawFoo = true
		}
	}
	if !sawFoo {
		t.Fatalf("expected Foo among changed symbols: %#v", res.ChangedSymbols)
	}
}
