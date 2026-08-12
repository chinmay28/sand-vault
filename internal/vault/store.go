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

	"github.com/sand-project/sand/internal/crypto"
	"github.com/sand-project/sand/internal/provider"
)

// StoreVersion is the on-disk vault format version.
const StoreVersion = 2

// checkPlaintext is sealed under the vault key at init and re-opened on every
// unlock; a successful GCM tag check is what verifies the password.
const checkPlaintext = "SAND-VAULT-OK"

// DataKeySize is the length of the random key that actually protects file
// content. Keeping it separate from the password means a password change
// re-wraps one key instead of re-encrypting every stored file.
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
	wrappedKey, err := seal(vaultKey, dataKey)
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
		DataKey:   wrappedKey,
		Providers: providers,
		Manifest:  manifest,
		Policy:    policy,
	}
	return sf, dataKey, nil
}

// unsealStore derives the vault key from password and decrypts every section.
func unsealStore(sf *storeFile, password string) (vaultKey, dataKey []byte, providers []provider.Config, manifest *Manifest, err error) {
	params, salt, err := sf.KDF.toArgon2()
	if err != nil {
		return nil, nil, nil, nil, err
	}

	vaultKey = crypto.DeriveKey(password, salt, params)

	plain, err := open(vaultKey, sf.Check)
	if err != nil || subtle.ConstantTimeCompare(plain, []byte(checkPlaintext)) != 1 {
		crypto.ZeroBytes(vaultKey)
		return nil, nil, nil, nil, ErrWrongPassword
	}

	dataKey, err = open(vaultKey, sf.DataKey)
	if err != nil {
		crypto.ZeroBytes(vaultKey)
		return nil, nil, nil, nil, fmt.Errorf("unwrapping data key: %w", err)
	}

	providers = []provider.Config{}
	if err := openJSON(vaultKey, sf.Providers, &providers); err != nil {
		crypto.ZeroBytes(vaultKey)
		return nil, nil, nil, nil, fmt.Errorf("decrypting connected accounts: %w", err)
	}

	manifest = newManifest()
	if err := openJSON(vaultKey, sf.Manifest, manifest); err != nil {
		crypto.ZeroBytes(vaultKey)
		return nil, nil, nil, nil, fmt.Errorf("decrypting file index: %w", err)
	}
	if manifest.Entries == nil {
		manifest.Entries = []*Entry{}
	}
	if manifest.Folders == nil {
		manifest.Folders = []string{}
	}

	return vaultKey, dataKey, providers, manifest, nil
}
