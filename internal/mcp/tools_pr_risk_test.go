package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// prRiskTestServer builds a server over a synthetic graph: a security-sensitive
// hub function with many inbound callers and no covering test.
func prRiskTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	g := graph.New()
	hubID := "internal/auth/login.go::ValidateToken"
	g.AddNode(&graph.Node{ID: hubID, Kind: graph.KindFunction, Name: "ValidateToken", FilePath: "internal/auth/login.go", StartLine: 1, EndLine: 10})
	for i := 0; i < 12; i++ {
		cid := "pkg/c.go::caller" + strconv.Itoa(i)
		g.AddNode(&graph.Node{ID: cid, Kind: graph.KindFunction, Name: "caller" + strconv.Itoa(i), FilePath: "pkg/c.go"})
		g.AddEdge(&graph.Edge{From: cid, To: hubID, Kind: graph.EdgeCalls})
	}
	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	return srv, hubID
}

func callPRRisk(t *testing.T, srv *Server, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "pr_risk"
	req.Params.Arguments = args
	res, err := srv.handlePRRisk(t.Context(), req)
	require.NoError(t, err)
	return res
}

func TestPRRisk_IDsPath(t *testing.T) {
	srv, hubID := prRiskTestServer(t)
	res := callPRRisk(t, srv, map[string]any{"ids": hubID})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Score            float64 `json:"score"`
		Risk             string  `json:"risk"`
		ReviewPriorities []struct {
			Axis   string  `json:"axis"`
			Score  float64 `json:"score"`
			Reason string  `json:"reason"`
		} `json:"review_priorities"`
		TotalAffected    int      `json:"total_affected"`
		UncoveredSymbols int      `json:"uncovered_symbols"`
		CommunitySpan    int      `json:"community_span"`
		SecurityHits     []string `json:"security_hits"`
		ChangedSymbols   int      `json:"changed_symbols"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))

	// Score is a 0-100 composite.
	require.GreaterOrEqual(t, out.Score, 0.0)
	require.LessOrEqual(t, out.Score, 100.0)

	// Risk label matches the impactRisk thresholds for the score.
	require.Equal(t, impactRisk(out.Score), out.Risk)

	// review_priorities non-empty and sorted descending by score.
	require.NotEmpty(t, out.ReviewPriorities)
	for i := 1; i < len(out.ReviewPriorities); i++ {
		require.GreaterOrEqual(t, out.ReviewPriorities[i-1].Score, out.ReviewPriorities[i].Score,
			"review_priorities must be sorted descending")
	}

	// A security path + 12-caller untested hub is at least HIGH.
	require.Contains(t, []string{"HIGH", "CRITICAL"}, out.Risk)
	require.Contains(t, out.SecurityHits, "auth")
	require.Equal(t, 1, out.ChangedSymbols)
}

func TestPRRisk_RequiresInput(t *testing.T) {
	srv, _ := prRiskTestServer(t)
	res := callPRRisk(t, srv, map[string]any{})
	require.True(t, res.IsError, "expected error when neither ids nor base is given")
}

func TestPRRisk_GCXAndTOONAndBudget(t *testing.T) {
	srv, hubID := prRiskTestServer(t)

	// GCX round-trip: the section headers must appear.
	gcx := callPRRisk(t, srv, map[string]any{"ids": hubID, "format": "gcx"})
	require.False(t, gcx.IsError)
	gtext := gcx.Content[0].(mcplib.TextContent).Text
	require.Contains(t, gtext, "pr_risk.summary")
	require.Contains(t, gtext, "pr_risk.priorities")

	// max_bytes budget is honoured (response stays bounded).
	budgeted := callPRRisk(t, srv, map[string]any{"ids": hubID, "format": "gcx", "max_bytes": float64(120)})
	require.False(t, budgeted.IsError)
	require.LessOrEqual(t, len(budgeted.Content[0].(mcplib.TextContent).Text), 400)

	// TOON round-trip: still carries the score key.
	toon := callPRRisk(t, srv, map[string]any{"ids": hubID, "format": "toon"})
	require.False(t, toon.IsError)
	require.Contains(t, toon.Content[0].(mcplib.TextContent).Text, "score")
}

// prRiskGitRepo creates a git repo with a base commit on `main` and a HEAD
// commit that mutates a security-sensitive file, then returns the root.
func prRiskGitRepo(t *testing.T) string {
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
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "diff.mnemonicPrefix", "false")
	run("config", "diff.noprefix", "false")

	authDir := filepath.Join(dir, "internal", "auth")
	require.NoError(t, os.MkdirAll(authDir, 0o755))
	src := "package auth\n\nfunc ValidateToken() int {\n\treturn 1\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(authDir, "login.go"), []byte(src), 0o644))
	run("add", ".")
	run("commit", "-m", "base")
	// Tag the base commit so the diff has a ref that is strictly behind HEAD
	// — a `base...HEAD` against the current branch tip would be empty.
	run("tag", "base-ref")

	// HEAD commit mutates the body inside ValidateToken.
	mutated := "package auth\n\nfunc ValidateToken() int {\n\tx := 1\n\treturn x\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(authDir, "login.go"), []byte(mutated), 0o644))
	run("add", ".")
	run("commit", "-m", "change")
	return dir
}

func TestPRRisk_BasePath(t *testing.T) {
	dir := prRiskGitRepo(t)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	srv.RunAnalysis()

	res := callPRRisk(t, srv, map[string]any{"base": "base-ref"})
	require.False(t, res.IsError, "errored: %v", res)

	var out struct {
		Risk           string   `json:"risk"`
		SecurityHits   []string `json:"security_hits"`
		ChangedSymbols int      `json:"changed_symbols"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))

	// The base diff derived at least one changed symbol (ValidateToken),
	// and the security axis picked up the auth path.
	require.GreaterOrEqual(t, out.ChangedSymbols, 1, "expected the base diff to map a changed symbol")
	require.Contains(t, out.SecurityHits, "auth")
}
