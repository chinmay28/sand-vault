package vault

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/google/uuid"
)

// StoreVersion is the on-disk vault format version written by this build.
const StoreVersion = 3

// minStoreVersion is the oldest format this build still opens. A version 2
// vault is read as it stands and upgraded the first time anything is written,
// because every version 3 field is additive: a vault with no sub vaults is
// byte-for-byte what version 2 always was.
//
// The bump matters in the other direction. Sub-vault sections are the only copy
// of what they hold, and an older build parsing this file would discard the
// field it does not know about and then write the file back without it —
// destroying them silently. Refusing to open a version it has never heard of is
// what that build already does, so raising the number is what makes the
// discarding impossible.
const minStoreVersion = 2

// checkPlaintext is sealed under the vault key at init and re-opened on every
// unlock; a successful GCM tag check is what verifies the password.
const checkPlaintext = "SAND-VAULT-OK"

// DataKeySize is the length of the random key that actually protects file
// content. Keeping it separate from the password is what lets the vault hold
// several of them at once: a password change mints a new one and the old one
// stays behind, still readable, until every file has been re-encrypted under
// the new one.
const DataKeySize = 32

// ErrLocked is returned by operations that need an unlocked vault.
var ErrLocked = errors.New("vault is locked")

// ErrWrongPassword is returned when the supplied password fails to open the
// vault's verifier.
var ErrWrongPassword = errors.New("wrong password")

// ErrNotInitialized is returned when no vault exists at the configured path.
var ErrNotInitialized = errors.New("vault has not been created yet")

