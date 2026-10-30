package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The paced re-encryption, from the server's side.
//
// A password change can leave its files to a plan — so much a day, these
// folders first — and vault.RekeyStep moves one file under that plan when the
// plan's meter allows. Somebody has to call it, and that is this loop: on a
// tick, while the vault is open, for as long as anything is on the previous
// key. See internal/vault/rekeyplan.go for the plan itself and the reasoning;
// here is only the clock, and the two things a clock has to know about the
// server around it.
//
// First, the idle timer. The server re-locks a vault nobody has used for a
// while, and a re-encryption is use only by courtesy: it must not be cut off
// mid-file, so the timer is held off while one is moving — the same courtesy
// the automation sweep gets — and otherwise left alone, so a plan does not
// quietly turn auto-lock off. A plan that asks to keep the vault open
// (KeepUnlocked) is the exception, and it is a choice the person made in so
// many words.
//
// Second, the slot. A finish-now pass (POST /api/vault/migrate) and this loop
// share one, inside the vault, so the loop never has to know whether one is
// running: the step comes back asking to be tried again shortly.
//
// The tick is only how often the loop asks whether anything is due. Once a
// file has moved the next is taken straight away (rekeyRound): the meter is
// what spaces files out on a paced plan, and a plan at full speed has no
// meter, so waiting a tick between files would make "as fast as the clouds
// take it" mean three files a minute. A plan set or changed over HTTP wakes
// the loop at once, too, so "finish now" starts now rather than within the
// next twenty seconds.

// rekeyTick is how often the loop asks whether a file may move. Short enough
// that a budget raised in the panel takes effect within a minute, cheap enough
// to ask forever: a step that is not due costs one lock and a walk of the index.
const rekeyTick = 20 * time.Second

// rekeyStepTimeout bounds one file. Generous, because one file can be a film
// on a slow link; a step that outlives it is a cloud that stopped answering,
// and the file is left for the next try.
const rekeyStepTimeout = 12 * time.Hour

// rekeyMaxSleep caps how long the loop trusts a wait the meter gave it, so a
// plan whose budget was raised is picked up within this rather than at the
// end of the wait it was told under the old budget.
const rekeyMaxSleep = 5 * time.Minute

// rekeyWakeCh is the channel a plan change pokes to have the loop look now.
// Made on first use, because a Server is a struct literal with no constructor.
func (s *Server) rekeyWakeCh() chan struct{} {
	s.rekeyWakeOnce.Do(func() { s.rekeyWake = make(chan struct{}, 1) })
	return s.rekeyWake
}

// wakeRekey asks the loop to look at the plan now rather than on its next
// tick. Never blocks: a wake already waiting is as good as two.
func (s *Server) wakeRekey() {
	select {
	case s.rekeyWakeCh() <- struct{}{}:
	default:
	}
}

func (s *Server) rekeyLoop() {
	ticker := time.NewTicker(rekeyTick)
	defer ticker.Stop()

	var notBefore time.Time
	for {
		woken := false
		select {
		case <-ticker.C:
		case <-s.rekeyWakeCh():
			woken = true
		}

		v, err := s.Vault()
		if err != nil || !v.Unlocked() {
			continue
		}
		now := time.Now()
		if v.RekeyKeepsUnlocked() {
			s.noteExternalActivity()
		}
		// A wake is a changed plan, and a wait the old plan was told under
		// does not bind the new one.
		if now.Before(notBefore) && !woken {
			continue
		}

		step, err := s.rekeyRound(v)
		if err != nil && !errors.Is(err, vault.ErrLocked) {
			// Locked between the check and the step is ordinary; anything
			// else is worth a line.
			log.Printf("re-encryption: %v", err)
		}
		now = time.Now()
		switch {
		case step.Idle:
			notBefore = now.Add(time.Minute)
		case step.Wait > rekeyMaxSleep:
			notBefore = now.Add(rekeyMaxSleep)
		case step.Wait > 0:
			notBefore = now.Add(step.Wait)
		default:
			notBefore = time.Time{}
		}
	}
}

