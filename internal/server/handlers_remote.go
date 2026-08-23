package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The machines a vault imports files from, over HTTP.
//
// Five endpoints, and the split between them is the one the feature turns on.
// Listing what is configured is index work. Adding one, browsing one and
// importing from one all talk to somebody else's machine, so each gets a
// deadline of its own — and they are wildly different deadlines, because
// listing a directory is one round trip and an import can be a media library.
//
// See internal/vault/source.go for what a source is and why it is deliberately
// not a connected account, and docs/sftp.md for the whole design.

// remoteConnectTimeout bounds adding or editing a source, which is a handshake,
// a directory listing, and nothing else.
const remoteConnectTimeout = 45 * time.Second

// remoteBrowseTimeout bounds one directory listing. Short: this happens on
// every click, and a source that has gone away should say so quickly rather
// than leaving the picker spinning.
const remoteBrowseTimeout = 60 * time.Second

// remoteImportTimeout is the ceiling on one import.
//
// Generous for the same reason the git track timeout is: this is a selection
// coming down somebody's home connection and going back up to three clouds, and
// a folder of films is not a five-minute job. It is a ceiling rather than a
// budget — an import that runs out of it has still committed everything that
// arrived, and re-running picks up where it stopped, because what is already in
// the vault is skipped. See vault.ImportFromSource.
const remoteImportTimeout = 12 * time.Hour

// sourceRequest is a machine somebody wants to import from.
//
// The secret fields carry the redaction placeholder back on an edit, meaning
// "leave the one you have alone", exactly as a connected account's do.
type sourceRequest struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	User string `json:"user"`
	Root string `json:"root"`

	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	Password   string `json:"password,omitempty"`

	// HostKey is the fingerprint to pin, for somebody who has read it off the
	// server. Left out, the first connection learns it.
	HostKey string `json:"host_key,omitempty"`

	// RelearnHostKey drops the stored pin so the next connection learns a new
	// one — how somebody says "I rebuilt this machine".
	//
	// A field of its own rather than an empty HostKey, because those mean
	// opposite things: an edit form that does not mention the fingerprint is
	// not asking to trust whatever answers next. It is also the one edit here
	// that weakens something, which makes it worth being unable to do by
	// accident.
	RelearnHostKey bool `json:"relearn_host_key,omitempty"`
}

// sourceFrom turns a request into a source, swapping a handle for a key SAND
// generated in place of the private key field.
//
// A generated key's private half is never sent to the browser, so what comes
// back in that field is the handle standing in for it — see
// handlers_sshkeys.go. Everything else, including a key somebody pasted in
// themselves, passes through untouched.
func (s *Server) sourceFrom(req sourceRequest) (vault.Source, error) {
	source := req.toSource()
	key, err := s.resolveGeneratedKey(source.PrivateKey)
	if err != nil {
		return vault.Source{}, err
	}
	source.PrivateKey = key
	return source, nil
}

func (req sourceRequest) toSource() vault.Source {
	return vault.Source{
		Name:       strings.TrimSpace(req.Name),
		Host:       strings.TrimSpace(req.Host),
		Port:       req.Port,
		User:       strings.TrimSpace(req.User),
		Root:       strings.TrimSpace(req.Root),
		PrivateKey: req.PrivateKey,
		Passphrase: req.Passphrase,
		Password:   req.Password,
		HostKey:    strings.TrimSpace(req.HostKey),
	}
}

// handleRemoteList answers with the configured sources, secrets redacted.
func (s *Server) handleRemoteList(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()

	sources, err := v.Sources()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

// handleRemoteAdd connects a machine and stores it.
func (s *Server) handleRemoteAdd(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	added, err := s.sourceFrom(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "KEY_EXPIRED")
		return
	}

	ctx, cancel := contextWithTimeout(r, remoteConnectTimeout)
	defer cancel()

	v, _ := s.Vault()
	source, err := v.AddSource(ctx, added)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"source": source})
}

