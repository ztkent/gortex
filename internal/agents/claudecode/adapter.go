package claudecode

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/agents"
)

// Name is the stable identifier for this adapter, matching the
// --agents=<name> CLI flag.
const Name = "claude-code"

// DocsURL is the page we point users at when something about our
// Claude Code integration surprises them. Used in the --json report.
const DocsURL = "https://docs.claude.com/en/docs/claude-code/overview"

// Adapter implements agents.Adapter for Claude Code.
type Adapter struct{}

// New returns the Claude Code adapter. Callers register it via
// `agents.Registry.Register(claudecode.New())`.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect always returns true. Claude Code is the "home" agent for
// `gortex init` — a project may not be opened in Claude Code today
// but we always want the integration files on disk so the team's
// next contributor is set up. Other adapters gate on detection
// because their artifacts make no sense without the IDE installed.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	return true, nil
}

// Plan reports the full set of files the adapter would touch for
// the given Env. Mode branches between project (`gortex init`) and
// global (`gortex install`) surfaces; InstallHooks elides hook files
// without affecting anything else.
func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}
	if env.Mode == agents.ModeGlobal {
		// User-level artifacts — machine-wide. Slash commands and
		// curated Gortex tool-usage skills both belong here: they're
		// codebase-agnostic, so duplicating them into every repo is
		// wasted disk and drift risk.
		p.Files = append(p.Files, agents.FileAction{Path: userClaudeJSONPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"mcpServers"}})
		p.Files = append(p.Files, agents.FileAction{Path: userSettingsPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"permissions"}})
		if env.InstallHooks {
			p.Files = append(p.Files, agents.FileAction{Path: userSettingsLocalPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"hooks"}})
		}
		if env.InstallGlobalInstructions {
			p.Files = append(p.Files, agents.FileAction{Path: userClaudeMdPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"gortex-rules-block"}})
		}
		if env.Home != "" {
			for name := range GlobalSkills {
				p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Home, ".claude", "skills", name, "SKILL.md"), Action: agents.ActionWouldCreate})
			}
			for name := range SlashCommands {
				p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Home, ".claude", "commands", name), Action: agents.ActionWouldCreate})
			}
		}
		return p, nil
	}

	// Project mode — only genuinely repo-specific artifacts. No
	// tool-usage duplication: that lives at ~/.claude/skills/
	// (installed by `gortex install`). CLAUDE.md gets a
	// marker-guarded block only when --analyze or --skills produce
	// codebase-derived content.
	p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".mcp.json"), Action: agents.ActionWouldCreate, Keys: []string{"mcpServers"}})
	p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".claude", "settings.json"), Action: agents.ActionWouldMerge, Keys: []string{"permissions"}})
	if env.InstallHooks {
		p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".claude", "settings.local.json"), Action: agents.ActionWouldMerge, Keys: []string{"hooks"}})
	}
	if env.AnalyzedOverview != "" || env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, "CLAUDE.md"), Action: agents.ActionWouldMerge, Keys: []string{"communities-block"}})
	}
	for _, s := range env.GeneratedSkills {
		p.Files = append(p.Files, agents.FileAction{Path: filepath.Join(env.Root, ".claude", "skills", "generated", s.DirName, "SKILL.md"), Action: agents.ActionWouldCreate})
	}
	return p, nil
}

