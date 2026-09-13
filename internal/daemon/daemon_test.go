package daemon

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olibar/hpscan/internal/config"
)

// fakePrinter emulates the LEDM endpoints of a WalkupScanToComp printer.
type fakePrinter struct {
	mu         sync.Mutex
	registered bool
	destURI    string
	eventType  string // current WalkupScanToCompEvent type
	aging      int
	jobPolls   int
	scans      int
	jpeg       []byte
	adfLoaded  bool // simulate a document feeder with two sheets
	polled     bool // the daemon has read the event table at least once
}

func (p *fakePrinter) fire(typ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.eventType = typ
	p.aging++
}

func (p *fakePrinter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/DevMgmt/DiscoveryTree.xml", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<ledm:DiscoveryTree xmlns:ledm="x" xmlns:dd="y">
<ledm:SupportedIfc><dd:ResourceType>ledm:hpLedmWalkupScanToCompManifest</dd:ResourceType><ledm:ManifestURI>/WalkupScanToComp/WalkupScanToCompManifest.xml</ledm:ManifestURI></ledm:SupportedIfc>
<ledm:SupportedIfc><ledm:ManifestURI>/EventMgmt/EventMgmtManifest.xml</ledm:ManifestURI></ledm:SupportedIfc>
<ledm:SupportedIfc><ledm:ManifestURI>/Scan/ScanManifest.xml</ledm:ManifestURI></ledm:SupportedIfc>
</ledm:DiscoveryTree>`)
	})
	mux.HandleFunc("/Scan/ScanCaps", func(w http.ResponseWriter, r *http.Request) {
		adf := ""
		if p.adfLoaded {
			adf = `<Adf><InputSourceCaps><MaxWidth>2550</MaxWidth><MaxHeight>4200</MaxHeight><MaxOpticalXResolution>300</MaxOpticalXResolution></InputSourceCaps></Adf>`
		}
		io.WriteString(w, `<ScanCaps><Platen><InputSourceCaps><MaxWidth>2550</MaxWidth><MaxHeight>3508</MaxHeight><MaxResolution>1200</MaxResolution></InputSourceCaps></Platen>`+adf+`</ScanCaps>`)
	})
	mux.HandleFunc("/Scan/Status", func(w http.ResponseWriter, r *http.Request) {
		state := "Empty"
		if p.adfLoaded {
			state = "Loaded"
		}
		io.WriteString(w, `<ScanStatus><ScannerState>Idle</ScannerState><AdfState>`+state+`</AdfState></ScanStatus>`)
	})
	mux.HandleFunc("/WalkupScanToComp/WalkupScanToCompDestinations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "<dd:Name>Test Mac</dd:Name>") {
			http.Error(w, "bad body: "+string(body), 400)
			return
		}
		p.mu.Lock()
		p.registered = true
		p.destURI = "/WalkupScanToComp/WalkupScanToCompDestinations/abc-123"
		p.mu.Unlock()
		w.Header().Set("Location", p.destURI)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/WalkupScanToComp/WalkupScanToCompDestinations/abc-123", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		io.WriteString(w, `<wus:WalkupScanToCompDestination xmlns:wus="x" xmlns:dd="y"><dd:Name>Test Mac</dd:Name><dd:ResourceURI>/WalkupScanToComp/WalkupScanToCompDestinations/abc-123</dd:ResourceURI><wus:WalkupScanToCompSettings><wus:Shortcut>SavePDF</wus:Shortcut></wus:WalkupScanToCompSettings></wus:WalkupScanToCompDestination>`)
	})
	mux.HandleFunc("/WalkupScanToComp/WalkupScanToCompEvent", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		fmt.Fprintf(w, `<wus:WalkupScanToCompEvent xmlns:wus="x"><wus:WalkupScanToCompEventType>%s</wus:WalkupScanToCompEventType></wus:WalkupScanToCompEvent>`, p.eventType)
	})
	mux.HandleFunc("/EventMgmt/EventTable", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		aging := p.aging
		p.polled = true
		p.mu.Unlock()
		etag := strconv.Itoa(aging)
		if r.Header.Get("If-None-Match") == etag {
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		fmt.Fprintf(w, `<ev:EventTable xmlns:ev="x" xmlns:dd="y"><ev:Event><dd:UnqualifiedEventCategory>ScanEvent</dd:UnqualifiedEventCategory><dd:AgingStamp>%d-0</dd:AgingStamp><ev:Payload><dd:ResourceURI>/WalkupScanToComp/WalkupScanToCompDestinations/abc-123</dd:ResourceURI><dd:ResourceType>wus:WalkupScanToCompDestination</dd:ResourceType></ev:Payload><ev:Payload><dd:ResourceURI>/WalkupScanToComp/WalkupScanToCompEvent</dd:ResourceURI><dd:ResourceType>wus:WalkupScanToCompEvent</dd:ResourceType></ev:Payload></ev:Event></ev:EventTable>`, aging)
	})
	mux.HandleFunc("/Scan/Jobs", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		want := "<scan:InputSource>Platen</scan:InputSource>"
		if p.adfLoaded {
			want = "<scan:InputSource>Adf</scan:InputSource>"
		}
		if !strings.Contains(string(body), want) {
			http.Error(w, "bad scan job: "+string(body), 400)
			return
		}
		p.mu.Lock()
		p.scans++
		p.jobPolls = 0
		p.mu.Unlock()
		w.Header().Set("Location", "/Jobs/JobList/7")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/Jobs/JobList/7", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.jobPolls++
		n := p.jobPolls
		p.mu.Unlock()
		if p.adfLoaded {
			p.adfJob(w, n)
			return
		}
		switch {
		case n == 1:
			io.WriteString(w, `<j:Job xmlns:j="x"><j:JobState>Processing</j:JobState><ScanJob><PreScanPage><PageNumber>1</PageNumber><PageState>PreparingScan</PageState></PreScanPage></ScanJob></j:Job>`)
		case n == 2:
			io.WriteString(w, `<j:Job xmlns:j="x"><j:JobState>Processing</j:JobState><ScanJob><PreScanPage><PageNumber>1</PageNumber><PageState>ReadyToUpload</PageState><BinaryURL>/Scan/Jobs/7/Pages/1</BinaryURL></PreScanPage></ScanJob></j:Job>`)
		default:
			io.WriteString(w, `<j:Job xmlns:j="x"><j:JobState>Completed</j:JobState><ScanJob><PostScanPage><PageNumber>1</PageNumber><PageState>UploadCompleted</PageState></PostScanPage></ScanJob></j:Job>`)
		}
	})
	mux.HandleFunc("/Scan/Jobs/7/Pages/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(p.jpeg)
	})
	return mux
}

// adfJob emits a two-sheet feeder job: page 1 ready, then page 2, then done.
func (p *fakePrinter) adfJob(w http.ResponseWriter, n int) {
	page := func(num int, state string) string {
		return fmt.Sprintf(`<PreScanPage><PageNumber>%d</PageNumber><PageState>%s</PageState><BinaryURL>/Scan/Jobs/7/Pages/1</BinaryURL></PreScanPage>`, num, state)
	}
	switch {
	case n == 1:
		fmt.Fprintf(w, `<j:Job xmlns:j="x"><j:JobState>Processing</j:JobState><ScanJob>%s</ScanJob></j:Job>`, page(1, "ReadyToUpload"))
	case n == 2:
		fmt.Fprintf(w, `<j:Job xmlns:j="x"><j:JobState>Processing</j:JobState><ScanJob>%s%s</ScanJob></j:Job>`, page(1, "UploadCompleted"), page(2, "ReadyToUpload"))
	default:
		fmt.Fprintf(w, `<j:Job xmlns:j="x"><j:JobState>Completed</j:JobState><ScanJob>%s%s</ScanJob></j:Job>`, page(1, "UploadCompleted"), page(2, "UploadCompleted"))
	}
}

func TestAdfScanProducesMultiPagePDF(t *testing.T) {
	var img bytes.Buffer
	if err := jpeg.Encode(&img, image.NewGray(image.Rect(0, 0, 20, 30)), nil); err != nil {
		t.Fatal(err)
	}
	p := &fakePrinter{jpeg: img.Bytes(), eventType: "HostSelected", adfLoaded: true}
	srv := httptest.NewServer(p.handler())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	out := t.TempDir()
	cfg := config.Defaults()
	cfg.Printer, cfg.Port, cfg.Name, cfg.OutputDir, cfg.Filename = u.Hostname(), port, "Test Mac", out, "adf"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runOnce(ctx, cfg) }()
	// Wait for the baseline event-table poll as well as registration. The
	// daemon records the current aging stamps when its loop starts, so an
	// event fired before that is seen as old and never acted on.
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.registered && p.polled })
	p.fire("ScanRequested")
	// A feeder run is a complete document: no ScanPagesComplete needed.
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(out, "adf.pdf")); return err == nil })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(out, "adf.pdf"))
	if !strings.Contains(string(data), "/Count 2") {
		t.Fatalf("expected 2 pages from the feeder, got: %s", firstKB(data))
	}
}

func firstKB(b []byte) string {
	if len(b) > 1024 {
		b = b[:1024]
	}
	return string(b)
}

func TestWalkupScanToCompFlow(t *testing.T) {
	var img bytes.Buffer
	if err := jpeg.Encode(&img, image.NewGray(image.Rect(0, 0, 20, 30)), nil); err != nil {
		t.Fatal(err)
	}
	p := &fakePrinter{jpeg: img.Bytes(), eventType: "HostSelected"}
	srv := httptest.NewServer(p.handler())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	out := t.TempDir()
	cfg := config.Defaults()
	cfg.Printer, cfg.Port, cfg.Name, cfg.OutputDir = u.Hostname(), port, "Test Mac", out
	cfg.Filename = "doc"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runOnce(ctx, cfg) }()

	// Wait for the baseline event-table poll as well as registration. The
	// daemon records the current aging stamps when its loop starts, so an
	// event fired before that is seen as old and never acted on.
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.registered && p.polled })
	p.fire("ScanRequested")
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.scans == 1 })
	time.Sleep(300 * time.Millisecond)
	p.fire("ScanNewPageRequested")
	waitFor(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.scans == 2 })
	time.Sleep(300 * time.Millisecond)
	p.fire("ScanPagesComplete")
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(out, "doc.pdf")); return err == nil })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "doc.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "/Count 2") || bytes.Count(data, []byte("/DCTDecode")) != 2 {
		t.Fatalf("expected a 2-page pdf, got %d bytes", len(data))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
