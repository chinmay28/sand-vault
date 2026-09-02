package server

import (
	"net/http"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// How long a download link lasts, read and set.
//
// A setting on the vault rather than on the server, because it is a judgement
// about the vault's owner's network and habits — how long an address to a
// folder in the clear should stay good once it has been copied somewhere —
// and it travels with the vault file the way the placement policy does.

// linkSettings is the setting and the room it has to move in, so the dialog
// can draw its choices without knowing the bounds by heart.
type linkSettings struct {
	Hours        int `json:"hours"`
	DefaultHours int `json:"default_hours"`
	MinHours     int `json:"min_hours"`
	MaxHours     int `json:"max_hours"`
}

func (s *Server) linkSettings(v *vault.Vault) linkSettings {
	return linkSettings{
		Hours:        v.LinkHours(),
		DefaultHours: int(vault.DefaultLinkLifetime.Hours()),
		MinHours:     vault.MinLinkHours,
		MaxHours:     vault.MaxLinkHours,
	}
}

// handleLinkSettings answers with how long a download link lasts.
func (s *Server) handleLinkSettings(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	writeJSON(w, http.StatusOK, s.linkSettings(v))
}

type linkSettingsRequest struct {
	// Hours is the new lifetime. Zero puts the default back.
	Hours int `json:"hours"`
}

// handleLinkSettingsSet changes it.
//
// The change reaches links already minted, in the one direction that matters:
// a lifetime made shorter shortens what is out there, so a link handed out
// under a longer rule does not outlive the new one. A lifetime made longer
// leaves them be until their next use, which extends them under the new rule
// like any other use.
func (s *Server) handleLinkSettingsSet(w http.ResponseWriter, r *http.Request) {
	var req linkSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if _, err := v.SetLinkHours(req.Hours); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	s.zips.setTTL(v.LinkLifetime())
	writeJSON(w, http.StatusOK, s.linkSettings(v))
}
