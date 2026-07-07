package main

// cameraorch.go (part 1 of the orchestration brain) — the backend-agnostic AI
// bridge and the decision protocol shared by the setup/describe phase (WP7) and
// the escalation loop (WP8, both in SEPARATE files).
//
// This file owns exactly three things:
//
//  1. analyzeWithAlias — ONE function that takes a model alias, a system prompt,
//     a user instruction, and a set of image/clip file paths, resolves the alias
//     to its backend (agy | claude | codex) via forwardForAlias, and runs a
//     vision analysis on that backend. Every backend is supported and the caller
//     never has to know which one it hit — that is the whole point (the user's
//     hard requirement that analysis work on all three).
//
//  2. The decision JSON schema (decision/perCameraObs/needMore/clipWindow/
//     triggeredAction) that the monitoring loop asks the model to emit.
//
//  3. A tolerant parser (extractFirstJSONObject + parseDecision) that pulls that
//     JSON out of whatever prose/fences a text model wraps around it, decodes it
//     defensively (stringified numbers/bools are accepted), normalizes the enums
//     to safe defaults, and drops any camera ids the model hallucinated.
//
// OBSERVABILITY: every analysis logs its backend, prompt size and latency via
// camlog. NO credentials ever flow through here (only local capture file paths),
// so there is nothing to mask — but we still never log raw image bytes.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────── decision schema ───────────────────────────────

// decision is the strict single-object JSON contract the analysis model must emit
// each round. Field types are plain (bool/float64/int) for easy downstream use;
// tolerance for stringified numbers/booleans lives in the custom UnmarshalJSON
// helpers below, so callers read d.Final / d.Confidence directly.
type decision struct {
	RoundSummary     string            `json:"round_summary"`
	PerCamera        []perCameraObs    `json:"per_camera"`
	Suspicion        string            `json:"suspicion"`   // none | low | medium | high
	RuleStatus       string            `json:"rule_status"` // not_met | possibly_met | met
	NeedMore         needMore          `json:"need_more"`
	TriggeredActions []triggeredAction `json:"triggered_actions"`
	Final            bool              `json:"final"`
	Confidence       float64           `json:"confidence"`
}

// perCameraObs is the model's per-camera observation for the round.
type perCameraObs struct {
	CameraID    string `json:"camera_id"`
	Observation string `json:"observation"`
	Notable     bool   `json:"notable"`
}

// needMore is the model's escalation request: what additional evidence it wants
// before it can decide. Type "none" ends the ladder for this round.
type needMore struct {
	Type      string      `json:"type"` // none | full_images | clip
	CameraIDs []string    `json:"camera_ids"`
	Clip      *clipWindow `json:"clip"` // set only when Type == "clip"
}

// clipWindow is a relative time window ("before 3 minutes until now" ->
// from_offset_sec=-180, to_offset_sec=0). Offsets are seconds relative to "now".
// Custom UnmarshalJSON tolerates float/string encodings from text models.
type clipWindow struct {
	FromOffsetSec int `json:"from_offset_sec"`
	ToOffsetSec   int `json:"to_offset_sec"`
}

// triggeredAction is one action the model wants dispatched (only honored when the
// run's rule_status is "met"; the loop, not the model, is the final gate).
type triggeredAction struct {
	Action    string   `json:"action"`
	Severity  string   `json:"severity"`
	Reason    string   `json:"reason"`
	CameraIDs []string `json:"camera_ids"`
}

