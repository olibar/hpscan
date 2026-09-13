package escl

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// eventFixture is a ScanRequested captured from an OfficeJet Pro 9120e after
// choosing a shortcut named "hpscan JPEG" on the panel.
const eventFixture = `<?xml version="1.0"?>
<scan:WalkupScanEvent xmlns:dest="http://schemas.hp.com/imaging/httpdestination/2011/10/13" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm" xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03">
	<scan:ResourceURI>/eSCL/WalkupSubscriptions/90dab29b-a443-445e-89b5-38b8d2fbfd80</scan:ResourceURI>
	<scan:HostIdentifier>HPSCAN-GO-TEST</scan:HostIdentifier>
	<scan:Connection>Network</scan:Connection>
	<scan:Event>ScanRequested</scan:Event>
	<scan:Shortcut>hpscan JPEG</scan:Shortcut>
	<scan:ScanSettings>
		<pwg:Version>2.4</pwg:Version>
		<scan:XResolution>300</scan:XResolution>
		<scan:YResolution>300</scan:YResolution>
		<scan:Duplex>false</scan:Duplex>
		<scan:Brightness>4</scan:Brightness>
		<scan:ContentOrientation>Portrait</scan:ContentOrientation>
	</scan:ScanSettings>
</scan:WalkupScanEvent>`

func TestParseWalkupEvent(t *testing.T) {
	var e walkupEventXML
	if err := xml.Unmarshal([]byte(eventFixture), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Event != "ScanRequested" {
		t.Errorf("Event = %q", e.Event)
	}
	if e.Shortcut != "hpscan JPEG" {
		t.Errorf("Shortcut = %q", e.Shortcut)
	}
	if e.Host != "HPSCAN-GO-TEST" {
		t.Errorf("HostIdentifier = %q", e.Host)
	}
	if e.Settings.XResolution != 300 || e.Settings.Brightness != 4 {
		t.Errorf("settings = %+v", e.Settings)
	}
	if e.Settings.ContentOrientation != "Portrait" {
		t.Errorf("orientation = %q", e.Settings.ContentOrientation)
	}
}

func TestSubscriptionListParsing(t *testing.T) {
	body := `<?xml version="1.0"?>
<scan:WalkupScanSubscriptions xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03">
  <scan:WalkupScanSubscription>
    <scan:HostIdentifier>CHRIS-X1-CARBON</scan:HostIdentifier>
    <scan:Connection>Network</scan:Connection>
    <scan:Shortcuts>
      <scan:Shortcut>Save as PDF</scan:Shortcut>
      <scan:Shortcut>Save as JPEG</scan:Shortcut>
    </scan:Shortcuts>
  </scan:WalkupScanSubscription>
</scan:WalkupScanSubscriptions>`
	var doc subscriptionsXML
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Subs) != 1 {
		t.Fatalf("got %d subscriptions", len(doc.Subs))
	}
	if doc.Subs[0].Host != "CHRIS-X1-CARBON" {
		t.Errorf("host = %q", doc.Subs[0].Host)
	}
	if len(doc.Subs[0].Shortcuts) != 2 {
		t.Errorf("shortcuts = %v", doc.Subs[0].Shortcuts)
	}
}

func TestEventApplyHonoursThePanel(t *testing.T) {
	var e walkupEventXML
	if err := xml.Unmarshal([]byte(eventFixture), &e); err != nil {
		t.Fatal(err)
	}
	ev := &WalkupEvent{Type: EventType(e.Event), Shortcut: e.Shortcut, Settings: e.Settings}

	base := ScanSettings{Resolution: 75, Format: FormatPDF, Width: A4Width, Height: A4Height}
	got := ev.Apply(base)

	if got.Resolution != 300 {
		t.Errorf("Resolution = %d, want the printer's 300 not the local 75", got.Resolution)
	}
	if got.Format != FormatJPEG {
		t.Errorf("Format = %q, want JPEG: the shortcut says JPEG and the panel wins", got.Format)
	}
	if got.Width != A4Width || got.Height != A4Height {
		t.Errorf("the scan area should be left alone: %dx%d", got.Width, got.Height)
	}
}

