package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/daemon"
)

// Install-only flags. Repo-local `gortex init` has its own set —
// kept separate to avoid cross-pollination when wiring from the
// interactive wizard or the --json reporters.
var (
	installYes         bool
	installAgents      string
	installAgentsSkip  string
	installJSON        bool
	installDryRun      bool
	installForce       bool
	installHooks       = true
	installNoHooks     bool
	installClaudeMd    = true
	installNoClaudeMd  bool
	installStartDaemon bool
	installTrackRepo   bool
	installTrackPath   string
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install machine-wide Gortex integration for every detected AI assistant",
	Long: `Install Gortex at the user level: user-wide MCP config, user-level
skills / Knowledge Items, and (optionally) user-level hooks.

Run once per machine. Per-repo setup is done by ` + "`gortex init`" + ` inside
each repository.`,
	Args: cobra.NoArgs,
	RunE: runInstall,
}

func init() {
	installCmd.Flags().BoolVarP(&installYes, "yes", "y", false, "skip any interactive prompts (implied when stdin is not a TTY)")
	installCmd.Flags().StringVar(&installAgents, "agents", "", "comma-separated list of agents to configure ('auto' means every registered adapter)")
	installCmd.Flags().StringVar(&installAgentsSkip, "agents-skip", "", "comma-separated list of agents to skip (composable with --agents)")
	installCmd.Flags().BoolVar(&installJSON, "json", false, "emit a structured JSON report on stdout (banner still goes to stderr)")
	installCmd.Flags().BoolVar(&installDryRun, "dry-run", false, "plan writes without modifying disk")
	installCmd.Flags().BoolVar(&installForce, "force", false, "overwrite keys we would otherwise preserve during a merge")
	installCmd.Flags().BoolVar(&installHooks, "hooks", true, "install user-level Claude Code hooks; use --no-hooks to skip")
	installCmd.Flags().BoolVar(&installNoHooks, "no-hooks", false, "skip user-level Claude Code hooks (inverse of --hooks)")
	installCmd.Flags().BoolVar(&installClaudeMd, "claude-md", true, "merge Gortex rule block into ~/.claude/CLAUDE.md; use --no-claude-md to skip")
	installCmd.Flags().BoolVar(&installNoClaudeMd, "no-claude-md", false, "skip the ~/.claude/CLAUDE.md rule block (inverse of --claude-md)")
	installCmd.Flags().BoolVar(&installStartDaemon, "start", false, "start the daemon immediately after setup (detached)")
	installCmd.Flags().BoolVar(&installTrackRepo, "track", false, "track a repository with the daemon after setup")
	installCmd.Flags().StringVar(&installTrackPath, "track-path", ".", "repository to track when --track is set (default: current directory)")

	rootCmd.AddCommand(installCmd)
}

