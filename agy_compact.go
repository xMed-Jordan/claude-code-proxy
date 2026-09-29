package main

// agy_compact.go — virtual compaction of the conversation itself.
//
// Fitting (agy_transcript.go) buys room by shrinking tool results, and never
// touches a message: losing a customer's own words is a different class of harm
// from dropping a stale lookup. That leaves one case it cannot answer. agy
// delivers roughly 191KB to the model; the caller's persona and the tool
// catalog are fixed cost, so a long enough conversation eventually overflows on
// the dialogue alone, and agy silently cuts the newest messages off the end.
//
// So at that point the proxy does what a person would: it writes down what has
// happened so far and carries on from the note. The oldest part of the dialogue
// is replaced by a brief — what the customer asked for, what was decided, what
// was actually done, what is still open — and the recent turns continue
// verbatim. The brief is written by the same model, cached against the exact
// material it summarises, and marked in the transcript as a system summary so
// the model never mistakes it for something the customer said.
//
// Nothing here knows about bookings or any business rule: it summarises a
// conversation, whatever the conversation is about.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// agyCompactKeepCustomerTurns is how many of the customer's most recent
// messages (and everything after them) stay verbatim. The brief covers only
// what precedes them.
const agyCompactKeepCustomerTurns = 4

// agyCompactMaxResultChars caps each tool result quoted into the summarisation
// material; the brief needs what a call achieved, not its payload.
const agyCompactMaxResultChars = 400

// agyBriefMarker prefixes the system note that replaces the summarised turns.
const agyBriefMarker = "[Summary of the earlier part of this conversation, written by the system because the full text no longer fits in one message. It replaces the messages before it; everything after it is the conversation verbatim. Treat it as an accurate record of what was said and done, not as something the customer wrote.]"

var (
	agyBriefCacheMu sync.Mutex
	agyBriefCache   = map[string]agyBriefEntry{}
)

type agyBriefEntry struct {
	text string
	at   time.Time
}

const agyBriefCacheTTL = 6 * time.Hour
const agyBriefCacheMax = 256

// agyCompactMinGainBytes is how much dialogue must precede the recent turns
// before the proxy spends a model call summarising it by choice rather than by
// necessity.
const agyCompactMinGainBytes = 6000

// agyCompactIfNeeded returns a transcript that fits, compacting the dialogue
// when fitting alone cannot get the prompt inside the budget — or when it can
// only by gutting the results the current turn is acting on while older
// dialogue sits intact. The second result reports whether a brief was produced.
//
// The second case is the common one. Fitting can always make the prompt fit by
// spending tool results, so "still over budget" almost never fired, and older
// dialogue was never touched while the turn's own data went. Prod 2026-09-29
// (conv 61411, the owner's test number): ~60 staff notices preceded the
// customer's four messages, fitting emptied every catalog lookup of the turn,
// and the model quoted a price its persona uses as an example.
func agyCompactIfNeeded(ctx context.Context, cfg config, in agyGenInput) (agyTranscript, bool) {
	budget := agyPromptBudget(cfg)
	if budget <= 0 || !cfg.AgyCompact {
		return in.Transcript, false
	}
	prompt, fitted := renderAgyPromptFittedTranscript(cfg, in.System, in.Temperature, in.Tools, in.Transcript)
	var cut int
	switch {
	case len(prompt) > budget:
		cut = agyCompactCutIndex(in.Transcript, agyCompactKeepCustomerTurns)
		if cut <= 0 {
			log.Printf("[agy-compact] prompt is %d bytes over the %d-byte budget but there is nothing older to summarise", len(prompt)-budget, budget)
			return in.Transcript, false
		}
	case agyRecentResultsStarved(in.Transcript, fitted, agyCompactStepCutIndex(in.Transcript, agyCompactKeepCustomerTurns)):
		cut = agyCompactStepCutIndex(in.Transcript, agyCompactKeepCustomerTurns)
		if cut <= 0 || agyDialogueBytes(in.Transcript.Turns[:cut]) < agyCompactMinGainBytes {
			return in.Transcript, false
		}
		log.Printf("[agy-compact] results in the recent dialogue had to be cut to fit; summarising the %d turns before it", cut)
	default:
		return in.Transcript, false
	}
	material := renderAgyCompactionMaterial(in.Transcript.Turns[:cut])
	if strings.TrimSpace(material) == "" {
		return in.Transcript, false
	}
	brief, cached, err := agyConversationBrief(ctx, cfg, in.Model, material)
	if err != nil || strings.TrimSpace(brief) == "" {
		log.Printf("[agy-compact] could not write the brief (%v); continuing without compaction", err)
		return in.Transcript, false
	}
	out := agyTranscript{Turns: make([]agyTurn, 0, len(in.Transcript.Turns)-cut+1)}
	out.Turns = append(out.Turns, agyTurn{Kind: agyTurnSystemNote, Text: agyBriefMarker + "\n\n" + strings.TrimSpace(brief)})
	// System notes are the system's own context, not dialogue — the customer
	// record Connect looked up before the first message, which tools are already
	// preflighted — and a brief would paraphrase the identifiers in them. They
	// stay verbatim.
	for _, tt := range in.Transcript.Turns[:cut] {
		if tt.Kind == agyTurnSystemNote {
			out.Turns = append(out.Turns, tt)
		}
	}
	out.Turns = append(out.Turns, in.Transcript.Turns[cut:]...)
	log.Printf("[agy-compact] summarised the first %d turns into %d bytes (cached=%v); transcript %d→%d turns",
		cut, len(brief), cached, len(in.Transcript.Turns), len(out.Turns))
	return out, true
}

