package main

import "strings"

// ───────────────────────────────────────────────────────────────────────────
// Codex "fast mode" (OpenAI's priority processing)
//
// What it actually is on THIS backend
// -----------------------------------
// The proxy talks to chatgpt.com/backend-api/codex with a ChatGPT subscription
// token, NOT to the pay-as-you-go platform API. Probing that backend directly
// (2026-08-06) settled how fast mode behaves here:
//
//	service_tier omitted    → HTTP 200
//	service_tier="priority" → HTTP 200
//	service_tier="fast"     → HTTP 400 {"detail":"Unsupported service_tier: fast"}
//
// So although OpenAI renamed priority processing to "Fast mode" on the developer
// platform (2026-07-30), the ChatGPT backend still only accepts the LEGACY wire
// value "priority". Sending the literal "fast" is a hard 400. CODEX_FAST_SERVICE_TIER
// therefore defaults to "priority" and should not be changed without re-probing.
//
// Per-model support: the ChatGPT model catalog (~/.codex/models_cache.json)
// advertises fast mode per model as
//
//	service_tiers: [{id: "priority", name: "Fast", description: "1.5x speed, increased usage"}]
//
// and the Codex CLI drops the field for any model that does not advertise it
// ("Configured service tier `priority` is not advertised as supported for model
// `gpt-5.3-codex-spark` and will be omitted from requests."). We mirror that: an
// unsupported model never gets service_tier, so we neither waste a round-trip
// nor rely on the 400-retry stripping it.
//
// Why it is OFF by default and never automatic
// --------------------------------------------
// "1.5x speed, INCREASED USAGE" — fast mode burns the ChatGPT plan's quota
// faster. That is a real cost, so it is never inferred: there is no "auto" mode
// and no model-name magic. It ships DISABLED and only ever turns on because a
// human flipped PROXY_CODEX_FAST_MODE=on (globally) or a caller explicitly asked
// for it per request (Connect's per-service / per-gateway-key toggles, which
// send Anthropic `speed`). Every applied and every suppressed request is
// recorded in requestStat.ServiceTier so usage can be audited after the fact.
// ───────────────────────────────────────────────────────────────────────────

const (
	// fastModeOff is the DEFAULT: never send service_tier, whatever any caller
	// asks for. The global kill switch.
	fastModeOff = "off"
	// fastModeOn enables fast mode. Individual requests still decide: an
	// explicit `speed` on the request wins, and a request that says nothing
	// gets fast mode (the operator turned it on for everything).
	fastModeOn = "on"
)

// defaultCodexFastTier is the ONLY value the ChatGPT codex backend accepts.
// See the probe results above before changing this.
const defaultCodexFastTier = "priority"

// defaultCodexFastModels are the upstream models whose ChatGPT catalog entry
// advertises service_tiers=[priority] (verified on the live account 2026-08-06).
// Override with PROXY_CODEX_FAST_MODELS; set it to "*" to skip the gate.
var defaultCodexFastModels = []string{
	"gpt-5.4",
	"gpt-5.5",
	"gpt-5.6-luna",
	"gpt-5.6-sol",
	"gpt-5.6-sol-wm",
	"gpt-5.6-terra",
	"codex-auto-review",
}

// defaultClaudeFastModels is EMPTY on purpose. An earlier build treated the
// alias claude-opus-4-6 as an implicit request for fast mode, so traffic could
// silently start costing 1.5x quota just by naming a model. Fast mode is now
// manual only. PROXY_CLAUDE_FAST_MODELS can restore an implicit trigger.
var defaultClaudeFastModels = []string{}

// normalizeFastMode maps PROXY_CODEX_FAST_MODE to on/off. Anything unrecognised
// (including "" and the retired "auto") is OFF — the safe, non-spending default.
func normalizeFastMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "1", "true", "yes", "enabled", "always", "force", "priority", "fast":
		return fastModeOn
	default:
		return fastModeOff
	}
}

// parseFastModelList splits a comma/space separated allowlist. An empty string
// yields fallback; "*" yields nil, which callers read as "no gate"; "none"
// yields an empty (but non-nil) list, which matches nothing.
func parseFastModelList(s string, fallback []string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	if s == "*" {
		return nil
	}
	if strings.EqualFold(s, "none") {
		return []string{}
	}
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, strings.ToLower(part))
		}
	}
	return out
}

// fastModeAsk is a caller's per-request position on fast mode. Tri-state,
// because "said nothing" and "explicitly said no" must behave differently once
// the operator has switched fast mode on globally.
type fastModeAsk int