func TestEventApplyKeepsDefaultForUnknownShortcut(t *testing.T) {
	ev := &WalkupEvent{Type: EventScanRequested, Shortcut: "Everyday Scan"}
	got := ev.Apply(ScanSettings{Resolution: 200, Format: FormatPDF})
	if got.Format != FormatPDF {
		t.Errorf("Format = %q, want the configured default: a renamed shortcut must not be guessed at", got.Format)
	}
	if got.Resolution != 200 {
		t.Errorf("Resolution = %d, want the configured 200 when the event carries none", got.Resolution)
	}
}

func TestEventApplyDuplex(t *testing.T) {
	ev := &WalkupEvent{Type: EventScanRequested, Shortcut: "x", Settings: WalkupSettings{Duplex: true}}
	if !ev.Apply(ScanSettings{}).Duplex {
		t.Error("a duplex shortcut must set Duplex")
	}
}

func TestSubscribeValidation(t *testing.T) {
	c := New("127.0.0.1", 0)
	ctx := context.Background()
	if _, err := c.Subscribe(ctx, "", []string{"a"}); err == nil {
		t.Error("an empty host identifier must be rejected")
	}
	if _, err := c.Subscribe(ctx, "host", nil); err == nil {
		t.Error("no shortcuts must be rejected")
	}
	if _, err := c.Subscribe(ctx, "host", []string{"a", "b", "c", "d", "e", "f"}); err == nil {
		t.Errorf("more than %d shortcuts must be rejected", MaxShortcuts)
	}
	long := strings.Repeat("x", MaxShortcutNameLen+1)
	if _, err := c.Subscribe(ctx, "host", []string{long}); err == nil {
		t.Errorf("a shortcut name longer than %d must be rejected", MaxShortcutNameLen)
	}
}

