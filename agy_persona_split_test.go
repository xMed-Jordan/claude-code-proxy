package main

// agy_persona_split_test.go — the STABLE/VOLATILE split of a Connect system
// prompt, and the bounded cleanup of persona agent directories.
//
// Why this exists. The agent name used to be a hash of the WHOLE system prompt.
// A Connect system prompt is the persona plus per-turn state — the date and
// time, the platform context, the cached summaries — so the name changed every
// turn and a fresh ~136 KB agent directory was written for every turn of every
// conversation. The owner's objection: "why do you rewrite it every time?"
//
// The split (agyPersonaSplit) keeps only the persona in the agent file and its
// name, and sends the per-turn tail in the message exactly as before. Measured
// on 25 real Connect prompts of one agent (25 turns, different customers): the
// text before "## Current Date & Time" hashed identically in all 25.
//
// These tests use synthetic personas only. The full-size check against the real
// 136 KB persona is run on the server by PROXY-1's manual test, not here.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// realShapeSystem is a system prompt shaped like Connect's: the persona, then
// the per-turn sections, in the order Connect writes them. turn1 and turn2 are
// two turns of one conversation — same persona, different date and different
// cached summaries — and are identical until the first per-turn heading.
func realShapeSystem(persona, dateLine, isoLine, topics, cachedEntry string) string {
	return persona +
		"\n\n## Current Date & Time\nTimezone: Asia/Amman\n" + dateLine + "\n" + isoLine + "\n" +
		"\n## Platform Context\nPlatform: whatsapp\nSpeaking with: Test Customer\nUser identifier: +962790000001\n" +
		"\n## User Context\n**Recent topics discussed**\n- " + topics + "\n" +
		"\n## Available Cached Data (Summaries)\n" + cachedEntry +
		"\n## Memory Management\n### Session Notes (conversation memory)\nkeep it short\n"
}

func splitTestPersona(marker string) string {
	return "# ULTRATHINK MODE ENABLED\n\n## CRITICAL INSTRUCTION\n" + marker +
		strings.Repeat("\nYou must never guess an identifier; only use what a tool returned.", 40) +
		"\n\n## Communication Style & Persona\nWarm, brief, in Arabic.\n"
}

func splitTurn(marker string) agyGenInput {
	a := realShapeSystem(splitTestPersona(marker), "Current date: Wednesday, October 2, 2026",
		"ISO timestamp: 2026-10-02T11:00:00+03:00", "the laser price", "- **book_laser** category=laser, name=Laser")
	b := realShapeSystem(splitTestPersona(marker), "Current date: Thursday, October 3, 2026",
		"ISO timestamp: 2026-10-03T09:30:00+03:00", "the retouch deposit", "- **book_retouch** category=retouch, name=Retouch")
	return agyGenInput{System: a, Transcript: secondTurn(b)}
}

func secondTurn(system string) agyTranscript {
	return agyTranscript{Turns: []agyTurn{{Kind: agyTurnCustomer, Text: "and tomorrow?"}}}
}

// ---------------------------------------------------------------------------
// The split itself
// ---------------------------------------------------------------------------

// The two halves together are the original prompt, in order. Only the
// whitespace at the seam is re-joined, so the invariant is checked without it:
// every character of the prompt, in its original order, is in one half or the
// other. Nothing the model used to read can go missing in the move.
func TestAgyPersonaSplitKeepsTheWholePrompt(t *testing.T) {
	in := splitTurn("PERSONA-MARKER")
	stable, volatile := agyPersonaSplit(in.System)
	squash := func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}
	if got := squash(stable + "\n" + volatile); got != squash(in.System) {
		t.Fatalf("stable+volatile is not the original prompt\ngot:  %.200q\nwant: %.200q", got, squash(in.System))
	}
	if !strings.Contains(stable, "PERSONA-MARKER") {
		t.Fatal("the persona is not in the stable part")
	}
	if strings.Contains(volatile, "PERSONA-MARKER") {
		t.Fatal("the persona leaked into the volatile part")
	}
	for _, want := range []string{"## Current Date & Time", "## Platform Context", "## User Context", "## Available Cached Data (Summaries)"} {
		if !strings.Contains(volatile, want) {
			t.Errorf("the volatile part lost %q", want)
		}
	}
	if strings.Contains(stable, "Current date:") {
		t.Error("the date stayed in the stable part, so the name would churn every turn")
	}
}

