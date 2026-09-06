// Package pdf assembles JPEG page images into a minimal PDF file without
// external dependencies. Each JPEG is embedded as-is using DCTDecode.
package pdf

import (
	"bytes"
	"fmt"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
)

// Page is one scanned page: raw JPEG bytes and the DPI it was scanned at.
type Page struct {
	JPEG []byte
	DPI  int
}

// Write emits a PDF containing one page per JPEG, sized so the image is
// printed at its native DPI.
func Write(w io.Writer, pages []Page) error {
	slog.Debug("pdf: writing", "pages", len(pages))
	if len(pages) == 0 {
		return fmt.Errorf("no pages to write")
	}
	var buf bytes.Buffer
	var offsets []int
	obj := func(body func()) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n", len(offsets))
		body()
		buf.WriteString("\nendobj\n")
	}
	buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	// Object numbering: 1 catalog, 2 pages, then per page: page, image, content.
	obj(func() { buf.WriteString("<< /Type /Catalog /Pages 2 0 R >>") })
	kids := ""
	for i := range pages {
		kids += fmt.Sprintf("%d 0 R ", 3+i*3)
	}
	obj(func() {
		fmt.Fprintf(&buf, "<< /Type /Pages /Kids [%s] /Count %d >>", kids, len(pages))
	})
	for i, p := range pages {
		if err := writePage(&buf, obj, p, 3+i*3); err != nil {
			return fmt.Errorf("page %d: %w", i+1, err)
		}
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	n, err := w.Write(buf.Bytes())
	if err != nil {
		return fmt.Errorf("write pdf: %w", err)
	}
	slog.Debug("pdf: written", "bytes", n)
	return nil
}

func writePage(buf *bytes.Buffer, obj func(func()), p Page, pageObj int) error {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(p.JPEG))
	if err != nil {
		return fmt.Errorf("decode jpeg header: %w", err)
	}
	dpi := p.DPI
	if dpi <= 0 {
		dpi = 300
	}
	wPt := float64(cfg.Width) * 72 / float64(dpi)
	hPt := float64(cfg.Height) * 72 / float64(dpi)
	cs := "/DeviceRGB"
	if cfg.ColorModel == color.GrayModel || cfg.ColorModel == color.Gray16Model {
		cs = "/DeviceGray"
	}
	slog.Debug("pdf: page", "width_px", cfg.Width, "height_px", cfg.Height, "dpi", dpi, "colorspace", cs)
	imgObj := pageObj + 1
	obj(func() {
		fmt.Fprintf(buf, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] "+
			"/Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>", wPt, hPt, imgObj, imgObj+1)
	})
	obj(func() {
		fmt.Fprintf(buf, "<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s "+
			"/BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n", cfg.Width, cfg.Height, cs, len(p.JPEG))
		buf.Write(p.JPEG)
		buf.WriteString("\nendstream")
	})
	content := fmt.Sprintf("q %.2f 0 0 %.2f 0 0 cm /Im0 Do Q", wPt, hPt)
	obj(func() {
		fmt.Fprintf(buf, "<< /Length %d >>\nstream\n%s\nendstream", len(content), content)
	})
	return nil
}