// UnmarshalJSON decodes a decision through an auxiliary shape whose finicky
// numeric/boolean fields use tolerant types, then copies into the plain-typed
// decision. This lets the parser survive a model that emits `"final":"true"` or
// `"confidence":"0.8"` instead of failing the whole round.
func (d *decision) UnmarshalJSON(b []byte) error {
	var aux struct {
		RoundSummary     string            `json:"round_summary"`
		PerCamera        []perCameraObs    `json:"per_camera"`
		Suspicion        string            `json:"suspicion"`
		RuleStatus       string            `json:"rule_status"`
		NeedMore         needMore          `json:"need_more"`
		TriggeredActions []triggeredAction `json:"triggered_actions"`
		Final            flexBool          `json:"final"`
		Confidence       flexFloat         `json:"confidence"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	d.RoundSummary = aux.RoundSummary
	d.PerCamera = aux.PerCamera
	d.Suspicion = aux.Suspicion
	d.RuleStatus = aux.RuleStatus
	d.NeedMore = aux.NeedMore
	d.TriggeredActions = aux.TriggeredActions
	d.Final = bool(aux.Final)
	d.Confidence = float64(aux.Confidence)
	return nil
}

// UnmarshalJSON tolerates clip offsets given as JSON numbers, floats, or strings.
func (c *clipWindow) UnmarshalJSON(b []byte) error {
	var aux struct {
		From flexInt `json:"from_offset_sec"`
		To   flexInt `json:"to_offset_sec"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	c.FromOffsetSec = int(aux.From)
	c.ToOffsetSec = int(aux.To)
	return nil
}

// ─────────────────────── tolerant scalar decoders ───────────────────────

// flexInt decodes an int from a JSON number, float, or numeric string ("180",
// "180.0"). Blank/null decode to 0. Floats are truncated toward zero.
type flexInt int

func (n *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*n = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*n = 0
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		*n = flexInt(int(f))
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*n = flexInt(int(f))
	return nil
}

// flexFloat decodes a float64 from a JSON number or numeric string.
type flexFloat float64

func (v *flexFloat) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*v = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*v = 0
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		*v = flexFloat(f)
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*v = flexFloat(f)
	return nil
}

// flexBool decodes a bool from a JSON bool, "true"/"false"/"yes"/"1" strings, or
// a number (nonzero -> true). Blank/null decode to false.
type flexBool bool

func (v *flexBool) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch string(b) {
	case "", "null", "false":
		*v = false
		return nil
	case "true":
		*v = true
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "y":
			*v = true
		default:
			*v = false
		}
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*v = f != 0
	return nil
}

// ─────────────────────────── enum normalization ───────────────────────────

var (
	camSuspicionLevels = map[string]bool{"none": true, "low": true, "medium": true, "high": true}
	camRuleStatuses    = map[string]bool{"not_met": true, "possibly_met": true, "met": true}
	camNeedMoreTypes   = map[string]bool{"none": true, "full_images": true, "clip": true}
)

// camNormalizeEnum lowercases/trims v and, if it is blank OR not a recognized
// member of allowed, returns the safe default def. Coercing unknown values to a
// safe default (low suspicion, not_met rule status, none need-more) guarantees a
// garbled enum can never over-alert or fire an action.
func camNormalizeEnum(v string, allowed map[string]bool, def string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || !allowed[v] {
		return def
	}
	return v
}

// ─────────────────────── robust JSON object extraction ───────────────────────

// extractFirstJSONObject returns the first balanced, string/escape-aware `{...}`
// span in s that is itself valid JSON, tolerating ```json fences and arbitrary
// leading/trailing prose. It scans for candidate '{' positions and returns the
// first whose brace-matched span parses — so a stray prose brace like "{maybe}"
// is skipped in favor of the real object. Returns (nil,false) when none is found.
func extractFirstJSONObject(s string) ([]byte, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		if span, ok := scanBalancedJSONObject(s, i); ok && json.Valid(span) {
			return span, true
		}
	}
	return nil, false
}

// scanBalancedJSONObject returns the byte span from the '{' at start to its
// matching '}', honoring JSON string quoting and backslash escapes (so braces or
// quotes inside a string never affect the depth count). ok is false if the object
// never closes. Byte-scanning is safe: all structural characters are ASCII and
// UTF-8 continuation bytes are >= 0x80, so multibyte runes cannot be misread.
func scanBalancedJSONObject(s string, start int) ([]byte, bool) {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return []byte(s[start : i+1]), true
			}
		}
	}
	return nil, false
}

// parseDecision extracts and decodes one decision object from a model's raw text
// output, then normalizes it: enum fields are coerced to safe defaults, camera
// ids not present in `allowed` are dropped (from need_more and each triggered
// action), and confidence is clamped to [0,1]. A non-nil allowed map enables the
// id filter; a nil map disables it (used where the caller has no id whitelist).
func parseDecision(raw string, allowed map[string]bool) (decision, error) {
	obj, ok := extractFirstJSONObject(raw)
	if !ok {
		return decision{}, errors.New("no JSON object found in model output")
	}
	var d decision
	if err := json.Unmarshal(obj, &d); err != nil {
		return decision{}, fmt.Errorf("decode decision JSON: %w", err)
	}
	d.Suspicion = camNormalizeEnum(d.Suspicion, camSuspicionLevels, "low")
	d.RuleStatus = camNormalizeEnum(d.RuleStatus, camRuleStatuses, "not_met")
	d.NeedMore.Type = camNormalizeEnum(d.NeedMore.Type, camNeedMoreTypes, "none")
	if allowed != nil {
		d.NeedMore.CameraIDs = camFilterAllowed(d.NeedMore.CameraIDs, allowed)
		for i := range d.TriggeredActions {
			d.TriggeredActions[i].CameraIDs = camFilterAllowed(d.TriggeredActions[i].CameraIDs, allowed)
		}
	}
	if d.Confidence < 0 {
		d.Confidence = 0
	}
	if d.Confidence > 1 {
		d.Confidence = 1
	}
	return d, nil
}

