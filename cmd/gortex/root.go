package main

import (
	"os"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	cfgFile    string
	logLevel   string
	noProgress bool
)

var rootCmd = &cobra.Command{
	Use:   "gortex",
	Short: "Code intelligence engine — indexes repos into a queryable knowledge graph",
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default .gortex.yaml)")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level: debug|info|warn|error")
	rootCmd.PersistentFlags().BoolVar(&noProgress, "no-progress", false, "disable the animated progress spinner (also honored: NO_COLOR, TERM=dumb, non-TTY stderr)")
}

func newLogger() *zap.Logger {
	level := zapcore.InfoLevel
	switch logLevel {
	case "debug":
		level = zapcore.DebugLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(level)
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	logger, err := cfg.Build()
	if err != nil {
		// Fallback.
		logger = zap.NewNop()
	}
	return logger
}

func execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
