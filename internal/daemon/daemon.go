// Package daemon runs the scan-to-computer loop: register as a destination,
// wait for the user to press Scan on the printer, fetch the page(s) and
// write them to the configured folder.
package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/olivier/hpscan/internal/config"
	"github.com/olivier/hpscan/internal/discover"
	"github.com/olivier/hpscan/internal/ledm"
	"github.com/olivier/hpscan/internal/pdf"
)

// Daemon holds the connection state for one printer.
type Daemon struct {
	cfg      config.Config
	client   *ledm.Client
	flavor   ledm.Flavor
	destURI  string
	hostname string
	platen   ledm.PlatenCaps
	doc      *document // in-progress multi-page PDF, nil when idle
	jpegPage int       // page counter for jpeg output within one walkup job
	seen     map[string]string
}

// document accumulates pages until the printer says the job is complete.
type document struct {
	pages    []pdf.Page
	format   string
	started  time.Time
	lastPage time.Time
}

// Connect resolves the printer (config or mDNS) and returns a ready client.
func Connect(ctx context.Context, cfg config.Config) (*ledm.Client, error) {
	host, port := cfg.Printer, cfg.Port
	if host == "" {
		slog.Info("daemon: no printer configured, discovering via mDNS")
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		s, err := discover.FirstHP(dctx)
		if err != nil {
			return nil, fmt.Errorf("discover printer: %w", err)
		}
		host = s.Address()
		if s.Port > 0 {
			port = s.Port
		}
		slog.Info("daemon: discovered printer", "name", s.Name, "host", host, "port", port)
	}
	if port == 0 {
		port = 8080
	}
	return ledm.New(host, port), nil
}

