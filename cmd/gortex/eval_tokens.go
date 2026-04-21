package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

var (
	evalTokensCases  string
	evalTokensOut    string
	evalTokensJSON   string
	evalTokensBinary string
)

var evalTokensCmd = &cobra.Command{
	Use:   "tokens",
	Short: "GCX1 wire-format token savings vs JSON (bench/wire-format)",
	Long: `Runs the GCX1 vs JSON size/token benchmark over fixture payloads in
bench/wire-format/cases. Thin wrapper around the existing bench/wire-format
binary — we delegate rather than duplicate the encoder matrix.

The wrapper builds and runs bench/wire-format on demand. Use --binary to
point at a prebuilt artifact if you want to pin versions.`,
	RunE: runEvalTokens,
}

func init() {
	evalTokensCmd.Flags().StringVar(&evalTokensCases, "cases", "bench/wire-format/cases", "directory of fixture YAML files")
	evalTokensCmd.Flags().StringVar(&evalTokensOut, "out", "", "scorecard markdown output path (default: stdout)")
	evalTokensCmd.Flags().StringVar(&evalTokensJSON, "json", "", "optional raw metrics JSON output path")
	evalTokensCmd.Flags().StringVar(&evalTokensBinary, "binary", "", "prebuilt bench/wire-format binary (default: go run ./bench/wire-format)")
	evalCmd.AddCommand(evalTokensCmd)
}

func runEvalTokens(_ *cobra.Command, _ []string) error {
	if _, err := os.Stat(evalTokensCases); err != nil {
		abs, _ := filepath.Abs(evalTokensCases)
		return fmt.Errorf("cases directory not found at %s: %w", abs, err)
	}

	args := []string{"-cases", evalTokensCases}
	if evalTokensOut != "" {
		args = append(args, "-out", evalTokensOut)
	}
	if evalTokensJSON != "" {
		args = append(args, "-json", evalTokensJSON)
	}

	var cmd *exec.Cmd
	if evalTokensBinary != "" {
		cmd = exec.Command(evalTokensBinary, args...)
	} else {
		cmd = exec.Command("go", append([]string{"run", "./bench/wire-format"}, args...)...)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
