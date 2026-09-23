package main

// estimate_media_test.go — tests for v0.31.0's Change 2: estimateContentChars
// stops pricing a media payload's base64 as if it were text.
//
// oldEstimate*RequestTokens / oldEstimateJSONTokens below are deliberate,
// frozen copies of the PRE-FIX formulas (contentToText + raw json.Marshal
// length, no media awareness) — used ONLY in these tests, to prove both
// "the old number really was huge" and "the new number is byte-identical
// to the old one when there is no media block".

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func oldEstimateAnthropicRequestTokens(in anthropicRequest) int {
	chars := len(contentToText(in.System)) + len(in.Model)
	for _, msg := range in.Messages {
		chars += len(contentToText(msg.Content))
	}
	return estimateTextTokensFromChars(chars)
}

func oldEstimateOpenAIChatRequestTokens(in openAIRequest) int {
	chars := len(in.Model)
	for _, msg := range in.Messages {
		chars += len(msg.Role) + len(msg.Name) + len(msg.ToolCallID) + len(contentToText(msg.Content))
		for _, call := range msg.ToolCalls {
			chars += len(call.ID) + len(call.Type) + len(call.Function.Name) + len(call.Function.Arguments)
		}
	}
	for _, tool := range in.Tools {
		chars += len(tool.Type) + len(tool.Function.Name) + len(tool.Function.Description) + len(contentToText(tool.Function.Parameters))
	}
	return estimateTextTokensFromChars(chars)
}

func oldEstimateResponsesRequestTokens(in responsesRequest) int {
	chars := len(in.Model) + len(in.Instructions) + len(in.PromptCacheKey)
	for _, item := range in.Input {
		chars += len(contentToText(item))
	}
	for _, tool := range in.Tools {
		chars += len(tool.Type) + len(tool.Name) + len(tool.Description) + len(contentToText(tool.Parameters))
	}
	return estimateTextTokensFromChars(chars)
}

func oldEstimateJSONTokens(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return estimateTextTokensFromChars(len(raw))
}

func oldEstimateCodexRequestTokens(in responsesRequest) int {
	return max(oldEstimateResponsesRequestTokens(in), oldEstimateJSONTokens(codexRequestBody(in)))
}

// ── text-only regression: byte-identical to the old estimate ───────────────

func TestEstimateAnthropicNoMediaMatchesOldEstimate(t *testing.T) {
	cases := []anthropicRequest{
		{Model: "agy", Messages: []anthropicMessage{{Role: "user", Content: "hello there, how are you?"}}},
		{Model: "agy", System: "you are a helpful clinic assistant", Messages: []anthropicMessage{
			{Role: "user", Content: "book me an appointment"},
			{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "sure, when works for you?"}}},
		}},
		{Model: "agy", Tools: []anthropicTool{{Name: "get_slots", Description: "list available slots", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"date": map[string]any{"type": "string"}}}}},
			Messages: []anthropicMessage{{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok, done"},
				map[string]any{"type": "text", "text": "thanks!"},
			}}}},
	}
	for i, in := range cases {
		got := estimateAnthropicRequestTokens(in)
		want := oldEstimateAnthropicRequestTokens(in)
		if got != want {
			t.Fatalf("case %d: estimate = %d, want %d (byte-identical to the old estimate for a no-media request)", i, got, want)
		}
	}
}

func TestEstimateOpenAIChatNoMediaMatchesOldEstimate(t *testing.T) {
	cases := []openAIRequest{
		{Model: "gpt-5.5", Messages: []openAIMessage{{Role: "user", Content: "hello there"}}},
		{Model: "gpt-5.5", Messages: []openAIMessage{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: []any{map[string]any{"type": "text", "text": "book me an appointment"}}},
			{Role: "assistant", Content: "sure, when?"},
		}, Tools: []openAITool{{Type: "function", Function: openAIFunction{Name: "get_slots", Description: "list slots", Parameters: map[string]any{"type": "object"}}}}},
	}
	for i, in := range cases {
		got := estimateOpenAIChatRequestTokens(in)
		want := oldEstimateOpenAIChatRequestTokens(in)
		if got != want {
			t.Fatalf("case %d: estimate = %d, want %d (byte-identical to the old estimate for a no-media request)", i, got, want)
		}
	}
}

