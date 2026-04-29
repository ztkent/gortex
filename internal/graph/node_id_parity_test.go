package graph_test

// Node-id stability parity test.
//
// Overlay and cloud paths assume node IDs produced by two different
// indexer invocations of the same source commit are byte-identical.
// If that ever drifts (host-local state in the ID, parse-order leaks,
// RNG, time, etc.), overlay merging silently breaks: the daemon's
// overlay node IDs no longer match the server's base node IDs, edges
// land on dangling endpoints, and queries return half-true answers.
//
// This test runs the live indexer pipeline twice on a freshly-copied
// pair of identical source trees. Different absolute paths simulate
// "two checkouts on one machine" (the cheap proxy for "two machines"
// — the only difference between the two cases is the absolute parent
// directory which the indexer is supposed to strip via repo-prefixing).

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func newParityRegistry() *parser.Registry {
	r := parser.NewRegistry()
	r.Register(languages.NewGoExtractor())
	return r
}

// fixtureFiles is the source tree planted under each "checkout".
// Mix of:
//   - top-level package (Go) — simple types and methods
//   - sub-package — exercises path-based ID composition
//   - HTTP route via stdlib mux — exercises contract emission
//   - import that crosses sub-packages — exercises resolver
//
// Multiple files per package and multiple symbols per file expose any
// parse-order or map-iteration-order leakage in node IDs.
var fixtureFiles = map[string]string{
	"go.mod": "module example.com/parity\n\ngo 1.21\n",
	"main.go": `package main

import (
	"net/http"

	"example.com/parity/internal/auth"
)

type Server struct{}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", auth.LoginHandler)
	mux.HandleFunc("/api/health", s.Health)
	return http.ListenAndServe(":8080", mux)
}

func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

func main() {
	srv := &Server{}
	_ = srv.Start()
}
`,
	"helpers.go": `package main

import "strings"

func normalize(s string) string { return strings.ToLower(s) }

func reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}
`,
	"internal/auth/login.go": `package auth

import "net/http"

type Credentials struct {
	User string
	Pass string
}

func LoginHandler(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("login"))
}

func Validate(c Credentials) bool {
	return c.User != "" && c.Pass != ""
}
`,
	"internal/auth/token.go": `package auth

import "time"

type Token struct {
	Value     string
	ExpiresAt time.Time
}

func NewToken(value string) Token {
	return Token{Value: value, ExpiresAt: time.Now().Add(time.Hour)}
}
`,
}

// TestNodeIDStability_Parity is the iteration-1 gate test for node-ID
// determinism. Indexes the same source tree from two distinct absolute
// paths (different temp dirs) and asserts the produced node IDs are
// identical sets after stripping the repo prefix. This catches:
//
//   - host-local state (working directory, hostname) leaking into IDs
//   - parse-order non-determinism (goroutine scheduling) leaking into IDs
//   - map-iteration-order leaking into IDs
//
// The repo prefix is allowed to differ because it's deliberately a
// human-readable disambiguator; the rest of the ID after the prefix
// must match byte-for-byte.
//
// For monorepo-shaped IDs the comparison is "same set" — we don't
// require parse-order to be stable, only the final ID set.
func TestNodeIDStability_Parity(t *testing.T) {
	idsA := indexFixture(t, "checkout-alpha")
	idsB := indexFixture(t, "checkout-beta")

	// Strip repo prefix so we're comparing what's structural about the
	// ID and not the deliberately-different prefix.
	stripped := func(in []string, prefix string) []string {
		out := make([]string, 0, len(in))
		pre := prefix + "/"
		for _, id := range in {
			if len(id) > len(pre) && id[:len(pre)] == pre {
				out = append(out, id[len(pre):])
				continue
			}
			out = append(out, id)
		}
		sort.Strings(out)
		return out
	}

	got := stripped(idsA.NodeIDs, idsA.Prefix)
	want := stripped(idsB.NodeIDs, idsB.Prefix)

	if !assert.Equal(t, want, got, "node IDs must be byte-identical across two indexings of the same source tree (after stripping repo prefix). divergence breaks overlay merging across daemon and cloud.") {
		// Surface the first few divergences directly so the failure
		// message points at the offending IDs rather than the full
		// list-of-thousands diff.
		diff := symmetricDifference(got, want)
		if len(diff) > 0 {
			t.Logf("symmetric difference (up to 20 ids): %v", diff[:minInt(len(diff), 20)])
		}
	}
}

type fixtureResult struct {
	NodeIDs []string
	Prefix  string
}

// indexFixture writes the fixture into a fresh temp dir under the
// given checkout name, indexes it via MultiIndexer (the warmup path),
// and returns the full set of node IDs in the resulting graph plus
// the repo prefix MultiIndexer assigned.
//
// We use MultiIndexer with two configured repos (the fixture + a
// throwaway sibling) so that willBeMultiRepo is true and the prefix
// path is exercised — that's the production code path the daemon
// runs and the one overlay/cloud merging will rely on.
func indexFixture(t *testing.T, checkoutName string) fixtureResult {
	t.Helper()

	// Plant the fixture in a unique temp tree.
	root := filepath.Join(t.TempDir(), checkoutName)
	require.NoError(t, os.MkdirAll(root, 0o755))
	for relPath, content := range fixtureFiles {
		full := filepath.Join(root, relPath)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}

	// Companion repo so multi-repo prefixing kicks in. We keep it
	// minimal — no parity comparison runs on it; it only exists to
	// flip willBeMultiRepo.
	companion := filepath.Join(t.TempDir(), checkoutName+"-companion")
	require.NoError(t, os.MkdirAll(companion, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(companion, "noop.go"),
		[]byte("package companion\n\nfunc Noop() {}\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config-"+checkoutName+".yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: root, Name: checkoutName},
			{Path: companion, Name: checkoutName + "-companion"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, newParityRegistry(), search.NewBM25(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err, "track %s", entry.Name)
	}

	ids := []string{}
	prefix := checkoutName
	for _, n := range g.AllNodes() {
		// This test is about source-symbol IDs (functions, methods,
		// types, files) — the things overlay merging keys on.
		// Contract-kind nodes (kind=contract) don't currently carry a
		// RepoPrefix field; skip them here so the parity gate is
		// precise about what it gates.
		if n.Kind == graph.KindContract {
			continue
		}
		if n.RepoPrefix == "" {
			t.Fatalf("node %q has empty RepoPrefix in multi-repo mode", n.ID)
		}
		if n.RepoPrefix != checkoutName {
			continue
		}
		ids = append(ids, n.ID)
	}

	require.NotEmpty(t, ids, "no fixture nodes produced — fixture or indexer regression")

	return fixtureResult{NodeIDs: ids, Prefix: prefix}
}

// symmetricDifference returns elements present in exactly one of a, b.
// Both must be sorted.
func symmetricDifference(a, b []string) []string {
	var diff []string
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			i++
			j++
		case a[i] < b[j]:
			diff = append(diff, "only-in-A:"+a[i])
			i++
		default:
			diff = append(diff, "only-in-B:"+b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		diff = append(diff, "only-in-A:"+a[i])
	}
	for ; j < len(b); j++ {
		diff = append(diff, "only-in-B:"+b[j])
	}
	return diff
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
