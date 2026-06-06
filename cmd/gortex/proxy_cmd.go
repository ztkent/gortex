package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

// proxyCmd is the canonical surface for managing the remote roster the
// local daemon federates/proxies to. `gortex daemon server add/remove`
// remain as hidden deprecated aliases that delegate here.
var proxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Manage the remote Gortex daemons this daemon federates with",
	Long: `Maintain ~/.gortex/servers.toml — the roster of remote Gortex daemons
the local daemon federates read queries to and proxies scoped calls to.

  gortex proxy add <slug> <url>   register a remote
  gortex proxy on|off <slug>      enable / disable federation to a remote
  gortex proxy list|status        inspect the roster

Toggling a remote persists across restarts (` + "`off`" + ` writes
enabled=false; ` + "`on`" + ` clears the key, back to default-on) and is
applied to a running daemon without a restart when possible.`,
}

var proxyListCmd = &cobra.Command{
	Use:          "list",
	Short:        "Print the remote roster",
	RunE:         runProxyList,
	SilenceUsage: true,
}

var proxyStatusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Show each remote's enabled state and reachability",
	RunE:         runProxyStatus,
	SilenceUsage: true,
}

var proxyOnCmd = &cobra.Command{
	Use:          "on <slug>",
	Short:        "Enable federation/proxy to a remote (default-on; clears the key)",
	Args:         cobra.ExactArgs(1),
	RunE:         func(_ *cobra.Command, args []string) error { return runProxyToggle(args[0], true) },
	SilenceUsage: true,
}

var proxyOffCmd = &cobra.Command{
	Use:          "off <slug>",
	Short:        "Disable federation/proxy to a remote (persists across restarts)",
	Args:         cobra.ExactArgs(1),
	RunE:         func(_ *cobra.Command, args []string) error { return runProxyToggle(args[0], false) },
	SilenceUsage: true,
}

var proxyAddCmd = &cobra.Command{
	Use:          "add <slug> <url>",
	Short:        "Add a remote to the roster (http://, https://, or unix:///path)",
	Args:         cobra.ExactArgs(2),
	RunE:         runProxyAdd,
	SilenceUsage: true,
}

var proxyRemoveCmd = &cobra.Command{
	Use:          "remove <slug>",
	Aliases:      []string{"rm"},
	Short:        "Remove a remote from the roster",
	Args:         cobra.ExactArgs(1),
	RunE:         runProxyRemove,
	SilenceUsage: true,
}

func init() {
	// proxy add reuses the daemon-server-add flag vars + logic so the
	// two surfaces stay in lockstep (D-27 canonical form).
	proxyAddCmd.Flags().BoolVar(&daemonServerAddDefault, "default", false, "mark this remote as the default")
	proxyAddCmd.Flags().StringVar(&daemonServerAddAuthToken, "auth-token", "", "literal bearer token (prefer --auth-token-env)")
	proxyAddCmd.Flags().StringVar(&daemonServerAddAuthTokenEnv, "auth-token-env", "", "env-var name the daemon reads at request time")
	proxyAddCmd.Flags().StringSliceVar(&daemonServerAddWorkspaces, "workspaces", nil, "pre-declared workspace slugs (comma-separated)")
	proxyAddCmd.Flags().BoolVar(&daemonServerAddReadOnly, "read-only", false, "mark this remote read-only (no write tools routed to it)")

	proxyCmd.AddCommand(proxyListCmd, proxyStatusCmd, proxyOnCmd, proxyOffCmd, proxyAddCmd, proxyRemoveCmd)
	rootCmd.AddCommand(proxyCmd)
}