// A prompt with no per-turn heading is all persona: today's behaviour, not a
// wrong one.
func TestAgyPersonaSplitWithoutAnyHeading(t *testing.T) {
	s := "You are Nadia.\n\nBe brief."
	stable, volatile := agyPersonaSplit(s)
	if stable != s || volatile != "" {
		t.Fatalf("stable=%q volatile=%q, want the whole prompt and no tail", stable, volatile)
	}
}

// A prompt that is nothing but per-turn state has no persona to keep, so it is
// not split: an empty agent file would be worse than the old behaviour.
func TestAgyPersonaSplitWithNothingBeforeTheHeading(t *testing.T) {
	s := "## Platform Context\nUser identifier: +962790000001\n"
	stable, volatile := agyPersonaSplit(s)
	if stable != strings.TrimSpace(s) || volatile != "" {
		t.Fatalf("stable=%q volatile=%q", stable, volatile)
	}
}

// ---------------------------------------------------------------------------
// The headline: one file per persona, not one per turn
// ---------------------------------------------------------------------------

// Two consecutive turns of one conversation — different date, different cached
// summaries — get the SAME agent name and write exactly ONE file. Before the
// split this failed: the name was a hash of the whole prompt.
func TestAgyPersonaOneAgentFilePerPersonaNotPerTurn(t *testing.T) {
	home := personaTestHome(t)
	captureLog(t)
	cfg := personaTestOnConfig()
	in := splitTurn("SAME-PERSONA")

	first, firstVolatile := agyPersonaAgentFor(cfg, in)
	if first == "" {
		t.Fatal("the first turn got no persona agent")
	}
	if want := personaWantName(agyPersonaAgentDefaultPrefix, in); first != want {
		t.Fatalf("the agent name %q is not the hash of the stable part alone (%q)", first, want)
	}
	if firstVolatile == "" {
		t.Fatal("the first turn carried no volatile tail to send in the message")
	}

	// The next turn of the same conversation: same persona, new date, new summaries.
	next := splitTurn("SAME-PERSONA")
	next.System = realShapeSystem(splitTestPersona("SAME-PERSONA"), "Current date: Friday, October 4, 2026",
		"ISO timestamp: 2026-10-04T12:00:00+03:00", "the membership price", "- **book_membership** category=membership, name=Membership")
	second, secondVolatile := agyPersonaAgentFor(cfg, next)
	if second != first {
		t.Fatalf("the second turn got a new agent: %q then %q — one directory per turn is exactly what the owner objected to", first, second)
	}
	if secondVolatile == firstVolatile {
		t.Fatal("the two turns sent the same volatile tail, so the test proves nothing")
	}

	dirs, err := os.ReadDir(personaAgentsDir(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 {
		names := make([]string, 0, len(dirs))
		for _, d := range dirs {
			names = append(names, d.Name())
		}
		t.Fatalf("agents dir holds %d entries (%s), want exactly one", len(dirs), strings.Join(names, ", "))
	}
	if dirs[0].Name() != first {
		t.Fatalf("the directory is %q, want %q", dirs[0].Name(), first)
	}
	// And the file holds the persona, not the date.
	def, err := os.ReadFile(personaAgentFile(home, first))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(def), "SAME-PERSONA") {
		t.Error("the installed agent does not carry the persona")
	}
	if strings.Contains(string(def), "Current date:") {
		t.Error("the installed agent carries the date, so it would be rewritten every turn")
	}
}

// A persona that actually changed gets its own agent — the split must not make
// the name constant.
func TestAgyPersonaChangedPersonaGetsItsOwnAgent(t *testing.T) {
	personaTestHome(t)
	captureLog(t)
	cfg := personaTestOnConfig()

	before, _ := agyPersonaAgentFor(cfg, splitTurn("PERSONA-V1"))
	after, _ := agyPersonaAgentFor(cfg, splitTurn("PERSONA-V2-EDITED"))
	if before == "" || after == "" {
		t.Fatal("a turn got no persona agent")
	}
	if before == after {
		t.Fatalf("a changed persona kept the agent name %q", before)
	}
}

// The model still reads the per-turn state: it travels in the message, beside
// the pointer to the agent, in its original order.
func TestAgyPersonaVolatileTailTravelsInTheMessage(t *testing.T) {
	personaTestHome(t)
	captureLog(t)
	cfg := personaTestOnConfig()
	in := splitTurn("IN-MESSAGE-MARKER")

	var sent string
	stubPersonaResolve(t, func(prompt string) string {
		sent = prompt
		return "Yes."
	})
	if _, _, err := agyGenerate(t.Context(), cfg, in); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sent, "IN-MESSAGE-MARKER") {
		t.Error("the persona is still spelled out in the message; it should be a pointer to the agent")
	}
	if !strings.Contains(sent, agyPersonaPointer[:40]) {
		t.Error("the message does not point at the agent")
	}
	for _, want := range []string{"Current date: Wednesday, October 2, 2026", "User identifier: +962790000001", "- **book_laser**", "Memory Management"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the message lost %q", want)
		}
	}
	if !strings.Contains(sent, agyPersonaPointer[:40]) {
		t.Error("the message does not point at the agent")
	}
	// The pointer, then the tail: the order the prompt had.
	if strings.Index(sent, agyPersonaPointer[:40]) > strings.Index(sent, "## Current Date & Time") {
		t.Error("the volatile tail was placed before the pointer to the agent")
	}
}

