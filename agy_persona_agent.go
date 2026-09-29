package main

// agy_persona_agent.go — the caller's persona delivered as an agy agent
// definition instead of inside the prompt message (PROXY_AGY_PERSONA_IN_AGENT,
// OFF by default).
//
// Why. agy silently cuts the prompt MESSAGE at about 191KB, and the caller's
// system prompt (the "persona", ~136KB for Connect's agents) is the biggest part
// of it, so it crowds out the conversation and the tool results the turn is
// acting on. The body of an agent definition
// (~/.gemini/config/agents/<name>/agent.md) is delivered to the model whole and
// does not count against that cut. So, with the switch on, a plain chat turn
// runs under a per-persona agent — `--agent <name>`, found ONLY in the user-level
// agents directory (agyAgentsDir) — whose body is a short instruction followed by
// the persona verbatim, and the message carries a one-line pointer instead of the
// persona (renderAgyPromptFittedTranscript).
//
// The name is derived from the definition's content, so an unchanged persona is
// installed once and a changed one gets a new agent of its own; nothing is ever
// edited in place under a running request.
//
// Only plain chat turns qualify (agyPersonaAgentFor): a run with attachments uses
// other agents, and an empty PROXY_AGY_AGENT means agy's default coding agent is
// configured on purpose. Anything that goes wrong installing the agent falls back
// to today's behaviour: the persona travels in the message.
//
// Who gets it. The switch is a rollout, so it also needs an allowlist of customer
// numbers (PROXY_AGY_PERSONA_IN_AGENT_NUMBERS): with the switch on, only a
// conversation whose number is listed (or any conversation, with "*") takes the
// agent path; everyone else keeps path A, the prompt exactly as it is with the
// switch off. The number is read from the "User identifier:" line of the "##
// Platform Context" section Connect writes into the system prompt
// (agyConversationIdentifier) and nowhere else: the persona itself is full of
// other numbers (tool docs, examples). An empty list means no one.
//
// Every call made with the switch on logs exactly one [agy-persona] line: which
// path the turn took and why, and a short hash of the number (never the number,
// never the persona). With the switch off nothing is logged and nothing is read.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// agyPersonaAgentDefaultPrefix is the name prefix of persona agents
// (PROXY_AGY_PERSONA_AGENT_PREFIX).
const agyPersonaAgentDefaultPrefix = "connect-chat-p"

// agyPersonaAgentInstructions is the text between the frontmatter and the
// persona in a persona agent's body.
const agyPersonaAgentInstructions = "# Instructions\nYou are the assistant defined by the SYSTEM INSTRUCTIONS & POLICIES below; they are your own standing instructions. Each user message is one chat turn request: it contains the current state, the tools you may call (described in the message, invoked ONLY by writing <tool_call> blocks in your reply), the conversation so far, and what to produce next. You have no files, shell, browser, web or other capabilities of your own; never attempt to read, search or run anything. Produce exactly the assistant's next turn as the message specifies, and nothing else.\n\n# SYSTEM INSTRUCTIONS & POLICIES\n\n"

// What the prompt message says where the persona would have been.
const (
	// agyPersonaPointer replaces the persona in the SYSTEM INSTRUCTIONS &
	// POLICIES section of the message.
	agyPersonaPointer = "Your system instructions and policies are your own standing agent instructions; they are not repeated in this message. Follow them exactly.\n\n"
	// agyPersonaHowToReadSystem replaces "the assistant's SYSTEM INSTRUCTIONS &
	// POLICIES (its identity and rules)" in the HOW TO READ THIS MESSAGE section.
	agyPersonaHowToReadSystem = "a SYSTEM INSTRUCTIONS & POLICIES pointer (its identity and rules are its own agent instructions)"
)

// agyPersonaAgentFrontmatter is the frontmatter of a persona agent: the key set
// of agyChatAgentDefinition (agy 1.1.27), under the persona agent's own name.
func agyPersonaAgentFrontmatter(name string) string {
	return "---\n" +
		"name: " + name + "\n" +
		"description: connect-ai-proxy chat agent carrying one caller persona. No built-in tools.\n" +
		"mainAgent: true\n" +
		"subagent: false\n" +
		"inheritMcp: false\n" +
		"tools: []\n" +
		"---\n"
}

// agyPersonaAgentDefinition is the whole agent.md of a persona agent: the
// frontmatter, the instructions, then the persona verbatim (trimmed of
// surrounding whitespace) and a final newline.
func agyPersonaAgentDefinition(name, persona string) string {
	return agyPersonaAgentFrontmatter(name) + agyPersonaAgentInstructions + strings.TrimSpace(persona) + "\n"
}

