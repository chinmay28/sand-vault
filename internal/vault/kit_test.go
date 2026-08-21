package vault

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/crypto"
	"github.com/chinmay28/sand-vault/internal/provider"
)

// Sealing a kit for real costs Argon2id at 512 MB, which is right for a secret
// typed once in three years and ruinous for a test suite. Opening always uses
// the parameters written into the envelope, so lowering what a *new* kit is
// sealed under cannot weaken a kit anybody actually holds.
func init() {
	kitKDFParams = crypto.Argon2Params{Time: 1, Memory: 8, Threads: 1, SaltLen: 16, KeyLen: 32}
}

// Small shims so the tests read as tests rather than as plumbing.
func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func readAllLimited(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, 64<<20))
}

// exportTestKit writes a kit to a temp file and returns the path, its
// fingerprint and the sealed bytes.
func exportTestKit(t *testing.T, v *Vault) (*KitFingerprint, []byte) {
	t.Helper()
	var buf bytes.Buffer
	fingerprint, err := v.ExportKit(KitExportOptions{}, &buf)
	if err != nil {
		t.Fatalf("ExportKit: %v", err)
	}
	if fingerprint.Code == "" {
		t.Fatal("a kit sealed under a generated code came back without one")
	}
	return fingerprint, buf.Bytes()
}

// openTestKit is the import side of the same: pull kit.sand out of the zip and
// open it with the code.
func openTestKit(t *testing.T, zipped []byte, code string) *Kit {
	t.Helper()
	sealedKit, err := ReadKitZip(zipped)
	if err != nil {
		t.Fatalf("ReadKitZip: %v", err)
	}
	kit, err := OpenKit(sealedKit, code)
	if err != nil {
		t.Fatalf("OpenKit: %v", err)
	}
	return kit
}

