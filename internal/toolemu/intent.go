// N13 / N23 — NLU intent-extractor for non-canonical tool calls.
//
// Cascade upstream's `SendUserCascadeMessage` proto has no OpenAI `tools[]`
// field. We inject tool definitions into the system prompt
// (additional_instructions_section) and instruct the model to emit
// `<tool_call>{...}</tool_call>` markup. GPT-5.x and Claude follow that
// contract reliably; GLM-4.7 / GLM-5.x / Kimi / Qwen often DON'T — they
// see the prompt, decide to call the tool, and emit it as natural-language
// NARRATION instead of the exact `<tool_call>` markup.
//
// Real probe captures (from Node-side scripts/probes/v2071-glm-kimi-tool-probe):
//
//   GLM-4.7  → "I should call the shell_exec function with the command
//               'echo HELLO_FROM_PROBE'."
//   GLM-5.1  → "I'll run the shell command as requested."  (no args!)
//   GPT-5.5  → "PROBE_V0270_1777751588"  (pure fabricated output)
//
// The first one carries enough signal to reconstruct the call; the second
// has the intent but no args; the third is hopeless. Layered extraction:
//
//   Layer 1 (highest confidence) — explicit invocation syntax:
//     "Let me run shell_command(command='echo HELLO')"
//     "function_call: shell_exec(\"echo HELLO\")"
//
//   Layer 2 — backtick-quoted name + value:
//     "I'll call `shell_exec` with command `echo HELLO`"
//     "use the `Read` function with file_path `/etc/hosts`"
//
//   Layer 3 — natural narrative (model "thinking out loud"):
//     "I should call the shell_exec function with the command 'echo HI'"
//     "Let me invoke the Read tool to read /etc/hosts"
//
// Each layer requires the extracted name to match a caller-declared tool.
// Layer 3 also requires the user prompt to plausibly want a tool call
// (action verbs in the most recent user message).
//
// CONSERVATIVE BY DESIGN: false-positive tool_calls drive agent loops
// to execute things the model didn't actually decide on. When in doubt,
// return nil. WINDSURFAPI_NLU_RECOVERY=0 disables the layer entirely.
package toolemu

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// IntentExtraction is one recovered tool call with provenance for logging.
type IntentExtraction struct {
	ID            string
	Name          string
	ArgumentsJSON string
	// Layer ∈ {"explicit-syntax", "backtick-quoted", "narrative"}; surfaced
	// in stats / log lines so operators can see WHICH layer fired and tune
	// the corresponding regex when GLM 5.x ships a new dialect.
	Layer      string
	Confidence float64
}

// ExtractIntent runs the 3-layer extraction over the model's text output.
// `tools` is the caller's declared tools[]; only names matching a declared
// tool are accepted. `userMsgTail` is the last user message (or "") and
// gates Layer 3 — pure narrative is only believed when the user actually
// asked for an action.
//
// Returns nil when WINDSURFAPI_NLU_RECOVERY=0, when text is empty, when
// no tools are declared, or when no layer fires.
func ExtractIntent(text string, tools []Tool, userMsgTail string) []IntentExtraction {
	if os.Getenv("WINDSURFAPI_NLU_RECOVERY") == "0" {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" || len(tools) == 0 {
		return nil
	}
	idx := indexTools(tools)
	if len(idx.names) == 0 {
		return nil
	}

	// Layer 1: explicit invocation syntax.
	if hits := extractExplicitSyntax(text, idx); len(hits) > 0 {
		return hits
	}
	// Layer 2: backtick-quoted name + arg.
	if hits := extractBacktickQuoted(text, idx); len(hits) > 0 {
		return hits
	}
	// Layer 3: narrative (gated on user-side action intent).
	if hasActionIntent(userMsgTail) {
		if hits := extractNarrative(text, idx); len(hits) > 0 {
			return hits
		}
	}
	return nil
}