// camFilterAllowed keeps only trimmed, non-empty ids present in allowed, in order.
func camFilterAllowed(ids []string, allowed map[string]bool) []string {
	if len(ids) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && allowed[id] {
			out = append(out, id)
		}
	}
	return out
}

// ─────────────────────────────── AI bridge ───────────────────────────────

var (
	// errCameraAnalysisBackend means the resolved backend has no usable camera
	// analysis path (unknown backend, or the upstream is switched off).
	errCameraAnalysisBackend = errors.New("camera analysis backend unavailable")
	// errCameraVisionUnsupported means images were supplied but the backend could
	// not carry them (e.g. codex rejected the image input). Camera analysis is
	// inherently visual, so the caller must surface this as a clean run error.
	errCameraVisionUnsupported = errors.New("camera analysis needs vision but the backend could not carry the images")
)

// analyzeWithAlias runs one vision analysis for a model alias and returns the
// model's raw text response (the caller parses it — with parseDecision for the
// loop, or its own schema for the describe phase). It resolves the alias to a
// backend and dispatches to the matching branch; all three (agy/claude/codex)
// carry the systemPrompt, userText, and the image/clip files at imagePaths.
//
// imagePaths may mix still images and .mp4/.mov clips: agy reads clips directly
// off disk, while claude/codex have clips sampled into a handful of JPEG frames
// first (see camExpandInlineImages). A file's label is its base name without
// extension, so callers name each capture after its camera id to get a clean
// "camera <id>" reference the model can cite.
func analyzeWithAlias(ctx context.Context, cfg config, alias, systemPrompt, userText string, imagePaths []string) (string, error) {
	modelConfigMu.RLock()
	back := forwardForAlias(cfg, alias)
	modelConfigMu.RUnlock()

	start := time.Now()
	camlog("info", "analyze", map[string]any{
		"alias":     alias,
		"backend":   back,
		"images":    len(imagePaths),
		"sys_bytes": len(systemPrompt),
		"txt_bytes": len(userText),
	})

	var (
		out string
		err error
	)
	switch back {
	case "agy":
		out, err = analyzeWithAgy(ctx, cfg, alias, systemPrompt, userText, imagePaths)
	case "claude":
		out, err = analyzeWithClaude(ctx, cfg, alias, systemPrompt, userText, imagePaths)
	case "codex":
		out, err = analyzeWithCodex(ctx, cfg, alias, systemPrompt, userText, imagePaths)
	default:
		err = fmt.Errorf("%w: backend %q has no camera analysis path", errCameraAnalysisBackend, back)
	}

	level := "info"
	if err != nil {
		level = "error"
	}
	camlog(level, "analyze_done", map[string]any{
		"alias":      alias,
		"backend":    back,
		"ok":         err == nil,
		"images":     len(imagePaths),
		"resp_bytes": len(out),
		"latency_ms": time.Since(start).Milliseconds(),
		"error":      camErrStr(err),
	})
	return out, err
}

// ─────────────────────── resilient investigation analysis ───────────────────────
//
// A camera investigation runs DETACHED on the background worker, so an analysis
// call that times out or hits a transient upstream fault must never terminalize
// the run: nobody is blocked on an HTTP response, and the alternative is an
// "exhausted" investigation the operator has to resume by hand ("كمل"). This
// section wraps analyzeWithAlias in a never-die ladder — raise the per-attempt
// timeout, retry transient faults, fail over to backup aliases — and classifies
// each error so the ladder knows whether to retry, jump aliases, or give up.

// camAnalyzeErrClass buckets an analysis error by how the resilient ladder should
// react: retry the same alias, skip to the next alias, abort the whole ladder
// (the run's own context died), or give up (a permanent, non-retryable fault).
type camAnalyzeErrClass int