func runProxyList(_ *cobra.Command, _ []string) error {
	cfg, err := daemon.LoadServersConfig("")
	if err != nil {
		return err
	}
	if cfg == nil || len(cfg.Server) == 0 {
		fmt.Fprintf(os.Stderr, "[gortex] no remotes configured (%s is empty or missing)\n", daemon.ServersConfigPath())
		return nil
	}
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.AppendHeader(table.Row{"slug", "url", "enabled", "read-only", "default", "auth"})
	for _, s := range cfg.Server {
		t.AppendRow(table.Row{
			s.Slug,
			s.URL,
			yesNo(s.IsEnabled()),
			yesNo(s.ReadOnly),
			yesNo(s.Default),
			authLabel(s),
		})
	}
	t.Render()
	return nil
}

func runProxyStatus(_ *cobra.Command, _ []string) error {
	cfg, err := daemon.LoadServersConfig("")
	if err != nil {
		return err
	}
	if cfg == nil || len(cfg.Server) == 0 {
		fmt.Fprintf(os.Stderr, "[gortex] no remotes configured (%s is empty or missing)\n", daemon.ServersConfigPath())
		return nil
	}
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.AppendHeader(table.Row{"slug", "url", "enabled", "reachable", "advertised"})
	for _, s := range cfg.Server {
		reach, advert := probeRemote(s)
		t.AppendRow(table.Row{s.Slug, s.URL, yesNo(s.IsEnabled()), reach, advert})
	}
	t.Render()
	return nil
}

// probeRemote does a short best-effort /v1/health fetch so `proxy status`
// can show liveness and the remote's advertised posture. Failures are
// reported, never fatal.
func probeRemote(s daemon.ServerEntry) (reachable, advertised string) {
	cli, err := daemon.NewServerClient(s)
	if err != nil {
		return "no (" + err.Error() + ")", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := cli.FetchHealth(ctx)
	if err != nil {
		return "no", ""
	}
	posture := "rw"
	if h.EffectiveReadOnly() {
		posture = "ro"
	}
	caps := ""
	if len(h.Capabilities) > 0 {
		caps = " [" + strings.Join(h.Capabilities, ",") + "]"
	}
	return "yes", posture + caps
}

func runProxyToggle(slug string, enabled bool) error {
	cfg, err := daemon.LoadServersConfig("")
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &daemon.ServersConfig{}
	}
	changed, err := cfg.SetEnabled(slug, enabled)
	if err != nil {
		return err // unknown slug — surfaced to the user
	}
	if err := cfg.Save(""); err != nil {
		return err
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	if !changed {
		fmt.Fprintf(os.Stderr, "[gortex] remote %q already %s\n", slug, state)
		return nil
	}
	fmt.Fprintf(os.Stderr, "[gortex] remote %q %s in %s\n", slug, state, daemon.ServersConfigPath())
	proxyApplyToRunningDaemon()
	return nil
}

func runProxyAdd(cmd *cobra.Command, args []string) error {
	// Positional <url> rides into the shared add path via its flag var.
	daemonServerAddURL = args[1]
	return runDaemonServerAdd(cmd, args[:1])
}

func runProxyRemove(cmd *cobra.Command, args []string) error {
	return runDaemonServerRemove(cmd, args)
}

// proxyApplyToRunningDaemon best-effort applies a roster change to a
// running daemon via the ControlProxy live-reload RPC. When the daemon
// is down or does not yet understand the verb, it degrades to the
// restart hint — never an error (the durable config write already
// succeeded).
func proxyApplyToRunningDaemon() {
	if !daemon.IsRunning() {
		return
	}
	c, err := daemonControlClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[gortex] note: run `gortex daemon restart` to apply to the running daemon")
		return
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Control(daemon.ControlProxy, nil)
	if err != nil || !resp.OK {
		fmt.Fprintln(os.Stderr, "[gortex] note: run `gortex daemon restart` to apply to the running daemon")
		return
	}
	fmt.Fprintln(os.Stderr, "[gortex] applied to the running daemon (no restart needed)")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

func authLabel(s daemon.ServerEntry) string {
	switch {
	case s.AuthTokenEnv != "":
		return "env:" + s.AuthTokenEnv
	case s.AuthToken != "":
		return "literal"
	}
	return ""
}
