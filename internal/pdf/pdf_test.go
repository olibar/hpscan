package pdf

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"
)

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{255, 0, 0, 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWriteTwoPages(t *testing.T) {
	var out bytes.Buffer
	err := Write(&out, []Page{{JPEG: testJPEG(t, 300, 600), DPI: 300}, {JPEG: testJPEG(t, 150, 150), DPI: 150}})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.HasPrefix(s, "%PDF-1.4") || !strings.HasSuffix(s, "%%EOF\n") {
		t.Fatalf("bad envelope: %q ... %q", s[:8], s[len(s)-6:])
	}
	if !strings.Contains(s, "/Count 2") || !strings.Contains(s, "/Kids [3 0 R 6 0 R ]") {
		t.Fatalf("page tree wrong: %s", firstKB(s))
	}
	// 300x600 at 300dpi -> 72x144 pt; 150x150 at 150dpi -> 72x72 pt.
	if !strings.Contains(s, "/MediaBox [0 0 72.00 144.00]") || !strings.Contains(s, "/MediaBox [0 0 72.00 72.00]") {
		t.Fatalf("media boxes wrong: %s", firstKB(s))
	}
	if strings.Count(s, "/DCTDecode") != 2 || !strings.Contains(s, "xref\n0 9\n") {
		t.Fatalf("objects wrong: %s", firstKB(s))
	}
}

func TestWriteEmpty(t *testing.T) {
	if err := Write(&bytes.Buffer{}, nil); err == nil {
		t.Fatal("expected error for no pages")
	}
}

func firstKB(s string) string {
	if len(s) > 1024 {
		return s[:1024]
	}
	return s
}
