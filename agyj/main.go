// agyj - JSON-output wrapper for the Antigravity CLI (agy).
//
// Passes args through to agy.exe (print mode), then reads the freshly-written
// SQLite conversation db and emits a single JSON object to stdout.
//
// Pure-Go SQLite via modernc.org/sqlite (no CGO required).
// Protobuf parsing is hand-rolled with no external proto library.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Paths
// ---------------------------------------------------------------------------

// agyBinaryName is the platform-specific Antigravity CLI executable name.
func agyBinaryName() string {
	if runtime.GOOS == "windows" {
		return "agy.exe"
	}
	return "agy"
}

// agyBin resolves the agy CLI binary, in order:
//  1. AGYJ_AGY_BIN env override (explicit path),
//  2. a sibling of this executable (bundled install),
//  3. the system PATH,
//  4. a per-OS default location.
func agyBin() string {
	if v := strings.TrimSpace(os.Getenv("AGYJ_AGY_BIN")); v != "" {
		return v
	}
	name := agyBinaryName()
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), name)
		if st, statErr := os.Stat(sibling); statErr == nil && !st.IsDir() {
			return sibling
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "AppData", "Local", "agy", "bin", "agy.exe")
		}
		return "agy.exe"
	}
	// Linux/macOS: fall back to the bare name and let exec resolve it at run time.
	return name
}