// Apply performs the actual writes. Errors mid-way do not abort the
// whole adapter — we log each failure and continue, matching the
// pre-refactor behaviour where Kiro/Cursor/etc. setup failures only
// emit warnings. Claude Code's .mcp.json, CLAUDE.md, and hook install
// are the exception: they're the core integration and propagate
// failures upward.
func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	w := env.Stderr
	res := &agents.Result{Name: Name, Detected: true, DocsURL: DocsURL}

	if env.Mode == agents.ModeGlobal {
		if err := a.applyGlobal(env, opts, res); err != nil {
			return res, err
		}
		res.Configured = true
		return res, nil
	}

	// 1. Project .mcp.json — create if absent, skip otherwise.
	mcpAction, err := agents.WriteIfNotExists(w, filepath.Join(env.Root, ".mcp.json"), ProjectMCPJSON, opts)
	if err != nil {
		return res, fmt.Errorf(".mcp.json: %w", err)
	}
	res.Files = append(res.Files, mcpAction)

	// 2. MCP permissions in .claude/settings.json — merge, not create.
	permAction, err := installPermissions(w, filepath.Join(env.Root, ".claude", "settings.json"), opts)
	if err != nil {
		logWarn(w, "could not install permissions: %v", err)
	}
	res.Files = append(res.Files, permAction)

	// 3. Hooks in .claude/settings.local.json — merge with healing.
	if env.InstallHooks {
		hookAction, err := InstallHookWithMode(w, filepath.Join(env.Root, ".claude", "settings.local.json"), env.HookMode, opts)
		if err != nil {
			logWarn(w, "could not install hook: %v", err)
		}
		res.Files = append(res.Files, hookAction)
	} else {
		logf(w, "[gortex init] skipping hook installation (--no-hooks)")
	}

	// 4. CLAUDE.md — only written when there's genuinely
	// codebase-specific content to place there: either the
	// --analyze overview, the --skills community routing, or both.
	// Generic tool-usage moved to user-level ~/.claude/skills/
	// (installed by `gortex install`).
	if env.AnalyzedOverview != "" || env.SkillsRouting != "" {
		claudeMdPath := filepath.Join(env.Root, "CLAUDE.md")
		var body strings.Builder
		if env.AnalyzedOverview != "" {
			body.WriteString(env.AnalyzedOverview)
			if !strings.HasSuffix(env.AnalyzedOverview, "\n") {
				body.WriteString("\n")
			}
		}
		if env.SkillsRouting != "" {
			if body.Len() > 0 {
				body.WriteString("\n")
			}
			body.WriteString(env.SkillsRouting)
		}
		claudeAction, err := agents.UpsertMarkedBlock(w, claudeMdPath, body.String(),
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			return res, fmt.Errorf("CLAUDE.md: %w", err)
		}
		res.Files = append(res.Files, claudeAction)
	}

	// 5. Generated community skills — per-community SKILL.md files
	// under .claude/skills/generated/. Claude Code auto-discovers
	// them next to the repo-local CLAUDE.md. Regenerated each init
	// run so they track the current graph.
	for _, s := range env.GeneratedSkills {
		path := filepath.Join(env.Root, ".claude", "skills", "generated", s.DirName, "SKILL.md")
		action, err := agents.WriteOwnedFile(w, path, s.Content, opts)
		if err != nil {
			logWarn(w, "could not write generated skill %s: %v", s.DirName, err)
			continue
		}
		res.Files = append(res.Files, action)
	}

	res.Configured = true
	return res, nil
}

