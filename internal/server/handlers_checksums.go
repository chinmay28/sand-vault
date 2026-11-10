package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Checksums over HTTP: the ledger, and the two paced jobs that use it. The
// work is internal/vault/checksums.go; this is what lets a browser start a
// job, change its pace, hold it, stop it, and walk away from it.
//
// One job at a time, of either kind, run as the other long jobs here are — on
// a context of its own, polled, its report held until the next replaces it.
// What it has that they do not is a control: a rate that can be changed while
// it runs and a pause that holds it between reads. The jobWatch carries the
// run; the control is kept beside it for the three endpoints that reach it.

// DefaultChecksumsEvery is how often the ledger is brought up to date with
// the index while the vault is unlocked.
const DefaultChecksumsEvery = 2 * time.Minute

type checksumWatch = jobWatch[vault.ChecksumProgress, vault.ChecksumReport]

// checksumJobs is the watch and the control of the run it is holding.
type checksumJobs struct {
	watch *checksumWatch

	mu      sync.Mutex
	control *vault.ChecksumControl
}

func newChecksumJobs() *checksumJobs {
	return &checksumJobs{watch: newJobWatch[vault.ChecksumProgress, vault.ChecksumReport]()}
}

func (j *checksumJobs) ctl() *vault.ChecksumControl {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.control
}

// busy reports a job that is running and not paused — the state that should
// keep an idle vault from locking. A paused job does not: holding a job is
// not a way to hold a vault open.
func (j *checksumJobs) busy() bool {
	ctl := j.ctl()
	return j.watch.running() && (ctl == nil || !ctl.Paused())
}

func (j *checksumJobs) stopAll() {
	j.watch.stopAll()
	j.mu.Lock()
	j.control = nil
	j.mu.Unlock()
}

// checksumStatus is the job's status with its control's state laid over it.
type checksumStatus struct {
	jobStatus[vault.ChecksumProgress, vault.ChecksumReport]

	// Paused and Rate are the control's, read now rather than as of the last
	// progress report: a held job reports nothing, and that is the moment
	// its state matters. Rate is bytes a second, zero for unlimited.
	Paused bool  `json:"paused"`
	Rate   int64 `json:"rate"`
}

func (j *checksumJobs) status() checksumStatus {
	out := checksumStatus{jobStatus: j.watch.status()}
	out.Scope = ""
	if ctl := j.ctl(); ctl != nil && out.Running {
		out.Paused, out.Rate = ctl.Paused(), ctl.Rate()
	}
	return out
}

// maxChecksumRate bounds a rate to something a duration can be worked out of.
const maxChecksumRate = 1 << 40

type checksumRequest struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Vault string `json:"vault,omitempty"`

	// Rate is the pace in bytes a second. Zero is as fast as the clouds
	// answer.
	Rate int64 `json:"rate,omitempty"`
}

func validRate(rate int64) bool { return rate >= 0 && rate <= maxChecksumRate }

// handleChecksumSurvey says what a verify and a compute of a folder would
// each read, and how the ledger stands.
func (s *Server) handleChecksumSurvey(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	survey, err := v.SurveyChecksums(requestScope(r), r.URL.Query().Get("path"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, survey)
}

// handleChecksumStart starts a verify or a compute of a folder, or joins the
// same one already running. A different one running is 409 with that run.
func (s *Server) handleChecksumStart(w http.ResponseWriter, r *http.Request) {
	var req checksumRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	kind := vault.ChecksumKind(req.Kind)
	if kind != vault.ChecksumVerify && kind != vault.ChecksumCompute {
		writeError(w, http.StatusBadRequest, `kind must be "verify" or "compute"`, "BAD_REQUEST")
		return
	}
	if !validRate(req.Rate) {
		writeError(w, http.StatusBadRequest, "rate is bytes a second, zero for unlimited", "BAD_REQUEST")
		return
	}
	v, _ := s.Vault()
	scope := vault.Scope(req.Vault)
	dir := vault.CleanDir(req.Path)
	if !v.FolderExists(scope, dir) {
		writeError(w, http.StatusNotFound, "no such folder: "+dir, "NOT_FOUND")
		return
	}

	jobs := s.checksums
	control := vault.NewChecksumControl(req.Rate)
	key := req.Kind + "\x00" + req.Vault + "\x00" + dir
	_, outcome := jobs.watch.start(key, func(ctx context.Context, onProgress func(vault.ChecksumProgress)) (*vault.ChecksumReport, error) {
		opts := vault.ChecksumOptions{
			Control: control,
			OnProgress: func(at vault.ChecksumProgress) {
				onProgress(at)
				s.noteExternalActivity()
			},
		}
		if kind == vault.ChecksumCompute {
			return v.ComputeChecksums(ctx, scope, dir, opts)
		}
		return v.VerifyChecksums(ctx, scope, dir, opts)
	}, checkTimeout)
	if outcome == jobStarted {
		jobs.mu.Lock()
		jobs.control = control
		jobs.mu.Unlock()
	}
	writeJSON(w, startCode(outcome), jobs.status())
}

func (s *Server) handleChecksumStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.checksums.status())
}

