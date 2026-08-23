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

// handleProviderMeasure counts what is on one account, for the backends that
// have no way to say it: a bucket is listed end to end and the sizes added up.
//
// Its own route, and its own long deadline, because it is the one figure in the
// panel that costs a walk of somebody else's storage — a request per thousand
// objects, and real money at the providers that bill for listing. The panel
// asks for it beside the cheap stats rather than as part of them, so the rest of
// the breakdown draws immediately and this fills in when it is done.
func (s *Server) handleProviderMeasure(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	usage, err := v.MeasureProvider(ctx, r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage": usage})
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

// editProviderRequest carries the fields of a connected account the edit dialog
// can change. The first four are pointers so that an absent field means "leave
// it alone" and a present empty one means something: "" is a colour cleared
// back to the automatic one, "" is a capacity nobody is declaring any more, ""
// is a quota nobody is enforcing any more, and a name that is blank is a
// mistake the vault rejects.
//
// Capacity and Quota arrive as text rather than as numbers because they are
// typed rather than picked — "10 GB" is what somebody reads off a bucket's
// console, and the form should not make them count the zeroes.
//
// Options is the account's own settings — keys, secrets, the bucket or folder
// it writes into. It is a partial map for the same reason the rest are
// pointers: the dialog sends the fields somebody touched, and a secret it hands
// back unchanged arrives as the placeholder it was shown rather than as a real
// one. Editing these is the one PATCH that reaches the backend, since settings
// SAND cannot connect with are settings it should refuse to store.
type editProviderRequest struct {
	Name     *string           `json:"name"`
	Color    *string           `json:"color"`
	Capacity *string           `json:"capacity"`
	Quota    *string           `json:"quota"`
	Options  map[string]string `json:"options"`
}

func (s *Server) handleProviderUpdate(w http.ResponseWriter, r *http.Request) {
	var req editProviderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	edit := vault.ProviderEdit{Name: req.Name, Color: req.Color, Options: req.Options}
	if req.Capacity != nil {
		bytes, err := provider.ParseSize(*req.Capacity)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
			return
		}
		edit.Capacity = &bytes
	}
	if req.Quota != nil {
		bytes, err := provider.ParseSize(*req.Quota)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
			return
		}
		edit.Quota = &bytes
	}

	// Long enough for the ping a settings change costs, and unused by an edit
	// that only renames or recolours: that one never leaves the process.
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	v, _ := s.Vault()
	cfg, err := v.UpdateProvider(ctx, r.PathValue("id"), edit)
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
