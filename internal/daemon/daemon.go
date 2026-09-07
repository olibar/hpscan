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
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/olibar/hpscan/internal/config"
	"github.com/olibar/hpscan/internal/discover"
	"github.com/olibar/hpscan/internal/escl"
	"github.com/olibar/hpscan/internal/ledm"
	"github.com/olibar/hpscan/internal/pdf"
)

// Daemon holds the connection state for one printer.
//
// A printer speaks either LEDM (2010-2020 models) or eSCL (2020 onwards);
// the client for the one in use is set and the other is nil. The output half
// of the daemon - documents, PDF assembly, file naming - is shared.
type Daemon struct {
	cfg      config.Config
	client   *ledm.Client
	flavor   ledm.Flavor
	destURI  string
	caps     ledm.ScanCaps
	doc      *document // in-progress multi-page PDF, nil when idle
	jpegPage int       // page counter for jpeg output within one walkup job
	seen     map[string]string

	// eSCL backend, used when the printer has no LEDM interface. esub is
	// guarded because the event poller reads it while the handler may be
	// replacing it after the printer forgets a subscription.
	escl   *escl.Client
	esubMu sync.Mutex
	esub   *escl.Subscription
	ecaps  escl.Caps
}

// document accumulates pages until the printer says the job is complete.
type document struct {
	pages    []pdf.Page
	format   string
	started  time.Time
	lastPage time.Time
}

