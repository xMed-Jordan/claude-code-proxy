package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

var (
	startedAt            = time.Now()
	proxyEnabled         atomic.Bool
	requestNotes         sync.Map
	requestStats         sync.Map
	requestProviderKeys  sync.Map
	logMu                sync.Mutex
	logRows              []uiLogRow
	traceMu              sync.Mutex
	modelConfigMu        sync.RWMutex
	validationMu         sync.Mutex
	lastValidation       map[string]any
	antigravityMu        sync.Mutex
	antigravityLastProbe map[string]any
	updateMu             sync.Mutex
	latestVersionCache   updateLatestCache
)

const (
	traceLogPath           = ".proxy.trace.log"
	traceBodyLimit         = 32768
	traceMaxBytes          = 10 * 1024 * 1024
	sseMaxLineSize         = 64 * 1024 * 1024
	antigravityExtensionID = "eeijfnjmjelapkebgockoeaadonbchdd"
	appVersionFallback     = "0.0.0-dev"
	defaultVersionURL      = "https://raw.githubusercontent.com/xMed-Jordan/claude-code-proxy/main/VERSION"
	defaultTokenHardLimit  = 200000
	defaultCodexWindow     = 200000
)

type updateLatestCache struct {
	Version   string
	CheckedAt time.Time
	Error     string
}

type config struct {
	OpenAIAPIKey      string
	OpenAIBaseURL     string
	Upstream          string
	CodexDisabled     bool // PROXY_CODEX_ENABLED=0 → refuse codex routing (zero value = enabled)
	ClaudeDisabled    bool // PROXY_CLAUDE_ENABLED=0 → refuse the claude upstream
	AgyDisabled       bool // PROXY_AGY_ENABLED=0 → refuse the agy upstream
	CodexBaseURL      string
	CodexAuthFile     string
	CodexSessionFile  string
	DBPath            string
	ProxyKey          string
	Host              string
	Port              string
	PublicURL         string
	Models            map[string]string
	ModelContexts     map[string]string
	ModelCustom       map[string]bool
	ModelForward      map[string]string
	ModelDefault      map[string]bool // alias → is the default choice (overrides the built-in seed)
	ModelRecommended  map[string]bool // alias → is the recommended choice
	ClaudeDefaults    map[string]string
	ReasoningEffort   string
	AgyBin            string        // path to the agyj wrapper binary ("" → sibling of proxy exe, else PATH)
	AgyCLI            string        // path to the real agy CLI, injected as AGYJ_AGY_BIN ("" → let agyj self-resolve)
	AgyModel          string        // optional global model override forwarded to agy ("" → agy's own default)
	AgyConcurrency    int           // max simultaneous agyj subprocesses
	AgyTimeout        time.Duration // per-call agy execution timeout
	AgyMedia          bool          // enable media attachments for agy requests
	AgyMediaDir       string        // scratch root for materialized media ("" → temp)
	AgyMediaModel     string        // default Antigravity model for media requests
	AgyMediaTimeout   time.Duration // per-call timeout for media (heavier) requests
	AgyMediaMaxBytes  int64         // max bytes per attached/extracted file
	AgyMediaMaxTotal  int64         // max total bytes materialized per request
	AgyMediaAllowURLs bool          // allow downloading media from remote URLs
	AgyMediaRetention time.Duration // keep materialized files this long for reuse
	AgyImage          bool          // enable the agy image-generation endpoint
	AgyImageDir       string        // served root for generated images ("" → temp)
	AgyImageModel     string        // default Antigravity model for image generation
	AgyImageRetention time.Duration // keep generated images this long for reuse
	// Claude (Anthropic) upstream — when a model alias's forward_to == "claude",
	// the request is served by the local Claude Code CLI (`claude -p`) backed by a
	// Claude subscription (long-lived OAuth token from `claude setup-token`).
	// Chat-only, stateless, tools disabled. See claude.go.
	ClaudeBin          string        // path to the `claude` CLI ("" → "claude" on PATH)
	ClaudeModel        string        // optional global model override ("" → alias Real, if a Claude name)
	ClaudeConcurrency  int           // max simultaneous claude subprocesses
	ClaudeTimeout      time.Duration // per-call claude execution timeout
	ClaudeOAuthToken   string        // long-lived OAuth token, injected as CLAUDE_CODE_OAUTH_TOKEN
	ClaudeWorkDir      string        // working dir for the child ("" → temp; keeps ambient CLAUDE.md out)
	ClaudeSystemPrompt string        // optional system-prompt override ("" → CLI default identity)
	ClaudeSafeMode     bool          // pass --safe-mode (disable ambient CLAUDE.md/skills/plugins, keep auth)
	ClaudeExtraArgs    string        // optional extra flags appended to every `claude` invocation
	// Groq Whisper speech-to-text for audio/video on the agy media path. agy's CLI
	// has no native audio understanding; rather than let it improvise (slow, fragile),
	// we transcribe audio/video with Groq's whisper-large-v3 (free tier, ~1s) before
	// handing anything to agy. Disabled when GroqAPIKey is empty.
	GroqAPIKey      string        // PROXY_GROQ_API_KEY ("" → STT disabled, audio falls through)
	GroqSTTModel    string        // Groq transcription model (default whisper-large-v3)
	GroqSTTLanguage string        // ISO-639-1 hint, e.g. "ar" ("" → auto-detect)
	GroqSTTPrompt   string        // optional context prompt to bias vocabulary (domain terms)
	GroqSTTTimeout  time.Duration // per-clip transcription timeout
	// VirusTotal malware scanning for media attachments (keys editable at runtime).
	VirusTotalEnabled    bool
	VirusTotalKeys       string        // newline/comma-separated API keys (load-balanced)
	VirusTotalThreshold  int           // reject when >= this many engines flag the file
	VirusTotalUpload     bool          // upload files unknown to VT (else hash-only)
	VirusTotalMaxWait    time.Duration // bound on upload+analysis polling
	VirusTotalFailClosed bool          // reject when scanning is unavailable
	AdminUsername        string
	AdminPasswordHash    string
	AdminSessionSecret   string
}

type modelAliasConfig struct {
	Alias       string `json:"alias"`
	Real        string `json:"real"`
	Context     string `json:"context,omitempty"`
	ForwardTo   string `json:"forward_to,omitempty"`
	Default     bool   `json:"default,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
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
	User                string          `json:"user,omitempty"`
	Metadata            map[string]any  `json:"metadata,omitempty"`
	ExtraBody           any             `json:"extra_body,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Name       string           `json:"name,omitempty"`
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
	Model           string              `json:"model"`
	Instructions    string              `json:"instructions,omitempty"`
	Input           []any               `json:"input"`
	Tools           []responsesTool     `json:"tools,omitempty"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	MaxOutputTokens int                 `json:"max_output_tokens,omitempty"`
	ServiceTier     string              `json:"service_tier,omitempty"`
	PromptCacheKey  string              `json:"prompt_cache_key,omitempty"`
	Stream          bool                `json:"stream,omitempty"`
	Store           bool                `json:"store"`
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
	ID          string                `json:"id"`
	Model       string                `json:"model"`
	Output      []responsesOutputItem `json:"output"`
	Error       responsesStreamError  `json:"error,omitempty"`
	StreamError string                `json:"-"`
	Usage       struct {
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
	Status    string                   `json:"status,omitempty"`
	Action    any                      `json:"action,omitempty"`
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
	Type         string               `json:"type"`
	Code         string               `json:"code"`
	Message      string               `json:"message"`
	Delta        string               `json:"delta"`
	Text         string               `json:"text"`
	Arguments    string               `json:"arguments"`
	ItemID       string               `json:"item_id"`
	OutputIndex  int                  `json:"output_index"`
	SummaryIndex int                  `json:"summary_index"`
	Item         responsesOutputItem  `json:"item"`
	Response     responsesResponse    `json:"response"`
	Error        responsesStreamError `json:"error"`
}

type responsesStreamError struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   string `json:"param,omitempty"`
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
	loadDotEnvIntoProcess()
	if handled, err := runCLI(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := runServe(); err != nil {
		log.Fatal(err)
	}
}

func newProxyMux(cfg config) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/anthropic/v1/models", requireAuth(cfg, requireProxyEnabled(handleModels(cfg))))
	mux.HandleFunc("/anthropic/v1/model-capabilities", requireAuth(cfg, requireProxyEnabled(handleModelCapabilities(cfg))))
	mux.HandleFunc("/anthropic/v1/messages", requireAuth(cfg, requireProxyEnabled(handleMessages(cfg))))
	mux.HandleFunc("/anthropic/v1/messages/count_tokens", requireAuth(cfg, requireProxyEnabled(handleCountTokens)))
	mux.HandleFunc("/openai/v1/models", requireAuth(cfg, requireProxyEnabled(handleModels(cfg))))
	mux.HandleFunc("/openai/v1/model-capabilities", requireAuth(cfg, requireProxyEnabled(handleModelCapabilities(cfg))))
	mux.HandleFunc("/openai/v1/chat/completions", requireAuth(cfg, requireProxyEnabled(handleChatCompletions(cfg))))
	mux.HandleFunc("/openai/v1/responses", requireAuth(cfg, requireProxyEnabled(handleResponses(cfg))))
	mux.HandleFunc("/openai/v1/files", requireAuth(cfg, requireProxyEnabled(handleOpenAIFiles(cfg))))
	mux.HandleFunc("/openai/v1/files/", requireAuth(cfg, requireProxyEnabled(handleOpenAIFiles(cfg))))
	mux.HandleFunc("/v1/images/generations", requireAuth(cfg, requireProxyEnabled(handleImageGenerations(cfg))))
	mux.HandleFunc("/openai/v1/images/generations", requireAuth(cfg, requireProxyEnabled(handleImageGenerations(cfg))))
	mux.HandleFunc("/media/generated/", handleGeneratedImage(cfg))
	mux.HandleFunc("/ui/api/auth/status", noStore(handleUIAuthStatus(cfg)))
	mux.HandleFunc("/ui/api/auth/setup", noStore(handleUIAuthSetup(cfg)))
	mux.HandleFunc("/ui/api/auth/login", noStore(handleUIAuthLogin(cfg)))
	mux.HandleFunc("/ui/api/auth/logout", noStore(handleUIAuthLogout(cfg)))
	mux.HandleFunc("/ui/api/status", noStore(requireAdmin(cfg, handleUIStatus(cfg))))
	mux.HandleFunc("/ui/api/analytics", noStore(requireAdmin(cfg, handleUIAnalytics(cfg))))
	mux.HandleFunc("/ui/api/config", noStore(requireAdmin(cfg, handleUIConfig(cfg))))
	mux.HandleFunc("/ui/api/models", noStore(requireAdmin(cfg, handleUIModels(cfg))))
	mux.HandleFunc("/ui/api/virustotal", noStore(requireAdmin(cfg, handleUIVirusTotal(cfg))))
	mux.HandleFunc("/ui/api/images/generate", noStore(requireAdmin(cfg, handleUIImageGenerate(cfg))))
	mux.HandleFunc("/ui/api/keys", noStore(requireAdmin(cfg, handleUIKeys(cfg))))
	mux.HandleFunc("/ui/api/keys/provider", noStore(requireAdmin(cfg, handleUIProviderKeys(cfg))))
	mux.HandleFunc("/ui/api/keys/client", noStore(requireAdmin(cfg, handleUIClientKeys(cfg))))
	mux.HandleFunc("/ui/api/keys/toggle", noStore(requireAdmin(cfg, handleUIKeyToggle(cfg))))
	mux.HandleFunc("/ui/api/validate", noStore(requireAdmin(cfg, handleUIValidate(cfg))))
	mux.HandleFunc("/ui/api/test", noStore(requireAdmin(cfg, handleUITest(cfg))))
	mux.HandleFunc("/ui/api/logs", noStore(requireAdmin(cfg, handleUILogs)))
	mux.HandleFunc("/ui/api/antigravity", noStore(requireAdmin(cfg, handleUIAntigravityStatus)))
	mux.HandleFunc("/ui/api/antigravity/probe", noStore(requireAdmin(cfg, handleUIAntigravityProbe)))
	mux.HandleFunc("/ui/api/update/status", noStore(requireAdmin(cfg, handleUIUpdateStatus(cfg))))
	mux.HandleFunc("/ui/api/update/check", noStore(requireAdmin(cfg, handleUIUpdateCheck(cfg))))
	mux.HandleFunc("/ui/api/update/start", noStore(requireAdmin(cfg, handleUIUpdateStart(cfg))))
	mux.HandleFunc("/ui/api/update/settings", noStore(requireAdmin(cfg, handleUIUpdateSettings(cfg))))
	mux.HandleFunc("/ui/api/settings/apply", noStore(requireAdmin(cfg, handleUISettingsApply(cfg))))
	mux.HandleFunc("/ui/api/proxy/stop", noStore(requireAdmin(cfg, handleUIStop)))
	mux.HandleFunc("/ui/api/proxy/start", noStore(requireAdmin(cfg, handleUIStart)))
	mux.HandleFunc("/ui/api/proxy/restart", noStore(requireAdmin(cfg, handleUIRestart(cfg))))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy_running": proxyEnabled.Load()})
	})
	mux.HandleFunc("/docs", handleAPIDocs(cfg))
	mux.HandleFunc("/openapi.json", handleOpenAPI(cfg))
	mux.HandleFunc("/postman.json", handlePostmanCollection(cfg))
	mux.HandleFunc("/antigravity/bridge", handleAntigravityBridge)
	mux.HandleFunc("/", handleUI)
	return mux
}

func loadConfig() config {
	port := getenv("PROXY_PORT", getenv("LITELLM_PORT", "4000"))
	host := strings.TrimSpace(getenv("PROXY_HOST", "127.0.0.1"))
	if host == "" {
		host = "127.0.0.1"
	}
	publicURL := normalizePublicBaseURL(getenv("PROXY_PUBLIC_URL", getenv("PUBLIC_BASE_URL", "")))
	baseURL := strings.TrimRight(getenv("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/")
	codexAuthFile := os.Getenv("CODEX_AUTH_FILE")
	if codexAuthFile == "" {
		codexAuthFile = defaultCodexAuthFile()
	}
	env := processEnvValue
	claudeDefaults := claudeDefaultsFromValues(env)
	models, modelContexts, modelCustom, modelForward, modelDefault, modelRecommended := modelAliasesFromValues(env)
	return config{
		OpenAIAPIKey:      os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:     baseURL,
		Upstream:          strings.ToLower(getenv("UPSTREAM", "codex")),
		CodexBaseURL:      strings.TrimRight(getenv("CODEX_BASE_URL", "https://chatgpt.com/backend-api/codex"), "/"),
		CodexAuthFile:     codexAuthFile,
		CodexSessionFile:  getenv("CODEX_SESSION_FILE", ".proxy.sessions.json"),
		DBPath:            getenv("PROXY_DB_PATH", ".proxy.db"),
		ProxyKey:          getenv("PROXY_API_KEY", os.Getenv("LITELLM_MASTER_KEY")),
		Host:              host,
		Port:              port,
		PublicURL:         publicURL,
		ReasoningEffort:   normalizeReasoningEffort(getenv("CLAUDE_CODE_EFFORT_LEVEL", getenv("OPENAI_REASONING_EFFORT", "xhigh"))),
		ClaudeDefaults:    claudeDefaults,
		Models:            models,
		ModelContexts:     modelContexts,
		ModelCustom:       modelCustom,
		ModelForward:      modelForward,
		ModelDefault:      modelDefault,
		ModelRecommended:  modelRecommended,
		AgyBin:            strings.TrimSpace(getenv("PROXY_AGY_BIN", "")),
		AgyCLI:            strings.TrimSpace(getenv("PROXY_AGY_CLI", "")),
		AgyModel:          strings.TrimSpace(getenv("PROXY_AGY_MODEL", "")),
		AgyConcurrency:    parseAgyConcurrency(getenv("PROXY_AGY_CONCURRENCY", "2")),
		AgyTimeout:        parseAgyTimeout(getenv("PROXY_AGY_TIMEOUT", "180")),
		AgyMedia:          envFlag("PROXY_AGY_MEDIA", true),
		AgyMediaDir:       strings.TrimSpace(getenv("PROXY_AGY_MEDIA_DIR", "")),
		AgyMediaModel:     strings.TrimSpace(getenv("PROXY_AGY_MEDIA_MODEL", "Gemini 3.5 Flash (Low)")),
		AgyMediaTimeout:   parseAgyTimeout(getenv("PROXY_AGY_MEDIA_TIMEOUT", "300")),
		AgyMediaMaxBytes:  parseByteSize(getenv("PROXY_AGY_MEDIA_MAX_BYTES", "1GB"), 1<<30),
		AgyMediaMaxTotal:  parseByteSize(getenv("PROXY_AGY_MEDIA_MAX_TOTAL", "1GB"), 1<<30),
		AgyMediaAllowURLs: envFlag("PROXY_AGY_MEDIA_ALLOW_URLS", true),
		AgyMediaRetention: parseAgyTimeout(getenv("PROXY_AGY_MEDIA_RETENTION", "86400")),
		AgyImage:          envFlag("PROXY_AGY_IMAGE", true),
		AgyImageDir:       strings.TrimSpace(getenv("PROXY_AGY_IMAGE_DIR", "")),
		AgyImageModel:     strings.TrimSpace(getenv("PROXY_AGY_IMAGE_MODEL", "Gemini 3.5 Flash (Low)")),
		AgyImageRetention: parseAgyTimeout(getenv("PROXY_AGY_IMAGE_RETENTION", "86400")),

		ClaudeBin:          strings.TrimSpace(getenv("PROXY_CLAUDE_BIN", "")),
		ClaudeModel:        strings.TrimSpace(getenv("PROXY_CLAUDE_MODEL", "")),
		ClaudeConcurrency:  parseAgyConcurrency(getenv("PROXY_CLAUDE_CONCURRENCY", "2")),
		ClaudeTimeout:      parseAgyTimeout(getenv("PROXY_CLAUDE_TIMEOUT", "180")),
		ClaudeOAuthToken:   strings.TrimSpace(getenv("PROXY_CLAUDE_OAUTH_TOKEN", "")),
		ClaudeWorkDir:      strings.TrimSpace(getenv("PROXY_CLAUDE_WORKDIR", "")),
		ClaudeSystemPrompt: strings.TrimSpace(getenv("PROXY_CLAUDE_SYSTEM_PROMPT", "")),
		ClaudeSafeMode:     envFlag("PROXY_CLAUDE_SAFE_MODE", false),
		ClaudeExtraArgs:    strings.TrimSpace(getenv("PROXY_CLAUDE_EXTRA_ARGS", "")),

		CodexDisabled:  !envFlag("PROXY_CODEX_ENABLED", true),
		ClaudeDisabled: !envFlag("PROXY_CLAUDE_ENABLED", true),
		AgyDisabled:    !envFlag("PROXY_AGY_ENABLED", true),

		GroqAPIKey:      strings.TrimSpace(getenv("PROXY_GROQ_API_KEY", "")),
		GroqSTTModel:    strings.TrimSpace(getenv("PROXY_GROQ_STT_MODEL", "whisper-large-v3")),
		GroqSTTLanguage: strings.TrimSpace(getenv("PROXY_GROQ_STT_LANGUAGE", "ar")),
		GroqSTTPrompt:   strings.TrimSpace(getenv("PROXY_GROQ_STT_PROMPT", "")),
		GroqSTTTimeout:  parseAgyTimeout(getenv("PROXY_GROQ_STT_TIMEOUT", "90")),

		VirusTotalEnabled:    envFlag("PROXY_VIRUSTOTAL_ENABLED", true),
		VirusTotalKeys:       strings.TrimSpace(getenv("PROXY_VIRUSTOTAL_KEYS", "")),
		VirusTotalThreshold:  parseIntDefault(getenv("PROXY_VIRUSTOTAL_THRESHOLD", "1"), 1),
		VirusTotalUpload:     envFlag("PROXY_VIRUSTOTAL_UPLOAD", true),
		VirusTotalMaxWait:    parseAgyTimeout(getenv("PROXY_VIRUSTOTAL_MAX_WAIT", "90")),
		VirusTotalFailClosed: envFlag("PROXY_VIRUSTOTAL_FAIL_CLOSED", false),

		AdminUsername:      strings.TrimSpace(getenv("ADMIN_USERNAME", "")),
		AdminPasswordHash:  strings.TrimSpace(getenv("ADMIN_PASSWORD_HASH", "")),
		AdminSessionSecret: strings.TrimSpace(getenv("ADMIN_SESSION_SECRET", "")),
	}
}

func normalizePublicBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := parsed.Hostname()
	port := parsed.Port()
	if host != "" {
		if port != "" && !isDefaultPublicPort(scheme, port) {
			parsed.Host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			parsed.Host = "[" + host + "]"
		} else {
			parsed.Host = host
		}
	}
	parsed.Scheme = scheme
	if parsed.Path == "/" {
		parsed.Path = ""
	} else {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}

func isDefaultPublicPort(scheme, port string) bool {
	return (scheme == "https" && port == "443") || (scheme == "http" && port == "80")
}

func localProxyBaseURL(cfg config) string {
	port := strings.TrimSpace(cfg.Port)
	if port == "" {
		port = "4000"
	}
	return "http://127.0.0.1:" + port
}

func clientFacingBaseURL(cfg config) string {
	if cfg.PublicURL != "" {
		return cfg.PublicURL
	}
	return localProxyBaseURL(cfg)
}

type envValueFunc func(key, fallback string) string

func processEnvValue(key, fallback string) string {
	return getenv(key, fallback)
}

func mapEnvValue(vals map[string]string) envValueFunc {
	return func(key, fallback string) string {
		if v := strings.TrimSpace(vals[key]); v != "" {
			return v
		}
		return getenv(key, fallback)
	}
}

func claudeDefaultsFromValues(env envValueFunc) map[string]string {
	return map[string]string{
		"opus":   env("ANTHROPIC_DEFAULT_OPUS_MODEL", "claude-opus-4-7[1m]"),
		"sonnet": env("ANTHROPIC_DEFAULT_SONNET_MODEL", "claude-sonnet-4-6[1m]"),
		"haiku":  env("ANTHROPIC_DEFAULT_HAIKU_MODEL", "claude-haiku-4-5"),
	}
}

func defaultModelAliasesFromValues(env envValueFunc) map[string]string {
	return map[string]string{
		"opus":                      cleanModel(env("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5")),
		"opus[1m]":                  cleanModel(env("OPENAI_CLAUDE_OPUS_1M_MODEL", env("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5"))),
		"claude-opus-4-6":           cleanModel(env("OPENAI_CLAUDE_FAST_MODEL", env("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5"))),
		"claude-opus-4-6[1m]":       cleanModel(env("OPENAI_CLAUDE_FAST_MODEL", env("OPENAI_CLAUDE_OPUS_1M_MODEL", env("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5")))),
		"claude-opus-4-7":           cleanModel(env("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5")),
		"claude-opus-4-7[1m]":       cleanModel(env("OPENAI_CLAUDE_OPUS_1M_MODEL", env("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5"))),
		"sonnet":                    cleanModel(env("OPENAI_CLAUDE_SONNET_MODEL", env("OPENAI_CLAUDE_3_7_MODEL", "gpt-5.5"))),
		"sonnet[1m]":                cleanModel(env("OPENAI_CLAUDE_SONNET_1M_MODEL", env("OPENAI_CLAUDE_SONNET_MODEL", env("OPENAI_CLAUDE_3_7_MODEL", "gpt-5.5")))),
		"claude-sonnet-4-6":         cleanModel(env("OPENAI_CLAUDE_SONNET_MODEL", env("OPENAI_CLAUDE_3_7_MODEL", "gpt-5.5"))),
		"claude-sonnet-4-6[1m]":     cleanModel(env("OPENAI_CLAUDE_SONNET_1M_MODEL", env("OPENAI_CLAUDE_SONNET_MODEL", env("OPENAI_CLAUDE_3_7_MODEL", "gpt-5.5")))),
		"haiku":                     cleanModel(env("OPENAI_CLAUDE_HAIKU_MODEL", env("OPENAI_CLAUDE_3_5_HAIKU_MODEL", "gpt-5.4-mini"))),
		"claude-haiku-4-5":          cleanModel(env("OPENAI_CLAUDE_HAIKU_MODEL", env("OPENAI_CLAUDE_3_5_HAIKU_MODEL", "gpt-5.4-mini"))),
		"claude-haiku-4-5-20251001": cleanModel(env("OPENAI_CLAUDE_HAIKU_MODEL", env("OPENAI_CLAUDE_3_5_HAIKU_MODEL", "gpt-5.4-mini"))),
		"gpt-5.3-codex":             cleanModel(env("OPENAI_CLAUDE_CODEX_MODEL", "gpt-5.3-codex")),
		"claude-3-7-sonnet-latest":  cleanModel(env("OPENAI_CLAUDE_SONNET_MODEL", env("OPENAI_CLAUDE_3_7_MODEL", "gpt-5.5"))),
		"claude-3-5-haiku-latest":   cleanModel(env("OPENAI_CLAUDE_HAIKU_MODEL", env("OPENAI_CLAUDE_3_5_HAIKU_MODEL", "gpt-5.4-mini"))),
		"claude-3-opus-20240229":    cleanModel(env("OPENAI_CLAUDE_OPUS_MODEL", "gpt-5.5")),
		"antigravity-opus":          "antigravity-opus",
		"antigravity-sonnet":        "antigravity-sonnet",
		"antigravity-haiku":         "antigravity-haiku",
		"nano-banana":               "nano-banana",
		"nano-banana-2":             "nano-banana-2",
		"nano-banana-pro":           "nano-banana-pro",
		"gemini-2.5-flash":          "gemini-2.5-flash",
		"gemini-2.5-pro":            "gemini-2.5-pro",
		"gemini-3.5-flash":          "gemini-3.5-flash",
		"gemini-3.5-pro":            "gemini-3.5-pro",
	}
}

func modelAliasesFromValues(env envValueFunc) (map[string]string, map[string]string, map[string]bool, map[string]string, map[string]bool, map[string]bool) {
	models := defaultModelAliasesFromValues(env)
	contexts := map[string]string{}
	custom := map[string]bool{}
	forwards := map[string]string{}
	defaults := map[string]bool{}
	recommended := map[string]bool{}
	for _, row := range parseModelAliasConfigs(env("PROXY_MODEL_ALIASES", "")) {
		alias := strings.TrimSpace(row.Alias)
		real := cleanModel(strings.TrimSpace(row.Real))
		if alias == "" || real == "" {
			continue
		}
		models[alias] = real
		contexts[alias] = normalizeModelContext(row.Context)
		custom[alias] = true
		forwards[alias] = normalizeForwardTarget(row.ForwardTo)
		// Presence of an override row makes the flags explicit (so unsetting a
		// built-in default/recommended persists). Absence → built-in seed.
		defaults[alias] = row.Default
		recommended[alias] = row.Recommended
	}
	for alias := range parseDisabledModelAliases(env("PROXY_MODEL_ALIASES_DISABLED", "")) {
		delete(models, alias)
		delete(contexts, alias)
		delete(custom, alias)
		delete(forwards, alias)
		delete(defaults, alias)
		delete(recommended, alias)
	}
	return models, contexts, custom, forwards, defaults, recommended
}

// knownForwardTargets is the set of backends a model alias may forward to.
// Only "codex" routes today; "agy", "claude" and "opencode" are reserved and
// take effect once their backends are wired (see requestUpstream).
var knownForwardTargets = map[string]bool{"codex": true, "agy": true, "claude": true, "opencode": true}

func normalizeForwardTarget(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if knownForwardTargets[s] {
		return s
	}
	return "codex"
}

// forwardForAlias returns the configured forward target for an alias, defaulting
// to "codex". Callers must hold modelConfigMu (read lock) as it reads cfg.ModelForward.
func forwardForAlias(cfg config, alias string) string {
	if cfg.ModelForward != nil {
		if v, ok := cfg.ModelForward[alias]; ok && v != "" {
			return normalizeForwardTarget(v)
		}
	}
	return "codex"
}

// defaultForAlias / recommendedForAlias report whether an alias is the default /
// recommended choice. An explicit stored flag (set via the Models page) wins;
// absent that, the built-in seed (sonnet / gpt-5.3-codex) applies so a fresh
// install keeps its out-of-box choices until the user customizes them.
func defaultForAlias(cfg config, alias string) bool {
	if cfg.ModelDefault != nil {
		if v, ok := cfg.ModelDefault[alias]; ok {
			return v
		}
	}
	return isDefaultModelAlias(alias)
}

func recommendedForAlias(cfg config, alias string) bool {
	if cfg.ModelRecommended != nil {
		if v, ok := cfg.ModelRecommended[alias]; ok {
			return v
		}
	}
	return isRecommendedModelAlias(alias)
}

func parseModelAliasConfigs(raw string) []modelAliasConfig {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var rows []modelAliasConfig
	if err := json.Unmarshal([]byte(raw), &rows); err == nil {
		return rows
	}
	return nil
}

func parseDisabledModelAliases(raw string) map[string]bool {
	out := map[string]bool{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	var aliases []string
	if strings.HasPrefix(raw, "[") && json.Unmarshal([]byte(raw), &aliases) == nil {
		for _, alias := range aliases {
			if alias = strings.TrimSpace(alias); alias != "" {
				out[alias] = true
			}
		}
		return out
	}
	for _, alias := range strings.Split(raw, ",") {
		if alias = strings.TrimSpace(alias); alias != "" {
			out[alias] = true
		}
	}
	return out
}

func normalizeModelContext(context string) string {
	context = strings.ToLower(strings.TrimSpace(context))
	if context == "1m" || context == "1m tokens" || context == "1000k" {
		return "1m"
	}
	return "200k"
}

const adminSessionCookieName = "ccp_admin_session"

type providerKeyRow struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	Schema     string `json:"schema"`
	Label      string `json:"label"`
	BaseURL    string `json:"base_url"`
	KeyPreview string `json:"key_preview"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type clientKeyRow struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	KeyPreview    string `json:"key_preview"`
	Schema        string `json:"schema"`
	Provider      string `json:"provider"`
	ProviderKeyID string `json:"provider_key_id"`
	ProviderLabel string `json:"provider_label,omitempty"`
	Enabled       bool   `json:"enabled"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	LastUsedAt    string `json:"last_used_at,omitempty"`
}

type providerCredential struct {
	Provider      string
	Label         string
	APIKey        string
	BaseURL       string
	ProviderKeyID string
	Schema        string
}

func initProxyDB(cfg config) error {
	db, err := openProxyDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrateProxyDB(db); err != nil {
		return err
	}
	if err := seedProxyDBFromEnv(db, cfg); err != nil {
		return err
	}
	return nil
}

func openProxyDB(cfg config) (*sql.DB, error) {
	path := strings.TrimSpace(cfg.DBPath)
	if path == "" {
		path = ".proxy.db"
	}
	return sql.Open("sqlite", path)
}

func migrateProxyDB(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS provider_keys (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			label TEXT NOT NULL,
			base_url TEXT NOT NULL,
			api_key_enc TEXT NOT NULL,
			key_preview TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS client_keys (
			id TEXT PRIMARY KEY,
			label TEXT NOT NULL,
			key_hash TEXT NOT NULL UNIQUE,
			key_preview TEXT NOT NULL,
			schema TEXT NOT NULL DEFAULT 'both',
			provider TEXT NOT NULL DEFAULT '',
			provider_key_id TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			last_used_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_provider_keys_provider ON provider_keys(provider, enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_client_keys_hash ON client_keys(key_hash, enabled)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	if err := ensureSQLiteColumn(db, "client_keys", "schema", "TEXT NOT NULL DEFAULT 'both'"); err != nil {
		return err
	}
	return nil
}

func ensureSQLiteColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, column) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}

func seedProxyDBFromEnv(db *sql.DB, cfg config) error {
	now := time.Now().Format(time.RFC3339)
	if key := strings.TrimSpace(cfg.ProxyKey); key != "" && key != "YOUR_PROXY_TOKEN" {
		var count int
		_ = db.QueryRow(`SELECT COUNT(*) FROM client_keys WHERE key_hash = ?`, hashString(key)).Scan(&count)
		if count == 0 {
			_, err := db.Exec(`INSERT INTO client_keys (id, label, key_hash, key_preview, provider, provider_key_id, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, '', '', 1, ?, ?)`,
				randomID("ck"), "Claude Code default", hashString(key), maskSecret(key), now, now)
			if err != nil {
				return err
			}
		}
	}
	openAIKey := strings.TrimSpace(cfg.OpenAIAPIKey)
	if openAIKey != "" && openAIKey != "YOUR_OPENAI_API_KEY" && !strings.HasPrefix(openAIKey, "sk-or-") {
		var count int
		_ = db.QueryRow(`SELECT COUNT(*) FROM provider_keys WHERE provider = 'openai'`).Scan(&count)
		if count == 0 {
			enc, err := encryptSecret(cfg, openAIKey)
			if err != nil {
				return err
			}
			_, err = db.Exec(`INSERT INTO provider_keys (id, provider, label, base_url, api_key_enc, key_preview, enabled, created_at, updated_at) VALUES (?, 'openai', ?, ?, ?, ?, 1, ?, ?)`,
				randomID("pk"), "OpenAI from .env", firstNonEmpty(cfg.OpenAIBaseURL, "https://api.openai.com/v1"), enc, maskSecret(openAIKey), now, now)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func randomID(prefix string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(raw)
}

func newClientAPIKey() string {
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	return "ccp_" + base64.RawURLEncoding.EncodeToString(raw)
}

func secretCipherKey(cfg config) []byte {
	seed := firstNonEmpty(cfg.ProxyKey, cfg.AdminSessionSecret, "connect-ai-proxy-local-secret")
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

func encryptSecret(cfg config, value string) (string, error) {
	block, err := aes.NewCipher(secretCipherKey(cfg))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(value), nil)
	return "aesgcm:" + base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func decryptSecret(cfg config, value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "aesgcm:") {
		return value, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "aesgcm:"))
	if err != nil {
		return "", err
	}
	keys := [][]byte{secretCipherKey(cfg)}
	if cfg.AdminSessionSecret != "" {
		adminKey := sha256.Sum256([]byte(cfg.AdminSessionSecret))
		keys = append(keys, adminKey[:])
	}
	var lastErr error
	for _, key := range keys {
		block, err := aes.NewCipher(key)
		if err != nil {
			lastErr = err
			continue
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			lastErr = err
			continue
		}
		if len(raw) < gcm.NonceSize() {
			return "", errors.New("encrypted secret is too short")
		}
		nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
		plain, err := gcm.Open(nil, nonce, ciphertext, nil)
		if err == nil {
			return string(plain), nil
		}
		lastErr = err
	}
	return "", lastErr
}

func adminConfigured(cfg config) bool {
	cfg = adminRuntimeConfig(cfg)
	return strings.TrimSpace(cfg.AdminUsername) != "" && strings.TrimSpace(cfg.AdminPasswordHash) != ""
}

func adminRuntimeConfig(cfg config) config {
	if v := strings.TrimSpace(os.Getenv("ADMIN_USERNAME")); v != "" {
		cfg.AdminUsername = v
	}
	if v := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD_HASH")); v != "" {
		cfg.AdminPasswordHash = v
	}
	if v := strings.TrimSpace(os.Getenv("ADMIN_SESSION_SECRET")); v != "" {
		cfg.AdminSessionSecret = v
	}
	return cfg
}

func requireAdmin(cfg config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg = adminRuntimeConfig(cfg)
		if !adminConfigured(cfg) {
			writeJSON(w, http.StatusLocked, map[string]any{"error": "admin setup required"})
			return
		}
		if adminSessionValid(cfg, r) {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "admin login required"})
	}
}

// noStore marks a control-panel API response as uncacheable so browsers never
// serve a stale auth/status payload after the session changes.
func noStore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

func handleUIAuthStatus(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg = adminRuntimeConfig(cfg)
		configured := adminConfigured(cfg)
		writeJSON(w, http.StatusOK, map[string]any{
			"configured":    configured,
			"authenticated": configured && adminSessionValid(cfg, r),
			"username":      cfg.AdminUsername,
		})
	}
}

func handleUIAuthSetup(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg = adminRuntimeConfig(cfg)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if adminConfigured(cfg) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "admin login is already configured"})
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Remember bool   `json:"remember"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		username := strings.TrimSpace(body.Username)
		if username == "" || len(body.Password) < 8 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "username and an 8+ character password are required"})
			return
		}
		hash, err := hashAdminPassword(body.Password)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		secret := randomID("sess")
		vals := readEnvMap()
		vals["ADMIN_USERNAME"] = username
		vals["ADMIN_PASSWORD_HASH"] = hash
		vals["ADMIN_SESSION_SECRET"] = secret
		if vals["PROXY_DB_PATH"] == "" {
			vals["PROXY_DB_PATH"] = ".proxy.db"
		}
		if err := writeEnvMap(vals); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		syncProcessEnvKeys(vals, "ADMIN_USERNAME", "ADMIN_PASSWORD_HASH", "ADMIN_SESSION_SECRET", "PROXY_DB_PATH")
		cfg.AdminUsername = username
		cfg.AdminPasswordHash = hash
		cfg.AdminSessionSecret = secret
		setAdminSessionCookie(w, cfg, username, body.Remember)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Admin login configured."})
	}
}

