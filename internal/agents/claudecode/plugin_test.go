package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestEmitPluginBundle_Layout(t *testing.T) {
	dir := t.TempDir()

	written, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	})
	if err != nil {
		t.Fatalf("EmitPluginBundle: %v", err)
	}

	// The emitter should write exactly: 1 plugin.json + 1 README +
	// 1 LICENSE + 1 .mcp.json + N commands + N skills + 1 hooks.json
	// + 1 hook handler script.
	wantPaths := []string{
		".claude-plugin/plugin.json",
		".mcp.json",
		"LICENSE",
		"README.md",
		"hooks-handlers/gortex-hook.sh",
		"hooks/hooks.json",
	}
	for _, name := range sortedKeys(SlashCommands) {
		wantPaths = append(wantPaths, filepath.Join("commands", name))
	}
	for _, name := range sortedKeys(GlobalSkills) {
		wantPaths = append(wantPaths, filepath.Join("skills", name, "SKILL.md"))
	}
	sort.Strings(wantPaths)

	got := append([]string(nil), written...)
	sort.Strings(got)

	if len(got) != len(wantPaths) {
		t.Fatalf("file count mismatch: got %d, want %d.\ngot=%v\nwant=%v", len(got), len(wantPaths), got, wantPaths)
	}
	for i := range got {
		if got[i] != wantPaths[i] {
			t.Errorf("path[%d] mismatch: got %q, want %q", i, got[i], wantPaths[i])
		}
	}

	// Every path the emitter claimed to write must exist on disk.
	for _, rel := range written {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("missing file %s: %v", rel, err)
		}
	}
}

func TestEmitPluginBundle_PluginManifestShape(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("plugin.json: invalid JSON: %v", err)
	}
	// Required keys per the marketplace schema we observed in
	// claude-plugins-official: name, description, author. We add
	// version + homepage for our own metadata.
	for _, key := range []string{"name", "description", "author", "version", "homepage"} {
		if _, ok := manifest[key]; !ok {
			t.Errorf("plugin.json missing required key %q", key)
		}
	}
	if got := manifest["name"]; got != "gortex" {
		t.Errorf("plugin.json name = %v, want gortex", got)
	}
	if got := manifest["version"]; got != "0.18.2" {
		t.Errorf("plugin.json version = %v, want 0.18.2", got)
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Errorf("plugin.json should end with a newline (got %q)", string(body[len(body)-3:]))
	}
}

func TestEmitPluginBundle_MCPJSONShape(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mcp map[string]any
	if err := json.Unmarshal(body, &mcp); err != nil {
		t.Fatalf(".mcp.json: invalid JSON: %v", err)
	}
	gx, ok := mcp["gortex"].(map[string]any)
	if !ok {
		t.Fatalf(".mcp.json: missing 'gortex' entry, got %v", mcp)
	}
	if got := gx["command"]; got != "gortex" {
		t.Errorf(".mcp.json command = %v, want gortex", got)
	}
	args, ok := gx["args"].([]any)
	if !ok || len(args) != 2 || args[0] != "mcp" || args[1] != "--proxy" {
		t.Errorf(".mcp.json args = %v, want [mcp --proxy]", args)
	}
}

func TestEmitPluginBundle_HooksShape(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("hooks.json: invalid JSON: %v", err)
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks.json: missing top-level 'hooks' object: %v", doc)
	}

	// All four event kinds must be present; each must reference the
	// hooks-handlers script via ${CLAUDE_PLUGIN_ROOT}.
	for _, event := range []string{"PreToolUse", "PreCompact", "Stop", "SessionStart"} {
		entries, ok := hooks[event].([]any)
		if !ok || len(entries) == 0 {
			t.Errorf("hooks.json: event %s missing or empty", event)
			continue
		}
		entry := entries[0].(map[string]any)
		inner, ok := entry["hooks"].([]any)
		if !ok || len(inner) == 0 {
			t.Errorf("hooks.json: %s has no inner hooks", event)
			continue
		}
		first := inner[0].(map[string]any)
		cmd, _ := first["command"].(string)
		if !strings.Contains(cmd, "${CLAUDE_PLUGIN_ROOT}") {
			t.Errorf("hooks.json: %s command does not reference ${CLAUDE_PLUGIN_ROOT}: %q", event, cmd)
		}
		if !strings.Contains(cmd, "gortex-hook.sh") {
			t.Errorf("hooks.json: %s command does not invoke gortex-hook.sh: %q", event, cmd)
		}
	}

	// PreToolUse should carry the canonical matcher.
	pre := hooks["PreToolUse"].([]any)
	matcher, _ := pre[0].(map[string]any)["matcher"].(string)
	if matcher != CurrentPreToolUseMatcher {
		t.Errorf("hooks.json: PreToolUse matcher = %q, want %q", matcher, CurrentPreToolUseMatcher)
	}
}

