// Package hermes implements the Gortex init/install integration for
// NousResearch Hermes (https://github.com/NousResearch/hermes-agent),
// a CLI agent-orchestrator that consumes MCP servers.
//
// Hermes is a user-level agent, not a repo-scoped IDE: it stores all
// state under ~/.hermes/ and the gortex daemon already resolves the
// active workspace per MCP session, so one global server entry serves
// every repo. We therefore write user-level artifacts in both
// `gortex init` (ModeProject) and `gortex install` (ModeGlobal), the
// same as the openclaw / antigravity adapters — the writes are
// idempotent, so running both is harmless.
//
// Three surfaces are configured:
//
//  1. Global ~/.hermes/config.yaml — upsert a `gortex` stdio server
//     under the snake_case `mcp_servers` map, comment-preservingly
//     (the config is hand-edited and comment-rich).
//  2. Every existing ~/.hermes/profiles/<name>/config.yaml — Hermes
//     profiles can re-declare their own `mcp_servers` block rather
//     than inheriting the global one, so we upsert the gortex stanza
//     into each profile config that already exists. This guarantees
//     every profile resolves the gortex tools regardless of the
//     global↔profile merge semantics. (We never create new profiles.)
//  3. A user-level skill at ~/.hermes/skills/gortex/SKILL.md teaching
//     the agent to prefer gortex graph tools — Hermes' equivalent of
//     the Claude Code / Antigravity user-level instruction surface.
package hermes

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
	yaml "gopkg.in/yaml.v3"
)

const Name = "hermes"
const DocsURL = "https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect returns true when Hermes is installed or its home directory
// exists. False means "skip", not an error — a machine without Hermes
// gets no ~/.hermes writes.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("hermes"); err == nil && p != "" {
		return true, nil
	}
	if env.Home == "" {
		return false, nil
	}
	if _, err := os.Stat(hermesDir(env.Home)); err == nil {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	if env.Home == "" {
		return &agents.Plan{}, nil
	}
	files := []agents.FileAction{
		{Path: globalConfigPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"mcp_servers"}},
	}
	for _, p := range profileConfigPaths(env.Home) {
		files = append(files, agents.FileAction{Path: p, Action: agents.ActionWouldMerge, Keys: []string{"mcp_servers"}})
	}
	files = append(files, agents.FileAction{Path: skillPath(env.Home), Action: agents.ActionWouldCreate})
	return &agents.Plan{Files: files}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected {
		internalutil.Logf(env.Stderr, "[gortex init] skip Hermes setup (hermes not detected)")
		return res, nil
	}
	if env.Home == "" {
		return res, fmt.Errorf("hermes: requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Hermes integration...")

	command := resolveGortexCommand()

	// 1. Global config — the entry every profile inherits when it
	//    doesn't re-declare its own server map.
	globalAction, err := upsertGortexServer(env.Stderr, globalConfigPath(env.Home), command, opts)
	if err != nil {
		return res, fmt.Errorf("hermes global config: %w", err)
	}
	res.Files = append(res.Files, globalAction)

	// 2. Per-profile configs — Hermes profiles may carry their own
	//    mcp_servers block, so upsert into each existing one too. A
	//    failure on one profile is a warning, not fatal: the global
	//    entry still covers profiles that do inherit.
	for _, profilePath := range profileConfigPaths(env.Home) {
		profileAction, perr := upsertGortexServer(env.Stderr, profilePath, command, opts)
		if perr != nil {
			internalutil.Warnf(env.Stderr, "hermes profile %s: %v", profilePath, perr)
			continue
		}
		res.Files = append(res.Files, profileAction)
	}

	// 3. User-level skill — teach the agent to prefer gortex tools.
	//    Skipped when it already exists so user edits survive.
	skillAction, err := agents.WriteIfNotExists(env.Stderr, skillPath(env.Home), SkillBody, opts)
	if err != nil {
		return res, fmt.Errorf("hermes skill: %w", err)
	}
	res.Files = append(res.Files, skillAction)

	res.Configured = true
	return res, nil
}

// upsertGortexServer merges the gortex stdio stanza into the
// `mcp_servers` map of a Hermes YAML config, preserving comments and
// unrelated keys.
func upsertGortexServer(w io.Writer, path, command string, opts agents.ApplyOpts) (agents.FileAction, error) {
	return agents.MergeYAML(w, path, func(root *yaml.Node, _ bool) (bool, error) {
		return agents.UpsertYAMLMapEntry(root, "mcp_servers", gortexServerName, gortexMCPEntry(command), opts.Force), nil
	}, opts)
}

// resolveGortexCommand returns the absolute path to the running gortex
// binary so Hermes can launch it regardless of how its subprocess PATH
// is set up, falling back to the bare "gortex" name (homebrew /
// go install deployments put it on PATH).
func resolveGortexCommand() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "gortex"
}

// hermesDir is the ~/.hermes root.
func hermesDir(home string) string { return filepath.Join(home, ".hermes") }

// globalConfigPath is ~/.hermes/config.yaml.
func globalConfigPath(home string) string { return filepath.Join(hermesDir(home), "config.yaml") }

// skillPath is ~/.hermes/skills/gortex/SKILL.md.
func skillPath(home string) string {
	return filepath.Join(hermesDir(home), "skills", SkillName, "SKILL.md")
}

// profileConfigPaths returns the config.yaml of every existing Hermes
// profile under ~/.hermes/profiles/<name>/, sorted for a stable
// install report and deterministic tests. Returns nil when the
// profiles directory is absent.
func profileConfigPaths(home string) []string {
	matches, err := filepath.Glob(filepath.Join(hermesDir(home), "profiles", "*", "config.yaml"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	sort.Strings(matches)
	return matches
}