// applyGlobal handles Mode=ModeGlobal writes (entered via `gortex
// install`). Everything here is codebase-agnostic user-level
// machinery: MCP config pointing at `gortex mcp`, user-level
// hooks, curated Gortex tool-usage skills, and Gortex slash
// commands. No per-repo artifacts.
func (a *Adapter) applyGlobal(env agents.Env, opts agents.ApplyOpts, res *agents.Result) error {
	w := env.Stderr
	if env.Home == "" {
		return fmt.Errorf("global mode requires a resolved home directory")
	}

	// 1. ~/.claude.json — MCP stanza pointing at `gortex mcp`.
	mcpPath := userClaudeJSONPath(env.Home)
	action, err := upsertGlobalMCPConfig(w, mcpPath, opts)
	if err != nil {
		return fmt.Errorf("global MCP config: %w", err)
	}
	res.Files = append(res.Files, action)

	// 2. ~/.claude/settings.json — user-level MCP permission allowlist.
	// Mirrors the project-mode call so `mcp__gortex__*` is auto-allowed
	// machine-wide; without this every Gortex tool call shows an
	// approval prompt until the user adds the rule by hand.
	permAction, err := installPermissions(w, userSettingsPath(env.Home), opts)
	if err != nil {
		logWarn(w, "could not install global permissions: %v", err)
	}
	res.Files = append(res.Files, permAction)

	// 3. ~/.claude/settings.local.json — user-level hooks.
	if env.InstallHooks {
		hookAction, err := InstallHookWithMode(w, userSettingsLocalPath(env.Home), env.HookMode, opts)
		if err != nil {
			return fmt.Errorf("global hooks: %w", err)
		}
		res.Files = append(res.Files, hookAction)
	}

	// 4. ~/.claude/CLAUDE.md — merge the rule block. Without this,
	// the rule only surfaces at deny-time (PreToolUse) which is
	// late: the agent has already wasted a turn on a forbidden
	// tool. The marker block keeps user content intact and is
	// regeneratable on re-install.
	if env.InstallGlobalInstructions {
		claudeMdPath := userClaudeMdPath(env.Home)
		mdAction, err := agents.UpsertMarkedBlock(w, claudeMdPath, agents.GlobalInstructionsBody,
			agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, opts)
		if err != nil {
			logWarn(w, "could not install global CLAUDE.md: %v", err)
		} else {
			logf(w, "[gortex install] wrote rule block to %s", claudeMdPath)
		}
		// UpsertMarkedBlock is shared with the per-repo communities
		// block, so it labels every action with "communities-block".
		// Relabel here so the install report distinguishes the two.
		if mdAction.Keys != nil {
			mdAction.Keys = []string{"gortex-rules-block"}
		}
		res.Files = append(res.Files, mdAction)
	}

	// 3. ~/.claude/skills/gortex-*/SKILL.md — curated tool-usage
	// skills (guide / explore / debug / impact / refactor). One
	// source of truth per user rather than duplicated into every
	// repo. Skipped when the files already exist so user edits
	// survive.
	skillActions, err := installGlobalSkills(w, env.Home, opts)
	if err != nil {
		logWarn(w, "could not install user-level skills: %v", err)
	}
	res.Files = append(res.Files, skillActions...)

	// 4. ~/.claude/commands/gortex-*.md — slash commands, also
	// codebase-agnostic and user-level. Claude Code discovers
	// user-level commands alongside project-level ones.
	cmdActions, err := installGlobalSlashCommands(w, env.Home, opts)
	if err != nil {
		logWarn(w, "could not install user-level slash commands: %v", err)
	}
	res.Files = append(res.Files, cmdActions...)

	return nil
}

// installPermissions merges an {"permissions": {"allow":
// ["mcp__gortex__*"]}} stanza into settings.json. Preserves any
// user-added entries; short-circuits when a gortex rule is already
// present.
func installPermissions(w io.Writer, settingsPath string, opts agents.ApplyOpts) (agents.FileAction, error) {
	return agents.MergeJSON(w, settingsPath, func(settings map[string]any, _ bool) (bool, error) {
		// Bail early if a gortex rule is already present.
		if perms, ok := settings["permissions"].(map[string]any); ok {
			if allow, ok := perms["allow"].([]any); ok {
				for _, entry := range allow {
					if s, ok := entry.(string); ok && strings.Contains(s, "mcp__gortex__") {
						return false, nil
					}
				}
			}
		}
		if _, ok := settings["permissions"]; !ok {
			settings["permissions"] = make(map[string]any)
		}
		perms := settings["permissions"].(map[string]any)
		if _, ok := perms["allow"]; !ok {
			perms["allow"] = []any{}
		}
		allow := perms["allow"].([]any)
		perms["allow"] = append(allow, "mcp__gortex__*")
		return true, nil
	}, opts)
}