const (
	fastAskUnset fastModeAsk = iota // request carried no opinion
	fastAskOn                       // speed:"fast" — Connect toggle ON
	fastAskOff                      // speed:"standard" — Connect toggle OFF
)

// String renders the ask for trace logs, where a bare 0/1/2 would be unreadable.
func (a fastModeAsk) String() string {
	switch a {
	case fastAskOn:
		return "fast"
	case fastAskOff:
		return "standard"
	default:
		return "unset"
	}
}

// parseFastModeAsk reads the Anthropic `speed` field. Unknown values are
// treated as no opinion rather than as an enable.
func parseFastModeAsk(speed string) fastModeAsk {
	switch strings.ToLower(strings.TrimSpace(speed)) {
	case "fast", "priority":
		return fastAskOn
	case "standard", "default", "normal", "slow", "off", "none":
		return fastAskOff
	default:
		return fastAskUnset
	}
}

// fastModeDecision is the full, auditable outcome of the fast-mode question for
// one request: what (if anything) goes on the wire, and why.
type fastModeDecision struct {
	// Tier is the service_tier to send, or "" to omit the field entirely.
	Tier string
	// Requested is true when fast mode was wanted — by the caller or by the
	// global switch — regardless of whether it survived the gates.
	Requested bool
	// Reason explains a suppressed request: "disabled" (global kill switch),
	// "caller_opted_out", or "model_unsupported". Empty when nothing was
	// suppressed.
	Reason string
}

// Suppressed reports a request for fast mode that did not make it onto the wire.
func (d fastModeDecision) Suppressed() bool { return d.Requested && d.Tier == "" }

// Note renders the decision for the request log, or "" when fast mode was
// neither wanted nor applied (the common case, kept silent so ordinary traffic
// notes are unchanged).
func (d fastModeDecision) Note() string {
	switch {
	case d.Tier != "":
		return " service_tier=" + d.Tier
	case d.Suppressed():
		return " fast_mode=suppressed(" + d.Reason + ")"
	default:
		return ""
	}
}

// codexModelSupportsFastMode reports whether the resolved UPSTREAM model
// advertises priority processing. A nil allowlist disables the gate.
func codexModelSupportsFastMode(cfg config, model string) bool {
	if cfg.CodexFastModels == nil {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	for _, allowed := range cfg.CodexFastModels {
		if model == allowed {
			return true
		}
	}
	return false
}

// Live master-switch states. Deliberately tri-state: once the switch has been
// seeded it is AUTHORITATIVE in both directions, so turning fast mode off in the
// control panel really stops the spend even though the handlers still hold the
// config value captured at startup. Zero (unseeded) falls back to cfg, which is
// what unit tests building a bare config literal rely on.
const (
	fastSwitchUnseeded int32 = 0
	fastSwitchOn       int32 = 1
	fastSwitchOff      int32 = 2
)

// setFastModeSwitch publishes the effective master switch to the request path.
func setFastModeSwitch(on bool) {
	if on {
		codexFastModeSwitch.Store(fastSwitchOn)
		return
	}
	codexFastModeSwitch.Store(fastSwitchOff)
}

// fastModeEnabled reports the effective master switch.
func fastModeEnabled(cfg config) bool {
	switch codexFastModeSwitch.Load() {
	case fastSwitchOn:
		return true
	case fastSwitchOff:
		return false
	default:
		return normalizeFastMode(cfg.CodexFastMode) == fastModeOn
	}
}

// codexFastMode resolves fast mode for one outbound codex request.
// ask is the caller's per-request position; model is the RESOLVED upstream
// model name, because the catalog gate is upstream-side.
//
// Truth table (mode × ask):
//
//	off × anything → omitted        (global kill switch always wins)
//	on  × unset    → priority       (operator enabled it for everything)
//	on  × on       → priority
//	on  × off      → omitted        (this caller opted out)
func codexFastMode(cfg config, ask fastModeAsk, model string) fastModeDecision {
	if !fastModeEnabled(cfg) {
		if ask == fastAskOn {
			// Someone flipped a Connect toggle on while the proxy-side switch
			// is off. Record it so the mismatch is visible instead of silent.
			return fastModeDecision{Requested: true, Reason: "disabled"}
		}
		return fastModeDecision{}
	}
	if ask == fastAskOff {
		return fastModeDecision{Requested: true, Reason: "caller_opted_out"}
	}
	if !codexModelSupportsFastMode(cfg, model) {
		return fastModeDecision{Requested: true, Reason: "model_unsupported"}
	}

	tier := strings.TrimSpace(cfg.CodexFastTier)
	if tier == "" {
		tier = defaultCodexFastTier
	}
	return fastModeDecision{Tier: tier, Requested: true}
}
