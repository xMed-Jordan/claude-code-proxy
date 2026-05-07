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
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
)

const (
	traceLogPath           = ".proxy.trace.log"
	traceBodyLimit         = 32768
	traceMaxBytes          = 10 * 1024 * 1024
	sseMaxLineSize         = 64 * 1024 * 1024
	antigravityExtensionID = "eeijfnjmjelapkebgockoeaadonbchdd"
)

type config struct {
	OpenAIAPIKey       string
	OpenAIBaseURL      string
	Upstream           string
	CodexBaseURL       string
	CodexAuthFile      string
	CodexSessionFile   string
	DBPath             string
	ProxyKey           string
	Port               string
	Models             map[string]string
	ModelContexts      map[string]string
	ModelCustom        map[string]bool
	ClaudeDefaults     map[string]string
	ReasoningEffort    string
	AdminUsername      string
	AdminPasswordHash  string
	AdminSessionSecret string
}

type modelAliasConfig struct {
	Alias   string `json:"alias"`
	Real    string `json:"real"`
	Context string `json:"context,omitempty"`
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
	Model          string              `json:"model"`
	Instructions   string              `json:"instructions,omitempty"`
	Input          []any               `json:"input"`
	Tools          []responsesTool     `json:"tools,omitempty"`
	Reasoning      *responsesReasoning `json:"reasoning,omitempty"`
	Temperature    *float64            `json:"temperature,omitempty"`
	ServiceTier    string              `json:"service_tier,omitempty"`
	PromptCacheKey string              `json:"prompt_cache_key,omitempty"`
	Stream         bool                `json:"stream,omitempty"`
	Store          bool                `json:"store"`
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
	loadDotEnvIntoProcess()
	if len(os.Args) > 1 && os.Args[1] == "--antigravity-mcp" {
		runAntigravityMCP()
		return
	}

	cfg := loadConfig()
	if err := initProxyDB(cfg); err != nil {
		log.Printf("proxy database init failed: %v", err)
	}
	proxyEnabled.Store(true)
	mux := newProxyMux(cfg)
	server := &http.Server{Addr: "127.0.0.1:" + cfg.Port, Handler: loggingMiddleware(mux), ReadHeaderTimeout: 15 * time.Second}
	log.Printf("claude-code-proxy listening on http://%s", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func newProxyMux(cfg config) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/anthropic/v1/models", requireAuth(cfg, requireProxyEnabled(handleModels(cfg))))
	mux.HandleFunc("/anthropic/v1/messages", requireAuth(cfg, requireProxyEnabled(handleMessages(cfg))))
	mux.HandleFunc("/anthropic/v1/messages/count_tokens", requireAuth(cfg, requireProxyEnabled(handleCountTokens)))
	mux.HandleFunc("/openai/v1/models", requireAuth(cfg, requireProxyEnabled(handleModels(cfg))))
	mux.HandleFunc("/openai/v1/chat/completions", requireAuth(cfg, requireProxyEnabled(handleChatCompletions(cfg))))
	mux.HandleFunc("/openai/v1/responses", requireAuth(cfg, requireProxyEnabled(handleResponses(cfg))))
	mux.HandleFunc("/openai/v1/files", requireAuth(cfg, requireProxyEnabled(handleOpenAIFiles(cfg))))
	mux.HandleFunc("/openai/v1/files/", requireAuth(cfg, requireProxyEnabled(handleOpenAIFiles(cfg))))
	mux.HandleFunc("/ui/api/auth/status", handleUIAuthStatus(cfg))
	mux.HandleFunc("/ui/api/auth/setup", handleUIAuthSetup(cfg))
	mux.HandleFunc("/ui/api/auth/login", handleUIAuthLogin(cfg))
	mux.HandleFunc("/ui/api/auth/logout", handleUIAuthLogout(cfg))
	mux.HandleFunc("/ui/api/status", requireAdmin(cfg, handleUIStatus(cfg)))
	mux.HandleFunc("/ui/api/config", requireAdmin(cfg, handleUIConfig(cfg)))
	mux.HandleFunc("/ui/api/models", requireAdmin(cfg, handleUIModels(cfg)))
	mux.HandleFunc("/ui/api/keys", requireAdmin(cfg, handleUIKeys(cfg)))
	mux.HandleFunc("/ui/api/keys/provider", requireAdmin(cfg, handleUIProviderKeys(cfg)))
	mux.HandleFunc("/ui/api/keys/client", requireAdmin(cfg, handleUIClientKeys(cfg)))
	mux.HandleFunc("/ui/api/keys/toggle", requireAdmin(cfg, handleUIKeyToggle(cfg)))
	mux.HandleFunc("/ui/api/validate", requireAdmin(cfg, handleUIValidate(cfg)))
	mux.HandleFunc("/ui/api/test", requireAdmin(cfg, handleUITest(cfg)))
	mux.HandleFunc("/ui/api/logs", requireAdmin(cfg, handleUILogs))
	mux.HandleFunc("/ui/api/antigravity", requireAdmin(cfg, handleUIAntigravityStatus))
	mux.HandleFunc("/ui/api/antigravity/probe", requireAdmin(cfg, handleUIAntigravityProbe))
	mux.HandleFunc("/ui/api/proxy/stop", requireAdmin(cfg, handleUIStop))
	mux.HandleFunc("/ui/api/proxy/start", requireAdmin(cfg, handleUIStart))
	mux.HandleFunc("/ui/api/proxy/restart", requireAdmin(cfg, handleUIRestart(cfg)))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy_running": proxyEnabled.Load()})
	})
	mux.HandleFunc("/antigravity/bridge", handleAntigravityBridge)
	mux.HandleFunc("/", handleUI)
	return mux
}