// agyCompactCutIndex finds where the verbatim tail begins: the start of the
// keep-th customer message counted from the end. Returns 0 when the
// conversation is too short to be worth summarising.
func agyCompactCutIndex(t agyTranscript, keep int) int {
	if keep < 1 {
		keep = 1
	}
	seen, first := 0, -1
	for i := len(t.Turns) - 1; i >= 0; i-- {
		if t.Turns[i].Kind != agyTurnCustomer {
			continue
		}
		seen++
		first = i
		if seen > keep {
			return i + 1 // everything strictly before this customer message
		}
	}
	// No more than keep customer messages: what precedes the first of them —
	// messages the business sent before the customer ever wrote — is still
	// older than all of the dialogue.
	if first > 0 {
		return first
	}
	return 0
}

// agyCompactStepCutIndex is the cut for summarising by choice: the verbatim
// tail keeps between keep and 2·keep−1 customer messages, and the cut moves in
// steps of keep, so the brief — cached by the exact material it covers — is
// written once every keep messages instead of on every turn. With fewer than
// 2·keep customer messages only what precedes the first of them is covered.
func agyCompactStepCutIndex(t agyTranscript, keep int) int {
	if keep < 1 {
		keep = 1
	}
	var customers []int
	for i, tt := range t.Turns {
		if tt.Kind == agyTurnCustomer {
			customers = append(customers, i)
		}
	}
	if len(customers) == 0 {
		return 0
	}
	steps := (len(customers) - keep) / keep
	if steps <= 0 {
		return customers[0]
	}
	return customers[steps*keep]
}

// agyRecentResultsStarved reports whether fitting cut a result inside the
// recent dialogue — from index from on, the part a brief would leave verbatim —
// to less than half of itself, or dropped it. That covers the turn being
// answered and the ones just before it: a customer who asks "and in Amman?"
// is asking about the previous turn's lookup. Only lossy cuts count: a stale
// copy and a record shown in full in a later result lose nothing.
func agyRecentResultsStarved(orig, fitted agyTranscript, from int) bool {
	if len(orig.Turns) != len(fitted.Turns) || from < 0 {
		return false
	}
	superseded := agySupersededResults(orig.Turns)
	for j := from; j < len(orig.Turns); j++ {
		if orig.Turns[j].Kind != agyTurnToolResult || superseded[j] || !fitted.Turns[j].Reduced {
			continue
		}
		if len(fitted.Turns[j].Text)*2 < len(orig.Turns[j].Text) {
			return true
		}
	}
	return false
}

// agyDialogueBytes is the size of the messages themselves in turns — what a
// brief would replace, as opposed to tool results the fitter can already spend.
func agyDialogueBytes(turns []agyTurn) int {
	n := 0
	for _, tt := range turns {
		if tt.Kind == agyTurnCustomer || tt.Kind == agyTurnAssistant {
			n += len(tt.Text)
		}
	}
	return n
}

