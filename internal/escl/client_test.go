package escl

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestScanSettingsXML(t *testing.T) {
	s := ScanSettings{Resolution: 300, Width: A4Width, Height: A4Height, Color: true}
	got := s.xml()

	for _, want := range []string{
		`xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm"`,
		`xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03"`,
		`<scan:ScanRegions>`,
		`<pwg:Width>2481</pwg:Width>`,
		`<pwg:Height>3507</pwg:Height>`,
		`<pwg:ContentRegionUnits>escl:ThreeHundredthsOfInch</pwg:ContentRegionUnits>`,
		`<pwg:InputSource>Platen</pwg:InputSource>`,
		`<scan:ColorMode>RGB24</scan:ColorMode>`,
		`<scan:XResolution>300</scan:XResolution>`,
		`<scan:DocumentFormatExt>image/jpeg</scan:DocumentFormatExt>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ScanSettings.xml() missing %s\ngot: %s", want, got)
		}
	}
	if strings.Contains(got, "scan:Duplex") {
		t.Error("a flatbed scan must not mention Duplex at all: sending it, even as " +
			"false, makes the printer treat the job as one-shot")
	}
	if err := xml.Unmarshal([]byte(got), new(struct{})); err != nil {
		t.Errorf("ScanSettings.xml() is not well-formed XML: %v", err)
	}
}

func TestScanSettingsDuplexOnlyForFeeder(t *testing.T) {
	platen := ScanSettings{Resolution: 300, Source: SourcePlaten, Duplex: true}
	if strings.Contains(platen.xml(), "scan:Duplex") {
		t.Error("Duplex must be omitted for a platen scan: the flatbed has one side, " +
			"and HP's own client omits the element there")
	}
	feeder := ScanSettings{Resolution: 300, Source: SourceAdf, Duplex: true}
	if !strings.Contains(feeder.xml(), `<scan:Duplex>true</scan:Duplex>`) {
		t.Error("Duplex must be honoured for a feeder scan")
	}
}

func TestScanSettingsGrayscale(t *testing.T) {
	s := ScanSettings{Resolution: 200, Color: false}
	if !strings.Contains(s.xml(), `<scan:ColorMode>Grayscale8</scan:ColorMode>`) {
		t.Error("Color=false must request Grayscale8")
	}
}

// capsFixture is trimmed from an HP OfficeJet Pro 9120e.
const capsFixture = `<?xml version="1.0"?>
<scan:ScannerCapabilities xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm" xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03">
  <pwg:MakeAndModel>HP OfficeJet Pro 9120e Series</pwg:MakeAndModel>
  <pwg:SerialNumber>TH00XX00XX</pwg:SerialNumber>
  <scan:Platen>
    <scan:PlatenInputCaps>
      <scan:MinWidth>8</scan:MinWidth><scan:MaxWidth>2550</scan:MaxWidth>
      <scan:MinHeight>8</scan:MinHeight><scan:MaxHeight>3508</scan:MaxHeight>
      <scan:SettingProfiles><scan:SettingProfile>
        <scan:ColorModes><scan:ColorMode>Grayscale8</scan:ColorMode><scan:ColorMode>RGB24</scan:ColorMode></scan:ColorModes>
        <scan:DocumentFormats><scan:DocumentFormatExt>image/jpeg</scan:DocumentFormatExt><scan:DocumentFormatExt>application/pdf</scan:DocumentFormatExt></scan:DocumentFormats>
        <scan:SupportedResolutions><scan:DiscreteResolutions>
          <scan:DiscreteResolution><scan:XResolution>300</scan:XResolution><scan:YResolution>300</scan:YResolution></scan:DiscreteResolution>
          <scan:DiscreteResolution><scan:XResolution>1200</scan:XResolution><scan:YResolution>1200</scan:YResolution></scan:DiscreteResolution>
        </scan:DiscreteResolutions></scan:SupportedResolutions>
      </scan:SettingProfile></scan:SettingProfiles>
    </scan:PlatenInputCaps>
  </scan:Platen>
  <scan:Adf>
    <scan:AdfSimplexInputCaps><scan:MaxWidth>2550</scan:MaxWidth><scan:MaxHeight>4200</scan:MaxHeight></scan:AdfSimplexInputCaps>
    <scan:AdfDuplexInputCaps><scan:MaxWidth>2550</scan:MaxWidth><scan:MaxHeight>4200</scan:MaxHeight></scan:AdfDuplexInputCaps>
    <scan:AdfOptions><scan:AdfOption>DetectPaperLoaded</scan:AdfOption><scan:AdfOption>Duplex</scan:AdfOption></scan:AdfOptions>
  </scan:Adf>
</scan:ScannerCapabilities>`

func TestParseCapabilities(t *testing.T) {
	var caps Caps
	if err := xml.Unmarshal([]byte(capsFixture), &caps); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if caps.MakeAndModel != "HP OfficeJet Pro 9120e Series" {
		t.Errorf("MakeAndModel = %q", caps.MakeAndModel)
	}
	if caps.Platen.MaxWidth != 2550 || caps.Platen.MaxHeight != 3508 {
		t.Errorf("platen area = %dx%d", caps.Platen.MaxWidth, caps.Platen.MaxHeight)
	}
	if got := caps.Platen.MaxResolution(); got != 1200 {
		t.Errorf("MaxResolution = %d, want 1200", got)
	}
	if !caps.Platen.Supports(FormatPDF) {
		t.Error("platen should report PDF support")
	}
	if !caps.HasAdf() || !caps.HasAdfDuplex() {
		t.Errorf("adf = %v, duplex = %v, want both true", caps.HasAdf(), caps.HasAdfDuplex())
	}
	if !caps.DetectsPaperLoaded() {
		t.Error("DetectPaperLoaded should be reported")
	}
}

func TestStatusHelpers(t *testing.T) {
	var st Status
	body := `<scan:ScannerStatus xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03">
	  <pwg:State xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">Idle</pwg:State>
	  <scan:AdfState>ScannerAdfLoaded</scan:AdfState></scan:ScannerStatus>`
	if err := xml.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !st.Idle() {
		t.Errorf("State %q should be idle", st.State)
	}
	if !st.PaperLoaded() {
		t.Errorf("AdfState %q should mean paper loaded", st.AdfState)
	}
}

// TestScanPagesCollectsUntil404 drives the page loop against a stub printer:
// two pages, then the 404 that ends the job.
func TestScanPagesCollectsUntil404(t *testing.T) {
	var deleted bool
	pages := [][]byte{[]byte("page-one"), []byte("page-two")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/eSCL/ScanJobs":
			w.Header().Set("Location", "http://"+r.Host+"/eSCL/ScanJobs/1")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/eSCL/ScanJobs/1/NextDocument":
			if len(pages) == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(pages[0])
			pages = pages[1:]
		case r.Method == http.MethodDelete:
			deleted = true
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + Root, http: srv.Client()}
	// The feeder, because only the feeder delivers several pages from one job:
	// the flatbed stops after one so the panel can offer "another page or done?".
	got, err := c.ScanPages(context.Background(), ScanSettings{
		Resolution: 300, Width: A4Width, Height: A4Height, Source: SourceAdf,
	})
	if err != nil {
		t.Fatalf("ScanPages: %v", err)
	}
	if len(got) != 2 || string(got[0]) != "page-one" || string(got[1]) != "page-two" {
		t.Fatalf("got %d pages: %q", len(got), got)
	}
	// The deferred DELETE races with the assertion; give it a moment.
	time.Sleep(50 * time.Millisecond)
	if !deleted {
		t.Error("the job should be deleted once its pages are collected")
	}
}

// TestScanPagesRelativeJobLocation covers the firmware that answers the job
// POST with a server-absolute path instead of a full URL. BaseURL already ends
// in /eSCL, so a naive join gives /eSCL/eSCL/ScanJobs/... and every page fetch
// 404s while the scan appears to succeed.
func TestScanPagesRelativeJobLocation(t *testing.T) {
	var deletedPath string
	page := []byte("the-page")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/eSCL/ScanJobs":
			w.Header().Set("Location", "/eSCL/ScanJobs/rel-1") // no scheme or host
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/eSCL/ScanJobs/rel-1/NextDocument":
			if page == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(page)
			page = nil
		case r.Method == http.MethodDelete:
			deletedPath = r.URL.Path
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + Root, http: srv.Client()}
	// The feeder, so the job runs to 404 and the cleanup DELETE actually fires
	// - that DELETE is what proves the relative Location was resolved.
	pages, err := c.ScanPages(context.Background(), ScanSettings{
		Resolution: 300, Width: A4Width, Height: A4Height, Source: SourceAdf,
	})
	if err != nil {
		t.Fatalf("ScanPages with a relative Location: %v", err)
	}
	if len(pages) != 1 || string(pages[0]) != "the-page" {
		t.Fatalf("got %d pages: %q", len(pages), pages)
	}
	time.Sleep(50 * time.Millisecond)
	if deletedPath != "/eSCL/ScanJobs/rel-1" {
		t.Errorf("cleanup deleted %q, want the job itself", deletedPath)
	}
}

func TestFeederReadsUntilExhausted(t *testing.T) {
	left := 3
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/eSCL/ScanJobs":
			w.Header().Set("Location", "/eSCL/ScanJobs/f1")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/eSCL/ScanJobs/f1/NextDocument":
			if left == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			left--
			_, _ = w.Write([]byte("sheet"))
		case r.Method == http.MethodDelete:
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + Root, http: srv.Client()}
	pages, err := c.ScanPages(context.Background(), ScanSettings{
		Resolution: 300, Width: A4Width, Height: A4Height, Source: SourceAdf,
	})
	if err != nil {
		t.Fatalf("ScanPages: %v", err)
	}
	if len(pages) != 3 {
		t.Fatalf("got %d sheets from the feeder, want 3", len(pages))
	}
}

func TestScanPagesNoPageIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", "http://"+r.Host+"/eSCL/ScanJobs/1")
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + Root, http: srv.Client()}
	if _, err := c.ScanPages(context.Background(), ScanSettings{Resolution: 300}); err == nil {
		t.Fatal("a job that yields no page must be an error, not an empty success")
	}
}

// TestAgainstRealPrinter runs only when HPSCAN_TEST_PRINTER is set to a
// printer address, e.g.
//
//	HPSCAN_TEST_PRINTER=192.168.1.10 go test ./internal/escl -run Real -v
//
// It moves the scan head, so it is off by default.
func TestAgainstRealPrinter(t *testing.T) {
	host := os.Getenv("HPSCAN_TEST_PRINTER")
	if host == "" {
		t.Skip("set HPSCAN_TEST_PRINTER to run against a real printer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := New(host, 0)
	if !c.Probe(ctx) {
		t.Fatalf("%s does not answer on the eSCL interface", host)
	}
	caps, err := c.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	t.Logf("model %q, platen %dx%d, max %d dpi, adf=%v duplex=%v",
		caps.MakeAndModel, caps.Platen.MaxWidth, caps.Platen.MaxHeight,
		caps.Platen.MaxResolution(), caps.HasAdf(), caps.HasAdfDuplex())

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	t.Logf("status %q, adf %q", st.State, st.AdfState)

	// 150 dpi grayscale keeps the round trip short.
	page, err := c.ScanPage(ctx, ScanSettings{
		Resolution: 150, Width: A4Width, Height: A4Height, Format: FormatJPEG,
	})
	if err != nil {
		t.Fatalf("ScanPage: %v", err)
	}
	if len(page) < 1024 {
		t.Fatalf("page is only %d bytes", len(page))
	}
	if page[0] != 0xFF || page[1] != 0xD8 {
		t.Fatalf("page does not start with the JPEG marker: % x", page[:4])
	}
	t.Logf("scanned %d bytes of JPEG", len(page))
}

// The ContextID is what makes a walkup scan work: it names the subscription the
// job belongs to, so the printer treats the scan as the session the user
// started at the panel. Without it the device sees an unrelated pull scan,
// finishes after one page, and never offers "another page or done?" from the
// glass. This was read off HP's own client, which sends the subscription's
// UUID in exactly this position.
func TestScanSettingsCarriesWalkupContext(t *testing.T) {
	const id = "0731e685-011a-49fd-a0a1-952b59049add"
	s := ScanSettings{
		Resolution: 300, Width: A4Width, Height: A4Height, Color: true,
		ContextID: id, Intent: IntentDocument,
	}
	got := s.xml()

	if !strings.Contains(got, `<scan:ContextID>`+id+`</scan:ContextID>`) {
		t.Errorf("ScanSettings.xml() is missing the walkup ContextID\ngot: %s", got)
	}
	if !strings.Contains(got, `<scan:Intent>Document</scan:Intent>`) {
		t.Errorf("ScanSettings.xml() is missing the Intent\ngot: %s", got)
	}
	// HP puts ContextID between Version and Intent; keep that order.
	if i, j := strings.Index(got, "scan:ContextID"), strings.Index(got, "scan:Intent"); i < 0 || j < 0 || i > j {
		t.Errorf("ContextID must precede Intent, as HP's client sends it\ngot: %s", got)
	}
	if err := xml.Unmarshal([]byte(got), new(struct{})); err != nil {
		t.Errorf("ScanSettings.xml() is not well-formed XML: %v", err)
	}
}

// An ordinary scan is not part of any panel session, so it must not claim one.
func TestScanSettingsOmitsContextWhenNotWalkup(t *testing.T) {
	s := ScanSettings{Resolution: 300, Width: A4Width, Height: A4Height, Color: true}
	if got := s.xml(); strings.Contains(got, "ContextID") {
		t.Errorf("a non-walkup scan must not send a ContextID\ngot: %s", got)
	}
}