// resolveTarget turns cfg.Printer ("host", "host:port", or empty for mDNS)
// into a host and a LEDM port.
func resolveTarget(ctx context.Context, cfg config.Config) (string, int, error) {
	host, port := cfg.Printer, cfg.Port
	if h, p, err := net.SplitHostPort(host); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			host, port = h, n
		}
	}
	if host == "" {
		slog.Info("daemon: no printer configured, discovering via mDNS")
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		s, err := discover.FirstHP(dctx)
		if err != nil {
			return "", 0, fmt.Errorf("discover printer: %w", err)
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
	return host, port, nil
}

// Connect resolves the printer (config or mDNS) and returns a ready LEDM
// client. cfg.Printer may be "host" or "host:port"; empty means mDNS.
func Connect(ctx context.Context, cfg config.Config) (*ledm.Client, error) {
	host, port, err := resolveTarget(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return ledm.New(host, port), nil
}

// Resolve reports the address a session would use: the configured printer, or
// the first one found via mDNS when none is configured. Callers that need to
// build their own clients (the probe command builds one per interface) must
// use this rather than cfg.Printer, which is empty in the discovery case.
func Resolve(ctx context.Context, cfg config.Config) (host string, port int, err error) {
	return resolveTarget(ctx, cfg)
}

// ConnectAny resolves the printer and returns a client for whichever scan
// interface it actually offers: LEDM on 2010-2020 models, eSCL on newer ones.
// Exactly one of the returned clients is non-nil.
func ConnectAny(ctx context.Context, cfg config.Config) (*ledm.Client, *escl.Client, error) {
	host, port, err := resolveTarget(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	lc := ledm.New(host, port)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, lerr := lc.Discover(pctx)
	cancel()
	if lerr == nil {
		return lc, nil, nil
	}
	slog.Debug("daemon: no LEDM interface, trying eSCL", "printer", host, "error", lerr)
	ec := escl.New(host, 0)
	pctx, cancel = context.WithTimeout(ctx, 10*time.Second)
	ok := ec.Probe(pctx)
	cancel()
	if ok {
		return nil, ec, nil
	}
	return nil, nil, fmt.Errorf("%s answers on neither the LEDM nor the eSCL scan interface: %w", host, lerr)
}

// Run blocks until ctx is cancelled. It serves every printer listed in the
// config (comma-separated) or, when none is configured, every HP scanner
// found via mDNS, each in its own loop.
func Run(ctx context.Context, cfg config.Config) error {
	if err := os.MkdirAll(cfg.ExpandedOutputDir(), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	targets, err := resolveTargets(ctx, cfg)
	if err != nil {
		return err
	}
	slog.Info("daemon: serving printers", "count", len(targets), "targets", strings.Join(targets, ", "))
	var wg sync.WaitGroup
	for _, t := range targets {
		one := cfg
		one.Printer = t
		wg.Add(1)
		go func() {
			defer wg.Done()
			runLoop(ctx, one)
		}()
	}
	wg.Wait()
	return nil
}

// resolveTargets returns the printer addresses to serve. Without a configured
// printer it discovers all HP scanners, retrying until one shows up.
func resolveTargets(ctx context.Context, cfg config.Config) ([]string, error) {
	if cfg.Printer != "" {
		var out []string
		for _, p := range strings.Split(cfg.Printer, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	}
	backoff := 5 * time.Second
	for {
		slog.Info("daemon: no printer configured, discovering all HP scanners via mDNS")
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		found, err := discover.AllHP(dctx)
		cancel()
		if err == nil {
			var out []string
			for _, s := range found {
				port := s.Port
				if port == 0 {
					port = 8080
				}
				slog.Info("daemon: discovered printer", "name", s.Name, "host", s.Address(), "port", port)
				out = append(out, net.JoinHostPort(s.Address(), strconv.Itoa(port)))
			}
			return out, nil
		}
		slog.Warn("daemon: discovery failed, retrying", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

// rediscoverTarget finds the configured printer (by Bonjour hostname or IP)
// in the mDNS results and returns its current address. It never substitutes a
// different printer. The address is returned rather than a client because the
// printer found may speak either interface.
func rediscoverTarget(ctx context.Context, cfg config.Config) (string, int, error) {
	want := strings.ToLower(strings.TrimSuffix(cfg.Printer, "."))
	if h, _, err := net.SplitHostPort(want); err == nil {
		want = h
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	found, err := discover.AllHP(dctx)
	if err != nil {
		return "", 0, fmt.Errorf("rediscover %s: %w", cfg.Printer, err)
	}
	for _, s := range found {
		host := strings.ToLower(strings.TrimSuffix(s.Host, "."))
		if host == want || s.IP == want {
			slog.Info("daemon: printer found via mDNS", "printer", cfg.Printer, "host", s.Address(), "port", s.Port)
			port := s.Port
			if port == 0 {
				port = 8080
			}
			return s.Address(), port, nil
		}
	}
	return "", 0, fmt.Errorf("printer %s not found via mDNS (%d HP scanners seen)", cfg.Printer, len(found))
}

// runLoop keeps one printer session alive, reconnecting after errors.
func runLoop(ctx context.Context, cfg config.Config) {
	backoff := 5 * time.Second
	for {
		started := time.Now()
		err := runOnce(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = 5 * time.Second // the session was healthy; do not carry over old backoff
		}
		slog.Error("daemon: session ended, will reconnect", "printer", cfg.Printer, "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

// errNoScanInterface means an address answered on neither LEDM nor eSCL. It
// is the only failure worth retrying at a different address.
var errNoScanInterface = errors.New("no scan interface")

func runOnce(ctx context.Context, cfg config.Config) error {
	host, port, err := resolveTarget(ctx, cfg)
	if err != nil {
		return err
	}
	err = serveOne(ctx, cfg, host, port)
	if !errors.Is(err, errNoScanInterface) || cfg.Printer == "" {
		return err
	}
	// The pinned address may be stale (DHCP gave the printer a new IP): look
	// for the same printer via mDNS and redo the whole selection there. The
	// printer found may well be an eSCL-only one, so this cannot shortcut
	// straight back to LEDM.
	slog.Warn("daemon: configured printer unreachable, looking it up via mDNS", "printer", cfg.Printer, "error", err)
	rhost, rport, rerr := rediscoverTarget(ctx, cfg)
	if rerr != nil {
		return rerr
	}
	return serveOne(ctx, cfg, rhost, rport)
}

// serveOne runs one full session against a single address, using whichever
// scan interface that address offers.
func serveOne(ctx context.Context, cfg config.Config, host string, port int) error {
	d := &Daemon{cfg: cfg, client: ledm.New(host, port), seen: map[string]string{}}
	ledmErr := d.setup(ctx)
	if ledmErr == nil {
		defer d.teardown()
		return d.loop(ctx)
	}
	slog.Debug("daemon: no LEDM interface here", "printer", host, "error", ledmErr)

	// Newer printers dropped /DevMgmt and /Scan entirely and offer only eSCL,
	// on the default HTTP port rather than 8080.
	ec := escl.New(host, 0)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	isESCL := ec.Probe(pctx)
	cancel()
	if !isESCL {
		return fmt.Errorf("%w: %s answers on neither LEDM (%v) nor eSCL", errNoScanInterface, host, ledmErr)
	}
	slog.Info("daemon: printer has no LEDM interface, using eSCL", "printer", host)
	d.escl = ec
	if err := d.esclSetup(ctx); err != nil {
		return err
	}
	defer d.esclTeardown()
	return d.esclLoop(ctx)
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
	if d.caps, err = d.client.Caps(ctx); err != nil {
		slog.Warn("daemon: could not read scan caps, using paper size as-is", "error", err)
	}
	return d.register(ctx)
}

func (d *Daemon) register(ctx context.Context) error {
	// The printer panel displays the Hostname field, not Name, so send the
	// configured name in both.
	uri, err := d.client.RegisterDestination(ctx, d.flavor, d.cfg.Name, d.cfg.Name)
	if err != nil {
		return err
	}
	d.destURI = uri
	slog.Info("daemon: ready, select this computer on the printer", "printer", d.cfg.Printer, "name", d.cfg.Name,
		"flavor", d.flavor.String(), "adf", d.caps.HasAdf())
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

// scanPage scans from the flatbed, or from the document feeder when it has
// paper, and either writes the pages (jpeg) or appends them to the current
// document (pdf). single forces the document closed afterwards.
func (d *Daemon) scanPage(ctx context.Context, dst ledm.Destination, single bool) error {
	format := d.formatFor(dst.Shortcut)
	wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	st, err := d.client.WaitIdle(wctx)
	cancel()
	if err != nil {
		return err
	}
	settings := d.settings(d.caps.HasAdf() && strings.EqualFold(st.AdfState, "Loaded"))
	pages, err := d.client.ScanPages(ctx, settings)
	if err != nil {
		return err
	}
	if settings.Source == ledm.SourceAdf {
		single = true // a feeder run is a complete document
	}
	if format == "jpeg" {
		for _, img := range pages {
			d.jpegPage++
			if err := d.writeJPEG(img, d.jpegPage); err != nil {
				return err
			}
		}
		if single {
			d.jpegPage = 0
		}
		return nil
	}
	if d.doc == nil {
		d.doc = &document{format: "pdf", started: time.Now()}
	}
	for _, img := range pages {
		d.doc.pages = append(d.doc.pages, pdf.Page{JPEG: img, DPI: settings.Resolution})
	}
	d.doc.lastPage = time.Now()
	slog.Info("daemon: pages captured", "new", len(pages), "total", len(d.doc.pages))
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

func (d *Daemon) settings(useAdf bool) ledm.ScanSettings {
	s := ledm.ScanSettings{Resolution: d.cfg.Resolution, Color: d.cfg.ColorMode == "color", Format: "Jpeg",
		Source: ledm.SourcePlaten}
	caps := d.caps.Platen
	if useAdf {
		s.Source = ledm.SourceAdf
		caps = *d.caps.Adf
	}
	if d.cfg.Paper == "letter" {
		s.Width, s.Height = ledm.LetterWidth, ledm.LetterHeight
	} else {
		s.Width, s.Height = ledm.A4Width, ledm.A4Height
	}
	if caps.MaxWidth > 0 && s.Width > caps.MaxWidth {
		s.Width = caps.MaxWidth
	}
	if caps.MaxHeight > 0 && s.Height > caps.MaxHeight {
		s.Height = caps.MaxHeight
	}
	if max := caps.EffectiveMaxResolution(); max > 0 && s.Resolution > max {
		s.Resolution = max
	}
	slog.Debug("daemon: scan settings", "source", s.Source, "width", s.Width, "height", s.Height, "resolution", s.Resolution)
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
		_ = os.Remove(path) // drop the reserved placeholder
		return
	}
	if err := writeAtomic(path, buf.Bytes()); err != nil {
		slog.Error("daemon: write pdf failed", "path", path, "error", err)
		_ = os.Remove(path)
		return
	}
	slog.Info("daemon: saved", "path", path, "pages", len(doc.pages))
}

// writeAtomic writes to a hidden temporary file in the same folder and
// renames it into place, so sync tools (Cloud Sync, Dropbox) see one
// complete file instead of a growing one. The final name was reserved with
// O_EXCL by outputPath, so concurrent printer sessions cannot clobber it.
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
		_ = os.Remove(path) // drop the reserved placeholder
		return fmt.Errorf("write jpeg: %w", err)
	}
	slog.Info("daemon: saved", "path", path, "bytes", len(img))
	return nil
}

// outputPath renders the filename pattern and reserves a free name by
// creating it exclusively, so two printer sessions saving in the same second
// get different files. The empty placeholder is replaced by writeAtomic.
func (d *Daemon) outputPath(ext string, page int) string {
	now := time.Now()
	name := d.cfg.Filename
	name = strings.ReplaceAll(name, "{date}", now.Format("2006-01-02"))
	name = strings.ReplaceAll(name, "{time}", now.Format("150405"))
	name = strings.ReplaceAll(name, "{page}", fmt.Sprintf("%02d", page))
	dir := d.cfg.ExpandedOutputDir()
	path := filepath.Join(dir, name+"."+ext)
	for i := 1; ; i++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return path
		}
		if !errors.Is(err, os.ErrExist) {
			slog.Warn("daemon: cannot reserve output name, using it anyway", "path", path, "error", err)
			return path
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.%s", name, i, ext))
	}
}