// renderAgyCompactionMaterial renders the turns to be summarised: the dialogue
// in full, and each tool call with a short excerpt of what it returned, so the
// brief can record what was actually done and not merely discussed.
func renderAgyCompactionMaterial(turns []agyTurn) string {
	var b strings.Builder
	for _, tt := range turns {
		switch tt.Kind {
		case agyTurnCustomer:
			b.WriteString("Customer: ")
			b.WriteString(strings.Join(strings.Fields(stripCustomerHeader(tt.Text)), " "))
		case agyTurnAssistant:
			b.WriteString("Assistant: ")
			b.WriteString(strings.Join(strings.Fields(tt.Text), " "))
		case agyTurnSystemNote:
			continue // carried over verbatim (agyCompactIfNeeded)
		case agyTurnToolCall:
			b.WriteString("Assistant called tool ")
			b.WriteString(tt.Tool)
			b.WriteString(" with ")
			b.WriteString(truncateString(tt.Args, 300))
		case agyTurnToolResult:
			b.WriteString("Result of ")
			b.WriteString(firstNonEmpty(tt.Tool, "the tool"))
			b.WriteString(": ")
			b.WriteString(truncateString(strings.Join(strings.Fields(tt.Text), " "), agyCompactMaxResultChars))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// agyConversationBrief writes (or reuses) the brief for one block of material.
func agyConversationBrief(ctx context.Context, cfg config, model, material string) (string, bool, error) {
	key := agyBriefKey(model, material)
	if text, ok := agyBriefCacheGet(key); ok {
		return text, true, nil
	}
	prompt := agyBriefPrompt(material)
	res, err := agyResolveWithFormatRetry(ctx, cfg, nil, prompt, model)
	if err != nil {
		return "", false, err
	}
	text := strings.TrimSpace(res.Response)
	if text == "" {
		return "", false, fmt.Errorf("empty brief")
	}
	// The summariser has no tools; a tool_call in its reply is a slip, not content.
	if cleaned, _ := parseAgyToolCalls(text); strings.TrimSpace(cleaned) != "" {
		text = strings.TrimSpace(cleaned)
	}
	agyBriefCachePut(key, text)
	return text, false, nil
}

func agyBriefPrompt(material string) string {
	var b strings.Builder
	b.WriteString("### TASK\n\n")
	b.WriteString("Below is the earlier part of a conversation between a customer and an assistant, including the tools the assistant ran and what they returned. It is about to be removed from the assistant's context to make room, and your summary will stand in its place.\n\n")
	b.WriteString("Write the record the assistant will rely on. Cover, as short bullet points and only from the material below:\n")
	b.WriteString("- what the customer asked for, and every preference, constraint or detail they stated about themselves (quote short phrases where the wording matters);\n")
	b.WriteString("- what the assistant told them, and any commitment, price, date, time or option it offered;\n")
	b.WriteString("- what was actually DONE rather than discussed — anything created, changed, confirmed or cancelled — with the exact identifiers, dates and times involved;\n")
	b.WriteString("- what is still open: questions the customer asked that were not answered, and anything the assistant said it would do.\n\n")
	b.WriteString("Rules: use ONLY what appears below; never guess or fill gaps. Copy identifiers, numbers, dates and times exactly. Write in the language the customer is using. No preamble, no closing remark — just the bullets.\n\n")
	b.WriteString("### CONVERSATION SO FAR\n\n")
	b.WriteString(material)
	return b.String()
}

func agyBriefKey(model, material string) string {
	sum := sha256.Sum256([]byte(model + "\x00" + material))
	return hex.EncodeToString(sum[:])
}

func agyBriefCacheGet(key string) (string, bool) {
	agyBriefCacheMu.Lock()
	defer agyBriefCacheMu.Unlock()
	e, ok := agyBriefCache[key]
	if !ok || time.Since(e.at) > agyBriefCacheTTL {
		if ok {
			delete(agyBriefCache, key)
		}
		return "", false
	}
	return e.text, true
}

func agyBriefCachePut(key, text string) {
	agyBriefCacheMu.Lock()
	defer agyBriefCacheMu.Unlock()
	if len(agyBriefCache) >= agyBriefCacheMax {
		oldestKey, oldest := "", time.Now()
		for k, v := range agyBriefCache {
			if v.at.Before(oldest) {
				oldestKey, oldest = k, v.at
			}
		}
		if oldestKey != "" {
			delete(agyBriefCache, oldestKey)
		}
	}
	agyBriefCache[key] = agyBriefEntry{text: text, at: time.Now()}
}