// sealed is one AES-256-GCM ciphertext with its nonce, base64 encoded for JSON.
type sealed struct {
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// kdfParams records the Argon2id settings used to stretch the password, so a
// vault written with different parameters still opens.
type kdfParams struct {
	Salt    string `json:"salt"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"`
	Threads uint8  `json:"threads"`
}

func (k kdfParams) toArgon2() (crypto.Argon2Params, []byte, error) {
	salt, err := base64.StdEncoding.DecodeString(k.Salt)
	if err != nil {
		return crypto.Argon2Params{}, nil, fmt.Errorf("decoding vault salt: %w", err)
	}
	return crypto.Argon2Params{
		Time:    k.Time,
		Memory:  k.Memory,
		Threads: k.Threads,
		SaltLen: uint32(len(salt)),
		KeyLen:  32,
	}, salt, nil
}

// wrappedKey is one retired data key, sealed under the vault key and tagged
// with the generation it belongs to.
type wrappedKey struct {
	ID  string `json:"id"`
	Key sealed `json:"key"`
}

// storeFile is the complete on-disk vault. Every field beyond the KDF
// parameters is encrypted: provider credentials and the manifest both leak
// meaningful information about the user, so neither is stored in the clear.
type storeFile struct {
	Version   int       `json:"version"`
	KDF       kdfParams `json:"kdf"`
	Check     sealed    `json:"check"`
	DataKey   sealed    `json:"data_key"`
	Providers sealed    `json:"providers"`
	Manifest  sealed    `json:"manifest"`
	Policy    Policy    `json:"policy"`

	// Settings holds the preferences that are themselves secrets — today that
	// is one, the film database key. Sealed like every other section, and
	// pointedly *not* kept in the manifest: the manifest is replicated to every
	// connected account as a recovery backup, and a credential for somebody's
	// account with a third party has no business being copied onto three cloud
	// providers. A vault that has never set one has no section at all.
	Settings *sealed `json:"settings,omitempty"`

	// DefaultAccounts names the accounts an upload spreads over when it does
	// not choose its own. Empty means "no preference", and every upload picks
	// its own three at random instead.
	//
	// In the clear beside the policy, and for the same reason: these are the
	// random IDs of accounts whose names, kinds and credentials all live inside
	// the encrypted providers section. A stolen vault file learns from them
	// only that up to three accounts were singled out, which the encrypted
	// section it cannot open would have told it anyway.
	DefaultAccounts []string `json:"default_accounts,omitempty"`

	// DefaultScheme names the erasure code uploads are cut with when they do not
	// choose their own, written "k-of-n". Empty means no preference, and how
	// many accounts a file lands on settles the code as it always did.
	//
	// It is a preference rather than a rule, and it is applied only where it
	// fits: a default of 3-of-5 cuts the files that go to five accounts, and a
	// file deliberately sent to six is 4-of-6 because 3-of-5 has nothing to say
	// about six accounts. See transferTarget.schemeFor.
	//
	// In the clear beside the accounts, and it gives away less than they do: two
	// small numbers saying how a vault likes to be cut, which the shard headers
	// on every account state outright anyway.
	DefaultScheme string `json:"default_scheme,omitempty"`

	// DataKeyID names the generation in DataKey, so a manifest entry can say
	// which key its parts were sealed under. Absent on a vault written before
	// the key could be rotated, which is why the empty string is a valid ID:
	// those entries carry no key ID either, so the two still match.
	DataKeyID string `json:"data_key_id,omitempty"`

	// RetiredKeys holds the generations that files still hold parts under
	// while a re-encryption is in flight. It is empty the rest of the time —
	// a key is dropped the moment the last entry stops naming it.
	RetiredKeys []wrappedKey `json:"retired_keys,omitempty"`

	// BackupNeedsForce says the copies of the index on the accounts are sealed
	// under a password this vault has stopped using, and must be overwritten
	// rather than protected as another vault's.
	//
	// It is on disk rather than in memory because the push that replaces them
	// can fail — the machine that changed the password may be offline for days
	// — and until it lands, what those accounts hold is an index sealed under
	// the old password carrying the old data key. Recovering from one would
	// produce a vault that cannot read anything re-encrypted since.
	BackupNeedsForce bool `json:"backup_needs_force,omitempty"`

	// HealthCheckMinutes is how often the connected accounts are pinged in the
	// background to see whether they are still answering, and HealthCheckOff
	// switches that off. Zero minutes means the default — an hour.
	//
	// Stored as the negative, the way ManifestBackupDisabled below is and for
	// the same reason: a vault written before this existed has no field at all,
	// and the absence of it has to mean the check is on.
	//
	// In the clear beside the policy, and it gives away less than the policy
	// does: an interval says how often this machine talks to storage it has
	// already been established it uses. Nothing about which accounts, or what
	// they hold, or whether any of them is currently down — those live in the
	// encrypted section and in memory respectively. See health.go.
	HealthCheckMinutes int  `json:"health_check_minutes,omitempty"`
	HealthCheckOff     bool `json:"health_check_off,omitempty"`

	// ManifestBackupDisabled turns off replicating the manifest to the
	// connected accounts. Stored as the negative so that the absence of the
	// field — an older vault, or one written by a build that predates the
	// feature — means the backup is on, which is the default.
	ManifestBackupDisabled bool `json:"manifest_backup_disabled,omitempty"`

	// InheritedKeyID names a data key this vault did not mint: the one a
	// recovery adopted from the vault it rebuilt.
	//
	// Recorded because adopting it is not the end of the story. That key is
	// derived from the *old* password, and every copy of the old manifest.sand
	// hands it over — including any that was taken off an account before this
	// vault existed, which no amount of overwriting can reach. So while the
	// active generation is this one, the files are readable by whoever could
	// read them before the machine died, and the vault has to keep saying so
	// until they have been re-encrypted. Cleared by RotateDataKey.
	InheritedKeyID string `json:"inherited_key_id,omitempty"`

	// SubVaults are the vaults inside this one, each sealed under a password of
	// its own rather than under the vault key. Nothing here opens with the main
	// password, which is the entire point: a sub vault's file tree is as hidden
	// from someone holding the main password as it is from someone holding
	// nothing.
	SubVaults []subVaultRecord `json:"sub_vaults,omitempty"`

	// What the last recovery kit was, so the settings panel can say how stale
	// it has become. All of it is in the clear beside the policy, and for the
	// same reason: a date, two counts and a uuid say nothing the encrypted
	// sections do not already imply, and the thing that would be a secret —
	// the kit's recovery code — lives in the sealed settings section instead.
	//
	// The counts are what "312 files added since" is a subtraction of, and
	// LastPasswordChangeAt is what tells a kit that predates a password change
	// from one that merely predates some uploads. Those are different problems
	// (see §6.5 of docs/recovery-kit.md) and the panel has to name the right
	// one.
	LastKitExportAt  time.Time `json:"last_kit_export_at,omitzero"`
	LastKitID        string    `json:"last_kit_id,omitempty"`
	LastKitSecret    string    `json:"last_kit_secret,omitempty"`
	LastKitFileCount int       `json:"last_kit_file_count,omitempty"`

	// LastKitAccounts are the ids the kit carries credentials for, not merely
	// how many. A count would call "removed one, connected another" no change
	// at all, which is precisely the state where the kit has stopped being able
	// to restore an account — and the ids are already in the clear beside the
	// policy, so this reveals nothing the file did not.
	LastKitAccounts []string `json:"last_kit_accounts,omitempty"`

	LastPasswordChangeAt time.Time `json:"last_password_change_at,omitzero"`
}

// vaultSettings is the plaintext of the settings section.
type vaultSettings struct {
	// MovieAPIKey is the user's own key for the film database. Empty — which is
	// how every vault starts — means no film lookup can happen at all.
	MovieAPIKey string `json:"movie_api_key,omitempty"`

	// Assistant is where the chat assistant's model server is, and which
	// model on it to talk to. It lives beside the film key rather than in
	// the manifest for the same reason: the manifest is replicated to every
	// connected account, and the address of a machine on the user's own
	// network — with, sometimes, a token that opens it — is not index. See
	// Vault.Assistant.
	Assistant *AssistantSettings `json:"assistant,omitempty"`

	// KitCodes are the recovery codes for the kits this vault has exported, by
	// kit id, so that Settings → Recovery kit → Show code can answer.
	//
	// Here rather than in the manifest, and for the reason the film key is
	// here: the manifest is replicated to every connected account, and the code
	// that opens a kit carrying every credential has no business being copied
	// onto three cloud providers. Nothing about the arrangement is weakened by
	// keeping it — see Vault.KitCode.
	KitCodes map[string]string `json:"kit_codes,omitempty"`

	// Sources are the machines files are imported *from* — see source.go.
	//
	// Here rather than in a section of their own, and for the reason stated
	// above rather than to save a section: this is the part of the vault that
	// is *not* replicated to the connected accounts. A source carries an SSH
	// private key for a machine the user owns, and a credential like that has
	// no business being copied onto three cloud providers as part of a
	// manifest backup — which is precisely the argument the film key is here
	// for, and it applies with more force to a key that opens a shell.
	//
	// The consequence is worth being explicit about: sources do not travel in
	// a recovery kit, so a rebuilt install reconnects its accounts and has to
	// be told about its sources again. That is the right trade. A kit exists
	// to restore access to the data, and an import source holds none of it —
	// what was imported is in the vault like any other file.
	Sources []Source `json:"sources,omitempty"`
}

// empty reports whether anything is set, so that a vault which has never
// touched these writes no section rather than an encrypted empty object.
func (s *vaultSettings) empty() bool {
	return s == nil || (s.MovieAPIKey == "" && s.Assistant == nil && len(s.KitCodes) == 0 && len(s.Sources) == 0)
}

// subVaultRecord is one sub vault as it sits on disk: its own KDF salt, its own
// verifier, its own data keys, and its file index sealed under a key derived
// from its own password.
//
// It is deliberately self-contained. A main password change rewrites every
// other section of the vault file under a fresh vault key; these records are
// carried across untouched, because nothing in them was ever sealed under the
// key being replaced. That is what makes "change the vault password" and "there
// are sub vaults" two facts that never have to be reconciled.
type subVaultRecord struct {
	ID  string    `json:"id"`
	KDF kdfParams `json:"kdf"`

	// Check is the verifier for this sub vault's password, the same
	// constant-time GCM check the main vault uses.
	Check sealed `json:"check"`

	// DataKey and DataKeyID are the generation new files in this sub vault are
	// sealed under; RetiredKeys are the generations its files are still on
	// while a re-encryption finishes. Exactly the main vault's arrangement,
	// because a sub vault changes its password the same way and defers the same
	// work.
	DataKey     sealed       `json:"data_key"`
	DataKeyID   string       `json:"data_key_id"`
	RetiredKeys []wrappedKey `json:"retired_keys,omitempty"`

	// Section is this sub vault's Manifest: its entries, its folders and its
	// thumbnail packs. It is the only copy — nothing about the files inside a
	// sub vault is recorded anywhere the main password reaches.
	Section sealed `json:"section"`
}

// keyIDs lists the data key generations this record advertises. They are in the
// clear so that a file naming one of them can be told apart from a file naming
// a key that is simply gone: the first is a locked sub vault and asks for a
// password, the second is a corrupt index and asks for a recovery.
func (r subVaultRecord) keyIDs() []string {
	ids := make([]string, 0, len(r.RetiredKeys)+1)
	ids = append(ids, r.DataKeyID)
	for _, k := range r.RetiredKeys {
		ids = append(ids, k.ID)
	}
	return ids
}

// newSubVaultRecord creates a sub vault sealed under password, with a fresh
// random data key of its own and an empty index.
func newSubVaultRecord(id, password string) (subVaultRecord, *subVault, error) {
	if strings.TrimSpace(password) == "" {
		return subVaultRecord{}, nil, fmt.Errorf("password must not be empty")
	}

	params := crypto.DefaultArgon2Params()
	salt, err := crypto.GenerateSalt(params.SaltLen)
	if err != nil {
		return subVaultRecord{}, nil, err
	}

	sectionKey := crypto.DeriveKey(password, salt, params)
	dataKey := make([]byte, DataKeySize)
	if _, err := io.ReadFull(rand.Reader, dataKey); err != nil {
		crypto.ZeroBytes(sectionKey)
		return subVaultRecord{}, nil, fmt.Errorf("generating data key: %w", err)
	}

	open := &subVault{
		id: id,
		kdf: kdfParams{
			Salt:    base64.StdEncoding.EncodeToString(salt),
			Time:    params.Time,
			Memory:  params.Memory,
			Threads: params.Threads,
		},
		sectionKey: sectionKey,
		dataKey:    dataKey,
		dataKeyID:  newKeyID(),
		retired:    map[string][]byte{},
		manifest:   newManifest(),
	}

	rec, err := open.record()
	if err != nil {
		open.zero()
		return subVaultRecord{}, nil, err
	}
	return rec, open, nil
}

// subVault is an opened sub vault: the key that seals its section, the data
// keys that protect its files, and its decrypted index.
type subVault struct {
	id string

	// kdf is the salt and cost this sub vault's section key was derived with.
	// Carried so that re-sealing the section — which happens on every index
	// change — writes the record back under the parameters it was opened with.
	// Only a password change mints new ones, and it does so by building a whole
	// new record.
	kdf kdfParams

	// sectionKey is Argon2id of this sub vault's password. It seals the
	// section and nothing else — file content answers to the data keys below,
	// which is what lets the password change without re-encrypting anything.
	sectionKey []byte

	dataKey   []byte
	dataKeyID string
	retired   map[string][]byte

	manifest *Manifest
}

// zero wipes every key this holds.
func (s *subVault) zero() {
	if s == nil {
		return
	}
	crypto.ZeroBytes(s.sectionKey)
	crypto.ZeroBytes(s.dataKey)
	for _, k := range s.retired {
		crypto.ZeroBytes(k)
	}
}

// record re-seals the sub vault into the form it takes on disk. The KDF
// parameters travel with the open sub vault, so re-sealing never moves one onto
// a different salt behind its own back — only a password change does that, by
// building a new record outright.
func (s *subVault) record() (subVaultRecord, error) {
	check, err := seal(s.sectionKey, []byte(checkPlaintext))
	if err != nil {
		return subVaultRecord{}, err
	}
	dataKey, err := seal(s.sectionKey, s.dataKey)
	if err != nil {
		return subVaultRecord{}, err
	}
	section, err := sealJSON(s.sectionKey, s.manifest)
	if err != nil {
		return subVaultRecord{}, err
	}

	rec := subVaultRecord{
		ID:        s.id,
		KDF:       s.kdf,
		Check:     check,
		DataKey:   dataKey,
		DataKeyID: s.dataKeyID,
		Section:   section,
	}
	for id, key := range s.retired {
		wrapped, err := seal(s.sectionKey, key)
		if err != nil {
			return subVaultRecord{}, err
		}
		rec.RetiredKeys = append(rec.RetiredKeys, wrappedKey{ID: id, Key: wrapped})
	}
	sort.Slice(rec.RetiredKeys, func(i, j int) bool { return rec.RetiredKeys[i].ID < rec.RetiredKeys[j].ID })
	return rec, nil
}

// unsealSubVault opens one sub vault record with its password.
func unsealSubVault(rec subVaultRecord, password string) (*subVault, error) {
	params, salt, err := rec.KDF.toArgon2()
	if err != nil {
		return nil, err
	}

	s := &subVault{
		id:         rec.ID,
		kdf:        rec.KDF,
		sectionKey: crypto.DeriveKey(password, salt, params),
		dataKeyID:  rec.DataKeyID,
		retired:    map[string][]byte{},
	}

	plain, err := open(s.sectionKey, rec.Check)
	if err != nil || subtle.ConstantTimeCompare(plain, []byte(checkPlaintext)) != 1 {
		s.zero()
		return nil, ErrWrongPassword
	}

	if s.dataKey, err = open(s.sectionKey, rec.DataKey); err != nil {
		s.zero()
		return nil, fmt.Errorf("unwrapping the sub vault's data key: %w", err)
	}
	for _, retired := range rec.RetiredKeys {
		key, err := open(s.sectionKey, retired.Key)
		if err != nil {
			s.zero()
			return nil, fmt.Errorf("unwrapping the data key the sub vault's files are still stored under: %w", err)
		}
		s.retired[retired.ID] = key
	}

	s.manifest = newManifest()
	if err := openJSON(s.sectionKey, rec.Section, s.manifest); err != nil {
		s.zero()
		return nil, fmt.Errorf("decrypting the sub vault's file index: %w", err)
	}
	s.manifest.normalize()
	return s, nil
}

// seal encrypts plaintext under key with a fresh random nonce.
func seal(key, plaintext []byte) (sealed, error) {
	nonce, err := crypto.GenerateNonce()
	if err != nil {
		return sealed{}, err
	}
	ct, err := crypto.Encrypt(key, nonce, plaintext, nil)
	if err != nil {
		return sealed{}, err
	}
	return sealed{
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}, nil
}

// open decrypts a sealed blob.
func open(key []byte, s sealed) ([]byte, error) {
	nonce, err := base64.StdEncoding.DecodeString(s.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decoding nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(s.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decoding ciphertext: %w", err)
	}
	return crypto.Decrypt(key, nonce, ct, nil)
}

// sealJSON marshals a value and seals the result.
func sealJSON(key []byte, v any) (sealed, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return sealed{}, err
	}
	return seal(key, data)
}

// openJSON opens a sealed blob and unmarshals it.
func openJSON(key []byte, s sealed, out any) error {
	data, err := open(key, s)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// readStore loads and parses the vault file.
func readStore(path string) (*storeFile, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, fmt.Errorf("reading vault: %w", err)
	}

	var sf storeFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parsing vault at %s: %w", path, err)
	}
	if sf.Version < minStoreVersion || sf.Version > StoreVersion {
		return nil, fmt.Errorf("unsupported vault version %d (this build understands %d to %d)",
			sf.Version, minStoreVersion, StoreVersion)
	}
	if !sf.Policy.Valid() {
		sf.Policy = PolicyStrict
	}
	return &sf, nil
}

