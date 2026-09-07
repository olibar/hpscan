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
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
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
//
// Set HPSCAN_ESCL_TLS=1 to talk to the interface over HTTPS on 443 instead,
// which is what HP's own client does. The printer serves the same resources
// on both, with a self-signed certificate that is not verified - there is
// nothing secret here, and the printer is the only party that could be
// impersonated on the local network.
func New(host string, port int) *Client {
	if os.Getenv("HPSCAN_ESCL_TLS") != "" {
		hostport := host
		if port > 0 && port != 80 && port != 443 {
			hostport = net.JoinHostPort(host, fmt.Sprint(port))
		}
		base := "https://" + hostport + Root
		slog.Debug("escl: new client", "base_url", base, "tls", true)
		return &Client{
			BaseURL: base,
			http: &http.Client{
				Timeout:   5 * time.Minute,
				Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
			},
		}
	}
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

	// Intents are the scan's purpose, as advertised in ScannerCapabilities.
	// HP's own client always sends Document for scan-to-computer.
	IntentDocument       = "Document"
	IntentPhoto          = "Photo"
	IntentTextAndGraphic = "TextAndGraphic"
	IntentPreview        = "Preview"
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

	// Intent is the scan's purpose - one of the values the scanner lists in
	// its capabilities, typically Document, TextAndGraphic, Photo or Preview.
	// Omitted when empty, which is what the scanner's own default applies.
	Intent string

	// JobSource says who is asking. The scanner advertises whether it wants
	// this as JobSourceInfoSupport, and HP's own driver always sends it.
	// Omitted entirely when MachineName is empty.
	JobSource JobSourceInfo

	// ContextID ties this job to a walkup subscription, and is what makes the
	// printer treat the scan as part of the session the user started at the
	// panel rather than an unrelated pull scan. Without it the panel never
	// offers "another page or done?" after a flatbed page: the device has no
	// way to know the job is the scan it just asked for. It is the
	// subscription's UUID, and is omitted for ordinary scans.
	ContextID string
}

// JobSourceInfo identifies the client behind a scan job. HP sends these
// children without a namespace prefix, so they are in no namespace at all;
// this reproduces that rather than tidying it up, because the printer is the
// authority on what it accepts.
type JobSourceInfo struct {
	UserName    string
	UserDomain  string
	MachineName string
	AppFileName string
	Application string
}

