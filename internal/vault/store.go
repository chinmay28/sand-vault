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

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/google/uuid"
)

// StoreVersion is the on-disk vault format version.
const StoreVersion = 2

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

	// ManifestBackupDisabled turns off replicating the manifest to the
	// connected accounts. Stored as the negative so that the absence of the
	// field — an older vault, or one written by a build that predates the
	// feature — means the backup is on, which is the default.
	ManifestBackupDisabled bool `json:"manifest_backup_disabled,omitempty"`
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
	if sf.Version != StoreVersion {
		return nil, fmt.Errorf("unsupported vault version %d (this build understands %d)",
			sf.Version, StoreVersion)
	}
	if !sf.Policy.Valid() {
		sf.Policy = PolicyStrict
	}
	return &sf, nil
}

// writeStore serializes the vault and replaces the file atomically, so an
// interrupted write can never destroy an existing index.
func writeStore(path string, sf *storeFile) error {
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
	if u.manifest.Entries == nil {
		u.manifest.Entries = []*Entry{}
	}
	if u.manifest.Folders == nil {
		u.manifest.Folders = []string{}
	}

	return u, nil
}