func handleUIAuthLogin(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg = adminRuntimeConfig(cfg)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !adminConfigured(cfg) {
			writeJSON(w, http.StatusPreconditionRequired, map[string]any{"error": "admin login is not configured"})
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Remember bool   `json:"remember"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		if !hmac.Equal([]byte(strings.TrimSpace(body.Username)), []byte(cfg.AdminUsername)) || !verifyAdminPassword(cfg.AdminPasswordHash, body.Password) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid username or password"})
			return
		}
		setAdminSessionCookie(w, cfg, cfg.AdminUsername, body.Remember)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleUIAuthLogout(_ config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: adminSessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

const (
	adminSessionTTL         = 24 * time.Hour
	adminSessionRememberTTL = 365 * 24 * time.Hour
)

func setAdminSessionCookie(w http.ResponseWriter, cfg config, username string, remember bool) {
	ttl := adminSessionTTL
	if remember {
		ttl = adminSessionRememberTTL
	}
	exp := time.Now().Add(ttl).Unix()
	payload := fmt.Sprintf("%s|%d", username, exp)
	sig := adminSessionSignature(cfg, payload)
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig,
		Path:     "/",
		MaxAge:   int(ttl / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func adminSessionValid(cfg config, r *http.Request) bool {
	c, err := r.Cookie(adminSessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	payload := string(payloadRaw)
	if !hmac.Equal([]byte(parts[1]), []byte(adminSessionSignature(cfg, payload))) {
		return false
	}
	fields := strings.Split(payload, "|")
	if len(fields) != 2 || fields[0] != cfg.AdminUsername {
		return false
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	return err == nil && time.Now().Unix() < exp
}

func adminSessionSignature(cfg config, payload string) string {
	secret := firstNonEmpty(cfg.AdminSessionSecret, cfg.ProxyKey, "connect-ai-proxy-admin")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func hashAdminPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	iterations := 120000
	dk := pbkdf2SHA256([]byte(password), salt, iterations, 32)
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s", iterations, base64.RawURLEncoding.EncodeToString(salt), base64.RawURLEncoding.EncodeToString(dk)), nil
}

func verifyAdminPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iterations, len(want))
	return hmac.Equal(got, want)
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	var out []byte
	block := 1
	for len(out) < keyLen {
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		_, _ = mac.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iterations; i++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
		block++
	}
	return out[:keyLen]
}

func requireAuth(cfg config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.ProxyKey != "" && cfg.ProxyKey != "YOUR_PROXY_TOKEN" {
			if route, ok := lookupClientKeyRoute(cfg, r); ok {
				if !clientKeySchemaAllowsPath(route.Schema, r.URL.Path) {
					writeSchemaAuthError(w, r, route.Schema)
					return
				}
				requestProviderKeys.Store(r, route)
				next(w, r)
				return
			}
			if proxyRequestHasKey(r, cfg.ProxyKey) {
				next(w, r)
				return
			}
			{
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

func writeSchemaAuthError(w http.ResponseWriter, r *http.Request, schema string) {
	msg := "client key is not allowed to use the " + strings.TrimSpace(schema) + " schema"
	if strings.HasPrefix(r.URL.Path, "/openai/") {
		writeOpenAIError(w, http.StatusUnauthorized, msg)
		return
	}
	writeAnthropicError(w, http.StatusUnauthorized, msg)
}

func clientKeySchemaAllowsPath(schema, path string) bool {
	// The bare /v1/images/generations alias is an OpenAI-style endpoint, so it
	// counts as the openai schema even though it isn't under /openai/.
	openaiPath := strings.HasPrefix(path, "/openai/") || strings.HasPrefix(path, "/v1/images/")
	anthropicPath := strings.HasPrefix(path, "/anthropic/")
	switch normalizeClientSchema(schema) {
	case "anthropic":
		return anthropicPath
	case "openai":
		return openaiPath
	default:
		return anthropicPath || openaiPath
	}
}

func normalizeClientSchema(schema string) string {
	switch strings.ToLower(strings.TrimSpace(schema)) {
	case "anthropic", "claude":
		return "anthropic"
	case "openai", "openai-compatible", "openai_compatible":
		return "openai"
	default:
		return "both"
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
	for _, value := range proxyRequestKeys(r) {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func proxyRequestKeys(r *http.Request) []string {
	out := []string{}
	for _, name := range []string{"x-api-key", "anthropic-api-key", "api-key"} {
		for _, value := range r.Header.Values(name) {
			if value = strings.TrimSpace(value); value != "" {
				out = append(out, value)
			}
		}
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
		auth = strings.TrimSpace(auth[7:])
	}
	if auth != "" {
		out = append(out, auth)
	}
	return out
}

func lookupClientKeyRoute(cfg config, r *http.Request) (providerCredential, bool) {
	for _, key := range proxyRequestKeys(r) {
		route, ok := lookupClientKeyRouteByKey(cfg, key)
		if ok {
			return route, true
		}
	}
	return providerCredential{}, false
}

func lookupClientKeyRouteByKey(cfg config, key string) (providerCredential, bool) {
	db, err := openProxyDB(cfg)
	if err != nil {
		return providerCredential{}, false
	}
	defer db.Close()
	if err := migrateProxyDB(db); err != nil {
		return providerCredential{}, false
	}
	var clientID, provider, providerKeyID, schema string
	err = db.QueryRow(`SELECT id, provider, provider_key_id, schema FROM client_keys WHERE key_hash = ? AND enabled = 1`, hashString(key)).Scan(&clientID, &provider, &providerKeyID, &schema)
	if err != nil {
		return providerCredential{}, false
	}
	_, _ = db.Exec(`UPDATE client_keys SET last_used_at = ? WHERE id = ?`, time.Now().Format(time.RFC3339), clientID)
	route := providerCredential{Provider: strings.ToLower(strings.TrimSpace(provider)), ProviderKeyID: strings.TrimSpace(providerKeyID), Schema: normalizeClientSchema(schema)}
	if route.ProviderKeyID != "" {
		cred, ok := providerCredentialByID(cfg, db, route.ProviderKeyID)
		if ok {
			cred.Schema = route.Schema
			return cred, true
		}
	}
	if route.Provider != "" {
		if cred, ok := firstProviderCredential(cfg, db, route.Provider); ok {
			cred.Schema = route.Schema
			return cred, true
		}
	}
	return route, true
}

func requestProviderRoute(r *http.Request) (providerCredential, bool) {
	value, ok := requestProviderKeys.Load(r)
	if !ok {
		return providerCredential{}, false
	}
	route, ok := value.(providerCredential)
	return route, ok
}

func takeRequestProviderRoute(r *http.Request) (providerCredential, bool) {
	value, ok := requestProviderKeys.LoadAndDelete(r)
	if !ok {
		return providerCredential{}, false
	}
	route, ok := value.(providerCredential)
	return route, ok
}

func providerCredentialByID(cfg config, db *sql.DB, id string) (providerCredential, bool) {
	var cred providerCredential
	var enc string
	var enabled int
	err := db.QueryRow(`SELECT id, provider, label, base_url, api_key_enc, enabled FROM provider_keys WHERE id = ?`, id).Scan(&cred.ProviderKeyID, &cred.Provider, &cred.Label, &cred.BaseURL, &enc, &enabled)
	if err != nil || enabled == 0 {
		return providerCredential{}, false
	}
	apiKey, err := decryptSecret(cfg, enc)
	if err != nil || strings.TrimSpace(apiKey) == "" {
		return providerCredential{}, false
	}
	cred.APIKey = apiKey
	cred.Provider = strings.ToLower(strings.TrimSpace(cred.Provider))
	if strings.TrimSpace(cred.BaseURL) == "" {
		cred.BaseURL = defaultProviderBaseURL(cred.Provider)
	}
	cred.BaseURL = strings.TrimRight(cred.BaseURL, "/")
	return cred, true
}

func firstProviderCredential(cfg config, db *sql.DB, provider string) (providerCredential, bool) {
	var id string
	err := db.QueryRow(`SELECT id FROM provider_keys WHERE provider = ? AND enabled = 1 ORDER BY created_at DESC LIMIT 1`, strings.ToLower(strings.TrimSpace(provider))).Scan(&id)
	if err != nil {
		return providerCredential{}, false
	}
	return providerCredentialByID(cfg, db, id)
}

func effectiveOpenAIUpstream(cfg config, r *http.Request) providerCredential {
	if r != nil {
		if route, ok := requestProviderRoute(r); ok && route.Provider != "" {
			pName := strings.ToLower(route.Provider)
			if pName == "antigravity" {
				if route.BaseURL == "" {
					route.BaseURL = "http://127.0.0.1:4005/v1"
				}
				if route.APIKey == "" {
					route.APIKey = "local"
				}
				return route
			}
			if route.APIKey != "" {
				route.BaseURL = strings.TrimRight(firstNonEmpty(route.BaseURL, defaultProviderBaseURL(route.Provider)), "/")
				return route
			}
			if route.ProviderKeyID != "" {
				if db, err := openProxyDB(cfg); err == nil {
					defer db.Close()
					if cred, ok := providerCredentialByID(cfg, db, route.ProviderKeyID); ok {
						return cred
					}
				}
			}
			if db, err := openProxyDB(cfg); err == nil {
				defer db.Close()
				if cred, ok := firstProviderCredential(cfg, db, route.Provider); ok {
					return cred
				}
			}
		}
	}
	if cfg.Upstream == "antigravity" {
		return providerCredential{Provider: "antigravity", Label: "Antigravity Sidecar", APIKey: "local", BaseURL: "http://127.0.0.1:4005/v1"}
	}
	return providerCredential{Provider: "openai", Label: "OpenAI from .env", APIKey: cfg.OpenAIAPIKey, BaseURL: strings.TrimRight(firstNonEmpty(cfg.OpenAIBaseURL, "https://api.openai.com/v1"), "/")}
}

func defaultProviderBaseURL(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "gemini", "google", "google-ai-studio":
		return "https://generativelanguage.googleapis.com/v1beta/openai"
	case "openai":
		return "https://api.openai.com/v1"
	default:
		return ""
	}
}

func providerSchema(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "gemini", "google", "google-ai-studio":
		return "openai-compatible"
	case "openai":
		return "openai-compatible"
	default:
		return ""
	}
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
		modelConfigMu.RLock()
		names := make([]string, 0, len(cfg.Models))
		for name := range cfg.Models {
			if !isAdvertisedModel(name) && !cfg.ModelCustom[name] {
				continue
			}
			names = append(names, name)
		}
		modelConfigMu.RUnlock()
		sort.Strings(names)
		data := make([]map[string]any, 0, len(names))
		for _, name := range names {
			data = append(data, map[string]any{"id": name, "type": "model", "display_name": name})
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	}
}

func handleModelCapabilities(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": modelCapabilityRows(cfg), "units": "tokens"})
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
		var in anthropicRequest
		if err := json.NewDecoder(bytes.NewReader(rawBody)).Decode(&in); err != nil {
			traceLogID(traceID, "anthropic.decode_error", map[string]any{"error": err.Error()})
			writeAnthropicError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		in.FastMode = requestUsesClaudeFastMode(r, in)
		traceLogID(traceID, "anthropic.decoded", summarizeAnthropicRequest(in))

		// Forwarded-to = Agy: serve this alias from the local agy (Antigravity)
		// CLI via the agyj subprocess instead of the codex/openai HTTP path.
		if forwardForAlias(cfg, in.Model) == "agy" {
			if cfg.AgyDisabled {
				writeAnthropicError(w, http.StatusServiceUnavailable, "agy upstream is disabled")
				return
			}
			serveAgyAnthropic(ctx, cfg, in, w, r)
			return
		}
		// Forwarded-to = Claude: serve this alias from the local Claude Code CLI
		// (`claude -p`) backed by a Claude subscription, instead of codex/openai.
		if forwardForAlias(cfg, in.Model) == "claude" {
			if cfg.ClaudeDisabled {
				writeAnthropicError(w, http.StatusServiceUnavailable, "claude upstream is disabled")
				return
			}
			serveClaudeAnthropic(ctx, cfg, in, w, r)
			return
		}

		if _, ok := requestProviderRoute(r); !ok {
			upstreamType := requestUpstream(cfg, r, in.Model)
			if upstreamType == "antigravity" {
				route := providerCredential{
					Provider: "antigravity",
					Label:    "Antigravity Dynamic",
					APIKey:   "local",
					BaseURL:  "http://127.0.0.1:4005/v1",
				}
				requestProviderKeys.Store(r, route)
			}
		}
		route, hasProviderRoute := requestProviderRoute(r)

		if cfg.Upstream == "openai" && !hasProviderRoute && (cfg.OpenAIAPIKey == "" || cfg.OpenAIAPIKey == "YOUR_OPENAI_API_KEY" || strings.HasPrefix(cfg.OpenAIAPIKey, "sk-or-")) {
			traceLogID(traceID, "anthropic.config_error", map[string]any{"message": "OPENAI_API_KEY is not configured"})
			writeAnthropicError(w, http.StatusBadRequest, "OPENAI_API_KEY is not configured")
			return
		}
		out, err := toOpenAI(cfg, in)
		if err != nil {
			traceLogID(traceID, "anthropic.convert_error", map[string]any{"error": err.Error()})
			writeAnthropicError(w, http.StatusBadRequest, err.Error())
			return
		}
		applyCustomSessionToOpenAIRequest(r, &out)
		if hasProviderRoute && route.Provider != "" {
			traceLogID(traceID, "openai_compatible.prepared", summarizeOpenAIRequest(out))
			setRequestStat(r, requestStat{Model: in.Model, Upstream: firstNonEmpty(route.Provider, out.Model), Stream: in.Stream})
			setRequestNote(r, requestNote(in.Model, out.Model, in.Stream, out.ReasoningEffort)+" provider="+route.Provider+" trace="+traceID)
			if in.Stream {
				streamOpenAI(ctx, cfg, out, in.Model, w, r)
				return
			}
			callOpenAI(ctx, cfg, out, in.Model, w, r)
			return
		}
		if cfg.Upstream == "codex" {
			if cfg.CodexDisabled {
				writeAnthropicError(w, http.StatusServiceUnavailable, "codex upstream is disabled")
				return
			}
			responsesReq := toResponses(cfg, in)
			estimatedInput := max(estimateAnthropicRequestTokens(in), estimateCodexRequestTokens(responsesReq))
			if decision := codexTokenLimitDecision(estimatedInput, in.MaxTokens); decision.Action != "" {
				traceLogID(traceID, "codex.token_limit_guard", map[string]any{"action": decision.Action, "reason": decision.Reason, "input_tokens": decision.InputTokens, "output_tokens": decision.OutputTokens, "total_tokens": decision.TotalTokens})
				applyTokenLimitErrorStat(r, decision, in.Model, responsesReq.Model)
				writeAnthropicError(w, http.StatusBadGateway, decision.Message)
				return
			}
			session := configureCodexSession(cfg, r, rawBody, in, &responsesReq)
			if session.Enabled {
				traceLogID(traceID, "codex.session", summarizeCodexSession(session))
			}
			traceLogID(traceID, "codex.prepared", summarizeResponsesRequest(responsesReq))
			setRequestStat(r, requestStat{Model: in.Model, Upstream: responsesReq.Model, Stream: in.Stream})
			note := requestNote(in.Model, responsesReq.Model, in.Stream, requestReasoningEffort(cfg, in))
			if responsesReq.ServiceTier != "" {
				note += " service_tier=" + responsesReq.ServiceTier
			}
			if session.Enabled {
				note += " flow=" + session.FlowHash + " codex_session=" + session.CodexSessionID
				if session.SideThread {
					note += " side_thread=" + session.SideThreadKind
				}
			}
			setRequestNote(r, note+" trace="+traceID)
			if in.Stream {
				streamCodex(ctx, cfg, responsesReq, in, w, r)
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
		applyCustomSessionToOpenAIRequest(r, &in)
		if forwardForAlias(cfg, in.Model) == "agy" {
			if cfg.AgyDisabled {
				writeOpenAIError(w, http.StatusServiceUnavailable, "agy upstream is disabled")
				return
			}
			serveAgyOpenAIChat(r.Context(), cfg, in, w, r)
			return
		}
		if forwardForAlias(cfg, in.Model) == "claude" {
			if cfg.ClaudeDisabled {
				writeOpenAIError(w, http.StatusServiceUnavailable, "claude upstream is disabled")
				return
			}
			serveClaudeOpenAIChat(r.Context(), cfg, in, w, r)
			return
		}
		if _, ok := requestProviderRoute(r); !ok {
			upstreamType := requestUpstream(cfg, r, in.Model)
			if upstreamType == "antigravity" {
				route := providerCredential{
					Provider: "antigravity",
					Label:    "Antigravity Dynamic",
					APIKey:   "local",
					BaseURL:  "http://127.0.0.1:4005/v1",
				}
				requestProviderKeys.Store(r, route)
			}
		}
		if route, ok := requestProviderRoute(r); ok && route.Provider != "" {
			setRequestStat(r, requestStat{Model: in.Model, Upstream: route.Provider, Stream: in.Stream})
			proxyOpenAIChat(r.Context(), cfg, in, w, r)
			return
		}
		if cfg.Upstream == "openai" {
			setRequestStat(r, requestStat{Model: in.Model, Upstream: in.Model, Stream: in.Stream})
			proxyOpenAIChat(r.Context(), cfg, in, w, r)
			return
		}
		if cfg.CodexDisabled {
			writeOpenAIError(w, http.StatusServiceUnavailable, "codex upstream is disabled")
			return
		}
		out := openAIChatToResponses(cfg, in)
		estimatedInput := max(estimateOpenAIChatRequestTokens(in), estimateCodexRequestTokens(out))
		if decision := codexTokenLimitDecision(estimatedInput, requestedOpenAIOutputTokens(in)); decision.Action != "" {
			applyTokenLimitErrorStat(r, decision, in.Model, out.Model)
			writeOpenAIError(w, http.StatusBadGateway, decision.Message)
			return
		}
		session := configureOpenAICompatibleSession(cfg, r, in, &out)
		setRequestStat(r, requestStat{Model: in.Model, Upstream: out.Model, Stream: in.Stream})
		if session.Enabled {
			setRequestNote(r, "openai session="+session.FlowHash+" codex_session="+session.CodexSessionID)
		}
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
		// Forwarded-to = Agy is keyed on the public alias, so route before
		// resolveModel rewrites in.Model to the upstream model name.
		if forwardForAlias(cfg, in.Model) == "agy" {
			if cfg.AgyDisabled {
				writeOpenAIError(w, http.StatusServiceUnavailable, "agy upstream is disabled")
				return
			}
			serveAgyResponses(r.Context(), cfg, in, w, r)
			return
		}
		if forwardForAlias(cfg, in.Model) == "claude" {
			if cfg.ClaudeDisabled {
				writeOpenAIError(w, http.StatusServiceUnavailable, "claude upstream is disabled")
				return
			}
			serveClaudeResponses(r.Context(), cfg, in, w, r)
			return
		}
		in.Model = resolveModel(cfg, in.Model)
		if _, ok := requestProviderRoute(r); !ok {
			upstreamType := requestUpstream(cfg, r, in.Model)
			if upstreamType == "antigravity" {
				route := providerCredential{
					Provider: "antigravity",
					Label:    "Antigravity Dynamic",
					APIKey:   "local",
					BaseURL:  "http://127.0.0.1:4005/v1",
				}
				requestProviderKeys.Store(r, route)
			}
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
		if route, ok := requestProviderRoute(r); ok && route.Provider != "" {
			setRequestStat(r, requestStat{Model: in.Model, Upstream: route.Provider, Stream: in.Stream})
			proxyOpenAIResponses(r.Context(), cfg, in, w, r)
			return
		}
		if cfg.Upstream == "openai" {
			setRequestStat(r, requestStat{Model: in.Model, Upstream: in.Model, Stream: in.Stream})
			proxyOpenAIResponses(r.Context(), cfg, in, w, r)
			return
		}
		if cfg.CodexDisabled {
			writeOpenAIError(w, http.StatusServiceUnavailable, "codex upstream is disabled")
			return
		}
		if decision := codexTokenLimitDecision(estimateCodexRequestTokens(in), in.MaxOutputTokens); decision.Action != "" {
			applyTokenLimitErrorStat(r, decision, in.Model, in.Model)
			writeOpenAIError(w, http.StatusBadGateway, decision.Message)
			return
		}
		session := configureResponsesCustomSession(cfg, r, &in)
		in.Temperature = nil
		setRequestStat(r, requestStat{Model: in.Model, Upstream: in.Model, Stream: in.Stream})
		if session.Enabled {
			setRequestNote(r, "responses session="+session.FlowHash+" codex_session="+session.CodexSessionID)
		}
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

func handleOpenAIFiles(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requestProviderRoute(r); ok {
			proxyOpenAIPassthrough(r.Context(), cfg, w, r, "/openai/v1")
			return
		}
		if cfg.Upstream == "openai" {
			proxyOpenAIPassthrough(r.Context(), cfg, w, r, "/openai/v1")
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "Files endpoint requires an OpenAI-compatible provider route")
	}
}

func toResponses(cfg config, in anthropicRequest) responsesRequest {
	model := resolveModel(cfg, in.Model)
	instructions := contentToText(in.System)
	if instructions == "" {
		instructions = "You are a helpful coding assistant."
	}
	out := responsesRequest{Model: model, Instructions: instructions, MaxOutputTokens: in.MaxTokens, Stream: in.Stream, Store: false}
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
	if len(in.Tools) > 0 {
		out.Instructions = strings.TrimSpace(out.Instructions + "\n\n" + noFakeToolJSONGuard)
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

type codexSessionInfo struct {
	Enabled        bool
	FlowID         string
	FlowHash       string
	FlowSource     string
	RegistryKey    string
	CodexSessionID string
	PromptCacheKey string
	SideThread     bool
	SideThreadKind string
}

type codexSessionRegistry struct {
	Version   int                           `json:"version"`
	UpdatedAt string                        `json:"updated_at"`
	Sessions  map[string]codexSessionRecord `json:"sessions"`
}

type codexSessionRecord struct {
	CodexSessionID string `json:"codex_session_id"`
	FlowHash       string `json:"flow_hash"`
	FlowSource     string `json:"flow_source"`
	SideThread     bool   `json:"side_thread"`
	SideThreadKind string `json:"side_thread_kind,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	RequestCount   int    `json:"request_count"`
}

var codexSessionMu sync.Mutex

func configureCodexSession(cfg config, r *http.Request, rawBody []byte, in anthropicRequest, out *responsesRequest) codexSessionInfo {
	if !codexSessionIsolationEnabled() {
		return codexSessionInfo{}
	}
	flowID, source := extractClaudeFlowID(r, rawBody, in)
	flowHash := hashString(flowID)[:16]
	sideThread, sideKind, sideKey := detectIsolatedClaudeRequest(r, rawBody, in)
	registryKey := flowHash
	if sideThread {
		if sideKey == "" {
			sideKey = hashString(string(rawBody))[:16]
		}
		registryKey = flowHash + ":isolated:" + firstNonEmpty(sideKind, "request") + ":" + sideKey
	}
	sessionID := stableCodexSessionID(registryKey)
	info := codexSessionInfo{
		Enabled:        true,
		FlowID:         flowID,
		FlowHash:       flowHash,
		FlowSource:     source,
		RegistryKey:    registryKey,
		CodexSessionID: sessionID,
		SideThread:     sideThread,
		SideThreadKind: sideKind,
	}
	if codexPromptCacheKeyEnabled() {
		out.PromptCacheKey = sessionID
		info.PromptCacheKey = sessionID
	}
	recordCodexSession(cfg, info)
	return info
}

func codexSessionIsolationEnabled() bool {
	return envFlag("CODEX_SESSION_ISOLATION", true)
}

func codexPromptCacheKeyEnabled() bool {
	return envFlag("CODEX_PROMPT_CACHE_KEY", true)
}

func envFlag(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func extractClaudeFlowID(r *http.Request, rawBody []byte, in anthropicRequest) (string, string) {
	for _, name := range customSessionHeaderNames() {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value, strings.ToLower(name)
		}
	}
	seed := strings.Join([]string{
		r.Header.Get("User-Agent"),
		r.Header.Get("X-App"),
		truncateString(latestUserText(in), 4096),
		truncateString(string(rawBody), 4096),
	}, "|")
	return "fallback:" + hashString(seed), "fallback_request_hash"
}

func customSessionHeaderNames() []string {
	return []string{
		"X-Proxy-Session-Id",
		"X-Codex-Session-Id",
		"X-OpenAI-Session-Id",
		"X-Claude-Code-Session-Id",
		"X-Claude-Session-Id",
		"X-Session-Id",
	}
}

func applyCustomSessionToOpenAIRequest(r *http.Request, out *openAIRequest) {
	if out == nil || strings.TrimSpace(out.User) != "" {
		return
	}
	if id, _ := extractCustomSessionIDFromHeaders(r); id != "" {
		out.User = id
	}
}

func configureOpenAICompatibleSession(cfg config, r *http.Request, in openAIRequest, out *responsesRequest) codexSessionInfo {
	flowID, source := extractOpenAICompatibleSessionID(r, in)
	return configureNamedCodexSession(cfg, flowID, source, "openai", out)
}

func configureResponsesCustomSession(cfg config, r *http.Request, out *responsesRequest) codexSessionInfo {
	flowID, source := extractCustomSessionIDFromHeaders(r)
	return configureNamedCodexSession(cfg, flowID, source, "openai", out)
}

func configureNamedCodexSession(cfg config, flowID, source, namespace string, out *responsesRequest) codexSessionInfo {
	if !codexSessionIsolationEnabled() || strings.TrimSpace(flowID) == "" || out == nil {
		return codexSessionInfo{}
	}
	flowID = strings.TrimSpace(flowID)
	flowHash := hashString(flowID)[:16]
	registryKey := firstNonEmpty(namespace, "custom") + ":" + flowHash
	sessionID := stableCodexSessionID(registryKey)
	info := codexSessionInfo{
		Enabled:        true,
		FlowID:         flowID,
		FlowHash:       flowHash,
		FlowSource:     firstNonEmpty(source, "custom_session"),
		RegistryKey:    registryKey,
		CodexSessionID: sessionID,
	}
	if codexPromptCacheKeyEnabled() && strings.TrimSpace(out.PromptCacheKey) == "" {
		out.PromptCacheKey = sessionID
		info.PromptCacheKey = sessionID
	} else {
		info.PromptCacheKey = out.PromptCacheKey
	}
	recordCodexSession(cfg, info)
	return info
}

func extractOpenAICompatibleSessionID(r *http.Request, in openAIRequest) (string, string) {
	if id, source := extractCustomSessionIDFromHeaders(r); id != "" {
		return id, source
	}
	if id := strings.TrimSpace(in.User); id != "" {
		return id, "user"
	}
	for _, key := range []string{"proxy_session_id", "session_id", "conversation_id", "thread_id"} {
		if value := metadataString(in.Metadata, key); value != "" {
			return value, "metadata." + key
		}
	}
	return "", ""
}

func extractCustomSessionIDFromHeaders(r *http.Request) (string, string) {
	if r == nil {
		return "", ""
	}
	for _, name := range customSessionHeaderNames() {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value, strings.ToLower(name)
		}
	}
	return "", ""
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func detectIsolatedClaudeRequest(r *http.Request, rawBody []byte, in anthropicRequest) (bool, string, string) {
	if agentID := firstNonEmpty(
		r.Header.Get("X-Claude-Code-Agent-Id"),
		r.Header.Get("X-Claude-Agent-Id"),
		r.Header.Get("X-Agent-Id"),
		extractMarkerValue(string(rawBody), "codex_proxy_subagent_id="),
	); agentID != "" {
		return true, "subagent", hashString("subagent:" + agentID)[:16]
	}

	text := strings.ToLower(latestUserText(in))
	switch {
	case strings.Contains(text, "<command-name>/btw</command-name>") || strings.HasPrefix(strings.TrimSpace(text), "/btw") || strings.Contains(text, "\n/btw"):
		return true, "btw", ""
	case strings.Contains(text, "this session is being continued from a previous conversation that ran out of context"):
		return true, "compact", ""
	case strings.Contains(text, "compact summary") || strings.Contains(text, "conversation compacted"):
		return true, "compact", ""
	case strings.Contains(text, "critical: respond with text only") && strings.Contains(text, "do not call any tools"):
		return true, "one-shot", ""
	default:
		return false, "", ""
	}
}

func extractMarkerValue(text, marker string) string {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, strings.ToLower(marker))
	if idx < 0 {
		return ""
	}
	start := idx + len(marker)
	if start >= len(text) {
		return ""
	}
	var b strings.Builder
	for _, ch := range text[start:] {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' || ch == ':' {
			b.WriteRune(ch)
			continue
		}
		break
	}
	return strings.TrimSpace(b.String())
}