// installGlobalSkills writes ~/.claude/skills/gortex-*/SKILL.md for
// each skill defined in GlobalSkills, skipping any that already
// exist (users may have customised their copy).
func installGlobalSkills(w io.Writer, home string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	out := make([]agents.FileAction, 0, len(GlobalSkills))
	skillsDir := filepath.Join(home, ".claude", "skills")
	for name, content := range GlobalSkills {
		dir := filepath.Join(skillsDir, name)
		path := filepath.Join(dir, "SKILL.md")
		action, err := agents.WriteIfNotExists(w, path, content, opts)
		if err != nil {
			return out, err
		}
		out = append(out, action)
	}
	return out, nil
}

// installGlobalSlashCommands writes ~/.claude/commands/gortex-*.md
// for each entry in SlashCommands. Skips existing files so users
// keep any local tweaks. Mirrors installGlobalSkills — both are
// user-level, codebase-agnostic artifacts installed by
// `gortex install`.
func installGlobalSlashCommands(w io.Writer, home string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	out := make([]agents.FileAction, 0, len(SlashCommands))
	dir := filepath.Join(home, ".claude", "commands")
	for name, content := range SlashCommands {
		path := filepath.Join(dir, name)
		action, err := agents.WriteIfNotExists(w, path, content, opts)
		if err != nil {
			return out, err
		}
		out = append(out, action)
	}
	return out, nil
}

// upsertGlobalMCPConfig is the user-level (~/.claude.json) MCP stanza
// installer. Unlike the project-level .mcp.json we never write from
// scratch with a static string here — we always merge so we don't
// clobber a user's other MCP servers or their permissions. If the
// existing file is malformed JSON, it's backed up before we
// overwrite.
func upsertGlobalMCPConfig(w io.Writer, path string, opts agents.ApplyOpts) (agents.FileAction, error) {
	exe, err := os.Executable()
	if err != nil {
		// Fall back to bare "gortex" on PATH. Reasonable for
		// homebrew / go install deployments.
		exe = "gortex"
	}
	entry := map[string]any{
		"command": exe,
		"args":    []string{"mcp"},
		"env":     map[string]any{},
	}

	// Try a direct merge first. MergeJSON handles malformed JSON
	// with a timestamped backup already.
	action, err := agents.MergeJSON(w, path, func(root map[string]any, existed bool) (bool, error) {
		_ = existed
		return agents.UpsertMCPServer(root, "gortex", entry, agents.ApplyOpts{Force: opts.Force}), nil
	}, opts)
	if err != nil {
		return agents.FileAction{}, err
	}

	// Historical behaviour: if the file existed but was malformed,
	// rename it to a .bak-<ts> to preserve the original. MergeJSON
	// uses a plain ".bak" name; we mirror the timestamp-suffix
	// convention for global mode because the user is more likely
	// to have edited ~/.claude.json than a project .mcp.json.
	if existing, statErr := os.Stat(path + ".bak"); statErr == nil && !existing.IsDir() {
		if err := os.Rename(path+".bak", fmt.Sprintf("%s.bak-%d", path, time.Now().Unix())); err != nil {
			logWarn(w, "could not timestamp malformed-config backup: %v", err)
		}
	}
	return action, nil
}

// Paths — user-level files.

func userClaudeJSONPath(home string) string {
	return filepath.Join(home, ".claude.json")
}

func userSettingsLocalPath(home string) string {
	return filepath.Join(home, ".claude", "settings.local.json")
}

// userSettingsPath is the user-level counterpart to
// `.claude/settings.json` in a project. Permissions live here (not
// in settings.local.json) so they survive when the user wipes the
// "local" overrides file.
func userSettingsPath(home string) string {
	return filepath.Join(home, ".claude", "settings.json")
}

// userClaudeMdPath is the machine-wide CLAUDE.md Claude Code reads on
// every session, regardless of cwd. We merge a marker-fenced rule
// block into it so the agent sees the Gortex rules from turn one.
func userClaudeMdPath(home string) string {
	return filepath.Join(home, ".claude", "CLAUDE.md")
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

func logWarn(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, "[gortex init] warning: "+format+"\n", args...)
}
