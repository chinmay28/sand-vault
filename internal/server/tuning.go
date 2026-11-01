package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/chinmay28/sand-vault/internal/compress"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/tuning"
	"github.com/chinmay28/sand-vault/internal/vault"
)

// Sizing SAND to the machine.
//
// The vault, the compressor and the provider layer each take a handful of
// numbers — windows, a cache, a chunk size, a pool — that decide how much is
// in flight and in memory at once. Where those numbers come from is
// internal/tuning; this file is what reads the operator's choice from beside
// the vault, works it out on the machine the process found itself on, hands
// the result to each layer, and answers the settings page that changes it.

// machineState is the server's view of the tuning: what was chosen, what it
// resolved to, and where it is kept.
type machineState struct {
	mu       sync.Mutex
	path     string
	config   tuning.Config
	resolved tuning.Resolved
	// pinned is true when the profile came from --profile or SAND_PROFILE
	// rather than the file, in which case the page may change the knobs but
	// not the profile: the flag would put it back at the next restart, and a
	// setting that reverts on its own is worse than one that says no.
	pinned bool
}

// loadTuning reads the machine config beside the vault, resolves it on the
// machine detected, and applies it. Called once, when the vault is opened.
//
// A config that will not resolve — a hand edit that put a window at zero,
// say — is reported and the defaults are applied, since a server that will
// not start over a tuning file is a worse failure than one that runs at the
// Pi's numbers and says why.
func (s *Server) loadTuning(v *vault.Vault) {
	path := tuning.PathFor(v.Path())
	config, err := tuning.Load(path)
	if err != nil {
		log.Printf("machine: %v; running on defaults", err)
		config = tuning.Config{}
	}

	pinned := false
	if s.Profile != "" {
		if p := tuning.Profile(s.Profile); p.Valid() {
			config.Profile, pinned = p, true
		} else {
			log.Printf("machine: --profile %q is not a profile; using the file's", s.Profile)
		}
	}

	detected := tuning.Detect()
	resolved, err := tuning.Resolve(config, detected)
	if err != nil {
		log.Printf("machine: %s cannot be applied here (%v); running on defaults", path, err)
		config = tuning.Config{Profile: config.Profile}
		if resolved, err = tuning.Resolve(config, detected); err != nil {
			// The profile itself does not fit — a workstation pin on a Pi.
			config = tuning.Config{}
			resolved, _ = tuning.Resolve(config, detected)
			pinned = false
		}
	}

	s.machine = &machineState{path: path, config: config, resolved: resolved, pinned: pinned}
	s.applyTuning(v, resolved)
}

// applyTuning hands a resolved tuning to each layer that reads one.
func (s *Server) applyTuning(v *vault.Vault, r tuning.Resolved) {
	t := r.Effective
	v.SetTuning(t)
	compress.Configure(compress.Options{Level: t.CompressLevel, Threads: t.CompressThreads})
	provider.ConfigureTransport(t.ConnsPerHost)
	log.Printf("machine: %s profile on %d CPUs and %s — %s chunks, upload window %d, read-ahead %d, cache %s",
		r.Profile, r.Machine.CPUs, tuning.FormatBytes(r.Machine.MemoryBytes),
		tuning.FormatBytes(t.ChunkSize), t.UploadWindow, t.ReadAhead, tuning.FormatBytes(t.ChunkCacheBytes))
}

// machineView is what the settings page is drawn from: the resolution, plus
// the vocabulary — every knob with its range, every profile with its label —
// so the page knows nothing the server does not tell it.
type machineView struct {
	tuning.Resolved
	Pinned    bool            `json:"pinned"`
	Footprint int64           `json:"footprint"`
	Knobs     []tuning.Knob   `json:"knobs"`
	Profiles  []profileChoice `json:"profiles"`
}

type profileChoice struct {
	ID      tuning.Profile  `json:"id"`
	Label   string          `json:"label"`
	Machine *tuning.Machine `json:"machine,omitempty"`
}

