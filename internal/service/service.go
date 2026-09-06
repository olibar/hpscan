// Package service installs hpscan as a login/boot service and controls it.
// macOS uses a launchd LaunchAgent, Linux uses systemd (system unit when
// root, user unit otherwise), Windows uses the Service Control Manager.
// Anywhere else, or when systemd is missing, a pidfile written by
// `hpscan run` lets stop/status still work.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