const (
	camErrPermanent camAnalyzeErrClass = iota // non-retryable fault — abandon this alias, move on
	camErrRetryable                           // transient (timeout / 5xx / overload / empty output) — retry the same alias
	camErrSkipAlias                           // alias structurally can't help (no vision path / backend off) — jump to the next alias
	camErrCancelled                           // the run's own context is dead — abort; every retry would fail identically
)

// camRetryableAnalyzeSubstrings are the lowercased markers of a transient analysis
// failure worth retrying. Unlike claudeTransient (claude.go:308) we DELIBERATELY
// retry the proxy's own deadline strings ("timed out after Ns"): the run is
// detached, so burning another attempt to reach an answer beats terminalizing.
var camRetryableAnalyzeSubstrings = []string{
	"timed out after", "produced no output", "no output", "returned no text",
	"internal server error", "api error: 5", "server-side issue",
	"overloaded", "529", "503", "502", "service unavailable", "bad gateway",
	"rate limit", "429", "too many requests",
	"connection reset", "connection refused", "econnreset",
}

// camClassifyAnalyzeErr buckets a non-nil analysis error for the resilient ladder.
// The run-context check wins first (a dead ctx means every retry fails the same
// way), then the typed sentinels (errCameraVisionUnsupported / -AnalysisBackend
// mean THIS alias structurally can't carry the request — jump aliases, don't
// retry), then the transient-substring match, else permanent.
func camClassifyAnalyzeErr(ctx context.Context, err error) camAnalyzeErrClass {
	if err == nil {
		return camErrPermanent // callers only classify non-nil errors; defensive
	}
	if ctx != nil && ctx.Err() != nil {
		return camErrCancelled
	}
	if errors.Is(err, errCameraVisionUnsupported) || errors.Is(err, errCameraAnalysisBackend) {
		return camErrSkipAlias
	}
	msg := strings.ToLower(err.Error())
	for _, s := range camRetryableAnalyzeSubstrings {
		if strings.Contains(msg, s) {
			return camErrRetryable
		}
	}
	return camErrPermanent
}

// camAnalyzeAttempt records one rung of the resilient ladder for logging and for
// the post-success "analysis retried" transcript note.
type camAnalyzeAttempt struct {
	Alias   string
	Err     string // "" on the successful attempt
	Elapsed time.Duration
}

// camAnalyzeFunc is the analyze call the resilient ladder drives; real runs use
// analyzeWithAlias, tests inject a fake to exercise retry/failover without a live
// backend.
type camAnalyzeFunc func(ctx context.Context, cfg config, alias, sys, user string, images []string) (string, error)

// camAnalyzeTimeout resolves the per-ATTEMPT floor the ladder raises each backend
// timeout to, so one detached analysis attempt gets a generous deadline (a
// multi-image vision turn legitimately takes minutes).
func camAnalyzeTimeout(cfg config) time.Duration {
	d := cfg.CameraAnalyzeTimeout
	if d <= 0 {
		d = 600 * time.Second
	}
	return d
}