// DetectToolIntentInNarrative is a lighter-weight check: returns the name
// of a tool the model is talking about, without trying to recover args.
// chat.go uses this to decide whether to ask the model for a retry with
// the canonical markup, vs accepting the response as a non-tool answer.
//
// Mirrors the Node-side fast path "did the model mention any tool name?"
// — used at the same site where we'd otherwise log "tool intent missed".
func DetectToolIntentInNarrative(text string, tools []Tool) string {
	if os.Getenv("WINDSURFAPI_NLU_RECOVERY") == "0" {
		return ""
	}
	idx := indexTools(tools)
	if len(idx.names) == 0 {
		return ""
	}
	low := strings.ToLower(text)
	// Pass 1: explicit name mention. The longer name wins on ambiguity
	// (`Read` vs `ReadFile` — caller declared one, narrative said one).
	var best string
	bestLen := 0
	for n := range idx.names {
		if strings.Contains(low, strings.ToLower(n)) {
			if len(n) > bestLen {
				best = n
				bestLen = len(n)
			}
		}
	}
	if best != "" {
		return best
	}
	// Pass 2: action-verb fallback. GLM-5.1 says "Let me list the files"
	// without naming Bash — if the caller declared exactly one tool whose
	// description matches that verb, return it.
	verbs := []string{
		"list the files", "list files", "list directory", "ls",
		"run the command", "run shell", "execute", "execute the command",
		"read the file", "read file", "open the file",
		"search for", "grep for", "find the",
	}
	for _, v := range verbs {
		if strings.Contains(low, v) {
			// Pick the single declared tool whose name suggests the verb,
			// or, if there's only one tool declared, return it.
			if len(idx.names) == 1 {
				for n := range idx.names {
					return n
				}
			}
			return ""
		}
	}
	return ""
}

// ─── Tool indexing ────────────────────────────────────────────

type toolIndex struct {
	names        map[string]struct{}
	primaryParam map[string]string // tool name → first required string param
}

func indexTools(tools []Tool) toolIndex {
	idx := toolIndex{
		names:        map[string]struct{}{},
		primaryParam: map[string]string{},
	}
	for _, t := range tools {
		if t.Type != "function" || t.Function.Name == "" {
			continue
		}
		idx.names[t.Function.Name] = struct{}{}
		if len(t.Function.Parameters) == 0 {
			continue
		}
		var schema struct {
			Type       string                     `json:"type"`
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(t.Function.Parameters, &schema); err != nil {
			continue
		}
		if schema.Type != "object" {
			continue
		}
		// Prefer the first required string param.
		var primary string
		for _, r := range schema.Required {
			raw, ok := schema.Properties[r]
			if !ok {
				continue
			}
			var p struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &p) == nil && p.Type == "string" {
				primary = r
				break
			}
		}
		if primary == "" {
			// Fall back to first declared string property.
			for k, raw := range schema.Properties {
				var p struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(raw, &p) == nil && p.Type == "string" {
					primary = k
					break
				}
			}
		}
		if primary != "" {
			idx.primaryParam[t.Function.Name] = primary
		}
	}
	return idx
}

// ─── Layer 1 — explicit syntax ────────────────────────────────

// reExplicit matches `func_name(...)` and `function_call: func_name(...)`
// forms. Group 1 = name, group 2 = body (between parens, may include
// nested quoted strings; we extract values heuristically below).
var reExplicit = regexp.MustCompile(
	`(?:function_call:\s*)?` +
		`([A-Za-z_][A-Za-z0-9_]{1,64})` + // tool name (≤64 chars)
		`\s*\(([^)]*)\)`)

func extractExplicitSyntax(text string, idx toolIndex) []IntentExtraction {
	matches := reExplicit.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []IntentExtraction
	for _, m := range matches {
		name := m[1]
		body := m[2]
		if _, ok := idx.names[name]; !ok {
			continue
		}
		args := parseExplicitArgs(body)
		// If we got nothing parseable, fall back to the primary-param
		// shorthand: pass the whole body as the primary string arg.
		if len(args) == 0 && body != "" {
			if pp, ok := idx.primaryParam[name]; ok {
				args = map[string]any{pp: strings.TrimSpace(strings.Trim(body, `"'`))}
			}
		}
		argJSON, err := marshalArgs(args)
		if err != nil {
			continue
		}
		out = append(out, IntentExtraction{
			ID:            fmt.Sprintf("call_nlu_%d", len(out)),
			Name:          name,
			ArgumentsJSON: argJSON,
			Layer:         "explicit-syntax",
			Confidence:    0.9,
		})
	}
	return out
}

// parseExplicitArgs tries to parse `key=value` / `key="value"` / `value` (positional).
// Returns map[string]any with the parsed pairs.
var reKVPair = regexp.MustCompile(
	`([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^,\s][^,]*))`)

func parseExplicitArgs(body string) map[string]any {
	out := map[string]any{}
	body = strings.TrimSpace(body)
	if body == "" {
		return out
	}
	for _, m := range reKVPair.FindAllStringSubmatch(body, -1) {
		key := m[1]
		var val string
		switch {
		case m[2] != "":
			val = m[2]
		case m[3] != "":
			val = m[3]
		case m[4] != "":
			val = strings.TrimSpace(m[4])
		}
		out[key] = val
	}
	return out
}

