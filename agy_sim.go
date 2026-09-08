package main

// agy_sim.go — `connect-ai-proxy agy-sim <scenario.json>`: replay a Connect
// agent conversation against the real agy upstream, with Connect's tool
// protocol emulated locally (strict preflight via get_tool_instructions, raw
// current-turn tool results, {"name","result"}-wrapped history results, the
// duplicated headered customer message). Used to reproduce production
// failures (e.g. the 2026-09-08 slot loop) and to A/B prompt modes
// (--legacy) and models (--model) without touching Connect.
//
// Scenario file:
//
//	{
//	  "model": "gemini-3.8-flash-medium",
//	  "system": "...persona...",
//	  "temperature": 0.3,
//	  "tools": [ {"name","description","input_schema"} ],
//	  "messages": [ ...Anthropic messages (seed history)... ],
//	  "script": [ {"text": "بكرا على ال 9"}, {"text": "الصبح", "if": "المسا"} ],
//	  "canned": {
//	    "get_available_slots": {"by": "from_date|to_date", "map": {"2026-09-09|2026-09-09": {...}}, "default": {...}},
//	    "get_tool_instructions": {"by": "tool_code", "map": {"create_reservation": {...}}},
//	    "*": {"default": {"success": false, "error": "not available in simulation"}}
//	  },
//	  "customer_phone": "+962782400075", "platform": "whatsapp",
//	  "max_iterations": 8, "stop_after": "create_reservation"
//	}
//
// Script entries are consumed in order; an entry with "if" (regex, matched
// against the assistant's previous reply) is only used when it matches, so the
// simulated customer can answer a clarifying question the assistant may or may
// not ask.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type simCanned struct {
	By      string                     `json:"by"`
	Map     map[string]json.RawMessage `json:"map"`
	Default json.RawMessage            `json:"default"`
}

type simScriptEntry struct {
	Text string `json:"text"`
	If   string `json:"if"`
}

type simScenario struct {
	Model         string               `json:"model"`
	System        string               `json:"system"`
	Temperature   *float64             `json:"temperature"`
	Tools         []anthropicTool      `json:"tools"`
	Messages      []anthropicMessage   `json:"messages"`
	Script        []simScriptEntry     `json:"script"`
	Canned        map[string]simCanned `json:"canned"`
	CustomerPhone string               `json:"customer_phone"`
	Platform      string               `json:"platform"`
	MaxIterations int                  `json:"max_iterations"`
	StopAfter     string               `json:"stop_after"`
	Preflight     *bool                `json:"preflight_strict"`
}

type simToolCall struct {
	Turn int
	Iter int
	Name string
	Args string
}

func rawJSONToString(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return ""
	}
	var s string
	if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// simLookupCanned resolves the canned result for a call.
func simLookupCanned(sc simScenario, name string, args map[string]any) (string, bool) {
	c, ok := sc.Canned[name]
	if !ok {
		c, ok = sc.Canned["*"]
		if !ok {
			return "", false
		}
	}
	if c.By != "" && len(c.Map) > 0 {
		var keyParts []string
		for _, k := range strings.Split(c.By, "|") {
			keyParts = append(keyParts, strings.TrimSpace(fmt.Sprint(args[strings.TrimSpace(k)])))
		}
		if v, ok := c.Map[strings.Join(keyParts, "|")]; ok {
			return simFillTemplate(rawJSONToString(v), args), true
		}
	}
	if len(c.Default) > 0 {
		return simFillTemplate(rawJSONToString(c.Default), args), true
	}
	return "", false
}

// simFillTemplate substitutes {{arg}} placeholders in a canned result with the
// call's argument values so e.g. a booking confirmation echoes the requested
// date/time.
func simFillTemplate(s string, args map[string]any) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	for k, v := range args {
		val := strings.TrimSpace(fmt.Sprint(v))
		val = strings.ReplaceAll(val, `\`, `\\`)
		val = strings.ReplaceAll(val, `"`, `\"`)
		s = strings.ReplaceAll(s, "{{"+k+"}}", val)
	}
	return s
}

