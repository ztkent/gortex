package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

// Service management — launchd on macOS, systemd --user on Linux.
//
// The service wraps `gortex daemon start` (foreground, without --detach)
// so the OS supervisor owns lifecycle. Users who prefer to manage the
// daemon manually keep using `gortex daemon start --detach`; the
// service integration is strictly opt-in via `install-service`.
//
// No root privileges. On both platforms the unit lives under the
// user's home so it starts with the user session and terminates on
// logout. System-wide installation (requires sudo) is a follow-up for
// shared-workstation deployments; not wired here.

var (
	daemonServiceName = "com.zzet.gortex" // launchd label + systemd unit base name
)

var daemonInstallServiceCmd = &cobra.Command{
	Use:   "install-service",
	Short: "Install a user-level launchd/systemd unit that keeps the daemon running",
	Long: `Writes a user-level service unit for the host OS (launchd on macOS,
systemd --user on Linux) that starts the daemon at login and restarts
it on crash. The service wraps 'gortex daemon start' in foreground mode
so the OS supervisor owns lifecycle; no --detach involved.

No root/sudo required — unit lives under your home directory.`,
	RunE: runDaemonInstallService,
}

var daemonUninstallServiceCmd = &cobra.Command{
	Use:   "uninstall-service",
	Short: "Remove the launchd/systemd unit and stop the daemon",
	RunE:  runDaemonUninstallService,
}

var daemonServiceStatusCmd = &cobra.Command{
	Use:   "service-status",
	Short: "Show whether the launchd/systemd unit is installed and active",
	RunE:  runDaemonServiceStatus,
}

func init() {
	daemonCmd.AddCommand(daemonInstallServiceCmd)
	daemonCmd.AddCommand(daemonUninstallServiceCmd)
	daemonCmd.AddCommand(daemonServiceStatusCmd)
}

// runDaemonInstallService writes the unit file and enables + starts it.
// Existing daemon processes are stopped first so the service owns the
// only running instance after this returns.
func runDaemonInstallService(cmd *cobra.Command, _ []string) error {
	w := cmd.ErrOrStderr()

	// Stop any manual daemon that's currently running so the service
	// doesn't fight with it over the socket.
	if daemon.IsRunning() {
		_, _ = fmt.Fprintln(w, "[gortex daemon] stopping existing daemon before install")
		// This is an internal lifecycle bounce — the supervisor starts the
		// daemon right after — not a user "stay down". Suppress the stop-intent
		// marker (as `daemon restart` does) so a failed install can't leave
		// autostart permanently disabled with no daemon and no service.
		daemonRestartActive = true
		err := runDaemonStop(cmd, nil)
		daemonRestartActive = false
		if err != nil {
			return fmt.Errorf("stop existing daemon: %w", err)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(w, exe)
	case "linux":
		return installSystemd(w, exe)
	default:
		return fmt.Errorf("service install is not supported on %s — run 'gortex daemon start --detach' to keep the daemon running in the background", runtime.GOOS)
	}
}

func runDaemonUninstallService(cmd *cobra.Command, _ []string) error {
	w := cmd.ErrOrStderr()
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd(w)
	case "linux":
		return uninstallSystemd(w)
	default:
		return fmt.Errorf("service uninstall not supported on %s", runtime.GOOS)
	}
}

func runDaemonServiceStatus(cmd *cobra.Command, _ []string) error {
	w := cmd.OutOrStdout()
	switch runtime.GOOS {
	case "darwin":
		return statusLaunchd(w)
	case "linux":
		return statusSystemd(w)
	default:
		return fmt.Errorf("service status not supported on %s", runtime.GOOS)
	}
}

// --- launchd (macOS) ------------------------------------------------------

