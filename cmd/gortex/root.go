package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/zzet/gortex/internal/platform"
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
	// Runs before every subcommand (cobra walks to the nearest
	// PersistentPreRun; no subcommand defines its own). Fold any state
	// left by older versions in the split ~/.config / ~/.cache / flat
	// ~/.gortex layout into the unified ~/.gortex tree before a command
	// opens the store or reads config. Best-effort + idempotent, so it's
	// cheap on every run and silent after the first.
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		platform.MigrateToUnifiedHome(func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		})
	},
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
	assignCommandGroups()
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// assignCommandGroups organizes `gortex --help` into intent groups instead
// of one flat command list, with the MCP server (how editors and agents
// connect) called out first and the CLI commands grouped by what they do.
// Called once before Execute, after every command's init() has registered
// it. Commands left unmapped (internal/utility verbs) fall under cobra's
// "Additional Commands" heading.
func assignCommandGroups() {
	if rootCmd.ContainsGroup("serve") {
		return // idempotent — already grouped
	}
	rootCmd.AddGroup(
		&cobra.Group{ID: "serve", Title: "MCP server — connect editors & agents:"},
		&cobra.Group{ID: "engine", Title: "Daemon & repositories:"},
		&cobra.Group{ID: "query", Title: "Query & explore the graph:"},
		&cobra.Group{ID: "index", Title: "Index & enrich:"},
		&cobra.Group{ID: "setup", Title: "Setup & configuration:"},
	)
	groupOf := map[string]string{
		"mcp":    "serve",
		"daemon": "engine", "track": "engine", "untrack": "engine",
		"repos": "engine", "status": "engine", "proxy": "engine", "workspace": "engine",
		"query": "query", "context": "query", "audit": "query", "wiki": "query",
		"docs": "query", "export": "query", "wakeup": "query", "prs": "query",
		"review": "query",
		"index":  "index", "enrich": "index", "db": "index",
		"init": "setup", "install": "setup", "uninstall": "setup", "agents": "setup",
		"hook": "setup", "githook": "setup", "config": "setup",
		"provider": "setup", "plugin": "setup", "cloud": "setup",
	}
	for _, c := range rootCmd.Commands() {
		if id, ok := groupOf[c.Name()]; ok {
			c.GroupID = id
		}
	}
}