func runInstall(cmd *cobra.Command, _ []string) error {
	if installNoHooks {
		installHooks = false
	}
	if installNoClaudeMd {
		installClaudeMd = false
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	if home == "" {
		return fmt.Errorf("gortex install needs a home directory; $HOME is empty")
	}

	if installClaudeMd && !installDryRun {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"[gortex install] will merge a Gortex rule block into %s — use --no-claude-md to skip\n",
			filepath.Join(home, ".claude", "CLAUDE.md"))
	}

	env := agents.Env{
		// Root is still set so adapters that write to the daemon
		// config use the right cwd for "this is where I was run
		// from", but no per-repo files get written in ModeGlobal.
		Root:                      mustAbs(installTrackPath),
		Home:                      home,
		HookCommand:               claudecode.ResolveHookCommand(cmd.ErrOrStderr()),
		Mode:                      agents.ModeGlobal,
		InstallHooks:              installHooks,
		InstallGlobalInstructions: installClaudeMd,
		Stderr:                    cmd.ErrOrStderr(),
	}

	registry := buildRegistry()
	selected, err := registry.Filter(installAgents, installAgentsSkip)
	if err != nil {
		return err
	}

	opts := agents.ApplyOpts{DryRun: installDryRun, Force: installForce}
	results := make([]*agents.Result, 0, len(selected))
	for _, a := range selected {
		r, err := a.Apply(env, opts)
		if err != nil {
			// Claude Code is load-bearing — propagate. Other
			// adapters warn so one broken editor doesn't abort.
			if a.Name() == claudecode.Name {
				return fmt.Errorf("%s: %w", a.Name(), err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex install] warning: %s setup failed: %v\n", a.Name(), err)
		}
		if r != nil {
			results = append(results, r)
		}
	}

	// Ensure the daemon config file exists so --track doesn't
	// fail with "no config" on first use.
	if !installDryRun {
		if err := ensureGlobalConfigExists(); err != nil {
			return err
		}
		if err := runInstallFollowUps(cmd); err != nil {
			return err
		}
	}

	if installJSON {
		if err := emitInstallJSON(cmd.OutOrStdout(), results, opts); err != nil {
			return err
		}
	}
	emitInstallHuman(cmd.ErrOrStderr(), results, opts)
	return nil
}

// runInstallFollowUps runs the daemon-side post-setup operations
// that don't fit the Adapter interface: `--start` spawns the daemon
// detached, `--track` registers installTrackPath with the running
// daemon.
func runInstallFollowUps(cmd *cobra.Command) error {
	w := cmd.ErrOrStderr()

	if installStartDaemon {
		if daemon.IsRunning() {
			_, _ = fmt.Fprintln(w, "[gortex install] daemon already running (skipped --start)")
		} else {
			if err := spawnDetachedDaemon(); err != nil {
				return fmt.Errorf("start daemon: %w", err)
			}
			_, _ = fmt.Fprintln(w, "[gortex install] daemon started (detached)")
		}
	}

	if installTrackRepo {
		abs := mustAbs(installTrackPath)
		if !daemon.IsRunning() {
			_, _ = fmt.Fprintln(w, "[gortex install] ⚠ skipping --track: daemon is not running (try `gortex daemon start`)")
		} else {
			resp, err := trackViaDaemon(abs)
			if err != nil {
				return fmt.Errorf("track %s: %w", abs, err)
			}
			_, _ = fmt.Fprintf(w, "[gortex install] tracked %s (%s)\n", abs, resp)
		}
	}

	if !installStartDaemon {
		_, _ = fmt.Fprintln(w, "\nNext: start the daemon → `gortex daemon start --detach`")
	}
	if !installTrackRepo && installStartDaemon {
		_, _ = fmt.Fprintln(w, "Then: track a repo → `cd <repo> && gortex init` (or `gortex track <path>`)")
	}
	return nil
}

func emitInstallJSON(w interface{ Write([]byte) (int, error) }, results []*agents.Result, opts agents.ApplyOpts) error {
	payload := map[string]any{
		"dry_run": opts.DryRun,
		"force":   opts.Force,
		"mode":    "global",
		"agents":  results,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func emitInstallHuman(w interface{ Write([]byte) (int, error) }, results []*agents.Result, opts agents.ApplyOpts) {
	_, _ = fmt.Fprintf(w, "\n[gortex install] done")
	if opts.DryRun {
		_, _ = fmt.Fprintf(w, " (dry-run — no files written)")
	}
	_, _ = fmt.Fprintf(w, ":\n")
	for _, r := range results {
		if r == nil {
			continue
		}
		detected := "detected"
		if !r.Detected {
			detected = "not detected"
		}
		_, _ = fmt.Fprintf(w, "  • %s — %s, %d file(s) [%s]\n", r.Name, detected, len(r.Files), countByAction(r.Files))
	}
	_, _ = fmt.Fprintln(w, "\nNext: cd into any repo and run `gortex init` to wire repo-local config.")
}

func mustAbs(p string) string {
	if p == "" {
		p = "."
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
