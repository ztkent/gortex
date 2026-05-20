package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/crashpool"
	"github.com/zzet/gortex/internal/parser/languages"
)

// crashWorkerEnv makes the indexer test binary re-execute itself as a
// crashpool worker subprocess instead of running the test suite.
const crashWorkerEnv = "GORTEX_INDEXER_TEST_PARSEWORKER"

// TestMain lets crash-isolation tests use the test binary itself as the
// parser worker subprocess — no built gortex binary required.
func TestMain(m *testing.M) {
	if os.Getenv(crashWorkerEnv) == "1" {
		_ = crashpool.RunWorker(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExtractFile_InProcess(t *testing.T) {
	g := graph.New()
	idx := newTestIndexer(g)
	result, quarantined, err := idx.extractFile(nil, nil, "x.go", "x.go", "go",
		languages.NewGoExtractor(), []byte("package main\n\nfunc Hello() {}\n"))
	require.NoError(t, err)
	require.False(t, quarantined)
	require.NotEmpty(t, result.Nodes)
}

// TestExtractFile_SubprocessPool exercises the full parent→worker→parent
// path: the file is parsed in a worker subprocess and its nodes come
// back over the gob pipe.
func TestExtractFile_SubprocessPool(t *testing.T) {
	pool, err := crashpool.NewPool(crashpool.Config{
		Argv:    []string{os.Args[0]},
		Env:     []string{crashWorkerEnv + "=1"},
		Workers: 2,
	})
	require.NoError(t, err)
	defer pool.Close()

	g := graph.New()
	idx := newTestIndexer(g)
	q := crashpool.LoadQuarantine("")

	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	src := []byte("package main\n\nfunc Hello() {}\n")
	writeFile(t, path, string(src))

	result, quarantined, err := idx.extractFile(pool, q, path, "main.go", "go",
		languages.NewGoExtractor(), src)
	require.NoError(t, err)
	require.False(t, quarantined)
	require.NotEmpty(t, result.Nodes)
}

// TestIndex_CrashIsolationFullPass indexes a repo end-to-end with crash
// isolation enabled — the IndexCtx wiring spawns worker subprocesses,
// parses every file through them, and completes.
func TestIndex_CrashIsolationFullPass(t *testing.T) {
	t.Setenv("GORTEX_PARSER_ISOLATION", "1")
	t.Setenv(crashWorkerEnv, "1")

	dir := setupTestDir(t)
	g := graph.New()
	idx := newTestIndexer(g)
	result, err := idx.Index(dir)
	require.NoError(t, err)
	require.Equal(t, 2, result.FileCount) // main.go + pkg/util.go (vendor excluded)
	require.Greater(t, result.NodeCount, 0)
	require.Equal(t, 0, result.QuarantinedFiles)
}

func TestCrashIsolationEnabled_EnvOverride(t *testing.T) {
	g := graph.New()
	idx := newTestIndexer(g)
	require.False(t, idx.crashIsolationEnabled())

	idx.config.CrashIsolation = true
	require.True(t, idx.crashIsolationEnabled())

	t.Setenv("GORTEX_PARSER_ISOLATION", "0")
	require.False(t, idx.crashIsolationEnabled()) // env overrides config-on

	t.Setenv("GORTEX_PARSER_ISOLATION", "1")
	idx.config.CrashIsolation = false
	require.True(t, idx.crashIsolationEnabled()) // env overrides config-off
}

func TestQuarantineResult(t *testing.T) {
	r := quarantineResult("bad/file.go", "go", "SIGSEGV in grammar")
	require.Len(t, r.Nodes, 1)
	n := r.Nodes[0]
	require.Equal(t, graph.KindFile, n.Kind)
	require.Equal(t, "bad/file.go", n.ID)
	require.Equal(t, "file.go", n.Name)
	require.Equal(t, true, n.Meta["quarantined"])
	require.Equal(t, "SIGSEGV in grammar", n.Meta["parse_error"])
}

func TestStampParseErrorCount(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "f.go", Kind: graph.KindFile},
		{ID: "f.go::Fn", Kind: graph.KindFunction},
	}
	stampParseErrorCount(nodes, 3)
	require.Equal(t, 3, nodes[0].Meta["parse_errors"])
	require.Equal(t, true, nodes[0].Meta["has_parse_errors"])
	require.Nil(t, nodes[1].Meta)

	clean := []*graph.Node{{ID: "g.go", Kind: graph.KindFile}}
	stampParseErrorCount(clean, 0) // zero count → no stamp
	require.Nil(t, clean[0].Meta)
}

// TestSharedParsePool_Reused proves the crash-isolation pool is created
// once and reused — the watcher path must not fork a fresh worker
// subprocess per file.
func TestSharedParsePool_Reused(t *testing.T) {
	t.Setenv("GORTEX_PARSER_ISOLATION", "1")
	t.Setenv(crashWorkerEnv, "1")

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRootPath(t.TempDir())
	defer idx.CloseParsePool()

	p1, _ := idx.sharedParsePool()
	require.NotNil(t, p1)
	p2, _ := idx.sharedParsePool()
	require.Same(t, p1, p2, "sharedParsePool must return the same long-lived pool")
}

// TestIndexFile_CrashIsolationReusesPool re-indexes single files through
// the crash-isolation path and confirms they all flow through one
// persistent pool rather than a per-file worker subprocess.
func TestIndexFile_CrashIsolationReusesPool(t *testing.T) {
	t.Setenv("GORTEX_PARSER_ISOLATION", "1")
	t.Setenv(crashWorkerEnv, "1")

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package main\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package main\n\nfunc B() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir) // cold index also anchors rootPath
	require.NoError(t, err)
	defer idx.CloseParsePool()

	require.NoError(t, idx.IndexFile(filepath.Join(dir, "a.go")))
	require.NotNil(t, idx.parsePool, "first IndexFile must create the shared pool")
	first := idx.parsePool

	require.NoError(t, idx.IndexFile(filepath.Join(dir, "b.go")))
	require.Same(t, first, idx.parsePool, "second IndexFile must reuse the shared pool")

	require.NotEmpty(t, g.FindNodesByName("A"))
	require.NotEmpty(t, g.FindNodesByName("B"))
}
