package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/olibar/hpscan/internal/config"
	"github.com/olibar/hpscan/internal/discover"
)

// printerCmd manages the comma-separated `printer` list interactively.
//
//	hpscan printer list
//	hpscan printer add [host]      no host: pick from scanners found on the network
//	hpscan printer remove [host]   no host: pick from the configured list
func printerCmd(args []string, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list", "ls":
		return printerList(cfg)
	case "add":
		return printerAdd(cfg, cfgPath, rest(args))
	case "remove", "rm":
		return printerRemove(cfg, cfgPath, rest(args))
	}
	return fmt.Errorf("usage: hpscan printer list | add [host] | remove [host]")
}

func rest(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return ""
}

// configured returns the printer list from the config, trimmed.
func configured(cfg config.Config) []string {
	var out []string
	for _, p := range strings.Split(cfg.Printer, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printerList(cfg config.Config) error {
	list := configured(cfg)
	if len(list) == 0 {
		fmt.Println("configured: (none) -> every HP scanner found at startup is served")
	} else {
		fmt.Println("configured:")
		for _, p := range list {
			fmt.Println("  " + p)
		}
	}
	found, _ := browse()
	if len(found) > 0 {
		fmt.Println("on the network now:")
		for _, s := range found {
			mark := " "
			if isConfigured(list, s) {
				mark = "*"
			}
			fmt.Printf("  %s %-24s %-16s %s\n", mark, strings.TrimSuffix(s.Host, "."), s.IP, s.Model)
		}
		fmt.Println("  (* = configured)")
	}
	return nil
}

func browse() ([]discover.Scanner, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	found, err := discover.AllHP(ctx)
	slog.Debug("printer: discovery", "found", len(found), "error", err)
	return found, err
}

func isConfigured(list []string, s discover.Scanner) bool {
	host := strings.ToLower(strings.TrimSuffix(s.Host, "."))
	for _, p := range list {
		p = strings.ToLower(strings.TrimSuffix(p, "."))
		if p == host || p == s.IP {
			return true
		}
	}
	return false
}

func printerAdd(cfg config.Config, cfgPath, host string) error {
	list := configured(cfg)
	if host == "" {
		fmt.Println("looking for HP scanners on the network...")
		found, err := browse()
		if err != nil {
			return fmt.Errorf("%w; pass the address explicitly: hpscan printer add <host>", err)
		}
		var candidates []string
		for _, s := range found {
			if !isConfigured(list, s) {
				candidates = append(candidates, strings.TrimSuffix(s.Host, "."))
				fmt.Printf("  %d) %-24s %-16s %s\n", len(candidates), strings.TrimSuffix(s.Host, "."), s.IP, s.Model)
			}
		}
		if len(candidates) == 0 {
			fmt.Println("every scanner found is already configured")
			return nil
		}
		if host, err = pick(os.Stdin, "add which printer? [number, or Enter to cancel] ", candidates); err != nil || host == "" {
			return err
		}
	}
	if len(list) == 0 {
		fmt.Println("note: the list was empty (auto-discover all); from now on only listed printers are served")
	}
	return savePrinters(cfgPath, append(list, host))
}

func printerRemove(cfg config.Config, cfgPath, host string) error {
	list := configured(cfg)
	if len(list) == 0 {
		fmt.Println("no printers configured (auto-discovery mode), nothing to remove")
		return nil
	}
	if host == "" {
		for i, p := range list {
			fmt.Printf("  %d) %s\n", i+1, p)
		}
		var err error
		if host, err = pick(os.Stdin, "remove which printer? [number, or Enter to cancel] ", list); err != nil || host == "" {
			return err
		}
	}
	var kept []string
	for _, p := range list {
		if !strings.EqualFold(strings.TrimSuffix(p, "."), strings.TrimSuffix(host, ".")) {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(list) {
		return fmt.Errorf("%s is not in the configured list", host)
	}
	if len(kept) == 0 {
		fmt.Println("note: the list is now empty; every HP scanner found at startup will be served")
	}
	return savePrinters(cfgPath, kept)
}

// pick reads a 1-based choice from r; empty input cancels and returns "".
func pick(r io.Reader, prompt string, options []string) (string, error) {
	fmt.Print(prompt)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read choice: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		fmt.Println("cancelled")
		return "", nil
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(options) {
		return "", fmt.Errorf("invalid choice %q", line)
	}
	return options[n-1], nil
}

// savePrinters writes the list and restarts the service if it is running.
func savePrinters(cfgPath string, list []string) error {
	value := strings.Join(list, ", ")
	if err := config.Set(cfgPath, "printer", value); err != nil {
		return err
	}
	fmt.Printf("printer = %q\n", value)
	mgr := manager(cfgPath)
	if _, running := mgr.Status(); running {
		fmt.Println("restarting the service to apply...")
		_ = mgr.Stop()
		if err := mgr.Start(); err != nil {
			return fmt.Errorf("restart service: %w", err)
		}
		fmt.Println("restarted")
	} else {
		fmt.Println("service not running; start it with: hpscan start")
	}
	return nil
}