func latestUserText(in anthropicRequest) string {
	for i := len(in.Messages) - 1; i >= 0; i-- {
		if in.Messages[i].Role != "user" {
			continue
		}
		if blocks, ok := in.Messages[i].Content.([]any); ok {
			for j := len(blocks) - 1; j >= 0; j-- {
				m, ok := blocks[j].(map[string]any)
				if !ok || fmt.Sprint(m["type"]) != "text" {
					continue
				}
				if text := strings.TrimSpace(fmt.Sprint(m["text"])); text != "" {
					return text
				}
			}
		}
		return contentToText(in.Messages[i].Content)
	}
	return ""
}

func stableCodexSessionID(registryKey string) string {
	return "ccp_" + hashString(registryKey)[:32]
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:])
}

func recordCodexSession(cfg config, info codexSessionInfo) {
	path := codexSessionFilePath(cfg)
	now := time.Now().Format(time.RFC3339)
	codexSessionMu.Lock()
	defer codexSessionMu.Unlock()
	registry := readCodexSessionRegistry(path)
	if registry.Version == 0 {
		registry.Version = 1
	}
	if registry.Sessions == nil {
		registry.Sessions = map[string]codexSessionRecord{}
	}
	record := registry.Sessions[info.RegistryKey]
	if record.CreatedAt == "" {
		record.CreatedAt = now
	}
	record.CodexSessionID = info.CodexSessionID
	record.FlowHash = info.FlowHash
	record.FlowSource = info.FlowSource
	record.SideThread = info.SideThread
	record.SideThreadKind = info.SideThreadKind
	record.UpdatedAt = now
	record.RequestCount++
	registry.Sessions[info.RegistryKey] = record
	registry.UpdatedAt = now
	if err := writeCodexSessionRegistry(path, registry); err != nil {
		traceLogID("", "codex.session_write_error", map[string]any{"path": path, "error": err.Error()})
	}
}

func readCodexSessionRegistry(path string) codexSessionRegistry {
	var registry codexSessionRegistry
	raw, err := os.ReadFile(path)
	if err != nil {
		return registry
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		return codexSessionRegistry{}
	}
	return registry
}

func writeCodexSessionRegistry(path string, registry codexSessionRegistry) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0600)
}

func codexSessionFilePath(cfg config) string {
	if filepath.IsAbs(cfg.CodexSessionFile) {
		return cfg.CodexSessionFile
	}
	return filepath.Clean(cfg.CodexSessionFile)
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
	source := anthropicBlockSource(block)
	if url := anthropicSourceImageURL(source); url != "" {
		return map[string]any{"type": "input_image", "image_url": url}
	}
	return nil
}

func anthropicBlockSource(block map[string]any) map[string]any {
	if source, ok := block["source"].(map[string]any); ok {
		return source
	}
	if source, ok := block["file"].(map[string]any); ok {
		return source
	}
	return block
}

func anthropicSourceImageURL(source map[string]any) string {
	if url := stringField(source, "url"); url != "" {
		return url
	}
	mediaType := firstNonEmpty(stringField(source, "media_type"), stringField(source, "mime_type"))
	data := stringField(source, "data")
	if data == "" || mediaType == "" {
		return ""
	}
	if !strings.EqualFold(stringField(source, "type"), "base64") && !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return ""
	}
	return "data:" + mediaType + ";base64," + data
}

func anthropicSourceMediaType(block, source map[string]any) string {
	return firstNonEmpty(
		stringField(source, "media_type"),
		stringField(source, "mime_type"),
		stringField(block, "media_type"),
		stringField(block, "mime_type"),
	)
}

func anthropicFileName(block, source map[string]any) string {
	return firstNonEmpty(
		stringField(block, "filename"),
		stringField(block, "title"),
		stringField(block, "name"),
		stringField(source, "filename"),
		stringField(source, "display_name"),
		stringField(source, "name"),
	)
}

func anthropicSourceText(source map[string]any) string {
	if strings.EqualFold(stringField(source, "type"), "text") {
		return firstNonEmpty(stringField(source, "text"), stringField(source, "content"), stringField(source, "data"))
	}
	mediaType := strings.ToLower(firstNonEmpty(stringField(source, "media_type"), stringField(source, "mime_type")))
	if strings.HasPrefix(mediaType, "text/") {
		return firstNonEmpty(stringField(source, "text"), stringField(source, "content"), stringField(source, "data"))
	}
	return firstNonEmpty(stringField(source, "text"), stringField(source, "content"))
}

func stringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := m[key]
		if !ok || value == nil {
			continue
		}
		text := strings.TrimSpace(fmt.Sprint(value))
		if text == "" || text == "<nil>" {
			continue
		}
		return text
	}
	return ""
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
	raw, _ := json.Marshal(codexRequestBody(body))
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

func codexRequestBody(body any) any {
	switch v := body.(type) {
	case responsesRequest:
		v.MaxOutputTokens = 0
		return v
	case *responsesRequest:
		if v == nil {
			return body
		}
		next := *v
		next.MaxOutputTokens = 0
		return next
	default:
		return body
	}
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
	if strings.TrimSpace(collected.StreamError) != "" {
		streamMsg := strings.TrimSpace(collected.StreamError)
		msg := "Codex upstream stream failed: " + streamMsg
		traceLog(ctx, "codex.stream_failure", map[string]any{"error": truncateString(msg, traceBodyLimit)})
		return responsesResponse{}, http.StatusBadGateway, msg, errors.New(msg)
	}
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
	if out.PromptCacheKey != "" && strings.Contains(lower, "prompt_cache_key") {
		out.PromptCacheKey = ""
		return codexRetryRequest{reason: "prompt_cache_key_not_accepted", request: out}, true
	}
	if out.MaxOutputTokens != 0 && strings.Contains(lower, "max_output_tokens") {
		out.MaxOutputTokens = 0
		return codexRetryRequest{reason: "max_output_tokens_not_accepted", request: out}, true
	}
	if out.Temperature != nil && strings.Contains(lower, "temperature") {
		out.Temperature = nil
		return codexRetryRequest{reason: "temperature_not_accepted", request: out}, true
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

func streamCodex(ctx context.Context, cfg config, out responsesRequest, in anthropicRequest, w http.ResponseWriter, r *http.Request) {
	out.Stream = true
	preflightThinking := webSearchPreflightThinking(in, out)
	streamStarted := preflightThinking != ""
	nextIndex := 0
	messageID := "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if streamStarted {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		inputTokens := estimateAnthropicRequestTokens(in)
		sendAnthropicMessageStart(w, messageID, firstNonEmpty(in.Model, out.Model), inputTokens, 0)
		traceLog(ctx, "anthropic.out.event", map[string]any{"event": "message_start", "message_id": messageID, "model": firstNonEmpty(in.Model, out.Model), "input_tokens_estimated": inputTokens})
		writeAnthropicThinkingBlock(ctx, w, nextIndex, preflightThinking, syntheticThinkingSignature("codex-web-search-preflight", preflightThinking))
		nextIndex++
		if flusher != nil {
			flusher.Flush()
		}
	}
	res, status, msg, err := callCodexResponses(ctx, cfg, out)
	if err != nil {
		if streamStarted {
			writeAnthropicTextBlock(ctx, w, nextIndex, "Proxy upstream error: "+msg)
			content := []any{map[string]any{"type": "text", "text": msg}}
			sendAnthropicMessageDeltaStop(w, anthropicStopReason(content), 0)
			sendEvent(w, "message_stop", map[string]any{"type": "message_stop"})
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}
		writeAnthropicError(w, status, msg)
		return
	}
	stopReason := anthropicStopReason(anthropicContentBlocks(res))
	updateRequestStat(r, func(stat *requestStat) {
		stat.InputTokens = res.Usage.InputTokens
		stat.OutputTokens = res.Usage.OutputTokens
		stat.StopReason = stopReason
	})

	flusher, _ := w.(http.Flusher)
	if !streamStarted {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		sendAnthropicMessageStart(w, messageID, firstNonEmpty(in.Model, out.Model), res.Usage.InputTokens, 0)
		traceLog(ctx, "anthropic.out.event", map[string]any{"event": "message_start", "message_id": messageID, "model": firstNonEmpty(in.Model, out.Model)})
	}
	writeAnthropicBufferedStreamFrom(ctx, w, res, nextIndex)
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
		msg := strings.TrimSpace(string(raw))
		writeOpenAIError(w, resp.StatusCode, msg)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	scanner := newSSEScanner(resp.Body)
	var outputText strings.Builder
	eventLines := []string{}
	flushEvent := func(lines []string) bool {
		data := ""
		for _, line := range lines {
			fmt.Fprint(w, line, "\n")
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			}
		}
		fmt.Fprint(w, "\n")
		if flusher != nil {
			flusher.Flush()
		}
		if data == "" || data == "[DONE]" {
			return false
		}
		var event responsesStreamEvent
		if json.Unmarshal([]byte(data), &event) != nil {
			return false
		}
		if text := codexStreamText(event); text != "" {
			outputText.WriteString(text)
		}
		return false
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if len(eventLines) > 0 {
				if flushEvent(eventLines) {
					return
				}
				eventLines = nil
				continue
			}
			fmt.Fprint(w, "\n")
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		eventLines = append(eventLines, line)
	}
	if len(eventLines) > 0 {
		_ = flushEvent(eventLines)
	}
	if err := scanner.Err(); err != nil {
		traceLog(ctx, "codex.responses_raw.scan_error", map[string]any{"error": err.Error()})
	}
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
		msg := strings.TrimSpace(string(raw))
		writeOpenAIError(w, resp.StatusCode, msg)
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
	var outputText strings.Builder
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
			outputText.WriteString(text)
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
		if item.Status == "" {
			item.Status = existing.Status
		}
		if item.Action == nil {
			item.Action = existing.Action
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
		if event.Response.Error.Message != "" || event.Response.Error.Code != "" || event.Response.Error.Type != "" {
			resp.Error = event.Response.Error
		}
		if msg := responsesStreamEventErrorMessage(event); msg != "" {
			resp.StreamError = firstNonEmpty(resp.StreamError, msg)
			traceLog(ctx, "codex.stream.error", map[string]any{"type": event.Type, "error": truncateString(msg, traceBodyLimit)})
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
		if !isContextCanceledError(err) {
			resp.StreamError = firstNonEmpty(resp.StreamError, "stream scan error: "+err.Error())
		}
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

func responsesStreamEventErrorMessage(event responsesStreamEvent) string {
	switch event.Type {
	case "error":
		return formatResponsesStreamError(
			firstNonEmpty(event.Error.Code, event.Code, event.Error.Type),
			firstNonEmpty(event.Error.Message, event.Message, event.Text, event.Delta, "stream error event without message"),
		)
	case "response.failed":
		return formatResponsesStreamError(
			firstNonEmpty(event.Response.Error.Code, event.Response.Error.Type, event.Error.Code, event.Code, event.Error.Type),
			firstNonEmpty(event.Response.Error.Message, event.Error.Message, event.Message, "response failed before returning output"),
		)
	default:
		if event.Response.Error.Message == "" && event.Response.Error.Code == "" && event.Response.Error.Type == "" {
			return ""
		}
		return formatResponsesStreamError(
			firstNonEmpty(event.Response.Error.Code, event.Response.Error.Type),
			firstNonEmpty(event.Response.Error.Message, "response contains error"),
		)
	}
}

func formatResponsesStreamError(code, msg string) string {
	code = strings.TrimSpace(code)
	msg = strings.TrimSpace(msg)
	if code != "" && !strings.Contains(strings.ToLower(msg), strings.ToLower(code)) {
		msg = code + ": " + msg
	}
	return msg
}

func isContextCanceledError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || strings.Contains(strings.ToLower(err.Error()), "context canceled")
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
				cleaned := stripFabricatedToolJSON(part.Text)
				if cleaned != "" {
					content = append(content, map[string]any{"type": "text", "text": cleaned})
				}
			}
		case "function_call":
			input := functionArgumentsToInput(item.Arguments)
			if toolActivityThinkingEnabled() {
				if summary := toolActivityThinkingText(item.Name, input); summary != "" {
					content = append(content, map[string]any{"type": "thinking", "thinking": summary, "signature": toolActivityThinkingSignature(item, summary)})
				}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    firstNonEmpty(item.CallID, item.ID, "toolu_"+strconv.FormatInt(time.Now().UnixNano(), 36)),
				"name":  item.Name,
				"input": input,
			})
		case "web_search_call":
			if summary := webSearchCallThinkingText(item); summary != "" {
				content = append(content, map[string]any{"type": "thinking", "thinking": summary, "signature": toolActivityThinkingSignature(item, summary)})
			}
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

func toolActivityThinkingEnabled() bool {
	return envFlag("CLAUDE_TOOL_ACTIVITY_THINKING", true)
}

func toolActivityThinkingText(toolName string, input any) string {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return ""
	}
	raw, _ := json.Marshal(safeToolActivityInput(input))
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return "Tool activity: requesting " + toolName + " with no arguments."
	}
	return "Tool activity: requesting " + toolName + " with arguments " + truncateString(string(raw), 2000)
}

func toolActivityThinkingSignature(item responsesOutputItem, thinking string) string {
	sum := sha256.Sum256([]byte(firstNonEmpty(item.CallID, item.ID, item.Name, item.Type) + "\n" + thinking))
	return "codex-tool-activity:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func syntheticThinkingSignature(prefix, thinking string) string {
	sum := sha256.Sum256([]byte(prefix + "\n" + thinking))
	return prefix + ":" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func webSearchPreflightThinking(in anthropicRequest, out responsesRequest) string {
	if !hasCodexWebSearchTool(out) {
		return ""
	}
	latest := strings.TrimSpace(latestUserText(in))
	if !requestLikelyNeedsWebSearch(latest) {
		return ""
	}
	if latest == "" {
		return "Tool activity: Codex web_search is enabled for this request. Waiting for Codex to report search activity."
	}
	return "Tool activity: Codex web_search is enabled. Codex will choose internal search queries from the latest user request: " + truncateString(latest, 500) + "\n\nIf the upstream exposes exact web_search query/action data, the proxy will show it in a later thinking block."
}

func requestLikelyNeedsWebSearch(text string) bool {
	text = strings.ToLower(text)
	for _, marker := range []string{"search the web", "web search", "search online", "look up", "find the official website", "official website", "homepage content", "latest", "current"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func webSearchCallThinkingText(item responsesOutputItem) string {
	parts := []string{"Tool activity: Codex web_search"}
	if item.Status != "" {
		parts = append(parts, "status="+item.Status)
	}
	if query := webSearchActionQuery(item.Action); query != "" {
		parts = append(parts, "query="+strconv.Quote(query))
	} else {
		parts = append(parts, "exact_query_not_exposed_by_upstream")
	}
	if item.ID != "" {
		parts = append(parts, "id="+shortID(item.ID))
	}
	return strings.Join(parts, " ")
}

func webSearchActionQuery(action any) string {
	switch v := action.(type) {
	case map[string]any:
		for _, key := range []string{"query", "search_query", "q"} {
			if query := strings.TrimSpace(fmt.Sprint(v[key])); query != "" && query != "<nil>" {
				return query
			}
		}
		for _, value := range v {
			if query := webSearchActionQuery(value); query != "" {
				return query
			}
		}
	case []any:
		for _, value := range v {
			if query := webSearchActionQuery(value); query != "" {
				return query
			}
		}
	}
	return ""
}

func shortID(value string) string {
	if len(value) <= 14 {
		return value
	}
	return value[:10] + "..." + value[len(value)-4:]
}

func estimateAnthropicRequestTokens(in anthropicRequest) int {
	chars := len(contentToText(in.System)) + len(in.Model)
	for _, msg := range in.Messages {
		chars += len(contentToText(msg.Content))
	}
	return estimateTextTokensFromChars(chars)
}

func estimateOpenAIChatRequestTokens(in openAIRequest) int {
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

func estimateResponsesRequestTokens(in responsesRequest) int {
	chars := len(in.Model) + len(in.Instructions) + len(in.PromptCacheKey)
	for _, item := range in.Input {
		chars += len(contentToText(item))
	}
	for _, tool := range in.Tools {
		chars += len(tool.Type) + len(tool.Name) + len(tool.Description) + len(contentToText(tool.Parameters))
	}
	return estimateTextTokensFromChars(chars)
}

func estimateCodexRequestTokens(in responsesRequest) int {
	return max(estimateResponsesRequestTokens(in), estimateJSONTokens(codexRequestBody(in)))
}

func estimateJSONTokens(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return estimateTextTokensFromChars(len(raw))
}

func estimateTextTokens(text string) int {
	return estimateTextTokensFromChars(len(text))
}

func estimateTextTokensFromChars(chars int) int {
	if chars <= 0 {
		return 1
	}
	return max(1, chars/4)
}

type tokenLimitDecision struct {
	Action       string
	Reason       string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	HardLimit    int
	Message      string
}

func codexTokenLimitDecision(inputTokens, requestedOutputTokens int) tokenLimitDecision {
	hardLimit := envIntDefault("CODEX_UPSTREAM_HARD_TOKENS", defaultTokenHardLimit)
	if hardLimit <= 0 || !codexBlockAtHardEnabled() {
		return tokenLimitDecision{}
	}
	outputTokens := max(0, requestedOutputTokens)
	totalTokens := inputTokens + outputTokens
	decision := tokenLimitDecision{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: totalTokens, HardLimit: hardLimit}
	switch {
	case inputTokens >= hardLimit:
		decision.Action = "error"
		decision.Reason = "input"
	case outputTokens >= hardLimit:
		decision.Action = "error"
		decision.Reason = "output"
	case totalTokens >= hardLimit:
		decision.Action = "error"
		decision.Reason = "total"
	}
	if decision.Action == "" {
		return tokenLimitDecision{}
	}
	decision.Message = tokenLimitHardText(decision)
	return decision
}

func codexBlockAtHardEnabled() bool {
	return envFlag("CODEX_UPSTREAM_BLOCK_AT_HARD", false)
}

func tokenLimitHardText(decision tokenLimitDecision) string {
	return fmt.Sprintf("context_length_exceeded: request is estimated at %s tokens against the configured hard limit of %s tokens.", formatTokenCount(decision.TotalTokens), formatTokenCount(decision.HardLimit))
}

func isContextLengthError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "context_length_exceeded") ||
		(strings.Contains(lower, "context") && strings.Contains(lower, "exceed"))
}

func formatTokenCount(tokens int) string {
	if tokens >= 1000000 {
		return fmt.Sprintf("%.1fm", float64(tokens)/1000000)
	}
	if tokens >= 1000 {
		return fmt.Sprintf("%.0fk", float64(tokens)/1000)
	}
	return strconv.Itoa(tokens)
}

func envIntDefault(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func requestedOpenAIOutputTokens(in openAIRequest) int {
	return max(in.MaxTokens, in.MaxCompletionTokens)
}

func applyTokenLimitErrorStat(r *http.Request, decision tokenLimitDecision, model, upstream string) {
	setRequestStat(r, requestStat{Model: model, Upstream: upstream, InputTokens: decision.InputTokens, OutputTokens: decision.OutputTokens, StopReason: "context_length_exceeded"})
}

func safeToolActivityInput(input any) any {
	switch v := input.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, value := range v {
			if sensitiveFieldName(key) {
				out[key] = "[redacted]"
				continue
			}
			out[key] = safeToolActivityInput(value)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, safeToolActivityInput(item))
		}
		return out
	default:
		return input
	}
}

func sensitiveFieldName(name string) bool {
	name = strings.ToLower(name)
	for _, marker := range []string{"authorization", "api_key", "apikey", "token", "secret", "password", "cookie"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
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
	return cleanClaudeToolInput(input)
}

func cleanClaudeToolInput(input any) any {
	switch v := input.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, value := range v {
			if strings.EqualFold(key, "pages") && emptyClaudeToolArgument(value) {
				continue
			}
			out[key] = cleanClaudeToolInput(value)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, cleanClaudeToolInput(item))
		}
		return out
	default:
		return input
	}
}

func emptyClaudeToolArgument(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	default:
		return false
	}
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
	writeAnthropicBufferedStreamFrom(ctx, w, resp, 0)
}

func writeAnthropicBufferedStreamFrom(ctx context.Context, w io.Writer, resp responsesResponse, startIndex int) {
	content := anthropicContentBlocks(resp)
	for i, block := range content {
		index := startIndex + i
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "thinking":
			writeAnthropicThinkingBlock(ctx, w, index, fmt.Sprint(m["thinking"]), fmt.Sprint(m["signature"]))
		case "text":
			writeAnthropicTextBlock(ctx, w, index, fmt.Sprint(m["text"]))
		case "tool_use":
			id := fmt.Sprint(m["id"])
			name := fmt.Sprint(m["name"])
			input := m["input"]
			sendEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}})
			traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_start", "index": index, "block_type": "tool_use", "id": id, "name": name})
			rawInput, _ := json.Marshal(input)
			if string(rawInput) != "{}" && string(rawInput) != "null" {
				sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(rawInput)}})
				traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_delta", "index": index, "delta_type": "input_json_delta", "json_chars": len(rawInput), "json_preview": truncateString(string(rawInput), 500)})
			}
			sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
			traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_stop", "index": index})
		}
	}
	sendAnthropicMessageDeltaStop(w, anthropicStopReason(content), resp.Usage.OutputTokens)
	sendEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	traceLog(ctx, "anthropic.out.event", map[string]any{"event": "message_delta", "stop_reason": anthropicStopReason(content), "output_tokens": resp.Usage.OutputTokens})
	traceLog(ctx, "anthropic.out.event", map[string]any{"event": "message_stop"})
}

func writeAnthropicThinkingBlock(ctx context.Context, w io.Writer, index int, thinking, signature string) {
	sendEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
	traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_start", "index": index, "block_type": "thinking"})
	if thinking != "" {
		sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": thinking}})
		traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_delta", "index": index, "delta_type": "thinking_delta", "thinking_chars": len(thinking), "thinking_preview": truncateString(thinking, 500)})
	}
	if signature != "" {
		sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "signature_delta", "signature": signature}})
		traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_delta", "index": index, "delta_type": "signature_delta"})
	}
	sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_stop", "index": index})
}

func writeAnthropicTextBlock(ctx context.Context, w io.Writer, index int, text string) {
	sendEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}})
	traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_start", "index": index, "block_type": "text"})
	if text != "" {
		sendEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": text}})
		traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_delta", "index": index, "delta_type": "text_delta", "text_chars": len(text), "text_preview": truncateString(text, 500)})
	}
	sendEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	traceLog(ctx, "anthropic.out.event", map[string]any{"event": "content_block_stop", "index": index})
}

