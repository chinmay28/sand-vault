package server

import (
	"net/http"
	"time"
)

// Converting a file out of the pre-chunking format.
//
// The read path refuses such a file rather than rebuilding it whole (§4.5), so
// this is the other half of that refusal: the thing the browser offers when it
// sees NEEDS_CONVERSION, and the thing that makes the refusal actionable rather
// than a dead end.
//
// It is a long request. Converting a film is a download and a re-upload of all
// of it, minutes on a home connection, and the handler holds the connection for
// the duration rather than reporting progress out of band — the browser shows a
// spinner it warned the user about. A client that gives up does not break
// anything: the conversion either committed or it did not, and an interrupted
// one leaves the file exactly as it was.

// pendingConversionResponse is what the browser polls to decide whether to
// mention the old format at all.
type pendingConversionResponse struct {
	Files []pendingFile `json:"files"`
	Bytes int64         `json:"bytes"`
}

type pendingFile struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// handleConversionsPending lists what is still in the old format.
func (s *Server) handleConversionsPending(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()

	resp := pendingConversionResponse{Files: []pendingFile{}}
	for _, entry := range v.PendingConversion() {
		resp.Files = append(resp.Files, pendingFile{
			ID: entry.ID, Path: entry.Path(), Size: entry.Size,
		})
		resp.Bytes += entry.Size
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleFileConvert rewrites one file into the chunked format.
func (s *Server) handleFileConvert(w http.ResponseWriter, r *http.Request) {
	// Generous, because the ceiling here is the size of the file and the speed
	// of the connection under it, not anything SAND controls.
	ctx, cancel := contextWithTimeout(r, 6*time.Hour)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.Convert(ctx, r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	// A conversion counts as use of the vault: it can run far longer than the
	// idle timeout, and locking underneath it would strand it.
	s.noteExternalActivity()

	writeJSON(w, http.StatusOK, report)
}
