package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/assistant"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// The assistant over HTTP: where its model is, and one question at a time.
//
// The conversation lives in the browser. Each question arrives with the
// turns before it, and the server keeps nothing between requests — which is
// what makes locking the vault end the conversation, the way it ends
// everything else, and what lets a question be answered by whichever server
// process happens to be running.

// askTimeout is the ceiling on one question. A question is two or three
// requests to the model server, and the first of those may be waiting for
// the model to load, so it gets the model server's own timeout plus room for
// the tools.
const askTimeout = assistant.RequestTimeout + time.Minute

// maxHistoryTurns bounds how much of the conversation is sent back with a
// question. Past this the oldest turns are dropped: a model's context is
// finite, and the tenth follow-up rarely needs the first answer.
const maxHistoryTurns = 20

// assistantErrorResponse maps the assistant's failures onto codes the browser
// switches on. A missing configuration wants the settings dialog; a server
// that would not answer wants the message shown as it is.
func assistantErrorResponse(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, assistant.ErrNotConfigured):
		writeError(w, http.StatusPreconditionFailed,
			"no assistant has been set up — point SAND at a model server in Settings first", "NO_ASSISTANT")
	case errors.Is(err, assistant.ErrNoSuchModel):
		writeError(w, http.StatusBadGateway, err.Error(), "NO_SUCH_MODEL")
	case errors.Is(err, assistant.ErrNoToolSupport):
		writeError(w, http.StatusBadGateway, err.Error(), "NO_TOOL_SUPPORT")
	case errors.Is(err, assistant.ErrEmptyQuestion):
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
	case errors.Is(err, assistant.ErrTooManySteps):
		writeError(w, http.StatusBadGateway, err.Error(), "ASSISTANT_LOOPED")
	case errors.Is(err, vault.ErrLocked), errors.Is(err, vault.ErrSubVaultLocked), errors.Is(err, vault.ErrNoSubVault):
		vaultErrorResponse(w, err)
	default:
		writeError(w, http.StatusBadGateway, err.Error(), "ASSISTANT_FAILED")
	}
}

// handleAssistantSettings reports where the assistant runs. The URL and the
// model are the user's own choices and come back as typed; the token, if
// there is one, does not — it is a credential, and the browser has no use
// for one it already gave.
func (s *Server) handleAssistantSettings(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	writeJSON(w, http.StatusOK, assistantSettingsView(v.Assistant()))
}

// assistantSettingsView is the settings as the browser sees them. The
// window comes in three figures: what the server said, what the user set
// over it, and which of those the panel measures against.
func assistantSettingsView(settings vault.AssistantSettings) map[string]any {
	return map[string]any{
		"configured":       settings.Configured(),
		"url":              settings.URL,
		"model":            settings.Model,
		"has_key":          settings.APIKey != "",
		"context_reported": settings.ReportedContext,
		"context_tokens":   settings.ContextTokens,
		"context_window":   settings.ContextWindow(),
		"web": map[string]any{
			"engine":  settings.Web.Engine,
			"url":     settings.Web.URL,
			"has_key": settings.Web.Key != "",
		},
	}
}

// webSettingsRequest is Sandy's web access as the browser sends it.
type webSettingsRequest struct {
	// Engine is "searxng", "ollama", or empty to turn the web off.
	Engine string `json:"engine"`
	URL    string `json:"url,omitempty"`

	// Key is the ollama.com key. Absent keeps the stored one; empty clears.
	Key *string `json:"key,omitempty"`
}

type assistantSettingsRequest struct {
	// URL is the model server's OpenAI-compatible root. Empty clears the
	// assistant altogether.
	URL   string `json:"url"`
	Model string `json:"model"`

	// Key is a bearer token for a server that wants one. Absent means keep
	// whatever is stored; empty means clear it.
	Key *string `json:"key,omitempty"`

	// ContextTokens overrides the window the server reports, for a server
	// that runs the model at less than the model allows — Ollama, by
	// default. Zero means trust the server.
	ContextTokens int `json:"context_tokens,omitempty"`

	// Web is whether and how Sandy may search the web. Absent keeps what is
	// stored.
	Web *webSettingsRequest `json:"web,omitempty"`
}

