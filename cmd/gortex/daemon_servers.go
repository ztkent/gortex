package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

var (
	daemonServerAddURL          string
	daemonServerAddDefault      bool
	daemonServerAddAuthToken    string
	daemonServerAddAuthTokenEnv string
	daemonServerAddWorkspaces   []string
)

var daemonServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage the multi-server roster in ~/.gortex/servers.toml",
	Long: `Maintain ~/.gortex/servers.toml — the multi-server roster the daemon
and the gortex server load on startup to wire up local-fast-path vs
proxy routing.

The file holds one [[server]] block per reachable Gortex server: a
local socket, a remote HTTPS endpoint, etc. Changes take effect on
the next daemon/server start; run "gortex daemon restart" after
adding or removing entries on a running daemon.`,
}

var daemonServerAddCmd = &cobra.Command{
	Use:   "add <slug>",
	Short: "Add a server entry to ~/.gortex/servers.toml",
	Long: `Append a new server to the roster. Slug must be unique. URL accepts
http://, https://, or unix:///path/to.sock. Auth: pass --auth-token-env
(preferred) to point at an environment variable, or --auth-token to
write the literal value into the file.

If --default is set and another entry already has default=true, the
command fails — remove or unflag the existing default first.`,
	Args:         cobra.ExactArgs(1),
	RunE:         runDaemonServerAdd,
	SilenceUsage: true,
}

var daemonServerRemoveCmd = &cobra.Command{
	Use:          "remove <slug>",
	Aliases:      []string{"rm"},
	Short:        "Remove a server entry from ~/.gortex/servers.toml",
	Args:         cobra.ExactArgs(1),
	RunE:         runDaemonServerRemove,
	SilenceUsage: true,
}

var daemonServerListCmd = &cobra.Command{
	Use:          "list",
	Short:        "Print the current ~/.gortex/servers.toml roster",
	RunE:         runDaemonServerList,
	SilenceUsage: true,
}

func init() {
	daemonServerAddCmd.Flags().StringVar(&daemonServerAddURL, "url", "", "server URL (http://, https://, or unix:///path) — required")
	daemonServerAddCmd.Flags().BoolVar(&daemonServerAddDefault, "default", false, "mark this entry as the default server")
	daemonServerAddCmd.Flags().StringVar(&daemonServerAddAuthToken, "auth-token", "", "literal bearer token (consider --auth-token-env instead)")
	daemonServerAddCmd.Flags().StringVar(&daemonServerAddAuthTokenEnv, "auth-token-env", "", "env-var name the daemon reads at request time")
	daemonServerAddCmd.Flags().StringSliceVar(&daemonServerAddWorkspaces, "workspaces", nil, "pre-declared workspace slugs (comma-separated)")
	_ = daemonServerAddCmd.MarkFlagRequired("url")

	daemonServerCmd.AddCommand(daemonServerAddCmd)
	daemonServerCmd.AddCommand(daemonServerRemoveCmd)
	daemonServerCmd.AddCommand(daemonServerListCmd)
	daemonCmd.AddCommand(daemonServerCmd)
}

func runDaemonServerAdd(_ *cobra.Command, args []string) error {
	slug := args[0]
	cfg, err := daemon.LoadServersConfig("")
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &daemon.ServersConfig{}
	}
	entry := daemon.ServerEntry{
		Slug:         slug,
		URL:          daemonServerAddURL,
		AuthToken:    daemonServerAddAuthToken,
		AuthTokenEnv: daemonServerAddAuthTokenEnv,
		Workspaces:   daemonServerAddWorkspaces,
		Default:      daemonServerAddDefault,
	}
	if err := cfg.AddServer(entry); err != nil {
		return err
	}
	if err := cfg.Save(""); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gortex] added server %q (%s) to %s\n", slug, daemonServerAddURL, daemon.ServersConfigPath())
	if daemon.IsRunning() {
		fmt.Fprintln(os.Stderr, "[gortex] note: run `gortex daemon restart` to apply changes to the running daemon")
	}
	return nil
}

func runDaemonServerRemove(_ *cobra.Command, args []string) error {
	slug := args[0]
	cfg, err := daemon.LoadServersConfig("")
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &daemon.ServersConfig{}
	}
	removed, err := cfg.RemoveServer(slug)
	if err != nil {
		return err
	}
	if !removed {
		fmt.Fprintf(os.Stderr, "[gortex] no server with slug %q in %s — nothing to remove\n", slug, daemon.ServersConfigPath())
		return nil
	}
	if err := cfg.Save(""); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gortex] removed server %q from %s\n", slug, daemon.ServersConfigPath())
	if daemon.IsRunning() {
		fmt.Fprintln(os.Stderr, "[gortex] note: run `gortex daemon restart` to apply changes to the running daemon")
	}
	return nil
}

func runDaemonServerList(_ *cobra.Command, _ []string) error {
	cfg, err := daemon.LoadServersConfig("")
	if err != nil {
		return err
	}
	if cfg == nil || len(cfg.Server) == 0 {
		fmt.Fprintf(os.Stderr, "[gortex] no servers configured (%s is empty or missing)\n", daemon.ServersConfigPath())
		return nil
	}
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.AppendHeader(table.Row{"slug", "url", "default", "auth", "workspaces"})
	yesno := func(b bool) string {
		if b {
			return "yes"
		}
		return ""
	}
	for _, s := range cfg.Server {
		auth := ""
		switch {
		case s.AuthTokenEnv != "":
			auth = "env:" + s.AuthTokenEnv
		case s.AuthToken != "":
			auth = "literal"
		}
		t.AppendRow(table.Row{
			s.Slug,
			s.URL,
			yesno(s.Default),
			auth,
			strings.Join(s.Workspaces, ", "),
		})
	}
	t.Render()
	return nil
}
