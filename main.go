package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	startedAt      = time.Now()
	proxyEnabled   atomic.Bool
	requestNotes   sync.Map
	requestStats   sync.Map
	logMu          sync.Mutex
	logRows        []uiLogRow
	traceMu        sync.Mutex
	validationMu   sync.Mutex
	lastValidation map[string]any
)

const (
	traceLogPath   = ".proxy.trace.log"
	traceBodyLimit = 32768
	traceMaxBytes  = 10 * 1024 * 1024
	sseMaxLineSize = 64 * 1024 * 1024
)

type config struct {
	OpenAIAPIKey    string
	OpenAIBaseURL   string
	Upstream        string
	CodexBaseURL    string
	CodexAuthFile   string
	ProxyKey        string
	Port            string
	Models          map[string]string
	ClaudeDefaults  map[string]string
	ReasoningEffort string
}

type codexAuthFile struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

type anthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens,omitempty"`
	System       any                    `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	Temperature  *float64               `json:"temperature,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
	Speed        string                 `json:"speed,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	FastMode     bool                   `json:"-"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicTool struct {
	Type           string   `json:"type,omitempty"`
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	InputSchema    any      `json:"input_schema,omitempty"`
	MaxUses        int      `json:"max_uses,omitempty"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	BlockedDomains []string `json:"blocked_domains,omitempty"`
	UserLocation   any      `json:"user_location,omitempty"`
}

type openAIRequest struct {
	Model               string          `json:"model"`
	Messages            []openAIMessage `json:"messages"`
	Tools               []openAITool    `json:"tools,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string        `json:"finish_reason"`
		Message      openAIMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		InputTokens  int `json:"prompt_tokens"`
		OutputTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error any `json:"error,omitempty"`
}

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content,omitempty"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Type     string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function,omitempty"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

type openAIChatCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int           `json:"index"`
		Message      openAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]int `json:"usage,omitempty"`
}

type responsesRequest struct {
	Model        string              `json:"model"`
	Instructions string              `json:"instructions,omitempty"`
	Input        []any               `json:"input"`
	Tools        []responsesTool     `json:"tools,omitempty"`
	Reasoning    *responsesReasoning `json:"reasoning,omitempty"`
	Temperature  *float64            `json:"temperature,omitempty"`
	ServiceTier  string              `json:"service_tier,omitempty"`
	Stream       bool                `json:"stream,omitempty"`
	Store        bool                `json:"store"`
}

type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responseInput struct {
	Role      string `json:"role,omitempty"`
	Content   any    `json:"content,omitempty"`
	Type      string `json:"type,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Output    string `json:"output,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type responsesTool struct {
	Type              string `json:"type"`
	Name              string `json:"name,omitempty"`
	Description       string `json:"description,omitempty"`
	Parameters        any    `json:"parameters,omitempty"`
	Filters           any    `json:"filters,omitempty"`
	UserLocation      any    `json:"user_location,omitempty"`
	SearchContextSize string `json:"search_context_size,omitempty"`
}

type responsesResponse struct {
	ID     string                `json:"id"`
	Model  string                `json:"model"`
	Output []responsesOutputItem `json:"output"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type responsesOutputItem struct {
	Type      string                   `json:"type"`
	Role      string                   `json:"role"`
	ID        string                   `json:"id"`
	CallID    string                   `json:"call_id"`
	Name      string                   `json:"name"`
	Arguments string                   `json:"arguments"`
	Content   []responsesOutputContent `json:"content"`
	Summary   []responsesReasoningPart `json:"summary,omitempty"`
}

type responsesOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesReasoningPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesStreamEvent struct {
	Type         string              `json:"type"`
	Delta        string              `json:"delta"`
	Text         string              `json:"text"`
	Arguments    string              `json:"arguments"`
	ItemID       string              `json:"item_id"`
	OutputIndex  int                 `json:"output_index"`
	SummaryIndex int                 `json:"summary_index"`
	Item         responsesOutputItem `json:"item"`
	Response     responsesResponse   `json:"response"`
}

type uiLogRow struct {
	AtUnixMS     int64  `json:"at"`
	TS           string `json:"ts"`
	Level        string `json:"lvl"`
	Method       string `json:"meth"`
	Path         string `json:"path"`
	Status       int    `json:"status"`
	DurMS        int64  `json:"dur"`
	IP           string `json:"ip"`
	Note         string `json:"note"`
	Model        string `json:"model,omitempty"`
	Upstream     string `json:"upstream,omitempty"`
	Stream       bool   `json:"stream,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
}

type requestStat struct {
	Model        string
	Upstream     string
	Stream       bool
	InputTokens  int
	OutputTokens int
	StopReason   string
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func main() {
	cfg := loadConfig()
	proxyEnabled.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", requireAuth(cfg, requireProxyEnabled(handleModels(cfg))))
	mux.HandleFunc("/v1/messages", requireAuth(cfg, requireProxyEnabled(handleMessages(cfg))))
	mux.HandleFunc("/v1/messages/count_tokens", requireAuth(cfg, requireProxyEnabled(handleCountTokens)))
	mux.HandleFunc("/v1/chat/completions", requireAuth(cfg, requireProxyEnabled(handleChatCompletions(cfg))))
	mux.HandleFunc("/v1/responses", requireAuth(cfg, requireProxyEnabled(handleResponses(cfg))))
	mux.HandleFunc("/ui/api/status", handleUIStatus(cfg))
	mux.HandleFunc("/ui/api/config", handleUIConfig(cfg))
	mux.HandleFunc("/ui/api/models", handleUIModels(cfg))
	mux.HandleFunc("/ui/api/validate", handleUIValidate(cfg))
	mux.HandleFunc("/ui/api/test", handleUITest(cfg))
	mux.HandleFunc("/ui/api/logs", handleUILogs)
	mux.HandleFunc("/ui/api/proxy/stop", handleUIStop)
	mux.HandleFunc("/ui/api/proxy/start", handleUIStart)
	mux.HandleFunc("/ui/api/proxy/restart", handleUIRestart(cfg))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy_running": proxyEnabled.Load()})
	})
	mux.HandleFunc("/", handleUI)

	server := &http.Server{Addr: "127.0.0.1:" + cfg.Port, Handler: loggingMiddleware(mux), ReadHeaderTimeout: 15 * time.Second}
	log.Printf("claude-code-proxy listening on http://%s", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func loadConfig() config {
	port := getenv("PROXY_PORT", getenv("LITELLM_PORT", "4000"))
	baseURL := strings.TrimRight(getenv("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/")
	codexAuthFile := os.Getenv("CODEX_AUTH_FILE")
	if codexAuthFile == "" {
		codexAuthFile = filepath.Join(os.Getenv("USERPROFILE"), ".codex", "auth.json")
	}
	return config{
		OpenAIAPIKey:    os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:   baseURL,
		Upstream:        strings.ToLower(getenv("UPSTREAM", "codex")),
		CodexBaseURL:    strings.TrimRight(getenv("CODEX_BASE_URL", "https://chatgpt.com/backend-api/codex"), "/"),
		CodexAuthFile:   codexAuthFile,
		ProxyKey:        getenv("PROXY_API_KEY", os.Getenv("LITELLM_MASTER_KEY")),
		Port:            port,
		ReasoningEffort: normalizeReasoningEffort(getenv("CLAUDE_CODE_EFFORT_LEVEL", getenv("OPENAI_REASONING_EFFORT", "xhigh"))),
		ClaudeDefaults: map[string]string{
			"opus":   getenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "claude-opus-4-7[1m]"),
			"sonnet": getenv("ANTHROPIC_DEFAULT_SONNET_MODEL", "claude-sonnet-4-6[1m]"),
			"haiku":  getenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "claude-haiku-4-5"),
		},
		Models: map[string]string{
			"opus":                      cleanModel(getenv("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5")),
			"opus[1m]":                  cleanModel(getenv("OPENAI_CLAUDE_OPUS_1M_MODEL", getenv("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5"))),
			"claude-opus-4-6":           cleanModel(getenv("OPENAI_CLAUDE_FAST_MODEL", getenv("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5"))),
			"claude-opus-4-6[1m]":       cleanModel(getenv("OPENAI_CLAUDE_FAST_MODEL", getenv("OPENAI_CLAUDE_OPUS_1M_MODEL", getenv("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5")))),
			"claude-opus-4-7":           cleanModel(getenv("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5")),
			"claude-opus-4-7[1m]":       cleanModel(getenv("OPENAI_CLAUDE_OPUS_1M_MODEL", getenv("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5"))),
			"sonnet":                    cleanModel(getenv("OPENAI_CLAUDE_SONNET_MODEL", getenv("OPENAI_CLAUDE_3_7_MODEL", "gpt-5.5"))),
			"sonnet[1m]":                cleanModel(getenv("OPENAI_CLAUDE_SONNET_1M_MODEL", getenv("OPENAI_CLAUDE_SONNET_MODEL", getenv("OPENAI_CLAUDE_3_7_MODEL", "gpt-5.5")))),
			"claude-sonnet-4-6":         cleanModel(getenv("OPENAI_CLAUDE_SONNET_MODEL", getenv("OPENAI_CLAUDE_3_7_MODEL", "gpt-5.5"))),
			"claude-sonnet-4-6[1m]":     cleanModel(getenv("OPENAI_CLAUDE_SONNET_1M_MODEL", getenv("OPENAI_CLAUDE_SONNET_MODEL", getenv("OPENAI_CLAUDE_3_7_MODEL", "gpt-5.5")))),
			"haiku":                     cleanModel(getenv("OPENAI_CLAUDE_HAIKU_MODEL", getenv("OPENAI_CLAUDE_3_5_HAIKU_MODEL", "gpt-5.4-mini"))),
			"claude-haiku-4-5":          cleanModel(getenv("OPENAI_CLAUDE_HAIKU_MODEL", getenv("OPENAI_CLAUDE_3_5_HAIKU_MODEL", "gpt-5.4-mini"))),
			"claude-haiku-4-5-20251001": cleanModel(getenv("OPENAI_CLAUDE_HAIKU_MODEL", getenv("OPENAI_CLAUDE_3_5_HAIKU_MODEL", "gpt-5.4-mini"))),
			"gpt-5.3-codex":             cleanModel(getenv("OPENAI_CLAUDE_CODEX_MODEL", "gpt-5.3-codex")),
			"claude-3-7-sonnet-latest":  cleanModel(getenv("OPENAI_CLAUDE_SONNET_MODEL", getenv("OPENAI_CLAUDE_3_7_MODEL", "gpt-5.5"))),
			"claude-3-5-haiku-latest":   cleanModel(getenv("OPENAI_CLAUDE_HAIKU_MODEL", getenv("OPENAI_CLAUDE_3_5_HAIKU_MODEL", "gpt-5.4-mini"))),
			"claude-3-opus-20240229":    cleanModel(getenv("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5")),
		},
	}
}

