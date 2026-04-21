package main

import (
	"github.com/spf13/cobra"
)

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Retrieval-quality and token-savings benchmarks",
	Long: `Gortex evaluation harness.

Subcommands:
  recall     — recall@1/5/20 over a curated query fixture, per ranker
  embedders  — quality vs speed comparison across MiniLM-L6-v2 ONNX variants
  swebench   — run the SWE-bench harness (eval/run_eval.py)
  tokens     — GCX1 wire-format token savings vs JSON

Typical use:

  gortex eval recall --fixture bench/fixtures/retrieval.yaml --format markdown
  gortex eval embedders --variants fp32,qint8_arm64
  gortex eval swebench --list-configs
  gortex eval tokens --cases bench/wire-format/cases
`,
}

func init() {
	rootCmd.AddCommand(evalCmd)
}
