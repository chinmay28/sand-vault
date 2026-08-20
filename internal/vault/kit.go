package vault

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/version"
	"github.com/google/uuid"
)

// The recovery kit: one sealed file that turns a fresh install back into this
// vault, clouds and all.
//
// Recovery already survives the loss of the vault file — every account carries
// manifest.sand, and Recover rebuilds the index from any one of them. What it
// cannot carry is the credentials: a copy of that file sits on every account,
// so a credential inside it would let one compromised account unlock all the
// others. That refusal is right, and it is what leaves somebody reinstalling
// with an afternoon of signing back in to five services before the password
// they still remember does anything at all.
//
// A kit is the manifest backup that never touches a cloud, and can therefore
// carry them. Everything else here follows from that one sentence.

// KitMagic identifies the envelope, so a reader can tell a kit from a manifest
// backup before trying to open either.
const KitMagic = "SAND-KIT"

// KitVersion is the kit format version written by this build.
const KitVersion = 1

// kitCheckPlaintext is sealed under the derived key so a reader can tell a
// wrong secret from a corrupt file.
const kitCheckPlaintext = "SAND-KIT-OK"

// KitSecretCode and KitSecretPassword are the two things that can open a kit.
const (
	KitSecretCode     = "code"
	KitSecretPassword = "password"
)

// ErrNotAKit is returned when a file is not a recovery kit at all.
var ErrNotAKit = errors.New("this is not a SAND recovery kit")

// ErrKitDamaged is returned when the envelope opened under the secret given
// but its payload would not decrypt — the secret was right and the bytes are
// not.
var ErrKitDamaged = errors.New("this recovery kit is damaged")

// kitKDFParams is what a new kit is sealed under. A package variable rather
// than a call to crypto.KitArgon2Params so tests can seal cheaply; opening
// always uses the parameters written into the envelope, so lowering it here
// cannot weaken a kit anybody actually has.
var kitKDFParams = crypto.KitArgon2Params()

// KitEnvelope is the outer layer of kit.sand.
//
// Deliberately the same shape as Backup, for the same reason it has that shape:
// the KDF parameters have to travel with the ciphertext, because a reader who
// has lost everything has nothing but the secret in their hand.
type KitEnvelope struct {
	Magic     string    `json:"magic"`
	Version   int       `json:"version"`
	KitID     string    `json:"kit_id"`
	CreatedAt time.Time `json:"created_at"`

	// Secret says what opens this kit: "code" for a generated recovery code,
	// "password" for the vault password the exporter chose instead.
	//
	// In the clear because it is not a secret and because an import that has to
	// guess asks the wrong question — "Recovery code" and "Vault password" are
	// different fields with different validation and different help text, and
	// the person typing into one of them is having a bad day.
	Secret string `json:"secret"`

	KDF     kdfParams `json:"kdf"`
	Check   sealed    `json:"check"`
	Payload sealed    `json:"payload"`
}

// Kit is what a recovery kit carries: a Snapshot plus the things a manifest
// backup must not have.
type Kit struct {
	Version    int       `json:"version"`
	KitID      string    `json:"kit_id"`
	CreatedAt  time.Time `json:"created_at"`
	AppVersion string    `json:"app_version"`

	// Snapshot is exactly the payload of a manifest backup, embedded whole.
	// Everything that already knows how to read one — Recover, restore
	// --manifest, manifest ls — reads a kit's without a line of change, and
	// every field added to a Snapshot later is in the kit for free.
	Snapshot Snapshot `json:"snapshot"`

	// Accounts is the reason this file exists: the full provider.Config for
	// every connected account, credentials included, under the id it already
	// has. Preserving the id is what leaves the manifest's shard records
	// correct on arrival — sealed sub vaults included, which no other recovery
	// route can manage.
	Accounts []provider.Config `json:"accounts"`

	// VaultKey is the key the lost vault's manifest backups are sealed under.
	//
	// It is here so an import can read a *newer* index off the accounts without
	// the old vault password. A kit from March describes March; the copies of
	// manifest.sand on the clouds are rewritten on every index change, so they
	// are what makes an old kit come back current — and they are sealed under
	// this key. Carrying it gives away nothing new: the snapshot beside it
	// already carries the data key, which opens the files themselves.
	VaultKey string `json:"vault_key"`

	// KDF is the lost vault's own parameters, so a restore under the same
	// password reproduces the same vault key rather than a re-derived one.
	KDF kdfParams `json:"kdf"`

	// The store fields a manifest backup has no room for.
	DefaultAccounts        []string `json:"default_accounts,omitempty"`
	DefaultScheme          string   `json:"default_scheme,omitempty"`
	MovieAPIKey            string   `json:"movie_api_key,omitempty"`
	ManifestBackupDisabled bool     `json:"manifest_backup_disabled,omitempty"`

	// ReadHistory is the .reads sidecar's plaintext, carried decoded. It is
	// sealed under the data key on disk, and the data key is in Snapshot, so
	// carrying it sealed would have been the same secret twice.
	ReadHistory *readHistoryFile `json:"read_history,omitempty"`

	// SecretKind is what opened this kit, stamped on by OpenKit from the
	// envelope rather than sealed inside it — the envelope has to say so in the
	// clear anyway, or an import could not label its field. Not serialized:
	// it describes the file this came out of, not the kit's own contents.
	SecretKind string `json:"-"`
}

