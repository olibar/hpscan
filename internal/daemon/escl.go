package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// here and in HP's own client. Two is deliberate - one per output format -
// where the panel would allow up to escl.MaxShortcuts.
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
	d.esub = sub
	slog.Info("daemon: ready, select this computer on the printer", "printer", d.cfg.Printer,
		"name", name, "interface", "eSCL", "model", d.ecaps.MakeAndModel,
		"adf", d.ecaps.HasAdf(), "duplex", d.ecaps.HasAdfDuplex())
	return nil
}

func (d *Daemon) esclTeardown() {
	if d.esub != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := d.escl.Unsubscribe(ctx, d.esub); err != nil {
			slog.Debug("daemon: unregister failed", "error", err)
		}
		cancel()
		d.esub = nil
	}
	if d.doc != nil {
		d.finishDocument()
	}
}

// esclLoop polls the subscription for walkup events until ctx ends.
//
// The poll is not only how events arrive: it is also what tells the printer
// this computer is still reachable. Stop polling and the panel reports the
// computer as unavailable, so there is no "idle" state in which it is safe to
// back off for long.
func (d *Daemon) esclLoop(ctx context.Context) error {
	const pollInterval = 700 * time.Millisecond
	failures := 0
	for ctx.Err() == nil {
		ev, err := d.escl.NextEvent(ctx, d.esub)
		if err != nil {
			var se *escl.StatusError
			if errors.As(err, &se) && se.Status == 404 {
				// The printer forgot us: rebooted, or the subscription was
				// evicted to make room for another computer.
				slog.Warn("daemon: subscription vanished, registering again")
				if rerr := d.esclRegister(ctx); rerr != nil {
					return rerr
				}
				continue
			}
			failures++
			slog.Warn("daemon: event poll failed", "error", err, "consecutive", failures)
			if failures >= 5 {
				return fmt.Errorf("event polling failed %d times: %w", failures, err)
			}
			d.sleep(ctx, 3*time.Second)
			continue
		}
		failures = 0

		if ev == nil {
			d.expireDocument()
			d.sleep(ctx, pollInterval)
			continue
		}
		if err := d.onESCLEvent(ctx, ev); err != nil {
			slog.Error("daemon: scan event handling failed", "error", err)
		}
	}
	return nil
}

func (d *Daemon) onESCLEvent(ctx context.Context, ev *escl.WalkupEvent) error {
	slog.Info("daemon: walkup event", "type", string(ev.Type), "shortcut", ev.Shortcut)
	switch ev.Type {
	case escl.EventHostSelected:
		// The user is browsing the shortcut menu; nothing to do but keep
		// polling, which is what proves we are here.
		return nil
	case escl.EventScanRequested:
		if d.doc != nil {
			d.finishDocument()
		}
		d.jpegPage = 0
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
