package main

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// agyAsRecords reads a compacted result the way the fitter does — tables back
// into records — so a test can look for a record's fields whatever the layout.
func agyAsRecords(t *testing.T, out string) string {
	t.Helper()
	v, ok := agyDecodeJSONValue(out)
	if !ok {
		t.Fatalf("not JSON: %s", truncateString(out, 300))
	}
	raw, err := marshalJSONNoHTML(agyUntabulate(v))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var serviceIDRe = regexp.MustCompile(`"service_id":\d+`)

// botoxSection is get_services_packages {"section_ids":"3"} exactly as it
// reached the proxy on 2026-09-29 (conv 61411): the Botox section, fourteen
// services, each with its packages — Full Face Botox (162) first, sold at 180 in
// Amman (",2,") and 144 in Irbid (",1,", the summer offer).
func botoxSection(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/catalog/botox_section.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTabulationLosesNothing(t *testing.T) {
	raw := `{"packages":[` +
		`{"package_id":1888,"price":"180.0000","package_branch_ids":",2,","is_active":true,"package_name_en":"Dysport Women"},` +
		`{"package_id":5883,"price":"144.0000","package_branch_ids":",1,","is_active":true,"package_name_en":"Dysport Women - Summer offer","offer_id":82},` +
		`{"package_id":5955,"price":"210.0000","package_branch_ids":",2,","is_active":true,"package_name_en":"Dysport Men"}]}`
	v, _ := agyDecodeJSONValue(raw)
	written, ok := agyTabulated(v)
	if !ok {
		t.Fatal("a list of flat records was not written as a table")
	}
	out, _ := marshalJSONNoHTML(written)
	if len(out) >= len(raw) {
		t.Fatalf("the table is not smaller: %d >= %d", len(out), len(raw))
	}
	if strings.Count(string(out), `"is_active"`) != 1 {
		t.Fatalf("a field with one value in every record should be stated once:\n%s", out)
	}
	orig, _ := agyDecodeJSONValue(raw)
	back, _ := agyDecodeJSONValue(string(out))
	if !reflect.DeepEqual(agyUntabulate(back), orig) {
		t.Fatalf("reading the table back does not give the records:\n%s", out)
	}
	if again, _ := marshalJSONNoHTML(v); string(again) != string(mustMarshal(t, orig)) {
		t.Fatal("writing tables changed the records it was given")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := marshalJSONNoHTML(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTabulationLeavesOtherShapesAlone(t *testing.T) {
	for _, raw := range []string{
		`{"a":[{"id":1,"sub":[1,2]},{"id":2,"sub":[3]}]}`, // records with nested lists
		`{"a":[{"id":1,"name":"x"}]}`,                     // a single record
		`{"a":[1,2,3]}`,                                   // no records
		`{"a":[{"x":1},{"y":2}]}`,                         // no table is smaller
	} {
		v, _ := agyDecodeJSONValue(raw)
		if _, ok := agyTabulated(v); ok {
			t.Errorf("%s was written as a table", raw)
		}
	}
}

// The notes the fitter writes about a list — the columns it dropped, the
// entries it dropped — survive the table and come back on reading it.
func TestATableKeepsTheListsNotes(t *testing.T) {
	list := []any{
		map[string]any{"package_id": "1", "package_name_en": "Package a", "price": "10.0000", agyDroppedFieldsKey: "notes"},
		map[string]any{"package_id": "2", "package_name_en": "Package b", "price": "20.0000"},
		map[string]any{"package_id": "3", "package_name_en": "Package c", "price": "30.0000"},
		map[string]any{"package_id": "4", "package_name_en": "Package d", "price": "40.0000"},
		agyOmissionMarker + "4 entries dropped by the system to fit the context window]",
	}
	table, ok := agyTableOf(list)
	if !ok {
		t.Fatal("not written as a table")
	}
	if table[agyDroppedFieldsKey] != "notes" {
		t.Fatalf("the dropped-columns note was lost: %v", table)
	}
	back := agyUntabulate(map[string]any{"x": table}).(map[string]any)["x"]
	if !reflect.DeepEqual(back, list) {
		t.Fatalf("read back as %v, want %v", back, list)
	}
}

// An API object that merely looks like a table is read as it is.
func TestUntabulateIgnoresLookalikes(t *testing.T) {
	for _, raw := range []string{
		`{"columns":["a","b"],"rows":[[1,2]],"total":1}`, // a key no table carries
		`{"columns":["a","b"],"rows":[[1,2,3]]}`,         // a row that does not fit the header
		`{"columns":[],"rows":[]}`,                       // no header
	} {
		v, _ := agyDecodeJSONValue(raw)
		if _, ok := agyUntabulate(v).(map[string]any); !ok {
			t.Errorf("%s was read as a table", raw)
		}
	}
}

// Prod 2026-09-29 (conv 61411): squeezed, every service in the section kept its
// name and lost its prices. The leading services must stay whole instead, and
// the trailing ones keep their ids and names.
func TestACatalogKeepsItsLeadingRecordsWhole(t *testing.T) {
	raw := botoxSection(t)
	out, ok := compactJSONForPrompt(raw, len(raw)/2, agyLevelHistory)
	if !ok || len(out) > len(raw)/2 {
		t.Fatalf("did not fit: ok=%v %d of %d", ok, len(out), len(raw))
	}
	records := agyAsRecords(t, out)
	for _, want := range []string{`"price":"180.0000"`, `"price":"144.0000"`, `"package_branch_ids":",2,"`, `"package_branch_ids":",1,"`} {
		if !strings.Contains(records, want) {
			t.Fatalf("Full Face Botox lost %s:\n%s", want, truncateString(out, 1500))
		}
	}
	if !strings.Contains(out, agyStubKey) {
		t.Fatalf("expected the trailing services to give up their details:\n%s", truncateString(out, 1500))
	}
	if strings.Contains(out, "entries dropped") {
		t.Fatalf("a list was thinned at the history level:\n%s", truncateString(out, 1500))
	}
	ids := serviceIDRe.FindAllString(raw, -1)
	if len(ids) != 14 {
		t.Fatalf("fixture changed: %d services", len(ids))
	}
	for _, id := range ids {
		if !strings.Contains(records, id) {
			t.Fatalf("%s is gone although its details could go first", id)
		}
	}
}

// Squeezed further, the services that already gave up their details go before
// the one whole service gives up its prices.
func TestReducedRecordsGoBeforeTheWholeOneIsThinned(t *testing.T) {
	raw := botoxSection(t)
	out, ok := compactJSONForPrompt(raw, 1500, agyLevelProse)
	if !ok || len(out) > 1500 {
		t.Fatalf("did not fit: ok=%v %d", ok, len(out))
	}
	records := agyAsRecords(t, out)
	for _, want := range []string{`"service_id":162`, `"price":"180.0000"`, `"price":"144.0000"`} {
		if !strings.Contains(records, want) {
			t.Fatalf("lost %s:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "entries dropped") {
		t.Fatalf("expected the trailing services to be counted as dropped:\n%s", out)
	}
}

// A result's own record list — the ids the next call names — keeps every
// record until the last level, whatever happened to their details
// (agyMinRecordKeys, prod conv 9f24dc22).
func TestTheRecordListKeepsEveryRecordBelowTheLastLevel(t *testing.T) {
	raw := buildPackagesResult(24, 12)
	for _, level := range []int{agyLevelHistory, agyLevelProse, agyLevelFields} {
		out, _ := compactJSONForPrompt(raw, 4000, level)
		records := agyAsRecords(t, out)
		for i := 0; i < 24; i++ {
			if !strings.Contains(records, fmt.Sprintf(`"user_package_id":%d`, 263660+i)) {
				t.Fatalf("level %d: record %d dropped:\n%s", level, 263660+i, truncateString(out, 1200))
			}
		}
	}
}

func TestRepeatedRecordsPointToTheirLaterCopy(t *testing.T) {
	rec := func(id int) string {
		return fmt.Sprintf(`{"service_id":%d,"service_name_en":"Service %d","service_description_en":"%s","packages":[{"package_id":%d,"price":"100.0000"}]}`,
			id, id, strings.Repeat("words ", 30), id*10)
	}
	wide := `{"data":[` + rec(1) + `,` + rec(2) + `,` + rec(3) + `]}`
	narrow := `{"data":[` + rec(2) + `]}`
	other := `{"data":[` + rec(3) + `]}`
	turns := []agyTurn{
		{Kind: agyTurnToolResult, Tool: "get_services_packages", Text: wide},
		{Kind: agyTurnToolResult, Tool: "another_tool", Text: other},
		{Kind: agyTurnToolResult, Tool: "get_services_packages", Text: narrow},
	}
	got := agyDedupeRepeatedRecords(turns)
	if len(got) != 1 {
		t.Fatalf("expected only the wide result to change, got %v", got)
	}
	text := got[0]
	if strings.Count(text, agyRepeatedKey) != 1 {
		t.Fatalf("expected one pointer, for service 2:\n%s", text)
	}
	if !strings.Contains(text, `"service_id":2`) || !strings.Contains(text, `"service_name_en":"Service 2"`) {
		t.Fatalf("the pointer lost the record's own short facts:\n%s", text)
	}
	if strings.Count(text, `"price":"100.0000"`) != 2 {
		t.Fatalf("services 1 and 3 must stay whole — service 3 is repeated by another tool only:\n%s", text)
	}
}

func TestSmallRepeatedRecordsStay(t *testing.T) {
	turns := []agyTurn{
		{Kind: agyTurnToolResult, Tool: "t", Text: `{"data":[{"id":1,"name":"a"},{"id":2,"name":"b"}]}`},
		{Kind: agyTurnToolResult, Tool: "t", Text: `{"data":[{"id":1,"name":"a"}]}`},
	}
	if got := agyDedupeRepeatedRecords(turns); len(got) != 0 {
		t.Fatalf("a pointer to a record this small saves nothing: %v", got)
	}
}

// Prod 2026-09-29 (conv 61411): the current turn fetched an instruction sheet
// and the Botox section; the section's prices were thinned three times while
// the sheet waited for the prose level. The sheet is shortened first now.
func TestAnInstructionSheetIsShortenedBeforeATurnsDataIsThinned(t *testing.T) {
	sheet := `{"success":true,"instructions":` + jsonQuote(strings.Repeat("Call get_offers_list first, then get_offer_by_id with the id you chose. ", 130)) + `}`
	msgs := []anthropicMessage{
		{Role: "user", Content: "تاكدي بعمان البوتوكس ب 180"},
		{Role: "assistant", Content: []any{
			map[string]any{"type": "tool_use", "id": "i1", "name": "get_tool_instructions", "input": map[string]any{"tool_code": "get_offers_list"}},
			map[string]any{"type": "tool_use", "id": "s3", "name": "get_services_packages", "input": map[string]any{"section_ids": "3"}},
		}},
		{Role: "user", Content: []any{
			map[string]any{"type": "tool_result", "tool_use_id": "i1", "content": sheet},
			map[string]any{"type": "tool_result", "tool_use_id": "s3", "content": botoxSection(t)},
		}},
	}
	tr := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: msgs})
	full := len(renderAgyTranscript(tr, 0, false))
	fitted, _ := fitAgyTranscript(tr, 0, false, full-6000)
	var section string
	for _, tt := range fitted.Turns {
		if tt.Kind == agyTurnToolResult && tt.Tool == "get_services_packages" {
			section = tt.Text
		}
	}
	records := agyAsRecords(t, section)
	for _, want := range []string{`"price":"180.0000"`, `"price":"144.0000"`} {
		if !strings.Contains(records, want) {
			t.Fatalf("the Botox prices went while the instruction sheet could be shortened:\n%s", truncateString(section, 1200))
		}
	}
}

// A result that fits once its record lists are written as tables has lost
// nothing: it is not reported as shortened, so the model is not told to fetch
// it again — the loop behind twelve lookups in three and a half minutes.
func TestAResultWrittenAsTablesIsNotShortened(t *testing.T) {
	raw := buildPackagesResult(12, 8)
	msgs := []anthropicMessage{
		{Role: "user", Content: "شو باقاتي؟"},
		{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "pk", "name": "get_customer_packages", "input": map[string]any{}}}},
		{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "pk", "content": raw}}},
	}
	tr := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: msgs})
	full := len(renderAgyTranscript(tr, 0, false))
	fitted, _ := fitAgyTranscript(tr, 0, false, full-len(raw)/2)
	last := fitted.Turns[len(fitted.Turns)-1]
	if len(last.Text) >= len(raw) {
		t.Fatalf("fixture: the result was not rewritten (%d bytes)", len(last.Text))
	}
	if last.Reduced {
		t.Fatal("a result that only changed layout is marked as shortened")
	}
	if rendered := renderAgyTranscript(fitted, 0, false); strings.Contains(rendered, agyRecallNote) {
		t.Fatal("the model is told to fetch again a result it has in full")
	}
	if got := strings.Count(agyAsRecords(t, last.Text), `"reservation_id"`); got != 12*8 {
		t.Fatalf("%d of %d reservations survived", got, 12*8)
	}
}