// agyPersonaAgentName is prefix-<first 12 hex chars of sha256(instructions +
// persona)>. The persona is hashed as it is written into the definition (trimmed),
// so two spellings of the same persona share one agent. A prefix that would not
// be a safe directory name and CLI argument is replaced by the default one.
func agyPersonaAgentName(prefix, persona string) string {
	sum := sha256.Sum256([]byte(agyPersonaAgentInstructions + strings.TrimSpace(persona)))
	return agyPersonaAgentSafePrefix(prefix) + "-" + hex.EncodeToString(sum[:])[:12]
}

// agyPersonaAgentSafePrefix keeps a configured prefix to [A-Za-z0-9._-], not
// starting with "-" or ".", because it becomes a directory name under the agents
// directory and the value of `--agent`.
func agyPersonaAgentSafePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || strings.HasPrefix(prefix, "-") || strings.HasPrefix(prefix, ".") {
		return agyPersonaAgentDefaultPrefix
	}
	for _, r := range prefix {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return agyPersonaAgentDefaultPrefix
		}
	}
	return prefix
}

var (
	// agyPersonaAgentMu serialises installs, so two requests carrying the same new
	// persona do not race each other over the same files.
	agyPersonaAgentMu sync.Mutex
	// agyPersonaAgentVerified holds the agent.md paths already found in place with
	// the right content by this process, so a 136KB file is not read back on every
	// request. Keyed by path, not name: the agents directory follows the home
	// directory (and the run-as user), which can differ.
	agyPersonaAgentVerified sync.Map
)

// ensureAgyPersonaAgent makes sure the persona's agent is installed in the
// user-level agents directory and returns its name. A file already there with
// byte-identical content is left alone; otherwise it is written atomically (a
// temp file in the same directory, renamed into place). On any error the caller
// falls back to sending the persona in the message.
func ensureAgyPersonaAgent(cfg config, persona string) (string, error) {
	persona = strings.TrimSpace(persona)
	if persona == "" {
		return "", errors.New("empty persona")
	}
	name := agyPersonaAgentName(cfg.AgyPersonaAgentPrefix, persona)
	dir, err := agyAgentsDir()
	if err != nil {
		return "", err
	}
	agentDir := filepath.Join(dir, name)
	path := filepath.Join(agentDir, "agent.md")
	definition := agyPersonaAgentDefinition(name, persona)

	if _, ok := agyPersonaAgentVerified.Load(path); ok {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Size() == int64(len(definition)) {
			return name, nil
		}
		agyPersonaAgentVerified.Delete(path) // gone or changed underneath us: verify again
	}

	agyPersonaAgentMu.Lock()
	defer agyPersonaAgentMu.Unlock()
	if cur, err := os.ReadFile(path); err == nil && string(cur) == definition {
		agyPersonaAgentVerified.Store(path, struct{}{})
		return name, nil
	}
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", agentDir, err)
	}
	tmp, err := os.CreateTemp(agentDir, ".agent.md.tmp-*")
	if err != nil {
		return "", fmt.Errorf("cannot create a temp file in %s: %w", agentDir, err)
	}
	tmpPath := tmp.Name()
	_, werr := tmp.WriteString(definition)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("cannot write %s: %w", tmpPath, errors.Join(werr, cerr))
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("cannot chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("cannot install %s: %w", path, err)
	}
	agyChownAgentDir(agentDir)
	agyPersonaAgentVerified.Store(path, struct{}{})
	log.Printf("[agy] installed persona agent %s (%d bytes)", name, len(definition))
	return name, nil
}

// ---------------------------------------------------------------------------
// Whose conversation: the allowlist
// ---------------------------------------------------------------------------

// agyPersonaMinSuffixDigits is how many digits the shorter of two numbers needs
// before it may match by being the tail of the longer one: a local form
// ("790000001") matches the international one ("962790000001"), a fragment does not.
const agyPersonaMinSuffixDigits = 8

var (
	// agyUserIdentifierRe matches the "User identifier:" line of Connect's Platform
	// Context section and captures its value. Only spaces and tabs may sit between
	// the colon and the value, so a line with an empty value can never take the
	// next line for its value.
	agyUserIdentifierRe = regexp.MustCompile(`(?m)^[ \t]*User identifier:[ \t]*(.*?)[ \t\r]*$`)
	// agyPlatformContextRe matches the "## Platform Context" heading.
	agyPlatformContextRe = regexp.MustCompile(`(?m)^[ \t]*#{1,6}[ \t]*Platform Context[ \t\r]*$`)
)