func TestEstimateResponsesNoMediaMatchesOldEstimate(t *testing.T) {
	cases := []responsesRequest{
		{Model: "gpt-5.5", Instructions: "you are helpful", Input: []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello there"}}},
		}},
		{Model: "gpt-5.5", Input: []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "book an appointment"}}},
			map[string]any{"type": "function_call", "name": "get_slots", "call_id": "c1", "arguments": `{"date":"2026-09-24"}`},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": `{"slots":["10:00","11:00"]}`},
		}, Tools: []responsesTool{{Type: "function", Name: "get_slots", Description: "list slots", Parameters: map[string]any{"type": "object"}}}},
	}
	for i, in := range cases {
		got := estimateResponsesRequestTokens(in)
		want := oldEstimateResponsesRequestTokens(in)
		if got != want {
			t.Fatalf("case %d: estimate = %d, want %d (byte-identical to the old estimate for a no-media request)", i, got, want)
		}
		gotCodex := estimateCodexRequestTokens(in)
		wantCodex := oldEstimateCodexRequestTokens(in)
		if gotCodex != wantCodex {
			t.Fatalf("case %d (codex): estimate = %d, want %d", i, gotCodex, wantCodex)
		}
	}
}

func TestEstimateJSONTokensNoMediaUnchanged(t *testing.T) {
	v := map[string]any{"model": "gpt-5.5", "input": []any{map[string]any{"role": "user", "content": "hello, a perfectly ordinary short request"}}}
	if got, want := estimateJSONTokens(v), oldEstimateJSONTokens(v); got != want {
		t.Fatalf("estimateJSONTokens = %d, want %d (unchanged for a body with no base64 blob)", got, want)
	}
}

// ── media discount: structural (per-block) path ─────────────────────────────

func TestEstimateAnthropicImageDiscountedVsOldEstimate(t *testing.T) {
	raw := make([]byte, 300*1024) // ~300KB, matches the brief's fixture size
	for i := range raw {
		raw[i] = byte(i)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	textOnly := anthropicRequest{Model: "agy", Messages: []anthropicMessage{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "what does this say?"},
	}}}}
	withImage := anthropicRequest{Model: "agy", Messages: []anthropicMessage{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "what does this say?"},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": b64}},
	}}}}

	oldTokens := oldEstimateAnthropicRequestTokens(withImage)
	newTokens := estimateAnthropicRequestTokens(withImage)
	textOnlyTokens := oldEstimateAnthropicRequestTokens(textOnly) // same formula either way for text-only

	t.Logf("old (bug) estimate = %d tokens, fixed estimate = %d tokens, text-only baseline = %d tokens", oldTokens, newTokens, textOnlyTokens)

	if oldTokens < 50000 {
		t.Fatalf("fixture too small to demonstrate the old inflation: old estimate = %d", oldTokens)
	}
	want := textOnlyTokens + 1600
	if diff := newTokens - want; diff < -2 || diff > 2 {
		t.Fatalf("fixed estimate = %d, want ~%d (text tokens %d + flat image rate 1600)", newTokens, want, textOnlyTokens)
	}
}

// TestEstimateAnthropicPDFPagesDiscountedPrecisely distinguishes the
// STRUCTURAL per-block check (mediaPartFromBlock + mediaEstimateTokens,
// which gives an exact per-page PDF price) from dropBase64BlobChars' flat
// fallback (which a plain base64 "data" field would ALSO be caught by,
// since it is just a long run of base64 characters): only the structural
// path can know this is a 3-page PDF and price it at 3*1600, not a flat
// 1600. This is what makes M1 (removing the structural check) observably
// fail even though the fallback alone still discounts SOME of the size.
func TestEstimateAnthropicPDFPagesDiscountedPrecisely(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(threePagePDF())

	textOnly := anthropicRequest{Model: "agy", Messages: []anthropicMessage{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "how many pages?"},
	}}}}
	withPDF := anthropicRequest{Model: "agy", Messages: []anthropicMessage{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "how many pages?"},
		map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": b64}},
	}}}}

	textOnlyTokens := oldEstimateAnthropicRequestTokens(textOnly)
	newTokens := estimateAnthropicRequestTokens(withPDF)
	want := textOnlyTokens + 3*1600
	t.Logf("text-only = %d, with 3-page PDF = %d, want ~%d", textOnlyTokens, newTokens, want)
	if diff := newTokens - want; diff < -2 || diff > 2 {
		t.Fatalf("estimate = %d, want ~%d (text %d + 3 pages * 1600) — the page count must come from the structural per-block check", newTokens, want, textOnlyTokens)
	}
}

