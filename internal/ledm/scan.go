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

// ScanSettings describes one scan job. Width and Height are in 1/300 inch.
type ScanSettings struct {
	Resolution int
	Width      int
	Height     int
	Color      bool   // false = grayscale
	Format     string // "Jpeg"
	Quality    int    // JPEG CompressionQFactor, lower is better (15..25 typical)
	Source     string // "Platen" (default) or "Adf"
}

// Input sources.
const (
	SourcePlaten = "Platen"
	SourceAdf    = "Adf"
)

func (s ScanSettings) source() string {
	if s.Source == "" {
		return SourcePlaten
	}
	return s.Source
}

// Paper sizes in 1/300 inch units.
const (
	A4Width      = 2481
	A4Height     = 3507
	LetterWidth  = 2550
	LetterHeight = 3300
)

func (s ScanSettings) xml() string {
	color := "Gray"
	if s.Color {
		color = "Color"
	}
	format := s.Format
	if format == "" {
		format = "Jpeg"
	}
	q := s.Quality
	if q <= 0 {
		q = 15
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>`+
		`<scan:ScanJob xmlns:scan="http://www.hp.com/schemas/imaging/con/cnx/scan/2008/08/19"`+
		` xmlns:dd="http://www.hp.com/schemas/imaging/con/dictionaries/1.0/"`+
		` xmlns:fw="http://www.hp.com/schemas/imaging/con/firewall/2011/01/05">`+
		`<scan:XResolution>%d</scan:XResolution><scan:YResolution>%d</scan:YResolution>`+
		`<scan:XStart>0</scan:XStart><scan:YStart>0</scan:YStart>`+
		`<scan:Width>%d</scan:Width><scan:Height>%d</scan:Height>`+
		`<scan:Format>%s</scan:Format><scan:CompressionQFactor>%d</scan:CompressionQFactor>`+
		`<scan:ColorSpace>%s</scan:ColorSpace><scan:BitDepth>8</scan:BitDepth>`+
		`<scan:InputSource>%s</scan:InputSource><scan:GrayRendering>NTSC</scan:GrayRendering>`+
		`<scan:ToneMap><scan:Gamma>1000</scan:Gamma><scan:Brightness>1000</scan:Brightness>`+
		`<scan:Contrast>1000</scan:Contrast><scan:Highlite>179</scan:Highlite><scan:Shadow>25</scan:Shadow></scan:ToneMap>`+
		`<scan:ContentType>Document</scan:ContentType>`+
		`</scan:ScanJob>`,
		s.Resolution, s.Resolution, s.Width, s.Height, format, q, color, s.source())
}

type jobXML struct {
	State string `xml:"JobState"`
	Pre   []struct {
		PageNumber int    `xml:"PageNumber"`
		PageState  string `xml:"PageState"`
		BinaryURL  string `xml:"BinaryURL"`
	} `xml:"ScanJob>PreScanPage"`
	Post []struct {
		PageNumber int    `xml:"PageNumber"`
		PageState  string `xml:"PageState"`
	} `xml:"ScanJob>PostScanPage"`
}

// WaitIdle polls /Scan/Status until the scanner reports Idle or ctx ends,
// returning the last status (which includes the feeder state).
func (c *Client) WaitIdle(ctx context.Context) (ScanStatus, error) {
	for {
		st, err := c.Status(ctx)
		if err != nil {
			return st, err
		}
		if strings.EqualFold(st.ScannerState, "Idle") {
			return st, nil
		}
		slog.Debug("ledm: scanner busy, waiting", "state", st.ScannerState)
		select {
		case <-ctx.Done():
			return st, fmt.Errorf("wait idle: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// ScanPage runs one flatbed scan and returns the image bytes (JPEG).
func (c *Client) ScanPage(ctx context.Context, s ScanSettings) ([]byte, error) {
	s.Source = SourcePlaten
	pages, err := c.ScanPages(ctx, s)
	if err != nil {
		return nil, err
	}
	return pages[0], nil
}

// ScanPages runs one scan job and returns every page it produced: one for the
// flatbed, one per sheet for the document feeder.
func (c *Client) ScanPages(ctx context.Context, s ScanSettings) ([][]byte, error) {
	slog.Info("ledm: starting scan", "source", s.source(), "resolution", s.Resolution,
		"width", s.Width, "height", s.Height, "color", s.Color)
	jobURL, err := c.createJob(ctx, s)
	if err != nil {
		return nil, err
	}
	var pages [][]byte
	got := map[int]bool{}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		job, err := c.getJob(ctx, jobURL)
		if err != nil {
			return nil, err
		}
		for _, p := range job.Pre {
			if got[p.PageNumber] || !strings.EqualFold(p.PageState, "ReadyToUpload") || p.BinaryURL == "" {
				continue
			}
			img, err := c.download(ctx, p.BinaryURL)
			if err != nil {
				return nil, err
			}
			got[p.PageNumber] = true
			pages = append(pages, img)
			deadline = time.Now().Add(5 * time.Minute)
			slog.Info("ledm: page received", "page", p.PageNumber, "bytes", len(img))
		}
		if jobFinished(job, s.source(), len(pages)) {
			if len(pages) == 0 {
				return nil, fmt.Errorf("scan job ended in state %q without a page", job.State)
			}
			slog.Info("ledm: scan finished", "job", jobURL, "state", job.State, "pages", len(pages))
			return pages, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("scan: %w", ctx.Err())
		case <-time.After(700 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("scan job %s did not finish within 5 minutes", jobURL)
}

// jobFinished decides when to stop polling. Some firmwares never leave
// "Processing" for flatbed jobs, so a single uploaded page is enough there;
// feeder jobs run until the printer reports a terminal state.
func jobFinished(j jobXML, source string, pages int) bool {
	switch strings.ToLower(j.State) {
	case "completed", "canceled", "aborted":
		return true
	}
	return source == SourcePlaten && pages > 0 && uploadCompleted(j)
}

func (c *Client) createJob(ctx context.Context, s ScanSettings) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := c.do(ctx, http.MethodPost, "/Scan/Jobs", []byte(s.xml()), nil)
	if err != nil {
		return "", fmt.Errorf("create scan job: %w", err)
	}
	if r.Status != http.StatusCreated && r.Status != http.StatusOK {
		return "", &StatusError{Method: "POST", Path: "/Scan/Jobs", Status: r.Status, Body: r.Body}
	}
	if r.Location == "" {
		return "", fmt.Errorf("create scan job: no Location header in %d reply", r.Status)
	}
	slog.Debug("ledm: scan job created", "job", r.Location)
	return r.Location, nil
}

func (c *Client) getJob(ctx context.Context, url string) (jobXML, error) {
	var job jobXML
	r, err := c.get(ctx, url)
	if err != nil {
		return job, fmt.Errorf("get scan job: %w", err)
	}
	if err := xml.Unmarshal(r.Body, &job); err != nil {
		return job, fmt.Errorf("parse scan job: %w", err)
	}
	slog.Debug("ledm: scan job state", "state", job.State, "pre_pages", len(job.Pre), "post_pages", len(job.Post))
	return job, nil
}

func uploadCompleted(j jobXML) bool {
	for _, p := range j.Post {
		if strings.EqualFold(p.PageState, "UploadCompleted") {
			return true
		}
	}
	return false
}

func (c *Client) download(ctx context.Context, url string) ([]byte, error) {
	slog.Debug("ledm: downloading page", "url", url)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	r, err := c.do(ctx, http.MethodGet, url, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("download page: %w", err)
	}
	if r.Status < 200 || r.Status > 299 {
		return nil, &StatusError{Method: "GET", Path: url, Status: r.Status, Body: r.Body}
	}
	slog.Debug("ledm: page downloaded", "bytes", len(r.Body), "content_type", r.Header.Get("Content-Type"))
	return r.Body, nil
}