func requireAuth(cfg config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.ProxyKey != "" && cfg.ProxyKey != "YOUR_PROXY_TOKEN" {
			if !proxyRequestHasKey(r, cfg.ProxyKey) {
				traceID := newTraceID()
				traceLogID(traceID, "auth.failure", map[string]any{"method": r.Method, "path": r.URL.Path, "remote": clientIP(r), "headers": safeHeaderSummary(r.Header), "summary": proxyAuthSummary(r)})
				setRequestNote(r, "auth mismatch; "+proxyAuthSummary(r)+" trace="+traceID)
				writeAnthropicError(w, http.StatusUnauthorized, "invalid proxy API key")
				return
			}
		}
		next(w, r)
	}
}

func setRequestNote(r *http.Request, note string) {
	requestNotes.Store(r, note)
}

func takeRequestNote(r *http.Request) string {
	if value, ok := requestNotes.LoadAndDelete(r); ok {
		if note, ok := value.(string); ok {
			return note
		}
	}
	return ""
}

func setRequestStat(r *http.Request, stat requestStat) {
	requestStats.Store(r, &stat)
}

func updateRequestStat(r *http.Request, update func(*requestStat)) {
	if r == nil || update == nil {
		return
	}
	value, _ := requestStats.LoadOrStore(r, &requestStat{})
	if stat, ok := value.(*requestStat); ok {
		update(stat)
	}
}

func takeRequestStat(r *http.Request) requestStat {
	if value, ok := requestStats.LoadAndDelete(r); ok {
		if stat, ok := value.(*requestStat); ok && stat != nil {
			return *stat
		}
	}
	return requestStat{}
}

func proxyRequestHasKey(r *http.Request, want string) bool {
	for _, name := range []string{"x-api-key", "anthropic-api-key", "api-key"} {
		for _, value := range r.Header.Values(name) {
			if strings.TrimSpace(value) == want {
				return true
			}
		}
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:]) == want
	}
	return auth == want
}

func proxyAuthSummary(r *http.Request) string {
	parts := []string{}
	for _, name := range []string{"x-api-key", "anthropic-api-key", "api-key", "Authorization"} {
		values := r.Header.Values(name)
		if len(values) == 0 {
			continue
		}
		shapes := []string{}
		for _, value := range values {
			shapes = append(shapes, secretShape(value))
		}
		parts = append(parts, name+"="+strings.Join(shapes, ","))
	}
	if len(parts) == 0 {
		return "no auth headers"
	}
	return strings.Join(parts, "; ")
}

func secretShape(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
		value = strings.TrimSpace(value[7:])
	}
	sum := 0
	for _, r := range value {
		sum = (sum*31 + int(r)) % 9973
	}
	return fmt.Sprintf("len:%d fp:%04d", len(value), sum)
}

func requireProxyEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !proxyEnabled.Load() {
			writeAnthropicError(w, http.StatusServiceUnavailable, "proxy is stopped from the local control panel")
			return
		}
		next(w, r)
	}
}

func handleModels(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		data := make([]map[string]any, 0, len(cfg.Models))
		for name := range cfg.Models {
			if !isAdvertisedModel(name) {
				continue
			}
			data = append(data, map[string]any{"id": name, "type": "model", "display_name": name})
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	}
}

func handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req anthropicRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	chars := len(req.Model)
	for _, msg := range req.Messages {
		chars += len(contentToText(msg.Content))
	}
	tokens := max(1, chars/4)
	setRequestStat(r, requestStat{Model: req.Model, Upstream: "local", InputTokens: tokens})
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}

func handleMessages(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		traceID := newTraceID()
		ctx := withTraceID(r.Context(), traceID)
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			traceLogID(traceID, "anthropic.read_error", map[string]any{"error": err.Error()})
			writeAnthropicError(w, http.StatusBadRequest, "failed to read request body")
			return
		}
		traceLogID(traceID, "anthropic.incoming", map[string]any{
			"method":            r.Method,
			"path":              r.URL.Path,
			"remote":            clientIP(r),
			"headers":           safeHeaderSummary(r.Header),
			"body_bytes":        len(rawBody),
			"body_truncated":    len(rawBody) > traceBodyLimit,
			"body_preview_json": truncateString(string(rawBody), traceBodyLimit),
		})
		if cfg.Upstream == "openai" && (cfg.OpenAIAPIKey == "" || cfg.OpenAIAPIKey == "YOUR_OPENAI_API_KEY" || strings.HasPrefix(cfg.OpenAIAPIKey, "sk-or-")) {
			traceLogID(traceID, "anthropic.config_error", map[string]any{"message": "OPENAI_API_KEY is not configured"})
			writeAnthropicError(w, http.StatusBadRequest, "OPENAI_API_KEY is not configured")
			return
		}

		var in anthropicRequest
		if err := json.NewDecoder(bytes.NewReader(rawBody)).Decode(&in); err != nil {
			traceLogID(traceID, "anthropic.decode_error", map[string]any{"error": err.Error()})
			writeAnthropicError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		in.FastMode = requestUsesClaudeFastMode(r, in)
		traceLogID(traceID, "anthropic.decoded", summarizeAnthropicRequest(in))
		out, err := toOpenAI(cfg, in)
		if err != nil {
			traceLogID(traceID, "anthropic.convert_error", map[string]any{"error": err.Error()})
			writeAnthropicError(w, http.StatusBadRequest, err.Error())
			return
		}
		if cfg.Upstream == "codex" {
			responsesReq := toResponses(cfg, in)
			traceLogID(traceID, "codex.prepared", summarizeResponsesRequest(responsesReq))
			setRequestStat(r, requestStat{Model: in.Model, Upstream: responsesReq.Model, Stream: in.Stream})
			note := requestNote(in.Model, responsesReq.Model, in.Stream, requestReasoningEffort(cfg, in))
			if responsesReq.ServiceTier != "" {
				note += " service_tier=" + responsesReq.ServiceTier
			}
			setRequestNote(r, note+" trace="+traceID)
			if in.Stream {
				streamCodex(ctx, cfg, responsesReq, in.Model, w, r)
				return
			}
			callCodex(ctx, cfg, responsesReq, in.Model, w, r)
			return
		}
		traceLogID(traceID, "openai.prepared", summarizeOpenAIRequest(out))
		setRequestStat(r, requestStat{Model: in.Model, Upstream: out.Model, Stream: in.Stream})
		setRequestNote(r, requestNote(in.Model, out.Model, in.Stream, out.ReasoningEffort)+" trace="+traceID)
		if in.Stream {
			streamOpenAI(ctx, cfg, out, in.Model, w, r)
			return
		}
		callOpenAI(ctx, cfg, out, in.Model, w, r)
	}
}

func handleChatCompletions(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var in openAIRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if in.MaxTokens == 0 {
			in.MaxTokens = in.MaxCompletionTokens
		}
		if cfg.Upstream == "openai" {
			setRequestStat(r, requestStat{Model: in.Model, Upstream: in.Model, Stream: in.Stream})
			proxyOpenAIChat(r.Context(), cfg, in, w)
			return
		}
		out := openAIChatToResponses(cfg, in)
		setRequestStat(r, requestStat{Model: in.Model, Upstream: out.Model, Stream: in.Stream})
		if in.Stream {
			streamCodexAsOpenAIChat(r.Context(), cfg, out, w)
			return
		}
		resp, status, msg, err := callCodexResponses(r.Context(), cfg, out)
		if err != nil {
			writeOpenAIError(w, status, msg)
			return
		}
		updateRequestStat(r, func(stat *requestStat) {
			stat.InputTokens = resp.Usage.InputTokens
			stat.OutputTokens = resp.Usage.OutputTokens
			stat.StopReason = "stop"
		})
		writeJSON(w, http.StatusOK, responsesToOpenAIChat(resp, out.Model))
	}
}

func handleResponses(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var in responsesRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if mapped, ok := cfg.Models[in.Model]; ok {
			in.Model = mapped
		} else {
			in.Model = cleanModel(in.Model)
		}
		if strings.TrimSpace(in.Instructions) == "" {
			in.Instructions = "You are a helpful coding assistant."
		}
		if in.Reasoning == nil {
			if effort := normalizeReasoningEffort(cfg.ReasoningEffort); effort != "" {
				in.Reasoning = &responsesReasoning{Effort: effort}
			}
		} else {
			in.Reasoning.Effort = normalizeReasoningEffort(in.Reasoning.Effort)
		}
		in.Store = false
		if cfg.Upstream == "openai" {
			setRequestStat(r, requestStat{Model: in.Model, Upstream: in.Model, Stream: in.Stream})
			proxyOpenAIResponses(r.Context(), cfg, in, w)
			return
		}
		setRequestStat(r, requestStat{Model: in.Model, Upstream: in.Model, Stream: in.Stream})
		if in.Stream {
			streamCodexResponsesRaw(r.Context(), cfg, in, w)
			return
		}
		resp, status, msg, err := callCodexResponses(r.Context(), cfg, in)
		if err != nil {
			writeOpenAIError(w, status, msg)
			return
		}
		updateRequestStat(r, func(stat *requestStat) {
			stat.InputTokens = resp.Usage.InputTokens
			stat.OutputTokens = resp.Usage.OutputTokens
			stat.StopReason = "completed"
		})
		writeJSON(w, http.StatusOK, responsesToOpenAIResponse(resp))
	}
}