func sendAnthropicMessageDeltaStop(w io.Writer, stopReason string, outputTokens int) {
	sendEvent(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": outputTokens}})
}

func openAIChatToResponses(cfg config, in openAIRequest) responsesRequest {
	model := resolveModel(cfg, in.Model)
	out := responsesRequest{Model: model, MaxOutputTokens: requestedOpenAIOutputTokens(in), Stream: in.Stream, Store: false}
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
		case "input_image":
			if url := stringField(m, "image_url"); url != "" {
				parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
			}
		case "file", "input_file":
			if file := openAIFileToResponses(m); file != nil {
				parts = append(parts, file)
			}
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

func openAIFileToResponses(block map[string]any) map[string]any {
	file := map[string]any{}
	if nested, ok := block["file"].(map[string]any); ok {
		for _, key := range []string{"file_id", "file_data", "filename"} {
			if value := stringField(nested, key); value != "" {
				file[key] = value
			}
		}
		if url := firstNonEmpty(stringField(nested, "url"), stringField(nested, "file_url")); url != "" {
			if strings.HasPrefix(strings.ToLower(url), "data:image/") || looksLikeImageURL(url) {
				return map[string]any{"type": "input_image", "image_url": url}
			}
			return map[string]any{"type": "input_text", "text": "Attached file: " + url}
		}
	}
	for _, key := range []string{"file_id", "file_data", "filename"} {
		if value := stringField(block, key); value != "" {
			file[key] = value
		}
	}
	if len(file) > 0 {
		file["type"] = "input_file"
		return file
	}
	if url := firstNonEmpty(stringField(block, "url"), stringField(block, "file_url")); url != "" {
		if strings.HasPrefix(strings.ToLower(url), "data:image/") || looksLikeImageURL(url) {
			return map[string]any{"type": "input_image", "image_url": url}
		}
		return map[string]any{"type": "input_text", "text": "Attached file: " + url}
	}
	return nil
}

func looksLikeImageURL(url string) bool {
	lower := strings.ToLower(strings.TrimSpace(url))
	for _, suffix := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
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
	model := resolveModel(cfg, in.Model)
	out := openAIRequest{Model: model, MaxTokens: in.MaxTokens, Temperature: in.Temperature, Stream: in.Stream, ReasoningEffort: requestReasoningEffort(cfg, in)}
	sys := contentToText(in.System)
	if len(in.Tools) > 0 {
		sys = strings.TrimSpace(sys + "\n\n" + noFakeToolJSONGuard)
	}
	if sys != "" {
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
	var parts []map[string]any
	var toolCalls []openAIToolCall
	var out []openAIMessage
	flushMessage := func() {
		if len(parts) == 0 && len(toolCalls) == 0 {
			return
		}
		out = append(out, openAIMessage{Role: msg.Role, Content: openAIContentFromParts(parts), ToolCalls: toolCalls})
		parts = nil
		toolCalls = nil
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
			if text := stringField(m, "text"); text != "" {
				parts = append(parts, openAITextPart(text))
			}
		case "image":
			if part := anthropicImageToOpenAIContentPart(m); part != nil {
				parts = append(parts, part)
			}
		case "document", "file":
			parts = append(parts, anthropicFileToOpenAIContentParts(m)...)
		case "tool_use":
			args, _ := json.Marshal(m["input"])
			toolCalls = append(toolCalls, openAIToolCall{ID: fmt.Sprint(m["id"]), Type: "function", Function: openAIToolFunction{Name: fmt.Sprint(m["name"]), Arguments: string(args)}})
		case "tool_result":
			flushMessage()
			out = append(out, openAIMessage{Role: "tool", ToolCallID: fmt.Sprint(m["tool_use_id"]), Content: contentToText(m["content"])})
		default:
			if text := contentToText(m); text != "" {
				parts = append(parts, openAITextPart(text))
			}
		}
	}
	flushMessage()
	return out
}

func openAITextPart(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func openAIContentFromParts(parts []map[string]any) any {
	if len(parts) == 0 {
		return ""
	}
	texts := []string{}
	for _, part := range parts {
		if fmt.Sprint(part["type"]) != "text" {
			out := make([]any, 0, len(parts))
			for _, p := range parts {
				out = append(out, p)
			}
			return out
		}
		if text := stringField(part, "text"); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func anthropicImageToOpenAIContentPart(block map[string]any) map[string]any {
	source := anthropicBlockSource(block)
	if url := anthropicSourceImageURL(source); url != "" {
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
	}
	return nil
}

func anthropicFileToOpenAIContentParts(block map[string]any) []map[string]any {
	source := anthropicBlockSource(block)
	mediaType := anthropicSourceMediaType(block, source)
	if strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		if part := anthropicImageToOpenAIContentPart(block); part != nil {
			return []map[string]any{part}
		}
	}
	if text := anthropicSourceText(source); text != "" {
		return []map[string]any{openAITextPart(text)}
	}
	file := map[string]any{}
	if fileID := stringField(source, "file_id", "id"); fileID != "" {
		file["file_id"] = fileID
	}
	if data := stringField(source, "file_data"); data != "" {
		file["file_data"] = data
	} else if strings.EqualFold(stringField(source, "type"), "base64") {
		if data := stringField(source, "data"); data != "" {
			file["file_data"] = data
		}
	}
	if filename := anthropicFileName(block, source); filename != "" {
		file["filename"] = filename
	}
	if len(file) > 0 {
		return []map[string]any{{"type": "file", "file": file}}
	}
	if uri := firstNonEmpty(stringField(source, "file_uri", "uri"), stringField(source, "url")); uri != "" {
		label := anthropicFileName(block, source)
		if label == "" {
			label = "attached file"
		}
		return []map[string]any{openAITextPart(label + ": " + uri)}
	}
	return nil
}

func proxyOpenAIChat(ctx context.Context, cfg config, out openAIRequest, w http.ResponseWriter, r *http.Request) {
	upstream := effectiveOpenAIUpstream(cfg, r)
	if upstream.APIKey == "" || upstream.APIKey == "YOUR_OPENAI_API_KEY" || strings.HasPrefix(upstream.APIKey, "sk-or-") {
		writeOpenAIError(w, http.StatusBadRequest, "OpenAI-compatible provider API key is not configured")
		return
	}
	out = prepareOpenAIRequestForUpstream(cfg, upstream, out)
	proxyOpenAIRequest(ctx, upstream.APIKey, strings.TrimRight(upstream.BaseURL, "/")+"/chat/completions", out, w)
}

func proxyOpenAIResponses(ctx context.Context, cfg config, out responsesRequest, w http.ResponseWriter, r *http.Request) {
	upstream := effectiveOpenAIUpstream(cfg, r)
	if upstream.APIKey == "" || upstream.APIKey == "YOUR_OPENAI_API_KEY" || strings.HasPrefix(upstream.APIKey, "sk-or-") {
		writeOpenAIError(w, http.StatusBadRequest, "OpenAI-compatible provider API key is not configured")
		return
	}
	proxyOpenAIRequest(ctx, upstream.APIKey, strings.TrimRight(upstream.BaseURL, "/")+"/responses", out, w)
}

func prepareOpenAIRequestForUpstream(cfg config, upstream providerCredential, out openAIRequest) openAIRequest {
	out.Model = resolveModel(cfg, out.Model)
	if normalizeReasoningEffort(out.ReasoningEffort) == "xhigh" {
		out.ReasoningEffort = "high"
	}
	return out
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

func proxyOpenAIPassthrough(ctx context.Context, cfg config, w http.ResponseWriter, r *http.Request, localPrefix string) {
	upstream := effectiveOpenAIUpstream(cfg, r)
	if upstream.APIKey == "" || upstream.APIKey == "YOUR_OPENAI_API_KEY" || strings.HasPrefix(upstream.APIKey, "sk-or-") {
		writeOpenAIError(w, http.StatusBadRequest, "OpenAI-compatible provider API key is not configured")
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, localPrefix)
	if suffix == r.URL.Path {
		writeOpenAIError(w, http.StatusBadRequest, "invalid OpenAI-compatible proxy path")
		return
	}
	upstreamURL := strings.TrimRight(upstream.BaseURL, "/") + suffix
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	copyPassthroughHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+upstream.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func copyPassthroughHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "Accept", "OpenAI-Beta", "OpenAI-Organization", "OpenAI-Project"} {
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "OpenAI-Request-ID", "X-Request-ID"} {
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
}

func callOpenAI(ctx context.Context, cfg config, out openAIRequest, requestedModel string, w http.ResponseWriter, r *http.Request) {
	upstream := effectiveOpenAIUpstream(cfg, r)
	if upstream.APIKey == "" || upstream.APIKey == "YOUR_OPENAI_API_KEY" || strings.HasPrefix(upstream.APIKey, "sk-or-") {
		writeAnthropicError(w, http.StatusBadRequest, "OpenAI-compatible provider API key is not configured")
		return
	}
	out = prepareOpenAIRequestForUpstream(cfg, upstream, out)
	body, _ := json.Marshal(out)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(upstream.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+upstream.APIKey)
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
	upstream := effectiveOpenAIUpstream(cfg, r)
	if upstream.APIKey == "" || upstream.APIKey == "YOUR_OPENAI_API_KEY" || strings.HasPrefix(upstream.APIKey, "sk-or-") {
		writeAnthropicError(w, http.StatusBadRequest, "OpenAI-compatible provider API key is not configured")
		return
	}
	out = prepareOpenAIRequestForUpstream(cfg, upstream, out)
	body, _ := json.Marshal(out)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(upstream.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+upstream.APIKey)
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
		if text := stripFabricatedToolJSON(contentToText(choice.Message.Content)); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, tc := range choice.Message.ToolCalls {
			input := functionArgumentsToInput(tc.Function.Arguments)
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

// noFakeToolJSONGuard is appended to the upstream system prompt whenever the
// request carries tool definitions. It prevents the upstream model (e.g. GPT
// behind the proxy) from imitating tool-result JSON in its text output, which
// has been observed to leak fabricated IDs and prices into customer replies.
const noFakeToolJSONGuard = "OUTPUT FORMAT RULE (HARD): Never emit JSON objects in your text content that mimic tool messages. Do not write text containing JSON with the keys \"tool_call\", \"tool_response\", \"tool_result\", or \"tool_use\". To invoke a tool, use the function/tool-call API only — do not narrate or rehearse it as JSON text. When citing data from tool results, paraphrase the values in natural language; do not reconstruct or invent JSON blocks. Never invent identifiers, package_ids, prices, amounts, dates, or any factual value that is not present verbatim in a real tool result message you have already received in this conversation."

// stripFabricatedToolJSON removes standalone JSON objects from text that contain
// any of the keys "tool_call", "tool_response", "tool_result", "tool_use". These
// are hallucinated tool-shaped blobs that the upstream model sometimes writes as
// text instead of using the proper tool/function-call mechanism. Leaving them in
// the assistant text leaks fabricated data to end users.
func stripFabricatedToolJSON(text string) string {
	if text == "" || !containsFakeToolMarker(text) {
		return text
	}
	runes := []rune(text)
	n := len(runes)
	var out strings.Builder
	out.Grow(len(text))
	stripped := 0
	i := 0
	for i < n {
		if runes[i] == '{' {
			if end, ok := matchJSONBrace(runes, i); ok {
				block := string(runes[i : end+1])
				if containsFakeToolMarker(block) {
					stripped++
					i = end + 1
					for i < n && (runes[i] == ' ' || runes[i] == '\t' || runes[i] == '\n' || runes[i] == '\r') {
						i++
					}
					continue
				}
			}
		}
		out.WriteRune(runes[i])
		i++
	}
	cleaned := strings.TrimSpace(out.String())
	if stripped > 0 {
		log.Printf("proxy: stripped %d fabricated tool-shaped JSON block(s) from upstream text output", stripped)
	}
	return cleaned
}

func containsFakeToolMarker(s string) bool {
	return strings.Contains(s, `"tool_call"`) ||
		strings.Contains(s, `"tool_response"`) ||
		strings.Contains(s, `"tool_result"`) ||
		strings.Contains(s, `"tool_use"`)
}

func matchJSONBrace(runes []rune, start int) (int, bool) {
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(runes); i++ {
		c := runes[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
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

type apiDocAuth string

const (
	apiDocAuthPublic apiDocAuth = "public"
	apiDocAuthProxy  apiDocAuth = "proxy_key"
	apiDocAuthAdmin  apiDocAuth = "admin_session"
)

type apiDocParam struct {
	Name        string
	In          string
	Type        string
	Description string
	Required    bool
	Example     any
}

type apiDocRoute struct {
	Group               string
	OperationID         string
	Method              string
	Path                string
	DisplayPath         string
	SourcePath          string
	Summary             string
	Description         string
	Auth                apiDocAuth
	Headers             []apiDocParam
	QueryParams         []apiDocParam
	PathParams          []apiDocParam
	RequestSchema       string
	ResponseSchema      string
	RequestContentType  string
	ResponseContentType string
	SuccessStatus       int
	RequestExample      any
	ResponseExample     any
	PostmanBodyMode     string
	PostmanFormData     []apiDocParam
}

func apiDocRoutes() []apiDocRoute {
	anthropicVersionHeader := apiDocParam{Name: "anthropic-version", In: "header", Type: "string", Description: "Anthropic API version. The proxy accepts this for Claude-compatible clients.", Required: true, Example: "{{anthropicVersion}}"}
	sessionHeader := apiDocParam{Name: "X-Proxy-Session-Id", In: "header", Type: "string", Description: "Optional stable local session id. X-Codex-Session-Id, X-OpenAI-Session-Id, X-Claude-Code-Session-Id, X-Claude-Session-Id, and X-Session-Id are also accepted.", Required: false, Example: "{{sessionId}}"}
	filePathParam := apiDocParam{Name: "path", In: "path", Type: "string", Description: "Provider file path after /openai/v1/files/, for example file-abc123 or file-abc123/content.", Required: true, Example: "file-abc123"}
	return []apiDocRoute{
		{
			Group: "Documentation", OperationID: "getDocs", Method: http.MethodGet, Path: "/docs", Summary: "Read local API documentation", Auth: apiDocAuthPublic,
			Description:         "Human-readable documentation generated from the same manifest as the OpenAPI and Postman exports.",
			ResponseContentType: "text/html", ResponseSchema: "HTMLDocument", ResponseExample: "<!doctype html>...",
		},
		{
			Group: "Documentation", OperationID: "getOpenAPI", Method: http.MethodGet, Path: "/openapi.json", Summary: "Download OpenAPI document", Auth: apiDocAuthPublic,
			Description:    "OpenAPI 3.0.3 JSON for Postman, Swagger UI, and other API tools.",
			ResponseSchema: "OpenAPIDocument", ResponseExample: map[string]any{"openapi": "3.0.3", "info": map[string]any{"title": "Connect AI Proxy API"}},
		},
		{
			Group: "Documentation", OperationID: "getPostmanCollection", Method: http.MethodGet, Path: "/postman.json", Summary: "Download Postman collection", Auth: apiDocAuthPublic,
			Description:    "Postman Collection v2.1 export generated from the local API documentation manifest.",
			ResponseSchema: "PostmanCollection", ResponseExample: map[string]any{"info": map[string]any{"name": "Connect AI Proxy API"}},
		},
		{
			Group: "Public", OperationID: "getHealth", Method: http.MethodGet, Path: "/health", Summary: "Check proxy health", Auth: apiDocAuthPublic,
			Description:    "Returns whether the local process is reachable and whether proxy endpoints are currently enabled.",
			ResponseSchema: "HealthResponse", ResponseExample: map[string]any{"ok": true, "proxy_running": true},
		},
		{
			Group: "Public", OperationID: "getAntigravityBridge", Method: http.MethodGet, Path: "/antigravity/bridge", Summary: "Open Antigravity browser bridge probe", Auth: apiDocAuthPublic,
			Description:         "Serves the local HTML page used by the browser extension bridge for a safe wake and connection probe.",
			ResponseContentType: "text/html", ResponseSchema: "HTMLDocument", ResponseExample: "<!doctype html>...",
		},
		{
			Group: "Anthropic-compatible", OperationID: "listAnthropicModels", Method: http.MethodGet, Path: "/anthropic/v1/models", Summary: "List Claude-compatible model aliases", Auth: apiDocAuthProxy,
			Description:    "Lists model aliases exposed through the Anthropic-compatible namespace.",
			Headers:        []apiDocParam{anthropicVersionHeader},
			ResponseSchema: "ModelListResponse", ResponseExample: map[string]any{"object": "list", "data": []map[string]any{{"id": "sonnet[1m]", "type": "model", "display_name": "sonnet[1m]"}}},
		},
		{
			Group: "Anthropic-compatible", OperationID: "listAnthropicModelCapabilities", Method: http.MethodGet, Path: "/anthropic/v1/model-capabilities", Summary: "List Claude-compatible model token limits", Auth: apiDocAuthProxy,
			Description:    "Lists exposed model aliases with upstream model ids, the active context window, maximum input budget, and maximum output tokens.",
			Headers:        []apiDocParam{anthropicVersionHeader},
			ResponseSchema: "ModelCapabilitiesResponse", ResponseExample: map[string]any{"object": "list", "units": "tokens", "data": []map[string]any{{"id": "sonnet[1m]", "type": "model_capability", "display_name": "sonnet[1m]", "upstream_model": "gpt-5.5", "context": "200k", "context_window_tokens": 200000, "max_input_tokens": 72000, "max_output_tokens": 128000, "limits_known": true, "limits_source": "built_in"}}},
		},
		{
			Group: "Anthropic-compatible", OperationID: "createAnthropicMessage", Method: http.MethodPost, Path: "/anthropic/v1/messages", Summary: "Create a Claude-compatible message", Auth: apiDocAuthProxy,
			Description: "Accepts Claude Messages API style requests, then routes them to Codex or an OpenAI-compatible provider. Streaming is supported with server-sent events.",
			Headers:     []apiDocParam{anthropicVersionHeader, sessionHeader}, RequestSchema: "AnthropicMessageRequest", ResponseSchema: "AnthropicMessageResponse",
			RequestExample:  map[string]any{"model": "{{model}}", "max_tokens": 256, "stream": false, "messages": []map[string]any{{"role": "user", "content": "Say hello in one sentence."}}},
			ResponseExample: map[string]any{"id": "msg_local", "type": "message", "role": "assistant", "content": []map[string]any{{"type": "text", "text": "Hello from the proxy."}}, "model": "{{model}}", "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]any{"input_tokens": 12, "output_tokens": 6}},
		},
		{
			Group: "Anthropic-compatible", OperationID: "countAnthropicTokens", Method: http.MethodPost, Path: "/anthropic/v1/messages/count_tokens", Summary: "Estimate Claude-compatible input tokens", Auth: apiDocAuthProxy,
			Description: "Returns a local approximate input token count for a Claude Messages API style request.",
			Headers:     []apiDocParam{anthropicVersionHeader}, RequestSchema: "AnthropicMessageRequest", ResponseSchema: "CountTokensResponse",
			RequestExample:  map[string]any{"model": "{{model}}", "messages": []map[string]any{{"role": "user", "content": "Quick count test"}}},
			ResponseExample: map[string]any{"input_tokens": 14},
		},
		{
			Group: "OpenAI-compatible", OperationID: "listOpenAIModels", Method: http.MethodGet, Path: "/openai/v1/models", Summary: "List OpenAI-compatible model aliases", Auth: apiDocAuthProxy,
			Description:    "Lists model aliases exposed through the OpenAI-compatible namespace.",
			ResponseSchema: "ModelListResponse", ResponseExample: map[string]any{"object": "list", "data": []map[string]any{{"id": "gpt-5.5", "type": "model", "display_name": "gpt-5.5"}}},
		},
		{
			Group: "OpenAI-compatible", OperationID: "listOpenAIModelCapabilities", Method: http.MethodGet, Path: "/openai/v1/model-capabilities", Summary: "List OpenAI-compatible model token limits", Auth: apiDocAuthProxy,
			Description:    "Lists exposed model aliases with upstream model ids, the active context window, maximum input budget, and maximum output tokens.",
			ResponseSchema: "ModelCapabilitiesResponse", ResponseExample: map[string]any{"object": "list", "units": "tokens", "data": []map[string]any{{"id": "gpt-5.3-codex", "type": "model_capability", "display_name": "gpt-5.3-codex", "upstream_model": "gpt-5.3-codex", "context": "200k", "context_window_tokens": 200000, "max_input_tokens": 72000, "max_output_tokens": 128000, "limits_known": true, "limits_source": "built_in"}}},
		},
		{
			Group: "OpenAI-compatible", OperationID: "createChatCompletion", Method: http.MethodPost, Path: "/openai/v1/chat/completions", Summary: "Create an OpenAI-compatible chat completion", Auth: apiDocAuthProxy,
			Description: "Accepts OpenAI Chat Completions style requests. The proxy either forwards to a provider route or converts to Codex Responses internally.",
			Headers:     []apiDocParam{sessionHeader}, RequestSchema: "OpenAIChatCompletionRequest", ResponseSchema: "OpenAIChatCompletionResponse",
			RequestExample:  map[string]any{"model": "{{model}}", "messages": []map[string]any{{"role": "user", "content": "Say hello."}}, "max_completion_tokens": 256, "stream": false},
			ResponseExample: map[string]any{"id": "chatcmpl_local", "object": "chat.completion", "created": 1767225600, "model": "{{model}}", "choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "Hello from the proxy."}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 6}},
		},
		{
			Group: "OpenAI-compatible", OperationID: "createResponse", Method: http.MethodPost, Path: "/openai/v1/responses", Summary: "Create an OpenAI-compatible response", Auth: apiDocAuthProxy,
			Description: "Accepts OpenAI Responses API style requests. The local Codex route disables storage and can map stable session ids to local prompt cache keys.",
			Headers:     []apiDocParam{sessionHeader}, RequestSchema: "ResponsesRequest", ResponseSchema: "ResponsesResponse",
			RequestExample:  map[string]any{"model": "{{model}}", "input": []map[string]any{{"role": "user", "content": "Say hello."}}, "stream": false},
			ResponseExample: map[string]any{"id": "resp_local", "object": "response", "model": "{{model}}", "output": []map[string]any{{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "Hello from the proxy."}}}}, "usage": map[string]any{"input_tokens": 12, "output_tokens": 6}},
		},
		{
			Group: "OpenAI-compatible", OperationID: "listOpenAIFiles", Method: http.MethodGet, Path: "/openai/v1/files", Summary: "List provider files", Auth: apiDocAuthProxy,
			Description:    "OpenAI-compatible provider pass-through. Requires the default upstream to be OpenAI-compatible or the client key to route to a provider key.",
			QueryParams:    []apiDocParam{{Name: "limit", In: "query", Type: "integer", Description: "Provider-specific page size.", Example: 20}, {Name: "after", In: "query", Type: "string", Description: "Provider-specific cursor.", Example: "file-abc123"}, {Name: "purpose", In: "query", Type: "string", Description: "Provider-specific file purpose filter.", Example: "assistants"}},
			ResponseSchema: "ProviderPassThroughResponse", ResponseExample: map[string]any{"object": "list", "data": []map[string]any{{"id": "file-abc123", "object": "file", "purpose": "assistants"}}},
		},
		{
			Group: "OpenAI-compatible", OperationID: "uploadOpenAIFile", Method: http.MethodPost, Path: "/openai/v1/files", Summary: "Upload a provider file", Auth: apiDocAuthProxy,
			Description:        "OpenAI-compatible multipart file upload pass-through.",
			RequestContentType: "multipart/form-data", ResponseSchema: "ProviderPassThroughResponse", PostmanBodyMode: "formdata",
			PostmanFormData: []apiDocParam{{Name: "purpose", Type: "string", Description: "Provider file purpose.", Required: true, Example: "assistants"}, {Name: "file", Type: "file", Description: "File selected in Postman.", Required: true}},
			ResponseExample: map[string]any{"id": "file-abc123", "object": "file", "purpose": "assistants"},
		},
		{
			Group: "OpenAI-compatible", OperationID: "getOpenAIFilePath", Method: http.MethodGet, Path: "/openai/v1/files/{path}", DisplayPath: "/openai/v1/files/{path...}", SourcePath: "/openai/v1/files/", Summary: "Retrieve a provider file path", Auth: apiDocAuthProxy,
			Description: "OpenAI-compatible provider pass-through for nested file paths such as file metadata or file content.",
			PathParams:  []apiDocParam{filePathParam}, ResponseSchema: "ProviderPassThroughResponse", ResponseExample: map[string]any{"id": "file-abc123", "object": "file"},
		},
		{
			Group: "OpenAI-compatible", OperationID: "deleteOpenAIFilePath", Method: http.MethodDelete, Path: "/openai/v1/files/{path}", DisplayPath: "/openai/v1/files/{path...}", SourcePath: "/openai/v1/files/", Summary: "Delete a provider file path", Auth: apiDocAuthProxy,
			Description: "OpenAI-compatible provider pass-through for file deletion where the upstream provider supports it.",
			PathParams:  []apiDocParam{filePathParam}, ResponseSchema: "ProviderPassThroughResponse", ResponseExample: map[string]any{"id": "file-abc123", "object": "file", "deleted": true},
		},
		{
			Group: "Admin auth", OperationID: "getAdminAuthStatus", Method: http.MethodGet, Path: "/ui/api/auth/status", Summary: "Check admin login status", Auth: apiDocAuthPublic,
			Description:    "Returns whether local admin auth is configured and whether the current browser session is authenticated.",
			ResponseSchema: "UIAuthStatusResponse", ResponseExample: map[string]any{"configured": true, "authenticated": true, "username": "admin"},
		},
		{
			Group: "Admin auth", OperationID: "setupAdminAuth", Method: http.MethodPost, Path: "/ui/api/auth/setup", Summary: "Create local admin login", Auth: apiDocAuthPublic,
			Description:   "Creates the first local admin login. Fails once admin auth is already configured.",
			RequestSchema: "UIAuthCredentialsRequest", ResponseSchema: "OKResponse",
			RequestExample:  map[string]any{"username": "{{adminUsername}}", "password": "{{adminPassword}}"},
			ResponseExample: map[string]any{"ok": true, "message": "Admin login configured."},
		},
		{
			Group: "Admin auth", OperationID: "loginAdmin", Method: http.MethodPost, Path: "/ui/api/auth/login", Summary: "Log in to the admin UI", Auth: apiDocAuthPublic,
			Description:   "Sets the local admin session cookie when the credentials are valid.",
			RequestSchema: "UIAuthCredentialsRequest", ResponseSchema: "OKResponse",
			RequestExample:  map[string]any{"username": "{{adminUsername}}", "password": "{{adminPassword}}", "remember": true},
			ResponseExample: map[string]any{"ok": true},
		},
		{
			Group: "Admin auth", OperationID: "logoutAdmin", Method: http.MethodPost, Path: "/ui/api/auth/logout", Summary: "Clear admin session", Auth: apiDocAuthPublic,
			Description:    "Expires the local admin session cookie.",
			ResponseSchema: "OKResponse", ResponseExample: map[string]any{"ok": true},
		},
		{
			Group: "Admin control panel", OperationID: "getUIStatus", Method: http.MethodGet, Path: "/ui/api/status", Summary: "Get dashboard status", Auth: apiDocAuthAdmin,
			Description:    "Returns runtime status, local and public URLs, model rows, auth metadata, dashboard metrics, and the latest request summary.",
			ResponseSchema: "UIStatusResponse", ResponseExample: map[string]any{"running": true, "proxy_running": true, "version": "0.4.3", "local_url": "http://127.0.0.1:4000", "public_url": "https://proxy.example.com", "display_url": "https://proxy.example.com", "anthropic_url": "https://proxy.example.com/anthropic", "openai_url": "https://proxy.example.com/openai/v1", "upstream": "codex", "update": map[string]any{"current_version": "0.4.3", "latest_version": "0.4.4", "update_available": true, "running": false}},
		},
		{
			Group: "Admin control panel", OperationID: "getUIUpdateStatus", Method: http.MethodGet, Path: "/ui/api/update/status", Summary: "Get update status", Auth: apiDocAuthAdmin,
			Description:    "Returns the installed version, latest GitHub version check, auto-update setting, and the last dashboard-started update status.",
			ResponseSchema: "UIUpdateStatusResponse", ResponseExample: map[string]any{"current_version": "0.4.3", "latest_version": "0.4.4", "update_available": true, "auto_update": false, "state": "idle", "running": false},
		},
		{
			Group: "Admin control panel", OperationID: "checkDashboardUpdate", Method: http.MethodPost, Path: "/ui/api/update/check", Summary: "Check for updates", Auth: apiDocAuthAdmin,
			Description:     "Refreshes the configured VERSION URL and returns the current update status without starting an update.",
			ResponseSchema:  "UIUpdateStartResponse",
			ResponseExample: map[string]any{"ok": true, "message": "Update check complete.", "update": map[string]any{"current_version": "0.4.3", "latest_version": "0.4.4", "update_available": true}},
		},
		{
			Group: "Admin control panel", OperationID: "startDashboardUpdate", Method: http.MethodPost, Path: "/ui/api/update/start", Summary: "Start non-interactive update", Auth: apiDocAuthAdmin,
			Description:    "Starts update-linux.sh in the background with a status file so the dashboard can reconnect after the service restarts.",
			ResponseSchema: "UIUpdateStartResponse", ResponseExample: map[string]any{"ok": true, "message": "Update started.", "update": map[string]any{"state": "queued", "running": true}},
		},
		{
			Group: "Admin control panel", OperationID: "saveDashboardUpdateSettings", Method: http.MethodPost, Path: "/ui/api/update/settings", Summary: "Save update settings", Auth: apiDocAuthAdmin,
			Description:   "Saves automatic-update, branch, VERSION URL, repository path, status-file path, and full-system updater options.",
			RequestSchema: "UIUpdateSettingsRequest", ResponseSchema: "UIUpdateSettingsResponse",
			RequestExample:  map[string]any{"auto_update": true, "full_system_update": false, "branch": "main", "version_url": defaultVersionURL, "repo_dir": "/root/claude-code-proxy", "status_file": "/opt/connect-ai-proxy/.proxy.update.json"},
			ResponseExample: map[string]any{"ok": true, "auto_update": true, "full_system_update": false, "update": map[string]any{"auto_update": true}},
		},
		{
			Group: "Admin control panel", OperationID: "getUIConfig", Method: http.MethodGet, Path: "/ui/api/config", Summary: "Read editable proxy config", Auth: apiDocAuthAdmin,
			Description:    "Returns .env-backed configuration values for the control panel. Secret-looking values are masked in config fields.",
			ResponseSchema: "UIConfigResponse", ResponseExample: map[string]any{"config": map[string]any{"OPENAI_BASE_URL": "https://api.openai.com/v1", "PROXY_API_KEY": "sk-...abcd"}, "secrets": map[string]any{"PROXY_API_KEY": "{{proxyApiKey}}"}, "aliases": []map[string]any{}},
		},
		{
			Group: "Admin control panel", OperationID: "saveUIConfig", Method: http.MethodPost, Path: "/ui/api/config", Summary: "Save editable proxy config", Auth: apiDocAuthAdmin,
			Description:   "Persists selected .env values and model aliases. Restart the proxy to apply process-level settings.",
			RequestSchema: "UIConfigRequest", ResponseSchema: "OKResponse",
			RequestExample:  map[string]any{"config": map[string]any{"OPENAI_BASE_URL": "https://api.openai.com/v1"}, "aliases": []map[string]any{{"From": "sonnet[1m]", "To": "gpt-5.5", "Context": "1m"}}},
			ResponseExample: map[string]any{"ok": true, "message": "Configuration saved. Restart proxy to apply changes."},
		},
		{
			Group: "Admin control panel", OperationID: "getUIModels", Method: http.MethodGet, Path: "/ui/api/models", Summary: "List model aliases", Auth: apiDocAuthAdmin,
			ResponseSchema: "UIModelsResponse", ResponseExample: map[string]any{"models": []map[string]any{{"alias": "sonnet[1m]", "real": "gpt-5.5", "context": "1m"}}},
		},
		{
			Group: "Admin control panel", OperationID: "saveUIModels", Method: http.MethodPost, Path: "/ui/api/models", Summary: "Save model aliases", Auth: apiDocAuthAdmin,
			RequestSchema: "UIModelsRequest", ResponseSchema: "UIModelsResponse",
			RequestExample:  map[string]any{"models": []map[string]any{{"alias": "sonnet[1m]", "real": "gpt-5.5", "context": "1m"}}},
			ResponseExample: map[string]any{"ok": true, "message": "Model aliases saved and active for new requests.", "models": []map[string]any{{"alias": "sonnet[1m]", "real": "gpt-5.5", "context": "1m"}}},
		},
		{
			Group: "Admin control panel", OperationID: "getUIKeys", Method: http.MethodGet, Path: "/ui/api/keys", Summary: "List provider and client keys", Auth: apiDocAuthAdmin,
			ResponseSchema: "UIKeysResponse", ResponseExample: map[string]any{"providers": []map[string]any{}, "clients": []map[string]any{}, "defaults": map[string]any{"anthropic_base": "https://proxy.example.com/anthropic", "openai_local_url": "https://proxy.example.com/openai/v1"}},
		},
		{
			Group: "Admin control panel", OperationID: "saveUIProviderKey", Method: http.MethodPost, Path: "/ui/api/keys/provider", Summary: "Create or renew a provider key", Auth: apiDocAuthAdmin,
			RequestSchema: "UIProviderKeyRequest", ResponseSchema: "UIKeysResponse",
			RequestExample:  map[string]any{"provider": "openai", "label": "OpenAI key", "base_url": "https://api.openai.com/v1", "api_key": "{{providerApiKey}}"},
			ResponseExample: map[string]any{"ok": true, "message": "Provider key saved.", "providers": []map[string]any{}, "clients": []map[string]any{}},
		},
		{
			Group: "Admin control panel", OperationID: "createUIClientKey", Method: http.MethodPost, Path: "/ui/api/keys/client", Summary: "Create a local client API key", Auth: apiDocAuthAdmin,
			RequestSchema: "UIClientKeyRequest", ResponseSchema: "UIClientKeyCreateResponse",
			RequestExample:  map[string]any{"label": "Postman", "schema": "both", "provider": "default", "provider_key_id": ""},
			ResponseExample: map[string]any{"ok": true, "message": "Client key created. Copy it now; it will not be shown again.", "api_key": "{{proxyApiKey}}", "providers": []map[string]any{}, "clients": []map[string]any{}},
		},
		{
			Group: "Admin control panel", OperationID: "toggleUIKey", Method: http.MethodPost, Path: "/ui/api/keys/toggle", Summary: "Enable or disable a key", Auth: apiDocAuthAdmin,
			RequestSchema: "UIKeyToggleRequest", ResponseSchema: "UIKeysResponse",
			RequestExample:  map[string]any{"kind": "client", "id": "{{clientKeyId}}", "enabled": true},
			ResponseExample: map[string]any{"ok": true, "providers": []map[string]any{}, "clients": []map[string]any{}},
		},
		{
			Group: "Admin control panel", OperationID: "validateProxy", Method: http.MethodGet, Path: "/ui/api/validate", Summary: "Run local validation checks", Auth: apiDocAuthAdmin,
			ResponseSchema: "UIValidateResponse", ResponseExample: map[string]any{"ok": true, "ran_at": "12:00:00", "model": "sonnet[1m]", "upstream_model": "gpt-5.5", "steps": []map[string]any{{"name": "GET /anthropic/v1/models", "ok": true, "status": 200}}, "duration_total": 123},
		},
		{
			Group: "Admin control panel", OperationID: "testProxyRequest", Method: http.MethodPost, Path: "/ui/api/test", Summary: "Send a quick test prompt", Auth: apiDocAuthAdmin,
			RequestSchema: "UITestRequest", ResponseSchema: "UITestResponse",
			RequestExample:  map[string]any{"model": "{{model}}", "prompt": "Reply with only OK.", "stream": false},
			ResponseExample: map[string]any{"status": 200, "duration_ms": 450, "raw": "{\"type\":\"message\"}", "text": "OK"},
		},
		{
			Group: "Admin control panel", OperationID: "getUILogs", Method: http.MethodGet, Path: "/ui/api/logs", Summary: "Read local logs", Auth: apiDocAuthAdmin,
			ResponseSchema: "UILogsResponse", ResponseExample: map[string]any{"rows": []map[string]any{}, "stdout": "", "stderr": "", "trace": ""},
		},
		{
			Group: "Admin control panel", OperationID: "getUIAntigravity", Method: http.MethodGet, Path: "/ui/api/antigravity", Summary: "Read browser bridge status", Auth: apiDocAuthAdmin,
			ResponseSchema: "AntigravityStatusResponse", ResponseExample: map[string]any{"available": true, "mode": "dedicated", "last_action": "browser_status"},
		},
		{
			Group: "Admin control panel", OperationID: "probeUIAntigravity", Method: http.MethodPost, Path: "/ui/api/antigravity/probe", Summary: "Record browser bridge probe data", Auth: apiDocAuthAdmin,
			RequestSchema: "AntigravityProbeRequest", ResponseSchema: "AntigravityProbeResponse",
			RequestExample:  map[string]any{"source": "postman", "url": "http://127.0.0.1:4000/antigravity/bridge"},
			ResponseExample: map[string]any{"ok": true, "probe": map[string]any{"source": "postman"}},
		},
		{
			Group: "Admin control panel", OperationID: "stopProxy", Method: http.MethodPost, Path: "/ui/api/proxy/stop", Summary: "Stop proxy endpoints", Auth: apiDocAuthAdmin,
			ResponseSchema: "OKResponse", ResponseExample: map[string]any{"ok": true, "running": false, "message": "Proxy endpoints stopped. Claude settings restored. Control panel remains online."},
		},
		{
			Group: "Admin control panel", OperationID: "startProxy", Method: http.MethodPost, Path: "/ui/api/proxy/start", Summary: "Start proxy endpoints", Auth: apiDocAuthAdmin,
			ResponseSchema: "OKResponse", ResponseExample: map[string]any{"ok": true, "running": true, "message": "Proxy endpoints started. Claude settings applied."},
		},
		{
			Group: "Admin control panel", OperationID: "restartProxy", Method: http.MethodPost, Path: "/ui/api/proxy/restart", Summary: "Restart proxy process", Auth: apiDocAuthAdmin,
			ResponseSchema: "OKResponse", ResponseExample: map[string]any{"ok": true, "message": "Proxy restarting."},
		},
	}
}

func handleAPIDocs(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = io.WriteString(w, renderAPIDocsHTML(cfg))
	}
}

func handleOpenAPI(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, buildOpenAPISpec(cfg))
	}
}

func handlePostmanCollection(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, buildPostmanCollection(cfg))
	}
}

func apiDocsBaseURL(cfg config) string {
	return clientFacingBaseURL(cfg)
}

func renderAPIDocsHTML(cfg config) string {
	routes := apiDocRoutes()
	groups := apiDocGroups(routes)
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"/><meta name="viewport" content="width=device-width, initial-scale=1"/><title>Connect AI Proxy API Docs</title>`)
	b.WriteString(`<style>:root{color-scheme:dark;--bg:#0c0f12;--panel:#141920;--panel2:#11161c;--line:#26313c;--fg:#eef3f8;--muted:#9ba8b5;--accent:#78c7ff;--ok:#77d7a2;--warn:#ffd275}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 Inter,Segoe UI,Arial,sans-serif}a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}header{position:sticky;top:0;z-index:2;background:rgba(12,15,18,.94);border-bottom:1px solid var(--line);backdrop-filter:blur(10px)}.wrap{max-width:1180px;margin:0 auto;padding:24px}.hero{display:grid;gap:10px}.eyebrow{color:var(--muted);font-size:12px;text-transform:uppercase;letter-spacing:.08em}h1{margin:0;font-size:32px;line-height:1.1}h2{margin:32px 0 12px;font-size:19px}h3{margin:0;font-size:17px}.links{display:flex;flex-wrap:wrap;gap:10px;margin-top:12px}.btn{display:inline-flex;align-items:center;border:1px solid var(--line);background:var(--panel);color:var(--fg);border-radius:7px;padding:8px 10px}.grid{display:grid;grid-template-columns:280px minmax(0,1fr);gap:24px}.nav{position:sticky;top:104px;align-self:start;border:1px solid var(--line);border-radius:8px;background:var(--panel2);padding:12px}.nav a{display:block;color:var(--muted);padding:5px 6px;border-radius:5px}.nav a:hover{background:var(--panel);color:var(--fg);text-decoration:none}.card{border:1px solid var(--line);border-radius:8px;background:var(--panel);margin:12px 0;padding:16px}.meta{display:flex;flex-wrap:wrap;gap:8px;margin:10px 0}.tag{border:1px solid var(--line);border-radius:999px;color:var(--muted);padding:2px 8px;font-size:12px}.method{font-weight:700;color:var(--ok)}.path{font-family:JetBrains Mono,Consolas,monospace;color:#d8ecff}.desc{color:var(--muted)}pre{overflow:auto;background:#090c0f;border:1px solid var(--line);border-radius:7px;padding:12px;color:#dbe7f2}code{font-family:JetBrains Mono,Consolas,monospace}.note{border-left:3px solid var(--warn);padding-left:10px;color:#d7c28b}.small{font-size:12px;color:var(--muted)}@media(max-width:880px){.grid{grid-template-columns:1fr}.nav{position:static}}</style></head><body>`)
	b.WriteString(`<header><div class="wrap hero"><div class="eyebrow">Local API documentation</div><h1>Connect AI Proxy API</h1><div class="desc">Readable endpoint docs plus importable OpenAPI and Postman exports. No local secrets are embedded; examples use variables such as <code>{{proxyApiKey}}</code>.</div><div class="links">`)
	b.WriteString(`<a class="btn" href="/openapi.json">OpenAPI JSON</a><a class="btn" href="/postman.json">Postman collection</a><a class="btn" href="/">Control panel</a>`)
	b.WriteString(`</div><div class="small">Base URL: <code>` + html.EscapeString(apiDocsBaseURL(cfg)) + `</code></div></div></header>`)
	b.WriteString(`<div class="wrap grid"><nav class="nav"><strong>Groups</strong>`)
	for _, group := range groups {
		b.WriteString(`<a href="#group-` + html.EscapeString(docAnchor(group)) + `">` + html.EscapeString(group) + `</a>`)
	}
	b.WriteString(`</nav><main>`)
	b.WriteString(`<section class="card"><h3>Authentication</h3><p class="desc">Proxy endpoints accept <code>x-api-key</code>, <code>anthropic-api-key</code>, <code>api-key</code>, or <code>Authorization: Bearer {{proxyApiKey}}</code>. Admin endpoints use the <code>ccp_admin_session</code> cookie created by <code>/ui/api/auth/setup</code> or <code>/ui/api/auth/login</code>.</p></section>`)
	for _, group := range groups {
		b.WriteString(`<h2 id="group-` + html.EscapeString(docAnchor(group)) + `">` + html.EscapeString(group) + `</h2>`)
		for _, route := range routes {
			if route.Group != group {
				continue
			}
			b.WriteString(`<article class="card" id="` + html.EscapeString(route.OperationID) + `">`)
			b.WriteString(`<h3><span class="method">` + html.EscapeString(route.Method) + `</span> <span class="path">` + html.EscapeString(routeDisplayPath(route)) + `</span></h3>`)
			b.WriteString(`<div class="meta"><span class="tag">` + html.EscapeString(route.Summary) + `</span><span class="tag">` + html.EscapeString(apiDocAuthLabel(route.Auth)) + `</span></div>`)
			if route.Description != "" {
				b.WriteString(`<p class="desc">` + html.EscapeString(route.Description) + `</p>`)
			}
			if len(route.Headers) > 0 || len(route.QueryParams) > 0 || len(route.PathParams) > 0 {
				b.WriteString(`<p class="small">`)
				writeDocParamsHTML(&b, "Headers", route.Headers)
				writeDocParamsHTML(&b, "Query", route.QueryParams)
				writeDocParamsHTML(&b, "Path", route.PathParams)
				b.WriteString(`</p>`)
			}
			if route.RequestExample != nil {
				b.WriteString(`<h4>Request example</h4><pre><code>` + html.EscapeString(docExample(route.RequestExample)) + `</code></pre>`)
			} else if len(route.PostmanFormData) > 0 {
				b.WriteString(`<h4>Request form data</h4><pre><code>`)
				for _, p := range route.PostmanFormData {
					b.WriteString(html.EscapeString(p.Name + " = " + fmt.Sprint(p.Example) + "\n"))
				}
				b.WriteString(`</code></pre>`)
			}
			if route.ResponseExample != nil {
				b.WriteString(`<h4>Response example</h4><pre><code>` + html.EscapeString(docExample(route.ResponseExample)) + `</code></pre>`)
			}
			if route.Auth == apiDocAuthAdmin {
				b.WriteString(`<p class="note">Admin routes require a valid local browser session cookie. In Postman, call the login endpoint first and let Postman store the cookie, or set <code>{{adminSessionCookie}}</code>.</p>`)
			}
			b.WriteString(`</article>`)
		}
	}
	b.WriteString(`</main></div></body></html>`)
	return b.String()
}

func writeDocParamsHTML(b *strings.Builder, label string, params []apiDocParam) {
	if len(params) == 0 {
		return
	}
	b.WriteString(`<strong>` + html.EscapeString(label) + `:</strong> `)
	for i, p := range params {
		if i > 0 {
			b.WriteString(`, `)
		}
		b.WriteString(`<code>` + html.EscapeString(p.Name) + `</code>`)
	}
	b.WriteString(`. `)
}

func apiDocGroups(routes []apiDocRoute) []string {
	seen := map[string]bool{}
	var groups []string
	for _, route := range routes {
		if route.Group == "" || seen[route.Group] {
			continue
		}
		seen[route.Group] = true
		groups = append(groups, route.Group)
	}
	return groups
}

func routeDisplayPath(route apiDocRoute) string {
	if route.DisplayPath != "" {
		return route.DisplayPath
	}
	return route.Path
}

func routeCoveragePath(route apiDocRoute) string {
	if route.SourcePath != "" {
		return route.SourcePath
	}
	return route.Path
}

func docAnchor(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	replacer := strings.NewReplacer(" ", "-", "/", "-", "{", "", "}", "", ".", "", ":", "", "_", "-")
	return strings.Trim(replacer.Replace(s), "-")
}

func docExample(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

func apiDocAuthLabel(auth apiDocAuth) string {
	switch auth {
	case apiDocAuthProxy:
		return "Proxy API key"
	case apiDocAuthAdmin:
		return "Admin session cookie"
	default:
		return "Public local"
	}
}

func buildOpenAPISpec(cfg config) map[string]any {
	paths := map[string]any{}
	for _, route := range apiDocRoutes() {
		pathItem, _ := paths[route.Path].(map[string]any)
		if pathItem == nil {
			pathItem = map[string]any{}
		}
		op := map[string]any{
			"operationId": route.OperationID,
			"summary":     route.Summary,
			"description": strings.TrimSpace(route.Description + "\n\nAuth: " + apiDocAuthLabel(route.Auth)),
			"tags":        []string{route.Group},
			"responses":   openAPIResponses(route),
		}
		if params := openAPIParameters(route); len(params) > 0 {
			op["parameters"] = params
		}
		if req := openAPIRequestBody(route); req != nil {
			op["requestBody"] = req
		}
		if sec := openAPISecurity(route.Auth); len(sec) > 0 {
			op["security"] = sec
		}
		pathItem[strings.ToLower(route.Method)] = op
		paths[route.Path] = pathItem
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Connect AI Proxy API",
			"version":     "1.0.0",
			"description": "Local Postman-ready API documentation for connect-ai-proxy. Examples use placeholders and never embed local secrets.",
		},
		"servers": []map[string]any{{"url": apiDocsBaseURL(cfg), "description": "Local proxy"}},
		"tags":    openAPITags(apiDocGroups(apiDocRoutes())),
		"paths":   paths,
		"components": map[string]any{
			"securitySchemes": openAPISecuritySchemes(),
			"schemas":         openAPISchemas(),
		},
	}
}

func openAPITags(groups []string) []map[string]string {
	tags := make([]map[string]string, 0, len(groups))
	for _, group := range groups {
		tags = append(tags, map[string]string{"name": group})
	}
	return tags
}

func openAPIParameters(route apiDocRoute) []map[string]any {
	all := append([]apiDocParam{}, route.PathParams...)
	all = append(all, route.QueryParams...)
	all = append(all, route.Headers...)
	params := make([]map[string]any, 0, len(all))
	for _, p := range all {
		in := p.In
		if in == "" {
			in = "query"
		}
		param := map[string]any{
			"name":        p.Name,
			"in":          in,
			"description": p.Description,
			"required":    p.Required || in == "path",
			"schema":      openAPIParamSchema(p),
		}
		if p.Example != nil {
			param["example"] = p.Example
		}
		params = append(params, param)
	}
	return params
}

func openAPIParamSchema(p apiDocParam) map[string]any {
	switch p.Type {
	case "integer":
		return map[string]any{"type": "integer"}
	case "boolean":
		return map[string]any{"type": "boolean"}
	case "file":
		return map[string]any{"type": "string", "format": "binary"}
	default:
		return map[string]any{"type": "string"}
	}
}

func openAPIRequestBody(route apiDocRoute) map[string]any {
	if route.RequestSchema == "" && route.RequestExample == nil && len(route.PostmanFormData) == 0 {
		return nil
	}
	contentType := firstNonEmpty(route.RequestContentType, "application/json")
	media := map[string]any{}
	if len(route.PostmanFormData) > 0 {
		props := map[string]any{}
		required := []string{}
		for _, p := range route.PostmanFormData {
			props[p.Name] = openAPIParamSchema(p)
			if p.Required {
				required = append(required, p.Name)
			}
		}
		schema := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			schema["required"] = required
		}
		media["schema"] = schema
	} else {
		media["schema"] = openAPISchemaRef(route.RequestSchema)
		if route.RequestExample != nil {
			media["example"] = route.RequestExample
		}
	}
	return map[string]any{"required": true, "content": map[string]any{contentType: media}}
}

func openAPIResponses(route apiDocRoute) map[string]any {
	status := route.SuccessStatus
	if status == 0 {
		status = http.StatusOK
	}
	contentType := firstNonEmpty(route.ResponseContentType, "application/json")
	success := map[string]any{"description": http.StatusText(status)}
	if route.ResponseSchema != "" || route.ResponseExample != nil {
		media := map[string]any{"schema": openAPISchemaRef(route.ResponseSchema)}
		if route.ResponseExample != nil {
			media["example"] = route.ResponseExample
		}
		success["content"] = map[string]any{contentType: media}
	}
	return map[string]any{
		strconv.Itoa(status): success,
		"default": map[string]any{
			"description": "Error response",
			"content": map[string]any{"application/json": map[string]any{
				"schema":  openAPISchemaRef("ErrorResponse"),
				"example": map[string]any{"error": "method not allowed"},
			}},
		},
	}
}

func openAPISecurity(auth apiDocAuth) []map[string][]string {
	switch auth {
	case apiDocAuthProxy:
		return []map[string][]string{{"ProxyApiKey": {}}, {"ProxyAnthropicApiKey": {}}, {"ProxyGenericApiKey": {}}, {"ProxyBearer": {}}}
	case apiDocAuthAdmin:
		return []map[string][]string{{"AdminSessionCookie": {}}}
	default:
		return nil
	}
}

func openAPISecuritySchemes() map[string]any {
	return map[string]any{
		"ProxyApiKey": map[string]any{
			"type":        "apiKey",
			"in":          "header",
			"name":        "x-api-key",
			"description": "Local proxy key. Use {{proxyApiKey}} in Postman.",
		},
		"ProxyAnthropicApiKey": map[string]any{"type": "apiKey", "in": "header", "name": "anthropic-api-key"},
		"ProxyGenericApiKey":   map[string]any{"type": "apiKey", "in": "header", "name": "api-key"},
		"ProxyBearer":          map[string]any{"type": "http", "scheme": "bearer"},
		"AdminSessionCookie":   map[string]any{"type": "apiKey", "in": "cookie", "name": adminSessionCookieName},
	}
}

func openAPISchemaRef(name string) map[string]any {
	if name == "" {
		return map[string]any{"type": "object", "additionalProperties": true}
	}
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func schemaObject(properties map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func schemaOpenObject(properties map[string]any, required ...string) map[string]any {
	out := schemaObject(properties, required...)
	out["additionalProperties"] = true
	return out
}

func schemaArray(item any) map[string]any {
	return map[string]any{"type": "array", "items": item}
}

func schemaString(description string) map[string]any {
	out := map[string]any{"type": "string"}
	if description != "" {
		out["description"] = description
	}
	return out
}

func schemaInteger(description string) map[string]any {
	out := map[string]any{"type": "integer"}
	if description != "" {
		out["description"] = description
	}
	return out
}

func schemaBoolean(description string) map[string]any {
	out := map[string]any{"type": "boolean"}
	if description != "" {
		out["description"] = description
	}
	return out
}

func openAPISchemas() map[string]any {
	anyValue := map[string]any{"nullable": true}
	stringMap := map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}
	anyMap := map[string]any{"type": "object", "additionalProperties": true}
	message := schemaObject(map[string]any{"role": schemaString("Message role."), "content": anyValue}, "role")
	modelRow := schemaObject(map[string]any{"id": schemaString("Model id."), "type": schemaString("Object type."), "display_name": schemaString("Display name.")})
	modelCapabilityRow := schemaObject(map[string]any{
		"id":                    schemaString("Local model alias."),
		"type":                  schemaString("Object type."),
		"display_name":          schemaString("Display name."),
		"alias":                 schemaString("Local model alias."),
		"upstream_model":        schemaString("Resolved upstream model id."),
		"context":               schemaString("Configured context label."),
		"context_window_tokens": schemaInteger("Active context window advertised to clients."),
		"max_input_tokens":      schemaInteger("Maximum input budget after reserving output tokens."),
		"max_output_tokens":     schemaInteger("Maximum output tokens for one request."),
		"limits_known":          schemaBoolean("Whether the proxy has a known output limit for this upstream model."),
		"limits_source":         schemaString("Source for the limit value."),
		"status":                schemaString("Proxy support status for the upstream model."),
		"default":               schemaBoolean("Whether this alias is a default choice."),
		"recommended":           schemaBoolean("Whether this alias is the recommended choice."),
	}, "id", "type", "context_window_tokens", "max_input_tokens", "max_output_tokens", "limits_known")
	providerKey := schemaOpenObject(map[string]any{"id": schemaString("Provider key id."), "provider": schemaString("Provider name."), "schema": schemaString("Provider schema."), "label": schemaString("Label."), "base_url": schemaString("Provider base URL."), "key_preview": schemaString("Masked key preview."), "enabled": schemaBoolean("Whether the key is enabled."), "created_at": schemaString("Created timestamp."), "updated_at": schemaString("Updated timestamp.")})
	clientKey := schemaOpenObject(map[string]any{"id": schemaString("Client key id."), "label": schemaString("Label."), "key_preview": schemaString("Masked key preview."), "schema": schemaString("anthropic, openai, or both."), "provider": schemaString("Provider route."), "provider_key_id": schemaString("Provider key id."), "provider_label": schemaString("Provider label."), "enabled": schemaBoolean("Whether the key is enabled."), "created_at": schemaString("Created timestamp."), "updated_at": schemaString("Updated timestamp."), "last_used_at": schemaString("Last-used timestamp.")})
	modelAlias := schemaObject(map[string]any{"alias": schemaString("Local model alias."), "real": schemaString("Upstream model id."), "context": schemaString("Optional context label.")})
	return map[string]any{
		"HTMLDocument":                 map[string]any{"type": "string", "description": "HTML document."},
		"OpenAPIDocument":              map[string]any{"type": "object", "additionalProperties": true},
		"PostmanCollection":            map[string]any{"type": "object", "additionalProperties": true},
		"ErrorResponse":                schemaOpenObject(map[string]any{"error": anyValue, "type": schemaString("Anthropic-style error wrapper type.")}),
		"OKResponse":                   schemaOpenObject(map[string]any{"ok": schemaBoolean("Whether the operation succeeded."), "message": schemaString("Human-readable status message."), "running": schemaBoolean("Proxy running state.")}),
		"HealthResponse":               schemaObject(map[string]any{"ok": schemaBoolean("Process is reachable."), "proxy_running": schemaBoolean("Proxy endpoints are enabled.")}, "ok", "proxy_running"),
		"ModelListResponse":            schemaObject(map[string]any{"object": schemaString("list"), "data": schemaArray(modelRow)}, "object", "data"),
		"ModelCapabilitiesResponse":    schemaObject(map[string]any{"object": schemaString("list"), "data": schemaArray(modelCapabilityRow), "units": schemaString("Token unit label.")}, "object", "data", "units"),
		"AnthropicMessageRequest":      schemaOpenObject(map[string]any{"model": schemaString("Claude-compatible model alias."), "max_tokens": schemaInteger("Maximum output tokens."), "system": anyValue, "messages": schemaArray(message), "tools": schemaArray(anyMap), "temperature": map[string]any{"type": "number"}, "stream": schemaBoolean("Enable SSE streaming."), "speed": schemaString("Optional speed setting."), "output_config": anyMap}, "model", "messages"),
		"AnthropicMessageResponse":     schemaOpenObject(map[string]any{"id": schemaString("Message id."), "type": schemaString("message"), "role": schemaString("assistant"), "content": schemaArray(anyMap), "model": schemaString("Requested model alias."), "stop_reason": schemaString("Stop reason."), "stop_sequence": anyValue, "usage": anyMap}),
		"CountTokensResponse":          schemaObject(map[string]any{"input_tokens": schemaInteger("Approximate input token count.")}, "input_tokens"),
		"OpenAIChatCompletionRequest":  schemaOpenObject(map[string]any{"model": schemaString("OpenAI-compatible model id or local alias."), "messages": schemaArray(message), "tools": schemaArray(anyMap), "max_tokens": schemaInteger("Maximum output tokens."), "max_completion_tokens": schemaInteger("Maximum output tokens."), "temperature": map[string]any{"type": "number"}, "stream": schemaBoolean("Enable SSE streaming."), "reasoning_effort": schemaString("Optional reasoning effort."), "user": schemaString("Optional user/session id."), "metadata": anyMap, "extra_body": anyValue}, "model", "messages"),
		"OpenAIChatCompletionResponse": schemaOpenObject(map[string]any{"id": schemaString("Completion id."), "object": schemaString("chat.completion"), "created": schemaInteger("Unix timestamp."), "model": schemaString("Model id."), "choices": schemaArray(anyMap), "usage": anyMap}),
		"ResponsesRequest":             schemaOpenObject(map[string]any{"model": schemaString("OpenAI-compatible model id or local alias."), "instructions": schemaString("Optional system instructions."), "input": schemaArray(anyValue), "tools": schemaArray(anyMap), "reasoning": anyMap, "temperature": map[string]any{"type": "number"}, "max_output_tokens": schemaInteger("Maximum output tokens."), "service_tier": schemaString("Optional service tier."), "prompt_cache_key": schemaString("Optional prompt cache key."), "stream": schemaBoolean("Enable SSE streaming."), "store": schemaBoolean("Ignored locally and forced false.")}, "model", "input"),
		"ResponsesResponse":            schemaOpenObject(map[string]any{"id": schemaString("Response id."), "object": schemaString("response"), "model": schemaString("Model id."), "output": schemaArray(anyMap), "usage": anyMap}),
		"ProviderPassThroughResponse":  map[string]any{"type": "object", "additionalProperties": true, "description": "Provider-shaped OpenAI-compatible response."},
		"UIAuthStatusResponse":         schemaObject(map[string]any{"configured": schemaBoolean("Admin auth is configured."), "authenticated": schemaBoolean("Current request has a valid admin cookie."), "username": schemaString("Configured admin username.")}, "configured", "authenticated", "username"),
		"UIAuthCredentialsRequest":     schemaObject(map[string]any{"username": schemaString("Admin username."), "password": schemaString("Admin password."), "remember": schemaBoolean("Keep the session signed in for a year instead of 24 hours.")}, "username", "password"),
		"UIStatusResponse":             schemaOpenObject(map[string]any{"running": schemaBoolean("Proxy endpoints are running."), "proxy_running": schemaBoolean("Proxy endpoints are running."), "pid": schemaInteger("Process id."), "uptime_seconds": schemaInteger("Process uptime."), "version": schemaString("Installed proxy version."), "update": anyMap, "local_url": schemaString("Local control panel URL."), "public_url": schemaString("Public reverse-proxy URL when configured."), "display_url": schemaString("Preferred URL for the control panel to show."), "anthropic_url": schemaString("Anthropic-compatible base URL."), "openai_url": schemaString("OpenAI-compatible base URL."), "host": schemaString("Local bind host."), "port": schemaString("Local port."), "bind_url": schemaString("Local bind address."), "upstream": schemaString("Configured upstream."), "codex_auth": anyMap, "codex_sessions": anyMap, "claude_settings": anyMap, "antigravity": anyMap, "dashboard": anyMap, "models": schemaArray(anyMap), "last_request": anyMap, "claude_version": schemaString("Claude CLI version."), "proxy_key": schemaString("Current proxy key."), "proxy_key_masked": schemaString("Masked proxy key.")}),
		"UIUpdateStatusResponse":       schemaOpenObject(map[string]any{"current_version": schemaString("Installed proxy version."), "latest_version": schemaString("Latest version from GitHub."), "update_available": schemaBoolean("A newer version is available."), "auto_update": schemaBoolean("Automatic app updates are enabled."), "full_system_update": schemaBoolean("Manual dashboard updates may run the full system updater."), "state": schemaString("idle, queued, running, succeeded, or failed."), "phase": schemaString("Current update phase."), "message": schemaString("Human-readable update status."), "running": schemaBoolean("An update is currently active."), "updated_at": schemaString("Last status update time."), "started_at": schemaString("Update start time."), "finished_at": schemaString("Update finish time."), "repo_dir": schemaString("Repository directory used by the updater."), "branch": schemaString("Git branch used by the updater."), "version_url": schemaString("VERSION URL checked for newer releases."), "status_file": schemaString("Machine-readable status file path."), "latest": anyMap}),
		"UIUpdateStartResponse":        schemaOpenObject(map[string]any{"ok": schemaBoolean("Whether the update was queued."), "message": schemaString("Human-readable status."), "update": anyMap}),
		"UIUpdateSettingsRequest":      schemaOpenObject(map[string]any{"auto_update": schemaBoolean("Enable automatic app updates."), "full_system_update": schemaBoolean("Allow manual dashboard updates to run the full system updater."), "branch": schemaString("Git branch used by the updater."), "version_url": schemaString("VERSION URL checked for newer releases."), "repo_dir": schemaString("Repository directory used by the updater."), "status_file": schemaString("Machine-readable status file path.")}),
		"UIUpdateSettingsResponse":     schemaOpenObject(map[string]any{"ok": schemaBoolean("Whether settings were saved."), "auto_update": schemaBoolean("Automatic update setting."), "full_system_update": schemaBoolean("Full system update setting."), "update": anyMap}),
		"UIConfigResponse":             schemaObject(map[string]any{"config": stringMap, "secrets": stringMap, "aliases": schemaArray(modelAlias)}, "config", "secrets", "aliases"),
		"UIConfigRequest":              schemaObject(map[string]any{"config": stringMap, "aliases": schemaArray(schemaObject(map[string]any{"From": schemaString("Alias."), "To": schemaString("Model id."), "Context": schemaString("Context label.")}))}),
		"UIModelsResponse":             schemaOpenObject(map[string]any{"models": schemaArray(modelAlias), "ok": schemaBoolean("Whether save succeeded."), "message": schemaString("Status message.")}),
		"UIModelsRequest":              schemaObject(map[string]any{"models": schemaArray(modelAlias)}, "models"),
		"UIKeysResponse":               schemaOpenObject(map[string]any{"providers": schemaArray(providerKey), "clients": schemaArray(clientKey), "defaults": stringMap}),
		"UIProviderKeyRequest":         schemaObject(map[string]any{"id": schemaString("Existing provider key id when renewing."), "provider": schemaString("openai or gemini."), "label": schemaString("Label."), "base_url": schemaString("Provider base URL."), "api_key": schemaString("Provider API key.")}, "provider", "api_key"),
		"UIClientKeyRequest":           schemaObject(map[string]any{"label": schemaString("Client key label."), "schema": schemaString("both, anthropic, or openai."), "provider": schemaString("default, openai, or gemini."), "provider_key_id": schemaString("Provider key id.")}),
		"UIClientKeyCreateResponse":    schemaOpenObject(map[string]any{"ok": schemaBoolean("Whether creation succeeded."), "message": schemaString("Status message."), "api_key": schemaString("Raw local client API key, shown once."), "providers": schemaArray(providerKey), "clients": schemaArray(clientKey)}),
		"UIKeyToggleRequest":           schemaObject(map[string]any{"kind": schemaString("provider or client."), "id": schemaString("Key id."), "enabled": schemaBoolean("Desired enabled state.")}, "kind", "id", "enabled"),
		"UIValidateResponse":           schemaOpenObject(map[string]any{"ok": schemaBoolean("All validation steps passed."), "ran_at": schemaString("Local time."), "model": schemaString("Requested model alias."), "upstream_model": schemaString("Resolved upstream model."), "steps": schemaArray(anyMap), "duration_total": schemaInteger("Total duration in ms.")}),
		"UITestRequest":                schemaObject(map[string]any{"model": schemaString("Model alias."), "prompt": schemaString("Prompt text."), "stream": schemaBoolean("Use streaming.")}),
		"UITestResponse":               schemaObject(map[string]any{"status": schemaInteger("HTTP status from test request."), "duration_ms": schemaInteger("Duration in ms."), "raw": schemaString("Raw response body."), "text": schemaString("Extracted assistant text.")}),
		"UILogsResponse":               schemaObject(map[string]any{"rows": schemaArray(anyMap), "stdout": schemaString("Proxy stdout log."), "stderr": schemaString("Proxy stderr log."), "trace": schemaString("Trace log.")}),
		"AntigravityStatusResponse":    map[string]any{"type": "object", "additionalProperties": true},
		"AntigravityProbeRequest":      map[string]any{"type": "object", "additionalProperties": true},
		"AntigravityProbeResponse":     schemaObject(map[string]any{"ok": schemaBoolean("Probe accepted."), "probe": anyMap}, "ok", "probe"),
	}
}

func buildPostmanCollection(cfg config) map[string]any {
	routes := apiDocRoutes()
	grouped := map[string][]apiDocRoute{}
	for _, route := range routes {
		grouped[route.Group] = append(grouped[route.Group], route)
	}
	items := []map[string]any{}
	for _, group := range apiDocGroups(routes) {
		groupItems := []map[string]any{}
		for _, route := range grouped[group] {
			groupItems = append(groupItems, postmanItem(route))
		}
		items = append(items, map[string]any{"name": group, "item": groupItems})
	}
	return map[string]any{
		"info": map[string]any{
			"name":        "Connect AI Proxy API",
			"description": "Local Postman collection generated from the proxy API documentation manifest. Set variables before sending authenticated requests.",
			"schema":      "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
		},
		"variable": []map[string]any{
			{"key": "baseUrl", "value": apiDocsBaseURL(cfg)},
			{"key": "proxyApiKey", "value": ""},
			{"key": "anthropicVersion", "value": "2023-06-01"},
			{"key": "model", "value": "sonnet[1m]"},
			{"key": "sessionId", "value": "postman-local-session"},
			{"key": "adminUsername", "value": "admin"},
			{"key": "adminPassword", "value": ""},
			{"key": "adminSessionCookie", "value": ""},
			{"key": "providerApiKey", "value": ""},
			{"key": "clientKeyId", "value": ""},
			{"key": "path", "value": "file-abc123"},
		},
		"item": items,
	}
}

func postmanItem(route apiDocRoute) map[string]any {
	request := map[string]any{
		"method":      route.Method,
		"header":      postmanHeaders(route),
		"url":         postmanURL(route),
		"description": strings.TrimSpace(route.Description + "\n\nAuth: " + apiDocAuthLabel(route.Auth)),
	}
	if body := postmanBody(route); body != nil {
		request["body"] = body
	}
	return map[string]any{
		"name":    route.Method + " " + routeDisplayPath(route),
		"request": request,
	}
}

func postmanHeaders(route apiDocRoute) []map[string]any {
	seen := map[string]bool{}
	var headers []map[string]any
	add := func(key, value, description string, disabled bool) {
		lower := strings.ToLower(key)
		if seen[lower] {
			return
		}
		seen[lower] = true
		h := map[string]any{"key": key, "value": value}
		if description != "" {
			h["description"] = description
		}
		if disabled {
			h["disabled"] = true
		}
		headers = append(headers, h)
	}
	if route.Auth == apiDocAuthProxy {
		add("x-api-key", "{{proxyApiKey}}", "Accepted alternatives: anthropic-api-key, api-key, or Authorization: Bearer {{proxyApiKey}}.", false)
	}
	if route.Auth == apiDocAuthAdmin {
		add("Cookie", adminSessionCookieName+"={{adminSessionCookie}}", "Optional. Postman can also store this cookie after login.", true)
	}
	for _, p := range route.Headers {
		add(p.Name, fmt.Sprint(p.Example), p.Description, false)
	}
	if route.RequestExample != nil && route.RequestContentType == "" && route.Method != http.MethodGet {
		add("Content-Type", "application/json", "", false)
	}
	return headers
}

func postmanURL(route apiDocRoute) map[string]any {
	path := route.Path
	for _, p := range route.PathParams {
		path = strings.ReplaceAll(path, "{"+p.Name+"}", "{{"+p.Name+"}}")
	}
	raw := "{{baseUrl}}" + path
	query := []map[string]any{}
	if len(route.QueryParams) > 0 {
		parts := []string{}
		for _, p := range route.QueryParams {
			value := "{{" + p.Name + "}}"
			if p.Example != nil {
				value = fmt.Sprint(p.Example)
			}
			parts = append(parts, p.Name+"="+value)
			query = append(query, map[string]any{"key": p.Name, "value": value, "description": p.Description, "disabled": !p.Required})
		}
		raw += "?" + strings.Join(parts, "&")
	}
	out := map[string]any{
		"raw":  raw,
		"host": []string{"{{baseUrl}}"},
		"path": postmanPathSegments(path),
	}
	if len(query) > 0 {
		out["query"] = query
	}
	return out
}

func postmanPathSegments(path string) []string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func postmanBody(route apiDocRoute) map[string]any {
	if route.PostmanBodyMode == "formdata" {
		form := []map[string]any{}
		for _, p := range route.PostmanFormData {
			item := map[string]any{"key": p.Name, "type": "text", "description": p.Description}
			if p.Type == "file" {
				item["type"] = "file"
				item["src"] = []string{}
			} else if p.Example != nil {
				item["value"] = fmt.Sprint(p.Example)
			}
			form = append(form, item)
		}
		return map[string]any{"mode": "formdata", "formdata": form}
	}
	if route.RequestExample == nil || route.Method == http.MethodGet {
		return nil
	}
	return map[string]any{
		"mode": "raw",
		"raw":  docExample(route.RequestExample),
		"options": map[string]any{"raw": map[string]any{
			"language": "json",
		}},
	}
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
		row := uiLogRow{
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
		}
		appendUILog(row)
		if isDashboardTraffic(row) {
			recordMetric(requestMetric{
				AtUnixMS:     row.AtUnixMS,
				Status:       row.Status,
				DurMS:        row.DurMS,
				Model:        row.Model,
				Upstream:     row.Upstream,
				InputTokens:  row.InputTokens,
				OutputTokens: row.OutputTokens,
				Stream:       row.Stream,
				IsError:      row.Status >= 400,
				Surface:      surfaceFromPath(row.Path),
			})
		}
		requestProviderKeys.Delete(r)
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

// ───────────────────────────────────────────────────────────────────────────
// Request-metrics persistence (Analytics)
//
// One row per LLM request is recorded into a SEPARATE WAL SQLite db (a sibling
// of .proxy.db) so the Analytics page can aggregate usage / latency / tokens /
// errors over long periods. Writes are queued and flushed by a single
// background goroutine; the request path never blocks and never fails because
// of metrics. If the db can't be opened, metrics are simply disabled.
// ───────────────────────────────────────────────────────────────────────────

type requestMetric struct {
	AtUnixMS     int64
	Status       int
	DurMS        int64
	Model        string
	Upstream     string
	InputTokens  int
	OutputTokens int
	Stream       bool
	IsError      bool
	Surface      string // "anthropic" | "openai"
}

var (
	metricsDB *sql.DB
	metricsCh chan requestMetric
)

const metricsRetentionDays = 400

func metricsDBPath(cfg config) string {
	base := strings.TrimSpace(cfg.DBPath)
	if base == "" {
		base = ".proxy.db"
	}
	if strings.HasSuffix(strings.ToLower(base), ".db") {
		return base[:len(base)-3] + ".metrics.db"
	}
	return base + ".metrics.db"
}

// initMetricsDB opens the persistent metrics db (WAL), migrates it, and starts
// the async writer + hourly pruner. Best-effort: on any error metrics stay
// disabled (metricsDB == nil) and the proxy runs unaffected.
func initMetricsDB(cfg config) {
	db, err := sql.Open("sqlite", metricsDBPath(cfg))
	if err != nil {
		fmt.Printf("metrics: open failed (analytics disabled): %v\n", err)
		return
	}
	db.SetMaxOpenConns(4)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA synchronous=NORMAL"} {
		if _, err := db.Exec(pragma); err != nil {
			fmt.Printf("metrics: pragma %q failed: %v\n", pragma, err)
		}
	}
	if err := migrateMetricsDB(db); err != nil {
		fmt.Printf("metrics: migrate failed (analytics disabled): %v\n", err)
		_ = db.Close()
		return
	}
	metricsDB = db
	metricsCh = make(chan requestMetric, 2000)
	go metricsWriter()
	go metricsPruner()
}

func migrateMetricsDB(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS request_metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			at_unix_ms INTEGER NOT NULL,
			status INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			model TEXT NOT NULL DEFAULT '',
			upstream TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			stream INTEGER NOT NULL DEFAULT 0,
			is_error INTEGER NOT NULL DEFAULT 0,
			surface TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_request_metrics_at ON request_metrics(at_unix_ms)`,
		// Local cache of malware-scan verdicts (keyed by file SHA-256, with the
		// MD5 stored alongside) so a re-uploaded known file skips VirusTotal.
		`CREATE TABLE IF NOT EXISTS file_scan_cache (
			sha256 TEXT PRIMARY KEY,
			md5 TEXT NOT NULL DEFAULT '',
			malicious_engines INTEGER NOT NULL DEFAULT 0,
			scanned_at_ms INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// recordMetric queues a metric for async persistence. Non-blocking: drops the
// row (rather than stalling the request) if the buffer is full.
func recordMetric(m requestMetric) {
	// A successful proxied request verifies the alias (reflected as Available on
	// the Models page). Done regardless of the metrics channel state.
	if !m.IsError && m.Status >= 200 && m.Status < 400 {
		markModelAliasVerified(m.Model)
	}
	if metricsCh == nil {
		return
	}
	select {
	case metricsCh <- m:
	default:
		// buffer full — drop rather than block the request path
	}
}

func metricsWriter() {
	const flushEvery = 50
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	batch := make([]requestMetric, 0, flushEvery)
	flush := func() {
		if len(batch) == 0 || metricsDB == nil {
			batch = batch[:0]
			return
		}
		tx, err := metricsDB.Begin()
		if err != nil {
			batch = batch[:0]
			return
		}
		stmt, err := tx.Prepare(`INSERT INTO request_metrics
			(at_unix_ms,status,duration_ms,model,upstream,input_tokens,output_tokens,stream,is_error,surface)
			VALUES (?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			_ = tx.Rollback()
			batch = batch[:0]
			return
		}
		for _, m := range batch {
			_, _ = stmt.Exec(m.AtUnixMS, m.Status, m.DurMS, m.Model, m.Upstream, m.InputTokens, m.OutputTokens, boolToInt(m.Stream), boolToInt(m.IsError), m.Surface)
		}
		_ = stmt.Close()
		_ = tx.Commit()
		batch = batch[:0]
	}
	for {
		select {
		case m := <-metricsCh:
			batch = append(batch, m)
			if len(batch) >= flushEvery {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func metricsPruner() {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		<-timer.C
		if metricsDB != nil {
			cutoff := time.Now().Add(-metricsRetentionDays * 24 * time.Hour).UnixMilli()
			_, _ = metricsDB.Exec(`DELETE FROM request_metrics WHERE at_unix_ms < ?`, cutoff)
		}
		timer.Reset(time.Hour)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func surfaceFromPath(path string) string {
	if strings.HasPrefix(path, "/anthropic/") {
		return "anthropic"
	}
	if strings.HasPrefix(path, "/openai/") {
		return "openai"
	}
	return ""
}

// ─── Analytics aggregation ───

type analyticsBucket struct {
	T            int64 `json:"t"`
	Requests     int   `json:"requests"`
	AvgLatencyMs int64 `json:"avg_latency_ms"`
	TokensIn     int   `json:"tokens_in"`
	TokensOut    int   `json:"tokens_out"`
	Errors       int   `json:"errors"`
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// analyticsBucketMS picks a bucket size from the total span so charts stay
// readable (~13–45 bars): hourly / daily / weekly / 30-day.
func analyticsBucketMS(span int64) int64 {
	const h = int64(3600_000)
	const d = 24 * h
	switch {
	case span <= 36*h:
		return h
	case span <= 45*d:
		return d
	case span <= 200*d:
		return 7 * d
	default:
		return 30 * d
	}
}

// analyticsRange resolves a named period (or a custom from/to) into [start,end)
// unix-ms (server-local time) plus the bucket size.
func analyticsRange(period string, fromMS, toMS int64, now time.Time) (start, end, bucketMS int64) {
	loc := now.Location()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	end = now.UnixMilli()
	switch period {
	case "today":
		start = midnight.UnixMilli()
	case "this_week":
		wd := (int(midnight.Weekday()) + 6) % 7 // Monday = 0
		start = midnight.AddDate(0, 0, -wd).UnixMilli()
	case "this_month":
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).UnixMilli()
	case "last_month":
		firstThis := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
		start = firstThis.AddDate(0, -1, 0).UnixMilli()
		end = firstThis.UnixMilli()
	case "last_30_days":
		start = midnight.AddDate(0, 0, -29).UnixMilli()
	case "last_6_months":
		start = midnight.AddDate(0, -6, 0).UnixMilli()
	case "last_year":
		start = midnight.AddDate(-1, 0, 0).UnixMilli()
	case "custom":
		start, end = fromMS, toMS
		if end <= start {
			end = start + 1
		}
	default:
		start = midnight.UnixMilli()
	}
	bucketMS = analyticsBucketMS(end - start)
	return start, end, bucketMS
}

func handleUIAnalytics(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		q := r.URL.Query()
		period := strings.TrimSpace(q.Get("period"))
		if period == "" {
			period = "today"
		}
		start, end, bucketMS := analyticsRange(period, parseInt64(q.Get("from")), parseInt64(q.Get("to")), time.Now())

		emptyTotals := map[string]any{"requests": 0, "avg_latency_ms": 0, "tokens_in": 0, "tokens_out": 0, "tokens_total": 0, "errors": 0, "error_rate": 0.0}
		resp := map[string]any{
			"period":    period,
			"from":      start,
			"to":        end,
			"bucket_ms": bucketMS,
			"buckets":   []analyticsBucket{},
			"totals":    emptyTotals,
			"available": metricsDB != nil,
		}
		if metricsDB == nil || bucketMS <= 0 || end <= start {
			writeJSON(w, http.StatusOK, resp)
			return
		}

		nBuckets := int((end - start + bucketMS - 1) / bucketMS)
		if nBuckets < 1 {
			nBuckets = 1
		}
		if nBuckets > 2000 {
			nBuckets = 2000
		}
		buckets := make([]analyticsBucket, nBuckets)
		for i := range buckets {
			buckets[i].T = start + int64(i)*bucketMS
		}

		rows, err := metricsDB.Query(
			`SELECT (at_unix_ms - ?) / ? AS b,
			        COUNT(*), COALESCE(SUM(duration_ms),0),
			        COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			        COALESCE(SUM(is_error),0)
			 FROM request_metrics
			 WHERE at_unix_ms >= ? AND at_unix_ms < ?
			 GROUP BY b ORDER BY b`,
			start, bucketMS, start, end)
		if err != nil {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		defer rows.Close()

		var totReq, totIn, totOut, totErr int
		var totDur int64
		for rows.Next() {
			var b, durSum int64
			var cnt, inTok, outTok, errCnt int
			if err := rows.Scan(&b, &cnt, &durSum, &inTok, &outTok, &errCnt); err != nil {
				continue
			}
			if b >= 0 && int(b) < nBuckets {
				bk := &buckets[b]
				bk.Requests = cnt
				bk.TokensIn = inTok
				bk.TokensOut = outTok
				bk.Errors = errCnt
				if cnt > 0 {
					bk.AvgLatencyMs = durSum / int64(cnt)
				}
			}
			totReq += cnt
			totIn += inTok
			totOut += outTok
			totErr += errCnt
			totDur += durSum
		}

		avgLat := int64(0)
		errRate := 0.0
		if totReq > 0 {
			avgLat = totDur / int64(totReq)
			errRate = float64(int(float64(totErr)/float64(totReq)*1000+0.5)) / 10 // 1 decimal %
		}
		resp["buckets"] = buckets
		resp["totals"] = map[string]any{
			"requests":       totReq,
			"avg_latency_ms": avgLat,
			"tokens_in":      totIn,
			"tokens_out":     totOut,
			"tokens_total":   totIn + totOut,
			"errors":         totErr,
			"error_rate":     errRate,
		}
		writeJSON(w, http.StatusOK, resp)
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
		"prompt_cache_key":   out.PromptCacheKey,
		"temperature":        out.Temperature,
	}
}

func summarizeCodexSession(info codexSessionInfo) map[string]any {
	return map[string]any{
		"enabled":          info.Enabled,
		"flow_hash":        info.FlowHash,
		"flow_source":      info.FlowSource,
		"registry_key":     info.RegistryKey,
		"codex_session_id": info.CodexSessionID,
		"prompt_cache_key": info.PromptCacheKey,
		"side_thread":      info.SideThread,
		"side_thread_kind": info.SideThreadKind,
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
		"user_set":         strings.TrimSpace(out.User) != "",
		"extra_body_set":   out.ExtraBody != nil,
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
			"status":           item.Status,
			"action":           safeToolActivityInput(item.Action),
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
		"stream_error":  truncateString(resp.StreamError, 1000),
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
	if msg := responsesStreamEventErrorMessage(event); msg != "" {
		out["error"] = truncateString(msg, 1000)
	}
	if event.Item.Type != "" {
		out["item"] = map[string]any{
			"type":            event.Item.Type,
			"id":              event.Item.ID,
			"call_id":         event.Item.CallID,
			"name":            event.Item.Name,
			"status":          event.Item.Status,
			"action":          safeToolActivityInput(event.Item.Action),
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

// uiSPARoutes is the allowlist of client-side routes the SPA owns. Each one
// (with or without a single trailing slash) serves the SPA HTML so deep links
// and reloads work. Anything else under "/" is a 404.
var uiSPARoutes = map[string]bool{
	"/":           true,
	"/dashboard":  true,
	"/analytics":  true,
	"/config":     true,
	"/models":     true,
	"/virustotal": true,
	"/images":     true,
	"/keys":       true,
	"/test":       true,
	"/logs":       true,
	"/updates":    true,
	"/browser":    true,
	"/setup":      true,
}

// uiIndexFile returns the SPA entry point: the new ui/index.html when present,
// otherwise the legacy filename (deploy-order safety so an out-of-sync rollout
// still serves a panel).
func uiIndexFile(uiDir string) string {
	index := filepath.Join(uiDir, "index.html")
	if _, err := os.Stat(index); err == nil {
		return index
	}
	return filepath.Join(uiDir, "Connect AI Proxy Control Panel.html")
}

func handleUI(w http.ResponseWriter, r *http.Request) {
	uiDir := filepath.Join(".", "ui")
	if strings.HasPrefix(r.URL.Path, "/ui/") {
		w.Header().Set("Cache-Control", "no-cache")
		http.StripPrefix("/ui/", http.FileServer(http.Dir(uiDir))).ServeHTTP(w, r)
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	if uiSPARoutes[path] {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, uiIndexFile(uiDir))
		return
	}
	http.NotFound(w, r)
}

func handleAntigravityBridge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, antigravityBridgeHTML())
}

func handleUIAntigravityStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, antigravityStatus())
}

func handleUIAntigravityProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	probe := sanitizeAntigravityProbe(body)
	antigravityMu.Lock()
	antigravityLastProbe = probe
	antigravityMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "probe": probe})
}

func antigravityStatus() map[string]any {
	extensionPath := antigravityExtensionPath()
	manifest := antigravityManifestInfo(extensionPath)
	profilePath := antigravityProfilePath()
	chromePath := chromeExecutablePath()
	launcherPath := preferredProxyBinaryPath()
	bridgeURL := "http://127.0.0.1:" + getenv("PROXY_PORT", getenv("LITELLM_PORT", "4000")) + "/antigravity/bridge"
	browserMode := antigravityBrowserMode()
	debugPort := antigravityBrowserDebugPort()
	browserURL := "http://127.0.0.1:" + debugPort
	bridgeState := readAntigravityBrowserState()
	debugRunning := antigravityBrowserEndpointRunning(browserURL)
	bridgeState["connected_now"] = debugRunning
	if !debugRunning {
		bridgeState["mode"] = "idle"
		bridgeState["connected"] = false
		bridgeState["last_action"] = "idle; browser opens on first action"
	}
	chromeProcesses := chromeProcesses()
	visibleWindows := visibleChromeWindowsFrom(chromeProcesses)
	visibleTitles := make([]string, 0, len(visibleWindows))
	for _, proc := range visibleWindows {
		if strings.TrimSpace(proc.Title) != "" {
			visibleTitles = append(visibleTitles, truncateString(proc.Title, 160))
		}
	}

	antigravityMu.Lock()
	lastProbe := cloneMap(antigravityLastProbe)
	antigravityMu.Unlock()

	return map[string]any{
		"extension": map[string]any{
			"id":              antigravityExtensionID,
			"path":            extensionPath,
			"exists":          extensionPath != "",
			"manifest":        manifest,
			"manifest_exists": extensionPath != "" && fileExists(filepath.Join(extensionPath, "manifest.json")),
		},
		"profile": map[string]any{
			"path":   profilePath,
			"exists": fileExists(profilePath),
		},
		"chrome": map[string]any{
			"path":                 chromePath,
			"exists":               chromePath != "",
			"mode":                 browserMode,
			"visibility_supported": runtime.GOOS == "windows",
			"startup_enabled":      envFlag("ANTIGRAVITY_BROWSER_PRELAUNCH_WITH_PROXY", false),
			"browser_url":          browserURL,
			"debug_port":           debugPort,
			"debug_running":        debugRunning,
			"process_count":        len(chromeProcesses),
			"visible_count":        len(visibleWindows),
			"visible_titles":       visibleTitles,
			"can_relaunch_default": canRelaunchDefaultChromeFrom(chromeProcesses),
			"default_cdp_forced":   defaultChromeCDPForced(),
			"default_cdp_note":     defaultChromeCDPNote(),
		},
		"launcher": map[string]any{
			"path":    launcherPath,
			"command": commandDisplay(launcherPath, "browser-mcp"),
			"exists":  fileExists(launcherPath),
		},
		"mcp":             antigravityMCPSettings(),
		"bridge_url":      bridgeURL,
		"last_probe":      lastProbe,
		"bridge_state":    bridgeState,
		"visible_overlay": true,
		"ready":           extensionPath != "" && chromePath != "" && fileExists(launcherPath),
		"mode":            "visible_overlay_cdp",
		"extension_id":    antigravityExtensionID,
	}
}

func antigravityBrowserMode() string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("ANTIGRAVITY_BROWSER_MODE")))
	if mode == "" && envFlag("ANTIGRAVITY_USE_DEFAULT_BROWSER", false) {
		return "default"
	}
	switch mode {
	case "default", "existing", "normal":
		return "default"
	default:
		return "dedicated"
	}
}

func antigravityBrowserDebugPort() string {
	port := strings.TrimSpace(os.Getenv("ANTIGRAVITY_BROWSER_DEBUG_PORT"))
	if port == "" {
		return "9233"
	}
	if n, err := strconv.Atoi(port); err == nil && n > 0 && n < 65536 {
		return port
	}
	return "9233"
}

func antigravityBrowserEndpointRunning(browserURL string) bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimRight(browserURL, "/") + "/json/version")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func antigravityExtensionPath() string {
	return findChromeExtensionPath(antigravityExtensionID)
}

func antigravityProfilePath() string {
	if path := strings.TrimSpace(os.Getenv("ANTIGRAVITY_BROWSER_PROFILE")); path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	}
	return filepath.Join(mustGetwd(), ".antigravity-browser-profile")
}

func antigravityManifestInfo(extensionPath string) map[string]any {
	info := map[string]any{
		"name":                   "",
		"version":                "",
		"service_worker":         "",
		"externally_connectable": false,
		"content_script_count":   0,
	}
	if extensionPath == "" {
		return info
	}
	raw, err := os.ReadFile(filepath.Join(extensionPath, "manifest.json"))
	if err != nil {
		return info
	}
	var manifest struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		Background struct {
			ServiceWorker string `json:"service_worker"`
		} `json:"background"`
		ExternallyConnectable struct {
			Matches []string `json:"matches"`
		} `json:"externally_connectable"`
		ContentScripts []struct {
			JS []string `json:"js"`
		} `json:"content_scripts"`
	}
	if json.Unmarshal(raw, &manifest) != nil {
		return info
	}
	info["name"] = manifest.Name
	info["version"] = manifest.Version
	info["service_worker"] = manifest.Background.ServiceWorker
	info["externally_connectable"] = len(manifest.ExternallyConnectable.Matches) > 0
	info["content_script_count"] = len(manifest.ContentScripts)
	return info
}

func antigravityMCPSettings() map[string]any {
	settingsPath := defaultClaudeRootConfigPath()
	out := map[string]any{
		"name":     "antigravity-browser",
		"path":     settingsPath,
		"present":  false,
		"command":  "",
		"args":     []string{},
		"launcher": "",
		"desktop":  antigravityDesktopMCPSettings(),
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return out
	}
	var settings struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &settings) != nil {
		return out
	}
	server, ok := settings.MCPServers["antigravity-browser"]
	if !ok {
		return out
	}
	out["present"] = true
	out["type"] = server.Type
	out["command"] = server.Command
	out["args"] = server.Args
	out["env_keys"] = sortedMapKeys(server.Env)
	for i, arg := range server.Args {
		if strings.EqualFold(arg, "-File") && i+1 < len(server.Args) {
			out["launcher"] = server.Args[i+1]
			break
		}
	}
	if out["launcher"] == "" && len(server.Args) > 0 && server.Args[0] == "browser-mcp" {
		out["launcher"] = server.Command
	}
	return out
}

func antigravityDesktopMCPSettings() map[string]any {
	out := map[string]any{
		"name":         "antigravity-browser",
		"path":         "",
		"paths":        []string{},
		"exists":       false,
		"present":      false,
		"command":      "",
		"args":         []string{},
		"launcher":     "",
		"server_count": 0,
		"servers":      []string{},
	}
	type desktopServer struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
	}
	mergedServers := map[string]desktopServer{}
	configPaths := claudeDesktopConfigPaths()
	out["paths"] = configPaths
	for _, configPath := range configPaths {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}
		if out["path"] == "" {
			out["path"] = configPath
		}
		out["exists"] = true
		var settings struct {
			MCPServers map[string]desktopServer `json:"mcpServers"`
		}
		if json.Unmarshal(raw, &settings) != nil {
			continue
		}
		for name, server := range settings.MCPServers {
			if _, ok := mergedServers[name]; !ok {
				mergedServers[name] = server
			}
		}
	}
	servers := make([]string, 0, len(mergedServers))
	for name := range mergedServers {
		servers = append(servers, name)
	}
	sort.Strings(servers)
	out["server_count"] = len(servers)
	out["servers"] = servers
	server, ok := mergedServers["antigravity-browser"]
	if !ok {
		return out
	}
	out["present"] = true
	out["command"] = server.Command
	out["args"] = server.Args
	out["env_keys"] = sortedMapKeys(server.Env)
	for i, arg := range server.Args {
		if strings.EqualFold(arg, "-File") && i+1 < len(server.Args) {
			out["launcher"] = server.Args[i+1]
			break
		}
	}
	if out["launcher"] == "" && len(server.Args) > 0 && server.Args[0] == "browser-mcp" {
		out["launcher"] = server.Command
	}
	return out
}

func claudeDesktopConfigPaths() []string {
	targets := claudeDesktopConfigTargets()
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, target.Path)
	}
	return out
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func chromeExecutablePath() string {
	return findChromeExecutablePath()
}

func executablePath(names ...string) string {
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err == nil && path != "" {
			return path
		}
	}
	return ""
}

func sanitizeAntigravityProbe(body map[string]any) map[string]any {
	out := map[string]any{
		"received_at": time.Now().Format(time.RFC3339),
	}
	for _, key := range []string{"ok", "runtime_available", "sent_at"} {
		if value, ok := body[key]; ok {
			out[key] = sanitizeProbeScalar(value)
		}
	}
	for _, key := range []string{"wake", "connection"} {
		if value, ok := body[key]; ok {
			out[key] = sanitizeProbeObject(value)
		}
	}
	if value, ok := body["user_agent"]; ok {
		out["user_agent"] = truncateString(fmt.Sprint(value), 180)
	}
	if value, ok := body["error"]; ok {
		out["error"] = truncateString(fmt.Sprint(value), 500)
	}
	return out
}

func sanitizeProbeObject(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range v {
			if strings.Contains(strings.ToLower(key), "token") || strings.Contains(strings.ToLower(key), "secret") {
				continue
			}
			out[key] = sanitizeProbeScalar(item)
		}
		return out
	default:
		return sanitizeProbeScalar(value)
	}
}

func sanitizeProbeScalar(value any) any {
	switch v := value.(type) {
	case string:
		return truncateString(v, 500)
	case bool, float64, int, int64, nil:
		return v
	default:
		raw, _ := json.Marshal(v)
		return truncateString(string(raw), 500)
	}
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func antigravityBridgeHTML() string {
	return strings.ReplaceAll(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Antigravity Bridge Probe</title>
  <style>
    body { margin: 0; font-family: Inter, system-ui, -apple-system, Segoe UI, sans-serif; background: #101418; color: #eef3f8; }
    main { max-width: 860px; margin: 0 auto; padding: 32px 20px; }
    h1 { font-size: 22px; margin: 0 0 8px; font-weight: 600; }
    p { color: #9aa8b5; line-height: 1.5; }
    .panel { border: 1px solid #27313a; background: #161c22; border-radius: 8px; padding: 16px; margin-top: 16px; }
    .row { display: grid; grid-template-columns: 180px 1fr; gap: 10px; border-top: 1px solid #27313a; padding: 10px 0; }
    .row:first-child { border-top: 0; }
    .k { color: #9aa8b5; font-size: 12px; text-transform: uppercase; letter-spacing: .04em; }
    .v { font-family: "JetBrains Mono", Consolas, monospace; font-size: 12px; white-space: pre-wrap; word-break: break-word; }
    .ok { color: #6be49c; }
    .bad { color: #ff8585; }
    button { border: 1px solid #3a4652; background: #202832; color: #eef3f8; border-radius: 6px; padding: 8px 10px; cursor: pointer; }
  </style>
</head>
<body>
  <main>
    <h1>Antigravity Browser Bridge Probe</h1>
    <p>This local page checks whether Chrome exposes the Antigravity extension messaging bridge to Claude Code's dedicated browser profile.</p>
    <button id="run">Run probe</button>
    <div class="panel" id="out"></div>
  </main>
  <script>
    const extensionId = "__EXTENSION_ID__";
    const out = document.getElementById("out");
    const rows = (data) => Object.keys(data).map((key) => '<div class="row"><div class="k">' + key + '</div><div class="v">' + escapeHtml(JSON.stringify(data[key], null, 2)) + '</div></div>').join("");
    const escapeHtml = (s) => String(s).replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c]));
    const send = (payload) => new Promise((resolve) => {
      if (!window.chrome || !chrome.runtime || !chrome.runtime.sendMessage) {
        resolve({ ok: false, error: "chrome.runtime.sendMessage is not available on this page" });
        return;
      }
      try {
        chrome.runtime.sendMessage(extensionId, payload, (response) => {
          const last = chrome.runtime.lastError ? chrome.runtime.lastError.message : "";
          resolve({ ok: !last, error: last, response: response || null });
        });
      } catch (err) {
        resolve({ ok: false, error: err && err.message ? err.message : String(err) });
      }
    });
    async function runProbe() {
      const result = {
        sent_at: new Date().toISOString(),
        ok: false,
        runtime_available: !!(window.chrome && chrome.runtime && chrome.runtime.sendMessage),
        user_agent: navigator.userAgent
      };
      result.wake = await send({ action: "serviceWorkerWakeUp", timestamp: Date.now() });
      result.connection = await send({ type: "CHECK_JETSKI_CONNECTION" });
      result.ok = !!(result.wake && result.wake.ok);
      out.innerHTML = rows(result);
      try {
        await fetch("/ui/api/antigravity/probe", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(result)
        });
      } catch (err) {
        result.report_error = err && err.message ? err.message : String(err);
        out.innerHTML = rows(result);
      }
    }
    document.getElementById("run").addEventListener("click", runProbe);
    runProbe();
  </script>
</body>
</html>`, "__EXTENSION_ID__", antigravityExtensionID)
}

func handleUIStatus(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		auth := codexAuthMetadata(cfg)
		claudeVersion := commandOutput(2*time.Second, "claude", "--version")
		localURL := localProxyBaseURL(cfg)
		displayURL := clientFacingBaseURL(cfg)
		desktopTargets := claudeDesktopConfigTargets()
		desktopPaths := make([]string, 0, len(desktopTargets))
		for _, target := range desktopTargets {
			desktopPaths = append(desktopPaths, target.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"running":              proxyEnabled.Load(),
			"proxy_running":        proxyEnabled.Load(),
			"pid":                  os.Getpid(),
			"version":              appVersion(),
			"platform":             platformName(),
			"goos":                 runtime.GOOS,
			"goarch":               runtime.GOARCH,
			"binary_path":          preferredProxyBinaryPath(),
			"launcher_commands":    launcherCommands(cfg),
			"desktop_supported":    desktopSupportedOnCurrentPlatform(),
			"desktop_config_paths": desktopPaths,
			"browser":              map[string]any{"visibility_supported": runtime.GOOS == "windows"},
			"uptime_seconds":       int(time.Since(startedAt).Seconds()),
			"local_url":            localURL,
			"public_url":           cfg.PublicURL,
			"display_url":          displayURL,
			"anthropic_url":        displayURL + "/anthropic",
			"openai_url":           displayURL + "/openai/v1",
			"host":                 cfg.Host,
			"port":                 cfg.Port,
			"bind_url":             cfg.Host + ":" + cfg.Port,
			"upstream":             cfg.Upstream,
			"codex_auth":           auth,
			"codex_sessions":       codexSessionMetadata(cfg),
			"claude_settings":      claudeSettingsMetadata(cfg),
			"antigravity":          antigravityStatus(),
			"dashboard":            dashboardMetrics(),
			"update":               updateStatusPayload(cfg),
			"models":               modelRows(cfg),
			"last_request":         lastRequest(),
			"claude_version":       strings.TrimSpace(claudeVersion),
			"proxy_key":            cfg.ProxyKey,
			"proxy_key_masked":     maskSecret(cfg.ProxyKey),
		})
	}
}

func codexSessionMetadata(cfg config) map[string]any {
	path := codexSessionFilePath(cfg)
	info := map[string]any{
		"enabled":                  codexSessionIsolationEnabled(),
		"prompt_cache_key_enabled": codexPromptCacheKeyEnabled(),
		"path":                     path,
		"exists":                   false,
		"count":                    0,
		"updated_at":               "",
	}
	registry := readCodexSessionRegistry(path)
	if registry.Version == 0 {
		return info
	}
	info["exists"] = true
	info["count"] = len(registry.Sessions)
	info["updated_at"] = registry.UpdatedAt
	sideThreads := 0
	for _, record := range registry.Sessions {
		if record.SideThread {
			sideThreads++
		}
	}
	info["side_thread_count"] = sideThreads
	return info
}

func claudeSettingsMetadata(cfg config) map[string]any {
	settingsPath := defaultClaudeSettingsPath()
	cachePath := defaultClaudeGatewayModelsCachePath()
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

func appVersion() string {
	if v := strings.TrimSpace(os.Getenv("PROXY_VERSION")); v != "" {
		return v
	}
	for _, path := range candidateVersionFiles() {
		raw, err := os.ReadFile(path)
		if err == nil {
			if v := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0]); v != "" {
				return v
			}
		}
	}
	return appVersionFallback
}

func candidateVersionFiles() []string {
	paths := []string{filepath.Join(".", "VERSION")}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "VERSION"))
	}
	if wd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(wd, "VERSION"))
	}
	return paths
}

func updateStatusFilePath() string {
	if v := strings.TrimSpace(os.Getenv("PROXY_UPDATE_STATUS_FILE")); v != "" {
		return v
	}
	return filepath.Join(".", ".proxy.update.json")
}

func updateRepoDir() string {
	if v := strings.TrimSpace(os.Getenv("PROXY_UPDATE_REPO_DIR")); v != "" {
		return v
	}
	if wd, err := os.Getwd(); err == nil && fileExists(filepath.Join(wd, ".git")) {
		return wd
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "claude-code-proxy")
	}
	return "."
}

