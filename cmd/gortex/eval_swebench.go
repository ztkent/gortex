package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

var (
	evalSwebenchScript string
	evalSwebenchPython string
)

var evalSwebenchCmd = &cobra.Command{
	Use:                "swebench [args...]",
	Short:              "Run the SWE-bench harness (eval/run_eval.py)",
	Long:               `Thin passthrough to the Python SWE-bench harness in eval/. All positional arguments and flags after -- are forwarded to run_eval.py unchanged.`,
	DisableFlagParsing: true,
	RunE:               runEvalSwebench,
}

func init() {
	evalSwebenchCmd.Flags().StringVar(&evalSwebenchScript, "script", "eval/run_eval.py", "path to the Python harness (repo-relative or absolute)")
	evalSwebenchCmd.Flags().StringVar(&evalSwebenchPython, "python", "", "python interpreter (default: $PYTHON or python3)")
	evalCmd.AddCommand(evalSwebenchCmd)
}

func runEvalSwebench(_ *cobra.Command, args []string) error {
	script := evalSwebenchScript
	passthrough := args
	for i, a := range args {
		switch a {
		case "--script":
			if i+1 < len(args) {
				script = args[i+1]
				passthrough = append(append([]string{}, args[:i]...), args[i+2:]...)
			}
		case "--python":
			if i+1 < len(args) {
				evalSwebenchPython = args[i+1]
				passthrough = append(append([]string{}, args[:i]...), args[i+2:]...)
			}
		}
	}

	if _, err := os.Stat(script); err != nil {
		abs, _ := filepath.Abs(script)
		return fmt.Errorf("SWE-bench harness not found at %s: %w", abs, err)
	}

	py := evalSwebenchPython
	if py == "" {
		py = os.Getenv("PYTHON")
	}
	if py == "" {
		py = "python3"
	}

	cmd := exec.Command(py, append([]string{script}, passthrough...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
