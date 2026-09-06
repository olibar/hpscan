// Package ledm talks to the HP "Low End Data Model" REST interface exposed by
// HP inkjet all-in-ones (Photosmart, OfficeJet, ENVY, ...) on port 8080.
// It covers the parts needed for "Scan to Computer": destination
// registration, event polling and scan jobs.
package ledm

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client is a thin HTTP client bound to one printer.
type Client struct {
	BaseURL string // e.g. http://192.168.1.10:8080
	http    *http.Client
}

// New creates a client for host:port.
func New(host string, port int) *Client {
	base := "http://" + net.JoinHostPort(host, fmt.Sprint(port))
	slog.Debug("ledm: new client", "base_url", base)
	return &Client{
		BaseURL: base,
		http: &http.Client{
			// Per-request deadlines come from the context; this is a safety net
			// for the long-poll on the event table.
			Timeout: 5 * time.Minute,
		},
	}
}

// Response is a decoded HTTP reply.
type Response struct {
	Status   int
	Header   http.Header
	Body     []byte
	Location string
	ETag     string
}

// do performs one request. Body may be nil. Extra headers are optional.
func (c *Client) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*Response, error) {
	url := c.BaseURL + path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		url = path
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, fmt.Errorf("build request %s %s: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "text/xml")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	slog.Debug("ledm: request", "method", method, "url", url, "body_bytes", len(body))
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", method, url, err)
	}
	r := &Response{
		Status:   resp.StatusCode,
		Header:   resp.Header,
		Body:     data,
		Location: resp.Header.Get("Location"),
		ETag:     resp.Header.Get("ETag"),
	}
	slog.Debug("ledm: response", "method", method, "url", url, "status", r.Status,
		"body_bytes", len(data), "location", r.Location, "etag", r.ETag, "elapsed", time.Since(start).Round(time.Millisecond))
	return r, nil
}

// get is a GET with a default deadline that fails on non-2xx status.
func (c *Client) get(ctx context.Context, path string) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if r.Status < 200 || r.Status > 299 {
		return r, &StatusError{Method: "GET", Path: path, Status: r.Status, Body: r.Body}
	}
	return r, nil
}

// StatusError reports an unexpected HTTP status from the printer.
type StatusError struct {
	Method string
	Path   string
	Status int
	Body   []byte
}

func (e *StatusError) Error() string {
	b := string(e.Body)
	if len(b) > 300 {
		b = b[:300] + "..."
	}
	return fmt.Sprintf("%s %s: http %d: %s", e.Method, e.Path, e.Status, strings.TrimSpace(b))
}

// IsNotFound reports whether err is a 404 from the printer.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// Fetch returns the raw body of a GET; used by the probe command.
func (c *Client) Fetch(ctx context.Context, path string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	return r.Body, r.Status, nil
}

// ---- Discovery tree -------------------------------------------------------

// Capabilities summarises which scan-to-computer flavour the printer offers.
type Capabilities struct {
	WalkupScanToComp bool // newer flavour (/WalkupScanToComp/...)
	WalkupScan       bool // older flavour (/WalkupScan/...)
	EventTable       bool
	Scan             bool
	Resources        []string
}

type discoveryTree struct {
	Ifcs []struct {
		ManifestURI  string `xml:"ManifestURI"`
		ResourceType string `xml:"ResourceType"`
	} `xml:"SupportedIfc"`
}

// Discover reads /DevMgmt/DiscoveryTree.xml and reports the supported
// interfaces.
func (c *Client) Discover(ctx context.Context) (Capabilities, error) {
	var caps Capabilities
	r, err := c.get(ctx, "/DevMgmt/DiscoveryTree.xml")
	if err != nil {
		return caps, fmt.Errorf("discovery tree: %w", err)
	}
	var tree discoveryTree
	if err := xml.Unmarshal(r.Body, &tree); err != nil {
		return caps, fmt.Errorf("parse discovery tree: %w", err)
	}
	for _, ifc := range tree.Ifcs {
		uri := ifc.ManifestURI
		caps.Resources = append(caps.Resources, uri)
		switch {
		case strings.HasPrefix(uri, "/WalkupScanToComp/"):
			caps.WalkupScanToComp = true
		case strings.HasPrefix(uri, "/WalkupScan/"):
			caps.WalkupScan = true
		case strings.HasPrefix(uri, "/EventMgmt/"):
			caps.EventTable = true
		case strings.HasPrefix(uri, "/Scan/"):
			caps.Scan = true
		}
	}
	slog.Debug("ledm: discovery", "walkup_scan_to_comp", caps.WalkupScanToComp,
		"walkup_scan", caps.WalkupScan, "event_table", caps.EventTable, "scan", caps.Scan,
		"resources", len(caps.Resources))
	return caps, nil
}

// ---- Scanner status and capabilities -------------------------------------

// ScanStatus is the scanner's idle/busy state.
type ScanStatus struct {
	ScannerState string `xml:"ScannerState"`
	AdfState     string `xml:"AdfState"`
}

// Status reads /Scan/Status.
func (c *Client) Status(ctx context.Context) (ScanStatus, error) {
	var st ScanStatus
	r, err := c.get(ctx, "/Scan/Status")
	if err != nil {
		return st, fmt.Errorf("scan status: %w", err)
	}
	if err := xml.Unmarshal(r.Body, &st); err != nil {
		return st, fmt.Errorf("parse scan status: %w", err)
	}
	slog.Debug("ledm: scan status", "scanner_state", st.ScannerState, "adf_state", st.AdfState)
	return st, nil
}

// PlatenCaps is the flatbed scan area (units of 1/300 inch) and resolutions.
type PlatenCaps struct {
	MaxWidth      int `xml:"InputSourceCaps>MaxWidth"`
	MaxHeight     int `xml:"InputSourceCaps>MaxHeight"`
	MinResolution int `xml:"InputSourceCaps>MinResolution"`
	MaxResolution int `xml:"InputSourceCaps>MaxResolution"`
}

type scanCaps struct {
	Platen PlatenCaps `xml:"Platen"`
}

// Caps reads /Scan/ScanCaps and returns the flatbed limits. Zero values mean
// the printer did not report them.
func (c *Client) Caps(ctx context.Context) (PlatenCaps, error) {
	r, err := c.get(ctx, "/Scan/ScanCaps")
	if err != nil {
		return PlatenCaps{}, fmt.Errorf("scan caps: %w", err)
	}
	var caps scanCaps
	if err := xml.Unmarshal(r.Body, &caps); err != nil {
		return PlatenCaps{}, fmt.Errorf("parse scan caps: %w", err)
	}
	slog.Debug("ledm: platen caps", "max_width", caps.Platen.MaxWidth, "max_height", caps.Platen.MaxHeight,
		"min_res", caps.Platen.MinResolution, "max_res", caps.Platen.MaxResolution)
	return caps.Platen, nil
}
