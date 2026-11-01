package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// One vault on several machines, over HTTP.
//
// Two endpoints. The register of machines is a look at the index in memory
// and answers at once; a pull reads one small object from every account this
// machine reaches and folds the copies in, so it gets a deadline of its own.
// See internal/vault/sync.go for what a pull is, and instance.go for what a
// machine is.

// syncTimeout bounds one pull: a read of the index copy from every account at
// once, the slowest deciding, plus the write.
const syncTimeout = 3 * time.Minute

// syncTick is how often the scheduler wakes to see whether a pull is due.
const syncTick = 30 * time.Second

// handleInstances answers with the machines that open this vault: this one
// first, each with when it last wrote the index and which accounts it reaches.
func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	instances, err := v.Instances()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instance":  v.Instance(),
		"instances": instances,
		"last_sync": zeroAsNil(v.LastSync()),
	})
}

// handleSync pulls the shared index now, whether or not one is due, and says
// what came of it: what somebody presses on the Pi after uploading on the desk,
// rather than waiting the few minutes for the scheduler.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, syncTimeout)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.SyncIndex(ctx)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// zeroAsNil turns the zero time into JSON null, so "never" is not a date in
// the year one.
func zeroAsNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// syncAfterUnlock pulls the shared index on a goroutine of its own, right after
// an unlock, so the first listing somebody opens is as current as the copies
// on the accounts can make it. Nothing waits on it: a pull is a round trip to
// every account, and the unlock has already answered.
func (s *Server) syncAfterUnlock(v *vault.Vault) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		defer cancel()
		report, err := v.SyncIndex(ctx)
		if err != nil && err != vault.ErrLocked {
			log.Printf("could not pull the shared index after unlocking: %v", err)
			return
		}
		logSyncReport(report)
	}()
}

// syncLoop pulls the shared index on its schedule: every few minutes once
// another machine is known to open the vault, every hour until one is, and
// not at all while the vault is locked or its index backup is off. The vault
// says when (SyncDue); this only asks.
//
// Nothing here counts as activity for the idle timer, for the reason the
// health check does not: a pull on a timer must not keep a vault open forever.
func (s *Server) syncLoop() {
	ticker := time.NewTicker(syncTick)
	defer ticker.Stop()

	for range ticker.C {
		v, err := s.Vault()
		if err != nil || !v.Unlocked() || !v.SyncDue(time.Now()) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		report, err := v.SyncIndex(ctx)
		cancel()
		if err != nil {
			// A vault locked between the check and the pull is the ordinary
			// case here, and is not news.
			if err != vault.ErrLocked {
				log.Printf("could not pull the shared index: %v", err)
			}
			continue
		}
		logSyncReport(report)
	}
}

// logSyncReport says what a pull did, when it did anything.
func logSyncReport(report *vault.SyncReport) {
	if report == nil || !report.Changed() {
		return
	}
	log.Printf("shared index %s: %d file(s) added, %d removed, %d changed",
		report.Outcome, report.Added, report.Removed, report.Updated)
	for _, line := range report.Renamed {
		log.Printf("shared index: renamed %s — both machines stored a file there", line)
	}
	for _, line := range report.Warnings {
		log.Printf("shared index: %s", line)
	}
}
