package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove Gortex configuration and generated files from a project",
	RunE:  runClean,
}

func init() {
	rootCmd.AddCommand(cleanCmd)
}

func runClean(_ *cobra.Command, _ []string) error {
	files := []string{
		".mcp.json",
	}
	dirs := []string{
		".claude/commands",
		".kiro/steering",
		".kiro/hooks",
		".kiro/settings",
	}

	removed := 0

	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			if err := os.Remove(f); err != nil {
				fmt.Fprintf(os.Stderr, "[gortex clean] failed to remove %s: %v\n", f, err)
			} else {
				fmt.Fprintf(os.Stderr, "[gortex clean] removed %s\n", f)
				removed++
			}
		}
	}

	for _, d := range dirs {
		if _, err := os.Stat(d); err == nil {
			if err := os.RemoveAll(d); err != nil {
				fmt.Fprintf(os.Stderr, "[gortex clean] failed to remove %s: %v\n", d, err)
			} else {
				fmt.Fprintf(os.Stderr, "[gortex clean] removed %s/\n", d)
				removed++
			}
		}
	}

	if removed == 0 {
		fmt.Fprintln(os.Stderr, "[gortex clean] nothing to clean")
	} else {
		fmt.Fprintf(os.Stderr, "[gortex clean] done (%d items removed)\n", removed)
		fmt.Fprintln(os.Stderr, "Note: CLAUDE.md was not modified — remove the Gortex block manually if needed.")
		fmt.Fprintln(os.Stderr, "Note: .kiro/steering/ files with 'gortex-' prefix were removed. Other .kiro/ files were preserved.")
		fmt.Fprintln(os.Stderr, "Note: Antigravity KIs are global and were not removed. Manually delete ~/.gemini/antigravity/knowledge/gortex-workflow if desired.")
	}

	return nil
}