// handleRemoteUpdate changes a stored source, reconnecting to check it.
func (s *Server) handleRemoteUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req sourceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	edits, err := s.sourceFrom(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "KEY_EXPIRED")
		return
	}

	ctx, cancel := contextWithTimeout(r, remoteConnectTimeout)
	defer cancel()

	v, _ := s.Vault()
	source, err := v.UpdateSource(ctx, id, edits, req.RelearnHostKey)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": source})
}

// handleRemoteRemove forgets a machine. Nothing already imported is touched.
func (s *Server) handleRemoteRemove(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	if err := v.RemoveSource(r.PathValue("id")); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}

// handleRemoteBrowse lists one directory on a source.
//
// The path in the query string is relative to the source's folder and is never
// absolute, in either direction: an API handing out absolute server paths would
// be inviting the next caller to send one back. Everything outside that folder
// is refused however the path is written, links included — see
// (*sftp.Client).resolve.
func (s *Server) handleRemoteBrowse(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, remoteBrowseTimeout)
	defer cancel()

	v, _ := s.Vault()
	listing, err := v.BrowseSource(ctx, r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

// remoteImportRequest is one pull into a folder of the vault.
type remoteImportRequest struct {
	Vault string `json:"vault,omitempty"`

	// Paths are what was selected, relative to the source's folder. A folder
	// brings everything under it.
	Paths []string `json:"paths"`

	// Dest is the vault folder they land in, made if it is not there.
	Dest string `json:"dest"`

	// Accounts and Scheme are the ordinary upload choices, because a file that
	// arrived over SFTP is an ordinary file and there is no reason it should
	// get less say over where it lands than one dragged into the browser.
	Accounts []string `json:"accounts,omitempty"`
	Scheme   string   `json:"scheme,omitempty"`

	// Overwrite replaces a file of the same name rather than storing this one
	// beside it under a numbered name, and re-fetches what would otherwise be
	// skipped as already imported.
	Overwrite bool `json:"overwrite,omitempty"`
}

// handleRemoteImport pulls files off a source into the vault.
//
// Synchronous, and deliberately. There is no background job machinery in this
// server and this is not the feature that should introduce it: what makes that
// affordable is that an interrupted import loses nothing — every file that
// arrived is committed, and re-running the same request skips them and carries
// on. The answer is one line per file so that a partial import is legible
// rather than mysterious.
func (s *Server) handleRemoteImport(w http.ResponseWriter, r *http.Request) {
	var req remoteImportRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	scheme, err := parseSchemeField(req.Scheme)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_SCHEME")
		return
	}

	ctx, cancel := contextWithTimeout(r, remoteImportTimeout)
	defer cancel()

	// Registered before the first byte moves and forgotten however this ends,
	// so what GET /api/remote/{id}/import answers with is what is actually
	// running. It is a place to write progress to and nothing the import reads
	// back — see import_watch.go.
	id := r.PathValue("id")
	ticket := s.imports.start(id, req.Dest, req.Vault)
	defer ticket.done()

	v, _ := s.Vault()
	summary, err := v.ImportFromSource(ctx, vault.Scope(req.Vault), id, vault.ImportRequest{
		Paths:      req.Paths,
		Dest:       req.Dest,
		Accounts:   req.Accounts,
		Scheme:     scheme,
		Overwrite:  req.Overwrite,
		OnProgress: ticket.update,
	})
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	// 201 when something arrived, 200 when everything was already here. A
	// request that fetched nothing because nothing needed fetching is a
	// success, and saying so with "created" would be a lie about what happened.
	status := http.StatusOK
	if summary.Imported > 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, summary)
}

// handleRemoteImportProgress answers with the imports running from one machine
// right now, and an empty list when there are none.
//
// An empty list is the ordinary answer rather than an error: an import that has
// finished, failed or was cancelled is not running, and those are the same
// thing to a bar that should stop being drawn. What the import *did* comes back
// from the POST that started it, which is the only place a result belongs.
func (s *Server) handleRemoteImportProgress(w http.ResponseWriter, r *http.Request) {
	// The source is checked against the vault so that a stale dialog polling a
	// machine that has since been forgotten is told so, rather than being left
	// watching an empty list forever.
	v, _ := s.Vault()
	if _, err := v.Source(r.PathValue("id")); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imports": s.imports.forSource(r.PathValue("id"))})
}