func TestSubscribeAndEventAgainstStub(t *testing.T) {
	var gotBody string
	events := []string{
		"", // 204
		eventFixture,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/eSCL/WalkupSubscriptions":
			b := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(b)
			gotBody = string(b)
			// The real printer answers with a server-absolute path, not a URL.
			w.Header().Set("Location", "/eSCL/WalkupSubscriptions/abc-123")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/eSCL/WalkupSubscriptions/abc-123/Event":
			next := events[0]
			if len(events) > 1 {
				events = events[1:]
			}
			if next == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = w.Write([]byte(next))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + Root, http: srv.Client()}
	ctx := context.Background()

	sub, err := c.Subscribe(ctx, "MY-PC", []string{"hpscan PDF", "hpscan JPEG"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	for _, want := range []string{
		"<scan:HostIdentifier>MY-PC</scan:HostIdentifier>",
		"<scan:Connection>Network</scan:Connection>",
		"<scan:Shortcut>hpscan PDF</scan:Shortcut>",
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %s\ngot: %s", want, gotBody)
		}
	}
	// The Location is server-absolute and BaseURL already ends in /eSCL, so a
	// naive join would produce /eSCL/eSCL/... and every later call would 404.
	if strings.Contains(sub.URI, "/eSCL/eSCL/") {
		t.Fatalf("subscription URI double-prefixed: %s", sub.URI)
	}
	if sub.URI != srv.URL+"/eSCL/WalkupSubscriptions/abc-123" {
		t.Fatalf("subscription URI = %s", sub.URI)
	}

	// First poll: nothing pending.
	ev, err := c.NextEvent(ctx, sub)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if ev != nil {
		t.Fatalf("expected no event, got %+v", ev)
	}
	// Second poll: the scan request.
	ev, err = c.NextEvent(ctx, sub)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if ev == nil || ev.Type != EventScanRequested {
		t.Fatalf("expected ScanRequested, got %+v", ev)
	}
	if ev.Shortcut != "hpscan JPEG" {
		t.Errorf("shortcut = %q", ev.Shortcut)
	}
	if err := c.Unsubscribe(ctx, sub); err != nil {
		t.Errorf("Unsubscribe: %v", err)
	}
}

func TestSubscriptionsEmptyBody(t *testing.T) {
	// With no destinations registered the printer returns 200 and an empty
	// body rather than an empty document.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + Root, http: srv.Client()}
	subs, err := c.Subscriptions(context.Background())
	if err != nil {
		t.Fatalf("Subscriptions: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("got %d subscriptions", len(subs))
	}
}

// TestWalkupAgainstRealPrinter registers a destination, waits for someone to
// pick it on the panel and collects the scan. It needs a person at the
// printer, so it runs only when both variables are set:
//
//	HPSCAN_TEST_PRINTER=192.168.1.10 HPSCAN_TEST_WALKUP=1 \
//	  go test ./internal/escl -run RealWalkup -v -timeout 5m
func TestWalkupAgainstRealPrinter(t *testing.T) {
	host := os.Getenv("HPSCAN_TEST_PRINTER")
	if host == "" || os.Getenv("HPSCAN_TEST_WALKUP") == "" {
		t.Skip("set HPSCAN_TEST_PRINTER and HPSCAN_TEST_WALKUP, and be ready at the printer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	c := New(host, 0)
	sub, err := c.Subscribe(ctx, "HPSCAN-GO-IT", []string{"go PDF", "go JPEG"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() {
		if err := c.Unsubscribe(context.Background(), sub); err != nil {
			t.Errorf("Unsubscribe: %v", err)
		}
	}()
	t.Logf("registered as HPSCAN-GO-IT; pick it on the panel and start a scan")

	for {
		ev, err := c.NextEvent(ctx, sub)
		if err != nil {
			t.Fatalf("NextEvent: %v", err)
		}
		if ev == nil {
			select {
			case <-ctx.Done():
				t.Fatal("timed out waiting for someone at the printer")
			case <-time.After(700 * time.Millisecond):
			}
			continue
		}
		t.Logf("event %s shortcut=%q settings=%+v", ev.Type, ev.Shortcut, ev.Settings)
		if ev.Type != EventScanRequested {
			continue
		}
		s := ev.Apply(ScanSettings{Resolution: 300, Width: A4Width, Height: A4Height, Color: true})
		pages, err := c.ScanPages(ctx, s)
		if err != nil {
			t.Fatalf("ScanPages: %v", err)
		}
		t.Logf("collected %d page(s), first is %d bytes", len(pages), len(pages[0]))
		return
	}
}

// TestSubscribeRoundTripAgainstRealPrinter exercises register, list and
// unregister against a real device. It needs nobody at the printer, so it runs
// whenever HPSCAN_TEST_PRINTER is set.
//
// It is the test that catches the Location-handling bug: the printer answers
// with a server-absolute path while BaseURL already ends in /eSCL.
func TestSubscribeRoundTripAgainstRealPrinter(t *testing.T) {
	host := os.Getenv("HPSCAN_TEST_PRINTER")
	if host == "" {
		t.Skip("set HPSCAN_TEST_PRINTER to run against a real printer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := New(host, 0)

	const name = "HPSCAN-GO-RT"
	sub, err := c.Subscribe(ctx, name, []string{"go PDF", "go JPEG"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Logf("subscription URI: %s", sub.URI)
	if strings.Contains(sub.URI, "/eSCL/eSCL/") {
		t.Fatalf("URI is double-prefixed: %s", sub.URI)
	}

	subs, err := c.Subscriptions(ctx)
	if err != nil {
		t.Fatalf("Subscriptions: %v", err)
	}
	var found bool
	for _, s := range subs {
		t.Logf("panel shows %q %v", s.Host, s.Shortcuts)
		if s.Host == name {
			found = true
			if len(s.Shortcuts) != 2 {
				t.Errorf("shortcuts = %v", s.Shortcuts)
			}
		}
	}
	if !found {
		t.Fatalf("%s did not appear in the destination list", name)
	}

	// Polling must work and report nothing pending while no one is at the panel.
	ev, err := c.NextEvent(ctx, sub)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if ev != nil {
		t.Logf("unexpected pending event (harmless): %+v", ev)
	}

	if err := c.Unsubscribe(ctx, sub); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	subs, err = c.Subscriptions(ctx)
	if err != nil {
		t.Fatalf("Subscriptions after unsubscribe: %v", err)
	}
	for _, s := range subs {
		if s.Host == name {
			t.Fatalf("%s is still registered after Unsubscribe", name)
		}
	}
	t.Logf("registered, listed and removed cleanly")
}
