package vault

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// Whether the clouds are still there, asked on a schedule rather than when
// somebody happens to look.
//
// Everything else that pings an account is something a person started. The
// sidebar pings when it is drawn, the Test button pings one account, a sweep
// pings them all before it checks a folder — and every one of those needs
// somebody sitting in front of the app. The failure this is here for is the
// one nobody is sitting in front of: an OAuth refresh token revoked in March,
// a bucket whose keys were rotated by somebody else on the team, a NAS that
// has been off since the power cut. None of that makes anything look broken.
// Files keep reading, because a file only needs k of its n parts and the
// clouds that are still answering carry it — right up until a second one goes
// and the file does not come back at all.
//
// So the clouds are asked on a timer, and what they said is kept where the
// accounts panel can put one line in front of somebody: 1 of 17 clouds
// unhealthy. That line is the whole point. The rest of this file is what makes
// it true without anybody asking for it.
//
// Three things about the shape of it, since they are what the code below is
// arguing for.
//
// It is a ping and nothing more. No listing, no usage call, no download —
// "answer me" and how long that took. A check that walked a bucket would cost
// real money at the providers that bill for listing, every hour, forever, and
// it would not answer a different question: an account that cannot be pinged
// cannot be listed either, and one that can is reachable, which is all this
// claims to know. What the account is actually *holding* is the health check
// on a folder's automation policy, which reads the index and asks after
// particular parts — a different job, an expensive one, and one that is
// deliberately opt-in per folder.
//
// What it finds lives in memory, not in the vault file. A ping result is true
// for about as long as it takes to read it: writing "backblaze was down at
// 14:05" into an encrypted file that has to be re-sealed to change is paying a
// disk write for a fact that expires in an hour. It also means a locked vault
// reports nothing rather than something stale, which is the honest answer —
// see lockLocked, which drops these along with the keys.
//
// And every other ping in the app feeds it. ProviderStatuses pings every
// account to draw the sidebar; TestProvider pings one because somebody pressed
// Test. Both record what they found here, so opening the drawer keeps the
// figure fresh and — because a full sweep of everything is exactly what the
// scheduled check does — pushes the next scheduled check out by an hour rather
// than duplicating one that just happened.

// DefaultHealthInterval is how often the clouds are asked when nobody has said
// otherwise. An hour: long enough that a dozen accounts cost a dozen requests a
// day each, short enough that a token revoked overnight is news in the morning
// rather than news next week.
const DefaultHealthInterval = time.Hour

// MinHealthInterval and MaxHealthInterval bound what the setting will take.
//
// The floor is not about load on anybody's servers — a ping every minute is
// nothing — but about what the figure means. A cloud that answers in 400ms
// nine times out of ten will miss occasionally, and an interval short enough to
// catch those turns the panel into a flicker of red that says nothing. The
// ceiling is a week, past which "checked recently" stops being a claim worth
// making at all.
const (
	MinHealthInterval = 5 * time.Minute
	MaxHealthInterval = 7 * 24 * time.Hour
)

// healthPingTimeout is how long one account gets to answer before it counts as
// unreachable. The same twenty seconds the sidebar's own ping allows, because
// this is the same ping — an account that needs longer than that is a problem
// whether or not it eventually answers.
const healthPingTimeout = 20 * time.Second

// CloudHealth is one account's standing: whether it answered last time it was
// asked, when that was, and what it said if it did not.
type CloudHealth struct {
	ID   string        `json:"id"`
	Name string        `json:"name"`
	Kind provider.Kind `json:"kind"`

	// Checked says this account has been asked at all since the vault was
	// opened. It is the difference between "we know it is fine" and "we have
	// not looked", and the panel says so rather than colouring an unasked
	// account green.
	Checked bool `json:"checked"`

	Healthy   bool      `json:"healthy"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitzero"`

	// Took is how long the answer took, in milliseconds. Kept because a cloud
	// that answers in eleven seconds is on its way to being a cloud that does
	// not answer, and that is visible here before it is visible anywhere else.
	Took int64 `json:"took_ms,omitempty"`

	// FailingSince is when this account *started* failing, carried across
	// checks for as long as it keeps failing. "Unreachable since 03:40" is a
	// different problem from "unreachable just now", and only one of them is
	// worth getting out of bed for.
	FailingSince time.Time `json:"failing_since,omitzero"`
}

