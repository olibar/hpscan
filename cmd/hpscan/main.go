// Command hpscan is a "Scan to Computer" client for HP all-in-one printers.
//
// Usage:
//
//	hpscan run                 run in the foreground (used by the service)
//	hpscan install|uninstall   register/unregister as a startup service
//	hpscan start|stop|restart  control the installed service
//	hpscan status              show service state
//	hpscan config init|show|set <key> <value>|path
//	hpscan printer list|add|remove  manage the printer list interactively
//	hpscan discover            list HP scanners on the network
//	hpscan scan                trigger one scan from the computer
//	hpscan probe               dump the printer's XML for troubleshooting
//	hpscan help                show this list
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/olibar/hpscan/internal/config"
	"github.com/olibar/hpscan/internal/daemon"
	"github.com/olibar/hpscan/internal/discover"
	"github.com/olibar/hpscan/internal/ledm"
	"github.com/olibar/hpscan/internal/pdf"
	"github.com/olibar/hpscan/internal/service"
)

var version = "dev"

func main() {
	fs := flag.NewFlagSet("hpscan", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file path (or $HPSCAN_CONFIG)")
	verbose := fs.Bool("v", false, "debug logging regardless of config")
	runAs := fs.String("user", "", "install only: run the systemd unit as this account")
	fs.Usage = usage(fs)
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		return
	}
	// Accept flags after the command too: `hpscan run --config <path>`.
	_ = fs.Parse(args[1:])
	args = append(args[:1], fs.Args()...)
	if err := dispatch(args, *cfgPath, *verbose, *runAs); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(fs *flag.FlagSet) func() {
	return func() {
		fmt.Fprintf(os.Stderr, `hpscan %s - HP "Scan to Computer" client

Usage: hpscan [flags] <command>

Commands:
  run                      run the client in the foreground
  install [--user <name>]  install as a startup service and start it
                           (--user: run the systemd unit as this account)
  uninstall                stop and remove the startup service
  start | stop | restart   control the installed service
  status                   show whether the service is running
  config init              write a default config file (auto-detects the printer)
  config show              print the current configuration
  config set <key> <val>   change one setting, e.g. config set output_dir ~/Scans
  config path              print the config file location
  printer list             show configured printers and those on the network
  printer add [host]       add a printer (interactive pick when no host given)
  printer remove [host]    remove a printer (interactive pick when no host given)
  discover                 list HP scanners found on the network
  scan [file]              scan one page now from the computer
  probe [host[:port]]      dump printer XML resources for troubleshooting
  help                     show this help

Config keys for "config set": printer port name output_dir format resolution
  color_mode paper filename page_timeout log_level log_file
Files: config %s
       log    %s

Examples:
  hpscan config set output_dir ~/Documents/Scans && hpscan restart
  hpscan -v scan ~/Desktop/test.pdf
  tail -f <log>

Flags:
`, version, config.DefaultPath(), filepath.Join(filepath.Dir(config.DefaultPath()), "hpscan.log"))
		fs.PrintDefaults()
	}
}

func dispatch(args []string, cfgPath string, verbose bool, runAs string) error {
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "config":
		return configCmd(rest, cfgPath, verbose)
	case "printer", "printers":
		setupLogging("info", "", verbose)
		return printerCmd(rest, cfgPath)
	case "discover":
		setupLogging("info", "", verbose)
		return discoverCmd()
	case "install", "uninstall", "start", "stop", "restart", "status":
		return serviceCmd(cmd, runAs, cfgPath, verbose)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		if os.IsNotExist(unwrapAll(err)) {
			return fmt.Errorf("no config at %s: run `hpscan config init` first", cfgPath)
		}
		return err
	}
	if cmd == "run" && cfg.LogFile == "" && runtime.GOOS == "windows" {
		cfg.LogFile = manager(cfgPath).LogPath
	}
	setupLogging(cfg.LogLevel, cfg.LogFile, verbose)
	switch cmd {
	case "run":
		return runCmd(cfg, cfgPath)
	case "scan":
		return scanCmd(firstPrinter(cfg), rest)
	case "probe":
		return probeCmd(firstPrinter(cfg), rest)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// firstPrinter narrows a multi-printer config to its first entry for the
// single-printer commands (scan, probe).
func firstPrinter(cfg config.Config) config.Config {
	if first, _, found := strings.Cut(cfg.Printer, ","); found {
		cfg.Printer = strings.TrimSpace(first)
		slog.Debug("hpscan: several printers configured, using the first", "printer", cfg.Printer)
	}
	return cfg
}

func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok || u.Unwrap() == nil {
			return err
		}
		err = u.Unwrap()
	}
}

