package review

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/platform"
)

// isolateGlobalConfig points platform.ConfigDir() at a fresh, empty
// tempdir for the duration of a test so the global rule layer never
// reads the developer's real ~/.gortex/config.yaml. It returns the
// resolved config dir (already created) so callers can plant a global
// config.yaml when they want to exercise that layer.
func isolateGlobalConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	dir := platform.ConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	return dir
}

// TestReviewConfigRoundTrip asserts config.Load round-trips a `review:`
// block carrying every field: the rule list, the gating thresholds, the
// depth-selection bounds, and post.allow_public.
func TestReviewConfigRoundTrip(t *testing.T) {
	yaml := `
review:
  rules:
    - name: tests
      path: "**/*_test.go"
      severity: warning
      rulepack: test
      disabled: false
    - name: auth
      path: "internal/auth/**"
      severity: error
      rulepack: security
      disabled: true
  min_confidence: 0.75
  min_severity: warning
  categories:
    - nil-deref
    - injection
  max_findings: 25
  quick_max_lines: 40
  deep_min_lines: 400
  deep_min_files: 12
  post:
    allow_public: true
`
	dir := t.TempDir()
	path := filepath.Join(dir, ".gortex.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	r := cfg.Review

	if len(r.Rules) != 2 {
		t.Fatalf("rules: got %d want 2", len(r.Rules))
	}
	if r.Rules[0].Name != "tests" || r.Rules[0].Path != "**/*_test.go" ||
		r.Rules[0].Severity != "warning" || r.Rules[0].Rulepack != "test" || r.Rules[0].Disabled {
		t.Errorf("rule[0] mismatch: %+v", r.Rules[0])
	}
	if r.Rules[1].Name != "auth" || r.Rules[1].Path != "internal/auth/**" ||
		r.Rules[1].Severity != "error" || r.Rules[1].Rulepack != "security" || !r.Rules[1].Disabled {
		t.Errorf("rule[1] mismatch: %+v", r.Rules[1])
	}
	if r.MinConfidence != 0.75 {
		t.Errorf("min_confidence: got %v want 0.75", r.MinConfidence)
	}
	if r.MinSeverity != "warning" {
		t.Errorf("min_severity: got %q want warning", r.MinSeverity)
	}
	if len(r.Categories) != 2 || r.Categories[0] != "nil-deref" || r.Categories[1] != "injection" {
		t.Errorf("categories: got %v", r.Categories)
	}
	if r.MaxFindings != 25 {
		t.Errorf("max_findings: got %d want 25", r.MaxFindings)
	}
	if r.QuickMaxLines != 40 {
		t.Errorf("quick_max_lines: got %d want 40", r.QuickMaxLines)
	}
	if r.DeepMinLines != 400 {
		t.Errorf("deep_min_lines: got %d want 400", r.DeepMinLines)
	}
	if r.DeepMinFiles != 12 {
		t.Errorf("deep_min_files: got %d want 12", r.DeepMinFiles)
	}
	if !r.Post.AllowPublic {
		t.Errorf("post.allow_public: got false want true")
	}
}

// TestRuleResolverDefaultsOnly asserts that with no config files present
// the embedded defaults resolve: a test file selects the test rulepack
// (ahead of the `**` catch-all), a non-test file falls to general, and
// `**` matches arbitrarily nested paths.
func TestRuleResolverDefaultsOnly(t *testing.T) {
	isolateGlobalConfig(t)
	repoRoot := t.TempDir() // empty repo — no .gortex.yaml, no .gortex/review.yaml

	rr, err := NewRuleResolver("", repoRoot)
	if err != nil {
		t.Fatalf("NewRuleResolver: %v", err)
	}

	// `**/*_test.go` wins over `**` for a test file, at any depth.
	for _, p := range []string{"foo_test.go", "internal/pkg/foo_test.go", "a/b/c/d_test.go"} {
		got, ok := rr.RuleFor(p)
		if !ok {
			t.Fatalf("RuleFor(%q): no match", p)
		}
		if got.Rulepack != "test" {
			t.Errorf("RuleFor(%q): rulepack=%q want test", p, got.Rulepack)
		}
	}

	// Non-test files fall through to the `**` general catch-all, nested too.
	for _, p := range []string{"main.go", "internal/auth/login.go", "a/b/c/d/e/f.go"} {
		got, ok := rr.RuleFor(p)
		if !ok {
			t.Fatalf("RuleFor(%q): no match (** must match nested paths)", p)
		}
		if got.Rulepack != "general" {
			t.Errorf("RuleFor(%q): rulepack=%q want general", p, got.Rulepack)
		}
	}
}

// TestRuleResolverLayerPrecedence asserts the four layers merge in
// precedence order: a custom rule shadows a project rule for the same
// glob, and the project layer in turn shadows the global layer.
func TestRuleResolverLayerPrecedence(t *testing.T) {
	globalDir := isolateGlobalConfig(t)

	// Global layer: config.yaml with a rule for *.go.
	globalYAML := `
review:
  rules:
    - name: global-go
      path: "**/*.go"
      rulepack: global-pack
`
	if err := os.WriteFile(filepath.Join(globalDir, "config.yaml"), []byte(globalYAML), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	// Project layer: .gortex.yaml shadows the global rule for *.go.
	repoRoot := t.TempDir()
	projectYAML := `
review:
  rules:
    - name: project-go
      path: "**/*.go"
      rulepack: project-pack
`
	if err := os.WriteFile(filepath.Join(repoRoot, ".gortex.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	// Custom layer (--rules): shadows everything for the same glob.
	customDir := t.TempDir()
	customPath := filepath.Join(customDir, "review.yaml")
	customYAML := `
rules:
  - name: custom-go
    path: "**/*.go"
    rulepack: custom-pack
`
	if err := os.WriteFile(customPath, []byte(customYAML), 0o644); err != nil {
		t.Fatalf("write custom config: %v", err)
	}

	// All four layers present → custom wins.
	rr, err := NewRuleResolver(customPath, repoRoot)
	if err != nil {
		t.Fatalf("NewRuleResolver: %v", err)
	}
	got, ok := rr.RuleFor("internal/x.go")
	if !ok {
		t.Fatalf("RuleFor: no match")
	}
	if got.Name != "custom-go" || got.Rulepack != "custom-pack" {
		t.Errorf("custom should shadow project+global: got %+v", got)
	}

	// Drop the custom layer → project wins over global.
	rr2, err := NewRuleResolver("", repoRoot)
	if err != nil {
		t.Fatalf("NewRuleResolver (no custom): %v", err)
	}
	got2, ok := rr2.RuleFor("internal/x.go")
	if !ok {
		t.Fatalf("RuleFor (no custom): no match")
	}
	if got2.Name != "project-go" || got2.Rulepack != "project-pack" {
		t.Errorf("project should shadow global: got %+v", got2)
	}
}

// TestRuleResolverRepoLocalCustom asserts the repo-local
// .gortex/review.yaml is picked up as the custom layer when no explicit
// --rules path is given.
func TestRuleResolverRepoLocalCustom(t *testing.T) {
	isolateGlobalConfig(t)
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".gortex"), 0o755); err != nil {
		t.Fatalf("mkdir .gortex: %v", err)
	}
	repoLocalYAML := `
rules:
  - name: repo-local-go
    path: "**/*.go"
    rulepack: repo-local-pack
`
	if err := os.WriteFile(filepath.Join(repoRoot, ".gortex", "review.yaml"), []byte(repoLocalYAML), 0o644); err != nil {
		t.Fatalf("write repo-local review.yaml: %v", err)
	}

	rr, err := NewRuleResolver("", repoRoot)
	if err != nil {
		t.Fatalf("NewRuleResolver: %v", err)
	}
	got, ok := rr.RuleFor("internal/x.go")
	if !ok {
		t.Fatalf("RuleFor: no match")
	}
	if got.Name != "repo-local-go" || got.Rulepack != "repo-local-pack" {
		t.Errorf("repo-local .gortex/review.yaml should win: got %+v", got)
	}
}

// TestRuleResolverDisabledSkipped asserts a Disabled rule is skipped
// during resolution, so a lower-precedence rule (or the embedded
// catch-all) governs the file instead.
func TestRuleResolverDisabledSkipped(t *testing.T) {
	isolateGlobalConfig(t)
	repoRoot := t.TempDir()

	// A disabled custom rule for *.go must NOT shadow anything. A second,
	// enabled custom rule for the same glob proves the disabled one was
	// skipped (the enabled one wins) rather than the disabled one matching.
	customDir := t.TempDir()
	customPath := filepath.Join(customDir, "review.yaml")
	customYAML := `
rules:
  - name: disabled-go
    path: "**/*.go"
    rulepack: disabled-pack
    disabled: true
  - name: enabled-go
    path: "**/*.go"
    rulepack: enabled-pack
`
	if err := os.WriteFile(customPath, []byte(customYAML), 0o644); err != nil {
		t.Fatalf("write custom config: %v", err)
	}

	rr, err := NewRuleResolver(customPath, repoRoot)
	if err != nil {
		t.Fatalf("NewRuleResolver: %v", err)
	}
	got, ok := rr.RuleFor("internal/x.go")
	if !ok {
		t.Fatalf("RuleFor: no match")
	}
	if got.Name == "disabled-go" {
		t.Fatalf("disabled rule must be skipped, but it matched: %+v", got)
	}
	if got.Name != "enabled-go" || got.Rulepack != "enabled-pack" {
		t.Errorf("enabled rule should win after disabled skipped: got %+v", got)
	}
}

// TestDefaultReviewRules asserts the embedded layer is the documented
// pair: a `**/*_test.go` test rulepack ahead of a `**` general catch-all.
func TestDefaultReviewRules(t *testing.T) {
	rules := defaultReviewRules()
	if len(rules) != 2 {
		t.Fatalf("defaultReviewRules: got %d want 2", len(rules))
	}
	if rules[0].Path != "**/*_test.go" || rules[0].Rulepack != "test" {
		t.Errorf("rule[0]: got %+v want test/**/*_test.go", rules[0])
	}
	if rules[1].Path != "**" || rules[1].Rulepack != "general" {
		t.Errorf("rule[1]: got %+v want general/**", rules[1])
	}
}