// camInvestigateFailoverAliases resolves the ordered fallback vision aliases the
// ladder tries once the primary alias is exhausted (CSV, trimmed, empties
// dropped). Empty when unset — the ladder then retries the primary alias alone.
func camInvestigateFailoverAliases(cfg config) []string {
	var out []string
	for _, a := range strings.Split(cfg.CameraInvestigateFailoverCSV, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// camDedupAliases returns the ordered, unique, non-empty aliases the ladder walks:
// the primary first, then each failover that isn't already listed. Always yields
// at least one rung (an empty primary still attempts once — analyzeWithAlias then
// resolves the backend default).
func camDedupAliases(primary string, failovers []string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(a string) {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			return
		}
		seen[a] = true
		out = append(out, a)
	}
	add(primary)
	for _, a := range failovers {
		add(a)
	}
	if len(out) == 0 {
		out = append(out, strings.TrimSpace(primary)) // preserve one rung even if it is ""
	}
	return out
}

// camResilientAnalyze runs ONE investigation analysis through the full never-die
// ladder: it raises every backend timeout to the per-attempt floor, then tries the
// primary alias plus any configured failover aliases, retrying transient faults on
// the same alias and jumping to the next alias on a structural one. It returns the
// model text on the first success, the per-attempt trail (for a budget-free
// "analysis retried" transcript note), and a wrapped error only when every rung
// failed. Delegates to the injectable variant so tests can drive it with a fake.
func camResilientAnalyze(ctx context.Context, cfg config, alias, sys, user string, images []string) (string, []camAnalyzeAttempt, error) {
	return camResilientAnalyzeWith(ctx, cfg, alias, sys, user, images, analyzeWithAlias)
}

// camResilientAnalyzeWith is camResilientAnalyze with the analyze call injected.
func camResilientAnalyzeWith(ctx context.Context, cfg config, alias, sys, user string, images []string, analyze camAnalyzeFunc) (string, []camAnalyzeAttempt, error) {
	// Raise each backend's per-call timeout to the analysis floor on a LOCAL cfg
	// copy (only ever raise, never lower) so one detached attempt gets minutes —
	// the same idiom analyzeWithClaude uses for ClaudeTimeout, but for all three
	// backends at once, since the ladder can fail over across backends.
	acfg := cfg
	floor := camAnalyzeTimeout(cfg)
	if acfg.AgyTimeout < floor {
		acfg.AgyTimeout = floor
	}
	if acfg.AgyMediaTimeout < floor {
		acfg.AgyMediaTimeout = floor
	}
	if acfg.ClaudeTimeout < floor {
		acfg.ClaudeTimeout = floor
	}

	aliases := camDedupAliases(alias, camInvestigateFailoverAliases(cfg))
	const attemptsPerAlias = 2

	var (
		attempts []camAnalyzeAttempt
		lastErr  error
		total    int
	)
	for _, al := range aliases {
	attemptLoop:
		for n := 1; n <= attemptsPerAlias; n++ {
			total++
			if n > 1 {
				// Exponential backoff (reuse claude.go:343) between same-alias
				// retries; a dead run context aborts the wait and the ladder.
				if !claudeBackoffWait(ctx, n-1) {
					if lastErr == nil {
						lastErr = ctx.Err()
					}
					return "", attempts, lastErr
				}
			}
			astart := time.Now()
			out, err := analyze(ctx, acfg, al, sys, user, images)
			elapsed := time.Since(astart)
			if err == nil {
				attempts = append(attempts, camAnalyzeAttempt{Alias: al, Elapsed: elapsed})
				return out, attempts, nil
			}
			class := camClassifyAnalyzeErr(ctx, err)
			attempts = append(attempts, camAnalyzeAttempt{Alias: al, Err: camErrStr(err), Elapsed: elapsed})
			lastErr = err
			camlog("warn", "investigate_analyze_retry", map[string]any{
				"alias": al, "attempt": n, "class": int(class), "error": camErrStr(err),
			})
			switch class {
			case camErrCancelled:
				return "", attempts, lastErr
			case camErrRetryable:
				continue // same alias again (until attemptsPerAlias)
			default: // camErrSkipAlias / camErrPermanent
				break attemptLoop // stop retrying this alias, fall to the next
			}
		}
	}
	if lastErr == nil {
		lastErr = errors.New("analysis produced no result")
	}
	return "", attempts, fmt.Errorf("analysis failed after %d attempt(s) across %d alias(es): %w", total, len(aliases), lastErr)
}

// camAnalyzeRetried reports whether the resilient ladder needed more than a clean
// first try — i.e. at least one attempt failed before the run eventually
// succeeded. Drives the budget-free "analysis retried" transcript breadcrumb.
func camAnalyzeRetried(attempts []camAnalyzeAttempt) bool {
	for _, a := range attempts {
		if a.Err != "" {
			return true
		}
	}
	return false
}

// analyzeWithAgy hands the capture files to agy via --add-dir (their parent dirs)
// and a prompt carrying a "camera <id> => <path>" label map — mirroring
// buildMediaPrompt. agy reads both images and .mp4 clips off disk itself.
func analyzeWithAgy(ctx context.Context, cfg config, alias, systemPrompt, userText string, imagePaths []string) (string, error) {
	if agyIsDisabled() {
		return "", fmt.Errorf("%w: %s", errCameraAnalysisBackend, upstreamDisabledMsg("agy"))
	}
	addDirs := camUniqueParentDirs(imagePaths)
	prompt := camBuildAgyPrompt(systemPrompt, userText, imagePaths)
	camlog("debug", "analyze_agy", map[string]any{"prompt_bytes": len(prompt), "add_dirs": len(addDirs), "images": len(imagePaths)})

	res, err := runAgyj(ctx, cfg, prompt, agyModelForRequest(cfg, alias, len(addDirs) > 0), addDirs)
	if err != nil {
		return "", fmt.Errorf("agy analysis failed: %w", err)
	}
	if !res.Ok {
		return "", fmt.Errorf("agy analysis error: %s", firstNonEmpty(res.Error, "unknown"))
	}
	text := strings.TrimSpace(res.Response)
	if text == "" {
		return "", errors.New("agy returned no text")
	}
	return text, nil
}

// cameraClaudeAnalyzeTimeout is the per-call floor for a claude camera-analysis
// run: a multi-image vision describe (16+ frames) or escalation round
// legitimately takes minutes, far longer than the chat-oriented
// cfg.ClaudeTimeout (180s default). It is applied via a LOCAL cfg copy so it
// never changes the global (inbound/Connect) claude chat timeout.
const cameraClaudeAnalyzeTimeout = 6 * time.Minute

// analyzeWithClaude inlines the capture files as base64 image blocks via
// claudeBuildInput and streams the run. Clips are pre-sampled to frames; images
// are downscaled to keep the total under claudeMaxInlineMediaBytes. The system
// prompt is folded into the text (the CLI has no separate system channel here)
// and an ordered "1. camera X" list correlates each attached image to its camera.
func analyzeWithClaude(ctx context.Context, cfg config, alias, systemPrompt, userText string, imagePaths []string) (string, error) {
	if claudeIsDisabled() {
		return "", fmt.Errorf("%w: %s", errCameraAnalysisBackend, upstreamDisabledMsg("claude"))
	}
	refs, cleanup, err := camExpandInlineImages(ctx, cfg, imagePaths)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return "", err
	}

	per := camPerImageBudget(len(refs))
	var parts []mediaPart
	for _, ref := range refs {
		b64, mt, ok := camEncodeImageForInline(ref.Path, per)
		if !ok {
			camlog("warn", "analyze_encode", map[string]any{"backend": "claude", "path": filepath.Base(ref.Path), "ok": false})
			continue
		}
		parts = append(parts, mediaPart{B64: b64, MediaType: mt, Filename: ref.Label})
	}

	prompt := camFoldSystemPrompt(systemPrompt, camComposeUserText(userText, refs))
	stdin, mediaIn, dropped := claudeBuildInput(prompt, parts)
	if len(refs) > 0 && !mediaIn {
		return "", fmt.Errorf("%w: no images could be inlined for claude", errCameraVisionUnsupported)
	}
	camlog("debug", "analyze_claude", map[string]any{
		"prompt_bytes": len(prompt), "inline_images": len(parts), "dropped_media": dropped, "media_in": mediaIn,
	})

	// Raise the per-call claude timeout to a generous floor for this camera
	// analysis so a multi-image describe/escalation round is not killed at the
	// 180s chat default. A local cfg copy keeps inbound claude traffic untouched.
	acfg := cfg
	if acfg.ClaudeTimeout < cameraClaudeAnalyzeTimeout {
		acfg.ClaudeTimeout = cameraClaudeAnalyzeTimeout
	}
	res, err := runClaudeStream(ctx, acfg, stdin, mediaIn, claudeModelFor(cfg, alias), nil)
	if err != nil {
		return "", fmt.Errorf("claude analysis failed: %w", err)
	}
	if !res.Ok {
		return "", fmt.Errorf("claude analysis error: %s", firstNonEmpty(res.Error, "unknown"))
	}
	text := strings.TrimSpace(res.Response)
	if text == "" {
		return "", errors.New("claude returned no text")
	}
	return text, nil
}