// handleChecksumControl is pause, resume and a change of rate: the three
// things that can be said to a running job short of stopping it. Said to no
// job, each is a no-op answered with the status as it stands.
func (s *Server) handleChecksumControl(act func(*vault.ChecksumControl, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ctl := s.checksums.ctl(); ctl != nil && s.checksums.watch.running() {
			if err := act(ctl, r); err != nil {
				writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
				return
			}
			s.noteExternalActivity()
		}
		writeJSON(w, http.StatusOK, s.checksums.status())
	}
}

func checksumPause(ctl *vault.ChecksumControl, _ *http.Request) error  { ctl.Pause(); return nil }
func checksumResume(ctl *vault.ChecksumControl, _ *http.Request) error { ctl.Resume(); return nil }

func checksumRate(ctl *vault.ChecksumControl, r *http.Request) error {
	var req struct {
		Rate int64 `json:"rate"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if !validRate(req.Rate) {
		return errRate
	}
	ctl.SetRate(req.Rate)
	return nil
}

var errRate = rateError("rate is bytes a second, zero for unlimited")

type rateError string

func (e rateError) Error() string { return string(e) }

// handleChecksumStop ends a running job. It finishes with what it had done —
// and is let go first if it was paused, so that it can.
func (s *Server) handleChecksumStop(w http.ResponseWriter, r *http.Request) {
	s.checksums.watch.cancel()
	writeJSON(w, http.StatusOK, s.checksums.status())
}

// handleChecksumLedgerWrite brings a vault's checksums.gz up to date now,
// rather than at the next tick of the loop below.
func (s *Server) handleChecksumLedgerWrite(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 10*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	ledger, err := v.WriteChecksumLedger(ctx, requestScope(r))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ledger)
}

// checksumLedgerLoop keeps every open vault's ledger in step with its index.
//
// It is a tick rather than a hook on the index write because storing the
// ledger is an upload to the default clouds, and a folder of ten thousand
// files being imported is ten thousand index writes: once every so often,
// and only when the index has changed since, is the whole of the cost. An
// unchanged vault is not touched — see WriteChecksumLedgers.
func (s *Server) checksumLedgerLoop() {
	every := s.ChecksumsEvery
	if every <= 0 {
		return
	}
	for range time.Tick(every) {
		v, err := s.Vault()
		if err != nil || !v.Unlocked() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		if err := v.WriteChecksumLedgers(ctx); err != nil && err != vault.ErrLocked {
			log.Printf("checksums.gz: %v", err)
		}
		cancel()
	}
}

// checksumOnRead digests a file as it is served, so that a file with no
// checksum on record that is read from its first byte to its last has one
// recorded from that read.
//
// ServeContent seeks to the end to learn the length and back to the start
// before it reads, and a range request seeks somewhere else entirely. So the
// digest follows the position: it is good only for bytes that arrived in
// order from zero, a return to zero starts it again, and any other jump
// spoils it for this request.
type checksumOnRead struct {
	io.ReadSeeker
	digest  hash.Hash
	at      int64 // where the next read lands
	hashed  int64 // how far the digest has got
	spoiled bool
}

func newChecksumOnRead(body io.ReadSeeker) *checksumOnRead {
	return &checksumOnRead{ReadSeeker: body, digest: sha256.New()}
}

func (c *checksumOnRead) Seek(offset int64, whence int) (int64, error) {
	at, err := c.ReadSeeker.Seek(offset, whence)
	if err == nil {
		c.at = at
	}
	return at, err
}

func (c *checksumOnRead) Read(p []byte) (int, error) {
	n, err := c.ReadSeeker.Read(p)
	if n > 0 {
		switch {
		case c.at == 0 && c.hashed != 0:
			c.digest.Reset()
			c.hashed, c.spoiled = 0, false
			fallthrough
		case c.at == c.hashed && !c.spoiled:
			c.digest.Write(p[:n])
			c.hashed += int64(n)
		default:
			c.spoiled = true
		}
		c.at += int64(n)
	}
	return n, err
}

// sum is the checksum of the whole file, if the whole file went by in order.
func (c *checksumOnRead) sum(size int64) (string, bool) {
	if c.spoiled || c.hashed != size {
		return "", false
	}
	return hex.EncodeToString(c.digest.Sum(nil)), true
}
