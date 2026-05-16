package main

import (
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/hooks"
)

var (
	hookPort int
	hookMode string
)

var hookCmd = &cobra.Command{
	Use:    "hook",
	Short:  "Claude Code hook handler (dispatches PreToolUse / PostToolUse / PreCompact / Stop / SessionStart)",
	Hidden: true, // Not for direct user invocation.
	Run: func(_ *cobra.Command, _ []string) {
		hooks.Run(hookPort, hooks.ParseMode(hookMode))
	},
}

func init() {
	hookCmd.Flags().IntVar(&hookPort, "port", 8765, "Gortex web server port")
	hookCmd.Flags().StringVar(&hookMode, "mode", "deny",
		"hook posture: 'deny' (redirect Grep/Glob/Read of indexed source) or 'enrich' (never deny; PostToolUse appends graph context)")
	rootCmd.AddCommand(hookCmd)
}