// analyzeWithCodex sends a Responses-API request with input_image data URLs (the
// codex vision path). Clips are pre-sampled to frames and images downscaled to a
// bounded budget. If the upstream rejects a request that carried images, the
// error is surfaced as errCameraVisionUnsupported so the run fails cleanly and the
// operator can pick a vision-capable alias (per the plan's codex-vision caveat).
func analyzeWithCodex(ctx context.Context, cfg config, alias, systemPrompt, userText string, imagePaths []string) (string, error) {
	if codexIsDisabled() {
		return "", fmt.Errorf("%w: %s", errCameraAnalysisBackend, upstreamDisabledMsg("codex"))
	}
	refs, cleanup, err := camExpandInlineImages(ctx, cfg, imagePaths)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return "", err
	}

	per := camPerImageBudget(len(refs))
	content := []map[string]any{{"type": "input_text", "text": camComposeUserText(userText, refs)}}
	inlined := 0
	for _, ref := range refs {
		dataURL, ok := camImageDataURL(ref.Path, per)
		if !ok {
			camlog("warn", "analyze_encode", map[string]any{"backend": "codex", "path": filepath.Base(ref.Path), "ok": false})
			continue
		}
		content = append(content, map[string]any{"type": "input_text", "text": ref.Label + ":"})
		content = append(content, map[string]any{"type": "input_image", "image_url": dataURL})
		inlined++
	}
	if len(refs) > 0 && inlined == 0 {
		return "", fmt.Errorf("%w: no images could be encoded for codex", errCameraVisionUnsupported)
	}

	req := responsesRequest{
		Model:        resolveModel(cfg, alias),
		Instructions: strings.TrimSpace(systemPrompt),
		Input:        []any{responseInput{Role: "user", Content: content}},
	}
	camlog("debug", "analyze_codex", map[string]any{"model": req.Model, "inline_images": inlined})

	resp, status, msg, err := callCodexResponses(ctx, cfg, req)
	if err != nil {
		detail := firstNonEmpty(strings.TrimSpace(msg), err.Error())
		if inlined > 0 {
			return "", fmt.Errorf("%w (codex status %d): %s", errCameraVisionUnsupported, status, detail)
		}
		return "", fmt.Errorf("codex analysis failed (status %d): %s", status, detail)
	}
	text := strings.TrimSpace(responsesText(resp))
	if text == "" {
		return "", errors.New("codex returned no text")
	}
	return text, nil
}

