package server

import "net/http"

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
	v, _ := s.Vault()
	writeJSON(w, http.StatusOK, map[string]any{"reads": v.ReadStats()})
}

// handleReadStatsReset starts the counting again from now.
//
// Worth having rather than making somebody restart the server: the figures are
// most useful right after something changed — an account moved to a different
// region, a laptop moved to a different network — and a fresh count is how you
// tell what changed from what has been averaged in since the server came up.
func (s *Server) handleReadStatsReset(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	v.ResetReadStats()
	writeJSON(w, http.StatusOK, map[string]any{"reads": v.ReadStats()})
}
