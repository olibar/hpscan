package escl

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Walkup ("Scan to Computer") on eSCL-generation HP printers.
//
// The flow mirrors the LEDM WalkupScanToComp flavour the ledm package
// implements, at different URLs and with a JSON-free XML vocabulary of its
// own. Everything is plain HTTP on the default port with no authentication.
//
//	POST   /eSCL/WalkupSubscriptions        register; 201 + Location
//	GET    <sub>/Event                      204 when idle, 200 with an event
//	POST   /eSCL/ScanJobs                   on ScanRequested, start the scan
//	GET    <job>/NextDocument               collect pages until 404
//	DELETE <sub>                            unregister
//
// The subscription is what puts this computer's name on the printer's panel.
// It is server-side state that outlives the process that created it, but the
// printer treats a host as reachable only while something is polling its Event
// resource: select a host that is not polling and the panel spends ten seconds
// on "Accessing..." before reporting the computer is not available. Polling is
// the liveness proof; there is no callback and nothing to listen on, so no
// inbound firewall rule is ever needed.
//
// Measured against an OfficeJet Pro 9120e in September 2026.

// Limits the scanner advertises in WalkupScanSupport. Exceeding them is
// rejected, in the case of the name length silently.
const (
	// MaxShortcutNameLen is the longest shortcut name the panel accepts.
	MaxShortcutNameLen = 32
	// MaxShortcuts is the most shortcuts one subscription may carry.
	MaxShortcuts = 5
)

// EventType is what the printer is asking the registered host to do.
type EventType string

// Walkup event types, spelled as the printer spells them. They are the same
// four the LEDM flavour uses.
const (
	// EventHostSelected means the user picked this computer on the panel and
	// the shortcut list is now on screen.
	EventHostSelected EventType = "HostSelected"
	// EventScanRequested means the user picked a shortcut and pressed Start.
	EventScanRequested EventType = "ScanRequested"
	// EventScanNewPageRequested means another flatbed page was requested for
	// the document already in progress.
	EventScanNewPageRequested EventType = "ScanNewPageRequested"
	// EventScanPagesComplete means the user chose Done.
	EventScanPagesComplete EventType = "ScanPagesComplete"
)

// Subscription is this computer's registration on one printer.
type Subscription struct {
	URI        string // absolute; empty until Subscribe returns
	Host       string
	Connection string
	Shortcuts  []string
}

type subscriptionXML struct {
	XMLName    xml.Name `xml:"WalkupScanSubscription"`
	Host       string   `xml:"HostIdentifier"`
	Connection string   `xml:"Connection"`
	Shortcuts  []string `xml:"Shortcuts>Shortcut"`
}

type subscriptionsXML struct {
	Subs []subscriptionXML `xml:"WalkupScanSubscription"`
}

// WalkupSettings are the scan settings the printer derived from the shortcut
// the user chose. Honouring them is what makes the panel's choice mean
// something; zero fields were not sent.
type WalkupSettings struct {
	XResolution        int    `xml:"XResolution"`
	YResolution        int    `xml:"YResolution"`
	Duplex             bool   `xml:"Duplex"`
	Brightness         int    `xml:"Brightness"`
	Contrast           int    `xml:"Contrast"`
	ContentOrientation string `xml:"ContentOrientation"`
}

// WalkupEvent is one instruction from the printer.
type WalkupEvent struct {
	Type        EventType
	Host        string
	Shortcut    string
	ResourceURI string
	Settings    WalkupSettings
}

type walkupEventXML struct {
	ResourceURI string         `xml:"ResourceURI"`
	Host        string         `xml:"HostIdentifier"`
	Connection  string         `xml:"Connection"`
	Event       string         `xml:"Event"`
	Shortcut    string         `xml:"Shortcut"`
	Settings    WalkupSettings `xml:"ScanSettings"`
}

// abs turns a Location header into an absolute URL. The printer answers with a
// server-absolute path ("/eSCL/WalkupSubscriptions/<uuid>") for subscriptions
// and a full URL for scan jobs, and BaseURL already ends in /eSCL, so joining
// naively yields /eSCL/eSCL/...
func (c *Client) abs(loc string) string {
	if loc == "" {
		return ""
	}
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return loc
	}
	if i := strings.Index(c.BaseURL, Root); i > 0 {
		return c.BaseURL[:i] + loc
	}
	return c.BaseURL + loc
}

