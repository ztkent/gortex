// Package agentstest provides shared helpers for writing
// golden-fixture-style tests against Adapter implementations.
// Every adapter test follows the same three-phase contract:
//
//  1. Create — Apply into an empty tmpdir. Expect N file creations.
//  2. Idempotent — Apply a second time into the same tmpdir. Expect
//     N skip actions (no writes, no merges).
//  3. Merge — Apply into a tmpdir that already contains a user file
//     with unrelated keys. Expect our content to be added without
//     clobbering the user's keys.
//
// The harness asserts on the agents.Result each phase returns so
// behaviour regressions surface with clear diffs rather than byte-
// level noise about map ordering.
package agentstest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	yaml "gopkg.in/yaml.v3"
)

// NewEnv returns an Env pointed at a temporary Root and Home.
// Stderr is a bytes.Buffer so tests can assert on progress lines;
// HookCommand is fixed so hook-writing adapters produce deterministic
// output.
func NewEnv(t *testing.T) (agents.Env, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	home := t.TempDir()
	buf := &bytes.Buffer{}
	return agents.Env{
		Root:         root,
		Home:         home,
		HookCommand:  "/tmp/test-gortex hook",
		Mode:         agents.ModeProject,
		InstallHooks: true,
		// SkillsRouting + GeneratedSkills are seeded so per-adapter
		// tests exercise the community-routing write path — which
		// is the only reason adapters touch per-repo instructions
		// files now. A real `gortex init` run sets these after
		// community generation; NewEnv fakes them with minimal
		// stubs so every adapter's test path stays realistic.
		SkillsRouting: StubSkillsRouting,
		GeneratedSkills: []agents.GeneratedSkill{{
			CommunityID: "test",
			Label:       "stub",
			DirName:     "gortex-stub",
			Content:     "---\nname: gortex-stub\ndescription: test stub\n---\n# Stub community\n",
		}},
		Stderr: buf,
	}, buf
}

// StubSkillsRouting is the fake routing payload NewEnv seeds. Kept
// small but with the marker-guarded wrapper contract preserved so
// tests that grep for the block shape stay realistic.
const StubSkillsRouting = `## Gortex Community Skills (test stub)

- stub: routes to gortex-stub/
`

// AssertCountsByAction fails the test when the result's action
// counts differ from the expected map. Used to declare "phase 1
// should produce 7 creates" in a readable way.
func AssertCountsByAction(t *testing.T, res *agents.Result, want map[agents.ActionKind]int) {
	t.Helper()
	got := make(map[agents.ActionKind]int)
	for _, f := range res.Files {
		got[f.Action]++
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected action kinds: got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %d, want %d (full: got %v, want %v)", k, got[k], v, got, want)
		}
	}
}

// AssertIdempotent re-runs Apply against the same env and checks
// that every returned action is a skip. This is the contract every
// adapter must honour: re-running `gortex init` never rewrites files
// that are already configured.
func AssertIdempotent(t *testing.T, a agents.Adapter, env agents.Env) {
	t.Helper()
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	for _, f := range res.Files {
		if f.Action != agents.ActionSkip {
			t.Fatalf("expected all skips on re-run, got %s for %s", f.Action, f.Path)
		}
	}
}

// ReadJSON parses a JSON file into a map — convenience for golden
// tests that want to assert on top-level keys without wrestling
// with the any type.
func ReadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

// WriteJSON writes a pretty-printed map to path — for seeding
// "pre-populated" scenarios in merge tests.
func WriteJSON(t *testing.T, path string, obj map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// ReadYAML parses a YAML file into a map — the YAML cousin of
// ReadJSON for adapters whose config lives in YAML (Hermes, Aider).
func ReadYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

// WriteYAML writes obj to path as YAML — for seeding "pre-populated"
// scenarios in merge tests.
func WriteYAML(t *testing.T, path string, obj map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := yaml.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// SortedFilePaths returns the Paths from a result's Files in sorted
// order. Makes golden assertions invariant to adapter iteration
// order over maps.
func SortedFilePaths(res *agents.Result) []string {
	out := make([]string, 0, len(res.Files))
	for _, f := range res.Files {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out
}