func toResponses(cfg config, in anthropicRequest) responsesRequest {
	model, ok := cfg.Models[in.Model]
	if !ok {
		model = cleanModel(in.Model)
	}
	instructions := contentToText(in.System)
	if instructions == "" {
		instructions = "You are a helpful coding assistant."
	}
	out := responsesRequest{Model: model, Instructions: instructions, Temperature: in.Temperature, Stream: in.Stream, Store: false}
	if in.FastMode {
		out.ServiceTier = getenv("CODEX_FAST_SERVICE_TIER", "priority")
	}
	if effort := requestReasoningEffort(cfg, in); effort != "" {
		out.Reasoning = &responsesReasoning{Effort: effort, Summary: codexReasoningSummaryMode()}
	}
	hasWebSearch := false
	for _, tool := range in.Tools {
		if isClaudeWebSearchTool(tool) {
			if !hasWebSearch {
				out.Tools = append(out.Tools, codexWebSearchTool(tool))
				hasWebSearch = true
			}
			continue
		}
		out.Tools = append(out.Tools, responsesTool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema})
	}
	if hasWebSearch {
		out.Instructions = strings.TrimSpace(out.Instructions + "\n\nUse the built-in web search tool when current web information is needed. Return cited source links in the final answer.")
	}
	for _, msg := range in.Messages {
		role := msg.Role
		if role != "user" && role != "assistant" && role != "system" {
			role = "user"
		}
		out.Input = append(out.Input, convertAnthropicMessageToResponses(role, msg.Content)...)
	}
	return out
}

func requestUsesClaudeFastMode(r *http.Request, in anthropicRequest) bool {
	if strings.EqualFold(strings.TrimSpace(in.Speed), "fast") {
		return true
	}
	if isClaudeFastModel(in.Model) {
		return true
	}
	for _, beta := range r.Header.Values("Anthropic-Beta") {
		if strings.Contains(strings.ToLower(beta), "fast-mode") && isClaudeFastModel(in.Model) {
			return true
		}
	}
	return false
}

func isClaudeFastModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(model, "[1m]", "")))
	return model == "claude-opus-4-6"
}

func codexReasoningSummaryMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_REASONING_SUMMARY"))) {
	case "none", "off", "false", "0":
		return ""
	case "concise", "detailed", "auto":
		return strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_REASONING_SUMMARY")))
	default:
		return "auto"
	}
}

func isClaudeWebSearchTool(tool anthropicTool) bool {
	return isClaudeWebSearchToolName(tool.Name) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(tool.Type)), "web_search")
}

func isClaudeWebSearchToolName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "websearch" || name == "web_search" || name == "mcp__web-search__web_search"
}

func codexWebSearchTool(tool anthropicTool) responsesTool {
	out := responsesTool{Type: codexWebSearchToolType(), UserLocation: tool.UserLocation}
	if filters := codexWebSearchFilters(tool); filters != nil && out.Type == "web_search" {
		out.Filters = filters
	}
	if size := strings.TrimSpace(os.Getenv("CODEX_WEB_SEARCH_CONTEXT_SIZE")); size != "" {
		out.SearchContextSize = size
	}
	return out
}

func codexWebSearchFilters(tool anthropicTool) any {
	filters := map[string]any{}
	if len(tool.AllowedDomains) > 0 {
		filters["allowed_domains"] = tool.AllowedDomains
	}
	if len(tool.BlockedDomains) > 0 {
		filters["blocked_domains"] = tool.BlockedDomains
	}
	if len(filters) == 0 {
		return nil
	}
	return filters
}

func codexWebSearchToolType() string {
	switch strings.TrimSpace(os.Getenv("CODEX_WEB_SEARCH_TOOL_TYPE")) {
	case "web_search_preview", "web_search_preview_2025_03_11":
		return strings.TrimSpace(os.Getenv("CODEX_WEB_SEARCH_TOOL_TYPE"))
	default:
		return "web_search"
	}
}

func convertAnthropicMessageToResponses(role string, content any) []any {
	blocks, ok := content.([]any)
	if !ok {
		text := contentToText(content)
		if text == "" {
			return nil
		}
		return []any{responseInput{Role: role, Content: text}}
	}

	var messageParts []map[string]any
	var items []any
	flushMessage := func() {
		if len(messageParts) == 0 {
			return
		}
		items = append(items, responseInput{Role: role, Content: messageParts})
		messageParts = nil
	}

	for _, block := range blocks {
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		switch fmt.Sprint(m["type"]) {
		case "thinking", "redacted_thinking":
			continue
		case "text":
			messageParts = append(messageParts, map[string]any{"type": inputTextType(role), "text": fmt.Sprint(m["text"])})
		case "image":
			if image := anthropicImageToResponses(m); image != nil {
				messageParts = append(messageParts, image)
			}
		case "tool_use":
			if isClaudeWebSearchToolName(fmt.Sprint(m["name"])) {
				rawInput, _ := json.Marshal(m["input"])
				messageParts = append(messageParts, map[string]any{"type": inputTextType(role), "text": "Web search requested: " + string(rawInput)})
				continue
			}
			flushMessage()
			args, _ := json.Marshal(m["input"])
			items = append(items, responseInput{Type: "function_call", CallID: fmt.Sprint(m["id"]), Name: fmt.Sprint(m["name"]), Arguments: string(args)})
		case "tool_result":
			text := contentToText(m["content"])
			if strings.HasPrefix(strings.TrimSpace(text), "Web search results for query:") {
				messageParts = append(messageParts, map[string]any{"type": inputTextType(role), "text": text})
				continue
			}
			flushMessage()
			items = append(items, responseInput{Type: "function_call_output", CallID: fmt.Sprint(m["tool_use_id"]), Output: text})
		}
	}
	flushMessage()
	return items
}

func inputTextType(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

func anthropicImageToResponses(block map[string]any) map[string]any {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil
	}
	if fmt.Sprint(source["type"]) != "base64" {
		return nil
	}
	mediaType := fmt.Sprint(source["media_type"])
	data := fmt.Sprint(source["data"])
	if mediaType == "" || data == "" {
		return nil
	}
	return map[string]any{"type": "input_image", "image_url": "data:" + mediaType + ";base64," + data}
}

func loadCodexAuth(cfg config) (codexAuthFile, error) {
	var auth codexAuthFile
	raw, err := os.ReadFile(cfg.CodexAuthFile)
	if err != nil {
		return auth, fmt.Errorf("Codex auth not found at %s; run codex login --device-auth", cfg.CodexAuthFile)
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		return auth, fmt.Errorf("invalid Codex auth file")
	}
	if auth.AuthMode != "chatgpt" || auth.Tokens.AccessToken == "" {
		return auth, fmt.Errorf("Codex auth file is not a ChatGPT subscription login; run codex login --device-auth")
	}
	return auth, nil
}

func newCodexRequest(ctx context.Context, cfg config, path string, body any) (*http.Request, error) {
	auth, err := loadCodexAuth(cfg)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CodexBaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+auth.Tokens.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("version", "0.128.0")
	if auth.Tokens.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", auth.Tokens.AccountID)
	}
	return req, nil
}

func callCodex(ctx context.Context, cfg config, out responsesRequest, requestedModel string, w http.ResponseWriter, r *http.Request) {
	codexResp, status, msg, err := callCodexResponses(ctx, cfg, out)
	if err != nil {
		writeAnthropicError(w, status, msg)
		return
	}
	anthropicOut := toAnthropicResponsesResponse(codexResp, requestedModel)
	updateRequestStat(r, func(stat *requestStat) {
		stat.InputTokens = codexResp.Usage.InputTokens
		stat.OutputTokens = codexResp.Usage.OutputTokens
		stat.StopReason = fmt.Sprint(anthropicOut["stop_reason"])
	})
	traceLog(ctx, "anthropic.out.json", map[string]any{"model": anthropicOut["model"], "stop_reason": anthropicOut["stop_reason"], "content_blocks": len(anthropicOut["content"].([]any)), "usage": anthropicOut["usage"]})
	writeJSON(w, http.StatusOK, anthropicOut)
}

func callCodexResponses(ctx context.Context, cfg config, out responsesRequest) (responsesResponse, int, string, error) {
	return callCodexResponsesOnce(ctx, cfg, out, true)
}

func callCodexResponsesOnce(ctx context.Context, cfg config, out responsesRequest, allowRetry bool) (responsesResponse, int, string, error) {
	out.Stream = true
	start := time.Now()
	traceLog(ctx, "codex.request", map[string]any{
		"url":     cfg.CodexBaseURL + "/responses",
		"payload": summarizeResponsesRequest(out),
	})
	req, err := newCodexRequest(ctx, cfg, "/responses", out)
	if err != nil {
		traceLog(ctx, "codex.request_error", map[string]any{"error": err.Error()})
		return responsesResponse{}, http.StatusBadRequest, err.Error(), err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		traceLog(ctx, "codex.transport_error", map[string]any{"error": err.Error(), "duration_ms": time.Since(start).Milliseconds()})
		return responsesResponse{}, http.StatusBadGateway, err.Error(), err
	}
	defer resp.Body.Close()
	traceLog(ctx, "codex.response_headers", map[string]any{
		"status":      resp.StatusCode,
		"duration_ms": time.Since(start).Milliseconds(),
		"headers":     safeHeaderSummary(resp.Header),
	})
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = resp.Status
		}
		traceLog(ctx, "codex.error_body", map[string]any{"status": resp.StatusCode, "body": truncateString(msg, traceBodyLimit)})
		if allowRetry {
			if retry, ok := retryCodexRequestAfter400(out, resp.StatusCode, msg); ok {
				traceLog(ctx, "codex.retry", map[string]any{"reason": retry.reason, "payload": summarizeResponsesRequest(retry.request)})
				return callCodexResponsesOnce(ctx, cfg, retry.request, false)
			}
		}
		return responsesResponse{}, resp.StatusCode, msg, fmt.Errorf("codex upstream returned %s", resp.Status)
	}
	collected := collectCodexStream(ctx, resp.Body, out.Model)
	traceLog(ctx, "codex.collected", summarizeResponsesResponse(collected))
	return collected, http.StatusOK, "", nil
}

type codexRetryRequest struct {
	reason  string
	request responsesRequest
}

func retryCodexRequestAfter400(out responsesRequest, status int, msg string) (codexRetryRequest, bool) {
	if status != http.StatusBadRequest {
		return codexRetryRequest{}, false
	}
	lower := strings.ToLower(msg)
	if out.Reasoning != nil && out.Reasoning.Summary != "" && strings.Contains(lower, "summary") {
		out.Reasoning.Summary = ""
		return codexRetryRequest{reason: "reasoning_summary_not_accepted", request: out}, true
	}
	if hasCodexWebSearchTool(out) && strings.Contains(lower, "web_search") {
		return codexRetryRequest{reason: "alternate_web_search_tool_type", request: swapCodexWebSearchToolType(out, alternateCodexWebSearchToolType(out))}, true
	}
	if out.ServiceTier != "" && strings.Contains(lower, "service_tier") {
		out.ServiceTier = ""
		return codexRetryRequest{reason: "service_tier_not_accepted", request: out}, true
	}
	return codexRetryRequest{}, false
}

