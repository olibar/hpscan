// Package service installs hpscan as a login/boot service and controls it.
// macOS uses a launchd LaunchAgent, Linux uses systemd (system unit when
// root, user unit otherwise). Anywhere else, or when systemd is missing, a
// pidfile written by `hpscan run` lets stop/status still work.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const label = "com.hpscan.agent"

// Manager knows how to install and control the service for this platform.
type Manager struct {
	Binary     string // absolute path of the hpscan executable
	ConfigPath string
	LogPath    string
	PidFile    string
	RunAs      string // systemd only: User= for the unit (empty = root)
}

// Kind reports the backend in use: launchd, systemd or pidfile.
func (m Manager) Kind() string {
	switch {
	case runtime.GOOS == "darwin":
		return "launchd"
	case runtime.GOOS == "linux" && hasSystemd():
		return "systemd"
	default:
		return "pidfile"
	}
}

func hasSystemd() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

// Install writes the service definition and starts it.
func (m Manager) Install() error {
	slog.Debug("service: install", "kind", m.Kind(), "binary", m.Binary, "config", m.ConfigPath)
	switch m.Kind() {
	case "launchd":
		return m.installLaunchd()
	case "systemd":
		return m.installSystemd()
	}
	return fmt.Errorf("no service manager available on %s; run `hpscan run` from your own startup mechanism (see README)", runtime.GOOS)
}

// Uninstall stops the service and removes its definition.
func (m Manager) Uninstall() error {
	slog.Debug("service: uninstall", "kind", m.Kind())
	switch m.Kind() {
	case "launchd":
		_ = run("launchctl", "bootout", m.launchdDomain()+"/"+label)
		return removeIfExists(m.plistPath())
	case "systemd":
		_ = run(m.systemctl("stop", "hpscan")...)
		_ = run(m.systemctl("disable", "hpscan")...)
		if err := removeIfExists(m.unitPath()); err != nil {
			return err
		}
		return run(m.systemctl("daemon-reload")...)
	}
	return m.Stop()
}

// Start starts the installed service.
func (m Manager) Start() error {
	slog.Debug("service: start", "kind", m.Kind())
	switch m.Kind() {
	case "launchd":
		if err := run("launchctl", "bootstrap", m.launchdDomain(), m.plistPath()); err != nil {
			// Already loaded: kick it instead.
			return run("launchctl", "kickstart", m.launchdDomain()+"/"+label)
		}
		return nil
	case "systemd":
		return run(m.systemctl("start", "hpscan")...)
	}
	return fmt.Errorf("no service manager: start it with `hpscan run` (add & to background it)")
}

// Stop stops the service without uninstalling it.
func (m Manager) Stop() error {
	slog.Debug("service: stop", "kind", m.Kind())
	switch m.Kind() {
	case "launchd":
		if err := run("launchctl", "bootout", m.launchdDomain()+"/"+label); err != nil {
			return err
		}
		return m.waitLaunchdGone()
	case "systemd":
		return run(m.systemctl("stop", "hpscan")...)
	}
	return m.killPid()
}

// waitLaunchdGone blocks until launchd has finished unloading the agent, so
// a following bootstrap does not race the teardown.
func (m Manager) waitLaunchdGone() error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("launchctl", "print", m.launchdDomain()+"/"+label).Run() != nil {
			slog.Debug("service: launchd agent unloaded")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("launchd agent still loaded after 15s")
}

// Status prints a human readable state and returns whether it is running.
func (m Manager) Status() (string, bool) {
	slog.Debug("service: status", "kind", m.Kind())
	switch m.Kind() {
	case "launchd":
		out, err := exec.Command("launchctl", "print", m.launchdDomain()+"/"+label).CombinedOutput()
		if err != nil {
			return "not loaded (launchd)", false
		}
		running := strings.Contains(string(out), "state = running")
		if running {
			return "running (launchd)", true
		}
		return "loaded but not running (launchd)", false
	case "systemd":
		args := m.systemctl("is-active", "hpscan")
		out, _ := exec.Command(args[0], args[1:]...).Output()
		state := strings.TrimSpace(string(out))
		return state + " (systemd)", state == "active"
	}
	pid, err := m.readPid()
	if err != nil {
		return "not running (no pidfile)", false
	}
	if syscall.Kill(pid, 0) != nil {
		return fmt.Sprintf("not running (stale pidfile, pid %d)", pid), false
	}
	return fmt.Sprintf("running (pid %d)", pid), true
}

