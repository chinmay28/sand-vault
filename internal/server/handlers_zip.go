package server

import (
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// A folder handed back as one zip, streamed.
//
// Two requests, for the reason a stream link is two: the archive can be far
// bigger than a browser will buffer, so it cannot come back down a fetch the
// page reads into memory — it has to be an address the browser saves from
// directly, and an address needs a credential of its own, since the session
// cookie is not something a download manager or another device can carry.
// So the POST plans the archive from the index, refuses what cannot be built,
// and mints a short-lived ticket; the GET streams it to whoever holds the
// ticket, with no session asked for.
//
// Nothing about the archive is held here. Each GET plans it again from the
// index and gathers each file from the clouds as the archive is written, so
// what leaves this machine at any moment is a chunk window — the same bargain
// the content endpoint makes for one file, made for a folder. See
// vault.WriteFolderZip.

// zipTicketTTL is how long a zip link is good for.
//
// Short, because it is a bearer link to a whole folder in the clear and the
// browser follows it within a second of being handed it. Long enough that the
// address can be pasted into a download manager on another machine — the case
// a Pi's owner actually has, pulling a folder to the desktop that has the disk
// for it — and it slides forward while the download runs.
const zipTicketTTL = 15 * time.Minute

// zipStreamTimeout is the ceiling on one archive. A folder is as big as a
// folder is, and the connection under it is what sets the pace.
const zipStreamTimeout = 12 * time.Hour

// zipTicket is what a zip token stands for: one folder, in one vault.
type zipTicket struct {
	scope vault.Scope
	dir   string
}

// zipRequest names the folder.
type zipRequest struct {
	Path string `json:"path"`

	// Vault names which of the vaults inside the file the folder belongs to.
	// Empty is the main vault.
	Vault string `json:"vault,omitempty"`
}

// zipLinkResponse is what the browser gets back: where to save from, what it
// will be called, and what it will hold — so the dialog can say "4.2 GB in
// 312 files" before the download starts rather than after.
type zipLinkResponse struct {
	URL       string    `json:"url"`
	Name      string    `json:"name"`
	Files     int       `json:"files"`
	Folders   int       `json:"folders"`
	Bytes     int64     `json:"bytes"`
	ExpiresAt time.Time `json:"expires_at"`
	ExpiresIn int       `json:"expires_in"`
}

// handleFolderZipLink plans a folder's archive and mints the link to save it
// from.
//
// Everything that can be refused is refused here, on a request the page can
// read the answer to: a folder that is not there, one with nothing in it, one
// holding a file still in the pre-chunking format. Once the GET has started
// writing, a 200 in flight cannot become a 409.
func (s *Server) handleFolderZipLink(w http.ResponseWriter, r *http.Request) {
	var req zipRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if req.Path == "" {
		req.Path = "/"
	}

	v, _ := s.Vault()
	scope := vault.Scope(req.Vault)
	plan, err := v.PlanFolderZip(scope, req.Path)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	if err := plan.Ready(); err != nil {
		vaultErrorResponse(w, err)
		return
	}

	token, expiry, err := s.zips.issue(zipTicket{scope: scope, dir: plan.Path})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not mint a download link", "INTERNAL_ERROR")
		return
	}

	name := plan.Name + ".zip"
	writeJSON(w, http.StatusCreated, zipLinkResponse{
		URL:       "/zip/" + token + "/" + url.PathEscape(name),
		Name:      name,
		Files:     plan.Files,
		Folders:   plan.Folders,
		Bytes:     plan.Bytes,
		ExpiresAt: expiry,
		ExpiresIn: int(time.Until(expiry).Round(time.Second).Seconds()),
	})
}

// handleFolderZip streams a ticket's folder as a zip to whoever holds the
// ticket.
//
// No session is required: the ticket is the credential, exactly as it is for
// a stream link. There is no Content-Length, because the archive is written as
// it is built and its size is not known until it is finished — a browser shows
// the bytes so far rather than a percentage, which is the honest reading of a
// download whose end is decided by three clouds.
func (s *Server) handleFolderZip(w http.ResponseWriter, r *http.Request) {
	t, ok := s.zips.lookup(r.PathValue("token"))
	if !ok {
		writeError(w, http.StatusNotFound, "this download link has expired", "NO_TICKET")
		return
	}

	v, err := s.Vault()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
		return
	}
	if !v.Unlocked() {
		writeError(w, http.StatusUnauthorized, "vault is locked", "LOCKED")
		return
	}

	// Planned again rather than from the ticket, so the archive is of the
	// folder as it is now — and so the refusals still hold if the folder
	// changed underneath the link.
	plan, err := v.PlanFolderZip(t.scope, t.dir)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	if err := plan.Ready(); err != nil {
		vaultErrorResponse(w, err)
		return
	}

	ctx, cancel := contextWithTimeout(r, zipStreamTimeout)
	defer cancel()

	// A download manager has no browser session keeping the vault awake, and
	// an archive can run for hours; without this the vault would lock itself
	// halfway through.
	s.noteExternalActivity()

	name := plan.Name + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// Reconstructed from encrypted parts: never let a shared cache hold the
	// plaintext, and never let a proxy pretend it knows how long it is.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Sand-Files", fmt.Sprint(plan.Files))
	w.Header().Set("X-Sand-Bytes", fmt.Sprint(plan.Bytes))
	// Everything below this line is the archive; a HEAD stops here with the
	// headers a download manager wants to see.
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Ticks the activity clock as the archive goes, so a folder that takes an
	// hour to leave is not locked out from under itself after the idle timeout.
	body := &activityWriter{w: w, note: s.noteExternalActivity}
	if err := v.WriteFolderZip(ctx, t.scope, t.dir, body); err != nil {
		// The status is already on the wire, so the only thing left to do is
		// stop — a truncated archive is what the browser sees, and the log is
		// where the reason goes. A reader that hung up is the usual cause and
		// not worth a line.
		if ctx.Err() == nil {
			log.Printf("zip of %s stopped: %v", t.dir, err)
		}
		return
	}
}

// activityWriter passes bytes through and notes that the vault is in use,
// every so often rather than on every write.
type activityWriter struct {
	w     http.ResponseWriter
	note  func()
	since int64
}

// activityEvery is how much has to leave before use is noted again. Sixty-four
// megabytes is seconds on any link this is worth doing over, and a lock clock
// touched every few seconds is touched often enough.
const activityEvery = 64 << 20

func (a *activityWriter) Write(p []byte) (int, error) {
	n, err := a.w.Write(p)
	a.since += int64(n)
	if a.since >= activityEvery {
		a.since = 0
		a.note()
	}
	return n, err
}