// HealthSchedule is how often the check comes round, and whether it does.
type HealthSchedule struct {
	Enabled bool `json:"enabled"`

	// Minutes is the interval. In minutes rather than as a duration because
	// this crosses to a browser, where a Go duration is an unreadable count of
	// nanoseconds, and because a minute is finer than anybody sets this.
	Minutes int `json:"interval_minutes"`
}

// Interval is the schedule as a duration, with the default filled in for a
// vault that has never chosen one.
func (s HealthSchedule) Interval() time.Duration {
	if s.Minutes <= 0 {
		return DefaultHealthInterval
	}
	return time.Duration(s.Minutes) * time.Minute
}

// HealthReport is every connected account's standing at once, which is what the
// accounts panel draws its one line from.
//
// It is built against the accounts as they are right now rather than as they
// were when the last check ran. Connecting a cloud a minute ago must not make
// the panel say "16 clouds" until the next sweep, and disconnecting one must
// not leave it being counted as unhealthy forever — so the list here is the
// current accounts, each carrying whatever was last found out about it.
type HealthReport struct {
	Clouds []CloudHealth `json:"clouds"`

	// The counts the sentence is made of. Accounts is every connected one;
	// Unchecked are the ones nothing has asked yet, which are neither healthy
	// nor unhealthy and must not be counted as either.
	Accounts  int `json:"accounts"`
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
	Unchecked int `json:"unchecked"`

	// CheckedAt is when the last sweep of *every* account finished — the "as
	// of" on the figure. A single account tested on its own updates that
	// account's own CheckedAt and deliberately not this one.
	CheckedAt time.Time `json:"checked_at,omitzero"`

	Schedule HealthSchedule `json:"schedule"`

	// NextCheckAt is when the scheduler will next come round, absent when the
	// schedule is off. Worked out here rather than in the browser because the
	// browser's clock is not the one the sweep runs on.
	NextCheckAt time.Time `json:"next_check_at,omitzero"`
}

// HealthSchedule returns how often the connected accounts are checked.
//
// Readable while the vault is locked, like the placement policy and for the
// same reason: it is a setting kept in the clear, and a caller deciding whether
// to start a scheduler should not have to unlock anything to find out.
func (v *Vault) HealthSchedule() HealthSchedule {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.healthScheduleLocked()
}

func (v *Vault) healthScheduleLocked() HealthSchedule {
	if v.store == nil {
		return HealthSchedule{Enabled: true, Minutes: int(DefaultHealthInterval / time.Minute)}
	}
	return HealthSchedule{
		// Stored as the negative — see storeFile.HealthCheckOff — so that a
		// vault written before this existed, which has no field at all, is a
		// vault with the check switched on.
		Enabled: !v.store.HealthCheckOff,
		Minutes: v.healthMinutesLocked(),
	}
}

func (v *Vault) healthMinutesLocked() int {
	if v.store == nil || v.store.HealthCheckMinutes <= 0 {
		return int(DefaultHealthInterval / time.Minute)
	}
	return v.store.HealthCheckMinutes
}

// SetHealthSchedule changes how often the clouds are checked, or switches the
// check off.
//
// Switching it off keeps the interval rather than forgetting it, so turning it
// back on returns to the schedule somebody chose rather than to the default.
func (v *Vault) SetHealthSchedule(s HealthSchedule) (HealthSchedule, error) {
	if s.Minutes != 0 {
		interval := time.Duration(s.Minutes) * time.Minute
		if interval < MinHealthInterval || interval > MaxHealthInterval {
			return HealthSchedule{}, fmt.Errorf(
				"check the clouds at most every %s and at least every %s",
				MinHealthInterval, MaxHealthInterval)
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return HealthSchedule{}, ErrLocked
	}

	v.store.HealthCheckOff = !s.Enabled
	if s.Minutes != 0 {
		v.store.HealthCheckMinutes = s.Minutes
	}
	if err := v.persistLocked(); err != nil {
		return HealthSchedule{}, err
	}
	return v.healthScheduleLocked(), nil
}

// CloudHealth answers with what is already known, contacting nobody.
//
// This is what the accounts panel polls. It is a read of a map in memory, so
// it costs nothing and can be asked for as often as anybody likes — which is
// the property that lets the figure in the sidebar stay live without every
// browser tab in the house pinging seventeen clouds on its own timer.
func (v *Vault) CloudHealth() (HealthReport, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return HealthReport{}, ErrLocked
	}
	configs := append([]provider.Config(nil), v.providers...)
	schedule := v.healthScheduleLocked()
	v.mu.RUnlock()

	return v.reportFor(configs, schedule), nil
}