// Subscribe registers this computer on the printer's panel under host, showing
// the given shortcut names. The returned Subscription's URI addresses it.
//
// Registering does not by itself make the computer usable: NextEvent must be
// polled for the printer to consider it reachable.
func (c *Client) Subscribe(ctx context.Context, host string, shortcuts []string) (*Subscription, error) {
	if host == "" {
		return nil, fmt.Errorf("subscribe: host identifier must not be empty")
	}
	if len(shortcuts) == 0 {
		return nil, fmt.Errorf("subscribe: at least one shortcut is required")
	}
	if len(shortcuts) > MaxShortcuts {
		return nil, fmt.Errorf("subscribe: %d shortcuts, the panel accepts at most %d", len(shortcuts), MaxShortcuts)
	}
	for _, s := range shortcuts {
		if len(s) > MaxShortcutNameLen {
			return nil, fmt.Errorf("subscribe: shortcut %q is %d characters, the panel accepts at most %d",
				s, len(s), MaxShortcutNameLen)
		}
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	fmt.Fprintf(&b, `<scan:WalkupScanSubscription xmlns:pwg=%q xmlns:scan=%q>`, nsPWG, nsScan)
	fmt.Fprintf(&b, `<scan:HostIdentifier>%s</scan:HostIdentifier>`, xmlEscape(host))
	b.WriteString(`<scan:Connection>Network</scan:Connection>`)
	b.WriteString(`<scan:Shortcuts>`)
	for _, s := range shortcuts {
		fmt.Fprintf(&b, `<scan:Shortcut>%s</scan:Shortcut>`, xmlEscape(s))
	}
	b.WriteString(`</scan:Shortcuts></scan:WalkupScanSubscription>`)

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	status, hdr, body, err := c.do(ctx, http.MethodPost, "/WalkupSubscriptions", []byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, &StatusError{Method: "POST", Path: "/WalkupSubscriptions", Status: status, Body: body}
	}
	loc := hdr.Get("Location")
	if loc == "" {
		return nil, fmt.Errorf("subscribe: no Location header in %d reply", status)
	}
	sub := &Subscription{URI: c.abs(loc), Host: host, Connection: "Network", Shortcuts: shortcuts}
	slog.Info("escl: registered as a scan destination", "host", host, "uri", sub.URI,
		"shortcuts", strings.Join(shortcuts, ", "))
	return sub, nil
}

// Subscriptions lists the destinations currently on the printer's panel,
// including ones other computers registered.
func (c *Client) Subscriptions(ctx context.Context) ([]Subscription, error) {
	body, err := c.get(ctx, "/WalkupSubscriptions")
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil // no destinations registered: the printer returns an empty body
	}
	var doc subscriptionsXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse subscriptions: %w", err)
	}
	out := make([]Subscription, 0, len(doc.Subs))
	for _, s := range doc.Subs {
		out = append(out, Subscription{Host: s.Host, Connection: s.Connection, Shortcuts: s.Shortcuts})
	}
	return out, nil
}

// Unsubscribe removes the destination from the panel. A 404 is success: it is
// already gone.
func (c *Client) Unsubscribe(ctx context.Context, sub *Subscription) error {
	if sub == nil || sub.URI == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	status, _, body, err := c.do(ctx, http.MethodDelete, sub.URI, nil)
	if err != nil {
		return fmt.Errorf("unsubscribe: %w", err)
	}
	if status != http.StatusOK && status != http.StatusNoContent && status != http.StatusNotFound {
		return &StatusError{Method: "DELETE", Path: sub.URI, Status: status, Body: body}
	}
	slog.Info("escl: destination removed from the panel", "host", sub.Host, "uri", sub.URI)
	return nil
}

// NextEvent polls the subscription for the next instruction. It returns
// (nil, nil) when the printer has nothing to say, which is the common case:
// the poll is also what tells the printer this computer is still reachable, so
// it must keep being called even while idle.
func (c *Client) NextEvent(ctx context.Context, sub *Subscription) (*WalkupEvent, error) {
	if sub == nil || sub.URI == "" {
		return nil, fmt.Errorf("next event: no subscription")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, _, body, err := c.do(ctx, http.MethodGet, sub.URI+"/Event", nil)
	if err != nil {
		return nil, fmt.Errorf("next event: %w", err)
	}
	switch status {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusNotFound:
		// The subscription is gone; the caller should register again.
		return nil, &StatusError{Method: "GET", Path: sub.URI + "/Event", Status: status, Body: body}
	}
	if status < 200 || status > 299 {
		return nil, &StatusError{Method: "GET", Path: sub.URI + "/Event", Status: status, Body: body}
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil
	}
	var e walkupEventXML
	if err := xml.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("parse walkup event: %w", err)
	}
	ev := &WalkupEvent{
		Type:        EventType(strings.TrimSpace(e.Event)),
		Host:        e.Host,
		Shortcut:    e.Shortcut,
		ResourceURI: e.ResourceURI,
		Settings:    e.Settings,
	}
	slog.Debug("escl: walkup event", "type", ev.Type, "shortcut", ev.Shortcut,
		"resolution", ev.Settings.XResolution, "duplex", ev.Settings.Duplex)
	return ev, nil
}

// Apply returns scan settings for the event, starting from base and overlaying
// whatever the printer sent. The panel's choice wins over local configuration:
// that is the whole point of choosing a shortcut at the machine.
func (e *WalkupEvent) Apply(base ScanSettings) ScanSettings {
	s := base
	if e.Settings.XResolution > 0 {
		s.Resolution = e.Settings.XResolution
	}
	if e.Settings.Duplex {
		s.Duplex = true
	}
	// The shortcut name is the only signal for the output format; the printer
	// sends no media type. Matching on the name is how HP's own client does it,
	// and users may rename shortcuts, so an unrecognised name keeps the
	// configured default rather than guessing.
	switch {
	case containsFold(e.Shortcut, "pdf"):
		s.Format = FormatPDF
	case containsFold(e.Shortcut, "jpeg"), containsFold(e.Shortcut, "jpg"):
		s.Format = FormatJPEG
	}
	return s
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), needle)
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