// ─── Layer 2 — backtick-quoted ────────────────────────────────

// reBacktickName matches `tool` and `tool_name`. Group 1 = name.
var reBacktickName = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]{1,64})`")

// reBacktickArg matches `key` `value` or `key`: `value`. Used after the
// name to harvest argument pairs in the same sentence.
var reBacktickArg = regexp.MustCompile(
	"`([A-Za-z_][A-Za-z0-9_]{0,32})`\\s*[:=]?\\s*`([^`]+)`")

func extractBacktickQuoted(text string, idx toolIndex) []IntentExtraction {
	matches := reBacktickName.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []IntentExtraction
	for _, m := range matches {
		nameStart := m[2]
		nameEnd := m[3]
		name := text[nameStart:nameEnd]
		if _, ok := idx.names[name]; !ok {
			continue
		}
		// Look at the next ~300 chars for backtick KV pairs, but don't
		// cross a line break — narrative often resets context per sentence.
		end := nameEnd + 300
		if end > len(text) {
			end = len(text)
		}
		segment := text[nameEnd:end]
		if nl := strings.IndexByte(segment, '\n'); nl != -1 {
			segment = segment[:nl]
		}
		args := map[string]any{}
		for _, kv := range reBacktickArg.FindAllStringSubmatch(segment, -1) {
			// Skip if "key" is also the tool name (common: `Bash` then `command`).
			if kv[1] == name {
				continue
			}
			args[kv[1]] = kv[2]
		}
		argJSON, err := marshalArgs(args)
		if err != nil {
			continue
		}
		out = append(out, IntentExtraction{
			ID:            fmt.Sprintf("call_nlu_%d", len(out)),
			Name:          name,
			ArgumentsJSON: argJSON,
			Layer:         "backtick-quoted",
			Confidence:    0.7,
		})
	}
	return out
}

// ─── Layer 3 — narrative ──────────────────────────────────────

// reNarrative matches "I should call the X function/tool with the/a Y 'Z'"
// and "Let me invoke the X tool to ..." patterns. Reasonable false-positive
// rate, hence the action-intent gate on the user side.
var reNarrative = regexp.MustCompile(
	`(?i)\b(?:I (?:should|will|can|need to|am going to)|let me|I'll|i\?ll)\s+` +
		`(?:call|use|invoke|run|execute|leverage)\s+` +
		`(?:the\s+)?` +
		`([A-Za-z_][A-Za-z0-9_]{1,64})\b` +
		`(?:\s+(?:function|tool|command))?` +
		`(?:\s+(?:with|using|on|to)\s+` +
		`(?:the\s+)?(?:command|argument|param|input|value|file|query|path|args)\s*` +
		`['"]([^'"]+)['"]` +
		`)?`)

func extractNarrative(text string, idx toolIndex) []IntentExtraction {
	matches := reNarrative.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []IntentExtraction
	for _, m := range matches {
		name := m[1]
		if _, ok := idx.names[name]; !ok {
			continue
		}
		args := map[string]any{}
		if m[2] != "" {
			if pp, ok := idx.primaryParam[name]; ok {
				args[pp] = m[2]
			}
		}
		argJSON, err := marshalArgs(args)
		if err != nil {
			continue
		}
		out = append(out, IntentExtraction{
			ID:            fmt.Sprintf("call_nlu_%d", len(out)),
			Name:          name,
			ArgumentsJSON: argJSON,
			Layer:         "narrative",
			Confidence:    0.5,
		})
	}
	return out
}

// hasActionIntent gates Layer 3. Returns true when the user message contains
// an action verb that suggests a tool call is appropriate.
func hasActionIntent(userMsgTail string) bool {
	if userMsgTail == "" {
		return false
	}
	low := strings.ToLower(userMsgTail)
	verbs := []string{
		"run", "execute", "list", "ls ", "cat ", "show me",
		"read", "open ", "what's in", "what is in",
		"check ", "test ", "search", "grep", "find",
		"create", "write ", "make ",
		"explore", "look at",
	}
	for _, v := range verbs {
		if strings.Contains(low, v) {
			return true
		}
	}
	return false
}

// marshalArgs JSON-encodes the argument map. Empty map → "{}" (not null).
func marshalArgs(args map[string]any) (string, error) {
	if args == nil {
		return "{}", nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	if string(b) == "null" {
		return "{}", nil
	}
	return string(b), nil
}
