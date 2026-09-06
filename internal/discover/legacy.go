package discover

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

const serviceName = "_scanner._tcp.local."

// mdnsGroup is the IPv4 mDNS multicast address.
var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// browseLegacy discovers _scanner._tcp services with "legacy unicast" mDNS
// queries (RFC 6762 section 6.7): the query is sent from an ephemeral port,
// so responders answer us directly and we never need to own UDP 5353. That
// port is held exclusively by the system resolver on Windows, which makes the
// regular multicast browse come back empty there.
func browseLegacy(ctx context.Context, dst *net.UDPAddr) ([]Scanner, error) {
	slog.Debug("discover: legacy unicast query", "service", serviceName, "dst", dst.String())
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("mdns socket: %w", err)
	}
	defer conn.Close()

	c := newCollector()
	if err := c.query(ctx, conn, dst, []dns.Question{{Name: serviceName, Qtype: dns.TypePTR, Qclass: dns.ClassINET}}); err != nil {
		return nil, err
	}
	// Follow up on instances whose SRV/TXT/A records were not included.
	if missing := c.missing(); len(missing) > 0 {
		slog.Debug("discover: follow-up queries", "count", len(missing))
		if err := c.query(ctx, conn, dst, missing); err != nil {
			return nil, err
		}
	}
	out := c.scanners()
	slog.Debug("discover: legacy unicast finished", "count", len(out))
	return out, nil
}

// collector accumulates DNS records across responses.
type collector struct {
	instances map[string]bool     // PTR targets, e.g. "Photosmart 6510 series [058DA0]._scanner._tcp.local."
	srv       map[string]*dns.SRV // by instance
	txt       map[string][]string // by instance
	addr      map[string]net.IP   // by host name
}

func newCollector() *collector {
	return &collector{instances: map[string]bool{}, srv: map[string]*dns.SRV{}, txt: map[string][]string{}, addr: map[string]net.IP{}}
}

// query sends one message and collects replies for up to 2 seconds.
func (c *collector) query(ctx context.Context, conn *net.UDPConn, dst *net.UDPAddr, qs []dns.Question) error {
	m := new(dns.Msg)
	m.Id = dns.Id()
	m.RecursionDesired = false
	m.Question = qs
	packed, err := m.Pack()
	if err != nil {
		return fmt.Errorf("pack mdns query: %w", err)
	}
	if _, err := conn.WriteToUDP(packed, dst); err != nil {
		return fmt.Errorf("send mdns query: %w", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil
			}
			return fmt.Errorf("read mdns reply: %w", err)
		}
		var resp dns.Msg
		if err := resp.Unpack(buf[:n]); err != nil {
			slog.Debug("discover: ignoring malformed reply", "from", from.String(), "error", err)
			continue
		}
		for _, rr := range append(append(resp.Answer, resp.Extra...), resp.Ns...) {
			c.add(rr)
		}
	}
}

func (c *collector) add(rr dns.RR) {
	switch r := rr.(type) {
	case *dns.PTR:
		if strings.EqualFold(r.Hdr.Name, serviceName) {
			c.instances[r.Ptr] = true
		}
	case *dns.SRV:
		c.srv[r.Hdr.Name] = r
	case *dns.TXT:
		c.txt[r.Hdr.Name] = r.Txt
	case *dns.A:
		c.addr[strings.ToLower(r.Hdr.Name)] = r.A
	}
}

// missing lists the follow-up questions needed to complete every instance.
func (c *collector) missing() []dns.Question {
	var qs []dns.Question
	for inst := range c.instances {
		srv, ok := c.srv[inst]
		if !ok {
			qs = append(qs, dns.Question{Name: inst, Qtype: dns.TypeSRV, Qclass: dns.ClassINET},
				dns.Question{Name: inst, Qtype: dns.TypeTXT, Qclass: dns.ClassINET})
			continue
		}
		if _, ok := c.txt[inst]; !ok {
			qs = append(qs, dns.Question{Name: inst, Qtype: dns.TypeTXT, Qclass: dns.ClassINET})
		}
		if _, ok := c.addr[strings.ToLower(srv.Target)]; !ok {
			qs = append(qs, dns.Question{Name: srv.Target, Qtype: dns.TypeA, Qclass: dns.ClassINET})
		}
	}
	return qs
}

func (c *collector) scanners() []Scanner {
	var out []Scanner
	for inst := range c.instances {
		srv, ok := c.srv[inst]
		if !ok {
			slog.Debug("discover: instance without SRV, skipped", "instance", inst)
			continue
		}
		s := Scanner{
			Name: unescapeInstance(strings.TrimSuffix(inst, "."+serviceName)),
			Host: srv.Target,
			Port: int(srv.Port),
		}
		if ip, ok := c.addr[strings.ToLower(srv.Target)]; ok {
			s.IP = ip.String()
		}
		for _, kv := range c.txt[inst] {
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
		slog.Debug("discover: legacy found", "name", s.Name, "host", s.Host, "ip", s.IP, "port", s.Port, "model", s.Model)
		out = append(out, s)
	}
	return out
}

// unescapeInstance turns the DNS presentation form ("Photosmart\ 6510") back
// into the display name.
func unescapeInstance(s string) string {
	s = strings.ReplaceAll(s, "\\ ", " ")
	s = strings.ReplaceAll(s, "\\.", ".")
	return strings.ReplaceAll(s, "\\\\", "\\")
}
