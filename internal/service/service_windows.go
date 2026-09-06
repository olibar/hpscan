//go:build windows

package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const winName = "hpscan"

// Kind reports the backend in use.
func (m Manager) Kind() string { return "windows-service" }

// Install registers hpscan with the Service Control Manager (automatic start,
// LocalSystem account) and starts it.
func (m Manager) Install() error {
	slog.Debug("service: install", "kind", m.Kind(), "binary", m.Binary, "config", m.ConfigPath)
	sm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer sm.Disconnect()
	if s, err := sm.OpenService(winName); err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists; run `hpscan uninstall` first", winName)
	}
	cfg := mgr.Config{
		DisplayName:  "HP scan-to-computer client (hpscan)",
		Description:  "Registers this PC as a Scan to Computer destination on HP printers.",
		StartType:    mgr.StartAutomatic,
		Dependencies: []string{"Tcpip", "Dnscache"},
	}
	s, err := sm.CreateService(winName, m.Binary, cfg, "run", "--config", m.ConfigPath)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	slog.Info("service: installed windows service", "name", winName, "log", m.LogPath)
	return nil
}

// Uninstall stops and deletes the service.
func (m Manager) Uninstall() error {
	slog.Debug("service: uninstall", "kind", m.Kind())
	_ = m.Stop()
	sm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer sm.Disconnect()
	s, err := sm.OpenService(winName)
	if err != nil {
		return nil // not installed
	}
	defer s.Close()
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	return nil
}

// Start starts the installed service.
func (m Manager) Start() error {
	slog.Debug("service: start", "kind", m.Kind())
	return m.withService(func(s *mgr.Service) error {
		if err := s.Start(); err != nil {
			return fmt.Errorf("start service: %w", err)
		}
		return nil
	})
}

// Stop stops the service and waits for it to exit.
func (m Manager) Stop() error {
	slog.Debug("service: stop", "kind", m.Kind())
	return m.withService(func(s *mgr.Service) error {
		st, err := s.Control(svc.Stop)
		if err != nil {
			return fmt.Errorf("stop service: %w", err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for st.State != svc.Stopped && time.Now().Before(deadline) {
			time.Sleep(300 * time.Millisecond)
			if st, err = s.Query(); err != nil {
				return fmt.Errorf("query service: %w", err)
			}
		}
		if st.State != svc.Stopped {
			return fmt.Errorf("service did not stop within 15s")
		}
		return nil
	})
}

// Status reports the service state.
func (m Manager) Status() (string, bool) {
	slog.Debug("service: status", "kind", m.Kind())
	var text string
	var running bool
	err := m.withService(func(s *mgr.Service) error {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("query service: %w", err)
		}
		running = st.State == svc.Running
		text = fmt.Sprintf("%s (windows service)", stateName(st.State))
		return nil
	})
	if err != nil {
		return "not installed (windows service)", false
	}
	return text, running
}

func (m Manager) withService(fn func(*mgr.Service) error) error {
	sm, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer sm.Disconnect()
	s, err := sm.OpenService(winName)
	if err != nil {
		return fmt.Errorf("service %s is not installed: %w", winName, err)
	}
	defer s.Close()
	return fn(s)
}

func stateName(s svc.State) string {
	switch s {
	case svc.Running:
		return "running"
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	}
	return fmt.Sprintf("state %d", s)
}

// RunAsWindowsService runs body under the Service Control Manager when the
// process was started by it. It returns handled=false when running from a
// normal console, in which case the caller runs body directly.
func RunAsWindowsService(body func(ctx context.Context) error) (handled bool, err error) {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return false, fmt.Errorf("detect service context: %w", err)
	}
	if !isSvc {
		return false, nil
	}
	slog.Debug("service: running under the service control manager")
	h := &handler{body: body}
	if err := svc.Run(winName, h); err != nil {
		return true, fmt.Errorf("service run: %w", err)
	}
	return true, h.err
}

type handler struct {
	body func(ctx context.Context) error
	err  error
}

// Execute implements svc.Handler: it runs the daemon and stops it on the
// Stop or Shutdown control request.
func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.body(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			cancel()
			h.err = err
			status <- svc.Status{State: svc.Stopped}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				slog.Info("service: stop requested")
				status <- svc.Status{State: svc.StopPending}
				cancel()
				h.err = <-done
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		}
	}
}