func (m *machineState) view() machineView {
	profiles := make([]profileChoice, 0, 4)
	for _, p := range tuning.Profiles() {
		choice := profileChoice{ID: p, Label: p.Label()}
		if machine, ok := tuning.PresetMachine(p); ok {
			choice.Machine = &machine
		}
		profiles = append(profiles, choice)
	}
	return machineView{
		Resolved:  m.resolved,
		Pinned:    m.pinned,
		Footprint: m.resolved.Effective.Footprint(),
		Knobs:     tuning.Knobs(),
		Profiles:  profiles,
	}
}

// handleMachineGet answers GET /api/machine: the tuning in effect and
// everything the page needs to change it.
func (s *Server) handleMachineGet(w http.ResponseWriter, r *http.Request) {
	m := s.machine
	m.mu.Lock()
	view := m.view()
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, view)
}

// machineRequest is the body of PUT /api/machine: the whole choice, each
// time. A profile is one thing and its overrides are part of it, so a patch
// that changed one knob would have to know every other, and sending the lot
// is simpler and harder to get wrong.
type machineRequest struct {
	Profile   tuning.Profile   `json:"profile"`
	Machine   *tuning.Machine  `json:"machine,omitempty"`
	Overrides map[string]int64 `json:"overrides,omitempty"`
}

// handleMachinePut answers PUT /api/machine: resolve the choice on this
// machine, refuse it if it does not fit, otherwise save it, apply it, and
// answer as GET does.
func (s *Server) handleMachinePut(w http.ResponseWriter, r *http.Request) {
	var req machineRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	v, err := s.Vault()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
		return
	}

	m := s.machine
	m.mu.Lock()
	defer m.mu.Unlock()

	config := tuning.Config{Profile: req.Profile, Machine: req.Machine, Overrides: req.Overrides}
	if config.Profile == "" {
		config.Profile = tuning.ProfileAuto
	}
	if m.pinned && config.Profile != m.config.Profile {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("the profile is set to %s by --profile on the server; change it there", m.config.Profile),
			"PROFILE_PINNED")
		return
	}
	// A custom profile keeps the machine it was given; any other profile
	// keeps whatever custom machine was last described, so that switching
	// away and back loses nothing.
	if config.Profile != tuning.ProfileCustom && config.Machine == nil {
		config.Machine = m.config.Machine
	}

	resolved, err := tuning.Resolve(config, m.resolved.Detected)
	if err != nil {
		code := "BAD_REQUEST"
		var verr *tuning.ValidationError
		if errors.As(err, &verr) {
			code = "OUT_OF_RANGE"
		}
		writeError(w, http.StatusBadRequest, err.Error(), code)
		return
	}
	if err := tuning.Save(m.path, config); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "SAVE_FAILED")
		return
	}
	m.config, m.resolved = config, resolved
	s.applyTuning(v, resolved)
	writeJSON(w, http.StatusOK, m.view())
}

// handleMachineReset answers DELETE /api/machine: back to automatic with
// nothing overridden, which is the state a fresh install is in. The file is
// rewritten rather than removed so that a hand edit is not what comes back
// after a reset.
func (s *Server) handleMachineReset(w http.ResponseWriter, r *http.Request) {
	v, err := s.Vault()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
		return
	}
	m := s.machine
	m.mu.Lock()
	defer m.mu.Unlock()

	config := tuning.Config{}
	if m.pinned {
		config.Profile = m.config.Profile
	}
	resolved, err := tuning.Resolve(config, m.resolved.Detected)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "RESOLVE_FAILED")
		return
	}
	if err := tuning.Save(m.path, config); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "SAVE_FAILED")
		return
	}
	m.config, m.resolved = config, resolved
	s.applyTuning(v, resolved)
	writeJSON(w, http.StatusOK, m.view())
}