// ---- launchd --------------------------------------------------------------

func (m Manager) launchdDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func (m Manager) plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func (m Manager) installLaunchd() error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
    <string>--config</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, label, m.Binary, m.ConfigPath, m.LogPath, m.LogPath)
	if err := os.MkdirAll(filepath.Dir(m.plistPath()), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(m.plistPath(), []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	_ = run("launchctl", "bootout", m.launchdDomain()+"/"+label)
	if err := run("launchctl", "bootstrap", m.launchdDomain(), m.plistPath()); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	slog.Info("service: installed launchd agent", "plist", m.plistPath(), "log", m.LogPath)
	return nil
}

// ---- systemd --------------------------------------------------------------

func (m Manager) systemctl(args ...string) []string {
	if os.Geteuid() == 0 {
		return append([]string{"systemctl"}, args...)
	}
	return append([]string{"systemctl", "--user"}, args...)
}

func (m Manager) unitPath() string {
	if os.Geteuid() == 0 {
		return "/etc/systemd/system/hpscan.service"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", "hpscan.service")
}

func (m Manager) installSystemd() error {
	unit := fmt.Sprintf(`[Unit]
Description=HP scan-to-computer client
After=network-online.target
Wants=network-online.target

[Service]
%sExecStart=%s run --config %s
Restart=always
RestartSec=10

[Install]
WantedBy=%s
`, m.userLine(), m.Binary, m.ConfigPath, m.wantedBy())
	if err := os.MkdirAll(filepath.Dir(m.unitPath()), 0o755); err != nil {
		return fmt.Errorf("create systemd dir: %w", err)
	}
	if err := os.WriteFile(m.unitPath(), []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := run(m.systemctl("daemon-reload")...); err != nil {
		return err
	}
	// Old systemd builds (Synology DSM) do not support "enable --now".
	if err := run(m.systemctl("enable", "hpscan")...); err != nil {
		return err
	}
	if err := run(m.systemctl("restart", "hpscan")...); err != nil {
		return err
	}
	slog.Info("service: installed systemd unit", "unit", m.unitPath())
	return nil
}

func (m Manager) userLine() string {
	if m.RunAs == "" || os.Geteuid() != 0 {
		return ""
	}
	return "User=" + m.RunAs + "\n"
}

func (m Manager) wantedBy() string {
	if os.Geteuid() == 0 {
		return "multi-user.target"
	}
	return "default.target"
}

// ---- pidfile --------------------------------------------------------------

// WritePid records the current process id; called by `hpscan run`.
func (m Manager) WritePid() error {
	if err := os.MkdirAll(filepath.Dir(m.PidFile), 0o755); err != nil {
		return fmt.Errorf("create pid dir: %w", err)
	}
	if err := os.WriteFile(m.PidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("write pidfile: %w", err)
	}
	slog.Debug("service: pidfile written", "path", m.PidFile, "pid", os.Getpid())
	return nil
}

// RemovePid deletes the pidfile on clean shutdown.
func (m Manager) RemovePid() {
	_ = removeIfExists(m.PidFile)
}

func (m Manager) readPid() (int, error) {
	data, err := os.ReadFile(m.PidFile)
	if err != nil {
		return 0, fmt.Errorf("read pidfile: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse pidfile: %w", err)
	}
	return pid, nil
}

func (m Manager) killPid() error {
	pid, err := m.readPid()
	if err != nil {
		return err
	}
	slog.Debug("service: sending SIGTERM", "pid", pid)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			m.RemovePid()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("process %d did not exit within 10s", pid)
}

// ---- helpers --------------------------------------------------------------

func run(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	slog.Debug("service: exec", "cmd", strings.Join(args, " "))
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