// launchdPlistTemplate renders the LaunchAgent plist. KeepAlive uses the
// SuccessfulExit=false policy so the agent is restarted on a crash but NOT on a
// clean exit — the launchd analogue of systemd's Restart=on-failure, so an
// explicit `gortex daemon stop` (which exits 0) stays down instead of being
// resurrected by KeepAlive. RunAtLoad starts on login.
//
// StandardOutPath / StandardErrorPath redirect logs into the same file
// `gortex daemon logs` tails, so users don't need to remember two paths.
const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.Exe}}</string>
        <string>daemon</string>
        <string>start</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>{{.LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{.LogPath}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>
    </dict>
</dict>
</plist>
`

func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", daemonServiceName+".plist"), nil
}

func installLaunchd(w io.Writer, exe string) error {
	path, err := launchdPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure LaunchAgents dir: %w", err)
	}

	var buf bytes.Buffer
	if err := template.Must(template.New("plist").Parse(launchdPlistTemplate)).Execute(&buf, map[string]string{
		"Label":   daemonServiceName,
		"Exe":     exe,
		"LogPath": daemon.LogFilePath(),
	}); err != nil {
		return fmt.Errorf("render plist: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	// -w persists the load across reboots; without it the service
	// starts only for the current login session.
	if err := runCmd(w, "launchctl", "load", "-w", path); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}
	_, _ = fmt.Fprintf(w, "[gortex daemon] service installed at %s\n", path)
	_, _ = fmt.Fprintf(w, "  logs: %s\n", daemon.LogFilePath())
	_, _ = fmt.Fprintf(w, "  check: gortex daemon service-status\n")
	return nil
}

func uninstallLaunchd(w io.Writer) error {
	path, err := launchdPlistPath()
	if err != nil {
		return err
	}
	// unload is idempotent — emits a warning if the plist isn't loaded,
	// but exit 0. Swallow its error path so uninstall is safe to retry.
	_ = runCmd(w, "launchctl", "unload", "-w", path)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove plist: %w", err)
	}
	_, _ = fmt.Fprintln(w, "[gortex daemon] service uninstalled")
	return nil
}

func statusLaunchd(w io.Writer) error {
	path, err := launchdPlistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintf(w, "launchd: not installed (expected %s)\n", path)
		return nil
	}
	_, _ = fmt.Fprintf(w, "launchd: installed at %s\n", path)

	// `launchctl list <label>` returns 0 and a plist-ish blob when loaded,
	// non-zero otherwise.
	out, err := exec.Command("launchctl", "list", daemonServiceName).CombinedOutput()
	if err != nil {
		_, _ = fmt.Fprintln(w, "status: not loaded — try `launchctl load ~/Library/LaunchAgents/"+daemonServiceName+".plist`")
		return nil
	}
	_, _ = fmt.Fprintln(w, "status: loaded")
	// Extract PID and LastExitStatus lines for a concise summary.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "\"PID\"") || strings.HasPrefix(line, "\"LastExitStatus\"") {
			_, _ = fmt.Fprintln(w, "  "+line)
		}
	}
	return nil
}

// --- systemd --user (Linux) ----------------------------------------------

// systemdUnitTemplate renders a user-level systemd service. Type=simple
// because `gortex daemon start` (without --detach) runs in the
// foreground; Restart=on-failure covers the crash-restart case without
// pounding on successful exits.
const systemdUnitTemplate = `[Unit]
Description=Gortex code intelligence daemon
Documentation=https://github.com/zzet/gortex
After=network.target

[Service]
Type=simple
ExecStart={{.Exe}} daemon start
Restart=on-failure
RestartSec=2
StandardOutput=append:{{.LogPath}}
StandardError=append:{{.LogPath}}

[Install]
WantedBy=default.target
`

func systemdUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", daemonServiceName+".service"), nil
}

func installSystemd(w io.Writer, exe string) error {
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("ensure systemd user dir: %w", err)
	}

	var buf bytes.Buffer
	if err := template.Must(template.New("unit").Parse(systemdUnitTemplate)).Execute(&buf, map[string]string{
		"Exe":     exe,
		"LogPath": daemon.LogFilePath(),
	}); err != nil {
		return fmt.Errorf("render unit: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := runCmd(w, "systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	// `enable --now` = enable for autostart + start immediately. Bundled
	// so the user gets a running service in one command.
	if err := runCmd(w, "systemctl", "--user", "enable", "--now", daemonServiceName); err != nil {
		return fmt.Errorf("systemctl enable --now: %w", err)
	}
	_, _ = fmt.Fprintf(w, "[gortex daemon] service installed at %s\n", path)
	_, _ = fmt.Fprintf(w, "  logs: %s (or `journalctl --user -u %s -f`)\n", daemon.LogFilePath(), daemonServiceName)
	_, _ = fmt.Fprintf(w, "  check: gortex daemon service-status\n")
	return nil
}

func uninstallSystemd(w io.Writer) error {
	// `disable --now` = stop + disable autostart. Swallowed so uninstall
	// is safe to run on a never-installed system.
	_ = runCmd(w, "systemctl", "--user", "disable", "--now", daemonServiceName)

	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove unit: %w", err)
	}
	_ = runCmd(w, "systemctl", "--user", "daemon-reload")
	_, _ = fmt.Fprintln(w, "[gortex daemon] service uninstalled")
	return nil
}

func statusSystemd(w io.Writer) error {
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintf(w, "systemd: not installed (expected %s)\n", path)
		return nil
	}
	_, _ = fmt.Fprintf(w, "systemd: installed at %s\n", path)
	out, err := exec.Command("systemctl", "--user", "is-active", daemonServiceName).CombinedOutput()
	status := strings.TrimSpace(string(out))
	_, _ = fmt.Fprintf(w, "status: %s", status)
	if err != nil {
		// is-active exits 3 for "inactive" — not really a CLI error.
		_, _ = fmt.Fprintln(w)
		return nil
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

// runCmd is a helper that runs an external command with its stdout /
// stderr piped through the caller's writer so users see launchctl /
// systemctl output in context with our own.
func runCmd(w io.Writer, name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = w
	c.Stderr = w
	return c.Run()
}
