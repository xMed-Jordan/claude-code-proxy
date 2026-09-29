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

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// agyPersonaAgentFor returns the name of the persona agent that should serve
// this generation, or "" when the persona stays in the message: the switch is
// off, there is no persona, the run carries attachments (media runs use other
// agents and keep the persona in the message), no chat agent is configured
// (an empty PROXY_AGY_AGENT means agy's default coding agent, on purpose), or the
// agent could not be installed.
func agyPersonaAgentFor(cfg config, in agyGenInput) string {
	if !cfg.AgyPersonaInAgent || strings.TrimSpace(in.System) == "" || len(in.Media) != 0 || strings.TrimSpace(cfg.AgyAgent) == "" {
		return ""
	}
	name, err := ensureAgyPersonaAgent(cfg, in.System)
	if err != nil {
		log.Printf("[agy] persona agent unavailable (%v); the persona travels in the message", err)
		return ""
	}
	return name
}