// The switch off must be untouched: no split, no agent, the message exactly as
// it has always been.
func TestAgyPersonaSplitIsInertWithTheSwitchOff(t *testing.T) {
	home := personaTestHome(t)
	logs := captureLog(t)
	cfg := personaTestOnConfig()
	cfg.AgyPersonaInAgent = false

	in := splitTurn("OFF-MARKER")
	var sent string
	stubPersonaResolve(t, func(prompt string) string { sent = prompt; return "Yes." })
	if _, _, err := agyGenerate(t.Context(), cfg, in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, in.System) {
		t.Error("with the switch off the message does not carry the system prompt whole")
	}
	if strings.Contains(sent, agyPersonaPointer[:40]) {
		t.Error("with the switch off the message still points at an agent")
	}
	if logs.Len() != 0 {
		t.Errorf("the switch off logged: %s", logs.String())
	}
	if _, err := os.Stat(personaAgentsDir(home)); err == nil {
		t.Error("the switch off installed an agent")
	}
}

// ---------------------------------------------------------------------------
// The bounded cleanup
// ---------------------------------------------------------------------------

// mkAgedAgent writes a persona agent directory and backdates its mtime.
func mkAgedAgent(t *testing.T, home, name string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(personaAgentsDir(home), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
	return dir
}

func cleanupConfig() config {
	return config{AgyPersonaAgentPrefix: agyPersonaAgentDefaultPrefix}
}

// It is a DRY RUN unless the config says enforce, and it says what it would do.
func TestAgyPersonaCleanupIsDryRunByDefault(t *testing.T) {
	home := personaTestHome(t)
	logs := captureLog(t)
	stale := mkAgedAgent(t, home, "connect-chat-p-0123456789ab", 48*time.Hour)

	agyPersonaCleanupOnce = sync.Once{}
	agyPersonaAgentCleanup(cleanupConfig(), "connect-chat-p-ffffffffffff")

	if _, err := os.Stat(stale); err != nil {
		t.Fatal("the dry run deleted the directory")
	}
	if !strings.Contains(logs.String(), "dry run") || !strings.Contains(logs.String(), "connect-chat-p-0123456789ab") {
		t.Errorf("the dry run did not report what it would remove:\n%s", logs.String())
	}
}

// "enforce" removes what is old, and reports each removal.
func TestAgyPersonaCleanupEnforceRemovesOldAgents(t *testing.T) {
	home := personaTestHome(t)
	logs := captureLog(t)
	stale := mkAgedAgent(t, home, "connect-chat-p-0123456789ab", 48*time.Hour)
	cfg := cleanupConfig()
	cfg.AgyPersonaAgentCleanup = "enforce"

	agyPersonaCleanupOnce = sync.Once{}
	agyPersonaAgentCleanup(cfg, "connect-chat-p-ffffffffffff")

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("enforce left the stale directory: %v", err)
	}
	if !strings.Contains(logs.String(), "removed") {
		t.Errorf("enforce did not report the removal:\n%s", logs.String())
	}
}