// simSyntheticInstructions mirrors Connect's GetToolInstructionsTool fallback shape.
func simSyntheticInstructions(sc simScenario, code string) string {
	for _, t := range sc.Tools {
		if t.Name != code {
			continue
		}
		var params []map[string]any
		if schema, ok := t.InputSchema.(map[string]any); ok {
			req := map[string]bool{}
			if rs, ok := schema["required"].([]any); ok {
				for _, r := range rs {
					req[fmt.Sprint(r)] = true
				}
			}
			if props, ok := schema["properties"].(map[string]any); ok {
				for p, def := range props {
					desc := ""
					if dm, ok := def.(map[string]any); ok {
						desc, _ = dm["description"].(string)
					}
					params = append(params, map[string]any{"name": p, "required": req[p], "description": desc})
				}
			}
		}
		out := map[string]any{
			"success":      true,
			"tool":         map[string]any{"code": code, "name": code, "type": "http_request", "description": t.Description},
			"instructions": "No specific instructions provided for this tool. Check the parameters below.",
			"parameters":   params,
			"usage_hints":  "This tool makes an HTTP API call. Ensure all required data is collected before calling.",
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	b, _ := json.Marshal(map[string]any{"success": false, "error": fmt.Sprintf("Tool '%s' not found. Use the exact tool code or name.", code)})
	return string(b)
}

// simBookings tracks reservations the simulation has "created" so a second
// booking for the same customer at an overlapping time is rejected the way
// the real clinic API rejects it (error_code 1017).
var simBookings = map[string]bool{}

// simExecute emulates Connect's tool execution for one call.
func simExecute(sc simScenario, preflighted map[string]bool, strict bool, name string, args map[string]any) string {
	if name == "create_reservation" || name == "create_retouch_reservation" || name == "create_multi_service_reservation" {
		key := strings.TrimSpace(fmt.Sprint(args["customer_uuid"])) + "|" + strings.TrimSpace(fmt.Sprint(args["date"])) + "|" + strings.TrimSpace(fmt.Sprint(args["time_from"]))
		if simBookings[key] {
			b, _ := json.Marshal(map[string]any{"success": false, "data": map[string]any{"status": 400, "body": map[string]any{
				"success": false, "error_code": 1017, "error": "customer already has a reservation at this time",
				"error_ar": "العميل لديه حجز مسبق في هذا الوقت"}}, "error": nil})
			return string(b)
		}
		defer func() { simBookings[key] = true }()
	}
	if name == "get_tool_instructions" {
		code := strings.TrimSpace(fmt.Sprint(args["tool_code"]))
		preflighted[code] = true
		if r, ok := simLookupCanned(sc, name, args); ok && r != "" {
			return r
		}
		return simSyntheticInstructions(sc, code)
	}
	if strict && !preflighted[name] {
		// Connect's guard: the tool does NOT run; instructions are embedded.
		preflighted[name] = true
		var instr any
		if r, ok := simLookupCanned(sc, "get_tool_instructions", map[string]any{"tool_code": name}); ok && r != "" {
			_ = json.Unmarshal([]byte(r), &instr)
		}
		if instr == nil {
			_ = json.Unmarshal([]byte(simSyntheticInstructions(sc, name)), &instr)
		}
		b, _ := json.Marshal(map[string]any{"success": false, "skipped": true, "reason": "preflight_required", "tool_instructions": instr})
		return string(b)
	}
	if r, ok := simLookupCanned(sc, name, args); ok {
		return r
	}
	b, _ := json.Marshal(map[string]any{"success": false, "error": "tool '" + name + "' is not available in this simulation"})
	return string(b)
}

// simPostAnthropic sends the turn to a running proxy's /anthropic/v1/messages
// and maps the Anthropic response back to the responsesResponse shape the
// simulator works with.
func simPostAnthropic(base string, cfg config, in anthropicRequest) (responsesResponse, error) {
	in.Stream = false
	if in.MaxTokens == 0 {
		in.MaxTokens = 65536
	}
	body, err := json.Marshal(in)
	if err != nil {
		return responsesResponse{}, err
	}
	req, err := http.NewRequest(http.MethodPost, base+"/anthropic/v1/messages", bytes.NewReader(body))
	if err != nil {
		return responsesResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if k := strings.TrimSpace(cfg.ProxyKey); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
		req.Header.Set("x-api-key", k)
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	res, err := client.Do(req)
	if err != nil {
		return responsesResponse{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode != http.StatusOK {
		return responsesResponse{}, fmt.Errorf("proxy HTTP %d: %s", res.StatusCode, truncateString(string(raw), 300))
	}
	var out struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return responsesResponse{}, fmt.Errorf("proxy response not JSON: %w", err)
	}
	var resp responsesResponse
	for _, c := range out.Content {
		switch c.Type {
		case "text":
			if strings.TrimSpace(c.Text) != "" {
				resp.Output = append(resp.Output, responsesOutputItem{Type: "message", Role: "assistant", Content: []responsesOutputContent{{Type: "output_text", Text: c.Text}}})
			}
		case "tool_use":
			args := strings.TrimSpace(string(c.Input))
			if args == "" {
				args = "{}"
			}
			resp.Output = append(resp.Output, responsesOutputItem{Type: "function_call", Name: c.Name, CallID: firstNonEmpty(c.ID, fmt.Sprintf("toolu_%d", time.Now().UnixNano())), Arguments: args})
		}
	}
	resp.Usage.InputTokens = out.Usage.InputTokens
	resp.Usage.OutputTokens = out.Usage.OutputTokens
	return resp, nil
}

func simPickScript(sc simScenario, used []bool, lastReply string) (int, bool) {
	for i, e := range sc.Script {
		if used[i] {
			continue
		}
		if strings.TrimSpace(e.If) == "" {
			return i, true
		}
		if re, err := regexp.Compile(e.If); err == nil && re.MatchString(lastReply) {
			return i, true
		}
	}
	return -1, false
}

func simCustomerHeader(sc simScenario) string {
	loc, err := time.LoadLocation("Asia/Amman")
	if err != nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	phone := firstNonEmpty(sc.CustomerPhone, "+962000000000")
	platform := firstNonEmpty(sc.Platform, "whatsapp")
	return fmt.Sprintf("[%s | Phone: %s | Platform: %s]", now.Format("Mon, Jan 2, 3:04 PM"), phone, platform)
}

func runAgySim(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: connect-ai-proxy agy-sim <scenario.json> [--model M] [--out DIR] [--legacy] [--cap N] [--max-turns N] [--dump-only]")
	}
	path := args[0]
	model, outDir := "", ""
	legacy := false
	cap := -1
	maxTurns := 0
	dumpOnly := false
	viaProxy := ""
	label := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--model":
			i++
			if i < len(args) {
				model = args[i]
			}
		case "--out":
			i++
			if i < len(args) {
				outDir = args[i]
			}
		case "--legacy":
			legacy = true
		case "--cap":
			i++
			if i < len(args) {
				cap, _ = strconv.Atoi(args[i])
			}
		case "--max-turns":
			i++
			if i < len(args) {
				maxTurns, _ = strconv.Atoi(args[i])
			}
		case "--dump-only":
			dumpOnly = true
		case "--via-proxy":
			i++
			if i < len(args) {
				viaProxy = strings.TrimRight(args[i], "/")
			}
		case "--label":
			i++
			if i < len(args) {
				label = args[i]
			}
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var sc simScenario
	if err := json.Unmarshal(raw, &sc); err != nil {
		return fmt.Errorf("scenario: %w", err)
	}
	if model == "" {
		model = sc.Model
	}
	if sc.MaxIterations <= 0 {
		sc.MaxIterations = 8
	}
	// Connect runs an EAGER preflight by default (the target executes in the
	// same iteration and the model only sees the real result); the visible
	// "preflight_required" skip is emulated only when preflight_strict=true.
	strict := sc.Preflight != nil && *sc.Preflight
	if maxTurns <= 0 {
		maxTurns = len(sc.Script)
	}

	cfg := loadConfig()
	if model != "" {
		cfg.AgyModel = model // forward verbatim to agy
	}
	if legacy {
		cfg.AgyPromptMode = "legacy"
	}
	if cap >= 0 {
		cfg.AgyToolResultCap = cap
	}
	initAgy(cfg)
	if outDir != "" {
		_ = os.MkdirAll(outDir, 0o755)
	}
	dump := func(name, content string) {
		if outDir == "" {
			return
		}
		_ = os.WriteFile(filepath.Join(outDir, name), []byte(content), 0o644)
	}

	fmt.Printf("=== agy-sim%s: model=%s mode=%s via=%s tools=%d seed_messages=%d script=%d strict_preflight=%v cap=%d\n",
		func() string {
			if label != "" {
				return " [" + label + "]"
			}
			return ""
		}(), firstNonEmpty(model, "(agy default)"), cfg.AgyPromptMode, firstNonEmpty(viaProxy, "direct"), len(sc.Tools), len(sc.Messages), len(sc.Script), strict, cfg.AgyToolResultCap)

	messages := append([]anthropicMessage(nil), sc.Messages...)
	preflighted := map[string]bool{}
	seed := buildAgyTranscriptFromAnthropic(anthropicRequest{Messages: messages})
	for _, p := range seed.preflightedTools() {
		preflighted[p] = true
	}
	fmt.Printf("preflighted from seed: %s\n", strings.Join(seed.preflightedTools(), ", "))

	used := make([]bool, len(sc.Script))
	lastReply := ""
	var allCalls []simToolCall
	stopHit := false
	loopFailures := 0
	totalCalls := 0
	start := time.Now()

	for turn := 1; turn <= maxTurns; turn++ {
		idx, ok := simPickScript(sc, used, lastReply)
		if !ok {
			fmt.Println("--- script exhausted")
			break
		}
		used[idx] = true
		text := sc.Script[idx].Text
		fmt.Printf("\n################ TURN %d — customer: %s\n", turn, text)
		messages = append(messages,
			anthropicMessage{Role: "user", Content: text},
			anthropicMessage{Role: "user", Content: simCustomerHeader(sc) + "\n" + text},
		)
		var turnResults []int // indexes into messages of this turn's tool_result user messages
		replied := false
		for iter := 1; iter <= sc.MaxIterations; iter++ {
			in := anthropicRequest{Model: firstNonEmpty(model, "agy"), MaxTokens: 65536, System: sc.System, Messages: messages, Tools: sc.Tools, Temperature: sc.Temperature}
			inputTokens := estimateAnthropicRequestTokens(in)
			if dumpOnly {
				gi := agyGenInputFromAnthropic(in, inputTokens)
				p := renderAgyPrompt(cfg, gi.System, gi.Temperature, gi.Tools, gi.Transcript)
				if legacy {
					p = flattenAnthropicToPrompt(in)
				}
				dump(fmt.Sprintf("t%02d_i%02d_prompt.txt", turn, iter), p)
				fmt.Printf("  dumped prompt (%d chars) and stopped (--dump-only)\n", len(p))
				return nil
			}
			t0 := time.Now()
			var resp responsesResponse
			var trace *agyGenTrace
			var gerr error
			if viaProxy != "" {
				// Exercise the real HTTP path (pool workers, guards, converters)
				// exactly as Connect does.
				resp, gerr = simPostAnthropic(viaProxy, cfg, in)
			} else if legacy {
				dump(fmt.Sprintf("t%02d_i%02d_prompt.txt", turn, iter), flattenAnthropicToPrompt(in))
				resp, gerr = agyLegacyAnthropicResponse(context.Background(), cfg, in, firstNonEmpty(model, "agy"), inputTokens)
			} else {
				resp, trace, gerr = agyGenerate(context.Background(), cfg, agyGenInputFromAnthropic(in, inputTokens))
				if trace != nil {
					dump(fmt.Sprintf("t%02d_i%02d_prompt.txt", turn, iter), trace.FinalPrompt)
					var b strings.Builder
					for ai, a := range trace.Attempts {
						fmt.Fprintf(&b, "==== attempt %d (%dms) note=%q problems=%v\n%s\n\n", ai+1, a.DurationMs, a.Note, a.Problems, a.Raw)
					}
					dump(fmt.Sprintf("t%02d_i%02d_raw.txt", turn, iter), b.String())
				}
			}
			totalCalls++
			if gerr != nil {
				fmt.Printf("  [iter %d] ERROR after %s: %v\n", iter, time.Since(t0).Round(time.Millisecond), gerr)
				return gerr
			}
			attempts := 1
			usage := ""
			if trace != nil {
				attempts = len(trace.Attempts)
				var in, out, think int
				for _, a := range trace.Attempts {
					in += a.Usage.InputTokens
					out += a.Usage.OutputTokens
					think += a.Usage.ThinkingTokens
				}
				if in > 0 || out > 0 {
					usage = fmt.Sprintf(" in=%d out=%d think=%d", in, out, think)
				}
			} else if resp.Usage.InputTokens > 0 {
				usage = fmt.Sprintf(" in=%d out=%d", resp.Usage.InputTokens, resp.Usage.OutputTokens)
			}
			replyText := agyResponseText(resp)
			var calls []responsesOutputItem
			for _, item := range resp.Output {
				if item.Type == "function_call" {
					calls = append(calls, item)
				}
			}
			fmt.Printf("  [iter %d] %s attempts=%d%s tool_calls=%d text=%q\n", iter, time.Since(t0).Round(time.Millisecond), attempts, usage, len(calls), truncateString(replyText, 300))
			for _, c := range calls {
				fmt.Printf("      → %s %s\n", c.Name, truncateString(c.Arguments, 300))
			}

			if len(calls) == 0 {
				messages = append(messages, anthropicMessage{Role: "assistant", Content: replyText})
				lastReply = replyText
				replied = true
				break
			}

			// Assistant turn with tool_use blocks (+ any text), then the results.
			var blocks []any
			if strings.TrimSpace(replyText) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": replyText})
			}
			var results []any
			for _, c := range calls {
				var argsMap map[string]any
				var argsAny any
				if json.Unmarshal([]byte(c.Arguments), &argsAny) == nil {
					argsMap, _ = argsAny.(map[string]any)
				}
				if argsMap == nil {
					argsMap = map[string]any{}
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.CallID, "name": c.Name, "input": argsMap})
				result := simExecute(sc, preflighted, strict, c.Name, argsMap)
				results = append(results, map[string]any{"type": "tool_result", "tool_use_id": c.CallID, "content": result})
				allCalls = append(allCalls, simToolCall{Turn: turn, Iter: iter, Name: c.Name, Args: canonicalToolArgs(argsMap)})
				fmt.Printf("      ← %s result: %s\n", c.Name, truncateString(result, 200))
				if sc.StopAfter != "" && c.Name == sc.StopAfter && !strings.Contains(result, `"skipped":true`) {
					stopHit = true
				}
			}
			messages = append(messages, anthropicMessage{Role: "assistant", Content: blocks})
			messages = append(messages, anthropicMessage{Role: "user", Content: results})
			turnResults = append(turnResults, len(messages)-1)
		}
		if !replied {
			loopFailures++
			fmt.Printf("  !! turn %d did not produce a customer reply within %d iterations (LOOP)\n", turn, sc.MaxIterations)
		}
		// Connect persists tool results as {"name","result"} and replays them that way in later turns.
		for _, mi := range turnResults {
			blocks, _ := messages[mi].Content.([]any)
			for _, b := range blocks {
				m, _ := b.(map[string]any)
				if m == nil {
					continue
				}
				name := ""
				for _, prev := range messages[:mi] {
					if pb, ok := prev.Content.([]any); ok {
						for _, x := range pb {
							if xm, ok := x.(map[string]any); ok && xm["type"] == "tool_use" && xm["id"] == m["tool_use_id"] {
								name, _ = xm["name"].(string)
							}
						}
					}
				}
				var res any
				_ = json.Unmarshal([]byte(fmt.Sprint(m["content"])), &res)
				wrapped, _ := json.Marshal(map[string]any{"name": name, "result": res})
				m["content"] = string(wrapped)
			}
		}
		if stopHit && replied {
			fmt.Printf("--- stop_after %s reached and answered\n", sc.StopAfter)
			break
		}
	}

	fmt.Printf("\n=== SUMMARY (%s, %d agy generations)\n", time.Since(start).Round(time.Second), totalCalls)
	counts := map[string]int{}
	for _, c := range allCalls {
		counts[c.Name]++
	}
	for _, c := range allCalls {
		fmt.Printf("  turn %d iter %d: %s %s\n", c.Turn, c.Iter, c.Name, truncateString(c.Args, 220))
	}
	fmt.Printf("  tool call counts: %v\n", counts)
	fmt.Printf("  loop failures (turn without reply): %d\n", loopFailures)
	if sc.StopAfter != "" {
		fmt.Printf("  %s executed: %v\n", sc.StopAfter, stopHit)
	}
	return nil
}
