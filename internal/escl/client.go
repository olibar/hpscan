// Package escl talks to the eSCL (AirScan / Mopria) scan interface exposed by
// HP all-in-ones from roughly 2016 onwards. Newer models (OfficeJet Pro 91xx,
// ENVY Inspire, ...) dropped the LEDM /Scan endpoints and offer eSCL only, so
// this package provides the same scan-job primitives the ledm package does:
// capabilities, scanner status and a scan job that yields one or more pages.
//
// eSCL only covers scans the computer asks for. Panel-initiated
// "Scan to Computer" on these models lives behind HP's CDM services, which
// need an authenticated bearer token; see the cdm package.
package escl

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// XML namespaces used by the eSCL request bodies.
const (
	nsPWG  = "http://www.pwg.org/schemas/2010/12/sm"
	nsScan = "http://schemas.hp.com/imaging/escl/2011/05/03"
)

// Root is the path prefix every eSCL resource sits under.
const Root = "/eSCL"

// Client is a thin HTTP client bound to one printer's eSCL interface.
type Client struct {
	BaseURL string // e.g. http://192.168.1.10/eSCL
	http    *http.Client
}

// New creates a client for host:port. Port 0 means the default HTTP port,
// which is where every eSCL implementation seen so far serves the interface
// (the LEDM port 8080 does not carry it).
func New(host string, port int) *Client {
	hostport := host
	if port > 0 && port != 80 {
		hostport = net.JoinHostPort(host, fmt.Sprint(port))
	}
	base := "http://" + hostport + Root
	slog.Debug("escl: new client", "base_url", base)
	return &Client{
		BaseURL: base,
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
}

// StatusError reports an unexpected HTTP status from the printer.
type StatusError struct {
	Method string
	Path   string
	Status int
	Body   []byte
}

func (e *StatusError) Error() string {
	b := strings.TrimSpace(string(e.Body))
	if len(b) > 300 {
		b = b[:300] + "..."
	}
	return fmt.Sprintf("%s %s: http %d: %s", e.Method, e.Path, e.Status, b)
}

// do performs one request against an absolute URL or a path below the eSCL
// root. Body may be nil.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (int, http.Header, []byte, error) {
	url := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		url = c.BaseURL + path
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build request %s %s: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "text/xml")
	}
	slog.Debug("escl: request", "method", method, "url", url, "body_bytes", len(body))
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: read body: %w", method, url, err)
	}
	slog.Debug("escl: response", "method", method, "url", url, "status", resp.StatusCode,
		"body_bytes", len(data), "location", resp.Header.Get("Location"),
		"elapsed", time.Since(start).Round(time.Millisecond))
	return resp.StatusCode, resp.Header, data, nil
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	status, _, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, &StatusError{Method: "GET", Path: path, Status: status, Body: body}
	}
	return body, nil
}

// Fetch returns the raw body and status of a GET below the eSCL root; used by
// the probe command, which wants the body whatever the status.
func (c *Client) Fetch(ctx context.Context, path string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	status, _, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, 0, err
	}
	return body, status, nil
}

// Probe reports whether the host answers on the eSCL interface. It is the
// cheapest way to tell an eSCL-only printer from a LEDM one.
func (c *Client) Probe(ctx context.Context) bool {
	_, err := c.get(ctx, "/ScannerStatus")
	if err != nil {
		slog.Debug("escl: probe failed", "base_url", c.BaseURL, "err", err)
		return false
	}
	return true
}

// ---- capabilities ---------------------------------------------------------

// SourceCaps is the scan area (in units of 1/300 inch) of one input source.
type SourceCaps struct {
	MinWidth  int `xml:"MinWidth"`
	MaxWidth  int `xml:"MaxWidth"`
	MinHeight int `xml:"MinHeight"`
	MaxHeight int `xml:"MaxHeight"`

	Resolutions []int    `xml:"SettingProfiles>SettingProfile>SupportedResolutions>DiscreteResolutions>DiscreteResolution>XResolution"`
	ColorModes  []string `xml:"SettingProfiles>SettingProfile>ColorModes>ColorMode"`
	Formats     []string `xml:"SettingProfiles>SettingProfile>DocumentFormats>DocumentFormatExt"`
}

