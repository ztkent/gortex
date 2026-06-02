package hermes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// seedHermesHome creates an empty ~/.hermes so Detect() passes without
// depending on a `hermes` binary being on the test machine's PATH.
func seedHermesHome(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".hermes"), 0o755); err != nil {
		t.Fatalf("seed ~/.hermes: %v", err)
	}
}

// TestHermesApplyWritesGlobalConfigAndSkill is the acceptance test:
// `gortex install` must write the global mcp_servers.gortex stanza and
// a user-level skill, and a re-run must be a pure no-op.
func TestHermesApplyWritesGlobalConfigAndSkill(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	seedHermesHome(t, env.Home)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	// Global config: mcp_servers.gortex with command + args:[mcp] + timeouts.
	cfg := agentstest.ReadYAML(t, globalConfigPath(env.Home))
	servers, ok := cfg["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("mcp_servers missing or wrong type: %#v", cfg)
	}
	gortex, ok := servers["gortex"].(map[string]any)
	if !ok {
		t.Fatalf("gortex server missing: %#v", servers)
	}
	if cmd, _ := gortex["command"].(string); cmd == "" {
		t.Errorf("gortex command empty: %#v", gortex)
	}
	args, ok := gortex["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "mcp" {
		t.Errorf("gortex args = %#v, want [mcp]", gortex["args"])
	}
	if gortex["connect_timeout"] != connectTimeoutSecs {
		t.Errorf("connect_timeout = %v, want %d", gortex["connect_timeout"], connectTimeoutSecs)
	}
	if gortex["timeout"] != requestTimeoutSecs {
		t.Errorf("timeout = %v, want %d", gortex["timeout"], requestTimeoutSecs)
	}

	// Skill present, with Hermes frontmatter.
	skill, err := os.ReadFile(skillPath(env.Home))
	if err != nil {
		t.Fatalf("skill missing: %v", err)
	}
	for _, want := range []string{"name: gortex", "metadata:", "hermes:", "set_active_project"} {
		if !strings.Contains(string(skill), want) {
			t.Errorf("skill missing %q", want)
		}
	}

	agentstest.AssertIdempotent(t, a, env)
}

// TestHermesUpsertsEveryProfileConfig covers the "extra" robustness:
// Hermes profiles can re-declare their own mcp_servers block, so the
// adapter must upsert the gortex stanza into every existing profile
// config, not just the global one.
func TestHermesUpsertsEveryProfileConfig(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	seedHermesHome(t, env.Home)

	// Two profiles that already declare their own servers.
	profiles := []string{"work", "personal"}
	for _, name := range profiles {
		p := filepath.Join(env.Home, ".hermes", "profiles", name, "config.yaml")
		agentstest.WriteYAML(t, p, map[string]any{
			"mcp_servers": map[string]any{
				"github": map[string]any{"command": "npx", "args": []string{"-y", "server-github"}},
			},
		})
	}

	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	for _, name := range profiles {
		p := filepath.Join(env.Home, ".hermes", "profiles", name, "config.yaml")
		cfg := agentstest.ReadYAML(t, p)
		servers, ok := cfg["mcp_servers"].(map[string]any)
		if !ok {
			t.Fatalf("profile %s: mcp_servers missing: %#v", name, cfg)
		}
		if _, ok := servers["gortex"]; !ok {
			t.Errorf("profile %s: gortex not upserted: %#v", name, servers)
		}
		if _, ok := servers["github"]; !ok {
			t.Errorf("profile %s: pre-existing github server dropped: %#v", name, servers)
		}
	}

	agentstest.AssertIdempotent(t, a, env)
}

// TestHermesPreservesGlobalConfigComments guards the comment-rich
// merge: a hand-edited config keeps its comments and unrelated keys.
func TestHermesPreservesGlobalConfigComments(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	seedHermesHome(t, env.Home)

	cfgPath := globalConfigPath(env.Home)
	original := `# my hermes config
model: hermes-4 # the good one

mcp_servers:
  # filesystem access
  filesystem:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem"]
`
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	out, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(out)
	for _, want := range []string{"# my hermes config", "# filesystem access", "the good one"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost comment %q:\n%s", want, got)
		}
	}
	cfg := agentstest.ReadYAML(t, cfgPath)
	servers := cfg["mcp_servers"].(map[string]any)
	if _, ok := servers["filesystem"]; !ok {
		t.Error("pre-existing filesystem server dropped")
	}
	if _, ok := servers["gortex"]; !ok {
		t.Error("gortex server not added")
	}
	if cfg["model"] != "hermes-4" {
		t.Errorf("unrelated key model clobbered: %v", cfg["model"])
	}
}

// TestHermesForceOverwritesEntry verifies --force replaces a stale
// gortex entry instead of skipping.
func TestHermesForceOverwritesEntry(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	seedHermesHome(t, env.Home)
	cfgPath := globalConfigPath(env.Home)
	agentstest.WriteYAML(t, cfgPath, map[string]any{
		"mcp_servers": map[string]any{
			"gortex": map[string]any{"command": "OLD", "args": []string{"stale"}},
		},
	})

	a := New()
	res, err := a.Apply(env, agents.ApplyOpts{Force: true})
	if err != nil {
		t.Fatalf("apply --force: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}
	cfg := agentstest.ReadYAML(t, cfgPath)
	gortex := cfg["mcp_servers"].(map[string]any)["gortex"].(map[string]any)
	if gortex["command"] == "OLD" {
		t.Errorf("force did not overwrite stale entry: %#v", gortex)
	}
	args, _ := gortex["args"].([]any)
	if len(args) != 1 || args[0] != "mcp" {
		t.Errorf("force did not rewrite args: %#v", gortex["args"])
	}
}

// TestHermesDetect checks the home-directory gate. The PATH branch is
// covered implicitly by exec.LookPath and not asserted here so the
// test is independent of what's installed on the runner.
func TestHermesDetect(t *testing.T) {
	a := New()

	// ~/.hermes present → detect.
	home := t.TempDir()
	seedHermesHome(t, home)
	if detected, _ := a.Detect(agents.Env{Home: home}); !detected {
		t.Error("expected detection when ~/.hermes exists")
	}

	// Empty Home and no detectable hermes → skip (unless the runner
	// happens to have hermes on PATH, in which case true is correct).
	if detected, _ := a.Detect(agents.Env{Home: ""}); detected {
		if _, err := exec.LookPath("hermes"); err != nil {
			t.Error("detected Hermes with empty Home and no hermes on PATH")
		}
	}
}

// TestHermesDryRunWritesNothing verifies the plan-only path.
func TestHermesDryRunWritesNothing(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	seedHermesHome(t, env.Home)
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{DryRun: true}); err != nil {
		t.Fatalf("dry-run apply: %v", err)
	}
	if _, err := os.Stat(globalConfigPath(env.Home)); !os.IsNotExist(err) {
		t.Error("dry-run wrote the global config")
	}
	if _, err := os.Stat(skillPath(env.Home)); !os.IsNotExist(err) {
		t.Error("dry-run wrote the skill")
	}
}
