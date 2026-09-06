package ledm

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Flavor distinguishes the two generations of HP scan-to-computer.
type Flavor int

const (
	// FlavorWalkupScan is the 2009 REST flavour (/WalkupScan/...).
	FlavorWalkupScan Flavor = iota
	// FlavorWalkupScanToComp is the 2010 LEDM flavour (/WalkupScanToComp/...).
	FlavorWalkupScanToComp
)

func (f Flavor) String() string {
	if f == FlavorWalkupScanToComp {
		return "WalkupScanToComp"
	}
	return "WalkupScan"
}

// destinationsPath returns the collection URI for the flavour.
func (f Flavor) destinationsPath() string {
	if f == FlavorWalkupScanToComp {
		return "/WalkupScanToComp/WalkupScanToCompDestinations"
	}
	return "/WalkupScan/WalkupScanDestinations"
}

// Destination is a registered "computer" as seen on the printer panel.
type Destination struct {
	URI      string
	Name     string
	Hostname string
	Shortcut string // e.g. SavePDF, SaveJPEG, SaveDocument1, SavePhoto1
}

// ---- registration ---------------------------------------------------------

// RegisterDestination adds this computer to the printer's destination list
// and returns its resource URI.
func (c *Client) RegisterDestination(ctx context.Context, f Flavor, name, hostname string) (string, error) {
	slog.Debug("ledm: register destination", "flavor", f.String(), "name", name, "hostname", hostname)
	body := registrationXML(f, name, hostname)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := c.do(ctx, http.MethodPost, f.destinationsPath(), []byte(body), nil)
	if err != nil {
		return "", fmt.Errorf("register destination: %w", err)
	}
	if r.Status != http.StatusCreated && r.Status != http.StatusOK {
		return "", &StatusError{Method: "POST", Path: f.destinationsPath(), Status: r.Status, Body: r.Body}
	}
	uri := r.Location
	if uri == "" {
		return "", fmt.Errorf("register destination: printer returned %d without Location header", r.Status)
	}
	// Some firmwares return an absolute URL; keep only the path.
	if i := strings.Index(uri, "://"); i >= 0 {
		if j := strings.Index(uri[i+3:], "/"); j >= 0 {
			uri = uri[i+3+j:]
		}
	}
	slog.Info("ledm: destination registered", "flavor", f.String(), "name", name, "uri", uri)
	return uri, nil
}