func TestEstimateOpenAIChatImageURLDataURLDiscounted(t *testing.T) {
	raw := make([]byte, 300*1024)
	for i := range raw {
		raw[i] = byte(i)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	in := openAIRequest{Model: "gpt-5.5", Messages: []openAIMessage{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "what does this say?"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + b64}},
	}}}}

	oldTokens := oldEstimateOpenAIChatRequestTokens(in)
	newTokens := estimateOpenAIChatRequestTokens(in)
	t.Logf("old (bug) estimate = %d tokens, fixed estimate = %d tokens", oldTokens, newTokens)

	if oldTokens < 50000 {
		t.Fatalf("fixture too small to demonstrate the old inflation: old estimate = %d", oldTokens)
	}
	if newTokens >= oldTokens/10 {
		t.Fatalf("fixed estimate = %d is not meaningfully smaller than the old %d", newTokens, oldTokens)
	}
	if newTokens > 2000 {
		t.Fatalf("fixed estimate = %d, want roughly text tokens + 1600 (a couple thousand at most)", newTokens)
	}
}

// ── media discount: Responses-wrapper (fallback / dropBase64BlobChars) path ─

func TestEstimateResponsesInputImageDiscounted(t *testing.T) {
	raw := make([]byte, 300*1024)
	for i := range raw {
		raw[i] = byte(i)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	in := responsesRequest{Model: "gpt-5.5", Input: []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "what does this say?"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + b64},
		}},
	}}

	oldTokens := oldEstimateResponsesRequestTokens(in)
	newTokens := estimateResponsesRequestTokens(in)
	t.Logf("old (bug) estimate = %d tokens, fixed estimate = %d tokens", oldTokens, newTokens)

	if oldTokens < 50000 {
		t.Fatalf("fixture too small to demonstrate the old inflation: old estimate = %d", oldTokens)
	}
	if newTokens >= oldTokens/10 {
		t.Fatalf("fixed estimate = %d is not meaningfully smaller than the old %d", newTokens, oldTokens)
	}
}

func TestEstimateCodexJSONDataURLDiscounted(t *testing.T) {
	raw := make([]byte, 300*1024)
	for i := range raw {
		raw[i] = byte(i)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	in := responsesRequest{Model: "gpt-5.5", Input: []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "what does this say?"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + b64},
		}},
	}}

	oldTokens := oldEstimateJSONTokens(codexRequestBody(in))
	newTokens := estimateJSONTokens(codexRequestBody(in))
	t.Logf("old (bug) estimateJSONTokens = %d tokens, fixed = %d tokens", oldTokens, newTokens)

	if oldTokens < 50000 {
		t.Fatalf("fixture too small to demonstrate the old inflation: old estimate = %d", oldTokens)
	}
	if newTokens >= oldTokens/10 {
		t.Fatalf("fixed estimate = %d is not meaningfully smaller than the old %d", newTokens, oldTokens)
	}

	oldCodex := oldEstimateCodexRequestTokens(in)
	newCodex := estimateCodexRequestTokens(in)
	t.Logf("old (bug) codex estimate = %d tokens, fixed = %d tokens", oldCodex, newCodex)
	if newCodex >= oldCodex/10 {
		t.Fatalf("fixed codex estimate = %d is not meaningfully smaller than the old %d", newCodex, oldCodex)
	}
}

// ── PDF page counting ────────────────────────────────────────────────────────

// threePagePDF returns synthetic (not a real, renderable) PDF bytes with
// three `/Type /Page` object markers and exactly one `/Type /Pages` (the
// page-tree root, which must NOT be counted as a page).
func threePagePDF() []byte {
	return []byte(
		"%PDF-1.4\n" +
			"1 0 obj<</Type/Pages/Kids[2 0 R 3 0 R 4 0 R]/Count 3>>endobj\n" +
			"2 0 obj<</Type/Page/Parent 1 0 R>>endobj\n" +
			"3 0 obj<</Type /Page/Parent 1 0 R>>endobj\n" + // whitespace variant
			"4 0 obj<</Type/Page/Parent 1 0 R>>endobj\n" +
			"%%EOF\n")
}

