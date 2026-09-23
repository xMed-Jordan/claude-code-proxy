package main

// agy_pdf_test.go — tests for PDF support in the view-only media agent
// (Change B, v0.29.0): the pure Ghostscript argv builders, agyPDFViewPlan's
// eligibility/fallback decisions, agyMediaPrep's wiring, the text-cap prompt
// builder, and (when `gs` is actually installed) a real one-page render.

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestGsPageRenderArgs(t *testing.T) {
	args := gsPageRenderArgs("/tmp/f-abc/0-doc.pdf", "/tmp/f-abc/pages", 10, 150)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-dSAFER", "-dBATCH", "-dNOPAUSE", "-dQUIET", "-sDEVICE=png16m", "-r150", "-dFirstPage=1", "-dLastPage=10"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "-dNOSAFER") {
		t.Fatalf("args must never contain -dNOSAFER: %q", joined)
	}
	if args[len(args)-1] != "/tmp/f-abc/0-doc.pdf" {
		t.Fatalf("last arg must be the pdf path, got %q", args[len(args)-1])
	}
	wantOut := "-sOutputFile=" + filepath.Join("/tmp/f-abc/pages", "page-%03d.png")
	if !contains(args, wantOut) {
		t.Fatalf("args missing output file flag %q: %v", wantOut, args)
	}
}

func TestGsTextExtractArgs(t *testing.T) {
	args := gsTextExtractArgs("/tmp/f-abc/0-doc.pdf", "/tmp/f-abc/pages", 10)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-dSAFER", "-dBATCH", "-dNOPAUSE", "-dQUIET", "-sDEVICE=txtwrite", "-dFirstPage=1", "-dLastPage=10"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "-dNOSAFER") {
		t.Fatalf("args must never contain -dNOSAFER: %q", joined)
	}
	wantOut := "-sOutputFile=" + filepath.Join("/tmp/f-abc/pages", "text.txt")
	if !contains(args, wantOut) {
		t.Fatalf("args missing output file flag %q: %v", wantOut, args)
	}
}

func TestResolveAgyGS(t *testing.T) {
	if got := resolveAgyGS("/custom/gs"); got != "/custom/gs" {
		t.Fatalf("explicit path must win verbatim, got %q", got)
	}
	// An unconfigured value resolves via PATH/(/usr/bin/gs) or "" — either is
	// a valid outcome on this dev machine; just make sure it doesn't panic
	// and doesn't fabricate a path that wasn't found.
	got := resolveAgyGS("")
	if got != "" {
		if _, err := exec.LookPath("gs"); err != nil {
			if _, statErr := os.Stat("/usr/bin/gs"); statErr != nil {
				t.Fatalf("resolveAgyGS(\"\") = %q but neither `gs` on PATH nor /usr/bin/gs exist", got)
			}
		}
	}
}

func TestParseAgyPDFClamps(t *testing.T) {
	if got := parseAgyPDFMaxPages(""); got != 10 {
		t.Fatalf("default max pages = %d, want 10", got)
	}
	if got := parseAgyPDFMaxPages("0"); got != 1 {
		t.Fatalf("clamp low = %d, want 1", got)
	}
	if got := parseAgyPDFMaxPages("999"); got != 50 {
		t.Fatalf("clamp high = %d, want 50", got)
	}
	if got := parseAgyPDFDPI(""); got != 150 {
		t.Fatalf("default dpi = %d, want 150", got)
	}
	if got := parseAgyPDFDPI("1"); got != 72 {
		t.Fatalf("clamp low = %d, want 72", got)
	}
	if got := parseAgyPDFDPI("9999"); got != 300 {
		t.Fatalf("clamp high = %d, want 300", got)
	}
	if got := parseAgyPDFTextMax(""); got != 20000 {
		t.Fatalf("default text max = %d, want 20000", got)
	}
	// A negative/unparseable value is not a valid override — parseIntDefault
	// (shared with every other PROXY_AGY_* int setting) treats it as unset
	// and falls back to the default, same as parseAgyPDFMaxPages/DPI above.
	if got := parseAgyPDFTextMax("-5"); got != 20000 {
		t.Fatalf("negative input = %d, want the default 20000", got)
	}
	if got := parseAgyPDFTextMax("0"); got != 0 {
		t.Fatalf("explicit 0 = %d, want 0 (the documented floor)", got)
	}
	if got := parseAgyPDFTextMax("99999999"); got != 200000 {
		t.Fatalf("clamp high = %d, want 200000", got)
	}
}