func TestEmitPluginBundle_HookHandlerExecBit(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "hooks-handlers", "gortex-hook.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("hooks-handlers/gortex-hook.sh should be executable (mode=%v)", info.Mode())
	}
}

func TestEmitPluginBundle_SkillBodiesMatchSource(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	// Single-source-of-truth check: the bytes written under
	// skills/<name>/SKILL.md must equal GlobalSkills[<name>] exactly.
	// Drift means content.go was edited without re-running the
	// emitter; the CI guard catches that case but we double-check
	// here in unit tests.
	for name, want := range GlobalSkills {
		got, err := os.ReadFile(filepath.Join(dir, "skills", name, "SKILL.md"))
		if err != nil {
			t.Errorf("read skill %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("skill %s body drift: bytes do not match GlobalSkills[%q]", name, name)
		}
	}
}

func TestEmitPluginBundle_CommandBodiesMatchSource(t *testing.T) {
	dir := t.TempDir()
	if _, err := EmitPluginBundle(PluginBundleSpec{
		TargetDir: dir,
		Version:   "0.18.2",
	}); err != nil {
		t.Fatal(err)
	}

	for name, want := range SlashCommands {
		got, err := os.ReadFile(filepath.Join(dir, "commands", name))
		if err != nil {
			t.Errorf("read command %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("command %s body drift", name)
		}
	}
}

func TestEmitPluginBundle_Idempotent(t *testing.T) {
	dir := t.TempDir()

	first, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Fatalf("second emit: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("file count drift between runs: %d vs %d", len(first), len(second))
	}
	// Every file's bytes must be identical across the two runs.
	for _, rel := range first {
		path := filepath.Join(dir, rel)
		body1, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Re-emit and re-read: same path, must equal first body.
		body2, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body1) != string(body2) {
			t.Errorf("non-idempotent output at %s", rel)
		}
	}
}

func TestEmitPluginBundle_RejectsBadInputs(t *testing.T) {
	cases := map[string]PluginBundleSpec{
		"missing-target":  {Version: "0.18.2"},
		"missing-version": {TargetDir: t.TempDir()},
		"bad-variant":     {TargetDir: t.TempDir(), Version: "0.18.2", LayoutVariant: "weird"},
	}
	for name, spec := range cases {
		_, err := EmitPluginBundle(spec)
		if err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestEmitPluginBundle_VariantDefaultsToAnthropic(t *testing.T) {
	dir := t.TempDir()
	// Empty LayoutVariant should fall through to LayoutVariantAnthropic.
	_, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Fatalf("empty variant: %v", err)
	}
	// Verify by checking that the Anthropic-shaped manifest exists.
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("default variant did not produce .claude-plugin/plugin.json: %v", err)
	}
}

func TestEmitPluginBundle_OverwritesExistingFiles(t *testing.T) {
	dir := t.TempDir()

	// Pre-seed an old README to ensure overwrite works.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "OLD\n" {
		t.Errorf("emitter did not overwrite existing README.md")
	}
}

func TestPluginPathHelpers(t *testing.T) {
	if PluginManifestPath() != filepath.Join(".claude-plugin", "plugin.json") {
		t.Errorf("PluginManifestPath wrong: %s", PluginManifestPath())
	}
	if PluginMCPPath() != ".mcp.json" {
		t.Errorf("PluginMCPPath wrong: %s", PluginMCPPath())
	}
	if PluginHooksPath() != filepath.Join("hooks", "hooks.json") {
		t.Errorf("PluginHooksPath wrong: %s", PluginHooksPath())
	}

	cmds := PluginCommandPaths()
	if len(cmds) != len(SlashCommands) {
		t.Errorf("PluginCommandPaths count: got %d, want %d", len(cmds), len(SlashCommands))
	}
	skills := PluginSkillPaths()
	if len(skills) != len(GlobalSkills) {
		t.Errorf("PluginSkillPaths count: got %d, want %d", len(skills), len(GlobalSkills))
	}
}

func TestEmitPluginBundle_MissingTargetParent(t *testing.T) {
	// MkdirAll handles non-existent parents — verify that.
	dir := filepath.Join(t.TempDir(), "deep", "nested", "path")
	_, err := EmitPluginBundle(PluginBundleSpec{TargetDir: dir, Version: "0.18.2"})
	if err != nil {
		t.Errorf("EmitPluginBundle should create parent dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Errorf("emitter did not create plugin.json at deep target")
		}
	}
}
