package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The summer beauty offer exactly as get_offer_by_id returned it on
// 2026-09-21 (conv 60746): 34 services, each with a few hundred bytes of
// marketing copy and a short packages list whose package_branch_ids ",1,"
// (Irbid) is the only place the offer says where it can be bought. The
// section and service branch lists (",1,2,4,") include Amman.
func summerOffer(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/offers/summer_beauty.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var packageBranchRe = regexp.MustCompile(`"package_branch_ids":",1,"`)

// Prod 2026-09-21: to fit, every packages list was emptied and all the copy
// kept; the model saw only the services' ",1,2,4," and sold the offer in Amman.
func TestAnOfferKeepsItsPackagesBeforeItsDescriptions(t *testing.T) {
	raw := summerOffer(t)
	want := len(packageBranchRe.FindAllString(raw, -1))
	if want < 40 {
		t.Fatalf("fixture changed: %d package branch lists", want)
	}
	got, ok := compactJSONForPrompt(raw, len(raw)*60/100, agyLevelHistory)
	if !ok || len(got) > len(raw)*60/100 {
		t.Fatalf("did not fit: %d of %d bytes", len(got), len(raw))
	}
	if n := len(packageBranchRe.FindAllString(got, -1)); n != want {
		t.Fatalf("%d of %d package branch lists survived", n, want)
	}
	if strings.Contains(got, "entries dropped") {
		t.Fatal("a packages list was thinned while descriptions could still go")
	}
	if !strings.Contains(got, `"price":"160.0000"`) {
		t.Fatal("the offer's prices were lost")
	}
}

// A record's prose is cut once per pass with ONE marker carrying the running
// count, never a marker inside a marker.
func TestShortenedProseCarriesOneMarker(t *testing.T) {
	long := strings.Repeat("وصف طويل للخدمة. ", 60)
	raw := `{"items":[{"id":1,"d":"` + long + `"},{"id":2,"d":"` + long + `"}]}`
	got, _ := compactJSONForPrompt(raw, 300, agyLevelHistory)
	if strings.Count(got, "[system note:") > 2 && !strings.Contains(got, agyDroppedFieldsKey) {
		t.Fatalf("markers nested:\n%s", got)
	}
	for _, m := range regexp.MustCompile(`… \[system note: [^\]]*\][^"]*\[system note`).FindAllString(got, -1) {
		t.Fatalf("a marker was cut into another: %q", m)
	}
}

// An instruction sheet is one long string outside any list. It is not a
// record's prose and waits for the prose level, as before.
func TestAnInstructionSheetWaitsForTheProseLevel(t *testing.T) {
	sheet := strings.Repeat("Always pass user_package_id, never the catalog id. ", 80)
	raw := `{"success":true,"instructions":"` + sheet + `","history":[{"d":"2026-09-01","n":"a"},{"d":"2026-09-02","n":"b"},{"d":"2026-09-03","n":"c"},{"d":"2026-09-04","n":"d"}]}`
	got, _ := compactJSONForPrompt(raw, len(raw)-50, agyLevelHistory)
	if !strings.Contains(got, sheet) {
		t.Fatal("the instruction sheet was shortened below the prose level")
	}
}