// rekeyRound takes steps back to back for as long as files move — or are
// skipped, which is a move the person stopped — and returns the first step
// that did not: the one that says how long to wait, or that the plan is idle.
// A file that would not move ends the round too, so a cloud that is down costs
// one try a tick rather than a tight loop of refusals.
//
// Every move attempted counts as use of the vault, the way a request does:
// the idle timer is for a vault nobody is touching, and a plan taking files one
// after another is touching it. Between rounds the timer is left alone, so a
// plan that is waiting on its meter does not keep the vault open.
func (s *Server) rekeyRound(v *vault.Vault) (vault.RekeyStep, error) {
	for {
		step, err := s.rekeyStep(v, time.Now())
		if err != nil {
			return step, err
		}
		switch {
		case step.Moved:
			log.Printf("re-encrypted %s (%s) under the current key", step.Path, formatBytes(step.Bytes))
			if step.Warning != "" {
				log.Printf("re-encryption: %s", step.Warning)
			}
		case step.Skipped:
			log.Printf("re-encryption: %s was skipped by request and left for last", step.Path)
		case step.Warning != "":
			log.Printf("re-encryption: %s", step.Warning)
		}
		if step.Moved || step.Skipped || step.Warning != "" {
			s.noteExternalActivity()
		}
		if !step.Moved && !step.Skipped {
			return step, nil
		}
		if !v.Unlocked() {
			return vault.RekeyStep{}, vault.ErrLocked
		}
	}
}

// rekeyStep runs one step with the idle timer held off for as long as the
// step takes, and no longer.
func (s *Server) rekeyStep(v *vault.Vault, now time.Time) (vault.RekeyStep, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rekeyStepTimeout)
	defer cancel()

	done := make(chan struct{})
	defer close(done)
	go func() {
		keep := time.NewTicker(time.Minute)
		defer keep.Stop()
		for {
			select {
			case <-done:
				return
			case <-keep.C:
				s.noteExternalActivity()
			}
		}
	}()

	return v.RekeyStep(ctx, now)
}

// handleRekeyStatus says where the re-encryption stands: how much is still on
// the previous key, folder by folder, and how the plan is taking it.
func (s *Server) handleRekeyStatus(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	status, err := v.RekeyStatus()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleRekeyFiles lists the files still on the previous key, a page at a
// time, in the order the plan takes them and with where each one stands:
// moving, waiting its turn, left for last, held for a cloud that is not
// answering, failed lately and waiting to be tried again. ?folder confines
// the list to one folder — the one whose count was tapped — and ?offset and
// ?limit window it. See vault.PendingRekeyFiles.
func (s *Server) handleRekeyFiles(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	offset, err := intParam(query.Get("offset"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "offset must be a whole number", "BAD_REQUEST")
		return
	}
	limit, err := intParam(query.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "limit must be a whole number", "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	page, err := v.PendingRekeyFiles(query.Get("folder"), offset, limit)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// rekeySkipRequest names a file to leave for last — the one being moved right
// now when ID is empty — or, with Undo, to put back in its turn.
type rekeySkipRequest struct {
	ID   string `json:"id"`
	Undo bool   `json:"undo"`
}

// handleRekeySkip leaves a file for last, stopping its move if it is the one
// being moved, and answers with the status as it stands afterwards.
func (s *Server) handleRekeySkip(w http.ResponseWriter, r *http.Request) {
	var req rekeySkipRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	var err error
	if req.Undo {
		_, err = v.UndeferRekeyFile(req.ID)
	} else {
		_, err = v.DeferRekeyFile(req.ID)
	}
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	status, err := v.RekeyStatus()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleRekeyDelete erases a file still on the previous key instead of
// re-encrypting it, stopping its move first if it is the one being moved.
// See vault.DeleteRekeyFile. The answer is the one DELETE /api/files/{id}
// gives — deleted, with a warning per part that could not be erased — so the
// list that asked can show the warnings and ask again for the rows.
func (s *Server) handleRekeyDelete(w http.ResponseWriter, r *http.Request) {
	// Detached, as a file delete is: once the erasing has begun the entry
	// is leaving the index whether or not the client stays to hear about it.
	// The timeout also bounds the wait for a move in flight to stop.
	ctx, cancel := detachedContextWithTimeout(r, 5*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	path, warnings, err := v.DeleteRekeyFile(ctx, r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	log.Printf("re-encryption: %s was deleted instead of being re-encrypted", path)
	// A move was stopped, or a file is gone from the plan: either way the
	// loop should look again now rather than at the end of a wait.
	s.wakeRekey()
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "path": path, "warnings": warnings})
}

// handleRekeyPlan sets how the outstanding re-encryption proceeds. The body is
// a vault.RekeySettings: the whole of it, each time, since a plan with one
// field changed is a different plan rather than a patch to the old one.
func (s *Server) handleRekeyPlan(w http.ResponseWriter, r *http.Request) {
	var req vault.RekeySettings
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	status, err := v.SetRekeyPlan(req)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	// Felt now, not at the next tick: a plan set to full speed is somebody
	// watching a progress bar.
	s.wakeRekey()
	writeJSON(w, http.StatusOK, status)
}