func loadConfig() config {
	port := getenv("PROXY_PORT", getenv("LITELLM_PORT", "4000"))
	baseURL := strings.TrimRight(getenv("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/")
	codexAuthFile := os.Getenv("CODEX_AUTH_FILE")
	if codexAuthFile == "" {
		codexAuthFile = filepath.Join(os.Getenv("USERPROFILE"), ".codex", "auth.json")
	}
	env := processEnvValue
	claudeDefaults := claudeDefaultsFromValues(env)
	models, modelContexts, modelCustom := modelAliasesFromValues(env)
	return config{
		OpenAIAPIKey:       os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:      baseURL,
		Upstream:           strings.ToLower(getenv("UPSTREAM", "codex")),
		CodexBaseURL:       strings.TrimRight(getenv("CODEX_BASE_URL", "https://chatgpt.com/backend-api/codex"), "/"),
		CodexAuthFile:      codexAuthFile,
		CodexSessionFile:   getenv("CODEX_SESSION_FILE", ".proxy.sessions.json"),
		DBPath:             getenv("PROXY_DB_PATH", ".proxy.db"),
		ProxyKey:           getenv("PROXY_API_KEY", os.Getenv("LITELLM_MASTER_KEY")),
		Port:               port,
		ReasoningEffort:    normalizeReasoningEffort(getenv("CLAUDE_CODE_EFFORT_LEVEL", getenv("OPENAI_REASONING_EFFORT", "xhigh"))),
		ClaudeDefaults:     claudeDefaults,
		Models:             models,
		ModelContexts:      modelContexts,
		ModelCustom:        modelCustom,
		AdminUsername:      strings.TrimSpace(getenv("ADMIN_USERNAME", "")),
		AdminPasswordHash:  strings.TrimSpace(getenv("ADMIN_PASSWORD_HASH", "")),
		AdminSessionSecret: strings.TrimSpace(getenv("ADMIN_SESSION_SECRET", "")),
	}
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
	}
}