func hasCodexWebSearchTool(out responsesRequest) bool {
	for _, tool := range out.Tools {
		if strings.HasPrefix(tool.Type, "web_search") {
			return true
		}
	}
	return false
}

func alternateCodexWebSearchToolType(out responsesRequest) string {
	for _, tool := range out.Tools {
		switch tool.Type {
		case "web_search":
			return "web_search_preview"
		case "web_search_preview", "web_search_preview_2025_03_11":
			return "web_search"
		}
	}
	return "web_search_preview"
}

func swapCodexWebSearchToolType(out responsesRequest, toolType string) responsesRequest {
	for i := range out.Tools {
		if strings.HasPrefix(out.Tools[i].Type, "web_search") {
			out.Tools[i].Type = toolType
			if toolType != "web_search" {
				out.Tools[i].Filters = nil
			}
		}
	}
	return out
}

func streamCodex(ctx context.Context, cfg config, out responsesRequest, requestedModel string, w http.ResponseWriter, r *http.Request) {
	out.Stream = true
	res, status, msg, err := callCodexResponses(ctx, cfg, out)
	if err != nil {
		writeAnthropicError(w, status, msg)
		return
	}
	stopReason := anthropicStopReason(anthropicContentBlocks(res))
	updateRequestStat(r, func(stat *requestStat) {
		stat.InputTokens = res.Usage.InputTokens
		stat.OutputTokens = res.Usage.OutputTokens
		stat.StopReason = stopReason
	})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	messageID := "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	sendAnthropicMessageStart(w, messageID, firstNonEmpty(requestedModel, out.Model), res.Usage.InputTokens, 0)
	traceLog(ctx, "anthropic.out.event", map[string]any{"event": "message_start", "message_id": messageID, "model": firstNonEmpty(requestedModel, out.Model)})
	writeAnthropicBufferedStream(ctx, w, res)
	fmt.Fprint(w, "data: [DONE]\n\n")
	traceLog(ctx, "anthropic.out.done", map[string]any{"done": true})
	if flusher != nil {
		flusher.Flush()
	}
}

func streamCodexResponsesRaw(ctx context.Context, cfg config, out responsesRequest, w http.ResponseWriter) {
	out.Stream = true
	req, err := newCodexRequest(ctx, cfg, "/responses", out)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		writeOpenAIError(w, resp.StatusCode, strings.TrimSpace(string(raw)))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}

func streamCodexAsOpenAIChat(ctx context.Context, cfg config, out responsesRequest, w http.ResponseWriter) {
	out.Stream = true
	req, err := newCodexRequest(ctx, cfg, "/responses", out)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		writeOpenAIError(w, resp.StatusCode, strings.TrimSpace(string(raw)))
		return
	}
	id := "chatcmpl_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	created := time.Now().Unix()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	sendOpenAIChatChunk(w, id, created, out.Model, map[string]any{"role": "assistant"}, nil)
	if flusher != nil {
		flusher.Flush()
	}
	scanner := newSSEScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var event responsesStreamEvent
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		if text := codexStreamText(event); text != "" {
			sendOpenAIChatChunk(w, id, created, out.Model, map[string]any{"content": text}, nil)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if err := scanner.Err(); err != nil {
		traceLog(ctx, "codex.openai_chat.scan_error", map[string]any{"error": err.Error()})
	}
	stop := "stop"
	sendOpenAIChatChunk(w, id, created, out.Model, map[string]any{}, &stop)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func collectCodexStream(ctx context.Context, r io.Reader, model string) responsesResponse {
	resp := responsesResponse{
		ID:    "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Model: model,
	}
	var text strings.Builder
	eventCounts := map[string]int{}
	items := map[int]responsesOutputItem{}
	itemOrder := []int{}
	rememberItem := func(idx int, item responsesOutputItem) {
		if item.Type == "" {
			return
		}
		if idx < 0 {
			idx = len(itemOrder)
		}
		if _, ok := items[idx]; !ok {
			itemOrder = append(itemOrder, idx)
		}
		existing := items[idx]
		if item.ID == "" {
			item.ID = existing.ID
		}
		if item.CallID == "" {
			item.CallID = existing.CallID
		}
		if item.Name == "" {
			item.Name = existing.Name
		}
		if item.Arguments == "" {
			item.Arguments = existing.Arguments
		}
		if len(item.Content) == 0 {
			item.Content = existing.Content
		}
		if len(item.Summary) == 0 {
			item.Summary = existing.Summary
		}
		items[idx] = item
	}
	scanner := newSSEScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var event responsesStreamEvent
		if json.Unmarshal([]byte(data), &event) != nil {
			traceLog(ctx, "codex.stream.unparsed", map[string]any{"line": truncateString(data, 500)})
			continue
		}
		eventCounts[event.Type]++
		traceLog(ctx, "codex.stream.event", summarizeResponsesStreamEvent(event))
		if event.Response.ID != "" {
			resp.ID = event.Response.ID
		}
		if event.Response.Model != "" {
			resp.Model = event.Response.Model
		}
		if event.Response.Usage.InputTokens != 0 || event.Response.Usage.OutputTokens != 0 {
			resp.Usage = event.Response.Usage
		}
		if len(event.Response.Output) > 0 {
			resp.Output = event.Response.Output
		}
		if event.Item.Type != "" {
			rememberItem(event.OutputIndex, event.Item)
		}
		if strings.Contains(event.Type, "function_call_arguments") && (event.Delta != "" || event.Arguments != "") {
			item := items[event.OutputIndex]
			item.Type = "function_call"
			if event.Arguments != "" {
				item.Arguments = event.Arguments
			} else {
				item.Arguments += event.Delta
			}
			rememberItem(event.OutputIndex, item)
		}
		if strings.Contains(event.Type, "reasoning_summary_text") && (event.Delta != "" || event.Text != "") {
			item := items[event.OutputIndex]
			item.Type = "reasoning"
			item.ID = firstNonEmpty(item.ID, event.ItemID)
			item.Summary = mergeReasoningSummaryText(item.Summary, event.SummaryIndex, firstNonEmpty(event.Text, event.Delta), event.Text != "")
			rememberItem(event.OutputIndex, item)
		}
		if delta := codexStreamText(event); delta != "" {
			text.WriteString(delta)
		}
	}
	if err := scanner.Err(); err != nil {
		traceLog(ctx, "codex.stream.scan_error", map[string]any{"error": err.Error()})
	}
	traceLog(ctx, "codex.stream.complete", map[string]any{"event_counts": eventCounts, "text_chars": text.Len(), "output_items": len(resp.Output)})
	if len(resp.Output) == 0 {
		for _, idx := range itemOrder {
			resp.Output = append(resp.Output, items[idx])
		}
	}
	if text.Len() > 0 && !responsesOutputHasText(resp.Output) {
		resp.Output = append(resp.Output, responsesOutputItem{
			Type:    "message",
			Role:    "assistant",
			Content: []responsesOutputContent{{Type: "output_text", Text: text.String()}},
		})
	}
	return resp
}

func codexStreamText(event responsesStreamEvent) string {
	switch event.Type {
	case "response.output_text.delta", "response.refusal.delta":
		return event.Delta
	default:
		return ""
	}
}

func mergeReasoningSummaryText(parts []responsesReasoningPart, idx int, text string, replace bool) []responsesReasoningPart {
	if text == "" {
		return parts
	}
	if idx < 0 {
		idx = 0
	}
	for len(parts) <= idx {
		parts = append(parts, responsesReasoningPart{Type: "summary_text"})
	}
	parts[idx].Type = firstNonEmpty(parts[idx].Type, "summary_text")
	if replace {
		parts[idx].Text = text
	} else {
		parts[idx].Text += text
	}
	return parts
}

func toAnthropicResponsesResponse(resp responsesResponse, requestedModel string) map[string]any {
	content := anthropicContentBlocks(resp)
	return map[string]any{"id": resp.ID, "type": "message", "role": "assistant", "model": firstNonEmpty(requestedModel, resp.Model), "content": content, "stop_reason": anthropicStopReason(content), "stop_sequence": nil, "usage": map[string]any{"input_tokens": resp.Usage.InputTokens, "output_tokens": resp.Usage.OutputTokens}}
}

func responsesOutputHasText(items []responsesOutputItem) bool {
	for _, item := range items {
		for _, part := range item.Content {
			if part.Text != "" {
				return true
			}
		}
	}
	return false
}

func anthropicContentBlocks(resp responsesResponse) []any {
	content := []any{}
	for _, item := range resp.Output {
		switch item.Type {
		case "reasoning":
			if thinking := reasoningSummaryText(item); thinking != "" {
				content = append(content, map[string]any{"type": "thinking", "thinking": thinking, "signature": codexThinkingSignature(item, thinking)})
			}
		case "message":
			for _, part := range item.Content {
				if part.Text != "" {
					content = append(content, map[string]any{"type": "text", "text": part.Text})
				}
			}
		case "function_call":
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    firstNonEmpty(item.CallID, item.ID, "toolu_"+strconv.FormatInt(time.Now().UnixNano(), 36)),
				"name":  item.Name,
				"input": functionArgumentsToInput(item.Arguments),
			})
		}
	}
	return content
}