func setupLogging(level, file string, verbose bool) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	if verbose {
		lvl = slog.LevelDebug
	}
	var w io.Writer = os.Stderr
	if file != "" {
		f, err := os.OpenFile(config.ExpandHome(file), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning: cannot open log file, logging to stderr:", err)
		} else {
			w = f
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})))
	slog.Debug("hpscan: logging configured", "level", lvl.String(), "file", file, "version", version)
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// ---- run ------------------------------------------------------------------

func runCmd(cfg config.Config, cfgPath string) error {
	mgr := manager(cfgPath)
	if err := mgr.WritePid(); err != nil {
		slog.Warn("hpscan: pidfile not written", "error", err)
	}
	defer mgr.RemovePid()
	slog.Info("hpscan: starting", "version", version, "config", cfgPath, "output_dir", cfg.ExpandedOutputDir())
	err := runPlatform(cfg, func(ctx context.Context) error { return daemon.Run(ctx, cfg) })
	slog.Info("hpscan: stopped")
	return err
}

// ---- service --------------------------------------------------------------

func manager(cfgPath string) service.Manager {
	bin, err := os.Executable()
	if err == nil {
		bin, _ = filepath.EvalSymlinks(bin)
	}
	dir := filepath.Dir(cfgPath)
	return service.Manager{
		Binary:     bin,
		ConfigPath: cfgPath,
		LogPath:    filepath.Join(dir, "hpscan.log"),
		PidFile:    filepath.Join(dir, "hpscan.pid"),
	}
}

func serviceCmd(cmd, runAs, cfgPath string, verbose bool) error {
	setupLogging("info", "", verbose)
	mgr := manager(cfgPath)
	mgr.RunAs = runAs // systemd system unit only
	switch cmd {
	case "install":
		if _, err := config.Load(cfgPath); err != nil {
			return fmt.Errorf("config must be valid before installing: %w", err)
		}
		if err := mgr.Install(); err != nil {
			return err
		}
		fmt.Printf("installed (%s), logs: %s\n", mgr.Kind(), mgr.LogPath)
	case "uninstall":
		if err := mgr.Uninstall(); err != nil {
			return err
		}
		fmt.Println("uninstalled")
	case "start":
		if err := mgr.Start(); err != nil {
			return err
		}
		fmt.Println("started")
	case "stop":
		if err := mgr.Stop(); err != nil {
			return err
		}
		fmt.Println("stopped")
	case "restart":
		_ = mgr.Stop()
		if err := mgr.Start(); err != nil {
			return err
		}
		fmt.Println("restarted")
	case "status":
		text, _ := mgr.Status()
		fmt.Println(text)
	}
	return nil
}

// ---- config ---------------------------------------------------------------

func configCmd(args []string, cfgPath string, verbose bool) error {
	setupLogging("info", "", verbose)
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "init":
		return configInit(cfgPath)
	case "show":
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
		fmt.Printf("# %s\n%s", cfgPath, data)
		return nil
	case "path":
		fmt.Println(cfgPath)
		return nil
	case "set":
		if len(args) != 3 {
			return fmt.Errorf("usage: hpscan config set <key> <value>")
		}
		if err := config.Set(cfgPath, args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("%s = %s\nrestart the service to apply: hpscan restart\n", args[1], args[2])
		return nil
	}
	return fmt.Errorf("unknown config subcommand %q", args[0])
}