// geminiCliDir is the Antigravity CLI data root. Defaults to
// ~/.gemini/antigravity-cli on every OS (the CLI uses home-relative paths);
// override with AGYJ_GEMINI_DIR when the install lives elsewhere (e.g. servers).
func geminiCliDir() string {
	if v := strings.TrimSpace(os.Getenv("AGYJ_GEMINI_DIR")); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

func conversationsDir() string {
	if v := strings.TrimSpace(os.Getenv("AGYJ_CONVERSATIONS_DIR")); v != "" {
		return v
	}
	return filepath.Join(geminiCliDir(), "conversations")
}

func settingsFile() string {
	return filepath.Join(geminiCliDir(), "settings.json")
}

// ---------------------------------------------------------------------------
// Protobuf wire-format helpers (no external library)
// ---------------------------------------------------------------------------

// readVarint reads a varint from buf at pos, returns (value, newPos, error).
func readVarint(buf []byte, pos int) (uint64, int, error) {
	var result uint64
	var shift uint
	for pos < len(buf) {
		b := buf[pos]
		result |= uint64(b&0x7F) << shift
		pos++
		if b&0x80 == 0 {
			return result, pos, nil
		}
		shift += 7
		if shift > 70 {
			return 0, pos, fmt.Errorf("varint too long")
		}
	}
	return 0, pos, fmt.Errorf("unexpected end of buffer reading varint")
}

// protoField holds a single parsed protobuf field.
type protoField struct {
	fieldNum uint64
	wireType uint64
	intVal   uint64 // wire type 0
	bytesVal []byte // wire type 2
}

// parseMessage parses a protobuf message into a slice of fields.
// Wire types 1 (64-bit fixed) and 5 (32-bit fixed) are skipped silently.
func parseMessage(data []byte) ([]protoField, error) {
	var fields []protoField
	pos := 0
	for pos < len(data) {
		key, newPos, err := readVarint(data, pos)
		if err != nil {
			return nil, err
		}
		pos = newPos

		fieldNum := key >> 3
		wireType := key & 7

		switch wireType {
		case 0: // varint
			val, np, err := readVarint(data, pos)
			if err != nil {
				return nil, err
			}
			pos = np
			fields = append(fields, protoField{fieldNum: fieldNum, wireType: 0, intVal: val})

		case 1: // 64-bit fixed — skip
			if pos+8 > len(data) {
				return nil, fmt.Errorf("not enough data for 64-bit fixed")
			}
			pos += 8

		case 2: // length-delimited
			ln, np, err := readVarint(data, pos)
			if err != nil {
				return nil, err
			}
			pos = np
			if pos+int(ln) > len(data) {
				return nil, fmt.Errorf("not enough data for length-delimited field")
			}
			chunk := data[pos : pos+int(ln)]
			pos += int(ln)
			fields = append(fields, protoField{fieldNum: fieldNum, wireType: 2, bytesVal: chunk})

		case 5: // 32-bit fixed — skip
			if pos+4 > len(data) {
				return nil, fmt.Errorf("not enough data for 32-bit fixed")
			}
			pos += 4

		default:
			return nil, fmt.Errorf("unknown wire type %d at offset %d", wireType, pos)
		}
	}
	return fields, nil
}

// getFields recursively extracts all leaf string values at path from a protobuf message.
// path is a list of field numbers, e.g. [20, 1] → for each field-20 sub-message,
// collect every field-1 string inside. Silently skips malformed data.
func getFields(data []byte, path []uint64) []string {
	if len(path) == 0 {
		return []string{string(data)}
	}

	fields, err := parseMessage(data)
	if err != nil {
		return nil
	}

	targetField := path[0]
	var results []string
	for _, f := range fields {
		if f.fieldNum != targetField {
			continue
		}
		if f.wireType == 2 {
			if len(path) == 1 {
				results = append(results, string(f.bytesVal))
			} else {
				results = append(results, getFields(f.bytesVal, path[1:])...)
			}
		} else if f.wireType == 0 && len(path) == 1 {
			results = append(results, fmt.Sprintf("%d", f.intVal))
		}
	}
	return results
}

// getFirstField returns the first value at path, or "".
func getFirstField(data []byte, path []uint64) string {
	vals := getFields(data, path)
	if len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// ---------------------------------------------------------------------------
// Session ID extraction from step type 15
// Navigates: top-level field 5 → field 9 → field 8 (kv pair: field 1=key, field 2=value)
// Returns the value where key == "sessionID".
// ---------------------------------------------------------------------------

func extractSessionIDFromStep(payload []byte) (result string) {
	defer func() { recover() }() //nolint:errcheck // never panic

	fields, err := parseMessage(payload)
	if err != nil {
		return ""
	}
	for _, f := range fields {
		if f.fieldNum != 5 || f.wireType != 2 {
			continue
		}
		f5parsed, err := parseMessage(f.bytesVal)
		if err != nil {
			continue
		}
		for _, f9 := range f5parsed {
			if f9.fieldNum != 9 || f9.wireType != 2 {
				continue
			}
			f9parsed, err := parseMessage(f9.bytesVal)
			if err != nil {
				continue
			}
			for _, f8 := range f9parsed {
				if f8.fieldNum != 8 || f8.wireType != 2 {
					continue
				}
				kvparsed, err := parseMessage(f8.bytesVal)
				if err != nil {
					continue
				}
				var kvKey, kvVal string
				for _, kv := range kvparsed {
					if kv.wireType == 2 {
						switch kv.fieldNum {
						case 1:
							kvKey = string(kv.bytesVal)
						case 2:
							kvVal = string(kv.bytesVal)
						}
					}
				}
				if kvKey == "sessionID" {
					return kvVal
				}
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Output helpers
// ---------------------------------------------------------------------------

// errorOutput is the JSON shape for error responses.
type errorOutput struct {
	Ok        bool    `json:"ok"`
	Error     string  `json:"error"`
	ExitCode  *int    `json:"exit_code,omitempty"`
	AgyStderr *string `json:"agy_stderr,omitempty"`
	AgyStdout *string `json:"agy_stdout,omitempty"`
	DbPath    *string `json:"db_path,omitempty"`
	SessionID *string `json:"session_id,omitempty"`
}

func emitError(msg string, exitCode int, extra *errorOutput) {
	if extra == nil {
		extra = &errorOutput{}
	}
	extra.Ok = false
	extra.Error = msg
	if exitCode != 0 {
		ec := exitCode
		extra.ExitCode = &ec
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(extra)
	os.Exit(exitCode)
}

// successOutput is the JSON shape for successful responses.
type successOutput struct {
	Ok                bool    `json:"ok"`
	SessionID         string  `json:"session_id"`
	InternalSessionID *string `json:"internal_session_id"`
	Title             *string `json:"title"`
	Prompt            *string `json:"prompt"`
	Response          string  `json:"response"`
	Model             *string `json:"model"`
	Workspace         string  `json:"workspace"`
	DurationMs        int64   `json:"duration_ms"`
	ExitCode          int     `json:"exit_code"`
	DbPath            string  `json:"db_path"`
	Timestamp         string  `json:"timestamp"`
	ResumeWith        string  `json:"resume_with"`
	AgyStdout         *string `json:"agy_stdout,omitempty"`
	AgyStderr         *string `json:"agy_stderr,omitempty"`
	// Data-dir fields — omitted entirely when AGYJ_DATA_DIR is not set.
	Folder       string `json:"folder,omitempty"`
	ConvDir      string `json:"conv_dir,omitempty"`
	Turn         int    `json:"turn,omitempty"`
	TurnFile     string `json:"turn_file,omitempty"`
	DataDirError string `json:"data_dir_error,omitempty"`
}

// ---------------------------------------------------------------------------
// Model from settings.json
// ---------------------------------------------------------------------------

func modelFromSettings() *string {
	data, err := os.ReadFile(settingsFile())
	if err != nil {
		return nil
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil
	}
	if m, ok := settings["model"].(string); ok && m != "" {
		return &m
	}
	return nil
}

// ---------------------------------------------------------------------------
// SQLite helpers
// ---------------------------------------------------------------------------

type dbRow struct {
	idx      int
	stepType int
	payload  []byte
}

func queryDB(path string) ([]dbRow, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT idx, step_type, step_payload FROM steps ORDER BY idx")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []dbRow
	for rows.Next() {
		var r dbRow
		if err := rows.Scan(&r.idx, &r.stepType, &r.payload); err != nil {
			continue
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Run agy.exe, capturing stdout + stderr; return (exitCode, stdout, stderr, execErr).
// execErr is non-nil only for non-exit errors (e.g. timeout, exe not found).
// ---------------------------------------------------------------------------

type bytesWriter struct{ buf []byte }

func (w *bytesWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func runAgy(ctx context.Context, exe string, args []string) (int, []byte, []byte, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	stdout := &bytesWriter{}
	stderr := &bytesWriter{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			// Non-zero exit from agy is not a fatal Go error
			return exitCode, stdout.buf, stderr.buf, nil
		}
		// Other error: timeout, binary not found, etc.
		return -1, stdout.buf, stderr.buf, err
	}
	return exitCode, stdout.buf, stderr.buf, nil
}

// ---------------------------------------------------------------------------
// Helper: string pointer
// ---------------------------------------------------------------------------

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// stripEmpty returns nil if s is empty after trimming, else a pointer to the trimmed value.
func stripEmpty(b []byte) *string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------------------
// Data-dir feature helpers
// ---------------------------------------------------------------------------

const dotEnvTemplate = `# agyj configuration (process environment variables take precedence over this file)
# AGYJ_MODEL=Gemini 3.5 Flash (High)
# AGYJ_TIMEOUT=600
`

// parseDotEnv parses KEY=VALUE lines from an .env file.
// Blank lines and lines starting with # are skipped.
// Whitespace is trimmed; surrounding single/double quotes are stripped from values.
func parseDotEnv(path string) map[string]string {
	result := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return result
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// Strip surrounding quotes
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		result[key] = val
	}
	return result
}

// writeFileAtomic writes data to path atomically using a temp file + rename.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-agyj-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil {
		os.Remove(tmpName)
		return writeErr
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return closeErr
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// generateRandomID returns 12 lowercase hex characters from crypto/rand.
func generateRandomID() (string, error) {
	b := make([]byte, 6) // 6 bytes = 12 hex chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// readMapJSON reads the map.json file; returns empty map on missing/corrupt.
func readMapJSON(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return make(map[string]string)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return make(map[string]string)
	}
	return m
}

// writeMapJSON atomically writes the map to map.json.
func writeMapJSON(path string, m map[string]string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// nextTurnNumber scans dir for files matching ^\d+\.json$ and returns max+1 (min 1).
var turnFileRe = regexp.MustCompile(`^(\d+)\.json$`)

func nextTurnNumber(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 1
	}
	max := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := turnFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max + 1
}

// dataDirConfig holds resolved configuration from the data dir.
type dataDirConfig struct {
	model   string // empty = not set
	timeout int    // 0 = use default
}

// resolveDataDirConfig reads .env (if present) and merges with process env.
// Precedence: process env > .env > defaults.
func resolveDataDirConfig(dotEnvPath string) dataDirConfig {
	cfg := dataDirConfig{timeout: 0}

	dotEnvVars := parseDotEnv(dotEnvPath)

	// AGYJ_MODEL: process env > .env
	if v := os.Getenv("AGYJ_MODEL"); v != "" {
		cfg.model = v
	} else if v := dotEnvVars["AGYJ_MODEL"]; v != "" {
		cfg.model = v
	}

	// AGYJ_TIMEOUT: process env > .env > 600
	if v := os.Getenv("AGYJ_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.timeout = n
		}
	} else if v := dotEnvVars["AGYJ_TIMEOUT"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.timeout = n
		}
	}

	return cfg
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	args := os.Args[1:]

	// --- Detect interactive / no-args modes ---
	for _, a := range args {
		if a == "-i" || a == "--prompt-interactive" {
			emitError(
				"agyj wraps print mode only; run agy directly for interactive sessions",
				2, nil,
			)
		}
	}
	if len(args) == 0 {
		emitError(
			"agyj wraps print mode only; run agy directly for interactive sessions",
			2, nil,
		)
	}

	// --- Check AGYJ_DATA_DIR ---
	dataDirPath := strings.TrimSpace(os.Getenv("AGYJ_DATA_DIR"))
	dataDirActive := dataDirPath != ""

	// --- Initialise data dir early (MkdirAll + .env template) ---
	// We do this before running agy so config can be read.
	var dotEnvPath string
	var mapJSONPath string
	var dataDirInitErr string

	var dataCfg dataDirConfig
	timeoutSecs := 600 // default

	if dataDirActive {
		if err := os.MkdirAll(dataDirPath, 0755); err != nil {
			dataDirInitErr = fmt.Sprintf("MkdirAll %s: %v", dataDirPath, err)
			dataDirActive = false
		} else {
			dotEnvPath = filepath.Join(dataDirPath, ".env")
			mapJSONPath = filepath.Join(dataDirPath, "map.json")

			// Create .env template only if missing
			if _, err := os.Stat(dotEnvPath); os.IsNotExist(err) {
				if writeErr := os.WriteFile(dotEnvPath, []byte(dotEnvTemplate), 0644); writeErr != nil {
					// Non-fatal: record but continue
					dataDirInitErr = fmt.Sprintf("write .env template: %v", writeErr)
				}
			}

			dataCfg = resolveDataDirConfig(dotEnvPath)
			if dataCfg.timeout > 0 {
				timeoutSecs = dataCfg.timeout
			}
		}
	}

	// --- Determine whether -p/--print/--prompt is already present ---
	hasPrintFlag := false
	for _, a := range args {
		if a == "-p" || a == "--print" || a == "--prompt" {
			hasPrintFlag = true
			break
		}
	}

	var agyArgs []string
	if hasPrintFlag {
		agyArgs = append([]string(nil), args...)
	} else {
		// Treat last arg as the prompt, prepend -p
		prompt := args[len(args)-1]
		agyArgs = make([]string, 0, len(args)+1)
		agyArgs = append(agyArgs, args[:len(args)-1]...)
		agyArgs = append(agyArgs, "-p", prompt)
	}

	// Ensure print/headless mode does not auto-deny read_url, web_search, or file tools
	hasSkipPerm := false
	for _, a := range agyArgs {
		if a == "--dangerously-skip-permissions" {
			hasSkipPerm = true
			break
		}
	}
	if !hasSkipPerm {
		agyArgs = append([]string{"--dangerously-skip-permissions"}, agyArgs...)
	}

	// --- Extract --conversation <id> if present ---
	convID := ""
	convIDArgIdx := -1
	for i, a := range agyArgs {
		if (a == "--conversation" || a == "-c") && i+1 < len(agyArgs) {
			convID = agyArgs[i+1]
			convIDArgIdx = i + 1
			break
		}
	}

	// --- Resume translation: if convID is a key in map.json, replace with uuid ---
	if dataDirActive && convID != "" && mapJSONPath != "" {
		mapData := readMapJSON(mapJSONPath)
		if uuid, ok := mapData[convID]; ok {
			agyArgs[convIDArgIdx] = uuid
			convID = uuid
		}
	}

	// --- Extract --model value if present ---
	var model *string
	hasModelFlag := false
	for i, a := range agyArgs {
		if a == "--model" && i+1 < len(agyArgs) {
			m := agyArgs[i+1]
			model = &m
			hasModelFlag = true
			break
		}
	}
	if !hasModelFlag {
		// Apply AGYJ_MODEL from data-dir config or process env
		if dataDirActive && dataCfg.model != "" {
			model = strPtr(dataCfg.model)
			agyArgs = append([]string{"--model", dataCfg.model}, agyArgs...)
		} else if envModel := os.Getenv("AGYJ_MODEL"); envModel != "" {
			model = strPtr(envModel)
			agyArgs = append([]string{"--model", envModel}, agyArgs...)
		} else {
			model = modelFromSettings()
		}
	}

	// --- Run agy ---
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	agyExit, agyStdoutBytes, agyStderrBytes, runErr := runAgy(ctx, agyBin(), agyArgs)

	durationMs := time.Since(start).Milliseconds()

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			emitError(fmt.Sprintf("agy timed out after %d seconds", timeoutSecs), 1, nil)
		}
		emitError(fmt.Sprintf("failed to run agy.exe: %v", runErr), 1, nil)
	}

	agyStdoutStr := stripEmpty(agyStdoutBytes)
	agyStderrStr := stripEmpty(agyStderrBytes)

	// --- Find the conversation db ---
	convDir := conversationsDir()
	dbPath := ""

	if convID != "" {
		candidate := filepath.Join(convDir, convID+".db")
		if _, err := os.Stat(candidate); err == nil {
			dbPath = candidate
		}
	}

	if dbPath == "" {
		// agy.exe writes the conversation db asynchronously, so it can still be
		// in flight when the process exits — especially on a loaded box where the
		// flush lands behind other I/O. A single scan the instant agy returns
		// therefore fails intermittently with "could not find a conversation db",
		// which surfaces to the caller as a 502 on a request that actually
		// succeeded. Poll for a couple of seconds before giving up.
		cutoff := start.Add(-5 * time.Second)
		deadline := time.Now().Add(3 * time.Second)
		for {
			dbPath = newestConversationDB(convDir, cutoff)
			if dbPath != "" || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	if dbPath == "" {
		extra := &errorOutput{
			ExitCode:  intPtr(agyExit),
			AgyStderr: agyStderrStr,
			AgyStdout: agyStdoutStr,
		}
		emitError("could not find a conversation db modified since this run started", 1, extra)
	}

	// --- Derive session_id from filename ---
	sessionID := strings.TrimSuffix(filepath.Base(dbPath), ".db")

	// --- Parse the db ---
	var promptText *string
	var responseParts []string
	var title *string
	var internalSessionID *string
	lastType14Idx := -1

	dbRows, dbErr := queryDB(dbPath)
	if dbErr != nil {
		emitError(fmt.Sprintf("failed to open/query db: %v", dbErr), 1, nil)
	}

	for _, row := range dbRows {
		if row.payload == nil {
			continue
		}
		payload := row.payload

		func() {
			defer func() { recover() }() //nolint:errcheck // never panic on malformed payloads

			switch row.stepType {
			case 14:
				// User message: prompt at 19.2, fallback 19.3.1
				val := getFirstField(payload, []uint64{19, 2})
				if val == "" {
					val = getFirstField(payload, []uint64{19, 3, 1})
				}
				if val != "" {
					promptText = strPtr(val)
					lastType14Idx = row.idx
					responseParts = nil // reset response accumulator
				}

			case 15:
				// Model response: text at 20.1; internal session id at 5.9.8
				if internalSessionID == nil {
					s := extractSessionIDFromStep(payload)
					if s != "" {
						internalSessionID = strPtr(s)
					}
				}
				if row.idx > lastType14Idx {
					vals := getFields(payload, []uint64{20, 1})
					for _, v := range vals {
						if strings.TrimSpace(v) != "" {
							responseParts = append(responseParts, v)
						}
					}
				}

			case 23:
				// Conversation title at 30.4
				val := getFirstField(payload, []uint64{30, 4})
				if val != "" {
					title = strPtr(val)
				}
			}
		}()
	}

	responseText := strings.TrimSpace(strings.Join(responseParts, "\n\n"))

	if responseText == "" {
		extra := &errorOutput{
			ExitCode:  intPtr(agyExit),
			DbPath:    strPtr(dbPath),
			SessionID: strPtr(sessionID),
			AgyStderr: agyStderrStr,
			AgyStdout: agyStdoutStr,
		}
		emitError("no response text found in conversation db", 1, extra)
	}

	// --- Build success output ---
	workspace, _ := os.Getwd()
	timestamp := time.Now().Format(time.RFC3339)

	out := &successOutput{
		Ok:                true,
		SessionID:         sessionID,
		InternalSessionID: internalSessionID,
		Title:             title,
		Prompt:            promptText,
		Response:          responseText,
		Model:             model,
		Workspace:         workspace,
		DurationMs:        durationMs,
		ExitCode:          agyExit,
		DbPath:            dbPath,
		Timestamp:         timestamp,
		ResumeWith:        fmt.Sprintf(`agyj --conversation %s -p "<your next prompt>"`, sessionID),
		AgyStdout:         agyStdoutStr,
		AgyStderr:         agyStderrStr,
	}

	// --- Data-dir bookkeeping (only when active and response succeeded) ---
	if dataDirActive {
		bookkeepErr := func() string {
			if dataDirInitErr != "" {
				return dataDirInitErr
			}

			// Read map.json
			mapData := readMapJSON(mapJSONPath)

			// Find or create folder for this session
			randomID := ""
			for k, v := range mapData {
				if v == sessionID {
					randomID = k
					break
				}
			}

			if randomID == "" {
				var err error
				randomID, err = generateRandomID()
				if err != nil {
					return fmt.Sprintf("generate random id: %v", err)
				}
				// Create subfolder
				folderPath := filepath.Join(dataDirPath, randomID)
				if err := os.MkdirAll(folderPath, 0755); err != nil {
					return fmt.Sprintf("create conv folder %s: %v", folderPath, err)
				}
				// Add to map and write
				mapData[randomID] = sessionID
				if err := writeMapJSON(mapJSONPath, mapData); err != nil {
					return fmt.Sprintf("write map.json: %v", err)
				}
			}

			folderPath := filepath.Join(dataDirPath, randomID)
			// Ensure folder exists (might have been cleaned)
			if err := os.MkdirAll(folderPath, 0755); err != nil {
				return fmt.Sprintf("ensure conv folder %s: %v", folderPath, err)
			}

			// Compute next turn number
			turn := nextTurnNumber(folderPath)
			turnFileName := fmt.Sprintf("%03d.json", turn)
			turnFilePath := filepath.Join(folderPath, turnFileName)

			// Populate data-dir fields on the output
			out.Folder = randomID
			out.ConvDir = folderPath
			out.Turn = turn
			out.TurnFile = turnFilePath

			// Marshal and write turn file atomically
			jsonBytes, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return fmt.Sprintf("marshal turn JSON: %v", err)
			}
			// Append newline to match json.Encoder output
			jsonBytes = append(jsonBytes, '\n')

			if err := writeFileAtomic(turnFilePath, jsonBytes); err != nil {
				return fmt.Sprintf("write turn file %s: %v", turnFilePath, err)
			}

			return ""
		}()

		if bookkeepErr != "" {
			out.DataDirError = bookkeepErr
		}
	}

	// --- Emit JSON to stdout ---
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// newestConversationDB returns the most recently modified *.db under convDir
// whose mtime is at or after cutoff, or "" when there is none. Split out of the
// scan so the caller can poll: agy.exe's db write can land after the process
// exits, and a single scan turns that race into a spurious hard error.
func newestConversationDB(convDir string, cutoff time.Time) string {
	best := ""
	var bestMtime time.Time
	_ = filepath.WalkDir(convDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".db") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		if info.ModTime().After(bestMtime) {
			bestMtime = info.ModTime()
			best = path
		}
		return nil
	})
	return best
}