// MaxResolution returns the highest resolution the source advertises, 0 when
// it advertises none.
func (s SourceCaps) MaxResolution() int {
	max := 0
	for _, r := range s.Resolutions {
		if r > max {
			max = r
		}
	}
	return max
}

// Supports reports whether the source advertises a document format.
func (s SourceCaps) Supports(format string) bool {
	for _, f := range s.Formats {
		if strings.EqualFold(f, format) {
			return true
		}
	}
	return false
}

// Caps is the subset of ScannerCapabilities hpscan needs.
type Caps struct {
	MakeAndModel string      `xml:"MakeAndModel"`
	SerialNumber string      `xml:"SerialNumber"`
	UUID         string      `xml:"UUID"`
	Platen       SourceCaps  `xml:"Platen>PlatenInputCaps"`
	Adf          *SourceCaps `xml:"Adf>AdfSimplexInputCaps"`
	AdfDuplex    *SourceCaps `xml:"Adf>AdfDuplexInputCaps"`
	AdfOptions   []string    `xml:"Adf>AdfOptions>AdfOption"`
}

// HasAdf reports whether the scanner has a document feeder.
func (c Caps) HasAdf() bool { return c.Adf != nil }

// HasAdfDuplex reports whether the feeder can scan both sides in one pass.
func (c Caps) HasAdfDuplex() bool { return c.AdfDuplex != nil }

// DetectsPaperLoaded reports whether ScannerStatus tells us honestly whether
// there is paper in the feeder. Without it, the feeder state cannot be
// trusted to choose between flatbed and feeder.
func (c Caps) DetectsPaperLoaded() bool {
	for _, o := range c.AdfOptions {
		if strings.EqualFold(o, "DetectPaperLoaded") {
			return true
		}
	}
	return false
}

// Capabilities reads /eSCL/ScannerCapabilities.
func (c *Client) Capabilities(ctx context.Context) (Caps, error) {
	var caps Caps
	body, err := c.get(ctx, "/ScannerCapabilities")
	if err != nil {
		return caps, fmt.Errorf("scanner capabilities: %w", err)
	}
	if err := xml.Unmarshal(body, &caps); err != nil {
		return caps, fmt.Errorf("parse scanner capabilities: %w", err)
	}
	slog.Debug("escl: capabilities", "model", caps.MakeAndModel,
		"platen_max", fmt.Sprintf("%dx%d", caps.Platen.MaxWidth, caps.Platen.MaxHeight),
		"platen_max_res", caps.Platen.MaxResolution(), "adf", caps.HasAdf(),
		"adf_duplex", caps.HasAdfDuplex(), "detect_paper", caps.DetectsPaperLoaded())
	return caps, nil
}

// ---- status ---------------------------------------------------------------

// Status is the scanner's idle/busy state and feeder state.
type Status struct {
	State    string `xml:"State"`
	AdfState string `xml:"AdfState"`
}

// Idle reports whether the scanner is ready to accept a job.
func (s Status) Idle() bool { return strings.EqualFold(s.State, "Idle") }

// PaperLoaded reports whether the feeder holds paper. Only meaningful when
// Caps.DetectsPaperLoaded is true.
func (s Status) PaperLoaded() bool { return strings.EqualFold(s.AdfState, "ScannerAdfLoaded") }

// Status reads /eSCL/ScannerStatus.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	body, err := c.get(ctx, "/ScannerStatus")
	if err != nil {
		return st, fmt.Errorf("scanner status: %w", err)
	}
	if err := xml.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("parse scanner status: %w", err)
	}
	slog.Debug("escl: status", "state", st.State, "adf_state", st.AdfState)
	return st, nil
}

