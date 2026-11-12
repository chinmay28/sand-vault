// Package tvaccess hands a television the key to one folder.
//
// A TV has a remote with arrows on it and nothing else. Typing the vault
// password on it would be miserable, and handing it the vault password at all
// would give it everything: every folder, every sub vault's door, the settings
// that delete files. What it wants is narrower — the films in one folder, for
// the evening — so that is what it is given.
//
// The owner, already signed in on a computer, asks for a code for a folder and
// a length of time. The code is four characters, short enough to read off one
// screen and key into another with a D-pad. The TV trades it for a device
// token, a long random bearer secret, and from then on presents the token. The
// token is good for what the code was good for — that folder, until the code
// expires — and nothing else.
//
// Four characters is a small space, about a million codes, so a code is only
// as safe as the rate at which wrong guesses are allowed. Redeem keeps a count
// of failures per client and across all clients, and refuses everything —
// right codes included — once either runs over, until the window has passed.
// See Throttle.
//
// Everything is in memory. A code that outlived the process would be a code
// that outlived the person who decided to hand it out, and restarting the
// server is how a user who has lost track of what they shared starts over.
package tvaccess

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Alphabet is the characters a code is drawn from: capitals and digits, less
// the four that are read as one another from across a room — 0 and O, 1 and I.
// Thirty-two of them, which is also what makes drawing one unbiased: a random
// byte modulo 32 favours no character.
const Alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// CodeLength is how many characters a code has.
const CodeLength = 4

// The bounds on how long a code lasts. Five minutes is the shortest time in
// which anybody gets from a laptop to a sofa and through a pairing screen;
// thirty days is long enough for "the kids' films, on the playroom TV" and short
// enough that a code nobody remembers handing out does eventually stop.
const (
	MinTTL     = 5 * time.Minute
	MaxTTL     = 30 * 24 * time.Hour
	DefaultTTL = 4 * time.Hour
)

// MaxGrants is how many codes can be live at once. Each one is a door, and a
// store that minted them without limit would make the space a guesser is
// searching denser with every one.
const MaxGrants = 32

// MaxDevicesPerGrant is how many TVs may pair with one code. A code read aloud
// across a living room reaches more than one screen; a code that has paired
// with a hundred has leaked.
const MaxDevicesPerGrant = 8

var (
	// ErrBadCode is a code that names no live grant — mistyped, expired,
	// revoked, or never issued. The four are not told apart, since telling a
	// guesser which it hit is telling them something.
	ErrBadCode = errors.New("that code is not valid — check it, or ask for a new one")

	// ErrThrottled is a redemption refused because too many wrong codes have
	// been tried recently. A ThrottledError carries how long to wait.
	ErrThrottled = errors.New("too many wrong codes — wait a few minutes and try again")

	// ErrBadTTL is a lifetime outside MinTTL..MaxTTL.
	ErrBadTTL = fmt.Errorf("a code must last between %s and %s", MinTTL, durationWords(MaxTTL))

	// ErrTooManyGrants is an issue refused because MaxGrants are live.
	ErrTooManyGrants = fmt.Errorf("%d codes are already live — revoke one first", MaxGrants)

	// ErrTooManyDevices is a correct code that has already paired with
	// MaxDevicesPerGrant devices.
	ErrTooManyDevices = fmt.Errorf("this code has already paired %d devices — ask for a new one", MaxDevicesPerGrant)
)