// agyNormalizePhone reduces a number to a comparable form: digits only; a leading
// international "00" dropped; then leading zeros (the local form) stripped. So
// "+962 79-000-0001", "00962790000001" and "962790000001" are one value, and
// "0790000001" is its local tail "790000001".
func agyNormalizePhone(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			b.WriteByte(c)
		}
	}
	return strings.TrimLeft(strings.TrimPrefix(b.String(), "00"), "0")
}

// agyPhoneMatches reports whether two NORMALISED numbers (agyNormalizePhone) are
// the same customer: equal, or the shorter is the tail of the longer and has at
// least agyPersonaMinSuffixDigits digits. An empty value matches nothing.
func agyPhoneMatches(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	return len(short) >= agyPersonaMinSuffixDigits && strings.HasSuffix(long, short)
}

// agyConversationIdentifier returns the value of the "User identifier:" line of
// the system prompt's Platform Context section, trimmed ("" when there is none):
//
//	## Platform Context
//	Platform: whatsapp
//	Speaking with: <name>
//	User identifier: <number>
//
// The first such line after a "## Platform Context" heading; with no such heading
// the first such line anywhere. With a heading but no line after it the answer is
// "" — the persona's own examples are never a stand-in. The persona carries other
// numbers (tool docs, examples); none of them is ever read.
func agyConversationIdentifier(system string) string {
	if !strings.Contains(system, "User identifier:") {
		return ""
	}
	scope := system
	if loc := agyPlatformContextRe.FindStringIndex(system); loc != nil {
		scope = system[loc[1]:]
	}
	if m := agyUserIdentifierRe.FindStringSubmatch(scope); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// agyParsePersonaNumbers reads PROXY_AGY_PERSONA_IN_AGENT_NUMBERS: a
// comma-separated list; each entry is normalised (agyNormalizePhone) and entries
// that carry no digits are dropped; an entry "*" sets all. Empty means no one.
func agyParsePersonaNumbers(raw string) (numbers []string, all bool) {
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			all = true
			continue
		}
		n := agyNormalizePhone(part)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		numbers = append(numbers, n)
	}
	return numbers, all
}

// agyPersonaNumberListed reports whether the NORMALISED number matches any entry
// of the allowlist.
func agyPersonaNumberListed(numbers []string, id string) bool {
	for _, n := range numbers {
		if agyPhoneMatches(n, id) {
			return true
		}
	}
	return false
}

// agyPersonaIDTag is what the log says about a customer instead of the number:
// the first 8 hex chars of sha256 of the normalised number, "-" when there is none.
func agyPersonaIDTag(id string) string {
	if id == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:8]
}

// agyPersonaAgentFor returns the name of the persona agent that should serve
// this generation, or "" when the persona stays in the message (path A): the
// switch is off, there is no persona, the run carries attachments (media runs use
// other agents and keep the persona in the message), no chat agent is configured
// (an empty PROXY_AGY_AGENT means agy's default coding agent, on purpose), the
// conversation's number is not on the allowlist (PROXY_AGY_PERSONA_IN_AGENT_NUMBERS;
// "*" lists everyone, an empty list no one), or the agent could not be installed.
//
// With the switch on it logs exactly one [agy-persona] line per call, naming the
// path taken and a hash of the number; never the number, never the persona. With
// the switch off it logs nothing and reads nothing.
func agyPersonaAgentFor(cfg config, in agyGenInput) string {
	if !cfg.AgyPersonaInAgent {
		return ""
	}
	id := agyNormalizePhone(agyConversationIdentifier(in.System))
	tag := agyPersonaIDTag(id)
	pathA := func(reason string) string {
		log.Printf("[agy-persona] path=A reason=%s id#%s", reason, tag)
		return ""
	}
	switch {
	case strings.TrimSpace(in.System) == "":
		return pathA("no-persona")
	case len(in.Media) != 0:
		return pathA("media")
	case strings.TrimSpace(cfg.AgyAgent) == "":
		return pathA("no-chat-agent")
	}
	if !cfg.AgyPersonaInAgentAll {
		if id == "" {
			return pathA("no-identifier")
		}
		if !agyPersonaNumberListed(cfg.AgyPersonaInAgentNumbers, id) {
			return pathA("not-allowlisted")
		}
	}
	name, err := ensureAgyPersonaAgent(cfg, in.System)
	if err != nil {
		log.Printf("[agy] persona agent unavailable (%v); the persona travels in the message", err)
		return pathA("install-error")
	}
	log.Printf("[agy-persona] path=B agent=%s id#%s", name, tag)
	return name
}