func updateBranch() string {
	if v := strings.TrimSpace(os.Getenv("PROXY_UPDATE_BRANCH")); v != "" {
		return v
	}
	return "main"
}

func autoUpdateEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(firstNonEmpty(os.Getenv("PROXY_AUTO_UPDATE"), readEnvMap()["PROXY_AUTO_UPDATE"])))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func dashboardFullSystemUpdateEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(firstNonEmpty(os.Getenv("PROXY_DASHBOARD_FULL_SYSTEM_UPDATE"), readEnvMap()["PROXY_DASHBOARD_FULL_SYSTEM_UPDATE"])))
	return v == "" || v == "1" || v == "true" || v == "yes" || v == "on"
}

func updateVersionURL() string {
	return firstNonEmpty(os.Getenv("PROXY_UPDATE_VERSION_URL"), readEnvMap()["PROXY_UPDATE_VERSION_URL"], defaultVersionURL)
}

func updateStatusPayload(cfg config) map[string]any {
	maybeRefreshLatestVersion()
	state := readUpdateState()
	latest := latestVersionInfo()
	current := appVersion()
	latestVersion, _ := latest["latest_version"].(string)
	if latestVersion == "" {
		if v, ok := latest["version"].(string); ok {
			latestVersion = v
		}
	}
	updateAvailable := latestVersion != "" && versionCompare(latestVersion, current) > 0
	state["current_version"] = current
	state["latest_version"] = latestVersion
	state["update_available"] = updateAvailable
	state["auto_update"] = autoUpdateEnabled()
	state["full_system_update"] = dashboardFullSystemUpdateEnabled()
	state["repo_dir"] = updateRepoDir()
	state["branch"] = updateBranch()
	state["version_url"] = updateVersionURL()
	state["status_file"] = updateStatusFilePath()
	state["latest"] = latest
	state["public_url"] = cfg.PublicURL
	return state
}

