package discover

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fakeResponder answers PTR queries with the full record set, like an HP
// printer replying to a legacy unicast query.
func fakeResponder(t *testing.T) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var q dns.Msg
			if q.Unpack(buf[:n]) != nil || len(q.Question) == 0 {
				continue
			}
			resp := new(dns.Msg)
			resp.SetReply(&q)
			inst := "Photosmart\\ 6510\\ series\\ [058DA0]._scanner._tcp.local."
			hdr := func(name string, t uint16) dns.RR_Header {
				return dns.RR_Header{Name: name, Rrtype: t, Class: dns.ClassINET, Ttl: 120}
			}
			resp.Answer = []dns.RR{&dns.PTR{Hdr: hdr(serviceName, dns.TypePTR), Ptr: inst}}
			resp.Extra = []dns.RR{
				&dns.SRV{Hdr: hdr(inst, dns.TypeSRV), Target: "HP058DA0.local.", Port: 8080},
				&dns.TXT{Hdr: hdr(inst, dns.TypeTXT), Txt: []string{"txtvers=1", "ty=Photosmart 6510 series", "mfg=HP"}},
				&dns.A{Hdr: hdr("HP058DA0.local.", dns.TypeA), A: net.IPv4(192, 168, 1, 20)},
			}
			out, _ := resp.Pack()
			conn.WriteToUDP(out, from)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr)
}

func TestBrowseLegacy(t *testing.T) {
	dst := fakeResponder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	found, err := browseLegacy(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("expected 1 scanner, got %d", len(found))
	}
	s := found[0]
	if s.Name != "Photosmart 6510 series [058DA0]" || s.Host != "HP058DA0.local." || s.Port != 8080 ||
		s.IP != "192.168.1.20" || s.Mfg != "HP" || s.Model != "Photosmart 6510 series" {
		t.Fatalf("unexpected scanner: %+v", s)
	}
}