// VaultKeyBytes decodes the carried vault key.
func (k *Kit) VaultKeyBytes() ([]byte, error) {
	if k.VaultKey == "" {
		return nil, fmt.Errorf("this kit carries no vault key")
	}
	key, err := base64.StdEncoding.DecodeString(k.VaultKey)
	if err != nil {
		return nil, fmt.Errorf("decoding the kit's vault key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("the kit's vault key is %d bytes, expected 32", len(key))
	}
	return key, nil
}

// KitFingerprint is what a kit says about itself: enough to tell one kit from
// another, and nothing that would be a secret.
type KitFingerprint struct {
	KitID      string    `json:"kit_id"`
	CreatedAt  time.Time `json:"created_at"`
	AppVersion string    `json:"app_version"`
	Secret     string    `json:"secret"`

	Accounts  int   `json:"accounts"`
	Files     int   `json:"files"`
	Bytes     int64 `json:"bytes"`
	SubVaults int   `json:"sub_vaults"`

	// Size and SHA256 describe the sealed kit.sand inside the zip.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`

	// Code is the recovery code this export minted. It is returned to the
	// caller and written nowhere else: not into the zip, not into its filename,
	// not into fingerprint.txt, and not into any log. Empty for a kit sealed
	// under the vault password.
	Code string `json:"code,omitempty"`
}

// ---------------------------------------------------------------------------
// Sealing and opening
// ---------------------------------------------------------------------------

// SealKit wraps a kit in an envelope that its secret alone opens.
func SealKit(kit *Kit, secretKind, secret string) ([]byte, error) {
	params := kitKDFParams
	salt, err := crypto.GenerateSalt(params.SaltLen)
	if err != nil {
		return nil, err
	}
	key := crypto.DeriveKey(secret, salt, params)
	defer crypto.ZeroBytes(key)

	check, err := seal(key, []byte(kitCheckPlaintext))
	if err != nil {
		return nil, err
	}
	payload, err := sealJSON(key, kit)
	if err != nil {
		return nil, err
	}

	return json.MarshalIndent(&KitEnvelope{
		Magic:     KitMagic,
		Version:   KitVersion,
		KitID:     kit.KitID,
		CreatedAt: kit.CreatedAt,
		Secret:    secretKind,
		KDF: kdfParams{
			Salt:    base64.StdEncoding.EncodeToString(salt),
			Time:    params.Time,
			Memory:  params.Memory,
			Threads: params.Threads,
		},
		Check:   check,
		Payload: payload,
	}, "", "  ")
}

// InspectKit reads a kit's header without any secret at all.
//
// This is what lets the import screen label its field "Recovery code" or "Vault
// password" rather than guessing, and what lets somebody holding three zips
// work out which one they are looking at.
func InspectKit(data []byte) (*KitEnvelope, error) {
	var env KitEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotAKit, err)
	}
	if env.Magic != KitMagic {
		return nil, fmt.Errorf("%w (magic %q)", ErrNotAKit, env.Magic)
	}
	if env.Version != KitVersion {
		return nil, fmt.Errorf(
			"this kit was written by a later version of SAND (kit format %d, this build understands %d)",
			env.Version, KitVersion)
	}
	if env.Secret != KitSecretCode && env.Secret != KitSecretPassword {
		// An older or hand-edited envelope. A code is what the export flow
		// mints by default, so that is the better guess to prompt for.
		env.Secret = KitSecretCode
	}
	return &env, nil
}