func readUpdateState() map[string]any {
	state := map[string]any{
		"state":      "idle",
		"phase":      "",
		"message":    "",
		"running":    false,
		"updated_at": "",
	}
	raw, err := os.ReadFile(updateStatusFilePath())
	if err != nil {
		return state
	}
	var loaded map[string]any
	if err := json.Unmarshal(raw, &loaded); err != nil {
		state["state"] = "unknown"
		state["message"] = "Update status file is unreadable."
		return state
	}
	for k, v := range loaded {
		state[k] = v
	}
	return state
}

func writeUpdateState(state, phase, message, source string) {
	path := updateStatusFilePath()
	current := readUpdateState()
	now := time.Now().UTC().Format(time.RFC3339)
	if state == "queued" || state == "running" {
		if fmt.Sprint(current["started_at"]) == "" {
			current["started_at"] = now
		}
	}
	if state == "succeeded" || state == "failed" {
		current["finished_at"] = now
	}
	current["state"] = state
	current["phase"] = phase
	current["message"] = message
	current["source"] = source
	current["version"] = appVersion()
	current["updated_at"] = now
	current["running"] = state == "queued" || state == "running"
	writeJSONFileAtomic(path, current, 0600)
}

func writeJSONFileAtomic(path string, value any, mode os.FileMode) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0700)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, mode); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func latestVersionInfo() map[string]any {
	updateMu.Lock()
	defer updateMu.Unlock()
	out := map[string]any{
		"latest_version": latestVersionCache.Version,
		"checked_at":     "",
		"error":          latestVersionCache.Error,
	}
	if !latestVersionCache.CheckedAt.IsZero() {
		out["checked_at"] = latestVersionCache.CheckedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func maybeRefreshLatestVersion() {
	updateMu.Lock()
	stale := latestVersionCache.CheckedAt.IsZero() || time.Since(latestVersionCache.CheckedAt) > 10*time.Minute
	updateMu.Unlock()
	if stale {
		go refreshLatestVersion()
	}
}

func refreshLatestVersion() {
	url := updateVersionURL()
	client := http.Client{Timeout: 6 * time.Second}
	resp, err := client.Get(url)
	version := ""
	errMsg := ""
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
			if readErr == nil {
				version = strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
			} else {
				errMsg = readErr.Error()
			}
		} else {
			errMsg = fmt.Sprintf("version check returned HTTP %d", resp.StatusCode)
		}
	} else {
		errMsg = err.Error()
	}
	updateMu.Lock()
	if version != "" {
		latestVersionCache.Version = version
		latestVersionCache.Error = ""
	} else {
		latestVersionCache.Error = errMsg
	}
	latestVersionCache.CheckedAt = time.Now()
	updateMu.Unlock()
}