// handleAssistantSet stores where the assistant runs, after checking that
// the server answers and holds the model.
//
// Checked because the alternative is finding out on the first question, and
// because one request to the server's model list is the cheapest way to
// ask. It also catches the commonest mistake — Ollama listening on loopback
// only, so that a URL naming the PC is right and unreachable — in the
// dialog where it can be fixed.
func (s *Server) handleAssistantSet(w http.ResponseWriter, r *http.Request) {
	var req assistantSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	current := v.Assistant()

	next := vault.AssistantSettings{
		URL:           strings.TrimSpace(req.URL),
		Model:         strings.TrimSpace(req.Model),
		APIKey:        current.APIKey,
		ContextTokens: req.ContextTokens,
		Web:           current.Web,
	}
	if req.Key != nil {
		next.APIKey = strings.TrimSpace(*req.Key)
	}
	if next.ContextTokens < 0 {
		writeError(w, http.StatusBadRequest, "the context window cannot be negative", "BAD_REQUEST")
		return
	}
	if req.Web != nil {
		web := vault.WebSettings{
			Engine: strings.ToLower(strings.TrimSpace(req.Web.Engine)),
			URL:    strings.TrimSpace(req.Web.URL),
			Key:    current.Web.Key,
		}
		if req.Web.Key != nil {
			web.Key = strings.TrimSpace(*req.Web.Key)
		}
		switch web.Engine {
		case vault.WebEngineNone:
			web = vault.WebSettings{}
		case vault.WebEngineSearXNG:
			url, err := assistant.ValidateBaseURL(web.URL)
			if err != nil {
				writeError(w, http.StatusBadRequest, "the SearXNG address: "+err.Error(), "BAD_REQUEST")
				return
			}
			web.URL, web.Key = url, ""
		case vault.WebEngineOllama:
			if web.Key == "" {
				writeError(w, http.StatusBadRequest, "Ollama web search needs an ollama.com key", "BAD_REQUEST")
				return
			}
			web.URL = ""
		default:
			writeError(w, http.StatusBadRequest, "the web engine must be searxng, ollama, or empty", "BAD_REQUEST")
			return
		}
		next.Web = web
	}

	if next.URL != "" {
		url, err := assistant.ValidateBaseURL(next.URL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
			return
		}
		next.URL = url
		if next.Model == "" {
			writeError(w, http.StatusBadRequest, "name the model to use", "BAD_REQUEST")
			return
		}

		ctx, cancel := contextWithTimeout(r, 30*time.Second)
		defer cancel()
		model := &assistant.ChatCompletions{BaseURL: next.URL, Model: next.Model, APIKey: next.APIKey}
		info, err := model.Describe(ctx)
		if err != nil {
			assistantErrorResponse(w, err)
			return
		}
		next.ReportedContext = info.ContextTokens
	}

	if err := v.SetAssistant(next); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, assistantSettingsView(v.Assistant()))
}

type assistantAskRequest struct {
	// Messages is the conversation so far, oldest first, ending with the
	// question being asked now.
	Messages []assistant.Turn `json:"messages"`
}

// handleAssistantAsk answers the last user turn of a conversation.
func (s *Server) handleAssistantAsk(w http.ResponseWriter, r *http.Request) {
	var req assistantAskRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].Role != assistant.RoleUser {
		writeError(w, http.StatusBadRequest, "the last message must be the question being asked", "BAD_REQUEST")
		return
	}

	history := req.Messages[:len(req.Messages)-1]
	if len(history) > maxHistoryTurns {
		history = history[len(history)-maxHistoryTurns:]
	}
	question := req.Messages[len(req.Messages)-1].Content

	a, err := s.assistantFor(requestScope(r))
	if err != nil {
		assistantErrorResponse(w, err)
		return
	}

	ctx, cancel := contextWithTimeout(r, askTimeout)
	defer cancel()

	answer, err := a.Ask(ctx, history, question)
	if err != nil {
		assistantErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}
