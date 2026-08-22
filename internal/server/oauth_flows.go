package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// oauthFlowTTL is how long a half-finished sign-in stays valid. Long enough to
// find a password manager and a second factor, short enough that an abandoned
// flow is not sitting around holding an authorization state.
const oauthFlowTTL = 15 * time.Minute

// maxOAuthFlows caps how many sign-ins can be in flight at once, so a stuck
// client cannot grow the map without bound.
const maxOAuthFlows = 16

// oauthFlow is one sign-in in progress: what was asked for, the secrets tying
// the redirect back to this request, and — once the provider has answered —
// the credentials waiting to become a connected account.
type oauthFlow struct {
	ID           string
	Kind         provider.Kind
	State        string
	Verifier     string
	ClientID     string
	ClientSecret string
	RedirectURI  string

	// ProviderID is the connected account this sign-in is replacing the
	// credentials of, when it is a reauthorization rather than a new
	// connection. It is what the app credentials were read from at the start,
	// and what the finish is checked against — a flow opened against one
	// account may not be spent on another.
	ProviderID string

	// Session is the browser session that started the flow. The redirect
	// itself arrives without a cookie — it is a cross-site navigation, and the
	// session cookie is SameSite=Strict — so the state parameter is what
	// authenticates the callback, and this is checked on the calls that come
	// back through the app.
	Session string

	CreatedAt time.Time

	// SignInURL is where the account holder has to go, for the sign-ins that
	// cannot send the browser there themselves. Proton's client prints one and
	// waits; there is no redirect to catch, so the link is shown instead — and
	// can be opened on another device, which is what lets a headless box
	// connect at all. Empty for every flow that redirects.
	SignInURL string

	// Filled in once the provider redirects back.
	Done    bool
	Err     string
	Options map[string]string
	Account string
}

// expired reports whether a flow has aged out.
func (f *oauthFlow) expired(now time.Time) bool {
	return now.Sub(f.CreatedAt) > oauthFlowTTL
}

// oauthFlowStore holds the sign-ins currently in flight.
type oauthFlowStore struct {
	mu    sync.Mutex
	flows map[string]*oauthFlow
}

func newOAuthFlowStore() *oauthFlowStore {
	return &oauthFlowStore{flows: map[string]*oauthFlow{}}
}

// start registers a new flow, minting its ID and state.
func (s *oauthFlowStore) start(flow *oauthFlow) (*oauthFlow, error) {
	id, err := randomToken()
	if err != nil {
		return nil, err
	}
	state, err := randomToken()
	if err != nil {
		return nil, err
	}

	flow.ID = id
	flow.State = state
	flow.CreatedAt = time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	// Over the cap, the oldest flow loses: whoever is actually at the keyboard
	// is the one who just started this.
	for len(s.flows) >= maxOAuthFlows {
		var oldestID string
		var oldest time.Time
		for id, existing := range s.flows {
			if oldestID == "" || existing.CreatedAt.Before(oldest) {
				oldestID, oldest = id, existing.CreatedAt
			}
		}
		delete(s.flows, oldestID)
	}

	s.flows[flow.ID] = flow
	return flow, nil
}

// byID returns a live flow.
func (s *oauthFlowStore) byID(id string) (*oauthFlow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	flow, ok := s.flows[id]
	return flow, ok
}

// byState returns the flow a redirect belongs to. The state is the only thing
// the callback carries, so it is compared in constant time.
func (s *oauthFlowStore) byState(state string) (*oauthFlow, bool) {
	if state == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	for _, flow := range s.flows {
		if subtle.ConstantTimeCompare([]byte(flow.State), []byte(state)) == 1 {
			return flow, true
		}
	}
	return nil, false
}

// setSignInURL records where a sign-in that cannot redirect wants the account
// holder to go, for the browser to pick up on its next poll. A flow that has
// aged out or been finished is left alone.
func (s *oauthFlowStore) setSignInURL(id, url string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	flow, ok := s.flows[id]
	if !ok || flow.Done {
		return
	}
	flow.SignInURL = url
}

// finish records the outcome of a callback against a flow.
func (s *oauthFlowStore) finish(id string, options map[string]string, account, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	flow, ok := s.flows[id]
	if !ok {
		return
	}
	flow.Done = true
	flow.Options = options
	flow.Account = account
	flow.Err = errText

	// The authorization code has been spent either way; retiring the state
	// stops a replayed redirect from starting a second exchange.
	flow.State = ""
}

// drop removes a flow, once it has become an account or been abandoned.
func (s *oauthFlowStore) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.flows, id)
}

// sweepLocked discards aged-out flows. The caller holds the lock.
func (s *oauthFlowStore) sweepLocked() {
	now := time.Now()
	for id, flow := range s.flows {
		if flow.expired(now) {
			delete(s.flows, id)
		}
	}
}

// randomToken mints a 256-bit URL-safe secret.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// sameSecret compares two secrets without leaking their contents through
// timing.
func sameSecret(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
