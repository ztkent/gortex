package releases

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func runCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupTaggedRepo creates a tiny repo with two commits and two
// tags. v0.1 contains a.go only; v0.2 adds b.go. Used by the
// EnrichGraph integration tests.
func setupTaggedRepo(t *testing.T) string {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runCmd(t, dir, "git", "init", "-q")
	runCmd(t, dir, "git", "config", "user.email", "test@example.com")
	runCmd(t, dir, "git", "config", "user.name", "Tester")

	writeFile(t, filepath.Join(dir, "a.go"), "package main\n")
	runCmd(t, dir, "git", "add", "a.go")
	runCmd(t, dir, "git", "commit", "-q", "-m", "v01")
	runCmd(t, dir, "git", "tag", "v0.1")

	writeFile(t, filepath.Join(dir, "b.go"), "package main\n")
	runCmd(t, dir, "git", "add", "b.go")
	runCmd(t, dir, "git", "commit", "-q", "-m", "v02")
	runCmd(t, dir, "git", "tag", "v0.2")

	return dir
}

func TestListTags(t *testing.T) {
	dir := setupTaggedRepo(t)
	tags := ListTags(dir)
	if len(tags) != 2 {
		t.Fatalf("expected 2 tags, got %d: %v", len(tags), tags)
	}
	if tags[0] != "v0.1" || tags[1] != "v0.2" {
		t.Errorf("tags out of chronological order: %v", tags)
	}
}

func TestListTags_NoTags(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runCmd(t, dir, "git", "init", "-q")
	tags := ListTags(dir)
	if len(tags) != 0 {
		t.Errorf("untagged repo should yield no tags, got %v", tags)
	}
}

func TestFilesAtTag(t *testing.T) {
	dir := setupTaggedRepo(t)
	v01 := FilesAtTag(dir, "v0.1")
	if len(v01) != 1 || v01[0] != "a.go" {
		t.Errorf("v0.1 files = %v, want [a.go]", v01)
	}
	v02 := FilesAtTag(dir, "v0.2")
	if len(v02) != 2 {
		t.Errorf("v0.2 files = %v, want 2", v02)
	}
}

func TestEnrichGraph_AssignsEarliestTag(t *testing.T) {
	dir := setupTaggedRepo(t)
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:       "a.go",
		Kind:     graph.KindFile,
		FilePath: "a.go",
	})
	g.AddNode(&graph.Node{
		ID:       "b.go",
		Kind:     graph.KindFile,
		FilePath: "b.go",
	})

	count, err := EnrichGraph(g, dir)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 enriched, got %d", count)
	}
	if got := g.GetNode("a.go").Meta["added_in"]; got != "v0.1" {
		t.Errorf("a.go added_in = %v, want v0.1", got)
	}
	if got := g.GetNode("b.go").Meta["added_in"]; got != "v0.2" {
		t.Errorf("b.go added_in = %v, want v0.2", got)
	}
}

func TestEnrichGraph_MultiRepoPrefixHandled(t *testing.T) {
	dir := setupTaggedRepo(t)
	g := graph.New()
	// Multi-repo prefixed path — strip the leading segment.
	g.AddNode(&graph.Node{
		ID:       "myrepo/a.go",
		Kind:     graph.KindFile,
		FilePath: "myrepo/a.go",
	})

	count, err := EnrichGraph(g, dir)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 enriched (with prefix-strip), got %d", count)
	}
	if got := g.GetNode("myrepo/a.go").Meta["added_in"]; got != "v0.1" {
		t.Errorf("added_in = %v", got)
	}
}

func TestEnrichGraph_SkipsNonFileKinds(t *testing.T) {
	dir := setupTaggedRepo(t)
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:   "a.go::F",
		Kind: graph.KindFunction, // not a file
	})

	count, err := EnrichGraph(g, dir)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if count != 0 {
		t.Errorf("non-file kind shouldn't be enriched, got %d", count)
	}
}

func TestEnrichGraph_NoTagsReturnsZero(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runCmd(t, dir, "git", "init", "-q")
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:       "a.go",
		Kind:     graph.KindFile,
		FilePath: "a.go",
	})
	count, err := EnrichGraph(g, dir)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if count != 0 {
		t.Errorf("no-tag repo should yield 0 enriched, got %d", count)
	}
}
