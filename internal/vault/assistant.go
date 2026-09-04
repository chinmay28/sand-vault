package vault

import "strings"

// AssistantSettings say where the chat assistant's model runs.
//
// The assistant is a language model the user runs on a machine of their own —
// Ollama on the PC with the graphics card, say — and SAND only ever talks to
// the one named here. There is no default: a vault with nothing set has no
// assistant, and asks nobody anything.
type AssistantSettings struct {
	// URL is the model server's OpenAI-compatible root, such as
	// http://gaming-pc:11434/v1. Empty means no assistant.
	URL string `json:"url"`

	// Model names which model on that server answers.
	Model string `json:"model"`

	// APIKey is sent as a bearer token when the server wants one. Most home
	// servers do not; vLLM can be started with one.
	APIKey string `json:"api_key,omitempty"`

	// ContextTokens is the model's context window as the user set it, for
	// the panel to show how full a conversation is. Zero means use what the
	// server reported.
	ContextTokens int `json:"context_tokens,omitempty"`

	// ReportedContext is the window the server reported when the settings
	// were checked, kept so the dialog can show what it said and the panel
	// has a figure without asking again.
	ReportedContext int `json:"reported_context,omitempty"`

	// Web is whether, and through what, Sandy may search the web. Empty
	// means he may not, which is how every vault starts.
	Web WebSettings `json:"web,omitempty"`
}

// WebSettings say how Sandy reaches the public web, if at all.
//
// A query is the one thing about a question that leaves the owner's
// network — the name of a chart, a series, a year — so which engine gets
// it is the owner's choice. Their own SearXNG keeps it on their own
// machines; Ollama's search service is a third party they have a key for.
type WebSettings struct {
	// Engine is "searxng", "ollama", or empty for no web at all.
	Engine string `json:"engine,omitempty"`

	// URL is the SearXNG instance, for that engine.
	URL string `json:"url,omitempty"`

	// Key is the ollama.com key, for that engine. A credential, sealed with
	// the rest and never read back.
	Key string `json:"key,omitempty"`
}

// Web engines.
const (
	WebEngineNone    = ""
	WebEngineSearXNG = "searxng"
	WebEngineOllama  = "ollama"
)

// Enabled reports whether Sandy may reach the web.
func (w WebSettings) Enabled() bool { return w.Engine != WebEngineNone }

// normalize trims what was typed and drops what the chosen engine does not
// use, so a key typed under one engine does not sit forgotten under another.
func (w WebSettings) normalize() WebSettings {
	w.Engine = strings.ToLower(strings.TrimSpace(w.Engine))
	w.URL = strings.TrimRight(strings.TrimSpace(w.URL), "/")
	w.Key = strings.TrimSpace(w.Key)
	switch w.Engine {
	case WebEngineSearXNG:
		w.Key = ""
	case WebEngineOllama:
		w.URL = ""
	default:
		return WebSettings{}
	}
	return w
}

// ContextWindow is the window the panel measures against: what the user
// set, or failing that what the server said, or zero when neither.
func (s AssistantSettings) ContextWindow() int {
	if s.ContextTokens > 0 {
		return s.ContextTokens
	}
	return s.ReportedContext
}

// Configured reports whether there is a server to talk to.
func (s AssistantSettings) Configured() bool {
	return strings.TrimSpace(s.URL) != "" && strings.TrimSpace(s.Model) != ""
}

// normalize trims what was typed. An empty URL clears the whole thing: a
// model name with nowhere to send it is not half a setting.
func (s AssistantSettings) normalize() AssistantSettings {
	s.URL = strings.TrimRight(strings.TrimSpace(s.URL), "/")
	s.Model = strings.TrimSpace(s.Model)
	s.APIKey = strings.TrimSpace(s.APIKey)
	if s.ContextTokens < 0 {
		s.ContextTokens = 0
	}
	if s.ReportedContext < 0 {
		s.ReportedContext = 0
	}
	s.Web = s.Web.normalize()
	if s.URL == "" {
		return AssistantSettings{}
	}
	return s
}

// Assistant returns where the assistant runs, or the zero value when nothing
// has been set — which is every vault until somebody sets one.
func (v *Vault) Assistant() AssistantSettings {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil || v.settings == nil || v.settings.Assistant == nil {
		return AssistantSettings{}
	}
	return *v.settings.Assistant
}

// SetAssistant stores where the assistant runs, or clears it when given an
// empty URL.
//
// It goes into the sealed settings section rather than the manifest: the
// manifest is copied to every connected account as a recovery backup, and
// the address of a machine on the user's network — plus, sometimes, a token
// that opens it — has no business being on three clouds.
func (v *Vault) SetAssistant(settings AssistantSettings) error {
	settings = settings.normalize()

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	if v.settings == nil {
		v.settings = &vaultSettings{}
	}

	previous := v.settings.Assistant
	if previous == nil && !settings.Configured() && settings.URL == "" {
		return nil
	}
	if previous != nil && *previous == settings {
		return nil
	}

	if settings.URL == "" {
		v.settings.Assistant = nil
	} else {
		stored := settings
		v.settings.Assistant = &stored
	}
	if err := v.persistLocked(); err != nil {
		v.settings.Assistant = previous
		return err
	}
	return nil
}
