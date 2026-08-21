package vault

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// Importing a recovery kit: the other half of kitzip.go.
//
// Seven phases, and the order is the point. The first three are the whole of
// the user's ask — a vault, the clouds reconnected, the tree back — and they
// stand alone. The four after them are about being *current* rather than being
// *usable*, and each of them reports rather than fails.

// The states a restored account can be in. A failure here is never fatal to
// the import: a year-old OAuth token is the expected case, not the edge case,
// and an import that refused to finish because Dropbox wanted a fresh sign-in
// would be useless precisely when it is needed.
const (
	// KitAccountConnected means the account answered.
	KitAccountConnected = "connected"

	// KitAccountNeedsReauth means its sign-in expired or was revoked while the
	// machine was gone. One button fixes it, and the account keeps its id.
	KitAccountNeedsReauth = "needs_reauth"

	// KitAccountNeedsPath means the folder it was connected with is not on
	// this machine — a different home directory, or a different operating
	// system entirely, which is what "a fresh install" usually means.
	KitAccountNeedsPath = "needs_path"

	// KitAccountUnreachable is everything else: network, DNS, a bucket that
	// has gone. Retrying later, or Reconcile, picks it up.
	KitAccountUnreachable = "unreachable"
)

// What would put a failed account right. The status says what is wrong; this
// says which door to open, because they are not the same question and the
// browser should not have to infer one from the other.
//
// A Dropbox account whose consent was revoked and an S3 bucket whose keys were
// rotated both fail authentication, and the repairs share no steps: one is a
// trip through somebody's consent screen, the other is a form. Only the backend
// knows which, so the backend is asked.
const (
	// KitRepairSignIn sends the account back through its provider's consent
	// screen, reusing the OAuth app the kit carried.
	KitRepairSignIn = "sign_in"

	// KitRepairSettings is the form: rotated keys, a moved bucket, a WebDAV
	// password that changed.
	KitRepairSettings = "settings"

	// KitRepairPath is a folder that is not on this machine, which is what a
	// fresh install usually means for a synced-folder account.
	KitRepairPath = "path"

	// KitRepairRetry is for an account with nothing to fix — the network, the
	// service, or the machine was simply down.
	KitRepairRetry = "retry"
)