func TestCountPDFPageMarkersExcludesPagesRoot(t *testing.T) {
	if got := countPDFPageMarkers(threePagePDF()); got != 3 {
		t.Fatalf("countPDFPageMarkers = %d, want 3", got)
	}
	// /PageLabels must not be miscounted as /Page either.
	weird := []byte("<</Type/PageLabels/Nums[]>><</Type/Page>>")
	if got := countPDFPageMarkers(weird); got != 1 {
		t.Fatalf("countPDFPageMarkers(with /PageLabels) = %d, want 1", got)
	}
}

func TestEstimatePDFPagesFromBase64(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(threePagePDF())
	pages, ok := estimatePDFPagesFromBase64(b64)
	if !ok || pages != 3 {
		t.Fatalf("estimatePDFPagesFromBase64 = (%d, %v), want (3, true)", pages, ok)
	}
}

func TestMediaEstimateTokensPDFCountsPages(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(threePagePDF())
	p := mediaPart{MediaType: "application/pdf", B64: b64, Filename: "report.pdf"}
	if got, want := mediaEstimateTokens(p), 3*1600; got != want {
		t.Fatalf("mediaEstimateTokens = %d, want %d (3 pages * 1600)", got, want)
	}
}

func TestMediaEstimateTokensPDFByURLIsFlat(t *testing.T) {
	p := mediaPart{MediaType: "application/pdf", URL: "https://example.com/report.pdf"}
	if got, want := mediaEstimateTokens(p), 1600; got != want {
		t.Fatalf("mediaEstimateTokens(PDF by URL) = %d, want %d (flat)", got, want)
	}
}

func TestMediaEstimateTokensAudioIsFlat(t *testing.T) {
	p := mediaPart{MediaType: "audio/mpeg", B64: "notreallyaudio"}
	if got, want := mediaEstimateTokens(p), 1600; got != want {
		t.Fatalf("mediaEstimateTokens(audio) = %d, want %d (flat, unmeasured)", got, want)
	}
}

func TestMediaEstimateTokensPDFPageCountClampedTo500(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 600; i++ {
		b.WriteString("<</Type/Page>>")
	}
	pages, ok := estimatePDFPagesFromBase64(base64.StdEncoding.EncodeToString([]byte(b.String())))
	if !ok || pages != 500 {
		t.Fatalf("estimatePDFPagesFromBase64 = (%d, %v), want (500, true) — clamped", pages, ok)
	}
}

// ── codex pre-check: the request-size gate itself ───────────────────────────

// TestCodexTokenLimitPassesWithLargeImageAfterFix reproduces the brief's
// pre-check scenario directly against codexTokenLimitDecision: a request
// whose only large part is a ~2MB image is refused under the OLD estimate
// but passes under the fixed one.
func TestCodexTokenLimitPassesWithLargeImageAfterFix(t *testing.T) {
	t.Setenv("CODEX_UPSTREAM_HARD_TOKENS", "200000")
	t.Setenv("CODEX_UPSTREAM_BLOCK_AT_HARD", "1")

	raw := make([]byte, 2*1024*1024) // ~2MB, matches the brief's fixture size
	for i := range raw {
		raw[i] = byte(i)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	in := responsesRequest{Model: "gpt-5.5", Input: []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "what is in this image?"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + b64},
		}},
	}}

	oldTokens := oldEstimateCodexRequestTokens(in)
	newTokens := estimateCodexRequestTokens(in)
	t.Logf("old (bug) estimate = %d tokens, fixed estimate = %d tokens, hard limit = 200000", oldTokens, newTokens)

	if oldTokens < 200000 {
		t.Fatalf("fixture too small to demonstrate the old bug: old estimate = %d, want >= 200000", oldTokens)
	}
	oldDecision := codexTokenLimitDecision(oldTokens, 0)
	if oldDecision.Action != "error" {
		t.Fatalf("expected the OLD estimate (%d) to trip the hard limit, got %+v", oldTokens, oldDecision)
	}

	newDecision := codexTokenLimitDecision(newTokens, 0)
	if newDecision.Action == "error" {
		t.Fatalf("the FIXED estimate (%d) should no longer trip the hard limit: %+v", newTokens, newDecision)
	}
}