func reasoningSummaryText(item responsesOutputItem) string {
	var parts []string
	for _, part := range item.Summary {
		if strings.TrimSpace(part.Text) != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func codexThinkingSignature(item responsesOutputItem, thinking string) string {
	sum := sha256.Sum256([]byte(firstNonEmpty(item.ID, item.Type) + "\n" + thinking))
	return "codex-reasoning-summary:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func functionArgumentsToInput(arguments string) any {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return map[string]any{}
	}
	var input any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil || input == nil {
		return map[string]any{"arguments": arguments}
	}
	if _, ok := input.(map[string]any); !ok {
		return map[string]any{"value": input}
	}
	return input
}

func anthropicStopReason(content []any) string {
	for _, block := range content {
		m, ok := block.(map[string]any)
		if ok && m["type"] == "tool_use" {
			return "tool_use"
		}
	}
	return "end_turn"
}

func sendAnthropicMessageStart(w io.Writer, messageID, model string, inputTokens, outputTokens int) {
	sendEvent(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": messageID, "type": "message", "role": "assistant", "content": []any{}, "model": model, "stop_reason": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens}}})
}

func writeAnthropicBufferedStream(ctx context.Context, w io.Writer, resp responsesResponse) {
	content := anthropicContentBlocks(resp)
	for i, block := range content {
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "thinking":
			thinking := fmt.Sprint(m["thinking"])
			signature := fmt.Sprint(m["signature"])
			sendEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
			traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_start", "index": i, "block_type": "thinking"})
			if thinking != "" {
				sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": thinking}})
				traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_delta", "index": i, "delta_type": "thinking_delta", "thinking_chars": len(thinking), "thinking_preview": truncateString(thinking, 500)})
			}
			if signature != "" {
				sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "signature_delta", "signature": signature}})
				traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_delta", "index": i, "delta_type": "signature_delta"})
			}
			sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
			traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_stop", "index": i})
		case "text":
			sendEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_start", "index": i, "block_type": "text"})
			if text := fmt.Sprint(m["text"]); text != "" {
				sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": text}})
				traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_delta", "index": i, "delta_type": "text_delta", "text_chars": len(text), "text_preview": truncateString(text, 500)})
			}
			sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
			traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_stop", "index": i})
		case "tool_use":
			id := fmt.Sprint(m["id"])
			name := fmt.Sprint(m["name"])
			input := m["input"]
			sendEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}})
			traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_start", "index": i, "block_type": "tool_use", "id": id, "name": name})
			rawInput, _ := json.Marshal(input)
			if string(rawInput) != "{}" && string(rawInput) != "null" {
				sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(rawInput)}})
				traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_delta", "index": i, "delta_type": "input_json_delta", "json_chars": len(rawInput), "json_preview": truncateString(string(rawInput), 500)})
			}
			sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
			traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_stop", "index": i})
		}
	}
	sendEvent(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": anthropicStopReason(content), "stop_sequence": nil}, "usage": map[string]any{"output_tokens": resp.Usage.OutputTokens}})
	sendEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	traceLog(ctx, "anthropic.out.event", map[string]any{"event": "message_delta", "stop_reason": anthropicStopReason(content), "output_tokens": resp.Usage.OutputTokens})
	traceLog(ctx, "anthropic.out.event", map[string]any{"event": "message_stop"})
}

func openAIChatToResponses(cfg config, in openAIRequest) responsesRequest {
	model, ok := cfg.Models[in.Model]
	if !ok {
		model = cleanModel(in.Model)
	}
	out := responsesRequest{Model: model, Temperature: in.Temperature, Stream: in.Stream, Store: false}
	if effort := normalizeReasoningEffort(firstNonEmpty(in.ReasoningEffort, cfg.ReasoningEffort)); effort != "" {
		out.Reasoning = &responsesReasoning{Effort: effort}
	}
	var instructions []string
	for _, tool := range in.Tools {
		out.Tools = append(out.Tools, responsesTool{Type: "function", Name: tool.Function.Name, Description: tool.Function.Description, Parameters: tool.Function.Parameters})
	}
	for _, msg := range in.Messages {
		switch msg.Role {
		case "system", "developer":
			if text := contentToText(msg.Content); text != "" {
				instructions = append(instructions, text)
			}
		case "tool":
			out.Input = append(out.Input, responseInput{Type: "function_call_output", CallID: msg.ToolCallID, Output: contentToText(msg.Content)})
		case "assistant":
			if content := openAIContentToResponses("assistant", msg.Content); content != nil {
				out.Input = append(out.Input, responseInput{Role: "assistant", Content: content})
			}
			for _, tc := range msg.ToolCalls {
				out.Input = append(out.Input, responseInput{Type: "function_call", CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
		default:
			if content := openAIContentToResponses("user", msg.Content); content != nil {
				out.Input = append(out.Input, responseInput{Role: "user", Content: content})
			}
		}
	}
	if len(instructions) > 0 {
		out.Instructions = strings.Join(instructions, "\n")
	} else {
		out.Instructions = "You are a helpful coding assistant."
	}
	return out
}

func openAIContentToResponses(role string, content any) any {
	blocks, ok := content.([]any)
	if !ok {
		text := contentToText(content)
		if text == "" {
			return nil
		}
		return text
	}
	parts := []map[string]any{}
	for _, block := range blocks {
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		switch fmt.Sprint(m["type"]) {
		case "text":
			parts = append(parts, map[string]any{"type": inputTextType(role), "text": fmt.Sprint(m["text"])})
		case "image_url":
			if url := openAIImageURL(m["image_url"]); url != "" {
				parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
			}
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

func openAIImageURL(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		return fmt.Sprint(x["url"])
	default:
		return ""
	}
}

func responsesText(resp responsesResponse) string {
	var text strings.Builder
	for _, item := range resp.Output {
		for _, part := range item.Content {
			if part.Text != "" {
				text.WriteString(part.Text)
			}
		}
	}
	return text.String()
}

func responsesToOpenAIChat(resp responsesResponse, model string) openAIChatCompletion {
	if resp.Model != "" {
		model = resp.Model
	}
	id := "chatcmpl_" + strings.TrimPrefix(resp.ID, "resp_")
	out := openAIChatCompletion{ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: model}
	choice := struct {
		Index        int           `json:"index"`
		Message      openAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	}{Index: 0, Message: openAIMessage{Role: "assistant", Content: responsesText(resp)}, FinishReason: "stop"}
	for _, item := range resp.Output {
		if item.Type == "function_call" {
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, openAIToolCall{ID: firstNonEmpty(item.CallID, item.ID), Type: "function", Function: openAIToolFunction{Name: item.Name, Arguments: item.Arguments}})
		}
	}
	out.Choices = []struct {
		Index        int           `json:"index"`
		Message      openAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	}{choice}
	out.Usage = map[string]int{"prompt_tokens": resp.Usage.InputTokens, "completion_tokens": resp.Usage.OutputTokens, "total_tokens": resp.Usage.InputTokens + resp.Usage.OutputTokens}
	return out
}

func responsesToOpenAIResponse(resp responsesResponse) map[string]any {
	return map[string]any{
		"id":          resp.ID,
		"object":      "response",
		"created_at":  time.Now().Unix(),
		"status":      "completed",
		"model":       resp.Model,
		"output":      resp.Output,
		"output_text": responsesText(resp),
		"usage": map[string]int{
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
			"total_tokens":  resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

func toOpenAI(cfg config, in anthropicRequest) (openAIRequest, error) {
	model, ok := cfg.Models[in.Model]
	if !ok {
		model = cleanModel(in.Model)
	}
	out := openAIRequest{Model: model, MaxTokens: in.MaxTokens, Temperature: in.Temperature, Stream: in.Stream}
	if sys := contentToText(in.System); sys != "" {
		out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: sys})
	}
	for _, msg := range in.Messages {
		converted := convertMessage(msg)
		out.Messages = append(out.Messages, converted...)
	}
	for _, tool := range in.Tools {
		out.Tools = append(out.Tools, openAITool{Type: "function", Function: openAIFunction{Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema}})
	}
	return out, nil
}

func convertMessage(msg anthropicMessage) []openAIMessage {
	blocks, ok := msg.Content.([]any)
	if !ok {
		return []openAIMessage{{Role: msg.Role, Content: contentToText(msg.Content)}}
	}
	var text []string
	var toolCalls []openAIToolCall
	var out []openAIMessage
	for _, block := range blocks {
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			text = append(text, fmt.Sprint(m["text"]))
		case "tool_use":
			args, _ := json.Marshal(m["input"])
			toolCalls = append(toolCalls, openAIToolCall{ID: fmt.Sprint(m["id"]), Type: "function", Function: openAIToolFunction{Name: fmt.Sprint(m["name"]), Arguments: string(args)}})
		case "tool_result":
			out = append(out, openAIMessage{Role: "tool", ToolCallID: fmt.Sprint(m["tool_use_id"]), Content: contentToText(m["content"])})
		}
	}
	if len(text) > 0 || len(toolCalls) > 0 {
		out = append([]openAIMessage{{Role: msg.Role, Content: strings.Join(text, "\n"), ToolCalls: toolCalls}}, out...)
	}
	return out
}

func proxyOpenAIChat(ctx context.Context, cfg config, out openAIRequest, w http.ResponseWriter) {
	if cfg.OpenAIAPIKey == "" || cfg.OpenAIAPIKey == "YOUR_OPENAI_API_KEY" {
		writeOpenAIError(w, http.StatusBadRequest, "OPENAI_API_KEY is not configured")
		return
	}
	if mapped, ok := cfg.Models[out.Model]; ok {
		out.Model = mapped
	} else {
		out.Model = cleanModel(out.Model)
	}
	proxyOpenAIRequest(ctx, cfg.OpenAIAPIKey, cfg.OpenAIBaseURL+"/chat/completions", out, w)
}

func proxyOpenAIResponses(ctx context.Context, cfg config, out responsesRequest, w http.ResponseWriter) {
	if cfg.OpenAIAPIKey == "" || cfg.OpenAIAPIKey == "YOUR_OPENAI_API_KEY" {
		writeOpenAIError(w, http.StatusBadRequest, "OPENAI_API_KEY is not configured")
		return
	}
	proxyOpenAIRequest(ctx, cfg.OpenAIAPIKey, cfg.OpenAIBaseURL+"/responses", out, w)
}

func proxyOpenAIRequest(ctx context.Context, apiKey, url string, body any, w http.ResponseWriter) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func callOpenAI(ctx context.Context, cfg config, out openAIRequest, requestedModel string, w http.ResponseWriter, r *http.Request) {
	body, _ := json.Marshal(out)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.OpenAIBaseURL+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		writeAnthropicError(w, resp.StatusCode, string(raw))
		return
	}
	var openResp openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&openResp); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "invalid OpenAI response")
		return
	}
	anthropicOut := toAnthropicResponse(openResp, requestedModel)
	updateRequestStat(r, func(stat *requestStat) {
		stat.InputTokens = openResp.Usage.InputTokens
		stat.OutputTokens = openResp.Usage.OutputTokens
		stat.StopReason = fmt.Sprint(anthropicOut["stop_reason"])
	})
	writeJSON(w, http.StatusOK, anthropicOut)
}

