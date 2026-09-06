// Package discover finds HP scanners announced over mDNS (Bonjour).
package discover

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

// Scanner is one _scanner._tcp service found on the local network.
type Scanner struct {
	Name  string // mDNS instance name, e.g. "Photosmart 6510 series [xxxxxx]"
	Host  string // hostname, e.g. "HPxxxxxx.local."
	IP    string // first IPv4 address, may be empty
	Port  int
	Model string // "ty" TXT record
	Mfg   string // "mfg" TXT record
}

// Address returns the best host to connect to: IP when known, else hostname.
func (s Scanner) Address() string {
	if s.IP != "" {
		return s.IP
	}
	return strings.TrimSuffix(s.Host, ".")
}

// Browse lists _scanner._tcp services until ctx expires. The multicast browse
// and a legacy unicast query run side by side and their results are merged:
// the browse needs UDP 5353, which the system resolver owns exclusively on
// Windows, while the unicast query works from any port.
func Browse(ctx context.Context) ([]Scanner, error) {
	slog.Debug("discover: browsing _scanner._tcp")
	legacyCh := make(chan []Scanner, 1)
	go func() {
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		found, err := browseLegacy(lctx, mdnsGroup)
		if err != nil {
			slog.Debug("discover: legacy unicast failed", "error", err)
		}
		legacyCh <- found
	}()
	found, err := browseMulticast(ctx)
	if err != nil {
		slog.Debug("discover: multicast browse failed", "error", err)
	}
	found = merge(found, <-legacyCh)
	slog.Debug("discover: browse finished", "count", len(found))
	return found, nil
}

func browseMulticast(ctx context.Context) ([]Scanner, error) {
	// IPv4 only: the IPv6 multicast path is unreliable on Windows and HP printers
	// announce themselves over IPv4 anyway.
	resolver, err := zeroconf.NewResolver(zeroconf.SelectIPTraffic(zeroconf.IPv4))
	if err != nil {
		return nil, fmt.Errorf("mdns resolver: %w", err)
	}
	entries := make(chan *zeroconf.ServiceEntry)
	if err := resolver.Browse(ctx, "_scanner._tcp", "local.", entries); err != nil {
		return nil, fmt.Errorf("mdns browse: %w", err)
	}
	var found []Scanner
	for e := range entries {
		s := fromEntry(e)
		slog.Debug("discover: found", "name", s.Name, "host", s.Host, "ip", s.IP,
			"port", s.Port, "model", s.Model, "mfg", s.Mfg)
		found = append(found, s)
	}
	return found, nil
}

// merge unions two result sets, keyed by hostname.
func merge(a, b []Scanner) []Scanner {
	seen := map[string]bool{}
	var out []Scanner
	for _, s := range append(a, b...) {
		key := strings.ToLower(strings.TrimSuffix(s.Host, "."))
		if key == "" {
			key = s.IP
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

// AllHP returns every HP scanner from the browse results.
func AllHP(ctx context.Context) ([]Scanner, error) {
	all, err := Browse(ctx)
	if err != nil {
		return nil, err
	}
	var hp []Scanner
	for _, s := range all {
		if isHP(s) {
			hp = append(hp, s)
		}
	}
	if len(hp) == 0 {
		return nil, fmt.Errorf("no HP scanner found via mDNS (%d scanners seen)", len(all))
	}
	return hp, nil
}

// FirstHP returns the first HP scanner from the browse results.
func FirstHP(ctx context.Context) (Scanner, error) {
	hp, err := AllHP(ctx)
	if err != nil {
		return Scanner{}, err
	}
	if len(hp) > 1 {
		slog.Warn("discover: several HP scanners found, using the first", "chosen", hp[0].Name, "total", len(hp))
	}
	return hp[0], nil
}

func isHP(s Scanner) bool {
	return strings.EqualFold(s.Mfg, "HP") || strings.Contains(strings.ToLower(s.Name), "hp") ||
		strings.Contains(strings.ToLower(s.Model), "photosmart")
}

func fromEntry(e *zeroconf.ServiceEntry) Scanner {
	s := Scanner{Name: strings.ReplaceAll(e.Instance, "\\ ", " "), Host: e.HostName, Port: e.Port}
	if len(e.AddrIPv4) > 0 {
		s.IP = e.AddrIPv4[0].String()
	}
	for _, kv := range e.Text {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "ty":
			s.Model = v
		case "mfg":
			s.Mfg = v
		}
	}
	return s
}
