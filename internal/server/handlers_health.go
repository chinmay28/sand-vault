package server

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// Whether the clouds are still answering, over HTTP.
//
// Three endpoints and only one of them contacts anybody. Reading the standing
// is a look at a map in memory and answers in microseconds, which is what lets
// the accounts panel poll it while it is open; running a check pings every
// connected account, so it gets a deadline of its own; and the schedule is a
// setting like any other. See internal/vault/health.go for what a check is and
// why it is a ping and nothing more.

// healthCheckTimeout is the ceiling on one sweep.
//
// Generous for what it is — every account is pinged at once and each ping gives
// up after twenty seconds — because the accounts are pinged in parallel and the
// slowest one decides. A minute is room for a backend that has to sign in
// before it can answer, which is what an expired OAuth token looks like on the
// way to being refreshed.
const healthCheckTimeout = 90 * time.Second

// handleCloudHealth answers with what is already known: which accounts
// answered when they were last asked, when that was, and when the next check
// is due. Nothing is contacted.
func (s *Server) handleCloudHealth(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	report, err := v.CloudHealth()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"health": report})
}

// handleCloudHealthCheck runs a check now, whether or not one is due.
//
// What somebody presses after fixing the cloud that was red, rather than
// waiting an hour to find out whether the fix took.
func (s *Server) handleCloudHealthCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, healthCheckTimeout)
	defer cancel()

	v, _ := s.Vault()
	report, err := v.CheckClouds(ctx)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"health": report})
}

// healthScheduleRequest is the setting as the browser sends it. Enabled is a
// pointer so that a request changing only the interval leaves the switch alone,
// and one flicking the switch leaves the interval alone.
type healthScheduleRequest struct {
	Enabled *bool `json:"enabled"`
	Minutes int   `json:"interval_minutes"`
}

func (s *Server) handleCloudHealthSchedule(w http.ResponseWriter, r *http.Request) {
	var req healthScheduleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	schedule := v.HealthSchedule()
	if req.Enabled != nil {
		schedule.Enabled = *req.Enabled
	}
	// Zero means "leave it", which is what the vault reads it as too — see
	// SetHealthSchedule.
	schedule.Minutes = req.Minutes

	saved, err := v.SetHealthSchedule(schedule)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	// Answered with the whole standing rather than with the schedule alone: the
	// panel that changed this is showing the figure beside it, and a new
	// interval moves when the next check lands.
	report, err := v.CloudHealth()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"schedule": saved})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"health": report, "schedule": saved})
}

// healthTick is how often the scheduler wakes to see whether a check is due. A
// minute, the same as the automation scheduler's, because the comparison is
// arithmetic over one timestamp — no account is contacted unless something is
// actually due.
const healthTick = time.Minute

// healthLoop pings the connected accounts on the vault's own schedule.
//
// Like the automation scheduler it runs only while the vault is unlocked, and
// for the same reason: the accounts and their credentials live in the encrypted
// section, so a locked vault has nothing to ping and nothing to ping it with. A
// slot that passes while the vault is shut is not lost — "due" is a comparison
// against the last check rather than a timer that has to have been running, so
// a vault opened at nine after being shut all night is checked at nine.
//
// Nothing here counts as activity for the idle timer, which is deliberate and
// the opposite of what the automation sweep does. A sweep that rebuilds files
// must not be locked out half way through; a ping must not keep a vault open
// forever — an hourly check that renewed the idle timer would mean no machine
// running SAND ever auto-locked again.
func (s *Server) healthLoop() {
	ticker := time.NewTicker(healthTick)
	defer ticker.Stop()

	// What was wrong last time round, so a cloud going down is said once and a
	// cloud coming back is said once, rather than the same line every hour
	// forever. By ID, since that is what survives a rename.
	failing := map[string]bool{}

	for range ticker.C {
		v, err := s.Vault()
		if err != nil || !v.Unlocked() || !v.HealthCheckDue(time.Now()) {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
		report, err := v.CheckClouds(ctx)
		cancel()
		if err != nil {
			// A vault locked between the check above and the sweep itself is
			// the ordinary case here, and is not news.
			continue
		}
		failing = logHealthChanges(report, failing)
	}
}

// logHealthChanges says what changed, and returns what is wrong now.
//
// Only the changes. This runs every hour for as long as the machine is up, and
// a log line per sweep saying everything is fine would bury the one line that
// matters under a hundred that do not — which is the opposite of the automation
// log's argument, because that one runs when somebody asked for a schedule and
// this one runs on every install by default.
func logHealthChanges(report vault.HealthReport, was map[string]bool) map[string]bool {
	now := map[string]bool{}
	var broke, mended []string

	for _, cloud := range report.Clouds {
		if !cloud.Checked {
			continue
		}
		if !cloud.Healthy {
			now[cloud.ID] = true
			if !was[cloud.ID] {
				// The reason travels with the name: "token expired" and "no
				// route to host" are the same colour in the app and are not
				// remotely the same problem.
				broke = append(broke, cloud.Name+" ("+cloud.Error+")")
			}
			continue
		}
		if was[cloud.ID] {
			mended = append(mended, cloud.Name)
		}
	}

	sort.Strings(broke)
	sort.Strings(mended)

	if len(broke) > 0 {
		log.Printf("cloud health: %s — %d of %d not answering",
			strings.Join(broke, "; "), report.Unhealthy, report.Accounts)
	}
	if len(mended) > 0 {
		log.Printf("cloud health: %s answering again", strings.Join(mended, ", "))
	}

	return now
}