// The agent serving this turn is never removed, however old it looks.
func TestAgyPersonaCleanupKeepsTheCurrentAgent(t *testing.T) {
	home := personaTestHome(t)
	captureLog(t)
	current := mkAgedAgent(t, home, "connect-chat-p-0123456789ab", 48*time.Hour)
	cfg := cleanupConfig()
	cfg.AgyPersonaAgentCleanup = "enforce"

	agyPersonaCleanupOnce = sync.Once{}
	agyPersonaAgentCleanup(cfg, "connect-chat-p-0123456789ab")

	if _, err := os.Stat(current); err != nil {
		t.Fatal("the cleanup removed the agent serving this turn")
	}
}

// A recent directory is another persona's, still in use: it stays.
func TestAgyPersonaCleanupKeepsRecentAgents(t *testing.T) {
	home := personaTestHome(t)
	captureLog(t)
	recent := mkAgedAgent(t, home, "connect-chat-p-0123456789ab", time.Hour)
	cfg := cleanupConfig()
	cfg.AgyPersonaAgentCleanup = "enforce"

	agyPersonaCleanupOnce = sync.Once{}
	agyPersonaAgentCleanup(cfg, "connect-chat-p-ffffffffffff")

	if _, err := os.Stat(recent); err != nil {
		t.Fatal("the cleanup removed an agent younger than 24h")
	}
}

// Nothing that is not a persona agent this feature writes is ever touched: the
// built-in agents, an operator's own agent, and a directory whose suffix is not
// 12 hex characters.
func TestAgyPersonaCleanupTouchesNothingElse(t *testing.T) {
	home := personaTestHome(t)
	captureLog(t)
	cfg := cleanupConfig()
	cfg.AgyPersonaAgentCleanup = "enforce"
	keep := []string{"connect-chat", "connect-media-view", "proxy1-probe", "connect-chat-p-nothex", "connect-chat-p-0123456789abEXTRA"}
	for _, n := range keep {
		mkAgedAgent(t, home, n, 48*time.Hour)
	}

	agyPersonaCleanupOnce = sync.Once{}
	agyPersonaAgentCleanup(cfg, "connect-chat-p-ffffffffffff")

	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(personaAgentsDir(home), n)); err != nil {
			t.Errorf("the cleanup removed %q, which is not a persona agent directory", n)
		}
	}
}

// It runs once per process, not on every request.
func TestAgyPersonaCleanupRunsOncePerProcess(t *testing.T) {
	home := personaTestHome(t)
	captureLog(t)
	stale := mkAgedAgent(t, home, "connect-chat-p-0123456789ab", 48*time.Hour)
	cfg := cleanupConfig()
	cfg.AgyPersonaAgentCleanup = "enforce"

	agyPersonaCleanupOnce = sync.Once{}
	agyPersonaAgentCleanup(cfg, "connect-chat-p-ffffffffffff")
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("the first pass did not remove it")
	}
	// A directory created afterwards is out of this process's reach.
	later := mkAgedAgent(t, home, "connect-chat-p-111111111111", 48*time.Hour)
	agyPersonaAgentCleanup(cfg, "connect-chat-p-ffffffffffff")
	if _, err := os.Stat(later); err != nil {
		t.Fatal("the cleanup ran a second time in one process")
	}
}

// The name test is the guard on the whole cleanup: it is what stops an
// operator's directory from being deleted.
func TestAgyPersonaAgentNameIsOurs(t *testing.T) {
	for _, name := range []string{"connect-chat-p-0123456789ab", "connect-chat-p-ffffffffffff"} {
		if !agyPersonaAgentNameIsOurs(agyPersonaAgentDefaultPrefix, name) {
			t.Errorf("%q should be ours", name)
		}
	}
	for _, name := range []string{"connect-chat", "connect-media-view", "proxy1-probe", "connect-chat-p-short", "connect-chat-p-0123456789abX", "other-p-0123456789ab"} {
		if agyPersonaAgentNameIsOurs(agyPersonaAgentDefaultPrefix, name) {
			t.Errorf("%q should NOT be ours", name)
		}
	}
}