// OpenKit decrypts a kit with the secret that sealed it. It needs nothing
// else — the envelope carries its own KDF parameters precisely so that a lost
// vault is not required to read it.
//
// A recovery code is normalized and check-summed first, so a typo is answered
// before the KDF runs rather than after a second and a half of Argon2id.
func OpenKit(data []byte, secret string) (*Kit, error) {
	env, err := InspectKit(data)
	if err != nil {
		return nil, err
	}

	if env.Secret == KitSecretCode {
		normalized, err := NormalizeKitCode(secret)
		if err != nil {
			return nil, err
		}
		secret = normalized
	}

	params, salt, err := env.KDF.toArgon2()
	if err != nil {
		return nil, err
	}
	key := crypto.DeriveKey(secret, salt, params)
	defer crypto.ZeroBytes(key)

	plain, err := open(key, env.Check)
	if err != nil || subtle.ConstantTimeCompare(plain, []byte(kitCheckPlaintext)) != 1 {
		if env.Secret == KitSecretCode {
			// The code was well-formed — the check symbol said so — but it is
			// not this kit's. Naming the kit is the useful half of that: the
			// person is probably holding two.
			return nil, fmt.Errorf("this code does not open this kit (kit %s, made %s)",
				shortKitID(env.KitID), env.CreatedAt.Format("2 January 2006"))
		}
		return nil, ErrWrongPassword
	}

	kit := &Kit{}
	if err := openJSON(key, env.Payload, kit); err != nil {
		// The check block opened, so the secret is right and the bytes are not.
		return nil, fmt.Errorf("%w: %v", ErrKitDamaged, err)
	}
	if kit.Snapshot.Manifest == nil {
		kit.Snapshot.Manifest = newManifest()
	}
	kit.Snapshot.Manifest.normalize()
	kit.SecretKind = env.Secret
	return kit, nil
}

// shortKitID is the head of a kit's uuid, which is what fingerprint.txt prints
// and what somebody comparing a zip to a slip of paper actually reads.
func shortKitID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ---------------------------------------------------------------------------
// Building one
// ---------------------------------------------------------------------------

// KitExportOptions says how the kit should be sealed.
type KitExportOptions struct {
	// UseVaultPassword seals the kit under the vault password rather than a
	// generated recovery code. Offered without discouragement — for somebody
	// whose password manager is itself backed up it is a real answer and one
	// fewer secret to file — but not the default, for the reasons in
	// kitcode.go.
	UseVaultPassword bool

	// Password is the vault password, required when UseVaultPassword is set
	// and checked before anything is written.
	Password string
}

// buildKitLocked assembles the plaintext. The caller must hold at least the
// read lock and the vault must be unlocked.
func (v *Vault) buildKitLocked(kitID string) *Kit {
	snapshot := v.snapshotLocked()

	accounts := make([]provider.Config, 0, len(v.providers))
	for _, cfg := range v.providers {
		// Credentials and all, under the id the account already has. This is
		// the whole point of the file.
		clone := cfg
		clone.Options = make(map[string]string, len(cfg.Options))
		for k, val := range cfg.Options {
			clone.Options[k] = val
		}
		accounts = append(accounts, clone)
	}

	kit := &Kit{
		Version:                KitVersion,
		KitID:                  kitID,
		CreatedAt:              snapshot.CreatedAt,
		AppVersion:             version.String(),
		Snapshot:               *snapshot,
		Accounts:               accounts,
		VaultKey:               base64.StdEncoding.EncodeToString(v.vaultKey),
		KDF:                    v.store.KDF,
		DefaultAccounts:        append([]string(nil), v.store.DefaultAccounts...),
		DefaultScheme:          v.store.DefaultScheme,
		ManifestBackupDisabled: v.store.ManifestBackupDisabled,
	}
	if v.settings != nil {
		kit.MovieAPIKey = v.settings.MovieAPIKey
	}
	kit.ReadHistory = openReadHistory(readHistoryPath(v.path), v.dataKey)
	return kit
}

