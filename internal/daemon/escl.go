package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/olibar/hpscan/internal/escl"
	"github.com/olibar/hpscan/internal/pdf"
)

// The eSCL half of the daemon: same job as the LEDM loop, for printers from
// roughly 2020 onwards that dropped LEDM. Registration, waiting and scanning
// differ; everything downstream - documents, PDF assembly, file naming - is
// shared with the LEDM path.

// esclShortcuts are the entries offered on the printer's panel. The names
// matter: the format is read back out of whichever one the user picks, both
// here and in HP's own client. These are free text and are shown verbatim -
// HP's own client registers exactly "Save as PDF" and "Save as JPEG", read
// back off the printer from its own subscription. Two is deliberate - one per
// output format - where the panel would allow up to escl.MaxShortcuts.
func esclShortcuts() []string {
	return []string{"Save as PDF", "Save as JPEG"}
}

// esclSetup reads the scanner's capabilities and puts this computer on the
// printer's panel.
func (d *Daemon) esclSetup(ctx context.Context) error {
	caps, err := d.escl.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("escl capabilities: %w", err)
	}
	d.ecaps = caps
	return d.esclRegister(ctx)
}

func (d *Daemon) esclRegister(ctx context.Context) error {
	name := d.cfg.Name
	if len(name) > escl.MaxShortcutNameLen {
		// The panel truncates silently; better to be the one who decided.
		name = name[:escl.MaxShortcutNameLen]
	}
	sub, err := d.escl.Subscribe(ctx, name, esclShortcuts())
	if err != nil {
		return fmt.Errorf("register as a scan destination: %w", err)
	}
	d.esubMu.Lock()
	d.esub = sub
	d.esubMu.Unlock()
	slog.Info("daemon: ready, select this computer on the printer", "printer", d.cfg.Printer,
		"name", name, "interface", "eSCL", "model", d.ecaps.MakeAndModel,
		"adf", d.ecaps.HasAdf(), "duplex", d.ecaps.HasAdfDuplex())
	return nil
}

func (d *Daemon) esclTeardown() {
	d.esubMu.Lock()
	sub := d.esub
	d.esub = nil
	d.esubMu.Unlock()
	if sub != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := d.escl.Unsubscribe(ctx, sub); err != nil {
			slog.Debug("daemon: unregister failed", "error", err)
		}
		cancel()
	}
	if d.doc != nil {
		d.finishDocument()
	}
}

// esclLoop watches the subscription for walkup events until ctx ends.
//
// The poll is not only how events arrive: it is also what tells the printer
// this computer is still reachable. That is why it runs in its own goroutine
// rather than inline. Scanning a page takes ten seconds or more, and a handler
// that scanned inline would stop polling for the whole of it - exactly while
// the printer is waiting at the "another page or done?" prompt for a host that
// has gone silent. The panel then pauses and reports that the file could not
// be saved, even though the pages arrived and were written correctly.
func (d *Daemon) esclLoop(ctx context.Context) error {
	events := make(chan *escl.WalkupEvent, 8)
	pollErr := make(chan error, 1)

	pollCtx, stopPolling := context.WithCancel(ctx)
	defer stopPolling()
	go d.esclPoll(pollCtx, events, pollErr)

	const idleTick = time.Second
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-pollErr:
			return err
		case ev := <-events:
			if err := d.onESCLEvent(ctx, ev); err != nil {
				slog.Error("daemon: scan event handling failed", "error", err)
			}
		case <-time.After(idleTick):
			d.expireDocument()
		}
	}
}

// esclPoll reads events and never stops for anything the handler is doing.
//
// The cadence matters. HP's own client polls roughly every ten seconds; a
// tight loop makes this printer answer 503, and a 503 storm used to make the
// daemon unregister itself. Slow and steady is what the device expects.
func (d *Daemon) esclPoll(ctx context.Context, events chan<- *escl.WalkupEvent, fatal chan<- error) {
	const pollInterval = 5 * time.Second
	failures := 0
	for ctx.Err() == nil {
		ev, err := d.escl.NextEvent(ctx, d.subscription())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var se *escl.StatusError
			if errors.As(err, &se) && se.Status == 404 {
				// The printer forgot us: rebooted, or the subscription was
				// evicted to make room for another computer.
				slog.Warn("daemon: subscription vanished, registering again")
				if rerr := d.esclRegister(ctx); rerr != nil {
					select {
					case fatal <- rerr:
					default:
					}
					return
				}
				continue
			}
			failures++
			slog.Warn("daemon: event poll failed", "error", err, "consecutive", failures)
			if failures >= 5 {
				select {
				case fatal <- fmt.Errorf("event polling failed %d times: %w", failures, err):
				default:
				}
				return
			}
			d.sleep(ctx, 3*time.Second)
			continue
		}
		failures = 0
		if ev == nil {
			d.sleep(ctx, pollInterval)
			continue
		}
		select {
		case events <- ev:
		case <-ctx.Done():
			return
		}
	}
}

