package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
	sandsftp "github.com/chinmay28/sand-vault/internal/sftp"
	"github.com/google/uuid"
)

// A Source is a machine files are brought *in* from, which is the opposite
// direction from everything else in this package.
//
// It is deliberately not a provider.Config, and the same VPS wearing both hats
// is deliberately two entries. A connected account holds opaque shards under
// keys SAND generates; a source holds the user's own files under paths the user
// browses, is read-only, and is never a place a part is written. Folding the
// two together would mean an import browser that can see the shard store, and a
// bug on the import path that can write into it. The cost of keeping them apart
// is typing the host twice.
//
// What they share is that both hold credentials, which is why a Source only
// ever exists inside the encrypted vault.
type Source struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	User string `json:"user"`

	// Root is the folder on the far end this source is scoped to. Nothing
	// outside it can be listed or read, however the path is written — see
	// sftp.Under and (*sftp.Client).resolve.
	Root string `json:"root"`

	// HostKey is the fingerprint this host is pinned to. Empty only between
	// somebody filling in the form and the first connection, which learns it:
	// AddSource does not store a source it could not reach, so a stored source
	// always carries one.
	HostKey string `json:"host_key,omitempty"`

	AddedAt time.Time `json:"added_at"`

	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	Password   string `json:"password,omitempty"`
}

// sourceSecrets names the fields redacted on the way out and merged on the way
// back in. In one place because the two lists have to agree: a field redacted
// but not merged is one that is wiped every time somebody renames the source.
var sourceSecrets = []struct {
	name  string
	field func(*Source) *string
}{
	{"private_key", func(s *Source) *string { return &s.PrivateKey }},
	{"passphrase", func(s *Source) *string { return &s.Passphrase }},
	{"password", func(s *Source) *string { return &s.Password }},
}

// Redacted returns a copy safe to hand to the API layer, with every secret
// replaced by the same placeholder a connected account's are.
//
// The placeholder is a value rather than a blank for the reason it is over in
// provider: the browser has to be able to tell "there is a key, you cannot see
// it" from "there is no key", and an edit that hands the placeholder back means
// "leave the one you have alone".
func (s Source) Redacted() Source {
	out := s
	for _, secret := range sourceSecrets {
		field := secret.field(&out)
		if *field != "" {
			*field = provider.RedactedSecret
		}
	}
	return out
}

// merge folds edited settings into a stored source, treating the redaction
// placeholder as "unchanged".
func (s Source) merge(edits Source) Source {
	out := edits
	out.ID = s.ID
	out.AddedAt = s.AddedAt

	for _, secret := range sourceSecrets {
		edited := secret.field(&out)
		if *edited == provider.RedactedSecret {
			*edited = *secret.field(&s)
		}
	}

	// An edit that does not mention the host key keeps the one it had. The
	// pin is not a secret, so it is not redacted on the way out and an edit
	// form need not send it back — which means an empty field here is
	// "unchanged", not "trust whatever answers next". Forgetting a pin is a
	// thing you ask for; see UpdateSource.
	if out.HostKey == "" {
		out.HostKey = s.HostKey
	}
	return out
}

// validate checks a source is filled in enough to be worth connecting.
func (s *Source) validate() error {
	s.Name = strings.TrimSpace(s.Name)
	s.Host = strings.TrimSpace(s.Host)
	s.User = strings.TrimSpace(s.User)
	s.Root = sandsftp.CleanPath(s.Root)

	if s.Host == "" {
		return errors.New("no host given")
	}
	if s.User == "" {
		return errors.New("no username given")
	}
	if s.Root == "" {
		return errors.New("no folder given: name the folder on the server this source can see")
	}
	if s.PrivateKey == "" && s.Password == "" {
		return errors.New("no way to sign in: give a private key or a password")
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("%d is not a port number", s.Port)
	}
	if s.Name == "" {
		s.Name = s.Host
	}

	// Checked when it is typed rather than on the first connection, where a
	// typo would be indistinguishable from somebody answering in the server's
	// place.
	normalized, err := sandsftp.NormalizeHostKey(s.HostKey)
	if err != nil {
		return err
	}
	s.HostKey = normalized
	return nil
}

// dialConfig is what internal/sftp needs to reach this source.
func (s Source) dialConfig() sandsftp.Config {
	return sandsftp.Config{
		Host:       s.Host,
		Port:       s.Port,
		User:       s.User,
		PrivateKey: s.PrivateKey,
		Passphrase: s.Passphrase,
		Password:   s.Password,
		HostKey:    s.HostKey,
	}
}

// Sources returns the configured import sources with secrets redacted.
func (v *Vault) Sources() ([]Source, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.dataKey == nil {
		return nil, ErrLocked
	}
	out := make([]Source, 0, len(v.settings.Sources))
	for _, s := range v.settings.Sources {
		out = append(out, s.Redacted())
	}
	return out, nil
}

// source returns one source with its secrets intact, for the code that has to
// connect with them.
func (v *Vault) source(id string) (Source, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.dataKey == nil {
		return Source{}, ErrLocked
	}
	for _, s := range v.settings.Sources {
		if s.ID == id {
			return s, nil
		}
	}
	return Source{}, fmt.Errorf("no source with id %q", id)
}

