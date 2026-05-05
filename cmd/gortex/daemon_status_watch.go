package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/progress"
)

// runDaemonStatusWatch launches the bubbletea-driven status TUI on a TTY.
// Falls back to a single static render on a pipe / when --no-progress is set.
func runDaemonStatusWatch(cmd *cobra.Command) error {
	w := cmd.OutOrStdout()
	if !progress.IsTTY(w) || noProgress {
		st, err := fetchDaemonStatusForCLI()
		if err != nil {
			return err
		}
		renderDaemonHeader(w, st)
		renderDaemonWorkspaces(w, st)
		renderDaemonRepos(w, st)
		renderDaemonSessions(w, st)
		renderDaemonServers(w, st)
		return nil
	}

	interval := max(daemonStatusInterval, 500*time.Millisecond)
	model := newStatusTUI(interval)
	prog := tea.NewProgram(
		model,
		tea.WithOutput(w),
		tea.WithAltScreen(),
	)
	_, err := prog.Run()
	return err
}