func versionCompare(a, b string) int {
	aa := versionParts(a)
	bb := versionParts(b)
	maxLen := len(aa)
	if len(bb) > maxLen {
		maxLen = len(bb)
	}
	for i := 0; i < maxLen; i++ {
		av, bv := 0, 0
		if i < len(aa) {
			av = aa[i]
		}
		if i < len(bb) {
			bv = bb[i]
		}
		if av > bv {
			return 1
		}
		if av < bv {
			return -1
		}
	}
	return 0
}

func versionParts(version string) []int {
	version = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(version)), "v")
	version = strings.NewReplacer("-", ".", "_", ".", "+", ".").Replace(version)
	parts := []int{}
	for _, part := range strings.Split(version, ".") {
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		parts = append(parts, n)
	}
	return parts
}

func startAutoUpdateWatcher(cfg config) {
	go func() {
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		for {
			<-timer.C
			refreshLatestVersion()
			if autoUpdateEnabled() {
				status := updateStatusPayload(cfg)
				if available, _ := status["update_available"].(bool); available {
					_ = startDashboardUpdate(cfg, "auto")
				}
			}
			timer.Reset(30 * time.Minute)
		}
	}()
}

func handleUIUpdateStatus(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, updateStatusPayload(cfg))
	}
}

func handleUIUpdateStart(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if err := startDashboardUpdate(cfg, "manual"); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error(), "update": updateStatusPayload(cfg)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Update started.", "update": updateStatusPayload(cfg)})
	}
}

func handleUIUpdateCheck(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		refreshLatestVersion()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Update check complete.", "update": updateStatusPayload(cfg)})
	}
}

func handleUIUpdateSettings(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			AutoUpdate       *bool   `json:"auto_update"`
			FullSystemUpdate *bool   `json:"full_system_update"`
			Branch           *string `json:"branch"`
			VersionURL       *string `json:"version_url"`
			RepoDir          *string `json:"repo_dir"`
			StatusFile       *string `json:"status_file"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		current := readEnvMap()
		changed := []string{}
		setBool := func(key string, value *bool) {
			if value == nil {
				return
			}
			if *value {
				current[key] = "1"
			} else {
				current[key] = "0"
			}
			changed = append(changed, key)
		}
		setString := func(key string, value *string) {
			if value == nil {
				return
			}
			current[key] = strings.TrimSpace(*value)
			changed = append(changed, key)
		}
		setBool("PROXY_AUTO_UPDATE", body.AutoUpdate)
		setBool("PROXY_DASHBOARD_FULL_SYSTEM_UPDATE", body.FullSystemUpdate)
		setString("PROXY_UPDATE_BRANCH", body.Branch)
		setString("PROXY_UPDATE_VERSION_URL", body.VersionURL)
		setString("PROXY_UPDATE_REPO_DIR", body.RepoDir)
		setString("PROXY_UPDATE_STATUS_FILE", body.StatusFile)
		if err := writeEnvMap(current); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		syncProcessEnvKeys(current, changed...)
		if body.VersionURL != nil {
			refreshLatestVersion()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                 true,
			"auto_update":        autoUpdateEnabled(),
			"full_system_update": dashboardFullSystemUpdateEnabled(),
			"update":             updateStatusPayload(cfg),
		})
	}
}

func startDashboardUpdate(cfg config, source string) error {
	updateMu.Lock()
	state := readUpdateState()
	if running, _ := state["running"].(bool); running {
		updateMu.Unlock()
		return errors.New("update already in progress")
	}
	writeUpdateState("queued", "queued", "Update queued.", source)
	updateMu.Unlock()

	repoDir := updateRepoDir()
	scriptPath := filepath.Join(repoDir, "update-linux.sh")
	if !fileExists(scriptPath) {
		writeUpdateState("failed", "missing-script", "Cannot find update-linux.sh. Check PROXY_UPDATE_REPO_DIR.", source)
		return fmt.Errorf("cannot find update-linux.sh at %s", scriptPath)
	}
	statusFile := updateStatusFilePath()
	branch := updateBranch()
	cmdText := fmt.Sprintf("cd %s && chmod +x update-linux.sh install-linux.sh && ./update-linux.sh -y --branch %s --status-file %s", shellQuote(repoDir), shellQuote(branch), shellQuote(statusFile))
	if source == "auto" || !dashboardFullSystemUpdateEnabled() {
		cmdText += " --no-system-upgrade --skip-go-latest"
	}
	if runtime.GOOS == "linux" {
		if systemdRunPath, err := exec.LookPath("systemd-run"); err == nil {
			unit := "connect-ai-proxy-dashboard-update"
			cmd := exec.Command(systemdRunPath, "--unit", unit, "--collect", "bash", "-lc", cmdText)
			cmd.Stdout = io.Discard
			cmd.Stderr = io.Discard
			if err := cmd.Start(); err == nil {
				return nil
			}
		}
	}
	cmd := exec.Command("sh", "-c", "nohup sh -c "+shellQuote(cmdText)+" >/dev/null 2>&1 &")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Start()
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
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"models": modelRows(cfg)})
		case http.MethodPost:
			var body struct {
				Models []modelAliasConfig `json:"models"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			current := readEnvMap()
			next, err := saveModelAliasesToEnvMap(current, body.Models)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			if err := writeEnvMap(current); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			syncProcessEnvKeys(current, "PROXY_MODEL_ALIASES", "PROXY_MODEL_ALIASES_DISABLED")
			replaceRuntimeModelConfig(cfg, next)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Model aliases saved and active for new requests.", "models": modelRows(cfg)})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

// handleUIVirusTotal manages the VirusTotal API key pool used to scan media
// attachments. Keys are load-balanced round-robin with a 1h cooldown on errors;
// edits take effect immediately (no restart) and persist to .env.
func handleUIVirusTotal(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			raw, enabled := getVTRuntime()
			keys := parseVTKeys(raw)
			writeJSON(w, http.StatusOK, map[string]any{
				"enabled":     enabled,
				"keys_raw":    strings.Join(keys, "\n"),
				"count":       len(keys),
				"statuses":    vtKeyStatuses(keys),
				"threshold":   vtThreshold(cfg),
				"upload":      cfg.VirusTotalUpload,
				"fail_closed": cfg.VirusTotalFailClosed,
			})
		case http.MethodPost:
			var body struct {
				Enabled bool   `json:"enabled"`
				Keys    string `json:"keys"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
			keys := parseVTKeys(body.Keys)
			joined := strings.Join(keys, "\n")
			current := readEnvMap()
			if joined == "" {
				delete(current, "PROXY_VIRUSTOTAL_KEYS")
			} else {
				current["PROXY_VIRUSTOTAL_KEYS"] = joined
			}
			if body.Enabled {
				current["PROXY_VIRUSTOTAL_ENABLED"] = "1"
			} else {
				current["PROXY_VIRUSTOTAL_ENABLED"] = "0"
			}
			if err := writeEnvMap(current); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			syncProcessEnvKeys(current, "PROXY_VIRUSTOTAL_KEYS", "PROXY_VIRUSTOTAL_ENABLED")
			setVTRuntime(joined, body.Enabled)
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "message": "VirusTotal settings saved.",
				"enabled": body.Enabled, "count": len(keys), "statuses": vtKeyStatuses(keys),
			})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

// imageGenRequest is the OpenAI-compatible images/generations request body.
type imageGenRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	User           string `json:"user,omitempty"`
}

// handleImageGenerations serves POST /v1/images/generations (OpenAI-compatible).
// Image generation always routes through the local agy (Antigravity) CLI: agy
// writes the file(s) to a scratch dir and we return a link (or base64) to each.
func handleImageGenerations(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !cfg.AgyImage {
			writeOpenAIError(w, http.StatusNotFound, "image generation is disabled (set PROXY_AGY_IMAGE=1)")
			return
		}
		var in imageGenRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if strings.TrimSpace(in.Prompt) == "" {
			writeOpenAIError(w, http.StatusBadRequest, "prompt is required")
			return
		}
		sz, err := parseImageSize(in.Size)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, err.Error())
			return
		}
		n := in.N
		if n <= 0 {
			n = 1
		}
		model := agyImageModelForRequest(cfg, in.Model)
		setRequestStat(r, requestStat{Model: firstNonEmpty(in.Model, model), Upstream: "agy", Stream: false})
		setRequestNote(r, "image_gen model="+model+" size="+sz.Label+" n="+strconv.Itoa(n))
		imgs, err := agyGenerateImages(r.Context(), cfg, in.Prompt, model, n, sz)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "agy image error: "+err.Error())
			return
		}
		b64 := strings.EqualFold(strings.TrimSpace(in.ResponseFormat), "b64_json")
		data := make([]map[string]any, 0, len(imgs))
		for _, img := range imgs {
			item := map[string]any{}
			if b64 {
				raw, rerr := os.ReadFile(img.Path)
				if rerr != nil {
					continue
				}
				item["b64_json"] = base64.StdEncoding.EncodeToString(raw)
			} else {
				item["url"] = generatedImageURL(cfg, r, img.File)
			}
			if img.Width > 0 {
				item["width"], item["height"] = img.Width, img.Height
			}
			data = append(data, item)
		}
		updateRequestStat(r, func(stat *requestStat) { stat.StopReason = "stop" })
		writeJSON(w, http.StatusOK, map[string]any{
			"created": time.Now().Unix(),
			"model":   model,
			"data":    data,
		})
	}
}

// handleGeneratedImage serves GET /media/generated/<token>.<ext> — an
// unauthenticated capability URL (the token is an unguessable 128-bit value and
// the file is retention-limited). Only files in the generated-image root whose
// names match the minted pattern are served, so there is no path traversal.
func handleGeneratedImage(cfg config) http.HandlerFunc {
	root := agyImageRoot(cfg)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/media/generated/")
		if !validGeneratedImageName(name) {
			http.NotFound(w, r)
			return
		}
		full := filepath.Join(root, name)
		if rel, err := filepath.Rel(root, full); err != nil || strings.HasPrefix(rel, "..") || strings.ContainsRune(rel, os.PathSeparator) {
			http.NotFound(w, r)
			return
		}
		fi, err := os.Stat(full)
		if err != nil || fi.IsDir() {
			http.NotFound(w, r)
			return
		}
		ct, _ := sniffImage(full)
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		http.ServeFile(w, r, full)
	}
}

// handleUIImageGenerate is the admin-authed control-panel wrapper around image
// generation: same engine, richer payload (dims, bytes) for the Images page.
func handleUIImageGenerate(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !cfg.AgyImage {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "Image generation is disabled (set PROXY_AGY_IMAGE=1)."})
			return
		}
		var body struct {
			Prompt string `json:"prompt"`
			Model  string `json:"model"`
			Size   string `json:"size"`
			N      int    `json:"n"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		if strings.TrimSpace(body.Prompt) == "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "Enter a prompt."})
			return
		}
		sz, err := parseImageSize(body.Size)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		n := body.N
		if n <= 0 {
			n = 1
		}
		model := agyImageModelForRequest(cfg, body.Model)
		imgs, err := agyGenerateImages(r.Context(), cfg, body.Prompt, model, n, sz)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "model": model})
			return
		}
		out := make([]map[string]any, 0, len(imgs))
		for _, img := range imgs {
			out = append(out, map[string]any{
				"url":          generatedImageURL(cfg, r, img.File),
				"width":        img.Width,
				"height":       img.Height,
				"bytes":        img.Bytes,
				"content_type": img.ContentType,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "model": model, "size": sz.Label, "images": out,
		})
	}
}

