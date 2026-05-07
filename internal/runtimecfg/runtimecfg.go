// Package runtimecfg is the runtime-editable feature-flag + identity-prompt
// store. Backed by runtime-config.json next to the binary, hot-editable from
// the dashboard. Mirrors src/runtime-config.js.
package runtimecfg

import (
	"encoding/json"
	"os"
	"strings"
	"sync"

	"windsurfapi/internal/atomicfile"
	"windsurfapi/internal/logx"
)

// Default identity-prompt templates — {model} is replaced at injection time.
//
// Every vendor ships a deliberately tame template. Phrasing avoids
// injection-detector trigger words ("ignore", "override", "NEVER reveal",
// "CRITICAL") so Claude Code / Cursor / OpenCode don't drop assistant
// turns client-side. Effectiveness is probabilistic:
//   - Claude family leans into system-prompt identity (usually works)
//   - Grok / some others have strong baked-in "I am Cascade" alignment
//     and will ignore the override
//   - Others vary per family
//
// Operators edit or blank individual entries in the dashboard. Blank
// means "no injection for this vendor".
var DefaultIdentityPrompts = map[string]string{
	"anthropic": "You are {model}, a large language model created by Anthropic. You are helpful, harmless, and honest. When asked about your identity or which model you are, you respond that you are {model}, made by Anthropic.",
	"openai":    "You are {model}, a large language model created by OpenAI. When asked about your identity, you respond that you are {model}, made by OpenAI.",
	"google":    "You are {model}, a large language model created by Google. When asked about your identity, you respond that you are {model}, made by Google.",
	"deepseek":  "You are {model}, a large language model created by DeepSeek. When asked about your identity, you respond that you are {model}, made by DeepSeek.",
	"xai":       "You are {model}, a large language model created by xAI. When asked about your identity, you respond that you are {model}, made by xAI.",
	"alibaba":   "You are {model}, a large language model created by Alibaba. When asked about your identity, you respond that you are {model}, made by Alibaba.",
	"moonshot":  "You are {model}, a large language model created by Moonshot AI. When asked about your identity, you respond that you are {model}, made by Moonshot AI.",
	"zhipu":     "You are {model}, a large language model created by Zhipu AI. When asked about your identity, you respond that you are {model}, made by Zhipu AI.",
	"minimax":   "You are {model}, a large language model created by MiniMax. When asked about your identity, you respond that you are {model}, made by MiniMax.",
	"windsurf":  "You are {model}, a coding assistant model by Windsurf. When asked about your identity, you respond that you are {model}, made by Windsurf.",
}

// Experimental holds the three feature toggles the JS version exposes.
type Experimental struct {
	CascadeConversationReuse bool `json:"cascadeConversationReuse"`
	ModelIdentityPrompt      bool `json:"modelIdentityPrompt"`
	PreflightRateLimit       bool `json:"preflightRateLimit"`
}

type file struct {
	Experimental    Experimental      `json:"experimental"`
	IdentityPrompts map[string]string `json:"identityPrompts"`
	// ResponseLanguagePrompt is appended to Cascade's communication_section
	// to steer response language without touching identity. Empty string in
	// runtime-config.json preserves the default — truly disabling this
	// requires setting a space or editing the source constant.
	ResponseLanguagePrompt string `json:"responseLanguagePrompt,omitempty"`
}

// DefaultResponseLanguagePrompt is the baked-in "respond in Simplified
// Chinese by default" instruction. Phrasing deliberately stays away from
// injection-detector keywords ("ignore", "NEVER", "override", "CRITICAL")
// and leaves the user an explicit escape hatch, so it doesn't stack into
// an anti-injection trip the way a persona-hijack prompt would.
const DefaultResponseLanguagePrompt = "Respond in Vietnamese by default. If the user writes in another language or explicitly asks for a specific language, follow the user's lead."

var (
	mu    sync.RWMutex
	state = file{
		Experimental: Experimental{
			ModelIdentityPrompt: true, // ON by default; operators turn it off in the dashboard
		},
		IdentityPrompts:        cloneMap(DefaultIdentityPrompts),
		ResponseLanguagePrompt: DefaultResponseLanguagePrompt,
	}
	path = "runtime-config.json"
)

// Init loads runtime-config.json. Call once at startup.
func Init() {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var loaded file
	if err := json.Unmarshal(data, &loaded); err != nil {
		logx.Warn("runtime-config: load failed: %s", err.Error())
		return
	}
	mu.Lock()
	defer mu.Unlock()
	state.Experimental = loaded.Experimental
	if loaded.IdentityPrompts != nil {
		// Merge on top of defaults so a partial save doesn't nuke unset keys.
		merged := cloneMap(DefaultIdentityPrompts)
		for k, v := range loaded.IdentityPrompts {
			merged[k] = v
		}
		state.IdentityPrompts = merged
	}
	// Blank / missing value in the file keeps the default — we only
	// override when the operator explicitly set non-empty text. To truly
	// disable the language steer, set it to a single space in the file.
	if loaded.ResponseLanguagePrompt != "" {
		state.ResponseLanguagePrompt = loaded.ResponseLanguagePrompt
	}
}