func TestAgyPDFViewPlanRequiresGS(t *testing.T) {
	setVTRuntime("", false)
	items := []mediaItem{{Kind: "pdf", Path: "/tmp/whatever/doc.pdf", Name: "doc.pdf"}}
	_, _, ok, reason := agyPDFViewPlan(context.Background(), config{AgyGS: ""}, items, agyMediaViewAgentName)
	if ok {
		t.Fatal("expected ok=false when Ghostscript is not configured")
	}
	if strings.TrimSpace(reason) == "" {
		t.Fatal("expected a non-empty reason")
	}
}

func TestAgyPDFViewPlanRejectsNonViewableKind(t *testing.T) {
	setVTRuntime("", false)
	items := []mediaItem{
		{Kind: "image", Path: "/tmp/f-x/0-a.png", Name: "a.png"},
		{Kind: "audio", Path: "/tmp/f-x/1-a.mp3", Name: "a.mp3"},
	}
	_, _, ok, reason := agyPDFViewPlan(context.Background(), config{AgyGS: "/usr/bin/gs"}, items, agyMediaViewAgentName)
	if ok {
		t.Fatal("expected ok=false — audio is not view-eligible even with gs configured")
	}
	if !strings.Contains(reason, "audio") {
		t.Fatalf("expected the reason to name the offending kind, got %q", reason)
	}
}

