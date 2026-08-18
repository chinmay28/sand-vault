package server

import (
	"net/http"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Which cloud has been answering the reads.
//
// A read is a race — every account holding a shard is asked at once and the
// first k to answer rebuild the file — and the accounts that lose it cost
// nothing and are never mentioned. That is the right behaviour and the wrong
// amount of silence: a cloud that has quietly stopped winning anything is
// indistinguishable, from the app, from one that is keeping up.
//
// So the vault keeps the score (internal/vault/readstats.go) and this hands it
// over. Nothing is computed here and nothing is fetched from an account: the
// figures are counters the read path has already written, so the panel opens
// instantly and opening it costs no request against anybody's storage.
func (s *Server) handleReadStats(w http.ResponseWriter, r *http.Request) {
	window, err := vault.ParseReadWindow(r.URL.Query().Get("window"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	writeJSON(w, http.StatusOK, map[string]any{"reads": v.ReadStats(window)})
}

// handleReadStatsForget erases the history, in memory and on disk.
//
// Worth having, and worth confirming before it runs: it is the only thing in
// the panel that destroys anything. What it is for is a fresh start after
// something changed — an account moved region, a laptop moved network — where
// the old figures are no longer measuring the same setup and averaging the two
// together answers nothing.
func (s *Server) handleReadStatsForget(w http.ResponseWriter, r *http.Request) {
	window, err := vault.ParseReadWindow(r.URL.Query().Get("window"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	if err := v.ForgetReadStats(); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reads": v.ReadStats(window)})
}