// claudeSettingsApplied reports whether ~/.claude/settings.json currently points
// Claude Code at this proxy (ANTHROPIC_BASE_URL == this proxy's /anthropic URL).
func claudeSettingsApplied(cfg config) bool {
	settings, err := readJSONMap(defaultClaudeSettingsPath())
	if err != nil {
		return false
	}
	env, ok := settings["env"].(map[string]any)
	if !ok {
		return false
	}
	base, _ := env["ANTHROPIC_BASE_URL"].(string)
	return strings.EqualFold(strings.TrimSpace(base), "http://127.0.0.1:"+cfg.Port+"/anthropic")
}

// handleUISettingsApply lets the admin manually write proxy settings into Claude
// Code's settings.json. The start/stop scripts no longer do this automatically,
// so the control panel (interface) and the proxy server stay decoupled.
func handleUISettingsApply(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{
				"applied":       claudeSettingsApplied(cfg),
				"settings_path": defaultClaudeSettingsPath(),
			})
		case http.MethodPost:
			if err := applyProxySettingsGo(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":            true,
				"applied":       true,
				"settings_path": defaultClaudeSettingsPath(),
				"message":       "Applied proxy settings to Claude Code. Restart Claude Code to pick them up.",
			})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	}
}

func handleUIKeys(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		providers, clients, err := readKeyRows(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		baseURL := clientFacingBaseURL(cfg)
		writeJSON(w, http.StatusOK, map[string]any{
			"providers": providers,
			"clients":   clients,
			"defaults": map[string]string{
				"openai_base_url":  defaultProviderBaseURL("openai"),
				"gemini_base_url":  defaultProviderBaseURL("gemini"),
				"anthropic_base":   baseURL + "/anthropic",
				"openai_local_url": baseURL + "/openai/v1",
			},
		})
	}
}

func handleUIProviderKeys(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
			Label    string `json:"label"`
			BaseURL  string `json:"base_url"`
			APIKey   string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		provider := strings.ToLower(strings.TrimSpace(body.Provider))
		if provider != "openai" && provider != "gemini" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider must be openai or gemini"})
			return
		}
		apiKey := strings.TrimSpace(body.APIKey)
		if apiKey == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider API key is required"})
			return
		}
		label := strings.TrimSpace(body.Label)
		if label == "" {
			label = strings.Title(provider) + " key"
		}
		baseURL := strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
		if baseURL == "" {
			baseURL = defaultProviderBaseURL(provider)
		}
		enc, err := encryptSecret(cfg, apiKey)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		if err := migrateProxyDB(db); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		now := time.Now().Format(time.RFC3339)
		message := "Provider key saved."
		if id := strings.TrimSpace(body.ID); id != "" {
			res, err := db.Exec(`UPDATE provider_keys SET provider = ?, label = ?, base_url = ?, api_key_enc = ?, key_preview = ?, updated_at = ? WHERE id = ?`,
				provider, label, baseURL, enc, maskSecret(apiKey), now, id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			affected, _ := res.RowsAffected()
			if affected == 0 {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "provider key not found"})
				return
			}
			message = "Provider key renewed."
		} else {
			_, err = db.Exec(`INSERT INTO provider_keys (id, provider, label, base_url, api_key_enc, key_preview, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
				randomID("pk"), provider, label, baseURL, enc, maskSecret(apiKey), now, now)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		providers, clients, _ := readKeyRows(cfg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message, "providers": providers, "clients": clients})
	}
}

func handleUIClientKeys(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			Label         string `json:"label"`
			Schema        string `json:"schema"`
			Provider      string `json:"provider"`
			ProviderKeyID string `json:"provider_key_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		label := strings.TrimSpace(body.Label)
		if label == "" {
			label = "Client key"
		}
		schema := normalizeClientSchema(body.Schema)
		provider := strings.ToLower(strings.TrimSpace(body.Provider))
		if provider != "" && provider != "default" && provider != "openai" && provider != "gemini" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provider must be default, openai, or gemini"})
			return
		}
		if provider == "default" {
			provider = ""
		}
		rawKey := newClientAPIKey()
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		if err := migrateProxyDB(db); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		now := time.Now().Format(time.RFC3339)
		_, err = db.Exec(`INSERT INTO client_keys (id, label, key_hash, key_preview, schema, provider, provider_key_id, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			randomID("ck"), label, hashString(rawKey), maskSecret(rawKey), schema, provider, strings.TrimSpace(body.ProviderKeyID), now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		providers, clients, _ := readKeyRows(cfg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Client key created. Copy it now; it will not be shown again.", "api_key": rawKey, "providers": providers, "clients": clients})
	}
}

func handleUIKeyToggle(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			Kind    string `json:"kind"`
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		table := ""
		switch body.Kind {
		case "provider":
			table = "provider_keys"
		case "client":
			table = "client_keys"
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "kind must be provider or client"})
			return
		}
		db, err := openProxyDB(cfg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer db.Close()
		enabled := 0
		if body.Enabled {
			enabled = 1
		}
		_, err = db.Exec(`UPDATE `+table+` SET enabled = ?, updated_at = ? WHERE id = ?`, enabled, time.Now().Format(time.RFC3339), strings.TrimSpace(body.ID))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		providers, clients, _ := readKeyRows(cfg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "providers": providers, "clients": clients})
	}
}

func readKeyRows(cfg config) ([]providerKeyRow, []clientKeyRow, error) {
	db, err := openProxyDB(cfg)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	if err := migrateProxyDB(db); err != nil {
		return nil, nil, err
	}
	providers := []providerKeyRow{}
	rows, err := db.Query(`SELECT id, provider, label, base_url, key_preview, enabled, created_at, updated_at FROM provider_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row providerKeyRow
		var enabled int
		if err := rows.Scan(&row.ID, &row.Provider, &row.Label, &row.BaseURL, &row.KeyPreview, &enabled, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, nil, err
		}
		row.Schema = providerSchema(row.Provider)
		row.Enabled = enabled != 0
		providers = append(providers, row)
	}
	providerLabels := map[string]string{}
	for _, row := range providers {
		providerLabels[row.ID] = row.Label
	}
	clients := []clientKeyRow{}
	clientRows, err := db.Query(`SELECT id, label, key_preview, schema, provider, provider_key_id, enabled, created_at, updated_at, last_used_at FROM client_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, nil, err
	}
	defer clientRows.Close()
	for clientRows.Next() {
		var row clientKeyRow
		var enabled int
		if err := clientRows.Scan(&row.ID, &row.Label, &row.KeyPreview, &row.Schema, &row.Provider, &row.ProviderKeyID, &enabled, &row.CreatedAt, &row.UpdatedAt, &row.LastUsedAt); err != nil {
			return nil, nil, err
		}
		row.Schema = normalizeClientSchema(row.Schema)
		row.Enabled = enabled != 0
		row.ProviderLabel = providerLabels[row.ProviderKeyID]
		clients = append(clients, row)
	}
	return providers, clients, nil
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
		steps = append(steps, validateHTTP(http.MethodGet, base+"/anthropic/v1/models", headers, nil, "GET /anthropic/v1/models"))
		body := []byte(`{"model":"sonnet[1m]","messages":[{"role":"user","content":"Quick count test"}]}`)
		steps = append(steps, validateHTTP(http.MethodPost, base+"/anthropic/v1/messages/count_tokens", headers, body, "POST /anthropic/v1/messages/count_tokens"))
		body = []byte(`{"model":"sonnet[1m]","max_tokens":32,"messages":[{"role":"user","content":"Say hello in one sentence."}]}`)
		steps = append(steps, validateHTTP(http.MethodPost, base+"/anthropic/v1/messages", headers, body, "POST /anthropic/v1/messages"))
		body = []byte(`{"model":"sonnet[1m]","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Say streaming hello."}]}`)
		steps = append(steps, validateHTTP(http.MethodPost, base+"/anthropic/v1/messages", headers, body, "POST /anthropic/v1/messages (stream)"))
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
			"upstream_model": resolveModel(cfg, modelAlias),
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
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+cfg.Port+"/anthropic/v1/messages", bytes.NewReader(payload))
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
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if scheduled, err := scheduleSystemdRestart(); scheduled {
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Proxy service restarting."})
			return
		}
		bin := preferredProxyBinaryPath()
		cmd := exec.Command(bin, "start")
		if err := cmd.Start(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Proxy restarting."})
		go func() { time.Sleep(250 * time.Millisecond); os.Exit(0) }()
	}
}

func scheduleSystemdRestart() (bool, error) {
	if runtime.GOOS != "linux" {
		return false, nil
	}
	serviceName := startupName + ".service"
	if !systemdServiceInstalled(serviceName) {
		return false, nil
	}
	systemctlPath, err := exec.LookPath("systemctl")
	if err != nil {
		return true, err
	}
	if systemdRunPath, err := exec.LookPath("systemd-run"); err == nil {
		cmd := exec.Command(systemdRunPath, "--unit", startupName+"-dashboard-restart", "--on-active=1s", "--collect", systemctlPath, "restart", serviceName)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err == nil {
			return true, nil
		}
	}
	cmd := exec.Command("sh", "-c", "nohup sh -c 'sleep 1; systemctl restart connect-ai-proxy.service' >/dev/null 2>&1 &")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return true, cmd.Start()
}

func systemdServiceInstalled(serviceName string) bool {
	for _, dir := range []string{"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"} {
		if _, err := os.Stat(filepath.Join(dir, serviceName)); err == nil {
			return true
		}
	}
	return false
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
	return runClaudeSettingsSyncGo(action)
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
	keys := []string{"UPSTREAM", "CODEX_BASE_URL", "CODEX_AUTH_FILE", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_CLAUDE_SONNET_MODEL", "OPENAI_CLAUDE_SONNET_1M_MODEL", "OPENAI_CLAUDE_HAIKU_MODEL", "OPENAI_CLAUDE_OPUS_MODEL", "OPENAI_CLAUDE_OPUS_1M_MODEL", "OPENAI_CLAUDE_FAST_MODEL", "OPENAI_CLAUDE_CODEX_MODEL", "PROXY_MODEL_ALIASES", "PROXY_MODEL_ALIASES_DISABLED", "CODEX_FAST_SERVICE_TIER", "CODEX_WEB_SEARCH_TOOL_TYPE", "CODEX_WEB_SEARCH_CONTEXT_SIZE", "CODEX_REASONING_SUMMARY", "CODEX_SESSION_ISOLATION", "CODEX_SESSION_FILE", "CODEX_PROMPT_CACHE_KEY", "CODEX_UPSTREAM_HARD_TOKENS", "CODEX_UPSTREAM_BLOCK_AT_HARD", "CLAUDE_TOOL_ACTIVITY_THINKING", "ANTIGRAVITY_CHROME_PATH", "ANTIGRAVITY_EXTENSION_PATH", "ANTIGRAVITY_BROWSER_PROFILE", "ANTIGRAVITY_BROWSER_MODE", "ANTIGRAVITY_BROWSER_PRELAUNCH_WITH_PROXY", "ANTIGRAVITY_BROWSER_DEBUG_PORT", "ANTIGRAVITY_SCREENSHOT_DIR", "ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP", "ANTIGRAVITY_BROWSER_SAFE_DEFAULT_RELAUNCH", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES", "ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES", "ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES", "CLAUDE_CODE_EFFORT_LEVEL", "OPENAI_REASONING_EFFORT", "API_TIMEOUT_MS", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "PROXY_API_KEY", "PROXY_HOST", "PROXY_PORT", "PROXY_PUBLIC_URL", "PROXY_DB_PATH", "PROXY_AUTO_UPDATE", "PROXY_UPDATE_REPO_DIR", "PROXY_UPDATE_BRANCH", "PROXY_UPDATE_VERSION_URL", "PROXY_UPDATE_STATUS_FILE", "PROXY_DASHBOARD_FULL_SYSTEM_UPDATE", "ADMIN_USERNAME", "ADMIN_PASSWORD_HASH", "ADMIN_SESSION_SECRET"}
	seen := map[string]bool{}
	var b strings.Builder
	for _, k := range keys {
		seen[k] = true
		if v, ok := vals[k]; ok {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	var extra []string
	for k := range vals {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		b.WriteString(k + "=" + vals[k] + "\n")
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

func saveModelAliasesToEnvMap(vals map[string]string, submitted []modelAliasConfig) (config, error) {
	rows, err := normalizeSubmittedModelAliases(submitted)
	if err != nil {
		return config{}, err
	}
	env := mapEnvValue(vals)
	claudeDefaults := claudeDefaultsFromValues(env)
	baseModels := defaultModelAliasesFromValues(env)
	baseCfg := config{Models: baseModels, ClaudeDefaults: claudeDefaults}

	submittedAliases := map[string]bool{}
	for _, row := range rows {
		submittedAliases[row.Alias] = true
	}

	var disabled []string
	for alias := range baseModels {
		if !isAdvertisedModel(alias) {
			continue
		}
		if !submittedAliases[alias] {
			disabled = append(disabled, alias)
		}
	}
	sort.Strings(disabled)

	var overrides []modelAliasConfig
	for _, row := range rows {
		baseReal, isBase := baseModels[row.Alias]
		baseContext := contextForAlias(baseCfg, row.Alias)
		forwardCustom := row.ForwardTo != "" && row.ForwardTo != "codex"
		// A flag differing from its built-in seed must persist (e.g. unsetting the
		// default on "sonnet" or marking a new default), even if real/context match.
		flagCustom := row.Default != isDefaultModelAlias(row.Alias) || row.Recommended != isRecommendedModelAlias(row.Alias)
		if !isBase || baseReal != row.Real || row.Context != baseContext || forwardCustom || flagCustom {
			out := row
			if !forwardCustom {
				out.ForwardTo = "" // keep .env tidy: omitempty drops default codex
			}
			overrides = append(overrides, out)
		}
	}
	sort.Slice(overrides, func(i, j int) bool { return overrides[i].Alias < overrides[j].Alias })

	if len(overrides) == 0 {
		delete(vals, "PROXY_MODEL_ALIASES")
	} else {
		raw, _ := json.Marshal(overrides)
		vals["PROXY_MODEL_ALIASES"] = string(raw)
	}
	if len(disabled) == 0 {
		delete(vals, "PROXY_MODEL_ALIASES_DISABLED")
	} else {
		raw, _ := json.Marshal(disabled)
		vals["PROXY_MODEL_ALIASES_DISABLED"] = string(raw)
	}

	next := config{
		Models:           map[string]string{},
		ModelContexts:    map[string]string{},
		ModelCustom:      map[string]bool{},
		ModelForward:     map[string]string{},
		ModelDefault:     map[string]bool{},
		ModelRecommended: map[string]bool{},
		ClaudeDefaults:   claudeDefaults,
	}
	for _, row := range rows {
		next.Models[row.Alias] = row.Real
		next.ModelContexts[row.Alias] = row.Context
		next.ModelForward[row.Alias] = normalizeForwardTarget(row.ForwardTo)
		next.ModelDefault[row.Alias] = row.Default
		next.ModelRecommended[row.Alias] = row.Recommended
		if _, isBase := baseModels[row.Alias]; !isBase || !isAdvertisedModel(row.Alias) {
			next.ModelCustom[row.Alias] = true
		}
	}
	for _, row := range overrides {
		next.ModelCustom[row.Alias] = true
	}
	return next, nil
}

func normalizeSubmittedModelAliases(submitted []modelAliasConfig) ([]modelAliasConfig, error) {
	rows := make([]modelAliasConfig, 0, len(submitted))
	seen := map[string]bool{}
	for _, row := range submitted {
		alias := strings.TrimSpace(row.Alias)
		real := cleanModel(strings.TrimSpace(row.Real))
		if alias == "" || real == "" {
			return nil, fmt.Errorf("alias and Codex model are required")
		}
		if strings.ContainsAny(alias, " \t\r\n") {
			return nil, fmt.Errorf("alias %q cannot contain whitespace", alias)
		}
		key := strings.ToLower(alias)
		if seen[key] {
			return nil, fmt.Errorf("alias %q is duplicated", alias)
		}
		seen[key] = true
		rows = append(rows, modelAliasConfig{Alias: alias, Real: real, Context: normalizeModelContext(row.Context), ForwardTo: normalizeForwardTarget(row.ForwardTo), Default: row.Default, Recommended: row.Recommended})
	}
	return rows, nil
}

func syncProcessEnvKeys(vals map[string]string, keys ...string) {
	for _, key := range keys {
		if v, ok := vals[key]; ok {
			_ = os.Setenv(key, v)
		} else {
			_ = os.Unsetenv(key)
		}
	}
}

func replaceRuntimeModelConfig(cfg config, next config) {
	modelConfigMu.Lock()
	defer modelConfigMu.Unlock()
	replaceStringMap(cfg.Models, next.Models)
	replaceStringMap(cfg.ModelContexts, next.ModelContexts)
	replaceBoolMap(cfg.ModelCustom, next.ModelCustom)
	replaceStringMap(cfg.ModelForward, next.ModelForward)
	replaceBoolMap(cfg.ModelDefault, next.ModelDefault)
	replaceBoolMap(cfg.ModelRecommended, next.ModelRecommended)
	replaceStringMap(cfg.ClaudeDefaults, next.ClaudeDefaults)
}

func replaceStringMap(dst map[string]string, src map[string]string) {
	if dst == nil {
		return
	}
	for k := range dst {
		delete(dst, k)
	}
	for k, v := range src {
		dst[k] = v
	}
}

func replaceBoolMap(dst map[string]bool, src map[string]bool) {
	if dst == nil {
		return
	}
	for k := range dst {
		delete(dst, k)
	}
	for k, v := range src {
		dst[k] = v
	}
}

var supportedUpstreamModels = map[string]bool{
	"gpt-5.5":             true,
	"gpt-5.4":             true,
	"gpt-5.4-mini":        true,
	"gpt-5.3-codex":       true,
	"gpt-5.3-codex-spark": true,
}

func modelRows(cfg config) []map[string]any {
	rows := []map[string]any{}
	modelConfigMu.RLock()
	defer modelConfigMu.RUnlock()
	for alias, real := range cfg.Models {
		if !isAdvertisedModel(alias) && !cfg.ModelCustom[alias] {
			continue
		}
		context := contextForAlias(cfg, alias)
		contextWindow := contextWindowTokensForModelContext(cfg, context)
		modelUpstream := requestUpstream(cfg, nil, alias)
		if strings.EqualFold(modelUpstream, "codex") {
			context = "200k"
		}
		maxOutput, limitsSource := maxOutputTokensForModel(real)
		maxInput := contextWindow
		if maxOutput > 0 {
			maxInput = contextWindow - maxOutput
			if maxInput < 0 {
				maxInput = 0
			}
		}
		rows = append(rows, map[string]any{
			"alias":                 alias,
			"real":                  real,
			"status":                modelStatusForRow(cfg, alias, real),
			"context":               context,
			"context_window_tokens": contextWindow,
			"max_input_tokens":      maxInput,
			"max_output_tokens":     maxOutput,
			"limits_known":          maxOutput > 0,
			"limits_source":         limitsSource,
			"default":               defaultForAlias(cfg, alias),
			"recommended":           recommendedForAlias(cfg, alias),
			"forward_to":            forwardForAlias(cfg, alias),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		left := fmt.Sprint(rows[i]["alias"])
		right := fmt.Sprint(rows[j]["alias"])
		return left < right
	})
	return rows
}

func modelCapabilityRows(cfg config) []map[string]any {
	models := modelRows(cfg)
	rows := make([]map[string]any, 0, len(models))
	for _, model := range models {
		alias := fmt.Sprint(model["alias"])
		real := fmt.Sprint(model["real"])
		rows = append(rows, map[string]any{
			"id":                    alias,
			"type":                  "model_capability",
			"display_name":          alias,
			"alias":                 alias,
			"upstream_model":        real,
			"context":               model["context"],
			"context_window_tokens": model["context_window_tokens"],
			"max_input_tokens":      model["max_input_tokens"],
			"max_output_tokens":     model["max_output_tokens"],
			"limits_known":          model["limits_known"],
			"limits_source":         model["limits_source"],
			"status":                model["status"],
			"default":               model["default"],
			"recommended":           model["recommended"],
		})
	}
	return rows
}

func modelStatus(real string) string {
	real = strings.ToLower(strings.TrimSpace(real))
	if supportedUpstreamModels[real] {
		return "ok"
	}
	if real != "" {
		return "untested"
	}
	return "unsupported"
}

// antigravityModels is the catalog the agy CLI accepts (from `agy models`),
// lowercased. Used so an Agy-forwarded alias shows "Available" rather than
// "Untested" for a recognized Antigravity model.
var antigravityModels = map[string]bool{
	"gemini 3.1 pro (high)":        true,
	"gemini 3.1 pro (low)":         true,
	"gemini 3.5 flash (high)":      true,
	"gemini 3.5 flash (medium)":    true,
	"gemini 3.5 flash (low)":       true,
	"claude sonnet 4.6 (thinking)": true,
	"claude opus 4.6 (thinking)":   true,
	"gpt-oss 120b (medium)":        true,
}

// modelVerified tracks aliases that have served at least one successful request
// this process lifetime, so "I tested it and it works" is reflected as Available.
// In-memory only — resets on restart and re-verifies on the next success.
var modelVerified sync.Map // alias → true

func markModelAliasVerified(alias string) {
	alias = strings.TrimSpace(alias)
	if alias != "" {
		modelVerified.Store(alias, true)
	}
}

func modelAliasVerified(alias string) bool {
	_, ok := modelVerified.Load(strings.TrimSpace(alias))
	return ok
}

// modelStatusForRow computes the Models-page status with awareness of the
// forward target and real usage: a verified alias is Available; an Agy alias is
// Available when its model is a known Antigravity model; otherwise the codex
// heuristic applies.
func modelStatusForRow(cfg config, alias, real string) string {
	if strings.TrimSpace(real) == "" {
		return "unsupported"
	}
	if modelAliasVerified(alias) {
		return "ok"
	}
	if forwardForAlias(cfg, alias) == "agy" {
		if antigravityModels[strings.ToLower(strings.TrimSpace(real))] {
			return "ok"
		}
		return "untested"
	}
	if forwardForAlias(cfg, alias) == "claude" {
		low := strings.ToLower(strings.TrimSpace(real))
		if i := strings.Index(low, "["); i > 0 {
			low = strings.TrimSpace(low[:i])
		}
		if low == "sonnet" || low == "opus" || low == "haiku" || low == "fable" || strings.HasPrefix(low, "claude") {
			return "ok"
		}
		return "untested"
	}
	return modelStatus(real)
}

func isDefaultModelAlias(alias string) bool {
	alias = strings.ToLower(strings.TrimSpace(alias))
	return alias == "sonnet" || alias == "sonnet[1m]"
}

func isRecommendedModelAlias(alias string) bool {
	return strings.EqualFold(strings.TrimSpace(alias), "gpt-5.3-codex")
}

func contextWindowTokensForLabel(context string) int {
	context = strings.ToLower(strings.TrimSpace(context))
	switch context {
	case "1m", "1m tokens", "1000k":
		return 1000000
	case "200k", "200k tokens", "":
		return 200000
	}
	if strings.HasSuffix(context, "k") {
		if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(context, "k"))); err == nil && n > 0 {
			return n * 1000
		}
	}
	if strings.HasSuffix(context, "m") {
		if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(context, "m"))); err == nil && n > 0 {
			return n * 1000000
		}
	}
	if n, err := strconv.Atoi(context); err == nil && n > 0 {
		return n
	}
	return 200000
}

func contextWindowTokensForModelContext(cfg config, context string) int {
	if strings.EqualFold(strings.TrimSpace(cfg.Upstream), "codex") {
		return defaultCodexWindow
	}
	return contextWindowTokensForLabel(context)
}

func maxOutputTokensForModel(model string) (int, string) {
	model = strings.ToLower(cleanModel(model))
	switch {
	case model == "":
		return 0, "unknown"
	case strings.Contains(model, "antigravity") || strings.Contains(model, "banana") || strings.Contains(model, "gemini"):
		return 8192, "built_in"
	case strings.Contains(model, "gpt-5") || strings.Contains(model, "codex"):
		return 128000, "built_in"
	case strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4"):
		return 100000, "built_in"
	case strings.HasPrefix(model, "gpt-4.1"):
		return 32768, "built_in"
	case strings.HasPrefix(model, "gpt-4o") || strings.HasPrefix(model, "gpt-4.5"):
		return 16384, "built_in"
	default:
		return 0, "unknown"
	}
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
	if context := normalizeModelContext(cfg.ModelContexts[alias]); context != "200k" || cfg.ModelContexts[alias] != "" {
		return context
	}
	if lower == "opus" || lower == "claude-opus-4-7" {
		return contextFromModel(cfg.ClaudeDefaults["opus"])
	}
	if lower == "sonnet" || lower == "claude-sonnet-4-6" {
		return contextFromModel(cfg.ClaudeDefaults["sonnet"])
	}
	if strings.Contains(lower, "[1m]") || lower == "gpt-5.3-codex" || strings.Contains(lower, "antigravity") || strings.Contains(lower, "banana") || strings.Contains(lower, "gemini") {
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
	return strings.HasPrefix(row.Path, "/anthropic/v1/") || strings.HasPrefix(row.Path, "/openai/v1/")
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

func resolveModel(cfg config, model string) string {
	modelConfigMu.RLock()
	mapped, ok := cfg.Models[model]
	modelConfigMu.RUnlock()
	if ok {
		return mapped
	}
	return cleanModel(model)
}

func responsesRequestToOpenAI(in responsesRequest) openAIRequest {
	out := openAIRequest{
		Model:  in.Model,
		Stream: in.Stream,
	}
	if in.Instructions != "" {
		out.Messages = append(out.Messages, openAIMessage{
			Role:    "system",
			Content: in.Instructions,
		})
	}
	for _, item := range in.Input {
		if m, ok := item.(map[string]any); ok {
			role := stringField(m, "role")
			content := m["content"]
			if role == "" {
				role = stringField(m, "type")
				if strings.HasPrefix(role, "input_") {
					role = strings.TrimPrefix(role, "input_")
				}
				if role == "image" {
					role = "user"
					content = []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": stringField(m, "image_url")}}}
				}
			}
			msg := openAIMessage{
				Role:    role,
				Content: content,
			}
			if role == "tool" {
				msg.ToolCallID = stringField(m, "call_id")
			}
			out.Messages = append(out.Messages, msg)
		}
	}
	return out
}

func extractBase64DataAndExt(part map[string]any) ([]byte, string, error) {
	if imgUrl, ok := part["image_url"].(map[string]any); ok {
		if urlStr, ok := imgUrl["url"].(string); ok {
			return parseDataURL(urlStr)
		}
	}
	if urlStr, ok := part["image_url"].(string); ok {
		return parseDataURL(urlStr)
	}

	if source, ok := part["source"].(map[string]any); ok {
		if stringField(source, "type") == "base64" {
			mediaType := stringField(source, "media_type")
			dataStr := stringField(source, "data")
			ext := ".jpg"
			if strings.Contains(mediaType, "png") {
				ext = ".png"
			} else if strings.Contains(mediaType, "webp") {
				ext = ".webp"
			} else if strings.Contains(mediaType, "pdf") {
				ext = ".pdf"
			} else if strings.Contains(mediaType, "mp4") {
				ext = ".mp4"
			}
			dec, err := base64.StdEncoding.DecodeString(dataStr)
			return dec, ext, err
		}
	}

	if fileObj, ok := part["file"].(map[string]any); ok {
		filename := stringField(fileObj, "filename")
		ext := filepath.Ext(filename)
		if ext == "" {
			ext = ".pdf"
		}
		dataStr := stringField(fileObj, "file_data")
		dec, err := base64.StdEncoding.DecodeString(dataStr)
		return dec, ext, err
	}

	return nil, "", fmt.Errorf("unsupported file block")
}

func parseDataURL(urlStr string) ([]byte, string, error) {
	if !strings.HasPrefix(urlStr, "data:") {
		return nil, "", fmt.Errorf("not a data URL")
	}
	comma := strings.Index(urlStr, ",")
	if comma == -1 {
		return nil, "", fmt.Errorf("invalid data URL")
	}
	header := urlStr[:comma]
	dataStr := urlStr[comma+1:]

	ext := ".jpg"
	if strings.Contains(header, "image/png") {
		ext = ".png"
	} else if strings.Contains(header, "image/webp") {
		ext = ".webp"
	} else if strings.Contains(header, "application/pdf") {
		ext = ".pdf"
	} else if strings.Contains(header, "video/mp4") {
		ext = ".mp4"
	}

	dec, err := base64.StdEncoding.DecodeString(dataStr)
	return dec, ext, err
}

func requestUpstream(cfg config, r *http.Request, requestedModel string) string {
	// Gemini/Antigravity support has been removed. This proxy only talks to the
	// OpenAI family: codex (ChatGPT backend) or the OpenAI API. Honour an
	// explicit per-key provider route when it names an OpenAI-family provider,
	// but never route to Google.
	if r != nil {
		if route, ok := requestProviderRoute(r); ok {
			if p := strings.ToLower(route.Provider); p == "openai" || p == "codex" {
				return p
			}
		}
	}
	return cfg.Upstream
}

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
