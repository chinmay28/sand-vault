package vault

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

// Taking ownership of files that were recovered rather than uploaded.
//
// Recovery adopts the lost vault's data key, because that key is the only thing
// that opens the parts already sitting on the accounts. That gets the files
// back, and it leaves something behind: those parts are still encrypted under
// the old key, which the old password still derives. Every copy of the old
// manifest.sand — one per account, and any that was ever taken off one — hands
// that key to whoever has the old password. Recovery replaces the copies it can
// reach; it cannot replace the ones it cannot.
//
// So the recovered files are readable by the vault that died for as long as
// they sit on its key. Reclaiming is what ends that: a fresh data key sealed
// under *this* vault's password, every file rebuilt onto it, the old parts
// erased, and — since the files are being gathered and scattered anyway — the
// chance to put them on whichever accounts the user actually wants to keep
// using rather than the ones the dead vault happened to choose.
//
// It costs a download and an upload of the whole vault, which is why it is
// offered rather than done: a recovery has to work with the network you have,
// and this can wait for the one you want.

// RotateDataKey mints a fresh data key for new writes and retires the current
// one, without touching the password.
//
// ChangePassword does this too, as half of what it does — but it needs the old
// password to unseal the vault file, and there is no password to hand here. The
// key that wraps the data key is the one derived at unlock and held in memory,
// and it is not changing; only what it wraps is. So this rewrites the vault
// file with the keys swapped and leaves the KDF, the password and the check
// value exactly as they were.
//
// Every stored file names the generation it is on, so nothing becomes
// unreadable: the retired key stays in the vault until the last file leaves it,
// which is what MigrateFiles is for.
func (v *Vault) RotateDataKey() (string, error) {
	fresh := make([]byte, DataKeySize)
	if _, err := io.ReadFull(rand.Reader, fresh); err != nil {
		return "", fmt.Errorf("generating a data key: %w", err)
	}
	freshID := newKeyID()

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.dataKey == nil {
		crypto.ZeroBytes(fresh)
		return "", ErrLocked
	}

	wrapped, err := seal(v.vaultKey, fresh)
	if err != nil {
		crypto.ZeroBytes(fresh)
		return "", err
	}
	// The outgoing generation has to be kept, and kept wrapped: every file is
	// still on it until it has been migrated, and a vault locked halfway
	// through has to be able to read them again on the next unlock.
	retiring, err := seal(v.vaultKey, v.dataKey)
	if err != nil {
		crypto.ZeroBytes(fresh)
		return "", err
	}

	previous := struct {
		dataKey    []byte
		dataKeyID  string
		retired    map[string][]byte
		storeKey   sealed
		storeKeyID string
		storeOld   []wrappedKey
		inherited  string
	}{v.dataKey, v.dataKeyID, v.retired, v.store.DataKey, v.store.DataKeyID, v.store.RetiredKeys,
		v.store.InheritedKeyID}

	retired := map[string][]byte{v.dataKeyID: v.dataKey}
	for id, key := range v.retired {
		retired[id] = key
	}

	v.store.DataKey = wrapped
	v.store.DataKeyID = freshID
	v.store.RetiredKeys = append(append([]wrappedKey(nil), v.store.RetiredKeys...),
		wrappedKey{ID: v.dataKeyID, Key: retiring})
	v.dataKey = fresh
	v.dataKeyID = freshID
	v.retired = retired
	// New writes are on a key this vault minted, so the standing warning about
	// an inherited one has done its job. Files still on the old generation are
	// counted by Stats.Pending until they move, which is the signal that
	// belongs to a migration rather than to a recovery.
	v.store.InheritedKeyID = ""

	// persistLocked prunes any generation nothing points at, so a vault holding
	// no files drops the key it just retired in the same write.
	if err := v.persistLocked(); err != nil {
		v.dataKey = previous.dataKey
		v.dataKeyID = previous.dataKeyID
		v.retired = previous.retired
		v.store.DataKey = previous.storeKey
		v.store.DataKeyID = previous.storeKeyID
		v.store.RetiredKeys = previous.storeOld
		v.store.InheritedKeyID = previous.inherited
		crypto.ZeroBytes(fresh)
		return "", err
	}
	return freshID, nil
}

// ReclaimReport says what taking ownership did.
type ReclaimReport struct {
	// KeyID is the generation the files were moved onto.
	KeyID string `json:"key_id"`

	// Accounts is where the re-encrypted parts were put, empty when each file
	// went back to the accounts it was already on.
	Accounts []string `json:"accounts,omitempty"`

	*MigrationReport
}

// Reclaim re-encrypts every stored file under a data key of this vault's own,
// optionally moving it onto the accounts named.
//
// This is the step that finishes a recovery. Until it runs, the files are on
// the key the vault that died was using, and the password that opened that
// vault still opens them.
//
// accounts is a set of connected account IDs, or nil to leave each file where
// it is. Naming fewer than a file has parts is refused rather than quietly
// storing fewer copies — see resolveAccounts and BuildPlan, which enforce the
// placement policy on the way through.
//
// Long, interruptible and repeatable, exactly as a password change is: the key
// rotates in a single write, and every file then moves on its own and commits
// on its own. Whatever moved stays moved.
func (v *Vault) Reclaim(ctx context.Context, accounts []string, progress ProgressFunc) (*ReclaimReport, error) {
	// Checked before anything is rotated, so a selection that cannot hold a
	// file fails with the vault untouched rather than halfway.
	if len(accounts) > 0 {
		v.mu.RLock()
		byID := map[string]struct{}{}
		for _, cfg := range v.providers {
			byID[cfg.ID] = struct{}{}
		}
		policy := v.store.Policy
		fallback := v.defaultSchemeLocked()
		v.mu.RUnlock()

		for _, id := range accounts {
			if _, ok := byID[id]; !ok {
				return nil, fmt.Errorf("no connected account with id %s", id)
			}
		}
		// The vault's default where it fits, and failing that the code the
		// count settles — the same order the scatter itself will use, so a
		// selection this accepts is one the migration can actually carry out.
		// A count that names no code at all is caught here, before the key is
		// rotated.
		scheme := fallback
		if scheme.Total != len(accounts) {
			var err error
			if scheme, err = SchemeFor(len(accounts)); err != nil {
				return nil, err
			}
		}
		if _, err := BuildPlan(accounts, policy, scheme, 0); err != nil {
			return nil, err
		}
	}

	// Sealed under the key being retired, and cheaper to draw again than to
	// gather and scatter. Dropped while that key is still the vault's own —
	// the main vault's alone, since a sub vault's packs are on a key this is
	// not rotating.
	v.dropAllThumbs(ctx, MainScope)

	keyID, err := v.RotateDataKey()
	if err != nil {
		return nil, err
	}

	report, err := v.MigrateFilesTo(ctx, accounts, progress)
	if report == nil {
		report = &MigrationReport{}
	}
	return &ReclaimReport{KeyID: keyID, Accounts: accounts, MigrationReport: report}, err
}
