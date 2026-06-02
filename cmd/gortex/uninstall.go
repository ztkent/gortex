package main

import (
	"fmt"
	"io"
	"os"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/tui"
)

var (
	uninstallYes bool
)

// uninstallCmd is the canonical "remove Gortex from this project" command.
// `clean` is preserved as an alias because the original name predates this
// rename and is referenced in the wild — cobra resolves the alias before
// dispatching, so both verbs land in the same RunE.
var uninstallCmd = &cobra.Command{
	Use:     "uninstall",
	Aliases: []string{"clean"},
	Short:   "Uninstall Gortex from this repository (remove per-repo config + generated files)",
	Long: `Removes the per-repo Gortex footprint from the current directory:

  .mcp.json
  .claude/commands/
  .kiro/{steering,hooks,settings}/

Counterpart to ` + "`gortex init`" + `. For machine-wide setup (user MCP config,
~/.claude/CLAUDE.md block, user hooks) see ` + "`gortex install`" + ` — those
files are preserved by this command; remove them manually if needed.

Destructive by design: the interactive run shows a confirm wizard listing
every file before any deletion. Non-TTY callers must pass --yes to bypass
the wizard — that gate is intentional, the safer default is to refuse and
tell the user instead of silently nuking config.

The legacy ` + "`gortex clean`" + ` invocation is kept as an alias so existing
docs / scripts / muscle memory still work.`,
	RunE: runUninstall,
}

func init() {
	uninstallCmd.Flags().BoolVarP(&uninstallYes, "yes", "y", false, "skip the confirmation prompt (required when stdin is not a TTY)")
	rootCmd.AddCommand(uninstallCmd)
}

// uninstallTargets is the canonical list of files / directories `gortex
// uninstall` removes from a project. Kept as a package-level slice so the
// confirm wizard can preview them before any deletion happens.
var (
	uninstallFiles = []string{
		".mcp.json",
	}
	uninstallDirs = []string{
		".claude/commands",
		".kiro/steering",
		".kiro/hooks",
		".kiro/settings",
	}
)

// runUninstall is the per-repo removal entry point. Used by both
// `gortex uninstall` and `gortex clean` (the legacy alias) — same flow,
// same wizard, same summary card.
func runUninstall(cmd *cobra.Command, _ []string) error {
	w := cmd.ErrOrStderr()

	// Build the actual list of present targets so the wizard previews only
	// what would really be removed — listing every potential path even
	// when nothing exists feels noisy.
	presentFiles, presentDirs := filterPresentUninstallTargets()
	totalPresent := len(presentFiles) + len(presentDirs)

	if totalPresent == 0 {
		emitUninstallNothingTodo(w)
		return nil
	}

	// Gate the destructive op behind a confirm wizard when running on a TTY
	// and --yes / -y wasn't passed. Non-TTY callers without --yes get a
	// hard error so a misconfigured CI script can't silently nuke files.
	if !uninstallYes {
		if !progress.IsTTY(w) || noProgress {
			return fmt.Errorf("`gortex uninstall` is destructive; pass --yes to confirm (or run with a TTY for the interactive prompt)")
		}
		accepted, err := runUninstallConfirmWizard(w, presentFiles, presentDirs)
		if err != nil {
			return err
		}
		if !accepted {
			_, _ = fmt.Fprintln(w, "  cancelled — no files removed.")
			return nil
		}
	}

	removed, failures := executeUninstall(presentFiles, presentDirs)
	emitUninstallSummary(w, removed, failures, totalPresent)
	return nil
}

// filterPresentUninstallTargets returns the subset of uninstallFiles /
// uninstallDirs that actually exist on disk. Lets the wizard preview the
// real blast radius and skip the "nothing to uninstall" branch in one pass.
func filterPresentUninstallTargets() ([]string, []string) {
	var pf, pd []string
	for _, f := range uninstallFiles {
		if _, err := os.Stat(f); err == nil {
			pf = append(pf, f)
		}
	}
	for _, d := range uninstallDirs {
		if _, err := os.Stat(d); err == nil {
			pd = append(pd, d)
		}
	}
	return pf, pd
}