// CheckClouds asks every connected account whether it is there, records what
// they said, and answers with the result.
//
// This is the sweep — the thing the scheduler runs on the hour and the thing
// the "Check now" button runs on demand. Both go through here so there is one
// definition of what a check is.
func (v *Vault) CheckClouds(ctx context.Context) (HealthReport, error) {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return HealthReport{}, ErrLocked
	}
	configs := append([]provider.Config(nil), v.providers...)
	schedule := v.healthScheduleLocked()
	v.mu.RUnlock()

	found := v.pingAll(ctx, configs)
	v.recordSweep(time.Now(), found)
	return v.reportFor(configs, schedule), nil
}

// HealthCheckDue reports whether the scheduler should sweep now.
//
// "Due" is a comparison against when the last sweep happened rather than a
// timer that has to have been running, which is what makes a missed slot
// harmless: a vault unlocked at nine after being shut all night is swept at
// nine, not at ten. A vault that has been open a while and has never been
// swept is due immediately, which is what makes the first check land shortly
// after unlocking rather than an hour later.
func (v *Vault) HealthCheckDue(now time.Time) bool {
	schedule := v.HealthSchedule()
	if !schedule.Enabled {
		return false
	}

	v.healthMu.Lock()
	last := v.healthSweptAt
	v.healthMu.Unlock()

	if last.IsZero() {
		return true
	}
	return !now.Before(last.Add(schedule.Interval()))
}

// pingAll asks every account at once and times each answer.
//
// All at once because they are independent and the slowest one should decide
// how long the sweep takes, not the sum of them; and with a deadline each,
// because an account that has stopped answering usually stops by never
// replying rather than by refusing.
func (v *Vault) pingAll(ctx context.Context, configs []provider.Config) []CloudHealth {
	found := make([]CloudHealth, len(configs))

	var wg sync.WaitGroup
	for i, cfg := range configs {
		found[i] = CloudHealth{ID: cfg.ID, Name: cfg.Name, Kind: cfg.Kind, Checked: true}

		wg.Add(1)
		go func(i int, cfg provider.Config) {
			defer wg.Done()

			started := time.Now()
			p, err := v.buildProvider(cfg)
			if err == nil {
				pingCtx, cancel := context.WithTimeout(ctx, healthPingTimeout)
				defer cancel()
				err = p.Ping(pingCtx)
			}

			found[i].CheckedAt = time.Now()
			found[i].Took = time.Since(started).Milliseconds()
			if err != nil {
				found[i].Error = err.Error()
				return
			}
			found[i].Healthy = true
		}(i, cfg)
	}
	wg.Wait()

	return found
}

// recordSweep files what a check of every account found, and moves the clock
// the next scheduled check is measured from.
func (v *Vault) recordSweep(at time.Time, found []CloudHealth) {
	v.healthMu.Lock()
	defer v.healthMu.Unlock()

	for _, one := range found {
		v.rememberLocked(one)
	}
	v.healthSweptAt = at
}

// rememberOne files what a ping of a single account found — the Test button,
// or an account that has just been connected or re-authorized.
//
// Deliberately without touching the sweep clock. One account answering says
// nothing about the other sixteen, so it must not push the sweep that would
// have asked them out by another hour.
func (v *Vault) rememberOne(one CloudHealth) {
	v.healthMu.Lock()
	defer v.healthMu.Unlock()
	v.rememberLocked(one)
}