// KitAccountResult is one account as the import left it.
type KitAccountResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`

	// Detail is what to say under the account's name on the report: the path
	// that is not here, or what the backend answered.
	Detail string `json:"detail,omitempty"`

	// PathOption names the setting a re-point would change, so the browser can
	// offer a folder picker without knowing anything about backends.
	PathOption string `json:"path_option,omitempty"`

	// Repair is which door to open: sign_in, settings, path or retry. Empty on
	// an account that connected.
	Repair string `json:"repair,omitempty"`
}

// KitImportReport describes what an import did, or would do.
type KitImportReport struct {
	KitID      string    `json:"kit_id"`
	KitCreated time.Time `json:"kit_created_at"`

	// IndexSource says where the installed index came from: "kit", or the id
	// of the account whose manifest.sand turned out to be newer. IndexAt is
	// that index's date.
	IndexSource string    `json:"index_source"`
	IndexName   string    `json:"index_source_name,omitempty"`
	IndexAt     time.Time `json:"index_at"`

	Accounts []KitAccountResult `json:"accounts"`

	// The same pairs RecoveryReport counts in, for the same reason: enough
	// parts rebuild a file, so one account short costs nothing and two costs
	// the file — and the bytes are usually the answer to "how bad is this".
	Files            int   `json:"files"`
	Folders          int   `json:"folders"`
	Recoverable      int   `json:"recoverable"`
	Bytes            int64 `json:"bytes"`
	RecoverableBytes int64 `json:"recoverable_bytes"`
	Lost             int   `json:"lost"`
	LostBytes        int64 `json:"lost_bytes"`

	Missing          []MissingFile    `json:"missing,omitempty"`
	MissingTruncated int              `json:"missing_truncated,omitempty"`
	Blocking         []MissingAccount `json:"blocking,omitempty"`

	// Repointed counts shard records that had to be moved to a different
	// account than the index named. On a kit import it is normally zero — the
	// ids came back intact — and that is worth showing rather than hiding.
	Repointed int `json:"repointed"`

	// Orphans are objects sitting on an account that the index does not name.
	// Counted and reported, never deleted: an orphan means the index is older
	// than the storage, and the honest thing to do with somebody's data that
	// the index has forgotten is to say so.
	Orphans     int   `json:"orphans"`
	OrphanBytes int64 `json:"orphan_bytes"`

	SubVaults int `json:"sub_vaults"`

	// PasswordChanged says the copies of the index on the accounts did not open
	// under the key the kit carries — uniformly, on every account, which is
	// what tells it from a damaged file. The kit's own index was used instead.
	//
	// A password change after the kit was made is the usual cause, and the one
	// worth naming; another vault having claimed these accounts since looks
	// identical from here, so what is said about it should describe the
	// observation rather than assert the reason.
	PasswordChanged bool `json:"password_changed"`

	Warnings []string `json:"warnings,omitempty"`
}

// Complete reports whether every file the kit described came back.
func (r *KitImportReport) Complete() bool { return r.Lost == 0 }

// KitImportOptions says how to import.
type KitImportOptions struct {
	// Password is what the recovered vault will be unlocked with from now on.
	// It need not be the one the lost vault used — and for a code-sealed kit
	// it cannot be inferred, so it is required.
	Password string

	// Replace allows importing over a vault that already holds files. Without
	// it that is refused, on exactly the terms Recover refuses it: adopting a
	// data key when files depend on the current one destroys them.
	Replace bool

	// OldPassword is optional, and only useful when the kit predates a
	// password change: the copies of manifest.sand on the accounts are sealed
	// under a vault key the kit does not carry, and this is what would open
	// them. Skipping it is a real option and costs only the files added
	// between the export and the change.
	OldPassword string

	// SkipCloudIndex leaves phase 4 out — no newer index is fetched, and the
	// kit's own is installed. What "skip" on the password-changed prompt does.
	SkipCloudIndex bool
}

// ReadKitZip pulls kit.sand out of a recovery kit archive.
//
// It accepts a bare kit.sand too, so that somebody who unpacked the zip and
// kept only the one file that matters is not stuck.
func ReadKitZip(data []byte) ([]byte, error) {
	if bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte("{")) {
		return data, nil
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: it is neither a zip nor a %s", ErrNotAKit, KitFile)
	}
	for _, f := range zr.File {
		if f.Name != KitFile {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		// Bounded so a hostile zip cannot ask for unlimited memory. A kit's
		// index is JSON, and a hundred megabytes of it is a vault far larger
		// than anything this format was drawn for.
		return io.ReadAll(io.LimitReader(rc, 512<<20))
	}
	return nil, fmt.Errorf("%w: the archive holds no %s", ErrNotAKit, KitFile)
}

// ImportKit rebuilds this vault from a kit: the clouds, the keys, the tree.
func (v *Vault) ImportKit(ctx context.Context, kit *Kit, opts KitImportOptions) (*KitImportReport, error) {
	if strings.TrimSpace(opts.Password) == "" {
		return nil, fmt.Errorf("choose a password for the recovered vault")
	}

	report := &KitImportReport{
		KitID:       kit.KitID,
		KitCreated:  kit.CreatedAt,
		IndexSource: "kit",
		IndexAt:     kit.Snapshot.CreatedAt,
		SubVaults:   len(kit.Snapshot.SubVaults),
	}

	// --- Phase 0: refuse to overwrite ---------------------------------------
	if err := v.refuseKitOverwrite(opts.Replace); err != nil {
		return nil, err
	}

	// Nothing is pushed to an account until the whole import has settled.
	// Every phase below persists, and every persist would otherwise schedule a
	// push of an index that is still half-built at accounts that still hold
	// the *lost* vault's copy — which trips the foreign-backup guard and says
	// so, in the middle of the one operation where finding another vault's
	// backup on every account is exactly what should happen. Phase 7 does the
	// single forced push that replaces them all.
	v.holdBackups()
	defer v.releaseBackups()

	// --- Phase 1: mint the vault, adopt the kit's keys ----------------------
	if err := v.adoptKit(kit, opts.Password); err != nil {
		return nil, err
	}

	// --- Phase 2: reconnect every account, keeping its id -------------------
	report.Accounts = v.restoreKitAccounts(ctx, kit)

	reachable := map[string]bool{}
	for _, a := range report.Accounts {
		if a.Status == KitAccountConnected {
			reachable[a.ID] = true
		}
	}

	// --- Phase 3+4: the index, preferring a newer one off the clouds --------
	snapshot := &kit.Snapshot
	if !opts.SkipCloudIndex && len(reachable) > 0 {
		newer, source, name, changed, warns := v.newerCloudIndex(ctx, kit, opts.OldPassword, reachable)
		report.Warnings = append(report.Warnings, warns...)
		report.PasswordChanged = changed
		if newer != nil {
			snapshot = newer
			report.IndexSource = source
			report.IndexName = name
			report.IndexAt = newer.CreatedAt
			report.SubVaults = len(newer.SubVaults)
		}
	}

	if err := v.installKitIndex(snapshot); err != nil {
		return nil, err
	}

	// --- Phase 5: discover — confirm every part is where the index says -----
	if err := v.discoverKitShards(ctx, report); err != nil {
		report.Warnings = append(report.Warnings, "checking the accounts against the index: "+err.Error())
	}

	// --- Phase 6: the sidecar ----------------------------------------------
	v.restoreKitReadHistory(kit, report)

	// --- Phase 7: push the index back --------------------------------------
	// Every account should carry the current index under the current password,
	// and the guard against clobbering a foreign backup has to stop being
	// armed against this vault. Same closing move as handleRecoveryRun.
	v.releaseBackups()
	v.AwaitBackupSync()
	if warns, err := v.SyncManifestBackup(ctx, true); err != nil {
		if !errors.Is(err, ErrBackupRefused) {
			report.Warnings = append(report.Warnings,
				"the recovered index could not be copied back to the accounts: "+err.Error())
		}
	} else {
		report.Warnings = append(report.Warnings, warns...)
	}

	sort.Strings(report.Warnings)
	return report, nil
}

// refuseKitOverwrite is Recover's rule, for the same reason: adopting a data
// key when files depend on the current one destroys them.
func (v *Vault) refuseKitOverwrite(replace bool) error {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.store == nil {
		return nil // No vault here at all; adoptKit creates one.
	}
	if replace {
		return nil
	}
	if v.dataKey == nil {
		// A vault file exists and is shut. Not knowing what is in it is a
		// reason to refuse rather than to proceed: adoptKit would write a whole
		// new store over it, and whatever it held — files, sub vaults, a data
		// key — would be gone with nothing left to say what it was.
		return fmt.Errorf(
			"there is already a vault at %s and it is locked, so what importing over it would "+
				"destroy cannot be known — unlock it first, or import into a fresh vault", v.path)
	}
	if n := len(v.manifest.Entries); n > 0 {
		return fmt.Errorf(
			"this vault already holds %d file(s) — importing a kit would replace its data key and "+
				"strand them; import into a fresh vault instead", n)
	}
	if n := len(v.store.SubVaults); n > 0 {
		return fmt.Errorf(
			"this vault already holds %d sub vault(s) — importing a kit would replace them, and "+
				"nothing here can open them to say what would be lost", n)
	}
	return nil
}

// adoptKit creates the vault under the new password and installs everything
// the kit carries except the manifest and the accounts.
func (v *Vault) adoptKit(kit *Kit, password string) error {
	dataKey, err := base64.StdEncoding.DecodeString(kit.Snapshot.DataKey)
	if err != nil || len(dataKey) != DataKeySize {
		return fmt.Errorf("this kit carries no usable data key")
	}
	retired, err := snapshotRetiredKeys(&kit.Snapshot)
	if err != nil {
		return err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	// A fresh store under the chosen password. Init's work, done here so the
	// adoption is one write rather than a create followed by a rewrite.
	policy := kit.Snapshot.Policy
	if !policy.Valid() {
		policy = PolicyStrict
	}
	sf, _, err := newStore(password, policy)
	if err != nil {
		return err
	}

	params, salt, err := sf.KDF.toArgon2()
	if err != nil {
		return err
	}
	vaultKey := crypto.DeriveKey(password, salt, params)

	// The kit's keys, not the fresh ones newStore just minted: these are what
	// the parts already sitting on the accounts were encrypted under.
	if sf.DataKey, err = seal(vaultKey, dataKey); err != nil {
		crypto.ZeroBytes(vaultKey)
		return err
	}
	sf.DataKeyID = kit.Snapshot.KeyID
	sf.RetiredKeys = nil
	for id, key := range retired {
		wrapped, err := seal(vaultKey, key)
		if err != nil {
			crypto.ZeroBytes(vaultKey)
			return err
		}
		sf.RetiredKeys = append(sf.RetiredKeys, wrappedKey{ID: id, Key: wrapped})
	}
	sort.Slice(sf.RetiredKeys, func(i, j int) bool { return sf.RetiredKeys[i].ID < sf.RetiredKeys[j].ID })

	// The sealed sub-vault records come back as they were. Nothing here can
	// open them and nothing needs to: they are ciphertext to this vault until
	// somebody types the password that made them, and then they are exactly
	// what they were.
	sf.SubVaults = append([]subVaultRecord(nil), kit.Snapshot.SubVaults...)
	sf.DefaultAccounts = append([]string(nil), kit.DefaultAccounts...)
	sf.DefaultScheme = kit.DefaultScheme
	sf.ManifestBackupDisabled = kit.ManifestBackupDisabled

	// The copies on the accounts are sealed under the lost vault's key, which
	// is not this one, so the foreign-backup guard has to stand aside for
	// exactly this vault's next push.
	sf.BackupNeedsForce = !sf.ManifestBackupDisabled

	settings := &vaultSettings{MovieAPIKey: kit.MovieAPIKey}
	if !settings.empty() {
		blob, err := sealJSON(vaultKey, settings)
		if err != nil {
			crypto.ZeroBytes(vaultKey)
			return err
		}
		sf.Settings = &blob
	}
	if sf.Providers, err = sealJSON(vaultKey, []provider.Config{}); err != nil {
		crypto.ZeroBytes(vaultKey)
		return err
	}
	if sf.Manifest, err = sealJSON(vaultKey, newManifest()); err != nil {
		crypto.ZeroBytes(vaultKey)
		return err
	}

	if err := writeStore(v.path, sf); err != nil {
		crypto.ZeroBytes(vaultKey)
		return err
	}

	crypto.ZeroBytes(v.vaultKey)
	crypto.ZeroBytes(v.dataKey)
	for _, key := range v.retired {
		crypto.ZeroBytes(key)
	}

	v.store = sf
	v.vaultKey = vaultKey
	v.dataKey = dataKey
	v.dataKeyID = kit.Snapshot.KeyID
	v.retired = retired
	v.providers = []provider.Config{}
	v.manifest = newManifest()
	v.settings = settings
	v.subs = map[string]*subVault{}
	v.resetLiveCache()
	return nil
}

// restoreKitAccounts reconnects every account the kit names, keeping its id,
// and reports what each one answered.
func (v *Vault) restoreKitAccounts(ctx context.Context, kit *Kit) []KitAccountResult {
	results := make([]KitAccountResult, len(kit.Accounts))

	var wg sync.WaitGroup
	for i, cfg := range kit.Accounts {
		result := KitAccountResult{ID: cfg.ID, Name: cfg.Name, Kind: string(cfg.Kind)}

		if err := v.RestoreProvider(cfg); err != nil {
			result.Status = KitAccountUnreachable
			result.Detail = err.Error()
			results[i] = result
			continue
		}
		results[i] = result

		wg.Add(1)
		go func(i int, cfg provider.Config) {
			defer wg.Done()
			results[i].Status, results[i].Detail, results[i].PathOption = v.probeRestored(ctx, cfg)
			results[i].Repair = repairFor(cfg, results[i].Status)
		}(i, cfg)
	}
	wg.Wait()
	return results
}

// probeRestored asks one restored account whether it is reachable, and tells
// the three interesting failures apart so each can be offered as one button.
func (v *Vault) probeRestored(ctx context.Context, cfg provider.Config) (status, detail, pathOption string) {
	// A path-configured backend is worth checking before the backend is even
	// built: "your folder is not on this machine" is a far better sentence
	// than whatever a filesystem error says, and it is the commonest failure
	// of all when a kit lands on a different computer.
	if key, root := configuredPath(cfg); key != "" {
		if _, err := os.Stat(root); err != nil {
			return KitAccountNeedsPath, fmt.Sprintf("%s is not on this machine", root), key
		}
	}

	p, err := v.buildProvider(cfg)
	if err != nil {
		return KitAccountUnreachable, err.Error(), ""
	}

	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := p.Ping(pingCtx); err != nil {
		if key, root := configuredPath(cfg); key != "" {
			return KitAccountNeedsPath, fmt.Sprintf("%s: %v", root, err), key
		}
		if looksLikeAuthFailure(err) {
			return KitAccountNeedsReauth, "its sign-in expired while the machine was gone", ""
		}
		return KitAccountUnreachable, err.Error(), ""
	}
	return KitAccountConnected, "", ""
}

// repairFor says which door would put a failed account right.
//
// Asked of the backend rather than guessed from the failure: whether an account
// signs in or is configured is a fact about the backend, and it is the one that
// decides whether the button says "sign in again" or "fix settings".
func repairFor(cfg provider.Config, status string) string {
	switch status {
	case KitAccountConnected:
		return ""
	case KitAccountNeedsPath:
		return KitRepairPath
	case KitAccountNeedsReauth:
		if spec, ok := provider.SpecFor(cfg.Kind); ok && spec.OAuth != nil {
			return KitRepairSignIn
		}
		// A backend with no consent screen answers a rejected credential with
		// the same 401 an expired token gets. There is nothing to sign in to;
		// what it wants is the key typed again.
		return KitRepairSettings
	default:
		return KitRepairRetry
	}
}

// configuredPath returns the option holding a backend's folder *on this
// machine*, and the folder, for the backends that have one.
//
// It asks the backend's own spec rather than guessing at key names, because
// the guess is wrong in a way that matters: Box and OneDrive both call their
// setting "folder" and both mean a folder inside somebody else's service,
// defaulting to "sand". Treating that as a local path would report every such
// account as "needs a folder" without ever pinging it, and would then offer a
// button that writes a local path into a remote setting.
//
// FieldSpec.Directory is exactly the distinction — it is what puts a folder
// picker on the connect form — so it is what is asked.
func configuredPath(cfg provider.Config) (string, string) {
	spec, ok := provider.SpecFor(cfg.Kind)
	if !ok {
		return "", ""
	}
	for _, f := range spec.Fields {
		if !f.Directory {
			continue
		}
		if value := strings.TrimSpace(cfg.Option(f.Key)); value != "" {
			return f.Key, value
		}
	}
	return "", ""
}

// looksLikeAuthFailure guesses whether a ping failed because the credentials
// have gone stale rather than because the network has.
//
// A guess, deliberately: the backends do not agree on how to say it, and the
// cost of being wrong is one wrong word on a button that still does something
// useful. An account marked needs_reauth that was really a network blip signs
// in again and works; one marked unreachable that really wanted a sign-in is
// retried and says so more clearly the second time.
func looksLikeAuthFailure(err error) bool {
	text := strings.ToLower(err.Error())
	for _, needle := range []string{
		"401", "403", "unauthorized", "unauthorised", "invalid_grant", "invalid grant",
		"expired", "revoked", "access denied", "accessdenied", "forbidden",
		"invalid credentials", "signaturedoesnotmatch", "invalidaccesskeyid", "token",
	} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// newerCloudIndex looks for a copy of manifest.sand newer than the kit.
//
// This is what makes an old kit as good as a fresh one. A kit from March
// describes March; the copies on the accounts are rewritten on every index
// change, so a machine that died in August left one dated August sitting on
// every cloud it could still reach. The kit carries the vault key those copies
// are sealed under precisely so this can be read without the old password.
func (v *Vault) newerCloudIndex(
	ctx context.Context, kit *Kit, oldPassword string, reachable map[string]bool,
) (best *Snapshot, sourceID, sourceName string, passwordChanged bool, warnings []string) {
	vaultKey, err := kit.VaultKeyBytes()
	if err != nil {
		return nil, "", "", false, []string{"this kit carries no vault key, so the copies of the " +
			"index on your accounts could not be read: " + err.Error()}
	}
	defer crypto.ZeroBytes(vaultKey)

	v.mu.RLock()
	configs := append([]provider.Config(nil), v.providers...)
	v.mu.RUnlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	refused := 0
	looked := 0

	for _, cfg := range configs {
		if !reachable[cfg.ID] {
			continue
		}
		wg.Add(1)
		go func(cfg provider.Config) {
			defer wg.Done()

			p, err := v.buildProvider(cfg)
			if err != nil {
				return
			}
			fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			data, err := p.Get(fetchCtx, BackupKey)
			if err != nil {
				return
			}

			mu.Lock()
			looked++
			mu.Unlock()

			snapshot, err := openBackupWithKey(data, vaultKey)
			if err != nil {
				// Either the bytes are damaged or this vault key is the wrong
				// one. Which it is cannot be told from one account — it can be
				// told from all of them answering the same way, below.
				mu.Lock()
				refused++
				mu.Unlock()
				return
			}

			mu.Lock()
			defer mu.Unlock()
			if best == nil || snapshot.CreatedAt.After(best.CreatedAt) {
				best, sourceID, sourceName = snapshot, cfg.ID, cfg.Name
			}
		}(cfg)
	}
	wg.Wait()

	// Every account refusing the same key, uniformly, is the signature of a
	// password change made after the kit — not of corruption, which does not
	// happen to four accounts at once.
	if looked > 0 && refused == looked && best == nil {
		passwordChanged = true
		if strings.TrimSpace(oldPassword) != "" {
			if snapshot, id, name, warns := v.cloudIndexWithPassword(ctx, configs, reachable, oldPassword); snapshot != nil {
				return snapshot, id, name, true, warns
			}
			warnings = append(warnings,
				"that password did not open the copies of the index on your accounts either, "+
					"so the kit's own index was used")
		}
		// No warning here: PasswordChanged is the structured form of exactly
		// this fact, and both the browser and the CLI render it from the flag.
		// Saying it twice in two voices reads as two problems.
		return nil, "", "", true, warnings
	}

	if best != nil && !best.CreatedAt.After(kit.Snapshot.CreatedAt) {
		// The kit is as new as anything out there, which is the ordinary case
		// for a kit exported this morning.
		return nil, "", "", false, warnings
	}
	return best, sourceID, sourceName, false, warnings
}

// cloudIndexWithPassword is newerCloudIndex's second try, with the password the
// vault was using when the machine died.
func (v *Vault) cloudIndexWithPassword(
	ctx context.Context, configs []provider.Config, reachable map[string]bool, password string,
) (best *Snapshot, sourceID, sourceName string, warnings []string) {
	for _, cfg := range configs {
		if !reachable[cfg.ID] {
			continue
		}
		p, err := v.buildProvider(cfg)
		if err != nil {
			continue
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		data, err := p.Get(fetchCtx, BackupKey)
		cancel()
		if err != nil {
			continue
		}
		snapshot, err := OpenBackup(data, password)
		if err != nil {
			continue
		}
		if best == nil || snapshot.CreatedAt.After(best.CreatedAt) {
			best, sourceID, sourceName = snapshot, cfg.ID, cfg.Name
		}
	}
	return best, sourceID, sourceName, warnings
}

// installKitIndex adopts a snapshot's manifest, and the key generations it
// names that the kit did not.
//
// A snapshot taken off a cloud after a password change describes files on a
// generation this vault has not got, so its keys are unioned in rather than
// replacing what adoptKit installed.
func (v *Vault) installKitIndex(snapshot *Snapshot) error {
	retired, err := snapshotRetiredKeys(snapshot)
	if err != nil {
		return err
	}
	dataKey, err := base64.StdEncoding.DecodeString(snapshot.DataKey)
	if err != nil || len(dataKey) != DataKeySize {
		return fmt.Errorf("this index carries no usable data key")
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}

	manifest := newManifest()
	manifest.Entries = append([]*Entry(nil), snapshot.Manifest.Entries...)
	manifest.Folders = append([]string(nil), snapshot.Manifest.Folders...)
	manifest.Thumbs = snapshot.Manifest.Thumbs
	manifest.MovieFolders = snapshot.Manifest.MovieFolders
	manifest.Movies = snapshot.Manifest.Movies
	manifest.FolderArt = snapshot.Manifest.FolderArt
	manifest.SubVaults = append([]*SubVaultMeta(nil), snapshot.Manifest.SubVaults...)
	// Deliberately not carried across: the ids are preserved by a kit import,
	// so there is nothing to translate. A remap left over from the snapshot's
	// own history would send a sub vault's shard records somewhere they are
	// not.
	manifest.AccountRemap = nil
	v.manifest = manifest

	// The generation the snapshot is on becomes the active one, and anything
	// this vault already held that the snapshot does not name is kept beside
	// it — a file on either side of an unfinished re-encryption needs both.
	if snapshot.KeyID != v.dataKeyID {
		v.retired[v.dataKeyID] = append([]byte(nil), v.dataKey...)
		crypto.ZeroBytes(v.dataKey)
		v.dataKey = dataKey
		v.dataKeyID = snapshot.KeyID
	}
	for id, key := range retired {
		if _, ok := v.retired[id]; !ok && id != v.dataKeyID {
			v.retired[id] = key
		}
	}
	delete(v.retired, v.dataKeyID)

	// Unconditional, as Recover does it. Guarding on a non-empty list would
	// keep sub vaults that were deleted between the kit and the newer index —
	// records this vault cannot open, cannot describe, and did not agree to
	// hold — while the report said there were none.
	v.store.SubVaults = append([]subVaultRecord(nil), snapshot.SubVaults...)

	wrapped, err := seal(v.vaultKey, v.dataKey)
	if err != nil {
		return err
	}
	v.store.DataKey = wrapped
	v.store.DataKeyID = v.dataKeyID
	v.store.RetiredKeys = nil
	for id, key := range v.retired {
		sealedKey, err := seal(v.vaultKey, key)
		if err != nil {
			return err
		}
		v.store.RetiredKeys = append(v.store.RetiredKeys, wrappedKey{ID: id, Key: sealedKey})
	}
	sort.Slice(v.store.RetiredKeys, func(i, j int) bool {
		return v.store.RetiredKeys[i].ID < v.store.RetiredKeys[j].ID
	})

	return v.persistLocked()
}

// discoverKitShards checks the index against what the accounts actually hold,
// and counts the shortfall the same way a recovery does.
//
// Because the ids were preserved, the overwhelmingly common result is that the
// index is already right and this pass is a verification. It is still run, and
// it still fixes what it finds: a part relocated after the kit was exported, a
// folder moved between accounts by hand.
func (v *Vault) discoverKitShards(ctx context.Context, report *KitImportReport) error {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return ErrLocked
	}
	configs := append([]provider.Config(nil), v.providers...)
	entries := append([]*Entry(nil), v.manifest.Entries...)
	folders := append([]string(nil), v.manifest.Folders...)
	subVaults := append([]*SubVaultMeta(nil), v.manifest.SubVaults...)
	v.mu.RUnlock()

	holders, answered, warnings := v.locateShardsAnswered(ctx, configs)
	report.Warnings = append(report.Warnings, warnings...)

	absent := newReconcileAccounts()
	claimed := map[string]bool{}
	repointed := map[string]Shard{}

	folderSet := map[string]bool{}
	for _, f := range folders {
		folderSet[f] = true
	}

	for _, entry := range entries {
		reachable := 0
		var lost lostShards

		for _, shard := range entry.Shards {
			switch cfg, found := holders[shard.Key]; {
			case found:
				claimed[shard.Key] = true
				if cfg.ID != shard.ProviderID {
					report.Repointed++
					fixed := shard
					fixed.ProviderID = cfg.ID
					fixed.ProviderName = cfg.Name
					fixed.ProviderKind = string(cfg.Kind)
					repointed[entry.ID+"\x00"+shard.Key] = fixed
				}
				reachable++
			case answered[shard.ProviderID]:
				// The account answered the listing and this key was not in it.
				// The record is kept — one absent object should not throw
				// placement away, and that is what the health check is for —
				// but it does not count towards being able to open the file.
				absent.note(shard, &lost)
			default:
				// The account never answered at all: not reachable here, and
				// not a judgement about what it holds. A kit restores every
				// account's credentials whether or not the account answers, so
				// "connected" cannot stand in for "reachable" the way it does
				// after a hand-reconnected recovery.
				absent.note(shard, &lost)
			}
		}

		report.Files++
		report.Bytes += entry.Size
		if entry.Dir != "/" {
			folderSet[entry.Dir] = true
		}

		if reachable >= entry.Scheme().Data {
			report.Recoverable++
			report.RecoverableBytes += entry.Size
			continue
		}
		report.Lost++
		report.LostBytes += entry.Size
		absent.blame(&lost)
		if len(report.Missing) < maxMissingListed {
			report.Missing = append(report.Missing, MissingFile{
				Path:        entry.Path(),
				Size:        entry.Size,
				PartsFound:  reachable,
				PartsNeeded: entry.Scheme().Data,
				Accounts:    lost.names,
			})
		} else {
			report.MissingTruncated++
		}
	}

	// A sub vault's objects are the index's too, even though nothing here can
	// read what they are. Counting them as claimed is what keeps them out of
	// the orphan tally.
	for _, meta := range subVaults {
		for _, key := range subVaultObjectKeys(meta) {
			claimed[key] = true
		}
	}

	for key, cfg := range holders {
		if claimed[key] {
			continue
		}
		report.Orphans++
		report.OrphanBytes += holderSize(cfg, key)
	}

	report.Folders = len(folderSet)
	report.Blocking = absent.list()

	if len(repointed) == 0 {
		return nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	for _, entry := range v.manifest.Entries {
		for i, shard := range entry.Shards {
			if fixed, ok := repointed[entry.ID+"\x00"+shard.Key]; ok {
				entry.Shards[i] = fixed
			}
		}
	}
	return v.persistLocked()
}

// subVaultObjectKeys is every object a sealed sub vault owns.
//
// Nothing here can read what those objects are — the section is sealed and the
// password that opens it is not the one doing the importing — but the keys are
// derivable from the inventory without opening anything, which is enough to
// keep a sub vault's parts out of the orphan tally. Counting them as forgotten
// would tell somebody their vault was littered with data it has lost track of,
// when in fact it is exactly where it should be.
func subVaultObjectKeys(meta *SubVaultMeta) []string {
	if meta == nil {
		return nil
	}
	var keys []string
	for _, item := range meta.Inventory {
		for _, part := range item.Parts {
			if item.ChunkCount > 0 {
				for chunk := 0; chunk < item.ChunkCount; chunk++ {
					keys = append(keys, ChunkShardKey(item.ArchiveID, chunk, part.Part))
				}
				continue
			}
			keys = append(keys, ShardKey(item.ArchiveID, part.Part))
		}
	}
	return keys
}

// holderSize is a placeholder for an orphan's weight. locateShards keeps only
// which account holds a key, not how big the object is, and re-listing to find
// out would double the cost of the pass for a number that is only ever shown
// beside a count.
func holderSize(provider.Config, string) int64 { return 0 }

// restoreKitReadHistory writes the sidecar back, so the read counters resume
// from where they stopped rather than from zero.
//
// A small thing, and exactly the kind of small thing whose absence makes a
// restored machine feel like somebody else's.
func (v *Vault) restoreKitReadHistory(kit *Kit, report *KitImportReport) {
	if kit.ReadHistory == nil {
		return
	}
	v.mu.RLock()
	dataKey := append([]byte(nil), v.dataKey...)
	path := v.path
	v.mu.RUnlock()
	defer crypto.ZeroBytes(dataKey)

	if len(dataKey) == 0 {
		return
	}
	if err := writeReadHistory(path, kit.ReadHistory, dataKey); err != nil {
		report.Warnings = append(report.Warnings, "the read history could not be restored: "+err.Error())
		return
	}
	v.mu.Lock()
	v.loadReadHistoryLocked()
	v.mu.Unlock()
}
