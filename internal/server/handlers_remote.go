package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The machines a vault moves files to and from, over HTTP.
//
// The endpoints split the way the feature does. Listing what is configured is
// index work. Adding one, browsing one, importing from one and exporting to
// one all talk to somebody else's machine, so each gets a deadline of its own
// — and they are wildly different deadlines, because listing a directory is
// one round trip and a transfer can be a media library.
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

// remoteTransferTimeout is the ceiling on one import or export.
//
// Generous for the same reason the git track timeout is: this is a selection
// crossing somebody's home connection in one direction and three clouds in the
// other, and a folder of films is not a five-minute job. It is a ceiling rather
// than a budget — a transfer that runs out of it has still committed everything
// that arrived, and re-running picks up where it stopped, because what is
// already there is skipped. See vault.ImportFromSource and vault.ExportToSource.
const remoteTransferTimeout = 12 * time.Hour

// sourceRequest is a machine somebody wants to import from or export to.
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

// handleRemoteRemove forgets a machine. Nothing already imported or exported
// is touched.
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

	// Detach runs the import on a context of its own and answers at once,
	// rather than holding the request open until it is done. Asked for, never
	// assumed: an import that keeps running after the page is closed is a thing
	// somebody should have decided, not something they discover later.
	Detach bool `json:"detach,omitempty"`
}

// remoteExportRequest is one push from the vault onto a source.
type remoteExportRequest struct {
	Vault string `json:"vault,omitempty"`

	// Paths are what was picked, as paths in the vault — files, folders, or
	// both. A folder brings everything under it, keeping its shape.
	Paths []string `json:"paths"`

	// Dest is the folder on the machine they land in, relative to the source's
	// folder and made if it is not there. Empty is that folder itself.
	Dest string `json:"dest"`

	// Overwrite replaces a file already at the name. Without it a file that is
	// there is left alone and reported: as already exported when it is the
	// same file, and as in the way when it is not.
	Overwrite bool `json:"overwrite,omitempty"`

	// Detach is the same bargain an import's is — see remoteImportRequest.
	Detach bool `json:"detach,omitempty"`
}

// transferJob is one direction of a transfer, described well enough for the
// machinery around it — the ticket, the deadline, foreground or detached — to
// run either without knowing which.
type transferJob struct {
	kind  transferKind
	dest  string
	scope string

	// run does the transfer and hands back its summary, which is the answer
	// to the request, and how many files actually moved, which decides
	// whether that answer is a 201 or a 200.
	run func(ctx context.Context, ticket *transferTicket) (summary any, moved int, err error)
}

// handleRemoteImport pulls files off a source into the vault.
//
// Synchronous unless asked otherwise, and that is still the default for a
// reason: a request that holds the connection open is one whose cost is visible
// while it is being paid, and closing the tab stops it. What makes it
// affordable is that an interrupted import loses no whole file — every file
// that arrived is committed, and re-running the same request skips them and
// carries on. The answer is one line per file so that a partial import is
// legible rather than mysterious.
//
// `detach` is the other half of that bargain, for the case the default is
// wrong: one very large file, where the page would have to stay open for an
// hour to fetch something the machine could fetch on its own. It answers 202
// at once and runs on a context of its own — see startDetachedTransfer, and
// transfer_watch.go for what "background" is allowed to mean here.
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

	id := r.PathValue("id")
	v, _ := s.Vault()
	job := transferJob{
		kind: transferImport, dest: req.Dest, scope: req.Vault,
		run: func(ctx context.Context, ticket *transferTicket) (any, int, error) {
			summary, err := v.ImportFromSource(ctx, vault.Scope(req.Vault), id, vault.ImportRequest{
				Paths:      req.Paths,
				Dest:       req.Dest,
				Accounts:   req.Accounts,
				Scheme:     scheme,
				Overwrite:  req.Overwrite,
				OnProgress: s.transferProgress(ticket),
			})
			if err != nil {
				return nil, 0, err
			}
			return &summary, summary.Imported, nil
		},
	}
	s.runTransfer(w, r, id, req.Detach, job)
}

// handleRemoteExport writes files out of the vault onto a source.
//
// The same shape as the import in every respect but the direction — the same
// per-file answer, the same skip on a re-run, the same detach — because it is
// the same kind of request. What is different is said in the dialog rather
// than here: the far end holds the files in the clear afterwards.
func (s *Server) handleRemoteExport(w http.ResponseWriter, r *http.Request) {
	var req remoteExportRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	id := r.PathValue("id")
	v, _ := s.Vault()
	job := transferJob{
		kind: transferExport, dest: req.Dest, scope: req.Vault,
		run: func(ctx context.Context, ticket *transferTicket) (any, int, error) {
			summary, err := v.ExportToSource(ctx, vault.Scope(req.Vault), id, vault.ExportRequest{
				Paths:      req.Paths,
				Dest:       req.Dest,
				Overwrite:  req.Overwrite,
				OnProgress: s.transferProgress(ticket),
			})
			if err != nil {
				return nil, 0, err
			}
			return &summary, summary.Exported, nil
		},
	}
	s.runTransfer(w, r, id, req.Detach, job)
}