// writeStore serializes the vault and replaces the file atomically, so an
// interrupted write can never destroy an existing index.
func writeStore(path string, sf *storeFile) error {
	// Anything this build writes is written in this build's format, so a vault
	// opened at version 2 is upgraded by the first change made to it.
	sf.Version = StoreVersion

	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return fmt.Errorf("serializing vault: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating vault directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".sand-vault-*")
	if err != nil {
		return fmt.Errorf("creating temp vault: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() { os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("writing vault: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("syncing vault: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing vault: %w", err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		cleanup()
		return fmt.Errorf("setting vault permissions: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("replacing vault: %w", err)
	}
	return nil
}

// newStore creates a fresh vault sealed under password.
func newStore(password string, policy Policy) (*storeFile, []byte, error) {
	params := crypto.DefaultArgon2Params()
	salt, err := crypto.GenerateSalt(params.SaltLen)
	if err != nil {
		return nil, nil, err
	}

	vaultKey := crypto.DeriveKey(password, salt, params)
	defer crypto.ZeroBytes(vaultKey)

	dataKey := make([]byte, DataKeySize)
	if _, err := io.ReadFull(rand.Reader, dataKey); err != nil {
		return nil, nil, fmt.Errorf("generating data key: %w", err)
	}

	check, err := seal(vaultKey, []byte(checkPlaintext))
	if err != nil {
		return nil, nil, err
	}
	sealedKey, err := seal(vaultKey, dataKey)
	if err != nil {
		return nil, nil, err
	}
	providers, err := sealJSON(vaultKey, []provider.Config{})
	if err != nil {
		return nil, nil, err
	}
	manifest, err := sealJSON(vaultKey, newManifest())
	if err != nil {
		return nil, nil, err
	}

	sf := &storeFile{
		Version: StoreVersion,
		KDF: kdfParams{
			Salt:    base64.StdEncoding.EncodeToString(salt),
			Time:    params.Time,
			Memory:  params.Memory,
			Threads: params.Threads,
		},
		Check:     check,
		DataKey:   sealedKey,
		DataKeyID: newKeyID(),
		Providers: providers,
		Manifest:  manifest,
		Policy:    policy,
	}
	return sf, dataKey, nil
}

// newKeyID mints an identifier for a data key generation. It is written into
// the vault beside the key and into every entry sealed under it, so it never
// has to be guessed from ordering.
func newKeyID() string { return uuid.NewString() }

// unsealed is everything a password opens: the key that protects the vault
// file, the data keys that protect the stored parts, and the two encrypted
// sections.
type unsealed struct {
	vaultKey  []byte
	dataKey   []byte            // the active generation, used by new uploads
	dataKeyID string            // its ID; "" on a vault written before rotation
	retired   map[string][]byte // older generations, by ID, still in use
	providers []provider.Config
	manifest  *Manifest
	settings  *vaultSettings
}

// zero wipes every key this holds. Callers that adopt the keys into a vault
// must not call it.
func (u *unsealed) zero() {
	crypto.ZeroBytes(u.vaultKey)
	crypto.ZeroBytes(u.dataKey)
	for _, k := range u.retired {
		crypto.ZeroBytes(k)
	}
}

// unsealStore derives the vault key from password and decrypts every section.
func unsealStore(sf *storeFile, password string) (*unsealed, error) {
	params, salt, err := sf.KDF.toArgon2()
	if err != nil {
		return nil, err
	}

	u := &unsealed{
		vaultKey:  crypto.DeriveKey(password, salt, params),
		dataKeyID: sf.DataKeyID,
		retired:   map[string][]byte{},
	}

	plain, err := open(u.vaultKey, sf.Check)
	if err != nil || subtle.ConstantTimeCompare(plain, []byte(checkPlaintext)) != 1 {
		u.zero()
		return nil, ErrWrongPassword
	}

	u.dataKey, err = open(u.vaultKey, sf.DataKey)
	if err != nil {
		u.zero()
		return nil, fmt.Errorf("unwrapping data key: %w", err)
	}
	for _, retired := range sf.RetiredKeys {
		key, err := open(u.vaultKey, retired.Key)
		if err != nil {
			u.zero()
			return nil, fmt.Errorf("unwrapping the data key files are still stored under: %w", err)
		}
		u.retired[retired.ID] = key
	}

	u.providers = []provider.Config{}
	if err := openJSON(u.vaultKey, sf.Providers, &u.providers); err != nil {
		u.zero()
		return nil, fmt.Errorf("decrypting connected accounts: %w", err)
	}

	u.manifest = newManifest()
	if err := openJSON(u.vaultKey, sf.Manifest, u.manifest); err != nil {
		u.zero()
		return nil, fmt.Errorf("decrypting file index: %w", err)
	}
	u.manifest.normalize()

	u.settings = &vaultSettings{}
	if sf.Settings != nil {
		if err := openJSON(u.vaultKey, *sf.Settings, u.settings); err != nil {
			u.zero()
			return nil, fmt.Errorf("decrypting settings: %w", err)
		}
	}

	return u, nil
}