func registrationXML(f Flavor, name, hostname string) string {
	name = xmlEscape(name)
	hostname = xmlEscape(hostname)
	if f == FlavorWalkupScanToComp {
		return `<?xml version="1.0" encoding="UTF-8"?>` +
			`<wus:WalkupScanToCompDestination xmlns:wus="http://www.hp.com/schemas/imaging/con/ledm/walkupscan/2010/09/28"` +
			` xmlns:dd="http://www.hp.com/schemas/imaging/con/dictionaries/1.0/"` +
			` xmlns:dd3="http://www.hp.com/schemas/imaging/con/dictionaries/2009/04/06">` +
			`<dd3:Hostname>` + hostname + `</dd3:Hostname>` +
			`<dd:Name>` + name + `</dd:Name>` +
			`<wus:LinkType>Network</wus:LinkType>` +
			`</wus:WalkupScanToCompDestination>`
	}
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<wus:WalkupScanDestination xmlns:wus="http://www.hp.com/schemas/imaging/con/rest/walkupscan/2009/09/21"` +
		` xmlns:dd="http://www.hp.com/schemas/imaging/con/dictionaries/1.0/">` +
		`<dd:Hostname>` + hostname + `</dd:Hostname>` +
		`<dd:Name>` + name + `</dd:Name>` +
		`<wus:LinkType>Network</wus:LinkType>` +
		`</wus:WalkupScanDestination>`
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// destinationXML matches both flavours; encoding/xml ignores namespaces on
// local names when no namespace is given in the tag.
type destinationXML struct {
	Name         string `xml:"Name"`
	Hostname     string `xml:"Hostname"`
	ResourceURI  string `xml:"ResourceURI"`
	ShortcutComp string `xml:"WalkupScanToCompSettings>Shortcut"`
	ShortcutOld  string `xml:"WalkupScanSettings>Shortcut"`
}

func (d destinationXML) toDestination(uri string) Destination {
	dst := Destination{URI: uri, Name: d.Name, Hostname: d.Hostname, Shortcut: d.ShortcutComp}
	if dst.Shortcut == "" {
		dst.Shortcut = d.ShortcutOld
	}
	if dst.URI == "" {
		dst.URI = d.ResourceURI
	}
	return dst
}

// GetDestination reads one destination, including the shortcut the user
// picked on the panel.
func (c *Client) GetDestination(ctx context.Context, uri string) (Destination, error) {
	r, err := c.get(ctx, uri)
	if err != nil {
		return Destination{}, fmt.Errorf("get destination: %w", err)
	}
	var d destinationXML
	if err := xml.Unmarshal(r.Body, &d); err != nil {
		return Destination{}, fmt.Errorf("parse destination: %w", err)
	}
	dst := d.toDestination(uri)
	slog.Debug("ledm: destination", "uri", uri, "name", dst.Name, "hostname", dst.Hostname, "shortcut", dst.Shortcut)
	return dst, nil
}

type destinationsXML struct {
	Comp []destinationXML `xml:"WalkupScanToCompDestination"`
	Old  []destinationXML `xml:"WalkupScanDestination"`
}

// ListDestinations returns every destination currently registered on the printer.
func (c *Client) ListDestinations(ctx context.Context, f Flavor) ([]Destination, error) {
	r, err := c.get(ctx, f.destinationsPath())
	if err != nil {
		return nil, fmt.Errorf("list destinations: %w", err)
	}
	var list destinationsXML
	if err := xml.Unmarshal(r.Body, &list); err != nil {
		return nil, fmt.Errorf("parse destinations: %w", err)
	}
	var out []Destination
	for _, d := range append(list.Comp, list.Old...) {
		out = append(out, d.toDestination(""))
	}
	slog.Debug("ledm: destinations listed", "flavor", f.String(), "count", len(out))
	return out, nil
}

// DeleteDestination removes a destination; 404 is treated as success.
func (c *Client) DeleteDestination(ctx context.Context, uri string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := c.do(ctx, http.MethodDelete, uri, nil, nil)
	if err != nil {
		return fmt.Errorf("delete destination: %w", err)
	}
	if r.Status != http.StatusOK && r.Status != http.StatusNoContent && r.Status != http.StatusNotFound {
		return &StatusError{Method: "DELETE", Path: uri, Status: r.Status, Body: r.Body}
	}
	slog.Debug("ledm: destination deleted", "uri", uri, "status", r.Status)
	return nil
}

// ---- events ---------------------------------------------------------------

// Event is one row of the printer's event table.
type Event struct {
	Category   string
	AgingStamp string
	Payloads   []Payload
}

// Payload is a resource reference attached to an event.
type Payload struct {
	ResourceURI  string
	ResourceType string
}

type eventTableXML struct {
	Events []struct {
		Category   string `xml:"UnqualifiedEventCategory"`
		AgingStamp string `xml:"AgingStamp"`
		Payloads   []struct {
			ResourceURI  string `xml:"ResourceURI"`
			ResourceType string `xml:"ResourceType"`
		} `xml:"Payload"`
	} `xml:"Event"`
}

// EventTable holds the last poll result so the next call can long-poll with
// If-None-Match.
type EventTable struct {
	ETag   string
	Events []Event
}

// PollEvents fetches /EventMgmt/EventTable. When prev has an ETag the request
// long-polls up to waitSec seconds and returns changed=false on 304.
func (c *Client) PollEvents(ctx context.Context, prev EventTable, waitSec int) (EventTable, bool, error) {
	path := "/EventMgmt/EventTable"
	hdr := map[string]string{}
	if prev.ETag != "" {
		path = fmt.Sprintf("/EventMgmt/EventTable?timeout=%d", waitSec)
		hdr["If-None-Match"] = prev.ETag
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(waitSec+30)*time.Second)
	defer cancel()
	r, err := c.do(ctx, http.MethodGet, path, nil, hdr)
	if err != nil {
		return prev, false, fmt.Errorf("poll events: %w", err)
	}
	if r.Status == http.StatusNotModified {
		slog.Debug("ledm: event table unchanged")
		return prev, false, nil
	}
	if r.Status < 200 || r.Status > 299 {
		return prev, false, &StatusError{Method: "GET", Path: path, Status: r.Status, Body: r.Body}
	}
	var t eventTableXML
	if err := xml.Unmarshal(r.Body, &t); err != nil {
		return prev, false, fmt.Errorf("parse event table: %w", err)
	}
	out := EventTable{ETag: r.ETag}
	for _, e := range t.Events {
		ev := Event{Category: e.Category, AgingStamp: e.AgingStamp}
		for _, p := range e.Payloads {
			ev.Payloads = append(ev.Payloads, Payload{ResourceURI: p.ResourceURI, ResourceType: p.ResourceType})
		}
		out.Events = append(out.Events, ev)
	}
	slog.Debug("ledm: event table", "events", len(out.Events), "etag", out.ETag)
	return out, true, nil
}

// CompEventType is the sub-event of a WalkupScanToComp ScanEvent.
type CompEventType string

const (
	CompHostSelected         CompEventType = "HostSelected"
	CompScanRequested        CompEventType = "ScanRequested"
	CompScanNewPageRequested CompEventType = "ScanNewPageRequested"
	CompScanPagesComplete    CompEventType = "ScanPagesComplete"
)

type compEventXML struct {
	Type string `xml:"WalkupScanToCompEventType"`
}

// GetCompEvent reads /WalkupScanToComp/WalkupScanToCompEvent to learn why a
// ScanEvent fired (host selected, scan requested, new page, complete).
func (c *Client) GetCompEvent(ctx context.Context) (CompEventType, error) {
	r, err := c.get(ctx, "/WalkupScanToComp/WalkupScanToCompEvent")
	if err != nil {
		return "", fmt.Errorf("get walkup event: %w", err)
	}
	var e compEventXML
	if err := xml.Unmarshal(r.Body, &e); err != nil {
		return "", fmt.Errorf("parse walkup event: %w", err)
	}
	slog.Debug("ledm: walkup comp event", "type", e.Type)
	return CompEventType(strings.TrimSpace(e.Type)), nil
}
