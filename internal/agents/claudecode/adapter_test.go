package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestClaudeCodeProjectModeCreatesCanonicalArtifacts is the
// acceptance test for the most important adapter. It asserts that
// a fresh project gets:
//   - .mcp.json with our server stanza
//   - .claude/settings.json with MCP permissions
//   - .claude/settings.local.json with the three hook events
//   - CLAUDE.md with the marker-guarded communities block (since
//     the test env seeds SkillsRouting)
//   - .claude/skills/generated/<DirName>/SKILL.md (one per
//     GeneratedSkill)
//
// Slash commands and the curated GlobalSkills are NOT written in
// project mode anymore — they're user-level artifacts installed by
// `gortex install`. TestClaudeCodeGlobalModeWritesUserFiles covers
// those.
//
// Re-running must be a no-op (idempotent contract).
func TestClaudeCodeProjectModeCreatesCanonicalArtifacts(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	expected := []string{
		filepath.Join(env.Root, ".mcp.json"),
		filepath.Join(env.Root, ".claude", "settings.json"),
		filepath.Join(env.Root, ".claude", "settings.local.json"),
		filepath.Join(env.Root, "CLAUDE.md"),
	}
	for _, s := range env.GeneratedSkills {
		expected = append(expected, filepath.Join(env.Root, ".claude", "skills", "generated", s.DirName, "SKILL.md"))
	}
	for _, p := range expected {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing artifact %s: %v", p, err)
		}
	}

	// Project-mode must NOT touch the user-level slash commands or
	// curated skills — those live in install mode now.
	for name := range SlashCommands {
		if _, err := os.Stat(filepath.Join(env.Home, ".claude", "commands", name)); err == nil {
			t.Errorf("project mode unexpectedly wrote user-level slash command %s", name)
		}
		if _, err := os.Stat(filepath.Join(env.Root, ".claude", "commands", name)); err == nil {
			t.Errorf("project mode unexpectedly wrote project-level slash command %s", name)
		}
	}
	for name := range GlobalSkills {
		if _, err := os.Stat(filepath.Join(env.Home, ".claude", "skills", name, "SKILL.md")); err == nil {
			t.Errorf("project mode unexpectedly wrote user-level skill %s", name)
		}
	}

	// CLAUDE.md must contain the communities-block markers (since
	// the stub SkillsRouting routes through UpsertMarkedBlock).
	claudeMd, _ := os.ReadFile(filepath.Join(env.Root, "CLAUDE.md"))
	if !strings.Contains(string(claudeMd), agents.CommunitiesStartMarker) {
		t.Fatalf("CLAUDE.md missing communities start marker: %s", claudeMd)
	}

	// Hooks file must reference our test hook command.
	hooksFile, _ := os.ReadFile(filepath.Join(env.Root, ".claude", "settings.local.json"))
	if !strings.Contains(string(hooksFile), "PreToolUse") {
		t.Fatalf("settings.local.json missing PreToolUse: %s", hooksFile)
	}

	// Idempotent re-run: every file should report skip.
	res2, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for _, f := range res2.Files {
		if f.Action != agents.ActionSkip {
			t.Errorf("expected skip on re-run for %s, got %s", f.Path, f.Action)
		}
	}
}

// TestClaudeCodeGlobalModeWritesUserFiles verifies that global mode
// (entered via `gortex install`) writes to ~/.claude.json, user-level
// hooks, and the user-level slash-commands + curated skills trees,
// while leaving the per-repo tree alone.
func TestClaudeCodeGlobalModeWritesUserFiles(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true in global mode")
	}

	// User-level files exist.
	expected := []string{
		filepath.Join(env.Home, ".claude.json"),
		filepath.Join(env.Home, ".claude", "settings.json"),
		filepath.Join(env.Home, ".claude", "settings.local.json"),
	}
	for name := range SlashCommands {
		expected = append(expected, filepath.Join(env.Home, ".claude", "commands", name))
	}
	for name := range GlobalSkills {
		expected = append(expected, filepath.Join(env.Home, ".claude", "skills", name, "SKILL.md"))
	}
	for _, p := range expected {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing user-level artifact %s: %v", p, err)
		}
	}

	// settings.json must contain the mcp__gortex__* permission rule
	// so MCP tool calls don't prompt for approval each session.
	settingsPath := filepath.Join(env.Home, ".claude", "settings.json")
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read %s: %v", settingsPath, err)
	}
	if !strings.Contains(string(body), "mcp__gortex__*") {
		t.Errorf("expected mcp__gortex__* in %s, got:\n%s", settingsPath, body)
	}

	// CLAUDE.md must contain the marker-fenced rule block when
	// InstallGlobalInstructions is true.
	claudeMd := filepath.Join(env.Home, ".claude", "CLAUDE.md")
	mdBody, err := os.ReadFile(claudeMd)
	if err != nil {
		t.Fatalf("read %s: %v", claudeMd, err)
	}
	if !strings.Contains(string(mdBody), agents.GlobalRulesStartMarker) ||
		!strings.Contains(string(mdBody), agents.GlobalRulesEndMarker) {
		t.Errorf("expected gortex marker block in %s, got:\n%s", claudeMd, mdBody)
	}
	if !strings.Contains(string(mdBody), "MANDATORY: Use Gortex MCP tools") {
		t.Errorf("expected rule body in %s, got:\n%s", claudeMd, mdBody)
	}

	// Per-repo files should *not* exist under global mode.
	for _, p := range []string{
		filepath.Join(env.Root, ".mcp.json"),
		filepath.Join(env.Root, "CLAUDE.md"),
		filepath.Join(env.Root, ".claude", "settings.local.json"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("global mode unexpectedly wrote per-repo file %s", p)
		}
	}
}

// TestClaudeCodeGlobalMode_NoClaudeMd skips the rule block when the
// caller opts out via InstallGlobalInstructions=false (i.e.
// `gortex install --no-claude-md`).
func TestClaudeCodeGlobalMode_NoClaudeMd(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = false
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	claudeMd := filepath.Join(env.Home, ".claude", "CLAUDE.md")
	if _, err := os.Stat(claudeMd); err == nil {
		t.Errorf("--no-claude-md ⇒ %s should not exist", claudeMd)
	}
}

// TestClaudeCodeGlobalMode_PreservesUserContent verifies the rule
// block is merged with marker fences without clobbering anything
// that already lives in ~/.claude/CLAUDE.md.
func TestClaudeCodeGlobalMode_PreservesUserContent(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true

	claudeMd := filepath.Join(env.Home, ".claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claudeMd), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := "# My personal Claude rules\n\nAlways respond in haiku.\n"
	if err := os.WriteFile(claudeMd, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	body, err := os.ReadFile(claudeMd)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "Always respond in haiku.") {
		t.Errorf("user content was clobbered, got:\n%s", body)
	}
	if !strings.Contains(string(body), agents.GlobalRulesStartMarker) {
		t.Errorf("rule block missing, got:\n%s", body)
	}
}

// TestClaudeCodeDryRunWritesNothing is the contract for --dry-run:
// Result must still classify every would-be write, but no bytes
// touch disk.
func TestClaudeCodeDryRunWritesNothing(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{DryRun: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Files) == 0 {
		t.Fatal("dry-run should still enumerate planned files")
	}
	// No actual files created.
	for _, f := range res.Files {
		if _, err := os.Stat(f.Path); err == nil {
			t.Errorf("dry-run wrote %s", f.Path)
		}
	}
}
