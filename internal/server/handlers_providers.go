package server

import (
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// handleProviderSpecs describes every backend SAND can connect to, including
// the fields each one needs. The connect form in the browser is generated from
// this, so a new backend shows up in the UI without frontend changes.
func (s *Server) handleProviderSpecs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"specs": provider.Specs()})
}

func (s *Server) handleProvidersList(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()

	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()

	statuses, err := v.ProviderStatuses(ctx)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": statuses})
}

// handleProviderStats reports one account on its own: what it is holding, what
// room is left on it, and what the load is made of. The sidebar's one-line
// summary is in here too — the panel is opened straight from that line and
// re-pings as it opens, so it answers with what it found rather than with what
// the last refresh found.
func (s *Server) handleProviderStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.ProviderStats(ctx, r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": report})
}

type addProviderRequest struct {
	Kind    provider.Kind     `json:"kind"`
	Name    string            `json:"name"`
	Options map[string]string `json:"options"`
}

func (s *Server) handleProviderAdd(w http.ResponseWriter, r *http.Request) {
	var req addProviderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if req.Kind == "" {
		writeError(w, http.StatusBadRequest, "missing provider kind", "BAD_REQUEST")
		return
	}

	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	v, _ := s.Vault()
	cfg, err := v.AddProvider(ctx, provider.Config{
		Kind:    req.Kind,
		Name:    req.Name,
		Options: req.Options,
	})
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"provider": cfg})
}

// editProviderRequest carries the fields of a connected account the edit menu
// can change. Both are pointers so that an absent field means "leave it alone"
// and a present empty one means something: "" is a colour cleared back to the
// automatic one, and a name that is blank is a mistake the vault rejects.
type editProviderRequest struct {
	Name  *string `json:"name"`
	Color *string `json:"color"`
}

func (s *Server) handleProviderUpdate(w http.ResponseWriter, r *http.Request) {
	var req editProviderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	cfg, err := v.UpdateProvider(r.PathValue("id"), vault.ProviderEdit{
		Name:  req.Name,
		Color: req.Color,
	})
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": cfg})
}

func (s *Server) handleProviderTest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	v, _ := s.Vault()
	if err := v.TestProvider(ctx, r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"online": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"online": true})
}

func (s *Server) handleProviderRemove(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"

	v, _ := s.Vault()
	if err := v.RemoveProvider(r.PathValue("id"), force); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}