// runUninstallConfirmWizard renders a confirm wizard listing every target
// that would be removed, then waits for the user to accept or cancel.
// Returns accepted=true only on an explicit yes.
func runUninstallConfirmWizard(w io.Writer, files, dirs []string) (bool, error) {
	items := make([]string, 0, len(files)+len(dirs))
	for _, f := range files {
		items = append(items, f+"  "+progress.StyleHint.Render("(file)"))
	}
	for _, d := range dirs {
		items = append(items, d+"/  "+progress.StyleHint.Render("(directory + contents)"))
	}

	m := tui.NewConfirmModel(
		"gortex uninstall",
		"Remove all Gortex-generated config from this repo.",
	)
	m.Warning = "This cannot be undone — re-run `gortex init` to restore."
	m.Items = items
	m.YesLabel = "yes, remove " + strconv.Itoa(len(items)) + " item(s)"
	m.NoLabel = "no, keep them"

	prog := tea.NewProgram(m,
		tea.WithOutput(w),
		tea.WithAltScreen(),
		tea.WithoutSignalHandler(),
	)
	final, err := prog.Run()
	if err != nil {
		return false, fmt.Errorf("confirm: %w", err)
	}
	out, ok := final.(*tui.ConfirmModel)
	if !ok {
		return false, nil
	}
	return out.Accepted(), nil
}

// executeUninstall removes each present target and accumulates removed-count
// + per-target failures. Failures are warnings, not hard errors — partial
// success still emits a useful summary. (No writer dependency — caller emits
// the styled summary; this helper just does the disk work.)
func executeUninstall(files, dirs []string) (int, []string) {
	removed := 0
	var failures []string
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		removed++
	}
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			failures = append(failures, fmt.Sprintf("%s/: %v", d, err))
			continue
		}
		removed++
	}
	return removed, failures
}

// emitUninstallNothingTodo prints the empty-state message. One-liner on
// non-TTY, styled hint card on TTY.
func emitUninstallNothingTodo(w io.Writer) {
	if !progress.IsTTY(w) || noProgress {
		_, _ = fmt.Fprintln(w, "[gortex uninstall] nothing to uninstall")
		return
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "  "+progress.StyleHint.Render("◌  nothing to uninstall — no Gortex artefacts found in this directory"))
	_, _ = fmt.Fprintln(w)
}

// emitUninstallSummary prints the post-uninstall summary. Three signals:
// removed count, failures (if any), and the standing notes about CLAUDE.md /
// Kiro / Antigravity that haven't been touched (those live at user scope and
// belong to a future `gortex uninstall --global`).
func emitUninstallSummary(w io.Writer, removed int, failures []string, totalPresent int) {
	if !progress.IsTTY(w) || noProgress {
		if removed == 0 && len(failures) == 0 {
			_, _ = fmt.Fprintln(w, "[gortex uninstall] nothing to uninstall")
			return
		}
		for _, f := range failures {
			_, _ = fmt.Fprintf(w, "[gortex uninstall] failed: %s\n", f)
		}
		_, _ = fmt.Fprintf(w, "[gortex uninstall] done (%d/%d items removed)\n", removed, totalPresent)
		_, _ = fmt.Fprintln(w, "Note: CLAUDE.md was not modified — remove the Gortex block manually if needed.")
		_, _ = fmt.Fprintln(w, "Note: .kiro/steering/ files with 'gortex-' prefix were removed. Other .kiro/ files were preserved.")
		_, _ = fmt.Fprintln(w, "Note: Antigravity KIs are global and were not removed. Manually delete ~/.gemini/antigravity/knowledge/gortex-workflow if desired.")
		_, _ = fmt.Fprintln(w, "Note: Hermes config is global and was not removed. Manually delete the gortex entry in ~/.hermes/config.yaml (+ profiles) and ~/.hermes/skills/gortex if desired.")
		return
	}

	_, _ = fmt.Fprintln(w)
	stats := []string{progress.Stat(strconv.Itoa(removed), "removed", progress.StatGood)}
	if len(failures) > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(len(failures)), "failed", progress.StatBad))
	}
	_, _ = fmt.Fprintln(w, "  "+progress.StyleOK.Render("✓")+"  "+progress.StyleStrong.Render("uninstall complete"))
	_, _ = fmt.Fprintln(w, "     "+progress.StatStrip(stats...))

	if len(failures) > 0 {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "     "+progress.Heading("failures"))
		for _, f := range failures {
			_, _ = fmt.Fprintln(w, "       "+progress.StyleErr.Render("✗")+"  "+progress.StyleVal.Render(f))
		}
	}

	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "     "+progress.Heading("preserved"))
	_, _ = fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render("CLAUDE.md — remove the Gortex block manually if needed"))
	_, _ = fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render(".kiro/ files without the 'gortex-' prefix"))
	_, _ = fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render("~/.gemini/antigravity/knowledge/gortex-workflow (global)"))
	_, _ = fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render("~/.hermes/ config.yaml + skills (global)"))
	_, _ = fmt.Fprintln(w)
}