// ThrottledError is ErrThrottled with the wait attached.
type ThrottledError struct {
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string { return ErrThrottled.Error() }

// Is makes errors.Is(err, ErrThrottled) true.
func (e *ThrottledError) Is(target error) bool { return target == ErrThrottled }

// Grant is one code and what it opens.
type Grant struct {
	Code string `json:"code"`

	// Scope names the vault inside the file the folder is in — empty for the
	// main vault. It is a string here so this package needs nothing from the
	// vault's.
	Scope string `json:"vault,omitempty"`

	// Dir is the folder, as the vault spells it. Everything the device asks
	// for is confined beneath it by the caller.
	Dir string `json:"path"`

	// Label is what the TV calls the folder, so it can say what it is showing
	// without being told where in the vault that is.
	Label string `json:"label"`

	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`

	// Devices is how many TVs are paired under the code right now.
	Devices int `json:"devices"`
}

// Request is what a grant is issued for.
type Request struct {
	Scope string
	Dir   string
	Label string
	TTL   time.Duration
}

// Device is what a successful redemption hands back: the bearer token the TV
// keeps, and the grant it stands for.
type Device struct {
	Token string
	Grant Grant
}

// Options configures a Store. The zero value is the real clock, the system's
// randomness and DefaultThrottle.
type Options struct {
	Now      func() time.Time
	Random   io.Reader
	Throttle Throttle
}

// grant is a Grant with the hashes of the tokens paired under it, so revoking
// the code can find and void every one.
type grant struct {
	Grant
	tokens map[tokenHash]struct{}
}

// tokenHash is the key a token is held under. The store never keeps a token
// itself: a lookup by hash takes the same time whichever token arrives, and a
// dump of the store's memory is not a list of working credentials.
type tokenHash [sha256.Size]byte

// Store holds the live grants and the devices paired under them. It is safe for
// concurrent use.
type Store struct {
	now      func() time.Time
	random   io.Reader
	throttle *throttle

	mu      sync.Mutex
	grants  map[string]*grant    // code -> grant
	devices map[tokenHash]string // token hash -> code
}

// New makes an empty store.
func New(opts Options) *Store {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	return &Store{
		now:      opts.Now,
		random:   opts.Random,
		throttle: newThrottle(opts.Throttle),
		grants:   map[string]*grant{},
		devices:  map[tokenHash]string{},
	}
}

// Issue mints a code for a folder. The caller has already checked that the
// folder exists; this only checks the lifetime and the count.
func (s *Store) Issue(req Request) (Grant, error) {
	if req.TTL < MinTTL || req.TTL > MaxTTL {
		return Grant{}, ErrBadTTL
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.pruneLocked(now)
	if len(s.grants) >= MaxGrants {
		return Grant{}, ErrTooManyGrants
	}

	code, err := s.freshCodeLocked()
	if err != nil {
		return Grant{}, err
	}
	g := &grant{
		Grant: Grant{
			Code:      code,
			Scope:     req.Scope,
			Dir:       req.Dir,
			Label:     strings.TrimSpace(req.Label),
			CreatedAt: now,
			ExpiresAt: now.Add(req.TTL),
		},
		tokens: map[tokenHash]struct{}{},
	}
	s.grants[code] = g
	return g.snapshot(), nil
}

// freshCodeLocked draws a code no live grant is using. With at most MaxGrants
// live out of a million, a collision is rare and two in a row rarer still; the
// bound is there so a broken random source fails rather than spins.
func (s *Store) freshCodeLocked() (string, error) {
	raw := make([]byte, CodeLength)
	for attempt := 0; attempt < 64; attempt++ {
		if _, err := io.ReadFull(s.random, raw); err != nil {
			return "", fmt.Errorf("drawing a code: %w", err)
		}
		var b strings.Builder
		for _, c := range raw {
			b.WriteByte(Alphabet[int(c)%len(Alphabet)])
		}
		if _, taken := s.grants[b.String()]; !taken {
			return b.String(), nil
		}
	}
	return "", errors.New("drawing a code: could not find an unused one")
}

// Redeem trades a code for a device token. client identifies who is asking —
// the remote address, in practice — so one TV mistyping cannot lock out
// another for long, while a guesser on many addresses still runs into the
// global limit.
func (s *Store) Redeem(client, code string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.pruneLocked(now)

	// Checked before the code is even looked at: a throttle that let a right
	// guess through would only slow a guesser down, not stop one.
	if wait, ok := s.throttle.allow(client, now); !ok {
		return Device{}, &ThrottledError{RetryAfter: wait}
	}

	g, ok := s.grants[Normalize(code)]
	if !ok {
		s.throttle.fail(client, now)
		return Device{}, ErrBadCode
	}
	if len(g.tokens) >= MaxDevicesPerGrant {
		return Device{}, ErrTooManyDevices
	}

	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.random, raw); err != nil {
		return Device{}, fmt.Errorf("minting a device token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	h := hashToken(token)
	g.tokens[h] = struct{}{}
	s.devices[h] = g.Code
	return Device{Token: token, Grant: g.snapshot()}, nil
}

// Authenticate resolves a device token to the grant it was paired under, or
// reports false when the token is unknown or its grant has expired or been
// revoked.
func (s *Store) Authenticate(token string) (Grant, bool) {
	if token == "" {
		return Grant{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(s.now())
	code, ok := s.devices[hashToken(token)]
	if !ok {
		return Grant{}, false
	}
	return s.grants[code].snapshot(), true
}

// Live reports whether a code still names a grant. A stream link minted for a
// device carries its grant's code, and asks this on every request, so revoking
// the code stops a film that is playing rather than only the next one.
func (s *Store) Live(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	_, ok := s.grants[code]
	return ok
}

// Forget unpairs one device, which is what signing out on the TV does. It
// reports whether the token was paired.
func (s *Store) Forget(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := hashToken(token)
	code, ok := s.devices[h]
	if !ok {
		return false
	}
	delete(s.devices, h)
	if g := s.grants[code]; g != nil {
		delete(g.tokens, h)
	}
	return true
}

// Revoke ends a grant and unpairs every device under it. It reports whether
// the code was live.
func (s *Store) Revoke(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.grants[Normalize(code)]
	if !ok {
		return false
	}
	s.dropLocked(g)
	return true
}

// Grants lists the live grants, newest first.
func (s *Store) Grants() []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(s.now())
	out := make([]Grant, 0, len(s.grants))
	for _, g := range s.grants {
		out = append(out, g.snapshot())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// Clear ends every grant.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = map[string]*grant{}
	s.devices = map[tokenHash]string{}
}

// pruneLocked drops every grant whose time is up, with its devices.
func (s *Store) pruneLocked(now time.Time) {
	for _, g := range s.grants {
		if !now.Before(g.ExpiresAt) {
			s.dropLocked(g)
		}
	}
}

func (s *Store) dropLocked(g *grant) {
	for h := range g.tokens {
		delete(s.devices, h)
	}
	delete(s.grants, g.Code)
}

func (g *grant) snapshot() Grant {
	out := g.Grant
	out.Devices = len(g.tokens)
	return out
}

// Normalize is how a typed code is read: case and the spaces or dashes a
// person might put in to make it easier to read back do not matter.
func Normalize(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		if r == ' ' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func hashToken(token string) tokenHash {
	return sha256.Sum256([]byte(token))
}

// durationWords prints a whole number of days as days, which is how the
// thirty-day ceiling should read in an error.
func durationWords(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	}
	return d.String()
}