func streamOpenAI(ctx context.Context, cfg config, out openAIRequest, requestedModel string, w http.ResponseWriter, r *http.Request) {
	body, _ := json.Marshal(out)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.OpenAIBaseURL+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		writeAnthropicError(w, resp.StatusCode, string(raw))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	sendEvent(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36), "type": "message", "role": "assistant", "content": []any{}, "model": firstNonEmpty(requestedModel, out.Model), "stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	sendEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	if flusher != nil {
		flusher.Flush()
	}

	scanner := newSSEScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk openAIStreamChunk
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		if text := chunk.Choices[0].Delta.Content; text != "" {
			sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if err := scanner.Err(); err != nil {
		traceLog(ctx, "openai.stream.scan_error", map[string]any{"error": err.Error()})
	}
	sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	sendEvent(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
	sendEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func toAnthropicResponse(resp openAIResponse, requestedModel string) map[string]any {
	content := []any{}
	stopReason := "end_turn"
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if s, ok := map[string]string{"tool_calls": "tool_use", "length": "max_tokens", "stop": "end_turn"}[choice.FinishReason]; ok {
			stopReason = s
		}
		if text := contentToText(choice.Message.Content); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, tc := range choice.Message.ToolCalls {
			var input any = map[string]any{}
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
		}
	}
	return map[string]any{"id": resp.ID, "type": "message", "role": "assistant", "model": firstNonEmpty(requestedModel, resp.Model), "content": content, "stop_reason": stopReason, "stop_sequence": nil, "usage": map[string]any{"input_tokens": resp.Usage.InputTokens, "output_tokens": resp.Usage.OutputTokens}}
}

func contentToText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := []string{}
		for _, item := range x {
			if m, ok := item.(map[string]any); ok && m["type"] == "text" {
				parts = append(parts, fmt.Sprint(m["text"]))
			} else {
				parts = append(parts, contentToText(item))
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, ok := x["text"]; ok {
			return fmt.Sprint(text)
		}
		b, _ := json.Marshal(x)
		return string(b)
	default:
		return fmt.Sprint(x)
	}
}

func newSSEScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), sseMaxLineSize)
	return scanner
}

func sendEvent(w io.Writer, event string, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

func sendOpenAIChatChunk(w io.Writer, id string, created int64, model string, delta map[string]any, finishReason *string) {
	payload := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAnthropicError(w http.ResponseWriter, code int, msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = http.StatusText(code)
	}
	writeJSON(w, code, map[string]any{"type": "error", "error": map[string]any{"type": anthropicErrorType(code), "message": msg}})
}

func anthropicErrorType(code int) string {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_error"
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnsupportedMediaType:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

func writeOpenAIError(w http.ResponseWriter, code int, msg string) {
	if strings.TrimSpace(msg) == "" {
		msg = http.StatusText(code)
	}
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": "api_error", "code": code}})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/ui/") || r.URL.Path == "/" {
			return
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		level := "info"
		if status >= 500 {
			level = "error"
		} else if status >= 400 {
			level = "warn"
		}
		now := time.Now()
		stat := takeRequestStat(r)
		appendUILog(uiLogRow{
			AtUnixMS:     now.UnixMilli(),
			TS:           now.Format("15:04:05.000"),
			Level:        level,
			Method:       r.Method,
			Path:         r.URL.Path,
			Status:       status,
			DurMS:        time.Since(start).Milliseconds(),
			IP:           clientIP(r),
			Note:         takeRequestNote(r),
			Model:        stat.Model,
			Upstream:     stat.Upstream,
			Stream:       stat.Stream,
			InputTokens:  stat.InputTokens,
			OutputTokens: stat.OutputTokens,
			StopReason:   stat.StopReason,
		})
	})
}

func appendUILog(row uiLogRow) {
	logMu.Lock()
	defer logMu.Unlock()
	logRows = append(logRows, row)
	if len(logRows) > 300 {
		logRows = logRows[len(logRows)-300:]
	}
}

type traceIDContextKey struct{}

func newTraceID() string {
	return "tr_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func withTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDContextKey{}, id)
}

func traceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(traceIDContextKey{}).(string)
	return id
}

func traceLog(ctx context.Context, stage string, fields map[string]any) {
	traceLogID(traceIDFromContext(ctx), stage, fields)
}