func (j JobSourceInfo) xml() string {
	if j.MachineName == "" {
		return ""
	}
	return fmt.Sprintf(`<scan:JobSourceInfo>`+
		`<UserName>%s</UserName>`+
		`<UserDomain>%s</UserDomain>`+
		`<MachineName>%s</MachineName>`+
		`<AppFileName>%s</AppFileName>`+
		`<Application>%s</Application>`+
		`</scan:JobSourceInfo>`,
		xmlEscape(j.UserName), xmlEscape(j.UserDomain), xmlEscape(j.MachineName),
		xmlEscape(j.AppFileName), xmlEscape(j.Application))
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
	// Duplex is a feeder concept and is sent only for a feeder scan. Sending
	// it on a flatbed job - even as false - makes the printer treat the job as
	// a one-shot single-sided scan: it closes the job after the first page and
	// never offers the user the "another page or done?" choice, so a walkup
	// scan stalls and reports that the file could not be saved. HP's own
	// client omits it too.
	duplex := ""
	if s.source() == SourceAdf {
		duplex = fmt.Sprintf(`<scan:Duplex>%t</scan:Duplex>`, s.Duplex)
	}
	// ContextID sits between Version and Intent, which is where HP's own
	// client puts it.
	context := ""
	if s.ContextID != "" {
		context = fmt.Sprintf(`<scan:ContextID>%s</scan:ContextID>`, xmlEscape(s.ContextID))
	}
	// Intent follows Version in the schema's sequence, and is left out
	// entirely when unset rather than sent empty.
	intent := ""
	if s.Intent != "" {
		intent = fmt.Sprintf(`<scan:Intent>%s</scan:Intent>`, xmlEscape(s.Intent))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>`+
		`<scan:ScanSettings xmlns:pwg=%q xmlns:scan=%q xmlns:escl=%q>`+
		`<pwg:Version>2.9</pwg:Version>`+
		`%s`+
		`%s`+
		`%s`+
		`<scan:DocumentFormatExt>%s</scan:DocumentFormatExt>`+
		`<pwg:InputSource>%s</pwg:InputSource>`+
		`<scan:XResolution>%d</scan:XResolution>`+
		`<scan:YResolution>%d</scan:YResolution>`+
		`<scan:ColorMode>%s</scan:ColorMode>`+
		`%s`+
		`%s`+
		`</scan:ScanSettings>`,
		nsPWG, nsScan, nsScan, context, intent, region, s.format(), s.source(),
		s.Resolution, s.Resolution, s.colorMode(), duplex, s.JobSource.xml())
}

// StartJob creates a scan job and returns its URL without collecting any
// page. Exposed for diagnostics: it allows a caller to observe what the
// printer does while a scanned page is still sitting on it, unfetched.
func (c *Client) StartJob(ctx context.Context, s ScanSettings) (string, error) {
	return c.createJob(ctx, s)
}

// CollectPages collects the pages of a job already created with StartJob.
func (c *Client) CollectPages(ctx context.Context, jobURL string, s ScanSettings) ([][]byte, error) {
	var pages [][]byte
	for {
		page, more, err := c.nextDocument(ctx, jobURL)
		if err != nil {
			if len(pages) > 0 {
				return pages, nil
			}
			return nil, err
		}
		if !more {
			break
		}
		pages = append(pages, page)
		slog.Info("escl: page received", "page", len(pages), "bytes", len(page))
		if s.source() == SourcePlaten {
			// One page per job from the glass; the panel asks the user about
			// the next one and the printer sends another ScanRequested.
			break
		}
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("job %s produced no page", jobURL)
	}
	return pages, nil
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
	// Only tear the job down if it ended on its own. DELETE is how a client
	// cancels, and the printer records it as JobCanceledByUser - so deleting a
	// flatbed job we deliberately left open would tell the printer the user
	// abandoned the scan, while it is still waiting to ask them about another
	// page. A job that has already returned 404 is finished and the DELETE is
	// a harmless tidy-up.
	exhausted := false
	defer func() {
		if exhausted {
			c.deleteJob(jobURL)
		}
	}()

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
			exhausted = true
			break
		}
		pages = append(pages, page)
		slog.Info("escl: page received", "page", len(pages), "bytes", len(page))

		// The flatbed yields one page per job, and we stop here: the job is
		// left in Processing while the panel asks the user about another page.
		// If they add one, the printer sends a fresh ScanRequested and we run
		// a new job. The feeder is the opposite - one job delivers every sheet
		// - so there we read until the printer says 404.
		if s.source() == SourcePlaten {
			slog.Debug("escl: flatbed page collected, leaving the job open for the panel")
			break
		}
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
	// The scanner answers 503 while it is still finishing with the previous
	// page, which happens routinely between the pages of a walkup scan. HP's
	// own client simply posts again, so do the same rather than failing the
	// page.
	var status int
	var hdr http.Header
	var resp []byte
	var err error
	for attempt := 0; ; attempt++ {
		status, hdr, resp, err = c.do(ctx, http.MethodPost, "/ScanJobs", body)
		if err != nil {
			return "", fmt.Errorf("create scan job: %w", err)
		}
		if status != http.StatusServiceUnavailable || attempt >= 5 {
			break
		}
		slog.Debug("escl: scanner busy, retrying the job", "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("create scan job: %w", ctx.Err())
		case <-time.After(700 * time.Millisecond):
		}
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
