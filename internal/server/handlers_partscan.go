package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// The scan of the local disks, from the server's side: see
// internal/vault/partscan.go for what it is and why. Here is the clock that
// runs it on the vault's schedule, and the three calls the browser makes —
// where it stands, run it now, change how often.

// partScanTick is how often the loop asks whether a scan is due. The
// schedule is in hours and days, so a minute is finer than it needs.
const partScanTick = time.Minute

// partScanTimeout bounds one scan. A listing per disk, so generous is still
// short; a disk that has stopped answering is cut off here rather than
// holding the next scan up for good.
const partScanTimeout = 30 * time.Minute

// partScanLoop scans the local disks on the vault's own schedule, while the
// vault is unlocked. Like the health loop it counts as no activity for the
// idle timer: a daily listing must not keep a vault open.
func (s *Server) partScanLoop() {
	ticker := time.NewTicker(partScanTick)
	defer ticker.Stop()
	for range ticker.C {
		v, err := s.Vault()
		if err != nil || !v.Unlocked() || !v.PartScanDue(time.Now()) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), partScanTimeout)
		report, err := v.ScanLocalParts(ctx)
		cancel()
		if err != nil {
			// Locked between the check and the scan, or a scan somebody
			// started by hand has the slot: neither is news.
			continue
		}
		logPartScan(report)
	}
}

// logPartScan says what a scan found, when it found anything: a part gone is
// worth a line, a clean scan every day is not.
func logPartScan(report *vault.PartScanReport) {
	for _, a := range report.Accounts {
		if a.Error != "" {
			log.Printf("local disk scan: %s could not be listed: %s", a.Name, a.Error)
		}
	}
	if report.Lost > 0 || report.Recovered > 0 {
		log.Printf("local disk scan: %d part(s) of %d missing or truncated across %d file(s), %d back — see Files missing a part",
			report.Lost, report.Parts, report.Files, report.Recovered)
	}
}

// handlePartScanStatus says where the scan stands. Contacts nobody.
func (s *Server) handlePartScanStatus(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	status, err := v.PartScanStatus()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handlePartScanRun runs a scan now, whether or not one is due, and answers
// with the standing afterwards. 409 PART_SCAN_BUSY while one is running.
func (s *Server) handlePartScanRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, partScanTimeout)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.ScanLocalParts(ctx)
	if err != nil {
		if err == vault.ErrPartScanBusy {
			writeError(w, http.StatusConflict, err.Error(), "PART_SCAN_BUSY")
			return
		}
		vaultErrorResponse(w, err)
		return
	}
	logPartScan(report)
	status, err := v.PartScanStatus()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// partScanScheduleRequest is the setting as the browser sends it. Enabled is
// a pointer so a request changing the interval leaves it alone, and a zero
// interval means "leave it" — see vault.SetPartScanSchedule.
type partScanScheduleRequest struct {
	Enabled *bool `json:"enabled"`
	Minutes int   `json:"interval_minutes"`
}

func (s *Server) handlePartScanSchedule(w http.ResponseWriter, r *http.Request) {
	var req partScanScheduleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	v, _ := s.Vault()
	schedule := v.PartScanSchedule()
	if req.Enabled != nil {
		schedule.Enabled = *req.Enabled
	}
	schedule.Minutes = req.Minutes
	if _, err := v.SetPartScanSchedule(schedule); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	status, err := v.PartScanStatus()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