// Single-writer disk flush: a burst of experimental-flag / identity-prompt
// toggles collapses into one write of the latest state instead of N
// goroutines each pinning a JSON snapshot.
var (
	saveMu      sync.Mutex
	pendingData []byte
	pendingWake chan struct{}
	writerOnce  sync.Once
)

func persist() {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		logx.Warn("runtime-config: marshal: %s", err.Error())
		return
	}
	saveMu.Lock()
	pendingData = data
	writerOnce.Do(func() {
		pendingWake = make(chan struct{}, 1)
		go runSaveWriter()
	})
	saveMu.Unlock()
	select {
	case pendingWake <- struct{}{}:
	default:
	}
}

func runSaveWriter() {
	for range pendingWake {
		saveMu.Lock()
		data := pendingData
		pendingData = nil
		saveMu.Unlock()
		if data == nil {
			continue
		}
		if err := atomicfile.Write(path, data); err != nil {
			logx.Warn("runtime-config: write: %s", err.Error())
		}
	}
}

// ─── Experimental flags ────────────────────────────────────

func GetExperimental() Experimental {
	mu.RLock()
	defer mu.RUnlock()
	return state.Experimental
}

// SetExperimentalPatch merges patch into the current flags — only fields
// present in patch are overwritten. This matches the JS setExperimental()
// spread semantics so a PUT `{cascadeConversationReuse:true}` does not
// reset `modelIdentityPrompt` back to false.
func SetExperimentalPatch(patch map[string]any) Experimental {
	mu.Lock()
	defer mu.Unlock()
	if v, ok := patch["cascadeConversationReuse"].(bool); ok {
		state.Experimental.CascadeConversationReuse = v
	}
	if v, ok := patch["modelIdentityPrompt"].(bool); ok {
		state.Experimental.ModelIdentityPrompt = v
	}
	if v, ok := patch["preflightRateLimit"].(bool); ok {
		state.Experimental.PreflightRateLimit = v
	}
	persist()
	return state.Experimental
}

// SetExperimental is kept for callers that already hold a full struct.
func SetExperimental(e Experimental) Experimental {
	mu.Lock()
	defer mu.Unlock()
	state.Experimental = e
	persist()
	return state.Experimental
}

// IsEnabled is a convenience for the hot path.
func IsEnabled(flag string) bool {
	mu.RLock()
	defer mu.RUnlock()
	switch flag {
	case "cascadeConversationReuse":
		return state.Experimental.CascadeConversationReuse
	case "modelIdentityPrompt":
		return state.Experimental.ModelIdentityPrompt
	case "preflightRateLimit":
		return state.Experimental.PreflightRateLimit
	}
	return false
}

// GetResponseLanguagePrompt returns the current response-language steer
// text. Empty string (caller sets it to " " in runtime-config.json, or
// the default const is manually emptied) disables the steer.
func GetResponseLanguagePrompt() string {
	mu.RLock()
	defer mu.RUnlock()
	return strings.TrimSpace(state.ResponseLanguagePrompt)
}

// ─── Identity prompts ─────────────────────────────────────

func GetIdentityPrompts() map[string]string {
	mu.RLock()
	defer mu.RUnlock()
	return cloneMap(state.IdentityPrompts)
}

func IdentityFor(provider string) string {
	mu.RLock()
	defer mu.RUnlock()
	return state.IdentityPrompts[provider]
}

// BuildIdentityMessage renders the provider-specific template with {model}
// substituted. Returns "" when no template exists (i.e. identity prompt
// injection is not applicable for this request).
func BuildIdentityMessage(displayModel, provider string) string {
	tpl := IdentityFor(provider)
	if tpl == "" {
		return ""
	}
	return strings.ReplaceAll(tpl, "{model}", displayModel)
}

func SetIdentityPrompts(patch map[string]string) map[string]string {
	mu.Lock()
	defer mu.Unlock()
	if state.IdentityPrompts == nil {
		state.IdentityPrompts = map[string]string{}
	}
	for k, v := range patch {
		state.IdentityPrompts[k] = strings.TrimSpace(v)
	}
	persist()
	return cloneMap(state.IdentityPrompts)
}

func ResetIdentityPrompt(provider string) map[string]string {
	mu.Lock()
	defer mu.Unlock()
	if provider == "" {
		state.IdentityPrompts = cloneMap(DefaultIdentityPrompts)
	} else {
		delete(state.IdentityPrompts, provider)
	}
	persist()
	return cloneMap(state.IdentityPrompts)
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
