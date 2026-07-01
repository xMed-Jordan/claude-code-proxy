package main

// camera_log.go — observability for the camera subsystem (a hard requirement).
//
// camlog(level, op, fields) does two things for every camera operation:
//  1. emits a single `[cameras]`-tagged structured line to stdout (the background
//     proxy redirects stdout into .proxy.log, which the control panel Logs view
//     reads), and
//  2. best-effort appends a queryable row to the camera_events table — the
//     diagnostics trail used by /health and the Diagnostics panel.
//
// Verbosity is gated by PROXY_CAMERA_LOG_LEVEL (debug|info|warn|error, default
// info). CREDENTIALS MUST NEVER be passed in fields; pass any device URL through
// maskCredsURL first so a password can never reach the log or the events table.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var camLogLevelOrder = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}

// camLogThreshold resolves the minimum level that is emitted. Reads the captured
// cfg first (set by initCameras), then the raw env (so logging works before init
// and in unit tests). Defaults to info.
func camLogThreshold() int {
	lvl := strings.ToLower(strings.TrimSpace(cameraCfg.CameraLogLevel))
	if lvl == "" {
		lvl = strings.ToLower(strings.TrimSpace(os.Getenv("PROXY_CAMERA_LOG_LEVEL")))
	}
	if v, ok := camLogLevelOrder[lvl]; ok {
		return v
	}
	return camLogLevelOrder["info"]
}

// camlog emits a leveled [cameras] line and appends a camera_events row. Both are
// gated by PROXY_CAMERA_LOG_LEVEL so debug bring-up chatter can be turned up/down
// without code changes; warn/error always pass the default threshold.
func camlog(level, op string, fields map[string]any) {
	level = strings.ToLower(strings.TrimSpace(level))
	if _, ok := camLogLevelOrder[level]; !ok {
		level = "info"
	}
	if camLogLevelOrder[level] < camLogThreshold() {
		return
	}
	fmt.Fprintln(os.Stdout, formatCamLogLine(level, op, fields))
	appendCameraEvent(level, op, fields)
}

// formatCamLogLine renders a deterministic key=value line (keys sorted) prefixed
// with the [cameras] tag, timestamp, level and op.
func formatCamLogLine(level, op string, fields map[string]any) string {
	var b strings.Builder
	b.WriteString("[cameras] ")
	b.WriteString(time.Now().Format(time.RFC3339))
	b.WriteString(" level=")
	b.WriteString(level)
	b.WriteString(" op=")
	b.WriteString(camLogValue(op))
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(" ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(camLogValue(fields[k]))
	}
	return b.String()
}

// camLogValue renders a field value for the log line, quoting anything with
// whitespace/quotes and JSON-encoding composite values.
func camLogValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		if x == "" || strings.ContainsAny(x, " \t\"\n") {
			return strconv.Quote(x)
		}
		return x
	case error:
		return strconv.Quote(x.Error())
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case time.Duration:
		return x.String()
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return strconv.Quote(fmt.Sprint(v))
		}
		return string(b)
	}
}

// camEventFields is the extracted, typed shape of a camera_events row.
type camEventFields struct {
	ts        string
	dvrID     string
	cameraID  string
	watchID   string
	runID     string
	op        string
	ok        int
	latencyMs int64
	detail    string
	errStr    string
}

// camEventFromFields pulls the well-known columns out of a fields map and folds
// everything else into the JSON detail blob. ok defaults to true unless an
// explicit ok=false is passed or the level is "error".
func camEventFromFields(level, op string, fields map[string]any) camEventFields {
	row := camEventFields{ts: time.Now().Format(time.RFC3339), op: op, ok: 1}
	detail := map[string]any{}
	for k, v := range fields {
		switch k {
		case "dvr_id":
			row.dvrID = camFieldStr(v)
		case "camera_id":
			row.cameraID = camFieldStr(v)
		case "watch_id":
			row.watchID = camFieldStr(v)
		case "run_id":
			row.runID = camFieldStr(v)
		case "op":
			// op is a first-class column; ignore any duplicate in fields.
		case "ok":
			if b, isBool := v.(bool); isBool {
				row.ok = boolToInt(b)
			}
		case "latency_ms":
			row.latencyMs = camFieldInt64(v)
		case "error", "err":
			row.errStr = camFieldStr(v)
		default:
			detail[k] = v
		}
	}
	if _, hasOK := fields["ok"]; !hasOK && level == "error" {
		row.ok = 0
	}
	if len(detail) > 0 {
		if b, err := json.Marshal(detail); err == nil {
			row.detail = string(b)
		}
	}
	return row
}

// appendCameraEvent writes one camera_events row via a short-lived DB handle
// (mirrors the fresh-openProxyDB handler idiom). Best-effort: any error is
// swallowed so logging never breaks an operation.
func appendCameraEvent(level, op string, fields map[string]any) {
	db, err := openProxyDB(cameraCfg)
	if err != nil {
		return
	}
	defer db.Close()
	e := camEventFromFields(level, op, fields)
	_, _ = db.Exec(`INSERT INTO camera_events
		(id, ts, level, dvr_id, camera_id, watch_id, run_id, op, ok, latency_ms, detail, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		randomID("cev"), e.ts, level, e.dvrID, e.cameraID, e.watchID, e.runID,
		e.op, e.ok, e.latencyMs, e.detail, e.errStr)
}

func camFieldStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func camFieldInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case time.Duration:
		return int64(x / time.Millisecond)
	default:
		return 0
	}
}

// maskCredsURL returns a log-safe rendering of a (possibly credential-bearing)
// URL: any userinfo is replaced with "user:***". Use this for EVERY device URL
// that appears in a log field or error message.
func maskCredsURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			u.User = url.UserPassword(u.User.Username(), "***")
		} else if u.User.Username() != "" {
			u.User = url.User(u.User.Username())
		}
		return u.String()
	}
	// Fallback for URLs url.Parse can't handle (odd chars in creds): manually
	// mask the "user:pass@" between "://" and the authority terminator.
	return camMaskInlineCreds(raw)
}

func camMaskInlineCreds(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return raw
	}
	// A '/' (or '?') before the '@' means the '@' is in the path, not userinfo.
	if term := strings.IndexAny(rest, "/?#"); term >= 0 && term < at {
		return raw
	}
	cred := rest[:at]
	user := cred
	if c := strings.Index(cred, ":"); c >= 0 {
		user = cred[:c]
	}
	return raw[:i+3] + user + ":***@" + rest[at+1:]
}