func configInit(cfgPath string) error {
	cfg := config.Defaults()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if s, err := discover.FirstHP(ctx); err == nil {
		cfg.Printer = s.Address()
		cfg.Port = s.Port
		fmt.Printf("found printer: %s at %s:%d\n", s.Name, cfg.Printer, cfg.Port)
	} else {
		fmt.Println("no printer found via mDNS; set 'printer' in the config by hand")
	}
	if err := config.WriteSample(cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("wrote %s\nscans go to %s (change with: hpscan config set output_dir <folder>)\n", cfgPath, cfg.OutputDir)
	return nil
}

// ---- discover -------------------------------------------------------------

func discoverCmd() error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	list, err := discover.Browse(ctx)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no scanners found")
		return nil
	}
	for _, s := range list {
		fmt.Printf("%-40s %-20s %-16s port %d  %s\n", s.Name, strings.TrimSuffix(s.Host, "."), s.IP, s.Port, s.Model)
	}
	return nil
}

// ---- scan -----------------------------------------------------------------

func scanCmd(cfg config.Config, args []string) error {
	ctx, cancel := signalContext()
	defer cancel()
	client, err := daemon.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	settings := ledm.ScanSettings{Resolution: cfg.Resolution, Color: cfg.ColorMode == "color", Format: "Jpeg",
		Width: ledm.A4Width, Height: ledm.A4Height}
	if cfg.Paper == "letter" {
		settings.Width, settings.Height = ledm.LetterWidth, ledm.LetterHeight
	}
	if caps, err := client.Caps(ctx); err == nil {
		if caps.Platen.MaxWidth > 0 && settings.Width > caps.Platen.MaxWidth {
			settings.Width = caps.Platen.MaxWidth
		}
		if caps.Platen.MaxHeight > 0 && settings.Height > caps.Platen.MaxHeight {
			settings.Height = caps.Platen.MaxHeight
		}
	}
	img, err := client.ScanPage(ctx, settings)
	if err != nil {
		return err
	}
	out := filepath.Join(cfg.ExpandedOutputDir(), "scan_"+time.Now().Format("2006-01-02_150405")+"."+ext(cfg.Format))
	if len(args) > 0 {
		out = args[0]
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	if strings.HasSuffix(strings.ToLower(out), ".pdf") {
		f, err := os.Create(out)
		if err != nil {
			return fmt.Errorf("create %s: %w", out, err)
		}
		defer f.Close()
		if err := pdf.Write(f, []pdf.Page{{JPEG: img, DPI: settings.Resolution}}); err != nil {
			return err
		}
	} else if err := os.WriteFile(out, img, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Println("saved", out)
	return nil
}

func ext(format string) string {
	if format == "jpeg" {
		return "jpg"
	}
	return "pdf"
}

// ---- probe ----------------------------------------------------------------

// probeCmd dumps the printer's XML. An optional host[:port] argument
// overrides the configured printer, e.g. to inspect a second device.
func probeCmd(cfg config.Config, args []string) error {
	ctx, cancel := signalContext()
	defer cancel()
	if len(args) > 0 {
		host, port, err := net.SplitHostPort(args[0])
		if err != nil {
			host, port = args[0], "8080"
		}
		cfg.Printer = host
		if cfg.Port, err = strconv.Atoi(port); err != nil {
			return fmt.Errorf("bad port in %q: %w", args[0], err)
		}
	}
	client, err := daemon.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	paths := []string{
		"/DevMgmt/DiscoveryTree.xml",
		"/DevMgmt/ProductConfigDyn.xml",
		"/Scan/ScanCaps",
		"/Scan/Status",
		"/WalkupScan/WalkupScanDestinations",
		"/WalkupScanToComp/WalkupScanToCompDestinations",
		"/WalkupScanToComp/WalkupScanToCompEvent",
		"/EventMgmt/EventTable",
		"/eSCL/ScannerCapabilities",
		"/eSCL/ScannerStatus",
	}
	fmt.Printf("printer: %s\n", client.BaseURL)
	for _, p := range paths {
		body, status, err := client.Fetch(ctx, p)
		fmt.Printf("\n===== GET %s\n", p)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Printf("HTTP %d\n%s\n", status, strings.TrimSpace(string(body)))
	}
	return nil
}