// rememberLocked stores one result, carrying forward how long a failing
// account has been failing. The caller holds healthMu.
func (v *Vault) rememberLocked(one CloudHealth) {
	if one.ID == "" {
		return
	}
	if v.healthSeen == nil {
		v.healthSeen = map[string]CloudHealth{}
	}

	// A cloud that was already down keeps the moment it went down. Anything
	// else — healthy now, or failing for the first time — starts the clock
	// where this check found it, so "unreachable for 3 days" is a fact about
	// the cloud rather than about how long the app has been running.
	if !one.Healthy {
		if was, ok := v.healthSeen[one.ID]; ok && was.Checked && !was.Healthy && !was.FailingSince.IsZero() {
			one.FailingSince = was.FailingSince
		} else if one.FailingSince.IsZero() {
			one.FailingSince = one.CheckedAt
		}
	}
	v.healthSeen[one.ID] = one
}

// forgetHealth drops what was known about an account that is no longer
// connected, so a disconnected cloud stops being counted the moment it goes
// rather than at the next sweep.
func (v *Vault) forgetHealth(id string) {
	v.healthMu.Lock()
	defer v.healthMu.Unlock()
	delete(v.healthSeen, id)
}

// forgetAllHealth drops every result. Called when the vault locks: these are
// facts about accounts whose names live in the section that just went away.
func (v *Vault) forgetAllHealth() {
	v.healthMu.Lock()
	defer v.healthMu.Unlock()
	v.healthSeen = nil
	v.healthSweptAt = time.Time{}
}

// reportFor builds the answer against the accounts as they are now.
func (v *Vault) reportFor(configs []provider.Config, schedule HealthSchedule) HealthReport {
	v.healthMu.Lock()
	seen := make(map[string]CloudHealth, len(v.healthSeen))
	for id, one := range v.healthSeen {
		seen[id] = one
	}
	swept := v.healthSweptAt
	v.healthMu.Unlock()

	report := HealthReport{
		Clouds:    make([]CloudHealth, 0, len(configs)),
		Accounts:  len(configs),
		CheckedAt: swept,
		Schedule:  schedule,
	}
	if schedule.Enabled && !swept.IsZero() {
		report.NextCheckAt = swept.Add(schedule.Interval())
	}

	for _, cfg := range configs {
		one, ok := seen[cfg.ID]
		if !ok {
			// Connected since the last check, or connected while the sweep was
			// in flight. Neither healthy nor unhealthy — nothing has asked.
			one = CloudHealth{ID: cfg.ID}
		}
		// The name and kind are read from the account rather than from what the
		// check recorded, so a cloud renamed since is reported under the name
		// the panel beside it is showing.
		one.Name, one.Kind = cfg.Name, cfg.Kind

		switch {
		case !one.Checked:
			report.Unchecked++
		case one.Healthy:
			report.Healthy++
		default:
			report.Unhealthy++
		}
		report.Clouds = append(report.Clouds, one)
	}

	// Worst first: the reason to open this list is to see what is wrong with
	// it, and on a vault with seventeen accounts the one that is down should
	// not be somewhere in the middle. Unchecked accounts sit between the two,
	// since "we have not asked" is a smaller worry than "it said no". Ties keep
	// the order the accounts are connected in, which is the order the cards
	// above are in.
	sort.SliceStable(report.Clouds, func(i, j int) bool {
		return healthRank(report.Clouds[i]) < healthRank(report.Clouds[j])
	})

	return report
}

func healthRank(c CloudHealth) int {
	switch {
	case c.Checked && !c.Healthy:
		return 0
	case !c.Checked:
		return 1
	default:
		return 2
	}
}

// healthFromStatuses turns the sidebar's own ping of every account into check
// results, so drawing the accounts panel counts as a sweep.
//
// It is the same ping against the same accounts at the same moment; treating it
// as anything else would mean pinging seventeen clouds twice in a minute and
// then telling somebody that the figure they are looking at is an hour old.
func healthFromStatuses(statuses []ProviderStatus) []CloudHealth {
	found := make([]CloudHealth, 0, len(statuses))
	now := time.Now()
	for _, st := range statuses {
		found = append(found, CloudHealth{
			ID:        st.ID,
			Name:      st.Name,
			Kind:      st.Kind,
			Checked:   true,
			Healthy:   st.Online,
			Error:     st.Error,
			CheckedAt: now,
		})
	}
	return found
}
