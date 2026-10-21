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

func (s *Server) rekeyLoop() {
	ticker := time.NewTicker(rekeyTick)
	defer ticker.Stop()

	var notBefore time.Time
	for range ticker.C {
		v, err := s.Vault()
		if err != nil || !v.Unlocked() {
			continue
		}
		now := time.Now()
		if v.RekeyKeepsUnlocked() {
			s.noteExternalActivity()
		}
		if now.Before(notBefore) {
			continue
		}

		step, err := s.rekeyStep(v, now)
		switch {
		case errors.Is(err, vault.ErrLocked):
			// Locked between the check and the step, which is ordinary.
		case err != nil:
			log.Printf("re-encryption: %v", err)
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

		switch {
		case step.Idle:
			notBefore = now.Add(time.Minute)
		case step.Wait > rekeyMaxSleep:
			notBefore = now.Add(rekeyMaxSleep)
		case step.Wait > 0:
			notBefore = now.Add(step.Wait)
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
	writeJSON(w, http.StatusOK, status)
}
