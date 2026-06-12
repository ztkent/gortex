package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Supervisor-aware stop / restart.
//
// When the daemon is owned by an OS supervisor installed via
// `gortex daemon install-service` (systemd --user on Linux, launchd on macOS),
// a plain socket-level `daemon stop` just kills the worker — and the supervisor
// promptly restarts it (launchd KeepAlive; systemd on a non-clean exit). So
// when a supervisor owns the daemon we drive the lifecycle THROUGH it:
//
//   - stop    → tell the supervisor to stop the unit (stays installed/enabled),
//   - restart → tell the supervisor to bounce the unit, keeping its ownership.
//
// A manual stop+start under a supervisor would orphan the freshly-started
// daemon from the unit (the unit reads inactive while a hand-started process
// holds the socket), so restart must route through the supervisor too.
//
// The three entry points are function vars so the routing in runDaemonStop /
// runDaemonRestart is testable without a real systemd / launchd.
var (
	serviceActive  = defaultServiceActive
	serviceStop    = defaultServiceStop
	serviceRestart = defaultServiceRestart
)

// defaultServiceActive reports whether an installed service unit currently owns
// the daemon (unit file present AND the supervisor reports it active/loaded).
// It short-circuits on a missing unit file so the common, unsupervised case
// never shells out.
func defaultServiceActive() bool {
	switch runtime.GOOS {
	case "linux":
		path, err := systemdUnitPath()
		if err != nil {
			return false
		}
		if _, err := os.Stat(path); err != nil {
			return false
		}
		out, _ := exec.Command("systemctl", "--user", "is-active", daemonServiceName).Output()
		return strings.TrimSpace(string(out)) == "active"
	case "darwin":
		path, err := launchdPlistPath()
		if err != nil {
			return false
		}
		if _, err := os.Stat(path); err != nil {
			return false
		}
		// `launchctl list <label>` exits 0 when the agent is loaded.
		return exec.Command("launchctl", "list", daemonServiceName).Run() == nil
	default:
		return false
	}
}

// defaultServiceStop stops the daemon via its supervisor so it stays down — the
// unit remains installed/enabled and comes back at next login or an explicit
// `daemon start`.
func defaultServiceStop(w io.Writer) error {
	switch runtime.GOOS {
	case "linux":
		if err := runCmd(w, "systemctl", "--user", "stop", daemonServiceName); err != nil {
			return fmt.Errorf("systemctl --user stop: %w", err)
		}
	case "darwin":
		// bootout stops + unloads for this login session; KeepAlive can't
		// resurrect a booted-out agent. RunAtLoad reloads it at next login.
		label := fmt.Sprintf("gui/%d/%s", os.Getuid(), daemonServiceName)
		if err := runCmd(w, "launchctl", "bootout", label); err != nil {
			return fmt.Errorf("launchctl bootout: %w", err)
		}
	default:
		return fmt.Errorf("supervised stop not supported on %s", runtime.GOOS)
	}
	_, _ = fmt.Fprintln(w, "[gortex daemon] stopped via service supervisor")
	return nil
}

// defaultServiceRestart bounces the daemon through its supervisor so the
// supervisor keeps owning the new process.
func defaultServiceRestart(w io.Writer) error {
	switch runtime.GOOS {
	case "linux":
		if err := runCmd(w, "systemctl", "--user", "restart", daemonServiceName); err != nil {
			return fmt.Errorf("systemctl --user restart: %w", err)
		}
	case "darwin":
		label := fmt.Sprintf("gui/%d/%s", os.Getuid(), daemonServiceName)
		if err := runCmd(w, "launchctl", "kickstart", "-k", label); err != nil {
			return fmt.Errorf("launchctl kickstart -k: %w", err)
		}
	default:
		return fmt.Errorf("supervised restart not supported on %s", runtime.GOOS)
	}
	_, _ = fmt.Fprintln(w, "[gortex daemon] restarted via service supervisor")
	return nil
}