func traceLogID(id, stage string, fields map[string]any) {
	if id == "" {
		return
	}
	row := map[string]any{
		"ts":    time.Now().Format(time.RFC3339Nano),
		"trace": id,
		"stage": stage,
	}
	for k, v := range fields {
		row[k] = v
	}
	raw, _ := json.Marshal(row)
	traceMu.Lock()
	defer traceMu.Unlock()
	rotateTraceLogLocked()
	f, err := os.OpenFile(traceLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.Printf("trace log open failed: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

func rotateTraceLogLocked() {
	st, err := os.Stat(traceLogPath)
	if err != nil || st.Size() < traceMaxBytes {
		return
	}
	_ = os.Rename(traceLogPath, traceLogPath+".old")
}

func safeHeaderSummary(headers http.Header) map[string]any {
	out := map[string]any{}
	for name, values := range headers {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "x-api-key" || lower == "anthropic-api-key" || lower == "api-key" {
			shapes := []string{}
			for _, value := range values {
				shapes = append(shapes, secretShape(value))
			}
			out[name] = shapes
			continue
		}
		if lower == "cookie" || lower == "set-cookie" {
			out[name] = []string{"[redacted]"}
			continue
		}
		out[name] = values
	}
	return out
}

func summarizeAnthropicRequest(in anthropicRequest) map[string]any {
	messages := []map[string]any{}
	for i, msg := range in.Messages {
		text := contentToText(msg.Content)
		messages = append(messages, map[string]any{
			"index":        i,
			"role":         msg.Role,
			"content_type": fmt.Sprintf("%T", msg.Content),
			"text_chars":   len(text),
			"text_preview": truncateString(text, 500),
		})
	}
	tools := []string{}
	for _, tool := range in.Tools {
		tools = append(tools, firstNonEmpty(tool.Name, tool.Type))
	}
	effort := ""
	if in.OutputConfig != nil {
		effort = in.OutputConfig.Effort
	}
	return map[string]any{
		"model":        in.Model,
		"stream":       in.Stream,
		"max_tokens":   in.MaxTokens,
		"temperature":  in.Temperature,
		"system_chars": len(contentToText(in.System)),
		"messages":     messages,
		"tools":        tools,
		"tool_count":   len(in.Tools),
		"effort":       effort,
		"speed":        in.Speed,
		"fast_mode":    in.FastMode,
	}
}

func summarizeResponsesRequest(out responsesRequest) map[string]any {
	tools := []string{}
	for _, tool := range out.Tools {
		tools = append(tools, firstNonEmpty(tool.Name, tool.Type))
	}
	effort := ""
	summary := ""
	if out.Reasoning != nil {
		effort = out.Reasoning.Effort
		summary = out.Reasoning.Summary
	}
	return map[string]any{
		"model":              out.Model,
		"stream":             out.Stream,
		"store":              out.Store,
		"instructions_chars": len(out.Instructions),
		"input_items":        len(out.Input),
		"tools":              tools,
		"tool_count":         len(out.Tools),
		"reasoning_effort":   effort,
		"reasoning_summary":  summary,
		"service_tier":       out.ServiceTier,
		"temperature":        out.Temperature,
	}
}

func summarizeOpenAIRequest(out openAIRequest) map[string]any {
	return map[string]any{
		"model":            out.Model,
		"stream":           out.Stream,
		"message_count":    len(out.Messages),
		"tool_count":       len(out.Tools),
		"max_tokens":       out.MaxTokens,
		"reasoning_effort": out.ReasoningEffort,
	}
}

func summarizeResponsesResponse(resp responsesResponse) map[string]any {
	items := []map[string]any{}
	for i, item := range resp.Output {
		text := ""
		for _, part := range item.Content {
			text += part.Text
		}
		thinking := reasoningSummaryText(item)
		items = append(items, map[string]any{
			"index":            i,
			"type":             item.Type,
			"role":             item.Role,
			"id":               item.ID,
			"call_id":          item.CallID,
			"name":             item.Name,
			"arguments_chars":  len(item.Arguments),
			"arguments":        truncateString(item.Arguments, 1000),
			"text_chars":       len(text),
			"text_preview":     truncateString(text, 1000),
			"thinking_chars":   len(thinking),
			"thinking_preview": truncateString(thinking, 1000),
		})
	}
	return map[string]any{
		"id":            resp.ID,
		"model":         resp.Model,
		"output_items":  items,
		"input_tokens":  resp.Usage.InputTokens,
		"output_tokens": resp.Usage.OutputTokens,
	}
}

func summarizeResponsesStreamEvent(event responsesStreamEvent) map[string]any {
	out := map[string]any{
		"type":          event.Type,
		"delta_chars":   len(event.Delta),
		"text_chars":    len(event.Text),
		"output_index":  event.OutputIndex,
		"summary_index": event.SummaryIndex,
		"item_id":       event.ItemID,
	}
	if event.Delta != "" {
		out["delta_preview"] = truncateString(event.Delta, 500)
	}
	if event.Text != "" {
		out["text_preview"] = truncateString(event.Text, 500)
	}
	if event.Arguments != "" {
		out["arguments_chars"] = len(event.Arguments)
		out["arguments"] = truncateString(event.Arguments, 1000)
	}
	if event.Item.Type != "" {
		out["item"] = map[string]any{
			"type":            event.Item.Type,
			"id":              event.Item.ID,
			"call_id":         event.Item.CallID,
			"name":            event.Item.Name,
			"arguments_chars": len(event.Item.Arguments),
			"arguments":       truncateString(event.Item.Arguments, 1000),
			"content_parts":   len(event.Item.Content),
			"summary_parts":   len(event.Item.Summary),
		}
	}
	if event.Response.ID != "" {
		out["response_id"] = event.Response.ID
		out["response_model"] = event.Response.Model
		out["response_output_count"] = len(event.Response.Output)
	}
	return out
}

func truncateString(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "...[truncated]"
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func handleUI(w http.ResponseWriter, r *http.Request) {
	uiDir := filepath.Join(".", "ui")
	if r.URL.Path == "/" {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(uiDir, "Codex Proxy Control Panel.html"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/ui/") {
		w.Header().Set("Cache-Control", "no-cache")
		http.StripPrefix("/ui/", http.FileServer(http.Dir(uiDir))).ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

func handleUIStatus(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		auth := codexAuthMetadata(cfg)
		claudeVersion := commandOutput(2*time.Second, "claude", "--version")
		writeJSON(w, http.StatusOK, map[string]any{
			"running":          proxyEnabled.Load(),
			"proxy_running":    proxyEnabled.Load(),
			"pid":              os.Getpid(),
			"uptime_seconds":   int(time.Since(startedAt).Seconds()),
			"local_url":        "http://127.0.0.1:" + cfg.Port,
			"port":             cfg.Port,
			"upstream":         cfg.Upstream,
			"codex_auth":       auth,
			"claude_settings":  claudeSettingsMetadata(cfg),
			"dashboard":        dashboardMetrics(),
			"models":           modelRows(cfg),
			"last_request":     lastRequest(),
			"claude_version":   strings.TrimSpace(claudeVersion),
			"proxy_key":        cfg.ProxyKey,
			"proxy_key_masked": maskSecret(cfg.ProxyKey),
		})
	}
}

func claudeSettingsMetadata(cfg config) map[string]any {
	settingsPath := filepath.Join(os.Getenv("USERPROFILE"), ".claude", "settings.json")
	cachePath := filepath.Join(os.Getenv("USERPROFILE"), ".claude", "cache", "gateway-models.json")
	info := map[string]any{
		"path":                  settingsPath,
		"exists":                false,
		"mode":                  "none",
		"api_key_present":       false,
		"auth_token_present":    false,
		"auth_token_matches":    false,
		"base_url":              "",
		"gateway_cache_present": fileExists(cachePath),
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return info
	}
	info["exists"] = true
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if json.Unmarshal(raw, &settings) != nil {
		return info
	}
	apiKey := strings.TrimSpace(settings.Env["ANTHROPIC_API_KEY"])
	authToken := strings.TrimSpace(settings.Env["ANTHROPIC_AUTH_TOKEN"])
	info["api_key_present"] = apiKey != ""
	info["auth_token_present"] = authToken != ""
	info["auth_token_matches"] = cfg.ProxyKey != "" && authToken == cfg.ProxyKey
	info["base_url"] = settings.Env["ANTHROPIC_BASE_URL"]
	switch {
	case authToken != "":
		info["mode"] = "anthropic_auth_token"
	case apiKey != "":
		info["mode"] = "anthropic_api_key"
	}
	return info
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func handleUIConfig(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			vals := readEnvMap()
			if vals["PROXY_API_KEY"] != "" {
				vals["PROXY_API_KEY"] = maskSecret(vals["PROXY_API_KEY"])
			}
			writeJSON(w, http.StatusOK, map[string]any{"config": vals, "secrets": map[string]string{"PROXY_API_KEY": cfg.ProxyKey}, "aliases": aliasesFromEnv(readEnvMap())})
		case http.MethodPost:
			var body struct {
				Config  map[string]string                    `json:"config"`
				Aliases []struct{ From, To, Context string } `json:"aliases"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			current := readEnvMap()
			for k, v := range body.Config {
				if k == "PROXY_API_KEY" && strings.Contains(v, "...") {
					continue
				}
				current[k] = v
			}
			applyAliases(current, body.Aliases)
			if err := writeEnvMap(current); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Configuration saved. Restart proxy to apply changes."})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

func handleUIModels(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"models": modelRows(cfg)})
	}
}

func handleUILogs(w http.ResponseWriter, r *http.Request) {
	logMu.Lock()
	structured := append([]uiLogRow(nil), logRows...)
	logMu.Unlock()
	stdout, _ := os.ReadFile(".proxy.log")
	stderr, _ := os.ReadFile(".proxy.err.log")
	trace, _ := os.ReadFile(traceLogPath)
	writeJSON(w, http.StatusOK, map[string]any{"rows": structured, "stdout": string(stdout), "stderr": string(stderr), "trace": string(trace)})
}

func handleUIValidate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		steps := []map[string]any{}
		base := "http://127.0.0.1:" + cfg.Port
		headers := map[string]string{"x-api-key": cfg.ProxyKey, "anthropic-version": "2023-06-01", "Content-Type": "application/json"}
		modelAlias := "sonnet[1m]"
		steps = append(steps, validateHTTP(http.MethodGet, base+"/v1/models", headers, nil, "GET /v1/models"))
		body := []byte(`{"model":"sonnet[1m]","messages":[{"role":"user","content":"Quick count test"}]}`)
		steps = append(steps, validateHTTP(http.MethodPost, base+"/v1/messages/count_tokens", headers, body, "POST /v1/messages/count_tokens"))
		body = []byte(`{"model":"sonnet[1m]","max_tokens":32,"messages":[{"role":"user","content":"Say hello in one sentence."}]}`)
		steps = append(steps, validateHTTP(http.MethodPost, base+"/v1/messages", headers, body, "POST /v1/messages"))
		body = []byte(`{"model":"sonnet[1m]","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Say streaming hello."}]}`)
		steps = append(steps, validateHTTP(http.MethodPost, base+"/v1/messages", headers, body, "POST /v1/messages (stream)"))
		ok := true
		for _, step := range steps {
			if step["ok"] != true {
				ok = false
				break
			}
		}
		summary := map[string]any{
			"ok":             ok,
			"ran_at":         time.Now().Format("15:04:05"),
			"model":          modelAlias,
			"upstream_model": firstNonEmpty(cfg.Models[modelAlias], modelAlias),
			"steps":          steps,
			"duration_total": validationDurationTotal(steps),
		}
		validationMu.Lock()
		lastValidation = summary
		validationMu.Unlock()
		writeJSON(w, http.StatusOK, summary)
	}
}

func validationDurationTotal(steps []map[string]any) int64 {
	var total int64
	for _, step := range steps {
		switch v := step["duration_ms"].(type) {
		case int64:
			total += v
		case int:
			total += int64(v)
		case float64:
			total += int64(v)
		}
	}
	return total
}

func handleUITest(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		if body.Model == "" {
			body.Model = "gpt-5.3-codex"
		}
		if body.Prompt == "" {
			body.Prompt = "Reply with only OK."
		}
		payload, _ := json.Marshal(map[string]any{"model": body.Model, "max_tokens": 1024, "stream": body.Stream, "messages": []map[string]string{{"role": "user", "content": body.Prompt}}})
		start := time.Now()
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+cfg.Port+"/v1/messages", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", cfg.ProxyKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		writeJSON(w, http.StatusOK, map[string]any{"status": resp.StatusCode, "duration_ms": time.Since(start).Milliseconds(), "raw": string(raw), "text": extractAnthropicText(raw, body.Stream)})
	}
}

func handleUIStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if err := runClaudeSettingsSync("Restore"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	proxyEnabled.Store(false)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": false, "message": "Proxy endpoints stopped. Claude settings restored. Control panel remains online."})
}

func handleUIStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if err := runClaudeSettingsSync("Apply"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	proxyEnabled.Store(true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "running": true, "message": "Proxy endpoints started. Claude settings applied."})
}

func handleUIRestart(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		script := filepath.Join(mustGetwd(), "start-proxy.ps1")
		cmd := exec.Command("pwsh", "-NoProfile", "-Command", "Start-Sleep -Seconds 1; & '"+strings.ReplaceAll(script, "'", "''")+"'")
		_ = cmd.Start()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Proxy restarting."})
		go func() { time.Sleep(250 * time.Millisecond); os.Exit(0) }()
	}
}

func validateHTTP(method, url string, headers map[string]string, body []byte, name string) map[string]any {
	start := time.Now()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return map[string]any{"name": name, "ok": false, "status": 0, "duration_ms": time.Since(start).Milliseconds(), "message": err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return map[string]any{"name": name, "ok": resp.StatusCode >= 200 && resp.StatusCode < 300, "status": resp.StatusCode, "duration_ms": time.Since(start).Milliseconds(), "message": strings.TrimSpace(string(raw))}
}

func runClaudeSettingsSync(action string) error {
	script := filepath.Join(mustGetwd(), "sync-claude-settings.ps1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pwsh", "-NoProfile", "-File", script, "-Action", action)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("Claude settings %s timed out", strings.ToLower(action))
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("Claude settings %s failed: %s", strings.ToLower(action), msg)
	}
	return nil
}

func codexAuthMetadata(cfg config) map[string]any {
	info := map[string]any{"path": cfg.CodexAuthFile, "exists": false, "mode": "", "has_access_token": false, "has_refresh_token": false, "last_modified": ""}
	st, err := os.Stat(cfg.CodexAuthFile)
	if err != nil {
		return info
	}
	info["exists"] = true
	info["last_modified"] = st.ModTime().Format(time.RFC3339)
	raw, err := os.ReadFile(cfg.CodexAuthFile)
	if err != nil {
		return info
	}
	var parsed struct {
		AuthMode string            `json:"auth_mode"`
		Tokens   map[string]string `json:"tokens"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		info["mode"] = parsed.AuthMode
		info["has_access_token"] = parsed.Tokens["access_token"] != ""
		info["has_refresh_token"] = parsed.Tokens["refresh_token"] != ""
	}
	return info
}

func readEnvMap() map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(".env")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

func writeEnvMap(vals map[string]string) error {
	keys := []string{"UPSTREAM", "CODEX_BASE_URL", "CODEX_AUTH_FILE", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_CLAUDE_SONNET_MODEL", "OPENAI_CLAUDE_SONNET_1M_MODEL", "OPENAI_CLAUDE_HAIKU_MODEL", "OPENAI_CLAUDE_OPUS_MODEL", "OPENAI_CLAUDE_OPUS_1M_MODEL", "OPENAI_CLAUDE_FAST_MODEL", "OPENAI_CLAUDE_CODEX_MODEL", "CODEX_FAST_SERVICE_TIER", "CODEX_WEB_SEARCH_TOOL_TYPE", "CODEX_WEB_SEARCH_CONTEXT_SIZE", "CODEX_REASONING_SUMMARY", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES", "ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES", "ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES", "CLAUDE_CODE_EFFORT_LEVEL", "OPENAI_REASONING_EFFORT", "API_TIMEOUT_MS", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "PROXY_API_KEY", "PROXY_PORT"}
	var b strings.Builder
	for _, k := range keys {
		if v, ok := vals[k]; ok {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	return os.WriteFile(".env", []byte(b.String()), 0600)
}

func aliasesFromEnv(vals map[string]string) []map[string]string {
	return []map[string]string{
		{"from": "opus", "to": firstNonEmpty(vals["OPENAI_CLAUDE_OPUS_MODEL"], "gpt-5.5"), "context": contextFromModel(firstNonEmpty(vals["ANTHROPIC_DEFAULT_OPUS_MODEL"], "claude-opus-4-7[1m]"))},
		{"from": "sonnet", "to": firstNonEmpty(vals["OPENAI_CLAUDE_SONNET_MODEL"], vals["OPENAI_CLAUDE_3_7_MODEL"], "gpt-5.5"), "context": contextFromModel(firstNonEmpty(vals["ANTHROPIC_DEFAULT_SONNET_MODEL"], "claude-sonnet-4-6[1m]"))},
		{"from": "haiku", "to": firstNonEmpty(vals["OPENAI_CLAUDE_HAIKU_MODEL"], vals["OPENAI_CLAUDE_3_5_HAIKU_MODEL"], "gpt-5.4-mini"), "context": "200k"},
		{"from": "gpt-5.3-codex", "to": firstNonEmpty(vals["OPENAI_CLAUDE_CODEX_MODEL"], "gpt-5.3-codex"), "context": "1m"},
	}
}

func applyAliases(vals map[string]string, aliases []struct{ From, To, Context string }) {
	for _, a := range aliases {
		switch a.From {
		case "opus", "claude-opus-4-7", "claude-opus-4-7[1m]", "opus[1m]":
			vals["OPENAI_CLAUDE_OPUS_MODEL"] = a.To
			vals["OPENAI_CLAUDE_OPUS_1M_MODEL"] = a.To
			vals["ANTHROPIC_DEFAULT_OPUS_MODEL"] = claudeDefaultModel("claude-opus-4-7", a.Context)
		case "sonnet", "claude-sonnet-4-6", "claude-sonnet-4-6[1m]", "sonnet[1m]", "claude-3-7-sonnet-latest":
			vals["OPENAI_CLAUDE_SONNET_MODEL"] = a.To
			vals["OPENAI_CLAUDE_SONNET_1M_MODEL"] = a.To
			vals["ANTHROPIC_DEFAULT_SONNET_MODEL"] = claudeDefaultModel("claude-sonnet-4-6", a.Context)
		case "haiku", "claude-haiku-4-5", "claude-haiku-4-5-20251001", "claude-3-5-haiku-latest":
			vals["OPENAI_CLAUDE_HAIKU_MODEL"] = a.To
			vals["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = "claude-haiku-4-5"
		case "gpt-5.3-codex":
			vals["OPENAI_CLAUDE_CODEX_MODEL"] = a.To
		}
	}
}

func modelRows(cfg config) []map[string]any {
	supported := map[string]bool{"gpt-5.5": true, "gpt-5.4": true, "gpt-5.4-mini": true, "gpt-5.3-codex": true}
	rows := []map[string]any{}
	for alias, real := range cfg.Models {
		if !isAdvertisedModel(alias) {
			continue
		}
		status := "unsupported"
		if supported[real] {
			status = "ok"
		} else if real != "" {
			status = "untested"
		}
		rows = append(rows, map[string]any{"alias": alias, "real": real, "status": status, "context": contextForAlias(cfg, alias), "default": alias == "sonnet" || alias == "sonnet[1m]", "recommended": alias == "gpt-5.3-codex"})
	}
	return rows
}

func isAdvertisedModel(alias string) bool {
	lower := strings.ToLower(alias)
	if strings.HasPrefix(lower, "claude-3-") || strings.HasPrefix(lower, "claude-instant") {
		return false
	}
	return lower != "claude-opus-4-6" && lower != "claude-opus-4-6[1m]"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func contextFromModel(model string) string {
	if strings.Contains(strings.ToLower(model), "[1m]") {
		return "1m"
	}
	return "200k"
}

func claudeDefaultModel(model, context string) string {
	if strings.EqualFold(context, "1m") && !strings.Contains(strings.ToLower(model), "[1m]") {
		return model + "[1m]"
	}
	return strings.ReplaceAll(model, "[1m]", "")
}

func requestReasoningEffort(cfg config, in anthropicRequest) string {
	if in.OutputConfig != nil && in.OutputConfig.Effort != "" {
		return normalizeReasoningEffort(in.OutputConfig.Effort)
	}
	return normalizeReasoningEffort(cfg.ReasoningEffort)
}

func normalizeReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return strings.ToLower(strings.TrimSpace(effort))
	case "max":
		return "xhigh"
	case "auto", "":
		return ""
	default:
		return "xhigh"
	}
}

func contextForAlias(cfg config, alias string) string {
	lower := strings.ToLower(alias)
	if lower == "opus" || lower == "claude-opus-4-7" {
		return contextFromModel(cfg.ClaudeDefaults["opus"])
	}
	if lower == "sonnet" || lower == "claude-sonnet-4-6" {
		return contextFromModel(cfg.ClaudeDefaults["sonnet"])
	}
	if strings.Contains(lower, "[1m]") || lower == "gpt-5.3-codex" {
		return "1m"
	}
	return "200k"
}

func lastRequest() any {
	logMu.Lock()
	defer logMu.Unlock()
	if len(logRows) == 0 {
		return nil
	}
	return logRows[len(logRows)-1]
}

func dashboardMetrics() map[string]any {
	now := time.Now()
	start60 := now.Add(-60 * time.Second).UnixMilli()
	start120 := now.Add(-120 * time.Second).UnixMilli()

	logMu.Lock()
	rows := append([]uiLogRow(nil), logRows...)
	logMu.Unlock()

	var current, previous []uiLogRow
	bars := make([]int, 60)
	latencyBars := make([]int64, 60)
	latencyCounts := make([]int, 60)
	tokenBars := make([]int, 60)
	errorBars := make([]int, 60)
	for _, row := range rows {
		if !isDashboardTraffic(row) {
			continue
		}
		if row.AtUnixMS >= start60 {
			current = append(current, row)
			idx := int((row.AtUnixMS - start60) / 1000)
			if idx >= 0 && idx < len(bars) {
				bars[idx]++
				latencyBars[idx] += row.DurMS
				latencyCounts[idx]++
				tokenBars[idx] += row.InputTokens + row.OutputTokens
				if row.Status >= 400 {
					errorBars[idx]++
				}
			}
		} else if row.AtUnixMS >= start120 && row.AtUnixMS < start60 {
			previous = append(previous, row)
		}
	}

	latencySpark := make([]int, 60)
	for i := range latencySpark {
		if latencyCounts[i] > 0 {
			latencySpark[i] = int(latencyBars[i] / int64(latencyCounts[i]))
		}
	}

	stats := summarizeTrafficRows(current)
	prev := summarizeTrafficRows(previous)
	stats["window_seconds"] = 60
	stats["generated_at"] = now.Format(time.RFC3339)
	stats["requests_delta_pct"] = percentDelta(asFloat(stats["requests_per_min"]), asFloat(prev["requests_per_min"]))
	stats["avg_latency_delta_pct"] = percentDelta(asFloat(stats["avg_latency_ms"]), asFloat(prev["avg_latency_ms"]))
	stats["previous_requests_per_min"] = prev["requests_per_min"]
	stats["sparks"] = map[string]any{
		"requests": bars,
		"latency":  latencySpark,
		"tokens":   tokenBars,
		"errors":   errorBars,
	}
	stats["last_validation"] = currentValidationSummary()
	return stats
}

func isDashboardTraffic(row uiLogRow) bool {
	return strings.HasPrefix(row.Path, "/v1/")
}

func summarizeTrafficRows(rows []uiLogRow) map[string]any {
	total := len(rows)
	var durSum int64
	var status2xx, status4xx, status5xx, streamed, inTokens, outTokens, errors int
	for _, row := range rows {
		durSum += row.DurMS
		switch {
		case row.Status >= 200 && row.Status < 300:
			status2xx++
		case row.Status >= 400 && row.Status < 500:
			status4xx++
			errors++
		case row.Status >= 500:
			status5xx++
			errors++
		}
		if row.Stream {
			streamed++
		}
		inTokens += row.InputTokens
		outTokens += row.OutputTokens
	}
	avgLatency := int64(0)
	if total > 0 {
		avgLatency = durSum / int64(total)
	}
	return map[string]any{
		"requests_per_min": total,
		"avg_latency_ms":   avgLatency,
		"error_rate":       percent(float64(errors), float64(total)),
		"error_count":      errors,
		"tokens_per_min":   inTokens + outTokens,
		"input_tokens":     inTokens,
		"output_tokens":    outTokens,
		"traffic": map[string]any{
			"total":          total,
			"status_2xx":     status2xx,
			"status_2xx_pct": percent(float64(status2xx), float64(total)),
			"status_4xx":     status4xx,
			"status_4xx_pct": percent(float64(status4xx), float64(total)),
			"status_5xx":     status5xx,
			"status_5xx_pct": percent(float64(status5xx), float64(total)),
			"streamed":       streamed,
			"streamed_pct":   percent(float64(streamed), float64(total)),
		},
	}
}

func percent(part, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return part / total * 100
}

func percentDelta(current, previous float64) float64 {
	if previous <= 0 {
		if current <= 0 {
			return 0
		}
		return 100
	}
	return (current - previous) / previous * 100
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	default:
		return 0
	}
}

func currentValidationSummary() map[string]any {
	validationMu.Lock()
	defer validationMu.Unlock()
	if lastValidation == nil {
		return map[string]any{"ok": false, "ran_at": "", "model": "", "upstream_model": "", "steps": []any{}}
	}
	return lastValidation
}

func maskSecret(s string) string {
	if len(s) <= 8 {
		return "••••••••"
	}
	return s[:4] + "..." + s[len(s)-4:]
}

func commandOutput(timeout time.Duration, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

func mustGetwd() string { wd, _ := os.Getwd(); return wd }

func extractAnthropicText(raw []byte, stream bool) string {
	if stream {
		var b strings.Builder
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var ev struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev)
			b.WriteString(ev.Delta.Text)
		}
		return b.String()
	}
	var msg struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &msg)
	var b strings.Builder
	for _, c := range msg.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func cleanModel(model string) string { return strings.TrimPrefix(model, "openai/") }

func requestNote(alias, upstream string, stream bool, effort string) string {
	parts := []string{"alias=" + firstNonEmpty(alias, upstream), "upstream=" + upstream, "stream=" + strconv.FormatBool(stream)}
	if effort != "" {
		parts = append(parts, "effort="+effort)
	}
	return strings.Join(parts, " ")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