// transferProgress is what a running transfer reports through: the ticket,
// and the activity clock beside it.
func (s *Server) transferProgress(ticket *transferTicket) func(vault.TransferProgress) {
	return func(at vault.TransferProgress) {
		ticket.update(at)
		// A transfer with nobody watching is still use, and without saying so
		// the idle timer would lock the vault out from under it — the keys it
		// is sealing or opening chunks with are the ones auto-locking takes
		// away. The share and the stream links say it the same way.
		s.noteExternalActivity()
	}
}

// runTransfer runs one job in the foreground, or hands it to the machine.
func (s *Server) runTransfer(w http.ResponseWriter, r *http.Request, id string, detach bool, job transferJob) {
	if detach {
		s.startDetachedTransfer(w, id, job)
		return
	}

	ctx, cancel := contextWithTimeout(r, remoteTransferTimeout)
	defer cancel()

	// Registered before the first byte moves and forgotten however this ends,
	// so what the progress GET answers with is what is actually running. It
	// is a place to write progress to and nothing the transfer reads back —
	// see transfer_watch.go.
	ticket, err := s.transfers.start(job.kind, id, job.dest, job.scope, false, cancel)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error(), "TRANSFER_BUSY")
		return
	}
	defer ticket.done()

	summary, moved, err := job.run(ctx, ticket)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	// 201 when something moved, 200 when everything was already there. A
	// request that moved nothing because nothing needed moving is a success,
	// and saying so with "created" would be a lie about what happened.
	status := http.StatusOK
	if moved > 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, summary)
}

// startDetachedTransfer answers 202 and lets the transfer run without a
// request behind it.
//
// The context is derived from Background rather than from r: that is the whole
// difference, and it is what makes closing the page harmless instead of fatal.
// The ceiling is the same one a foreground transfer gets, so a detached one
// cannot outlive the day either.
func (s *Server) startDetachedTransfer(w http.ResponseWriter, id string, job transferJob) {
	// Refused before anything is started, so a bad source ID or a locked sub
	// vault comes back as an error on this request rather than as a run that
	// appears and immediately fails. Everything after this is the transfer's
	// own business to report.
	v, _ := s.Vault()
	if _, err := v.Source(id); err != nil {
		vaultErrorResponse(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), remoteTransferTimeout)
	ticket, err := s.transfers.start(job.kind, id, job.dest, job.scope, true, cancel)
	if err != nil {
		cancel()
		writeError(w, http.StatusConflict, err.Error(), "TRANSFER_BUSY")
		return
	}

	go func() {
		defer cancel()
		summary, _, err := job.run(ctx, ticket)
		// A cancelled transfer is not a failed one. It stopped because somebody
		// stopped it, and what it had already moved is where it was going — the
		// summary says how much, which is exactly what the next run will skip.
		cancelled := ctx.Err() != nil
		if err != nil && cancelled {
			err = nil
		}
		if err != nil {
			summary = nil
		}
		ticket.finish(summary, err, cancelled)
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{"run": s.transfers.forRun(ticket.id)})
}

// handleRemoteImportStop cancels a running import, or forgets a finished one's
// result. Both are the same gesture: stop showing me this.
func (s *Server) handleRemoteImportStop(w http.ResponseWriter, r *http.Request) {
	s.stopTransfer(w, r, transferImport)
}

// handleRemoteExportStop is the same gesture aimed at an export.
func (s *Server) handleRemoteExportStop(w http.ResponseWriter, r *http.Request) {
	s.stopTransfer(w, r, transferExport)
}

func (s *Server) stopTransfer(w http.ResponseWriter, r *http.Request, kind transferKind) {
	if !s.transfers.stop(r.PathValue("run"), kind) {
		writeError(w, http.StatusNotFound, "no "+string(kind)+" by that name is running", "NOT_FOUND")
		return
	}
	// 200 with what is left rather than 204, so the dialog that asked redraws
	// from the answer instead of from a guess about what it did.
	writeJSON(w, http.StatusOK, map[string]any{
		string(kind) + "s": s.transfers.forSource(r.PathValue("id"), kind),
	})
}

// handleRemoteImportProgress answers with what one machine has coming in, and
// what a detached import lately finished with.
//
// An empty list is the ordinary answer rather than an error. A foreground
// import that is over is not listed at all — its answer went back down the
// request that started it, which is the only place a result belongs when there
// is a request to put it in. A detached one has no such request, so its summary
// waits here until it is dismissed or goes stale.
func (s *Server) handleRemoteImportProgress(w http.ResponseWriter, r *http.Request) {
	s.transferProgressFor(w, r, transferImport)
}

// handleRemoteExportProgress is the same window onto what is going out.
func (s *Server) handleRemoteExportProgress(w http.ResponseWriter, r *http.Request) {
	s.transferProgressFor(w, r, transferExport)
}

func (s *Server) transferProgressFor(w http.ResponseWriter, r *http.Request, kind transferKind) {
	// The source is checked against the vault so that a stale dialog polling a
	// machine that has since been forgotten is told so, rather than being left
	// watching an empty list forever.
	v, _ := s.Vault()
	if _, err := v.Source(r.PathValue("id")); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		string(kind) + "s": s.transfers.forSource(r.PathValue("id"), kind),
	})
}