// AddSource stores a machine to import from, after connecting to it.
//
// Reached before it is stored, like AddProvider, and for a reason of its own on
// top: this is the connection that learns the host key. A source stored without
// one would pin nothing, since the first connection after it would learn
// whatever answered.
func (v *Vault) AddSource(ctx context.Context, s Source) (Source, error) {
	if err := s.validate(); err != nil {
		return Source{}, err
	}

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return Source{}, ErrLocked
	}
	for _, existing := range v.settings.Sources {
		if strings.EqualFold(existing.Name, s.Name) {
			v.mu.Unlock()
			return Source{}, fmt.Errorf("a source named %q already exists", s.Name)
		}
	}
	v.mu.Unlock()

	// Outside the lock: this is a network round trip to somebody else's
	// machine, and browsing must not stop while it happens.
	fingerprint, err := checkSource(ctx, s)
	if err != nil {
		return Source{}, err
	}
	s.HostKey = fingerprint
	s.ID = uuid.NewString()
	s.AddedAt = time.Now().UTC()

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return Source{}, ErrLocked
	}
	v.settings.Sources = append(v.settings.Sources, s)
	if err := v.persistLocked(); err != nil {
		v.settings.Sources = v.settings.Sources[:len(v.settings.Sources)-1]
		return Source{}, err
	}
	return s.Redacted(), nil
}

// UpdateSource changes a stored source's settings, reconnecting to check them.
//
// relearnHostKey is how somebody says "I rebuilt this machine": it drops the
// pin so the reconnection below learns the new one. It is a separate argument
// rather than an empty field because those two mean opposite things — an edit
// form that leaves the fingerprint out is not asking to trust a stranger — and
// because forgetting a pin is the one edit here that weakens something, which
// makes it worth being unable to do by accident.
func (v *Vault) UpdateSource(ctx context.Context, id string, edits Source, relearnHostKey bool) (Source, error) {
	before, err := v.source(id)
	if err != nil {
		return Source{}, err
	}

	after := before.merge(edits)
	if relearnHostKey {
		after.HostKey = ""
	}
	if err := after.validate(); err != nil {
		return Source{}, err
	}

	fingerprint, err := checkSource(ctx, after)
	if err != nil {
		return Source{}, err
	}
	after.HostKey = fingerprint

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return Source{}, ErrLocked
	}
	for i, s := range v.settings.Sources {
		if s.ID != id {
			if strings.EqualFold(s.Name, after.Name) {
				return Source{}, fmt.Errorf("a source named %q already exists", after.Name)
			}
			continue
		}
		v.settings.Sources[i] = after
		if err := v.persistLocked(); err != nil {
			v.settings.Sources[i] = before
			return Source{}, err
		}
		return after.Redacted(), nil
	}
	return Source{}, fmt.Errorf("no source with id %q", id)
}

// RemoveSource forgets a machine. Nothing already imported is touched: an
// imported file is a file in the vault like any other, and where it came from
// stopped mattering the moment it was scattered.
func (v *Vault) RemoveSource(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.dataKey == nil {
		return ErrLocked
	}
	for i, s := range v.settings.Sources {
		if s.ID != id {
			continue
		}
		removed := v.settings.Sources
		v.settings.Sources = append(append([]Source{}, removed[:i]...), removed[i+1:]...)
		if err := v.persistLocked(); err != nil {
			v.settings.Sources = removed
			return err
		}
		return nil
	}
	return fmt.Errorf("no source with id %q", id)
}

// checkSource connects, confirms the folder is there and can be listed, and
// returns the host key fingerprint the connection settled on.
func checkSource(ctx context.Context, s Source) (string, error) {
	client, err := sandsftp.Dial(ctx, s.dialConfig())
	if err != nil {
		return "", err
	}
	defer client.Close()

	if _, err := client.ReadDir(s.Root, ""); err != nil {
		return "", fmt.Errorf("connected to %s, but cannot read %s: %w", s.Host, s.Root, err)
	}
	return client.HostKey(), nil
}

// connectSource dials one stored source.
//
// A connection per request rather than a pool, which is the opposite of what
// the SFTP *backend* does — and the difference is the access pattern rather
// than an inconsistency. A scatter writes every shard of a chunk at once and
// would pay a handshake per shard; browsing is one round trip a click, and an
// import is one long request that dials once and reads every file over the same
// session. Neither wants a connection kept open between requests, and not
// keeping one is one less thing to get wrong when the vault locks.
func (v *Vault) connectSource(ctx context.Context, id string) (*sandsftp.Client, Source, error) {
	s, err := v.source(id)
	if err != nil {
		return nil, Source{}, err
	}
	client, err := sandsftp.Dial(ctx, s.dialConfig())
	if err != nil {
		return nil, Source{}, err
	}
	return client, s, nil
}

// BrowseSource lists one directory on a source.
func (v *Vault) BrowseSource(ctx context.Context, id, dir string) (sandsftp.Listing, error) {
	client, s, err := v.connectSource(ctx, id)
	if err != nil {
		return sandsftp.Listing{}, err
	}
	defer client.Close()

	return client.ReadDir(s.Root, dir)
}
