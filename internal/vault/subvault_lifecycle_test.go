package vault

import (
	"errors"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/movie"
)

// A file assigned into a sub vault stays on the main vault's key generation
// until it is migrated, so that generation has to survive — in the main vault —
// for as long as the sub vault's index names it. A main password change
// migrates the main vault's own files and then prunes the keys nothing names,
// and "nothing" must include what the sub vaults still point at.
func TestMainKeyBorrowedByASubVaultSurvivesAPasswordChange(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	// One file that stays in the main vault, so the old generation is retained
	// at the rotation for its sake — the sub vault's claim on it has to be what
	// keeps it alive after this file has migrated off it.
	if _, _, err := v.Upload(t.Context(), MainScope, "/", "stays.txt", []byte("ordinary"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := v.Upload(t.Context(), MainScope, "/", "moved.txt", []byte("assigned in"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Assign(t.Context(), MainScope, "/moved.txt", Scope(id), false); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	// The password change migrates every main file onto the fresh key. The old
	// generation is then named by nothing in the main index — only by the file
	// sitting in the sub vault.
	const newMain = "an entirely new main password"
	if _, err := v.ChangePassword(t.Context(), testPassword, newMain, true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	entry, err := v.EntryByPath(Scope(id), "/moved.txt")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	data, _, err := v.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch of the assigned file after the main password change: %v", err)
	}
	if string(data) != "assigned in" {
		t.Errorf("content = %q, want %q", data, "assigned in")
	}
}

// The same borrowing, with the sub vault locked while the password changes.
// The main vault cannot read the locked index, so it has to have remembered
// that the sub vault still points at one of its generations.
func TestMainKeyBorrowedByALockedSubVaultSurvivesAPasswordChange(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), MainScope, "/", "moved.txt", []byte("assigned in"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Assign(t.Context(), MainScope, "/moved.txt", Scope(id), false); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := v.LockSubVault(id); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	// No file in the main index names the old generation now — the one that
	// did is inside a locked section.
	const newMain = "an entirely new main password"
	if _, err := v.ChangePassword(t.Context(), testPassword, newMain, true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	if err := v.UnlockSubVault(id, subPassword); err != nil {
		t.Fatalf("UnlockSubVault: %v", err)
	}
	entry, err := v.EntryByPath(Scope(id), "/moved.txt")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if _, _, err := v.Fetch(t.Context(), entry.ID); err != nil {
		t.Fatalf("Fetch of the assigned file after the main password change: %v", err)
	}
	// And the deferred re-encryption can still finish.
	report, err := v.MigrateFilesIn(t.Context(), Scope(id), nil, nil)
	if err != nil {
		t.Fatalf("MigrateFilesIn: %v", err)
	}
	if report.Remaining != 0 {
		t.Errorf("%d file(s) still un-migrated: %v", report.Remaining, report.Warnings)
	}
}

// A sub vault's retired key stays in the sub vault's record for as long as a
// file assigned out of it still answers to that generation — pruning it the
// moment no *sub* entry names it erases the only copy while the main index
// still points at it.
func TestSubKeyLentToTheMainVaultSurvivesTheSubsOwnPruning(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "receipt.pdf", []byte("a receipt"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// The sub vault's password changes without migrating, so the file is left
	// on a *retired* generation of the sub vault's own.
	if _, err := v.ChangeSubVaultPassword(t.Context(), id, subPassword, "a fresh sub password", false); err != nil {
		t.Fatalf("ChangeSubVaultPassword: %v", err)
	}

	// Assigned out while still on that retired generation. The write behind
	// the assignment re-seals the sub vault, and its pruning must notice the
	// main vault now holds the only reference.
	if _, err := v.Assign(t.Context(), Scope(id), "/receipt.pdf", MainScope, false); err != nil {
		t.Fatalf("Assign out: %v", err)
	}

	entry, err := v.EntryByPath(MainScope, "/receipt.pdf")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	// Both vaults are open, which is the state the assignment's contract says
	// the file is readable in.
	data, _, err := v.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch of an assigned-out file with both vaults open: %v", err)
	}
	if string(data) != "a receipt" {
		t.Errorf("content = %q, want %q", data, "a receipt")
	}
}

// The mirror of the retention above: a sub vault's password change must carry
// its old key for the sake of a file assigned out and not yet migrated.
func TestSubPasswordChangeKeepsAKeyTheMainVaultStillNeeds(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "receipt.pdf", []byte("a receipt"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Assign(t.Context(), Scope(id), "/receipt.pdf", MainScope, false); err != nil {
		t.Fatalf("Assign out: %v", err)
	}

	// The sub vault is empty now, so nothing in its own index names its active
	// generation — the assigned-out file in the main vault does.
	if _, err := v.ChangeSubVaultPassword(t.Context(), id, subPassword, "a fresh sub password", true); err != nil {
		t.Fatalf("ChangeSubVaultPassword: %v", err)
	}

	entry, err := v.EntryByPath(MainScope, "/receipt.pdf")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	data, _, err := v.Fetch(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("Fetch of an assigned-out file after the sub vault's password change: %v", err)
	}
	if string(data) != "a receipt" {
		t.Errorf("content = %q, want %q", data, "a receipt")
	}
}

// A sub vault's password can change while a file assigned *into* it has not
// migrated yet. That file is on the main vault's key, which this change
// neither holds nor needs — it is skipped, not an error.
func TestSubPasswordChangeWithAnUnmigratedAssignmentIn(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), MainScope, "/", "moved.txt", []byte("assigned in"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Assign(t.Context(), MainScope, "/moved.txt", Scope(id), false); err != nil {
		t.Fatalf("Assign in: %v", err)
	}

	report, err := v.ChangeSubVaultPassword(t.Context(), id, subPassword, "a fresh sub password", true)
	if err != nil {
		t.Fatalf("ChangeSubVaultPassword with an assigned-in file pending: %v", err)
	}
	if report.Remaining != 0 {
		t.Errorf("%d file(s) still un-migrated: %v", report.Remaining, report.Warnings)
	}

	entry, err := v.EntryByPath(Scope(id), "/moved.txt")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if _, _, err := v.Fetch(t.Context(), entry.ID); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

// Deleting a sub vault whose key still seals a file assigned out of it says
// so, rather than erasing the only copy of the key in silence.
func TestDeleteSubVaultWarnsAboutFilesItStillSeals(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Taxes", subPassword)

	if _, _, err := v.Upload(t.Context(), Scope(id), "/", "receipt.pdf", []byte("a receipt"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := v.Assign(t.Context(), Scope(id), "/receipt.pdf", MainScope, false); err != nil {
		t.Fatalf("Assign out: %v", err)
	}

	warnings, err := v.DeleteSubVault(t.Context(), id, false)
	if err != nil {
		t.Fatalf("DeleteSubVault: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "receipt.pdf") {
			found = true
		}
	}
	if !found {
		t.Errorf("deleting the sub vault did not warn about the file it still seals: %v", warnings)
	}

	// The file's record survives; its content is honestly unreadable.
	entry, err := v.EntryByPath(MainScope, "/receipt.pdf")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if _, _, err := v.Fetch(t.Context(), entry.ID); err == nil {
		t.Error("a file sealed under a deleted sub vault's key still read back")
	} else if errors.Is(err, ErrSubVaultLocked) {
		t.Error("the deleted sub vault is still being offered as merely locked")
	}
}

// Film details are index, so a sub vault's live in the sub vault's own sealed
// section — never in the main manifest, which is replicated to every connected
// account — and the folder opt-in is the sub vault's own choice.
func TestFilmDetailsStayInTheVaultThatHoldsTheFile(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Films", subPassword)
	scope := Scope(id)

	if _, _, err := v.Upload(t.Context(), scope, "/", "one.mp4", []byte("a film"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := v.SetMovieLookup(scope, "/", true); err != nil {
		t.Fatalf("SetMovieLookup in the sub vault: %v", err)
	}
	if v.MovieLookupFor(MainScope, "/").Enabled {
		t.Error("opting a sub vault folder in switched the main vault on")
	}
	if !v.MovieLookupFor(scope, "/").Enabled {
		t.Error("the sub vault folder does not report the setting made on it")
	}

	entry, err := v.EntryByPath(scope, "/one.mp4")
	if err != nil {
		t.Fatalf("EntryByPath: %v", err)
	}
	if err := v.SetMovie(entry.ID, &movie.Info{Title: "Secret Film"}); err != nil {
		t.Fatalf("SetMovie for a sub vault file: %v", err)
	}
	if v.manifest.Movies[entry.ID] != nil {
		t.Error("a sub vault file's film title landed in the main manifest, which every account carries")
	}
	if got := v.Movie(entry.ID); got == nil || got.Title != "Secret Film" {
		t.Errorf("Movie = %+v, want the stored details", got)
	}
}

// Assigning a file between vaults carries its film details, so a title never
// stays behind in an index the file has left.
func TestAssignCarriesFilmDetailsAcrossTheBoundary(t *testing.T) {
	v, _ := newTestVault(t, 3)
	id := createSubVault(t, v, "Films", subPassword)

	entry, _, err := v.Upload(t.Context(), MainScope, "/", "one.mp4", []byte("a film"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := v.SetMovie(entry.ID, &movie.Info{Title: "Secret Film"}); err != nil {
		t.Fatalf("SetMovie: %v", err)
	}
	if _, err := v.Assign(t.Context(), MainScope, "/one.mp4", Scope(id), true); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if v.manifest.Movies[entry.ID] != nil {
		t.Error("the film's title stayed in the main manifest after the file left for a sub vault")
	}
	if got := v.Movie(entry.ID); got == nil || got.Title != "Secret Film" {
		t.Errorf("the film details did not travel with the file: %+v", got)
	}
}