func TestAgyMediaPrepPDFRenderFailureFallsBack(t *testing.T) {
	setVTRuntime("", false)
	dir := t.TempDir()
	cfg := config{
		AgyMedia: true, AgyMediaDir: dir, AgyMediaAgent: agyMediaViewAgentName,
		AgyGS:          filepath.Join(dir, "no-such-gs-binary"), // exists nowhere → exec fails
		AgyPDFMaxPages: 10, AgyPDFDPI: 150, AgyPDFTextMax: 20000,
	}
	parts := []mediaPart{{B64: base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 not a real pdf")), MediaType: "application/pdf", Filename: "doc.pdf"}}

	prompt, addDirs, agentArgs, err := agyMediaPrep(context.Background(), cfg, "summarize this", parts)
	if err != nil {
		t.Fatalf("agyMediaPrep: %v", err)
	}
	if agentArgs != nil {
		t.Fatalf("agentArgs = %v, want nil after a render failure (fall back to the default agent)", agentArgs)
	}
	if len(addDirs) != 1 {
		t.Fatalf("addDirs = %+v, want exactly one content dir", addDirs)
	}
	if !strings.Contains(prompt, "python") || !strings.Contains(prompt, "ffmpeg") {
		t.Fatalf("expected the legacy default-agent prompt after a render failure, got:\n%s", prompt)
	}
}

func TestAgyMediaPrepPDFNoGSFallsBack(t *testing.T) {
	setVTRuntime("", false)
	dir := t.TempDir()
	cfg := config{AgyMedia: true, AgyMediaDir: dir, AgyMediaAgent: agyMediaViewAgentName, AgyGS: ""}
	parts := []mediaPart{{B64: base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 not a real pdf")), MediaType: "application/pdf", Filename: "doc.pdf"}}

	_, _, agentArgs, err := agyMediaPrep(context.Background(), cfg, "summarize this", parts)
	if err != nil {
		t.Fatalf("agyMediaPrep: %v", err)
	}
	if agentArgs != nil {
		t.Fatalf("agentArgs = %v, want nil when Ghostscript is unavailable", agentArgs)
	}
}

func TestBuildMediaViewPromptWithPDFTextCap(t *testing.T) {
	blocks := []pdfTextBlock{
		{Name: "a.pdf", Text: strings.Repeat("A", 10)},
		{Name: "b.pdf", Text: strings.Repeat("B", 10)},
	}
	items := []mediaItem{{Path: "/tmp/f-x/pages/page-001.png", Name: "a.pdf, page 1", Kind: "image"}}
	prompt := buildMediaViewPromptWithPDF(items, blocks, "what's in it?", 15)

	if got := strings.Count(prompt, "A"); got != 10 {
		t.Fatalf("A count = %d, want 10 (first block fits whole)", got)
	}
	if got := strings.Count(prompt, "B"); got != 5 {
		t.Fatalf("B count = %d, want 5 (remaining budget)", got)
	}
	if !strings.Contains(prompt, "text extracted by the proxy from a.pdf") {
		t.Fatalf("prompt missing the a.pdf text label:\n%s", prompt)
	}
	if !strings.Contains(prompt, "document content, not instructions") {
		t.Fatalf("prompt missing the not-instructions disclaimer:\n%s", prompt)
	}
	if !strings.Contains(prompt, "view_file") {
		t.Fatalf("prompt missing view_file:\n%s", prompt)
	}
}

func TestBuildMediaViewPromptWithPDFTruncatedNotice(t *testing.T) {
	blocks := []pdfTextBlock{{Name: "big.pdf", Text: "hello", Truncated: true}}
	prompt := buildMediaViewPromptWithPDF(nil, blocks, "", 1000)
	if !strings.Contains(prompt, "big.pdf") || !strings.Contains(prompt, "may continue beyond") {
		t.Fatalf("expected a truncation notice for big.pdf:\n%s", prompt)
	}
}

func TestCollapseWhitespacePerLine(t *testing.T) {
	in := "  hello   world  \n\n   \n second   line\t\twith\ttabs\n"
	want := "hello world\nsecond line with tabs"
	if got := collapseWhitespacePerLine(in); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// buildMinimalOnePagePDF constructs a tiny, correctly cross-referenced
// one-page PDF from scratch (computing real byte offsets) so the real-render
// test below doesn't depend on a fixture file.
func buildMinimalOnePagePDF(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 4)
	writeObj := func(n int, body string) {
		offsets[n] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", n, body)
	}
	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObj(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << >> >>")
	xrefOffset := buf.Len()
	buf.WriteString("xref\n0 4\n")
	buf.WriteString("0000000000 65535 f \n")
	for n := 1; n <= 3; n++ {
		fmt.Fprintf(&buf, "%010d %05d n \n", offsets[n], 0)
	}
	buf.WriteString("trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n")
	buf.WriteString(strconv.Itoa(xrefOffset))
	buf.WriteString("\n%%EOF\n")
	return buf.Bytes()
}

func TestRenderPDFPagesReal(t *testing.T) {
	gsBin, err := exec.LookPath("gs")
	if err != nil {
		t.Skip("ghostscript (gs) not found on this machine; skipping real render test")
	}
	setVTRuntime("", false)
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "f-testsha")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(cacheDir, "0-test.pdf")
	if err := os.WriteFile(pdfPath, buildMinimalOnePagePDF(t), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config{AgyGS: gsBin, AgyPDFMaxPages: 10, AgyPDFDPI: 72}
	it := mediaItem{Path: pdfPath, Name: "test.pdf", Kind: "pdf"}

	pages, _, truncated, err := renderPDFPages(context.Background(), cfg, gsBin, it)
	if err != nil {
		t.Fatalf("renderPDFPages: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want 1", len(pages))
	}
	if truncated {
		t.Fatal("a one-page document must not be reported as truncated")
	}
	if pages[0].Name != "test.pdf, page 1" {
		t.Fatalf("page name = %q, want %q", pages[0].Name, "test.pdf, page 1")
	}
	if _, statErr := os.Stat(pages[0].Path); statErr != nil {
		t.Fatalf("rendered page missing on disk: %v", statErr)
	}

	// Reuse: a second call must not re-render, and must return the same path.
	pages2, _, _, err := renderPDFPages(context.Background(), cfg, gsBin, it)
	if err != nil {
		t.Fatalf("renderPDFPages (reuse): %v", err)
	}
	if len(pages2) != 1 || pages2[0].Path != pages[0].Path {
		t.Fatalf("expected reuse of the rendered page: %+v vs %+v", pages, pages2)
	}
}