// freshVault is the machine that replaces the one that died: same accounts on
// disk, no vault file at all.
func freshVault(t *testing.T) *Vault {
	t.Helper()
	v, err := Open(filepath.Join(t.TempDir(), "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)
	t.Cleanup(v.AwaitReadHistory)
	return v
}

// The test that matters: export, destroy the machine, import, and get
// everything back — clouds connected, tree intact, files readable.
func TestKitRoundTrip(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	for _, dir := range []string{"/papers", "/papers/2026"} {
		if err := v.Mkdir(MainScope, dir); err != nil {
			t.Fatalf("Mkdir %s: %v", dir, err)
		}
	}

	payload := []byte("the quick brown fox jumps over the lazy dog\n")
	entry, _, err := v.Upload(ctx, MainScope, "/papers", "notes.txt", payload, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	nested := []byte(strings.Repeat("deep\n", 400))
	if _, _, err := v.Upload(ctx, MainScope, "/papers/2026", "minutes.txt", nested, UploadOptions{}); err != nil {
		t.Fatalf("Upload nested: %v", err)
	}

	before, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}

	fingerprint, zipped := exportTestKit(t, v)
	if fingerprint.Files != 2 {
		t.Errorf("fingerprint says %d files, want 2", fingerprint.Files)
	}
	if fingerprint.Accounts != 3 {
		t.Errorf("fingerprint says %d accounts, want 3", fingerprint.Accounts)
	}
	if fingerprint.Secret != KitSecretCode {
		t.Errorf("Secret = %q, want %q", fingerprint.Secret, KitSecretCode)
	}

	// The machine dies. The clouds do not.
	v.AwaitBackupSync()
	v.Lock()

	restored := freshVault(t)
	kit := openTestKit(t, zipped, fingerprint.Code)

	const newPassword = "a different password entirely"
	report, err := restored.ImportKit(ctx, kit, KitImportOptions{Password: newPassword})
	if err != nil {
		t.Fatalf("ImportKit: %v", err)
	}

	if !report.Complete() {
		t.Errorf("report is not complete: %d lost, blocking %+v", report.Lost, report.Blocking)
	}
	if report.Files != 2 || report.Recoverable != 2 {
		t.Errorf("Files/Recoverable = %d/%d, want 2/2", report.Files, report.Recoverable)
	}
	for _, a := range report.Accounts {
		if a.Status != KitAccountConnected {
			t.Errorf("account %s came back %s: %s", a.Name, a.Status, a.Detail)
		}
	}

	// Every account back under the id it had — which is what leaves the shard
	// records correct without a single one being re-pointed.
	after, err := restored.Providers()
	if err != nil {
		t.Fatalf("Providers after import: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("%d accounts came back, want %d", len(after), len(before))
	}
	ids := map[string]provider.Config{}
	for _, cfg := range after {
		ids[cfg.ID] = cfg
	}
	for _, cfg := range before {
		got, ok := ids[cfg.ID]
		if !ok {
			t.Errorf("account %s (%s) did not come back under its own id", cfg.Name, cfg.ID)
			continue
		}
		if got.Name != cfg.Name || got.Kind != cfg.Kind {
			t.Errorf("account %s came back as %s/%s", cfg.ID, got.Name, got.Kind)
		}
	}
	if report.Repointed != 0 {
		t.Errorf("Repointed = %d, want 0 — a kit preserves account ids", report.Repointed)
	}

	// And the files actually read.
	data, fetched, err := restored.Fetch(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Fetch after import: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("file came back as %q, want %q", data, payload)
	}
	if fetched.Path() != "/papers/notes.txt" {
		t.Errorf("Path = %q, want /papers/notes.txt", fetched.Path())
	}

	// The new password is the one that opens it now.
	restored.Lock()
	if err := restored.Unlock(newPassword); err != nil {
		t.Fatalf("Unlock with the chosen password: %v", err)
	}
	if err := restored.Unlock(testPassword); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("the dead vault's password still opens the new one: %v", err)
	}
}

// A sub vault comes back present and shut, and its own password still opens it.
// This is the case no other recovery route handles cleanly: preserving account
// ids means the shard records inside the sealed section are correct without
// ever being touched, so no AccountRemap is needed.
func TestKitRoundTripCarriesSubVaults(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	const subPassword = "the sub vault has its own"
	sub, err := v.CreateSubVault("private", subPassword)
	if err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}
	secret := []byte("kept apart from everything else\n")
	if _, _, err := v.Upload(ctx, Scope(sub.ID), "/", "secret.txt", secret, UploadOptions{}); err != nil {
		t.Fatalf("Upload into the sub vault: %v", err)
	}

	fingerprint, zipped := exportTestKit(t, v)
	if fingerprint.SubVaults != 1 {
		t.Fatalf("fingerprint says %d sub vaults, want 1", fingerprint.SubVaults)
	}
	v.AwaitBackupSync()
	v.Lock()

	restored := freshVault(t)
	kit := openTestKit(t, zipped, fingerprint.Code)
	report, err := restored.ImportKit(ctx, kit, KitImportOptions{Password: "new password"})
	if err != nil {
		t.Fatalf("ImportKit: %v", err)
	}
	if report.SubVaults != 1 {
		t.Fatalf("report says %d sub vaults, want 1", report.SubVaults)
	}

	subs, err := restored.SubVaults()
	if err != nil {
		t.Fatalf("SubVaults: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("%d sub vaults came back, want 1", len(subs))
	}
	if subs[0].Unlocked {
		t.Error("a sub vault came back open — it should be as shut as it was")
	}

	// No remap: the ids never changed, so there is nothing to translate.
	restored.mu.RLock()
	remap := restored.manifest.AccountRemap
	restored.mu.RUnlock()
	if len(remap) != 0 {
		t.Errorf("AccountRemap = %v, want none — a kit preserves account ids", remap)
	}

	if err := restored.UnlockSubVault(subs[0].ID, subPassword); err != nil {
		t.Fatalf("UnlockSubVault with its own password: %v", err)
	}
	listing, err := restored.List(Scope(subs[0].ID), "/")
	if err != nil {
		t.Fatalf("List inside the recovered sub vault: %v", err)
	}
	if len(listing.Files) != 1 || listing.Files[0].Name != "secret.txt" {
		t.Fatalf("the sub vault came back holding %+v, want secret.txt", listing.Files)
	}
}

// An account that will not connect never stops the import. The tree still comes
// back, and the shortfall is counted and blamed on the right account.
func TestKitImportSurvivesAMissingAccount(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	payload := []byte("two of three parts rebuild this\n")
	if _, _, err := v.Upload(ctx, MainScope, "/", "notes.txt", payload, UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	fingerprint, zipped := exportTestKit(t, v)
	v.AwaitBackupSync()
	v.Lock()

	// Two of the three folders go with the machine. A 2-of-3 file needs both
	// survivors, so this is the boundary: one more loss and it is gone.
	if err := os.RemoveAll(roots[2]); err != nil {
		t.Fatalf("removing an account's folder: %v", err)
	}

	restored := freshVault(t)
	kit := openTestKit(t, zipped, fingerprint.Code)
	report, err := restored.ImportKit(ctx, kit, KitImportOptions{Password: "new password"})
	if err != nil {
		t.Fatalf("ImportKit: %v", err)
	}

	// The import completed, and the file is still readable from the two that
	// remain.
	if report.Files != 1 || report.Recoverable != 1 {
		t.Errorf("Files/Recoverable = %d/%d, want 1/1", report.Files, report.Recoverable)
	}

	gone := 0
	for _, a := range report.Accounts {
		if a.Status != KitAccountConnected {
			gone++
			if a.Status != KitAccountNeedsPath {
				t.Errorf("a missing folder came back as %q, want %q", a.Status, KitAccountNeedsPath)
			}
			if a.PathOption != "path" {
				t.Errorf("PathOption = %q, want \"path\" so a picker can be offered", a.PathOption)
			}
		}
	}
	if gone != 1 {
		t.Errorf("%d accounts failed, want 1", gone)
	}
}

// Two accounts gone takes the file with them, and the report has to say which
// accounts would bring it back.
func TestKitImportReportsWhatIsBlocking(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)

	if _, _, err := v.Upload(ctx, MainScope, "/", "notes.txt", []byte("gone\n"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	fingerprint, zipped := exportTestKit(t, v)
	v.AwaitBackupSync()
	v.Lock()

	for _, root := range roots[1:] {
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("removing an account's folder: %v", err)
		}
	}

	restored := freshVault(t)
	kit := openTestKit(t, zipped, fingerprint.Code)
	report, err := restored.ImportKit(ctx, kit, KitImportOptions{Password: "new password"})
	if err != nil {
		t.Fatalf("ImportKit: %v", err)
	}

	if report.Lost != 1 {
		t.Fatalf("Lost = %d, want 1", report.Lost)
	}
	if len(report.Missing) != 1 || report.Missing[0].Path != "/notes.txt" {
		t.Fatalf("Missing = %+v, want /notes.txt", report.Missing)
	}
	if len(report.Blocking) == 0 {
		t.Fatal("nothing was named as blocking, so the report says what is wrong and not what to do")
	}
	blocking := false
	for _, a := range report.Blocking {
		if a.Blocking {
			blocking = true
		}
	}
	if !blocking {
		t.Error("no account is marked blocking")
	}
}

// The old-kit case, and the reason the kit carries the vault key: files added
// after the export come back anyway, off the copy of the index on the clouds.
func TestKitImportPrefersTheNewerIndexOnTheClouds(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	if _, _, err := v.Upload(ctx, MainScope, "/", "before.txt", []byte("in the kit\n"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	fingerprint, zipped := exportTestKit(t, v)

	// Months pass. More files land, and every index change rewrites the copy
	// of manifest.sand on each account.
	after, _, err := v.Upload(ctx, MainScope, "/", "after.txt", []byte("added later\n"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload after the export: %v", err)
	}
	v.AwaitBackupSync()
	v.Lock()

	restored := freshVault(t)
	kit := openTestKit(t, zipped, fingerprint.Code)
	report, err := restored.ImportKit(ctx, kit, KitImportOptions{Password: "new password"})
	if err != nil {
		t.Fatalf("ImportKit: %v", err)
	}

	if report.IndexSource == "kit" {
		t.Fatalf("the kit's own index was used, so the newer copy on the clouds was not found")
	}
	if report.Files != 2 {
		t.Errorf("Files = %d, want 2 — the file added after the export should be in the tree", report.Files)
	}

	data, _, err := restored.Fetch(ctx, after.ID)
	if err != nil {
		t.Fatalf("the file added after the export does not read back: %v", err)
	}
	if string(data) != "added later\n" {
		t.Errorf("got %q", data)
	}
}

// Skipping the cloud index is a real option, and it has to leave a usable
// vault: the kit's own tree, every account connected.
func TestKitImportCanSkipTheCloudIndex(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	if _, _, err := v.Upload(ctx, MainScope, "/", "before.txt", []byte("in the kit\n"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	fingerprint, zipped := exportTestKit(t, v)
	if _, _, err := v.Upload(ctx, MainScope, "/", "after.txt", []byte("added later\n"), UploadOptions{}); err != nil {
		t.Fatalf("Upload after the export: %v", err)
	}
	v.AwaitBackupSync()
	v.Lock()

	restored := freshVault(t)
	kit := openTestKit(t, zipped, fingerprint.Code)
	report, err := restored.ImportKit(ctx, kit, KitImportOptions{
		Password:       "new password",
		SkipCloudIndex: true,
	})
	if err != nil {
		t.Fatalf("ImportKit: %v", err)
	}
	if report.IndexSource != "kit" {
		t.Errorf("IndexSource = %q, want \"kit\"", report.IndexSource)
	}
	if report.Files != 1 {
		t.Errorf("Files = %d, want 1 — only what the kit described", report.Files)
	}
}

// The password chosen for the recovered vault is genuinely free — it changes
// what unlocks the machine and nothing else. In particular it does not decide
// which index the import lands on: the copies on the clouds open under the key
// the kit carries, whatever is typed here.
//
// This is the claim the import screen makes under its password field, so it is
// worth a test rather than a comment.
func TestKitImportChoiceOfPasswordDoesNotChangeWhatComesBack(t *testing.T) {
	ctx := context.Background()

	// Each case needs its own clouds: a finished import rewrites manifest.sand
	// on every account under the password it was given, so two cases sharing a
	// set of accounts would measure the first one's push.
	run := func(password string) *KitImportReport {
		t.Helper()
		v, _ := newTestVault(t, 3)
		if _, _, err := v.Upload(ctx, MainScope, "/", "before.txt", []byte("in the kit\n"), UploadOptions{}); err != nil {
			t.Fatalf("Upload: %v", err)
		}
		fingerprint, zipped := exportTestKit(t, v)
		if _, _, err := v.Upload(ctx, MainScope, "/", "after.txt", []byte("added later\n"), UploadOptions{}); err != nil {
			t.Fatalf("Upload after the export: %v", err)
		}
		v.AwaitBackupSync()
		v.Lock()

		restored := freshVault(t)
		report, err := restored.ImportKit(ctx, openTestKit(t, zipped, fingerprint.Code),
			KitImportOptions{Password: password})
		if err != nil {
			t.Fatalf("ImportKit(%q): %v", password, err)
		}
		return report
	}

	for _, password := range []string{"a password never used before", testPassword} {
		report := run(password)
		if report.PasswordChanged {
			t.Errorf("password %q: PasswordChanged is set — the kit's key should have opened the clouds", password)
		}
		if report.IndexSource == "kit" {
			t.Errorf("password %q: the kit's own index was used rather than the newer one on the clouds", password)
		}
		if report.Files != 2 || report.Recoverable != 2 {
			t.Errorf("password %q: Files/Recoverable = %d/%d, want 2/2", password, report.Files, report.Recoverable)
		}
	}
}

// A password change made after the kit was exported retires the key the kit
// carries, so the newer copies of the index on the accounts stop opening — and
// then the old password is the only thing that opens them.
//
// Choosing that same password for the recovered vault does *not* stand in for
// it: the new vault gets a salt of its own, so the same password derives a
// different key. It has to be handed over as OldPassword.
func TestKitImportNeedsTheOldPasswordAfterAPasswordChange(t *testing.T) {
	ctx := context.Background()
	const wasUsingAtDeath = "the password set after the kit was made"

	// One file in the kit, one added after it, one added after the password
	// change: three at death, and only the kit's one without the old password.
	setup := func() (*KitFingerprint, []byte) {
		t.Helper()
		v, _ := newTestVault(t, 3)
		if _, _, err := v.Upload(ctx, MainScope, "/", "before.txt", []byte("in the kit\n"), UploadOptions{}); err != nil {
			t.Fatalf("Upload: %v", err)
		}
		fingerprint, zipped := exportTestKit(t, v)
		if _, _, err := v.Upload(ctx, MainScope, "/", "after.txt", []byte("added later\n"), UploadOptions{}); err != nil {
			t.Fatalf("Upload after the export: %v", err)
		}
		if _, err := v.ChangePassword(ctx, testPassword, wasUsingAtDeath, false); err != nil {
			t.Fatalf("ChangePassword: %v", err)
		}
		if _, _, err := v.Upload(ctx, MainScope, "/", "after-change.txt", []byte("after the change\n"), UploadOptions{}); err != nil {
			t.Fatalf("Upload after the change: %v", err)
		}
		v.AwaitBackupSync()
		v.Lock()
		return fingerprint, zipped
	}

	run := func(opts KitImportOptions) *KitImportReport {
		t.Helper()
		fingerprint, zipped := setup()
		restored := freshVault(t)
		report, err := restored.ImportKit(ctx, openTestKit(t, zipped, fingerprint.Code), opts)
		if err != nil {
			t.Fatalf("ImportKit: %v", err)
		}
		return report
	}

	// Without it: the clouds refuse the kit's key uniformly, which is reported
	// rather than guessed at, and the kit's own index is what is left.
	without := run(KitImportOptions{Password: "a password never used before"})
	if !without.PasswordChanged {
		t.Error("PasswordChanged is not set, so the uniform refusal was not noticed")
	}
	if without.IndexSource != "kit" || without.Files != 1 {
		t.Errorf("IndexSource/Files = %q/%d, want \"kit\"/1", without.IndexSource, without.Files)
	}

	// Choosing the old password for the recovered vault is not the same thing
	// and must not be mistaken for it — the new vault's salt is its own.
	reused := run(KitImportOptions{Password: wasUsingAtDeath})
	if reused.IndexSource != "kit" || reused.Files != 1 {
		t.Errorf("reusing the old password as the new one changed the outcome: "+
			"IndexSource/Files = %q/%d, want \"kit\"/1", reused.IndexSource, reused.Files)
	}

	// With it: the whole tree, including what landed after the change.
	with := run(KitImportOptions{Password: "a password never used before", OldPassword: wasUsingAtDeath})
	if !with.PasswordChanged {
		t.Error("PasswordChanged should still say what was observed on the accounts")
	}
	if with.IndexSource == "kit" {
		t.Fatal("the old password did not open the copies of the index on the accounts")
	}
	if with.Files != 3 || with.Recoverable != 3 {
		t.Errorf("Files/Recoverable = %d/%d, want 3/3", with.Files, with.Recoverable)
	}

	// A wrong old password is a warning, not a failure: the import still lands
	// on the kit's index rather than refusing to run.
	wrong := run(KitImportOptions{Password: "a password never used before", OldPassword: testPassword})
	if wrong.IndexSource != "kit" {
		t.Errorf("IndexSource = %q, want \"kit\"", wrong.IndexSource)
	}
	if len(wrong.Warnings) == 0 {
		t.Error("a password that opened nothing was not mentioned in the report")
	}
}

// A kit read back must open with its code and refuse everything else, and each
// refusal has to be its own distinguishable answer.
func TestOpenKitRejections(t *testing.T) {
	v, _ := newTestVault(t, 3)
	fingerprint, zipped := exportTestKit(t, v)
	sealedKit, err := ReadKitZip(zipped)
	if err != nil {
		t.Fatalf("ReadKitZip: %v", err)
	}

	t.Run("a typo is caught before the KDF runs", func(t *testing.T) {
		bare := strings.ReplaceAll(fingerprint.Code, "-", "")
		swapped := bare[:3] + string(bare[4]) + string(bare[3]) + bare[5:]
		if swapped == bare {
			t.Skip("this code has no distinct adjacent pair in the first group")
		}
		if _, err := OpenKit(sealedKit, swapped); !errors.Is(err, ErrKitCodeTypo) {
			t.Fatalf("got %v, want ErrKitCodeTypo", err)
		}
	})

	t.Run("a well-formed code for another kit", func(t *testing.T) {
		other, err := NewKitCode()
		if err != nil {
			t.Fatalf("NewKitCode: %v", err)
		}
		_, err = OpenKit(sealedKit, other)
		if err == nil {
			t.Fatal("another kit's code opened this one")
		}
		if errors.Is(err, ErrKitCodeTypo) {
			t.Fatalf("a valid code was reported as a typo: %v", err)
		}
		// It names the kit, because the person is probably holding two.
		if !strings.Contains(err.Error(), shortKitID(fingerprint.KitID)) {
			t.Errorf("error does not name the kit: %v", err)
		}
	})

	t.Run("a damaged payload", func(t *testing.T) {
		var env KitEnvelope
		if err := jsonUnmarshal(sealedKit, &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		env.Payload.Ciphertext = "AAAA" + env.Payload.Ciphertext[4:]
		damaged, err := jsonMarshal(&env)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := OpenKit(damaged, fingerprint.Code); !errors.Is(err, ErrKitDamaged) {
			t.Fatalf("got %v, want ErrKitDamaged", err)
		}
	})

	t.Run("a kit from a later build", func(t *testing.T) {
		var env KitEnvelope
		if err := jsonUnmarshal(sealedKit, &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		env.Version = KitVersion + 1
		future, err := jsonMarshal(&env)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		_, err = OpenKit(future, fingerprint.Code)
		if err == nil || !strings.Contains(err.Error(), "later version") {
			t.Fatalf("got %v, want a version refusal", err)
		}
	})

	t.Run("not a kit at all", func(t *testing.T) {
		if _, err := OpenKit([]byte("hello"), fingerprint.Code); !errors.Is(err, ErrNotAKit) {
			t.Fatalf("got %v, want ErrNotAKit", err)
		}
	})
}

// The opt-out: a kit sealed under the vault password says so, retains no code,
// and opens with the password.
func TestKitSealedUnderTheVaultPassword(t *testing.T) {
	v, _ := newTestVault(t, 3)

	var buf bytes.Buffer
	fingerprint, err := v.ExportKit(KitExportOptions{
		UseVaultPassword: true,
		Password:         testPassword,
	}, &buf)
	if err != nil {
		t.Fatalf("ExportKit: %v", err)
	}
	if fingerprint.Secret != KitSecretPassword {
		t.Errorf("Secret = %q, want %q", fingerprint.Secret, KitSecretPassword)
	}
	if fingerprint.Code != "" {
		t.Error("a password-sealed kit came back with a generated code")
	}

	code, err := v.KitCode(fingerprint.KitID)
	if err != nil {
		t.Fatalf("KitCode: %v", err)
	}
	if code != "" {
		t.Error("a password-sealed kit retained a code")
	}

	sealedKit, err := ReadKitZip(buf.Bytes())
	if err != nil {
		t.Fatalf("ReadKitZip: %v", err)
	}
	if _, err := OpenKit(sealedKit, testPassword); err != nil {
		t.Fatalf("the vault password does not open its own kit: %v", err)
	}
	if _, err := OpenKit(sealedKit, "not the password"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("got %v, want ErrWrongPassword", err)
	}

	env, err := InspectKit(sealedKit)
	if err != nil {
		t.Fatalf("InspectKit: %v", err)
	}
	if env.Secret != KitSecretPassword {
		t.Errorf("the envelope says %q, so the import would prompt for the wrong thing", env.Secret)
	}

	// And the wrong vault password is refused before a kit is ever built.
	if _, err := v.ExportKit(KitExportOptions{UseVaultPassword: true, Password: "wrong"}, &buf); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("got %v, want ErrWrongPassword", err)
	}
}

// A vault that still works can show the code again, which is the whole answer
// to a mislaid slip of paper.
func TestKitCodeIsRetainedAndForgettable(t *testing.T) {
	v, _ := newTestVault(t, 3)
	fingerprint, _ := exportTestKit(t, v)

	got, err := v.KitCode(fingerprint.KitID)
	if err != nil {
		t.Fatalf("KitCode: %v", err)
	}
	if got != fingerprint.Code {
		t.Fatalf("KitCode = %q, want %q", got, fingerprint.Code)
	}

	// It survives a lock and unlock, because it is in the sealed settings
	// section rather than in memory.
	v.AwaitBackupSync()
	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if got, _ := v.KitCode(fingerprint.KitID); got != fingerprint.Code {
		t.Fatalf("after an unlock KitCode = %q, want %q", got, fingerprint.Code)
	}

	status, err := v.KitStatus()
	if err != nil {
		t.Fatalf("KitStatus: %v", err)
	}
	if !status.Exported || !status.CodeRetained {
		t.Errorf("status = %+v, want an exported kit with its code retained", status)
	}

	if err := v.ForgetKitCode(fingerprint.KitID); err != nil {
		t.Fatalf("ForgetKitCode: %v", err)
	}
	if got, _ := v.KitCode(fingerprint.KitID); got != "" {
		t.Errorf("the code survived being forgotten: %q", got)
	}
}

// Nothing readable may escape into the archive: not a credential, not a
// filename, not an account name, and above all not the code.
func TestKitZipLeaksNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	v, err := Open(filepath.Join(dir, "vault.sand"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := v.Init(testPassword, PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(v.AwaitBackupSync)
	t.Cleanup(v.AwaitReadHistory)

	const (
		secretKey     = "SUPERSECRETACCESSKEY123"
		accountName   = "zzz-account-with-a-memorable-name"
		secretFileTag = "an-unmistakable-filename"
	)
	for i := 0; i < 3; i++ {
		root := filepath.Join(dir, "cloud", string(rune('a'+i)))
		_, err := v.AddProvider(ctx, provider.Config{
			Kind: provider.KindLocal,
			Name: accountName + "-" + string(rune('a'+i)),
			Options: map[string]string{
				"path": root,
				// Not a real option for this backend, but it rides in Options
				// like a credential would and must not survive into the clear.
				"secret_probe": secretKey,
			},
		})
		if err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}
	if _, _, err := v.Upload(ctx, MainScope, "/", secretFileTag+".txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	fingerprint, zipped := exportTestKit(t, v)

	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatalf("reading the kit as a zip: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true

		rc, err := f.Open()
		if err != nil {
			t.Fatalf("opening %s: %v", f.Name, err)
		}
		body, err := readAllLimited(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", f.Name, err)
		}

		for _, secret := range []struct{ what, value string }{
			{"a credential", secretKey},
			{"an account name", accountName},
			{"a filename", secretFileTag},
			{"the recovery code", strings.ReplaceAll(fingerprint.Code, "-", "")},
			{"the recovery code", fingerprint.Code},
		} {
			if bytes.Contains(body, []byte(secret.value)) {
				t.Errorf("%s is readable in %s: %q", secret.what, f.Name, secret.value)
			}
		}
	}

	for _, want := range []string{KitFile, KitManifestFile, KitFingerprintFile, KitReadmeFile} {
		if !names[want] {
			t.Errorf("the kit is missing %s", want)
		}
	}
}

// The manifest copy in the zip is the offline path, and it has to be the same
// file the accounts carry — openable by the vault password with no vault, no
// network and no kit code.
func TestKitCarriesAnOpenableManifest(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	if _, _, err := v.Upload(ctx, MainScope, "/", "notes.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	_, zipped := exportTestKit(t, v)

	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatalf("reading the kit as a zip: %v", err)
	}
	var manifestBlob []byte
	for _, f := range zr.File {
		if f.Name != KitManifestFile {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("opening %s: %v", f.Name, err)
		}
		manifestBlob, err = readAllLimited(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", f.Name, err)
		}
	}
	if manifestBlob == nil {
		t.Fatalf("the kit carries no %s", KitManifestFile)
	}

	snapshot, err := OpenBackup(manifestBlob, testPassword)
	if err != nil {
		t.Fatalf("the manifest in the kit does not open with the vault password: %v", err)
	}
	if len(snapshot.Manifest.Entries) != 1 {
		t.Errorf("the manifest describes %d files, want 1", len(snapshot.Manifest.Entries))
	}
}

// Importing over a live vault is refused, on Recover's terms and for its
// reason.
func TestKitImportRefusesToOverwriteALiveVault(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	fingerprint, zipped := exportTestKit(t, v)

	other, _ := newTestVault(t, 3)
	if _, _, err := other.Upload(ctx, MainScope, "/", "mine.txt", []byte("already here\n"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	kit := openTestKit(t, zipped, fingerprint.Code)
	_, err := other.ImportKit(ctx, kit, KitImportOptions{Password: "new password"})
	if err == nil {
		t.Fatal("a kit was imported over a vault holding files")
	}
	if !strings.Contains(err.Error(), "already holds") {
		t.Errorf("got %v, want a refusal naming what is already here", err)
	}

	// And with Replace it goes through.
	if _, err := other.ImportKit(ctx, kit, KitImportOptions{Password: "new password", Replace: true}); err != nil {
		t.Fatalf("ImportKit with Replace: %v", err)
	}
}

// A locked vault is still a vault. Not knowing what it holds is a reason to
// refuse rather than to proceed: importing over it would write a whole new
// store, and whatever it held would be gone with nothing left to say what.
func TestKitImportRefusesToOverwriteALockedVault(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	fingerprint, zipped := exportTestKit(t, v)

	other, _ := newTestVault(t, 3)
	if _, _, err := other.Upload(ctx, MainScope, "/", "mine.txt", []byte("already here\n"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	other.AwaitBackupSync()
	other.Lock()

	kit := openTestKit(t, zipped, fingerprint.Code)
	_, err := other.ImportKit(ctx, kit, KitImportOptions{Password: "new password"})
	if err == nil {
		t.Fatal("a kit was imported over a locked vault that held files")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Errorf("got %v, want a refusal that says the vault is locked", err)
	}

	// And the vault it refused to touch still opens, with its file intact.
	if err := other.Unlock(testPassword); err != nil {
		t.Fatalf("the vault it refused to overwrite no longer opens: %v", err)
	}
	listing, err := other.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Files) != 1 || listing.Files[0].Name != "mine.txt" {
		t.Fatalf("the vault came back holding %+v, want mine.txt", listing.Files)
	}
}

// A backend's "folder" is only a folder on this machine when the backend says
// so. Box and OneDrive both call theirs "folder" and both mean a folder inside
// somebody else's service, defaulting to "sand" — treating that as a local path
// reports a healthy account as needing one, and then offers a button that would
// write a local path into a remote setting.
func TestConfiguredPathOnlyMatchesLocalFolders(t *testing.T) {
	local := provider.Config{
		Kind:    provider.KindLocal,
		Options: map[string]string{"path": "/mnt/backup/sand"},
	}
	if key, root := configuredPath(local); key != "path" || root != "/mnt/backup/sand" {
		t.Errorf("local: got %q/%q, want path and /mnt/backup/sand", key, root)
	}

	for _, kind := range []provider.Kind{provider.KindBox, provider.KindOneDrive} {
		spec, ok := provider.SpecFor(kind)
		if !ok {
			t.Fatalf("no spec for %s", kind)
		}
		cfg := provider.Config{Kind: kind, Options: map[string]string{}}
		for _, f := range spec.Fields {
			if f.Key == "folder" {
				cfg.Options["folder"] = "sand"
			}
		}
		if cfg.Options["folder"] == "" {
			t.Fatalf("%s has no folder field to test against", kind)
		}
		if key, root := configuredPath(cfg); key != "" {
			t.Errorf("%s: a remote folder was read as a local path (%q = %q)", kind, key, root)
		}
	}
}

// Which door a failed account opens is a fact about the backend, not about the
// failure. A revoked Dropbox consent and a rotated S3 key both come back as an
// authentication failure, and the repairs share no steps.
func TestRepairForAsksTheBackend(t *testing.T) {
	signIn := []provider.Kind{provider.KindGDrive, provider.KindDropbox, provider.KindOneDrive, provider.KindBox}
	for _, kind := range signIn {
		spec, ok := provider.SpecFor(kind)
		if !ok {
			t.Fatalf("no spec for %s", kind)
		}
		if spec.OAuth == nil {
			t.Fatalf("%s is expected to be an OAuth backend", kind)
		}
		cfg := provider.Config{Kind: kind}
		if got := repairFor(cfg, KitAccountNeedsReauth); got != KitRepairSignIn {
			t.Errorf("%s: repair = %q, want %q", kind, got, KitRepairSignIn)
		}
	}

	// No consent screen to send anybody to: what these want is the key typed
	// again, which is a form rather than a round trip.
	for _, kind := range []provider.Kind{provider.KindS3, provider.KindWebDAV} {
		if spec, ok := provider.SpecFor(kind); ok && spec.OAuth != nil {
			t.Fatalf("%s unexpectedly signs in", kind)
		}
		cfg := provider.Config{Kind: kind}
		if got := repairFor(cfg, KitAccountNeedsReauth); got != KitRepairSettings {
			t.Errorf("%s: repair = %q, want %q", kind, got, KitRepairSettings)
		}
	}

	local := provider.Config{Kind: provider.KindLocal}
	for _, tc := range []struct{ status, want string }{
		{KitAccountConnected, ""},
		{KitAccountNeedsPath, KitRepairPath},
		{KitAccountUnreachable, KitRepairRetry},
	} {
		if got := repairFor(local, tc.status); got != tc.want {
			t.Errorf("status %q: repair = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// An import has to say which door to open, not merely that something is wrong.
func TestKitImportReportsHowToRepairEachAccount(t *testing.T) {
	ctx := context.Background()
	v, roots := newTestVault(t, 3)
	if _, _, err := v.Upload(ctx, MainScope, "/", "notes.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	fingerprint, zipped := exportTestKit(t, v)
	v.AwaitBackupSync()
	v.Lock()

	if err := os.RemoveAll(roots[2]); err != nil {
		t.Fatalf("removing an account's folder: %v", err)
	}

	restored := freshVault(t)
	kit := openTestKit(t, zipped, fingerprint.Code)
	report, err := restored.ImportKit(ctx, kit, KitImportOptions{Password: "new password"})
	if err != nil {
		t.Fatalf("ImportKit: %v", err)
	}

	for _, a := range report.Accounts {
		switch a.Status {
		case KitAccountConnected:
			if a.Repair != "" {
				t.Errorf("%s connected but names a repair (%q)", a.Name, a.Repair)
			}
		case KitAccountNeedsPath:
			if a.Repair != KitRepairPath {
				t.Errorf("%s: repair = %q, want %q", a.Name, a.Repair, KitRepairPath)
			}
		default:
			if a.Repair == "" {
				t.Errorf("%s failed as %q and names no repair", a.Name, a.Status)
			}
		}
	}
}

// Swapping one account for another leaves the count alone and makes the kit
// strictly less able to help, so it has to read as a change.
func TestKitStatusNoticesASwappedAccount(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	exportTestKit(t, v)

	accounts, err := v.Providers()
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	if err := v.RemoveProvider(accounts[0].ID, true); err != nil {
		t.Fatalf("RemoveProvider: %v", err)
	}
	if _, err := v.AddProvider(ctx, provider.Config{
		Kind: provider.KindLocal, Name: "swapped-in",
		Options: map[string]string{"path": filepath.Join(t.TempDir(), "swapped")},
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	status, err := v.KitStatus()
	if err != nil {
		t.Fatalf("KitStatus: %v", err)
	}
	if status.Accounts != 3 {
		t.Fatalf("Accounts = %d, want 3 — the swap has to leave the count alone", status.Accounts)
	}
	if !status.AccountsChanged {
		t.Error("swapping an account reads as no change, so the kit looks current when it is not")
	}
}

// A password-sealed kit must not be reported as wanting a code — the fire drill
// labels its field from this.
func TestVerifyKitReportsTheSecretItWasSealedUnder(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	var buf bytes.Buffer
	if _, err := v.ExportKit(KitExportOptions{
		UseVaultPassword: true, Password: testPassword,
	}, &buf); err != nil {
		t.Fatalf("ExportKit: %v", err)
	}
	sealedKit, err := ReadKitZip(buf.Bytes())
	if err != nil {
		t.Fatalf("ReadKitZip: %v", err)
	}
	kit, err := OpenKit(sealedKit, testPassword)
	if err != nil {
		t.Fatalf("OpenKit: %v", err)
	}
	if kit.SecretKind != KitSecretPassword {
		t.Fatalf("SecretKind = %q, want %q", kit.SecretKind, KitSecretPassword)
	}

	report, err := v.VerifyKit(ctx, kit)
	if err != nil {
		t.Fatalf("VerifyKit: %v", err)
	}
	if report.Secret != KitSecretPassword {
		t.Errorf("Secret = %q, want %q", report.Secret, KitSecretPassword)
	}
}

// The kit must never be written into a folder one of this vault's own accounts
// is syncing: it would be uploaded to the very cloud whose credentials it
// carries.
func TestKitRefusesToBeWrittenIntoASyncedFolder(t *testing.T) {
	v, roots := newTestVault(t, 3)

	inside := filepath.Join(roots[0], "my-recovery-kit.zip")
	if _, err := v.WriteKitTo(inside, KitExportOptions{}); !errors.Is(err, ErrKitInSyncedFolder) {
		t.Fatalf("got %v, want ErrKitInSyncedFolder", err)
	}
	if _, err := os.Stat(inside); err == nil {
		t.Error("a refused kit was written anyway")
	}

	outside := filepath.Join(t.TempDir(), "my-recovery-kit.zip")
	if _, err := v.WriteKitTo(outside, KitExportOptions{}); err != nil {
		t.Fatalf("WriteKitTo somewhere safe: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the kit was not written: %v", err)
	}
}

// The read counters resume from where they stopped rather than from zero.
func TestKitRestoresTheReadHistory(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	entry, _, err := v.Upload(ctx, MainScope, "/", "notes.txt", []byte("read me\n"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := v.Fetch(ctx, entry.ID); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	}
	v.AwaitReadHistory()

	before := v.ReadStats(WindowAll)
	if before.Races == 0 {
		t.Skip("nothing was counted, so there is nothing to carry")
	}

	fingerprint, zipped := exportTestKit(t, v)
	v.AwaitBackupSync()
	v.Lock()

	restored := freshVault(t)
	kit := openTestKit(t, zipped, fingerprint.Code)
	if _, err := restored.ImportKit(ctx, kit, KitImportOptions{Password: "new password"}); err != nil {
		t.Fatalf("ImportKit: %v", err)
	}

	after := restored.ReadStats(WindowAll)
	if after.Races < before.Races {
		t.Errorf("read history came back with %d races, want at least %d",
			after.Races, before.Races)
	}
}

// The fire drill proves the carried credentials, and names the accounts a kit
// this old could not restore.
func TestVerifyKit(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	if _, _, err := v.Upload(ctx, MainScope, "/", "notes.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	fingerprint, zipped := exportTestKit(t, v)
	kit := openTestKit(t, zipped, fingerprint.Code)

	report, err := v.VerifyKit(ctx, kit)
	if err != nil {
		t.Fatalf("VerifyKit: %v", err)
	}
	if report.Working != 3 || report.Unusable != 0 {
		t.Errorf("Working/Unusable = %d/%d, want 3/0", report.Working, report.Unusable)
	}
	if report.Recoverable != 1 {
		t.Errorf("Recoverable = %d, want 1", report.Recoverable)
	}

	// An account connected after the kit is the honest ceiling of an old kit.
	extra := filepath.Join(t.TempDir(), "later")
	if _, err := v.AddProvider(ctx, provider.Config{
		Kind: provider.KindLocal, Name: "connected-later",
		Options: map[string]string{"path": extra},
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	report, err = v.VerifyKit(ctx, kit)
	if err != nil {
		t.Fatalf("VerifyKit: %v", err)
	}
	if len(report.AccountsAdded) != 1 || report.AccountsAdded[0].Name != "connected-later" {
		t.Errorf("AccountsAdded = %+v, want the account connected after the kit", report.AccountsAdded)
	}

	// A credential in the kit that has stopped working shows up as unusable,
	// and the drill still completes — which is the whole point of running one.
	// Mutated on the opened kit rather than by deleting the live folder: the
	// vault is still running, and its backup syncer would put the folder back
	// underneath the test.
	kit.Accounts[0].Options["path"] = filepath.Join(t.TempDir(), "gone")
	report, err = v.VerifyKit(ctx, kit)
	if err != nil {
		t.Fatalf("VerifyKit after a loss: %v", err)
	}
	if report.Unusable != 1 {
		t.Errorf("Unusable = %d, want 1", report.Unusable)
	}
	if report.Accounts[0].Status != KitAccountNeedsPath {
		t.Errorf("a vanished folder came back as %q, want %q",
			report.Accounts[0].Status, KitAccountNeedsPath)
	}
}

// The staleness the settings panel draws its nudge from.
func TestKitStatusTracksDrift(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)

	status, err := v.KitStatus()
	if err != nil {
		t.Fatalf("KitStatus: %v", err)
	}
	if status.Exported {
		t.Fatal("a vault that has never exported a kit says it has")
	}

	if _, _, err := v.Upload(ctx, MainScope, "/", "one.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	fingerprint, _ := exportTestKit(t, v)

	if status, err = v.KitStatus(); err != nil {
		t.Fatalf("KitStatus: %v", err)
	}
	if !status.Exported || status.KitID != fingerprint.KitID {
		t.Fatalf("status = %+v, want the kit just exported", status)
	}
	if status.FilesAdded != 0 || status.AccountsChanged {
		t.Errorf("a kit exported a moment ago reads as stale: %+v", status)
	}

	if _, _, err := v.Upload(ctx, MainScope, "/", "two.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if status, err = v.KitStatus(); err != nil {
		t.Fatalf("KitStatus: %v", err)
	}
	if status.FilesAdded != 1 {
		t.Errorf("FilesAdded = %d, want 1", status.FilesAdded)
	}

	extra := filepath.Join(t.TempDir(), "later")
	if _, err := v.AddProvider(ctx, provider.Config{
		Kind: provider.KindLocal, Name: "connected-later",
		Options: map[string]string{"path": extra},
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if status, err = v.KitStatus(); err != nil {
		t.Fatalf("KitStatus: %v", err)
	}
	if !status.AccountsChanged {
		t.Error("connecting an account after the kit does not show as a change")
	}
}