// KitStatus is what the settings panel draws its nudge from: when the last kit
// was exported, and what has changed since.
type KitStatus struct {
	// ExportedAt is zero on a vault that has never exported one.
	ExportedAt time.Time `json:"exported_at,omitzero"`
	KitID      string    `json:"kit_id,omitempty"`
	Secret     string    `json:"secret,omitempty"`

	// CodeRetained says whether this vault can still show the code for that
	// kit. False for a password-sealed kit, which the panel has to state
	// rather than imply — for those there is no Show code to fall back on.
	CodeRetained bool `json:"code_retained"`

	// No omitempty: a kit exported today is nought days old, and a field that
	// vanishes at nought reaches the browser as undefined and prints as it.
	AgeDays    int  `json:"age_days"`
	Files      int  `json:"files"`
	FilesAdded int  `json:"files_added"`
	Accounts   int  `json:"accounts"`
	Exported   bool `json:"exported"`

	// AccountsChanged and PasswordChangedSince are the two that matter, and
	// the two the panel leads with: a kit that is merely old still recovers
	// everything through the newer index on the accounts, while one that
	// predates an account or a password change recovers less.
	AccountsChanged      bool `json:"accounts_changed"`
	PasswordChangedSince bool `json:"password_changed_since"`
}

// KitStatus reports on the last kit this vault exported.
func (v *Vault) KitStatus() (*KitStatus, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}

	status := &KitStatus{
		Files:    len(v.manifest.Entries),
		Accounts: len(v.providers),
	}
	if v.store.LastKitExportAt.IsZero() {
		return status, nil
	}

	status.Exported = true
	status.ExportedAt = v.store.LastKitExportAt
	status.KitID = v.store.LastKitID
	status.Secret = v.store.LastKitSecret
	if status.Secret == "" {
		status.Secret = KitSecretCode
	}
	status.AgeDays = int(time.Since(v.store.LastKitExportAt).Hours() / 24)
	if added := len(v.manifest.Entries) - v.store.LastKitFileCount; added > 0 {
		status.FilesAdded = added
	}
	status.AccountsChanged = !sameAccountSet(v.providers, v.store.LastKitAccounts)
	status.PasswordChangedSince = v.store.LastPasswordChangeAt.After(v.store.LastKitExportAt)

	if v.settings != nil {
		_, status.CodeRetained = v.settings.KitCodes[v.store.LastKitID]
	}
	return status, nil
}

// sameAccountSet reports whether the connected accounts are exactly the ones a
// kit carries credentials for.
//
// By identity rather than by count, because the state that matters is "there is
// an account this kit cannot restore" — and swapping one account for another
// leaves the count alone while making the kit strictly less able to help.
func sameAccountSet(connected []provider.Config, inKit []string) bool {
	if len(connected) != len(inKit) {
		return false
	}
	known := make(map[string]bool, len(inKit))
	for _, id := range inKit {
		known[id] = true
	}
	for _, cfg := range connected {
		if !known[cfg.ID] {
			return false
		}
	}
	return true
}

// KitCode reads back the recovery code this vault is holding for a kit it
// exported, or "" if it holds none.
//
// Retaining it looks like it gives something away and does not: reading it
// requires an unlocked vault, and anybody with an unlocked vault can export a
// fresh kit with a fresh code in ten seconds. What it buys is the ordinary
// case — a person who still has their working machine, has mislaid the slip of
// paper, and would otherwise be holding a zip that nothing on earth opens.
func (v *Vault) KitCode(kitID string) (string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return "", ErrLocked
	}
	if kitID == "" {
		kitID = v.store.LastKitID
	}
	if v.settings == nil || v.settings.KitCodes == nil {
		return "", nil
	}
	return v.settings.KitCodes[kitID], nil
}

// ForgetKitCode drops the code this vault is holding for a kit.
//
// Offered as tidying up after a rotation, never instead of one: a kit somebody
// else has is not revoked by forgetting its code, because they may have taken
// it from a screenshot, a note app, or the .txt beside the zip. See §8 of
// docs/recovery-kit.md for what actually ends that.
func (v *Vault) ForgetKitCode(kitID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	if kitID == "" {
		kitID = v.store.LastKitID
	}
	if v.settings == nil || v.settings.KitCodes == nil {
		return nil
	}
	delete(v.settings.KitCodes, kitID)
	return v.persistLocked()
}

// newKitID mints the identity a kit is known by, and that its code is stored
// against.
func newKitID() string { return uuid.NewString() }