func modelAliasesFromValues(env envValueFunc) (map[string]string, map[string]string, map[string]bool) {
	models := defaultModelAliasesFromValues(env)
	contexts := map[string]string{}
	custom := map[string]bool{}
	for _, row := range parseModelAliasConfigs(env("PROXY_MODEL_ALIASES", "")) {
		alias := strings.TrimSpace(row.Alias)
		real := cleanModel(strings.TrimSpace(row.Real))
		if alias == "" || real == "" {
			continue
		}
		models[alias] = real
		contexts[alias] = normalizeModelContext(row.Context)
		custom[alias] = true
	}
	for alias := range parseDisabledModelAliases(env("PROXY_MODEL_ALIASES_DISABLED", "")) {
		delete(models, alias)
		delete(contexts, alias)
		delete(custom, alias)
	}
	return models, contexts, custom
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
	seed := firstNonEmpty(cfg.ProxyKey, cfg.AdminSessionSecret, "claude-code-proxy-local-secret")
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
		setAdminSessionCookie(w, cfg, username)
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
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
			return
		}
		if !hmac.Equal([]byte(strings.TrimSpace(body.Username)), []byte(cfg.AdminUsername)) || !verifyAdminPassword(cfg.AdminPasswordHash, body.Password) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid username or password"})
			return
		}
		setAdminSessionCookie(w, cfg, cfg.AdminUsername)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleUIAuthLogout(_ config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: adminSessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func setAdminSessionCookie(w http.ResponseWriter, cfg config, username string) {
	exp := time.Now().Add(24 * time.Hour).Unix()
	payload := fmt.Sprintf("%s|%d", username, exp)
	sig := adminSessionSignature(cfg, payload)
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig,
		Path:     "/",
		MaxAge:   24 * 60 * 60,
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
	secret := firstNonEmpty(cfg.AdminSessionSecret, cfg.ProxyKey, "claude-code-proxy-admin")
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
			if proxyRequestHasKey(r, cfg.ProxyKey) {
				next(w, r)
				return
			}
			if route, ok := lookupClientKeyRoute(cfg, r); ok {
				if !clientKeySchemaAllowsPath(route.Schema, r.URL.Path) {
					writeSchemaAuthError(w, r, route.Schema)
					return
				}
				requestProviderKeys.Store(r, route)
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
	switch normalizeClientSchema(schema) {
	case "anthropic":
		return strings.HasPrefix(path, "/anthropic/")
	case "openai":
		return strings.HasPrefix(path, "/openai/")
	default:
		return strings.HasPrefix(path, "/anthropic/") || strings.HasPrefix(path, "/openai/")
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
		route, hasProviderRoute := requestProviderRoute(r)
		if cfg.Upstream == "openai" && !hasProviderRoute && (cfg.OpenAIAPIKey == "" || cfg.OpenAIAPIKey == "YOUR_OPENAI_API_KEY" || strings.HasPrefix(cfg.OpenAIAPIKey, "sk-or-")) {
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
			responsesReq := toResponses(cfg, in)
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
		out := openAIChatToResponses(cfg, in)
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
		in.Model = resolveModel(cfg, in.Model)
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
	out := responsesRequest{Model: model, Instructions: instructions, Stream: in.Stream, Store: false}
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
	if out.PromptCacheKey != "" && strings.Contains(lower, "prompt_cache_key") {
		out.PromptCacheKey = ""
		return codexRetryRequest{reason: "prompt_cache_key_not_accepted", request: out}, true
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
	if chars <= 0 {
		return 1
	}
	return max(1, chars/4)
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
	out := responsesRequest{Model: model, Stream: in.Stream, Store: false}
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
		if text := contentToText(choice.Message.Content); text != "" {
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
	launcherPath := filepath.Join(mustGetwd(), "start-antigravity-browser-mcp.ps1")
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
			"path":   launcherPath,
			"exists": fileExists(launcherPath),
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
	if path := strings.TrimSpace(os.Getenv("ANTIGRAVITY_EXTENSION_PATH")); path != "" && fileExists(filepath.Join(path, "manifest.json")) {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	}
	root := filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "User Data", "Default", "Extensions", antigravityExtensionID)
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	bestPath := ""
	var bestTime time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if !fileExists(filepath.Join(path, "manifest.json")) {
			continue
		}
		info, err := entry.Info()
		if err == nil && (bestPath == "" || info.ModTime().After(bestTime)) {
			bestPath = path
			bestTime = info.ModTime()
		}
		if bestPath == "" {
			bestPath = path
		}
	}
	return bestPath
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
	settingsPath := filepath.Join(os.Getenv("USERPROFILE"), ".claude.json")
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
	return out
}

func claudeDesktopConfigPaths() []string {
	paths := []string{}
	if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
		paths = append(paths, filepath.Join(localAppData, "Claude-3p", "claude_desktop_config.json"))
	}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		paths = append(paths, filepath.Join(appData, "Claude", "claude_desktop_config.json"))
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		key := strings.ToLower(path)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, path)
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
	if path := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CHROME_PATH")); path != "" && fileExists(path) {
		return path
	}
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "Application", "chrome.exe"),
	}
	for _, candidate := range candidates {
		if candidate != "" && fileExists(candidate) {
			return candidate
		}
	}
	return ""
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
		localURL := "http://127.0.0.1:" + cfg.Port
		writeJSON(w, http.StatusOK, map[string]any{
			"running":          proxyEnabled.Load(),
			"proxy_running":    proxyEnabled.Load(),
			"pid":              os.Getpid(),
			"uptime_seconds":   int(time.Since(startedAt).Seconds()),
			"local_url":        localURL,
			"anthropic_url":    localURL + "/anthropic",
			"openai_url":       localURL + "/openai/v1",
			"port":             cfg.Port,
			"upstream":         cfg.Upstream,
			"codex_auth":       auth,
			"codex_sessions":   codexSessionMetadata(cfg),
			"claude_settings":  claudeSettingsMetadata(cfg),
			"antigravity":      antigravityStatus(),
			"dashboard":        dashboardMetrics(),
			"models":           modelRows(cfg),
			"last_request":     lastRequest(),
			"claude_version":   strings.TrimSpace(claudeVersion),
			"proxy_key":        cfg.ProxyKey,
			"proxy_key_masked": maskSecret(cfg.ProxyKey),
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
		writeJSON(w, http.StatusOK, map[string]any{
			"providers": providers,
			"clients":   clients,
			"defaults": map[string]string{
				"openai_base_url":  defaultProviderBaseURL("openai"),
				"gemini_base_url":  defaultProviderBaseURL("gemini"),
				"anthropic_base":   "http://127.0.0.1:" + cfg.Port + "/anthropic",
				"openai_local_url": "http://127.0.0.1:" + cfg.Port + "/openai/v1",
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
	keys := []string{"UPSTREAM", "CODEX_BASE_URL", "CODEX_AUTH_FILE", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_CLAUDE_SONNET_MODEL", "OPENAI_CLAUDE_SONNET_1M_MODEL", "OPENAI_CLAUDE_HAIKU_MODEL", "OPENAI_CLAUDE_OPUS_MODEL", "OPENAI_CLAUDE_OPUS_1M_MODEL", "OPENAI_CLAUDE_FAST_MODEL", "OPENAI_CLAUDE_CODEX_MODEL", "PROXY_MODEL_ALIASES", "PROXY_MODEL_ALIASES_DISABLED", "CODEX_FAST_SERVICE_TIER", "CODEX_WEB_SEARCH_TOOL_TYPE", "CODEX_WEB_SEARCH_CONTEXT_SIZE", "CODEX_REASONING_SUMMARY", "CODEX_SESSION_ISOLATION", "CODEX_SESSION_FILE", "CODEX_PROMPT_CACHE_KEY", "CLAUDE_TOOL_ACTIVITY_THINKING", "ANTIGRAVITY_CHROME_PATH", "ANTIGRAVITY_EXTENSION_PATH", "ANTIGRAVITY_BROWSER_PROFILE", "ANTIGRAVITY_BROWSER_MODE", "ANTIGRAVITY_BROWSER_PRELAUNCH_WITH_PROXY", "ANTIGRAVITY_BROWSER_DEBUG_PORT", "ANTIGRAVITY_SCREENSHOT_DIR", "ANTIGRAVITY_BROWSER_FORCE_DEFAULT_CDP", "ANTIGRAVITY_BROWSER_SAFE_DEFAULT_RELAUNCH", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES", "ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES", "ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES", "CLAUDE_CODE_EFFORT_LEVEL", "OPENAI_REASONING_EFFORT", "API_TIMEOUT_MS", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "PROXY_API_KEY", "PROXY_PORT", "PROXY_DB_PATH", "ADMIN_USERNAME", "ADMIN_PASSWORD_HASH", "ADMIN_SESSION_SECRET"}
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
		if !isBase || baseReal != row.Real || row.Context != baseContext {
			overrides = append(overrides, row)
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
		Models:         map[string]string{},
		ModelContexts:  map[string]string{},
		ModelCustom:    map[string]bool{},
		ClaudeDefaults: claudeDefaults,
	}
	for _, row := range rows {
		next.Models[row.Alias] = row.Real
		next.ModelContexts[row.Alias] = row.Context
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
		rows = append(rows, modelAliasConfig{Alias: alias, Real: real, Context: normalizeModelContext(row.Context)})
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

func modelRows(cfg config) []map[string]any {
	supported := map[string]bool{"gpt-5.5": true, "gpt-5.4": true, "gpt-5.4-mini": true, "gpt-5.3-codex": true}
	rows := []map[string]any{}
	modelConfigMu.RLock()
	defer modelConfigMu.RUnlock()
	for alias, real := range cfg.Models {
		if !isAdvertisedModel(alias) && !cfg.ModelCustom[alias] {
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
	sort.Slice(rows, func(i, j int) bool {
		left := fmt.Sprint(rows[i]["alias"])
		right := fmt.Sprint(rows[j]["alias"])
		return left < right
	})
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
	if context := normalizeModelContext(cfg.ModelContexts[alias]); context != "200k" || cfg.ModelContexts[alias] != "" {
		return context
	}
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