// subscriptionID pulls the UUID out of a subscription URI. That id is what a
// scan job quotes as its ContextID to say which walkup session it belongs to.
func subscriptionID(uri string) string {
	if i := strings.LastIndex(uri, "/"); i >= 0 {
		return uri[i+1:]
	}
	return uri
}

// subscription reads d.esub under the lock: the poller uses it while the
// handler may be replacing it after a re-registration.
func (d *Daemon) subscription() *escl.Subscription {
	d.esubMu.Lock()
	defer d.esubMu.Unlock()
	return d.esub
}

func (d *Daemon) onESCLEvent(ctx context.Context, ev *escl.WalkupEvent) error {
	slog.Info("daemon: walkup event", "type", string(ev.Type), "shortcut", ev.Shortcut)
	switch ev.Type {
	case escl.EventHostSelected:
		// The user is browsing the shortcut menu; nothing to do but keep
		// polling, which is what proves we are here.
		return nil
	case escl.EventScanRequested:
		// Every extra page the user adds at the panel arrives as another
		// ScanRequested, not as ScanNewPageRequested - the printer reuses the
		// same event and only ScanPagesComplete ends the document. Finishing
		// here would split a multi-page walkup scan into one file per page.
		return d.esclScan(ctx, ev)
	case escl.EventScanNewPageRequested:
		return d.esclScan(ctx, ev)
	case escl.EventScanPagesComplete:
		d.finishDocument()
		d.jpegPage = 0
		return nil
	default:
		slog.Warn("daemon: unknown walkup event type", "type", string(ev.Type))
		return nil
	}
}

// esclScan runs one scan and files the pages, from the feeder when it has
// paper and the flatbed otherwise.
func (d *Daemon) esclScan(ctx context.Context, ev *escl.WalkupEvent) error {
	wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	st, err := d.escl.WaitIdle(wctx)
	cancel()
	if err != nil {
		return err
	}

	// Only trust the feeder state when the scanner says it can detect paper;
	// otherwise a stale AdfState would send us to an empty feeder.
	useAdf := d.ecaps.HasAdf() && d.ecaps.DetectsPaperLoaded() && st.PaperLoaded()
	settings := d.esclSettings(useAdf)

	// What the user chose at the panel beats local configuration.
	settings = ev.Apply(settings)

	// PDFs are assembled here from JPEG pages, as on the LEDM path, so the
	// scanner is always asked for JPEG whatever the shortcut says. Doing it
	// this way keeps multi-page flatbed documents working: the printer would
	// otherwise hand back one self-contained PDF per page.
	outputPDF := d.formatFor(ev.Shortcut) == "pdf"
	settings.Format = escl.FormatJPEG

	// Naming the walkup session in the job is what makes the printer treat
	// this as the scan the user started at the panel. Without it the device
	// sees an unrelated pull scan, finishes after one page, and never offers
	// "another page or done?" from the glass. HP's own client sends the same
	// field, holding the subscription's UUID.
	if sub := d.subscription(); sub != nil {
		settings.ContextID = subscriptionID(sub.URI)
	}

	// The scan's purpose. HP's client always sends Document for scan-to-computer.
	settings.Intent = escl.IntentDocument

	// Identify ourselves, as HP's client does; the scanner advertises that it
	// wants this as JobSourceInfoSupport.
	settings.JobSource = escl.JobSourceInfo{
		UserName:    os.Getenv("USERNAME"),
		UserDomain:  os.Getenv("USERDOMAIN"),
		MachineName: d.cfg.Name,
		AppFileName: "hpscan.exe",
		Application: "hpscan",
	}

	pages, err := d.escl.ScanPages(ctx, settings)
	if err != nil {
		return err
	}
	// A feeder run is a complete document; the flatbed may be followed by
	// "scan another page".
	single := settings.Source == escl.SourceAdf

	if !outputPDF {
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

// esclSettings builds the scan settings from the configuration, clamped to
// what the chosen input source can actually do.
func (d *Daemon) esclSettings(useAdf bool) escl.ScanSettings {
	s := escl.ScanSettings{
		Resolution: d.cfg.Resolution,
		Color:      d.cfg.ColorMode == "color",
		Format:     escl.FormatJPEG,
		Source:     escl.SourcePlaten,
	}
	caps := d.ecaps.Platen
	if useAdf {
		s.Source = escl.SourceAdf
		caps = *d.ecaps.Adf
	}
	if strings.EqualFold(d.cfg.Paper, "letter") {
		s.Width, s.Height = escl.LetterWidth, escl.LetterHeight
	} else {
		s.Width, s.Height = escl.A4Width, escl.A4Height
	}
	if caps.MaxWidth > 0 && s.Width > caps.MaxWidth {
		s.Width = caps.MaxWidth
	}
	if caps.MaxHeight > 0 && s.Height > caps.MaxHeight {
		s.Height = caps.MaxHeight
	}
	if max := caps.MaxResolution(); max > 0 && s.Resolution > max {
		s.Resolution = max
	}
	slog.Debug("daemon: scan settings", "source", s.Source, "width", s.Width,
		"height", s.Height, "resolution", s.Resolution)
	return s
}