// Run blocks until ctx is cancelled, reconnecting after errors.
func Run(ctx context.Context, cfg config.Config) error {
	if err := os.MkdirAll(cfg.ExpandedOutputDir(), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	backoff := 5 * time.Second
	for {
		started := time.Now()
		err := runOnce(ctx, cfg)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) > time.Minute {
			backoff = 5 * time.Second // the session was healthy; do not carry over old backoff
		}
		slog.Error("daemon: session ended, will reconnect", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func runOnce(ctx context.Context, cfg config.Config) error {
	client, err := Connect(ctx, cfg)
	if err != nil {
		return err
	}
	d := &Daemon{cfg: cfg, client: client, seen: map[string]string{}}
	d.hostname, _ = os.Hostname()
	if err := d.setup(ctx); err != nil {
		if cfg.Printer == "" {
			return err
		}
		// The pinned address may be stale (DHCP gave the printer a new IP):
		// fall back to mDNS discovery for this session.
		slog.Warn("daemon: configured printer unreachable, trying mDNS discovery", "printer", cfg.Printer, "error", err)
		fallback := cfg
		fallback.Printer = ""
		if d.client, err = Connect(ctx, fallback); err != nil {
			return err
		}
		if err := d.setup(ctx); err != nil {
			return err
		}
	}
	defer d.teardown()
	return d.loop(ctx)
}

func (d *Daemon) setup(ctx context.Context) error {
	caps, err := d.client.Discover(ctx)
	if err != nil {
		return err
	}
	switch {
	case caps.WalkupScanToComp:
		d.flavor = ledm.FlavorWalkupScanToComp
	case caps.WalkupScan:
		d.flavor = ledm.FlavorWalkupScan
	default:
		return fmt.Errorf("printer offers neither WalkupScan nor WalkupScanToComp (resources: %s)",
			strings.Join(caps.Resources, ", "))
	}
	if !caps.EventTable {
		slog.Warn("daemon: printer did not advertise /EventMgmt, trying anyway")
	}
	if d.platen, err = d.client.Caps(ctx); err != nil {
		slog.Warn("daemon: could not read scan caps, using paper size as-is", "error", err)
	}
	return d.register(ctx)
}

func (d *Daemon) register(ctx context.Context) error {
	uri, err := d.client.RegisterDestination(ctx, d.flavor, d.cfg.Name, d.hostname)
	if err != nil {
		return err
	}
	d.destURI = uri
	slog.Info("daemon: ready, select this computer on the printer", "name", d.cfg.Name, "flavor", d.flavor.String())
	return nil
}

func (d *Daemon) teardown() {
	if d.destURI == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.client.DeleteDestination(ctx, d.destURI); err != nil {
		slog.Debug("daemon: unregister failed", "error", err)
	}
	if d.doc != nil {
		d.finishDocument()
	}
}

// loop long-polls the event table and reacts to scan events.
func (d *Daemon) loop(ctx context.Context) error {
	table, _, err := d.client.PollEvents(ctx, ledm.EventTable{}, 0)
	if err != nil {
		return err
	}
	// Remember current stamps so old events are not replayed on startup.
	for _, e := range table.Events {
		d.seen[e.Category] = e.AgingStamp
	}
	failures := 0
	for ctx.Err() == nil {
		next, changed, err := d.client.PollEvents(ctx, table, 20)
		if err != nil {
			failures++
			slog.Warn("daemon: event poll failed", "error", err, "consecutive", failures)
			if failures >= 5 {
				return fmt.Errorf("event polling failed %d times: %w", failures, err)
			}
			d.sleep(ctx, 3*time.Second)
			continue
		}
		failures = 0
		table = next
		if changed {
			d.handleEvents(ctx, table.Events)
		} else {
			d.sleep(ctx, time.Second)
		}
		d.expireDocument()
	}
	return nil
}

func (d *Daemon) sleep(ctx context.Context, dur time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(dur):
	}
}

func (d *Daemon) handleEvents(ctx context.Context, events []ledm.Event) {
	for _, e := range events {
		if d.seen[e.Category] == e.AgingStamp {
			continue
		}
		d.seen[e.Category] = e.AgingStamp
		slog.Debug("daemon: new event", "category", e.Category, "aging_stamp", e.AgingStamp, "payloads", len(e.Payloads))
		if e.Category != "ScanEvent" {
			continue
		}
		if !d.forMe(e) {
			slog.Debug("daemon: scan event for another destination, ignoring")
			continue
		}
		if err := d.onScanEvent(ctx); err != nil {
			slog.Error("daemon: scan event handling failed", "error", err)
		}
	}
}

// forMe reports whether the event's payload references our destination.
// Events that carry no destination reference at all are treated as ours.
func (d *Daemon) forMe(e ledm.Event) bool {
	sawDestination := false
	for _, p := range e.Payloads {
		if !strings.Contains(p.ResourceType, "Destination") {
			continue
		}
		sawDestination = true
		if strings.HasSuffix(p.ResourceURI, d.destURI) || strings.HasSuffix(d.destURI, p.ResourceURI) {
			return true
		}
	}
	return !sawDestination
}

func (d *Daemon) onScanEvent(ctx context.Context) error {
	dst, err := d.client.GetDestination(ctx, d.destURI)
	if ledm.IsNotFound(err) {
		slog.Warn("daemon: destination vanished (printer reboot?), re-registering")
		if rerr := d.register(ctx); rerr != nil {
			return rerr
		}
		return nil
	}
	if err != nil {
		return err
	}
	if d.flavor == ledm.FlavorWalkupScan {
		return d.scanPage(ctx, dst, true)
	}
	typ, err := d.client.GetCompEvent(ctx)
	if err != nil {
		return err
	}
	slog.Info("daemon: walkup event", "type", string(typ), "shortcut", dst.Shortcut)
	switch typ {
	case ledm.CompScanRequested:
		if d.doc != nil {
			d.finishDocument()
		}
		d.jpegPage = 0
		return d.scanPage(ctx, dst, false)
	case ledm.CompScanNewPageRequested:
		return d.scanPage(ctx, dst, false)
	case ledm.CompScanPagesComplete:
		d.finishDocument()
		d.jpegPage = 0
	case ledm.CompHostSelected:
		// Nothing to do: the user is browsing the shortcut menu.
	default:
		slog.Warn("daemon: unknown walkup event type", "type", string(typ))
	}
	return nil
}

// scanPage scans one page and either writes it (jpeg) or appends it to the
// current document (pdf). single forces the document closed afterwards.
func (d *Daemon) scanPage(ctx context.Context, dst ledm.Destination, single bool) error {
	format := d.formatFor(dst.Shortcut)
	settings := d.settings()
	wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	err := d.client.WaitIdle(wctx)
	cancel()
	if err != nil {
		return err
	}
	img, err := d.client.ScanPage(ctx, settings)
	if err != nil {
		return err
	}
	if format == "jpeg" {
		d.jpegPage++
		if single {
			d.jpegPage = 0
			return d.writeJPEG(img, 1)
		}
		return d.writeJPEG(img, d.jpegPage)
	}
	if d.doc == nil {
		d.doc = &document{format: "pdf", started: time.Now()}
	}
	d.doc.pages = append(d.doc.pages, pdf.Page{JPEG: img, DPI: settings.Resolution})
	d.doc.lastPage = time.Now()
	slog.Info("daemon: page captured", "pages", len(d.doc.pages))
	if single {
		d.finishDocument()
	}
	return nil
}

func (d *Daemon) formatFor(shortcut string) string {
	s := strings.ToLower(shortcut)
	switch {
	case strings.Contains(s, "pdf"), strings.Contains(s, "document"):
		return "pdf"
	case strings.Contains(s, "jpeg"), strings.Contains(s, "jpg"), strings.Contains(s, "photo"):
		return "jpeg"
	}
	return d.cfg.Format
}

func (d *Daemon) settings() ledm.ScanSettings {
	s := ledm.ScanSettings{Resolution: d.cfg.Resolution, Color: d.cfg.ColorMode == "color", Format: "Jpeg"}
	if d.cfg.Paper == "letter" {
		s.Width, s.Height = ledm.LetterWidth, ledm.LetterHeight
	} else {
		s.Width, s.Height = ledm.A4Width, ledm.A4Height
	}
	if d.platen.MaxWidth > 0 && s.Width > d.platen.MaxWidth {
		s.Width = d.platen.MaxWidth
	}
	if d.platen.MaxHeight > 0 && s.Height > d.platen.MaxHeight {
		s.Height = d.platen.MaxHeight
	}
	if d.platen.MaxResolution > 0 && s.Resolution > d.platen.MaxResolution {
		s.Resolution = d.platen.MaxResolution
	}
	return s
}

// expireDocument closes a multi-page PDF that has not received a page for
// page_timeout (the printer never sent ScanPagesComplete).
func (d *Daemon) expireDocument() {
	if d.doc == nil || d.doc.format != "pdf" {
		return
	}
	if time.Since(d.doc.lastPage) > d.cfg.PageTimeoutDuration() {
		slog.Info("daemon: no further pages, closing document", "pages", len(d.doc.pages))
		d.finishDocument()
	}
}

func (d *Daemon) finishDocument() {
	doc := d.doc
	d.doc = nil
	if doc == nil || len(doc.pages) == 0 || doc.format != "pdf" {
		return
	}
	path := d.outputPath("pdf", 0)
	var buf bytes.Buffer
	if err := pdf.Write(&buf, doc.pages); err != nil {
		slog.Error("daemon: build pdf failed", "path", path, "error", err)
		return
	}
	if err := writeAtomic(path, buf.Bytes()); err != nil {
		slog.Error("daemon: write pdf failed", "path", path, "error", err)
		return
	}
	slog.Info("daemon: saved", "path", path, "pages", len(doc.pages))
}

// writeAtomic writes to a hidden temporary file in the same folder and
// renames it into place, so sync tools (Cloud Sync, Dropbox) see one
// complete file instead of a growing one.
func writeAtomic(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".part")
	slog.Debug("daemon: writing", "tmp", tmp, "bytes", len(data))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	return nil
}

func (d *Daemon) writeJPEG(img []byte, page int) error {
	path := d.outputPath("jpg", page)
	if err := writeAtomic(path, img); err != nil {
		return fmt.Errorf("write jpeg: %w", err)
	}
	slog.Info("daemon: saved", "path", path, "bytes", len(img))
	return nil
}

// outputPath renders the filename pattern and avoids collisions.
func (d *Daemon) outputPath(ext string, page int) string {
	now := time.Now()
	name := d.cfg.Filename
	name = strings.ReplaceAll(name, "{date}", now.Format("2006-01-02"))
	name = strings.ReplaceAll(name, "{time}", now.Format("150405"))
	name = strings.ReplaceAll(name, "{page}", fmt.Sprintf("%02d", page))
	dir := d.cfg.ExpandedOutputDir()
	path := filepath.Join(dir, name+"."+ext)
	for i := 1; ; i++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return path
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.%s", name, i, ext))
	}
}
