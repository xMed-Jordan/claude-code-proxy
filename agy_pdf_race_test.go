package main

// agy_pdf_race_test.go — tests for the PDF render race fix (defect F2 found
// in review of v0.29.0, fixed in v0.29.1): renderPDFPages must render into a
// private temp dir and publish it to the shared, content-addressed pagesDir
// with a single atomic rename — never write into (or RemoveAll) pagesDir
// itself on any path. Uses the standard Go re-exec-the-test-binary idiom
// (TestHelperProcess + GO_WANT_HELPER_PROCESS — see the Go stdlib's own
// os/exec tests for the original pattern) via the gsCommand seam in
// agy_pdf.go, since pointing cfg.AgyGS at a script that stands in for `gs`
// isn't practical to write portably for Windows.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHelperProcess is not a real test. It only does anything when
// GO_WANT_HELPER_PROCESS=1 is set in its environment (never true for a plain
// `go test` run), in which case it plays "gs": it parses the same
// -sDEVICE=/-sOutputFile= argv shape gsPageRenderArgs/gsTextExtractArgs
// build and writes fake page/text output accordingly, or exits with a
// configured code, letting the tests below exercise renderPDFPages' real
// tmp-dir + atomic-rename + reuse logic without an actual Ghostscript
// install.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) > 0 {
		args = args[1:] // drop the "--" separator itself
	}

	if exitStr := os.Getenv("FAKE_GS_EXIT"); exitStr != "" {
		n, _ := strconv.Atoi(exitStr)
		os.Exit(n)
	}

	var device, outFile string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "-sDEVICE="):
			device = strings.TrimPrefix(a, "-sDEVICE=")
		case strings.HasPrefix(a, "-sOutputFile="):
			outFile = strings.TrimPrefix(a, "-sOutputFile=")
		}
	}

	pages := 1
	if n, err := strconv.Atoi(os.Getenv("FAKE_GS_PAGES")); err == nil && n > 0 {
		pages = n
	}
	var pageDelay time.Duration
	if n, err := strconv.Atoi(os.Getenv("FAKE_GS_PAGE_DELAY_MS")); err == nil && n > 0 {
		pageDelay = time.Duration(n) * time.Millisecond
	}

	switch device {
	case "png16m":
		// outFile is ".../page-%03d.png" — write into its directory. A
		// configured delay between pages (FAKE_GS_PAGE_DELAY_MS) widens the
		// window in which a concurrent renderer could observe a PARTIAL page
		// set — exactly the race M2's mutation control needs to expose: the
		// FIXED code never lets another goroutine see this directory at all
		// (it is a private tmp dir until the atomic rename), so the delay is
		// a no-op for correctness there, but is exactly what exposes a
		// content-based, direct-into-pagesDir reuse check reading a
		// half-written set.
		dir := filepath.Dir(outFile)
		for i := 1; i <= pages; i++ {
			if i > 1 && pageDelay > 0 {
				time.Sleep(pageDelay)
			}
			name := fmt.Sprintf("page-%03d.png", i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte("fake-png"), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
	case "txtwrite":
		if outFile != "" {
			if err := os.WriteFile(outFile, []byte(os.Getenv("FAKE_GS_TEXT")), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
	}
}

// fakeGSCommand replaces the package-level gsCommand seam for the duration
// of the calling test with one that re-execs this test binary as
// TestHelperProcess, restoring the original via t.Cleanup. When calls is
// non-nil it is incremented (atomically — tests (e) call this concurrently)
// once per invocation, so a test can assert whether "gs" was invoked at all.
func fakeGSCommand(t *testing.T, calls *int32, extraEnv ...string) {
	t.Helper()
	orig := gsCommand
	gsCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		cs := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], cs...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		cmd.Env = append(cmd.Env, extraEnv...)
		return cmd
	}
	t.Cleanup(func() { gsCommand = orig })
}

// (a) full render path works on this Windows box.
func TestRenderPDFPagesFullPathViaFakeGS(t *testing.T) {
	var calls int32
	fakeGSCommand(t, &calls, "FAKE_GS_PAGES=2", "FAKE_GS_TEXT=hello world")

	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "f-testsha")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(cacheDir, "0-test.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4 fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config{AgyPDFMaxPages: 10, AgyPDFDPI: 150}
	it := mediaItem{Path: pdfPath, Name: "test.pdf", Kind: "pdf"}

	pages, text, truncated, err := renderPDFPages(context.Background(), cfg, "fake-gs", it)
	if err != nil {
		t.Fatalf("renderPDFPages: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(pages))
	}
	if truncated {
		t.Fatal("2 pages < the configured max (10) must not be reported truncated")
	}
	if text != "hello world" {
		t.Fatalf("text = %q", text)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 gs invocations (page render + text extract), got %d", got)
	}

	pagesDir := filepath.Join(cacheDir, "pages")
	if fi, err := os.Stat(pagesDir); err != nil || !fi.IsDir() {
		t.Fatalf("pagesDir missing after a successful render: %v", err)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pages.tmp-") {
			t.Fatalf("leftover tmp dir after a successful render: %s", e.Name())
		}
	}
}

// (b) an existing complete pagesDir is reused and the fake gs is NOT invoked.
func TestRenderPDFPagesReusesExistingPagesDirWithoutInvokingGS(t *testing.T) {
	var calls int32
	fakeGSCommand(t, &calls, "FAKE_GS_EXIT=1") // would fail loudly if ever invoked

	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "f-testsha")
	pagesDir := filepath.Join(cacheDir, "pages")
	if err := os.MkdirAll(pagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pagesDir, "page-001.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pagesDir, "text.txt"), []byte("cached text"), 0o644); err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(cacheDir, "0-test.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config{AgyPDFMaxPages: 10, AgyPDFDPI: 150}
	it := mediaItem{Path: pdfPath, Name: "test.pdf", Kind: "pdf"}

	pages, text, _, err := renderPDFPages(context.Background(), cfg, "fake-gs", it)
	if err != nil {
		t.Fatalf("renderPDFPages: %v", err)
	}
	if len(pages) != 1 || text != "cached text" {
		t.Fatalf("expected the cached render to be reused, got pages=%+v text=%q", pages, text)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("gs must not be invoked when pagesDir already exists, got %d call(s)", got)
	}
}

// (c) a leftover pages.tmp-x with page files and no pagesDir is NOT reused
// (gs is invoked, and the leftover is left alone for the retention reaper).
func TestRenderPDFPagesIgnoresLeftoverTmpDir(t *testing.T) {
	var calls int32
	fakeGSCommand(t, &calls, "FAKE_GS_PAGES=1", "FAKE_GS_TEXT=fresh")

	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "f-testsha")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	leftoverTmp := filepath.Join(cacheDir, "pages.tmp-leftover")
	if err := os.MkdirAll(leftoverTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftoverTmp, "page-001.png"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(cacheDir, "0-test.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config{AgyPDFMaxPages: 10, AgyPDFDPI: 150}
	it := mediaItem{Path: pdfPath, Name: "test.pdf", Kind: "pdf"}

	pages, text, _, err := renderPDFPages(context.Background(), cfg, "fake-gs", it)
	if err != nil {
		t.Fatalf("renderPDFPages: %v", err)
	}
	if len(pages) != 1 || text != "fresh" {
		t.Fatalf("expected a fresh render, got pages=%+v text=%q", pages, text)
	}
	if got := atomic.LoadInt32(&calls); got == 0 {
		t.Fatal("gs must be invoked — a leftover pages.tmp-* dir must never count as a complete render")
	}
	if _, err := os.Stat(leftoverTmp); err != nil {
		t.Fatalf("the leftover tmp dir must be left alone (for the retention reaper), not touched: %v", err)
	}
}

// (d) a fake gs that exits 1 never creates pagesDir, leaks no tmp dir, and
// returns an error — which agyPDFViewPlan/agyMediaPrep turn into the
// fallback (today's default-agent path). A pre-existing pagesDir being left
// untouched by a failing render is covered by (b) above: when pagesDir
// already exists, gs is never even invoked, so there is nothing for a
// failure to corrupt in the first place.
func TestRenderPDFPagesGSFailureCreatesNoPagesDir(t *testing.T) {
	var calls int32
	fakeGSCommand(t, &calls, "FAKE_GS_EXIT=1")

	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "f-testsha")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(cacheDir, "0-test.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config{AgyPDFMaxPages: 10, AgyPDFDPI: 150}
	it := mediaItem{Path: pdfPath, Name: "test.pdf", Kind: "pdf"}

	_, _, _, err := renderPDFPages(context.Background(), cfg, "fake-gs", it)
	if err == nil {
		t.Fatal("expected an error when gs exits non-zero")
	}
	pagesDir := filepath.Join(cacheDir, "pages")
	if _, statErr := os.Stat(pagesDir); statErr == nil {
		t.Fatal("pagesDir must not be created when rendering fails")
	}
	entries, rerr := os.ReadDir(cacheDir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pages.tmp-") {
			t.Fatalf("tmp dir leaked after a failed render: %s", e.Name())
		}
	}
	if got := atomic.LoadInt32(&calls); got == 0 {
		t.Fatal("expected gs to actually have been invoked")
	}
}

// TestAgyMediaPrepPDFRenderFailureViaFakeGSFallsBack shows the failure in
// (d) actually reaching agyMediaPrep's caller as the documented fallback:
// nil agent args and today's default-agent prompt.
func TestAgyMediaPrepPDFRenderFailureViaFakeGSFallsBack(t *testing.T) {
	setVTRuntime("", false)
	var calls int32
	fakeGSCommand(t, &calls, "FAKE_GS_EXIT=1")

	dir := t.TempDir()
	cfg := config{
		AgyMedia: true, AgyMediaDir: dir, AgyMediaAgent: agyMediaViewAgentName,
		AgyGS:          "fake-gs", // seam-intercepted; the literal value is irrelevant
		AgyPDFMaxPages: 10, AgyPDFDPI: 150, AgyPDFTextMax: 20000,
	}
	parts := []mediaPart{{B64: base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 not a real pdf")), MediaType: "application/pdf", Filename: "doc.pdf"}}

	prompt, _, agentArgs, err := agyMediaPrep(context.Background(), cfg, "summarize", parts)
	if err != nil {
		t.Fatalf("agyMediaPrep: %v", err)
	}
	if agentArgs != nil {
		t.Fatalf("agentArgs = %v, want nil (the fallback) after a render failure", agentArgs)
	}
	if !strings.Contains(prompt, "python") {
		t.Fatalf("expected the legacy default-agent prompt:\n%s", prompt)
	}
	if got := atomic.LoadInt32(&calls); got == 0 {
		t.Fatal("expected gs to actually have been invoked before falling back")
	}
}

// (e) two goroutines rendering the same PDF concurrently both get the full
// page count (never a partial render) and exactly one pagesDir results — the
// core race fix.
// firstPageFileUnder reports whether any "page-*.png" file exists anywhere
// one level under root — inside "pages" (the fixed code's final dir) or a
// "pages.tmp-*" dir (a render in progress), whichever the code under test
// happens to use. Used to deterministically detect "a render has produced
// at least its first page" without depending on wall-clock timing.
func firstPageFileUnder(root string) bool {
	subdirs, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, sd := range subdirs {
		if !sd.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, sd.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.IsDir() && strings.HasPrefix(f.Name(), "page-") && strings.HasSuffix(f.Name(), ".png") {
				return true
			}
		}
	}
	return false
}

func TestRenderPDFPagesConcurrentRaceProducesOnePagesDir(t *testing.T) {
	var calls int32
	// A generous per-page write delay keeps a render "in flight" long enough
	// that the deterministic handoff below (poll for the first page file,
	// THEN release the rest) reliably lands the other goroutines' initial
	// reuse-checks while goroutine 0 is still mid-render — exactly the race
	// window M2's mutation control needs to expose (see the v0.29.1 report).
	fakeGSCommand(t, &calls, "FAKE_GS_PAGES=4", "FAKE_GS_TEXT=race", "FAKE_GS_PAGE_DELAY_MS=200")

	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "f-testsha")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(cacheDir, "0-test.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config{AgyPDFMaxPages: 10, AgyPDFDPI: 150}
	it := mediaItem{Path: pdfPath, Name: "test.pdf", Kind: "pdf"}

	const n = 6
	type result struct {
		pages []mediaItem
		err   error
	}
	results := make([]result, n)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		pages, _, _, err := renderPDFPages(context.Background(), cfg, "fake-gs", it)
		results[0] = result{pages: pages, err: err}
	}()

	// Deterministically wait for goroutine 0's render to have produced its
	// first page file before releasing the rest — a real, observed
	// filesystem event, not a guessed sleep duration. With a 200ms delay
	// between the (four) fake pages, goroutine 0 still has ~600ms of work
	// left when this returns, giving the other goroutines' initial
	// reuse-checks a wide, reliable window to land mid-render.
	deadline := time.Now().Add(10 * time.Second)
	for !firstPageFileUnder(cacheDir) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the first goroutine's first page file to appear")
		}
		time.Sleep(2 * time.Millisecond)
	}

	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pages, _, _, err := renderPDFPages(context.Background(), cfg, "fake-gs", it)
			results[i] = result{pages: pages, err: err}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("goroutine %d: renderPDFPages error: %v", i, r.err)
		}
		if len(r.pages) != 4 {
			t.Fatalf("goroutine %d: got %d pages, want 4 (the full count — no goroutine may see a partial render)", i, len(r.pages))
		}
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	pagesDirs, tmpDirs := 0, 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		switch {
		case e.Name() == "pages":
			pagesDirs++
		case strings.HasPrefix(e.Name(), "pages.tmp-"):
			tmpDirs++
		}
	}
	if pagesDirs != 1 {
		t.Fatalf("expected exactly one pages dir, found %d", pagesDirs)
	}
	if tmpDirs != 0 {
		t.Fatalf("expected no leftover tmp dirs once every goroutine has finished, found %d", tmpDirs)
	}
}