// ─────────────────────── media prep shared by the backends ───────────────────────

// camImageRef is one inline-ready still with a human/model-facing label.
type camImageRef struct {
	Label string // e.g. "camera front-door" or "camera front-door clip frame 2"
	Path  string // decodable still image on disk
}

// camExpandInlineImages turns imagePaths into a flat list of still-image refs
// suitable for inlining into claude/codex: still images pass through, while each
// clip is sampled to a few JPEG frames (via sampleClipFrames) in a scratch dir.
// The returned cleanup removes any scratch dirs and must be deferred by the
// caller. Sampling failures are logged and skipped (best-effort, never fatal).
func camExpandInlineImages(ctx context.Context, cfg config, imagePaths []string) ([]camImageRef, func(), error) {
	var (
		refs    []camImageRef
		tmpDirs []string
	)
	cleanup := func() {
		for _, d := range tmpDirs {
			_ = os.RemoveAll(d)
		}
	}
	for _, p := range imagePaths {
		if !camIsClip(p) {
			refs = append(refs, camImageRef{Label: "camera " + camImageLabel(p), Path: p})
			continue
		}
		dir, err := os.MkdirTemp("", "camframes-")
		if err != nil {
			camlog("warn", "analyze_frames", map[string]any{"clip": filepath.Base(p), "ok": false, "error": err.Error()})
			continue
		}
		tmpDirs = append(tmpDirs, dir)
		frames, ferr := sampleClipFrames(ctx, cfg, p, dir, 2, 8, 768)
		if ferr != nil {
			// sampleClipFrames already camlog'd the ffmpeg failure with stderr.
			continue
		}
		label := camImageLabel(p)
		for i, f := range frames {
			refs = append(refs, camImageRef{Label: fmt.Sprintf("camera %s clip frame %d", label, i+1), Path: f})
		}
	}
	return refs, cleanup, nil
}

// camPerImageBudget splits the total inline media budget across n images, with a
// floor so a very large fleet still yields usable (if small) thumbnails.
func camPerImageBudget(n int) int {
	if n <= 0 {
		return claudeMaxInlineMediaBytes
	}
	per := claudeMaxInlineMediaBytes / n
	if per < 64*1024 {
		per = 64 * 1024
	}
	return per
}

// camImageDataURL builds a `data:<type>;base64,<...>` URL for the Responses API
// input_image block, downscaling as needed to fit maxBytes.
func camImageDataURL(path string, maxBytes int) (string, bool) {
	b64, mt, ok := camEncodeImageForInline(path, maxBytes)
	if !ok {
		return "", false
	}
	return "data:" + mt + ";base64," + b64, true
}

