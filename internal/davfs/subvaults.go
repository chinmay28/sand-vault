package davfs

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/chinmay28/sand-vault/internal/vault"
	"golang.org/x/net/webdav"
)

// SubVaultShares serves one WebDAV share per sub vault, each at its own address
// and behind its own password.
//
// The vault's share at /dav/ presents the main vault and never a sub vault:
// a mounted drive is a folder every process running as that user can read,
// and what goes in a sub vault is what the main password should not reach.
// This is the other half of that arrangement. A sub vault can be mounted on
// its own, at /sub/<name>/, by whoever holds its password — and that mount
// presents the sub vault and nothing else. The main vault is not under it, no
// other sub vault is under it, and a MOVE whose destination is another share
// is refused at the prefix. Mounting one share isolates the rest of the vault
// from it; the app, which has the vault password, still sees every sub vault
// as it did.
//
// A sub vault is named in the address by its label or by its ID, the two names
// the CLI's --in accepts. The label is what someone types and what a file
// manager calls the mounted volume; the ID survives a rename. Each name gets
// an endpoint of its own, because the library writes the mount path into every
// listing it answers and so has to be told it exactly — but every endpoint for
// one sub vault shares one set of locks, so a client holding a file open under
// one name is not written over through the other.
type SubVaultShares struct {
	vault *vault.Vault
	opts  Options

	mu sync.Mutex
	// endpoints is keyed by the path segment that named the sub vault.
	endpoints map[string]*subShare
	// locks is keyed by sub vault ID, and shared by every segment naming it.
	locks map[string]webdav.LockSystem
}

// subShare is one endpoint and the sub vault it was built for, so a name that
// has come to mean a different sub vault — two of them renamed past each other
// — is noticed and rebuilt rather than answered with the wrong tree.
type subShare struct {
	id       string
	endpoint *Endpoint
}

// SubVaultHandler builds the shares. opts.Prefix is the path the shares sit
// under, e.g. "/sub"; each sub vault answers one segment below it. opts.Scope
// is ignored — every share here is a sub vault's.
func SubVaultHandler(v *vault.Vault, opts Options) *SubVaultShares {
	if opts.Realm == "" {
		opts.Realm = "SAND Vault"
	}
	return &SubVaultShares{
		vault:     v,
		opts:      opts,
		endpoints: map[string]*subShare{},
		locks:     map[string]webdav.LockSystem{},
	}
}

// InFlight is how many requests the shares are serving between them at this
// moment, for the same reader as Endpoint.InFlight.
func (s *SubVaultShares) InFlight() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, share := range s.endpoints {
		total += share.endpoint.InFlight()
	}
	return total
}

func (s *SubVaultShares) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := stripPrefix(s.opts.Prefix, r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	segment, _, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
	if segment == "" || segment == "." || segment == ".." {
		// The root of the shares lists nothing. There is no one password it
		// could ask for, and which sub vaults exist is what the vault
		// password is for knowing — the lock screen does not volunteer it,
		// and neither does this.
		http.Error(w, "name a sub vault: "+s.opts.Prefix+"/<name>/", http.StatusNotFound)
		return
	}

	id, err := s.vault.LookupSubVault(segment)
	switch {
	case errors.Is(err, vault.ErrLocked):
		// Nothing can be checked against a locked vault, let alone served
		// from it — see subVaultVerifier.
		http.Error(w, "the vault is locked; unlock it in the app, or mount the vault's own share, "+
			"and this sub vault's share will answer", http.StatusServiceUnavailable)
		return
	case errors.Is(err, vault.ErrNoSubVault):
		// Answered exactly as a wrong password is, so that whether a sub
		// vault by that name exists cannot be read off the status code by
		// someone guessing names at the port.
		unauthorized(w, s.opts.Realm, vault.Scope(segment))
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	endpoint, err := s.endpointFor(segment, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	endpoint.ServeHTTP(w, r)
}

// endpointFor returns the share answering at one segment, building it on first
// use. It is keyed by the segment rather than the sub vault because the mount
// path the library writes into its answers has to match the one asked for.
func (s *SubVaultShares) endpointFor(segment, id string) (*Endpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if share, ok := s.endpoints[segment]; ok && share.id == id {
		return share.endpoint, nil
	}

	// A new name is the moment to let go of old ones: a label that was
	// renamed away, or a sub vault that was deleted, leaves an endpoint
	// nothing will ask for again.
	for name, share := range s.endpoints {
		if current, err := s.vault.LookupSubVault(name); err != nil || current != share.id {
			delete(s.endpoints, name)
		}
	}

	locks, ok := s.locks[id]
	if !ok {
		locks = webdav.NewMemLS()
		s.locks[id] = locks
	}

	opts := s.opts
	opts.Prefix = s.opts.Prefix + "/" + segment
	opts.Scope = vault.Scope(id)
	endpoint, err := newEndpoint(s.vault, opts, locks)
	if err != nil {
		return nil, err
	}
	s.endpoints[segment] = &subShare{id: id, endpoint: endpoint}
	return endpoint, nil
}