// WaitIdle polls the scanner until it reports Idle or ctx ends.
func (c *Client) WaitIdle(ctx context.Context) (Status, error) {
	for {
		st, err := c.Status(ctx)
		if err != nil {
			return st, err
		}
		if st.Idle() {
			return st, nil
		}
		slog.Debug("escl: scanner busy, waiting", "state", st.State)
		select {
		case <-ctx.Done():
			return st, fmt.Errorf("wait idle: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// ---- scan jobs ------------------------------------------------------------

// Input sources, spelled as the eSCL schema spells them.
const (
	SourcePlaten = "Platen"
	SourceAdf    = "Feeder"
)

// Document formats.
const (
	FormatJPEG = "image/jpeg"
	FormatPDF  = "application/pdf"
)

// Paper sizes in 1/300 inch units, matching the ledm package.
const (
	A4Width      = 2481
	A4Height     = 3507
	LetterWidth  = 2550
	LetterHeight = 3300
)

// ScanSettings describes one scan job. Width and Height are in 1/300 inch.
type ScanSettings struct {
	Resolution int
	Width      int
	Height     int
	Color      bool   // false = grayscale
	Format     string // FormatJPEG (default) or FormatPDF
	Source     string // SourcePlaten (default) or SourceAdf
	Duplex     bool   // feeder only, and only when the scanner advertises it
}

func (s ScanSettings) source() string {
	if s.Source == "" {
		return SourcePlaten
	}
	return s.Source
}

func (s ScanSettings) format() string {
	if s.Format == "" {
		return FormatJPEG
	}
	return s.Format
}

func (s ScanSettings) colorMode() string {
	if s.Color {
		return "RGB24"
	}
	return "Grayscale8"
}

// xml renders the ScanSettings request body.
//
// Two details are measured against real HP firmware rather than taken from the
// eSCL specification, and both produce a bare "409 Conflict" with no message
// when they are wrong:
//
//   - ScanRegions and ScanRegion are in HP's scan: namespace, NOT the pwg:
//     namespace the eSCL/Mopria spec puts them in. Their child elements stay
//     in pwg:. A spec-conformant <pwg:ScanRegions> is rejected outright, which
//     is why a scan with no region at all succeeds where a correct one fails.
//   - The format goes in scan:DocumentFormatExt. Element order follows the
//     schema sequence.
//
// Measured on an OfficeJet Pro 9120e (pwg:Version 2.9, mopria-certified-scan
// 1.5) in September 2026.
func (s ScanSettings) xml() string {
	region := ""
	if s.Width > 0 && s.Height > 0 {
		region = fmt.Sprintf(
			`<scan:ScanRegions>`+
				`<scan:ScanRegion>`+
				`<pwg:Height>%d</pwg:Height>`+
				`<pwg:ContentRegionUnits>escl:ThreeHundredthsOfInch</pwg:ContentRegionUnits>`+
				`<pwg:Width>%d</pwg:Width>`+
				`<pwg:XOffset>0</pwg:XOffset><pwg:YOffset>0</pwg:YOffset>`+
				`</scan:ScanRegion>`+
				`</scan:ScanRegions>`, s.Height, s.Width)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>`+
		`<scan:ScanSettings xmlns:pwg=%q xmlns:scan=%q xmlns:escl=%q>`+
		`<pwg:Version>2.9</pwg:Version>`+
		`%s`+
		`<scan:DocumentFormatExt>%s</scan:DocumentFormatExt>`+
		`<pwg:InputSource>%s</pwg:InputSource>`+
		`<scan:XResolution>%d</scan:XResolution>`+
		`<scan:YResolution>%d</scan:YResolution>`+
		`<scan:ColorMode>%s</scan:ColorMode>`+
		`<scan:Duplex>%t</scan:Duplex>`+
		`</scan:ScanSettings>`,
		nsPWG, nsScan, nsScan, region, s.format(), s.source(),
		s.Resolution, s.Resolution, s.colorMode(), s.Duplex && s.source() == SourceAdf)
}

// ScanPage runs one flatbed scan and returns the image bytes.
func (c *Client) ScanPage(ctx context.Context, s ScanSettings) ([]byte, error) {
	s.Source = SourcePlaten
	pages, err := c.ScanPages(ctx, s)
	if err != nil {
		return nil, err
	}
	return pages[0], nil
}

// ScanPages runs one scan job and returns every page it produced: one for the
// flatbed, one per side for the document feeder.
//
// eSCL has no job state to poll. The job is a queue of documents: GET
// NextDocument yields the next page and answers 404 once the job is done.
func (c *Client) ScanPages(ctx context.Context, s ScanSettings) ([][]byte, error) {
	slog.Info("escl: starting scan", "source", s.source(), "resolution", s.Resolution,
		"width", s.Width, "height", s.Height, "color", s.Color, "format", s.format(),
		"duplex", s.Duplex && s.source() == SourceAdf)
	jobURL, err := c.createJob(ctx, s)
	if err != nil {
		return nil, err
	}
	// A job left open holds the scanner busy, so always close it out.
	defer c.deleteJob(jobURL)

	var pages [][]byte
	for {
		page, more, err := c.nextDocument(ctx, jobURL)
		if err != nil {
			if len(pages) > 0 {
				slog.Warn("escl: scan interrupted, keeping the pages received",
					"job", jobURL, "pages", len(pages), "err", err)
				return pages, nil
			}
			return nil, err
		}
		if !more {
			break
		}
		pages = append(pages, page)
		slog.Info("escl: page received", "page", len(pages), "bytes", len(page))
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("scan job %s produced no page: is there paper in the feeder?", jobURL)
	}
	slog.Info("escl: scan finished", "job", jobURL, "pages", len(pages))
	return pages, nil
}

func (c *Client) createJob(ctx context.Context, s ScanSettings) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body := []byte(s.xml())
	status, hdr, resp, err := c.do(ctx, http.MethodPost, "/ScanJobs", body)
	if err != nil {
		return "", fmt.Errorf("create scan job: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", &StatusError{Method: "POST", Path: "/ScanJobs", Status: status, Body: resp}
	}
	loc := hdr.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("create scan job: no Location header in %d reply", status)
	}
	// Some firmwares answer with a full URL and others with a server-absolute
	// path. BaseURL already ends in /eSCL, so the latter must be resolved or
	// every page fetch would go to /eSCL/eSCL/ScanJobs/... and 404.
	job := c.abs(strings.TrimSuffix(loc, "/"))
	slog.Debug("escl: scan job created", "job", job)
	return job, nil
}

// nextDocument fetches one page. more is false when the job has no page left,
// which the printer signals with 404 (some firmwares use 410).
func (c *Client) nextDocument(ctx context.Context, jobURL string) (page []byte, more bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	url := jobURL + "/NextDocument"
	for {
		status, _, body, err := c.do(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, false, fmt.Errorf("next document: %w", err)
		}
		switch {
		case status == http.StatusNotFound, status == http.StatusGone:
			return nil, false, nil
		case status == http.StatusServiceUnavailable:
			// The page is not ready yet; the scanner is still moving.
			slog.Debug("escl: page not ready, retrying")
			select {
			case <-ctx.Done():
				return nil, false, fmt.Errorf("next document: %w", ctx.Err())
			case <-time.After(500 * time.Millisecond):
			}
			continue
		case status < 200 || status > 299:
			return nil, false, &StatusError{Method: "GET", Path: url, Status: status, Body: body}
		case len(body) == 0:
			// An empty 200 means the same thing as 404 on some firmwares.
			return nil, false, nil
		}
		return body, true, nil
	}
}

// deleteJob releases the job. Failures are logged and ignored: the scan has
// already been collected by this point.
func (c *Client) deleteJob(jobURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, _, err := c.do(ctx, http.MethodDelete, jobURL, nil); err != nil {
		slog.Debug("escl: delete job failed", "job", jobURL, "err", err)
	}
}