// camEncodeImageForInline reads an image file and returns its base64 payload and
// media type, keeping the base64 length within maxBytes. If the raw file already
// fits it is used verbatim; otherwise the image is decoded and iteratively
// box-downscaled + JPEG re-encoded (dropping dimensions and quality) until it
// fits. Undecodable files are returned as-is (best-effort). ok is false only when
// the file can't be read or a re-encode produces nothing.
func camEncodeImageForInline(path string, maxBytes int) (string, string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return "", "", false
	}
	ct, _ := sniffImage(path)
	if ct == "" {
		ct = "image/jpeg"
	}
	if maxBytes <= 0 || base64.StdEncoding.EncodedLen(len(raw)) <= maxBytes {
		return base64.StdEncoding.EncodeToString(raw), ct, true
	}

	img, _, derr := image.Decode(bytes.NewReader(raw))
	if derr != nil {
		// Can't shrink an image we can't decode; hand back the raw bytes and let
		// the backend's own budget guard drop it if truly too large.
		return base64.StdEncoding.EncodeToString(raw), ct, true
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	quality := 82
	for i := 0; i < 10 && w >= 2 && h >= 2; i++ {
		scaled := boxDownscale(img, w, h)
		var buf bytes.Buffer
		if encErr := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: quality}); encErr != nil {
			break
		}
		if base64.StdEncoding.EncodedLen(buf.Len()) <= maxBytes {
			return base64.StdEncoding.EncodeToString(buf.Bytes()), "image/jpeg", true
		}
		w = w * 4 / 5
		h = h * 4 / 5
		if quality > 45 {
			quality -= 8
		}
	}
	// Last resort: a tiny low-quality encode so the camera is at least visible.
	scaled := boxDownscale(img, max(2, w), max(2, h))
	var buf bytes.Buffer
	if encErr := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: 40}); encErr == nil && buf.Len() > 0 {
		return base64.StdEncoding.EncodeToString(buf.Bytes()), "image/jpeg", true
	}
	return "", "", false
}

// camBuildAgyPrompt renders the agy prompt: the system prompt, a file label map
// (camera id => path, clips flagged), then the request line. Mirrors the intent
// of buildMediaPrompt (tell the model to open the files and answer only from
// their actual contents).
func camBuildAgyPrompt(systemPrompt, userText string, imagePaths []string) string {
	var b strings.Builder
	if s := strings.TrimSpace(systemPrompt); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	if len(imagePaths) > 0 {
		b.WriteString("Open and analyze the following camera file(s) directly ")
		b.WriteString("(use ffmpeg/ffprobe to read .mp4 clips frame by frame); base your answer ONLY on their actual contents:\n")
		for _, p := range imagePaths {
			b.WriteString("- camera ")
			b.WriteString(camImageLabel(p))
			b.WriteString(" => ")
			b.WriteString(p)
			if camIsClip(p) {
				b.WriteString(" (video clip)")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Request: ")
	if t := strings.TrimSpace(userText); t != "" {
		b.WriteString(t)
	} else {
		b.WriteString("Describe what each camera shows.")
	}
	return b.String()
}

// camComposeUserText prepends the user instruction to an ordered list mapping
// attachment position to camera label, so a model that only sees images in order
// (claude) can still say "camera front-door" unambiguously.
func camComposeUserText(userText string, refs []camImageRef) string {
	var b strings.Builder
	if t := strings.TrimSpace(userText); t != "" {
		b.WriteString(t)
		b.WriteString("\n\n")
	}
	if len(refs) > 0 {
		b.WriteString("The images are attached in this order:\n")
		for i, ref := range refs {
			fmt.Fprintf(&b, "%d. %s\n", i+1, ref.Label)
		}
	}
	return strings.TrimSpace(b.String())
}

// camFoldSystemPrompt joins a system prompt and user text into a single block for
// backends (claude CLI) that have no separate system channel here.
func camFoldSystemPrompt(sys, user string) string {
	sys = strings.TrimSpace(sys)
	user = strings.TrimSpace(user)
	switch {
	case sys == "":
		return user
	case user == "":
		return sys
	default:
		return sys + "\n\n" + user
	}
}

// camUniqueParentDirs returns the de-duplicated, sorted parent directories of the
// given paths (for agy's --add-dir scoping).
func camUniqueParentDirs(paths []string) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, p := range paths {
		d := filepath.Dir(p)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// camImageLabel is a capture file's camera label: its base name without extension
// (callers name each capture after its camera id).
func camImageLabel(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// camIsClip reports whether a path is a video clip (sampled to frames for the
// inline backends, read directly by agy).
func camIsClip(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".mov", ".mkv", ".avi", ".webm", ".ts", ".m4v":
		return true
	}
	return false
}

// camErrStr renders an error for a log field ("" when nil).
func camErrStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
