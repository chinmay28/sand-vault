package vault

import (
	"context"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/movie"
)

// filmsFolder stores three films under dir, each with a poster, and returns
// their IDs in the order they were added.
func filmsFolder(t *testing.T, v *Vault, dir string, titles ...string) []string {
	t.Helper()
	ctx := context.Background()

	if err := v.Mkdir(MainScope, dir); err != nil {
		t.Fatalf("Mkdir %s: %v", dir, err)
	}
	ids := make([]string, 0, len(titles))
	for _, title := range titles {
		entry, _, err := v.Upload(ctx, MainScope, dir, title+".mkv", []byte("pretend this is "+title), UploadOptions{})
		if err != nil {
			t.Fatalf("Upload %s: %v", title, err)
		}
		storeThumb(t, v, entry.ID, "poster of "+title)
		if err := v.SetMovie(entry.ID, &movie.Info{TMDBID: len(ids) + 1, Title: title, Year: 2000 + len(ids)}); err != nil {
			t.Fatalf("SetMovie %s: %v", title, err)
		}
		ids = append(ids, entry.ID)
	}
	return ids
}

func TestAFolderHasNoPictureUntilOneIsPicked(t *testing.T) {
	v, _ := newTestVault(t, 3)

	filmsFolder(t, v, "/films", "Alien", "Aliens", "Alien 3")

	if art, ok := v.FolderArtFor(MainScope, "/films"); ok {
		t.Errorf("a folder picked a picture for itself: %+v — nothing should happen until asked", art)
	}

	// The listing still carries an entry for it, empty, because "nothing chosen
	// yet" and "nothing to choose" are different things and the browser draws a
	// different row for each.
	listing, err := v.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	art, listed := listing.FolderArt["/films"]
	if !listed {
		t.Fatal("a folder with films inside it offers nothing to choose from")
	}
	if art.ID != "" {
		t.Errorf("listing says the folder is drawn with %+v, want nothing", art)
	}
}

// A folder with nothing picturable under it is left out of the listing's map
// altogether — there is nothing to offer, so nothing offers it.
func TestAFolderWithNothingPicturableOffersNothing(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if err := v.Mkdir(MainScope, "/plain"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := v.Upload(ctx, MainScope, "/plain", "notes.txt", []byte("no picture here"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	listing, err := v.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if art, listed := listing.FolderArt["/plain"]; listed {
		t.Errorf("folder art = %+v, want no entry at all", art)
	}
}

// The choices come from anywhere beneath the folder: a library of films holds
// folders, not films, and a poster from two levels down can still stand for it.
func TestFolderArtReachesIntoSubfolders(t *testing.T) {
	v, _ := newTestVault(t, 3)

	ids := filmsFolder(t, v, "/library/batman", "Batman Begins", "The Dark Knight")

	choices, _, err := v.FolderArtChoices(MainScope, "/library")
	if err != nil {
		t.Fatalf("FolderArtChoices: %v", err)
	}
	if len(choices) != len(ids) {
		t.Fatalf("choices = %d, want the %d films one level down", len(choices), len(ids))
	}

	art, err := v.SetFolderArt(MainScope, "/library", ids[1])
	if err != nil {
		t.Fatalf("SetFolderArt: %v", err)
	}
	if art.ID != ids[1] || !art.Film {
		t.Errorf("art = %+v, want the film from the folder underneath, marked as a poster", art)
	}
}

// It need not be a film. A folder of photographs can wear one of them, and the
// grid has to know it is not a poster so it does not lay it out as one.
func TestAPhotographCanStandForAFolder(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if err := v.Mkdir(MainScope, "/photos"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(ctx, MainScope, "/photos", "hike.jpg", []byte("not really a photo"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Nothing has a picture yet, so there is nothing to offer either.
	if _, err := v.SetFolderArt(MainScope, "/photos", entry.ID); err == nil {
		t.Error("a file with no thumbnail was accepted as a folder's picture")
	}

	storeThumb(t, v, entry.ID, "thumbnail-bytes")
	art, err := v.SetFolderArt(MainScope, "/photos", entry.ID)
	if err != nil {
		t.Fatalf("SetFolderArt: %v", err)
	}
	if art.ID != entry.ID {
		t.Fatalf("art = %+v, want the photograph", art)
	}
	if art.Film {
		t.Error("a photograph is not a poster and must not be laid out as one")
	}
}

func TestChoosingAndUnchoosingAFolderPicture(t *testing.T) {
	v, _ := newTestVault(t, 3)

	ids := filmsFolder(t, v, "/films", "Alien", "Aliens", "Alien 3")

	art, err := v.SetFolderArt(MainScope, "/films", ids[1])
	if err != nil {
		t.Fatalf("SetFolderArt: %v", err)
	}
	if art.ID != ids[1] {
		t.Errorf("art = %+v, want %s", art, ids[1])
	}
	if got, ok := v.FolderArtFor(MainScope, "/films"); !ok || got.ID != ids[1] {
		t.Errorf("the choice did not stick: %+v", got)
	}

	// Picking another replaces it rather than adding to it.
	if _, err := v.SetFolderArt(MainScope, "/films", ids[2]); err != nil {
		t.Fatalf("SetFolderArt again: %v", err)
	}
	if got, _ := v.FolderArtFor(MainScope, "/films"); got.ID != ids[2] {
		t.Errorf("second choice did not stick: %+v", got)
	}

	// And taking it away puts the folder back to its icon rather than to some
	// other picture.
	cleared, err := v.SetFolderArt(MainScope, "/films", "")
	if err != nil {
		t.Fatalf("SetFolderArt(clear): %v", err)
	}
	if cleared.ID != "" {
		t.Errorf("clearing left %+v behind", cleared)
	}
	if got, ok := v.FolderArtFor(MainScope, "/films"); ok {
		t.Errorf("after clearing = %+v, want no picture at all", got)
	}
	if v.manifest.FolderArt != nil {
		t.Errorf("the cleared choice is still in the index: %v", v.manifest.FolderArt)
	}
}

func TestAFolderPictureHasToBeSomethingInsideIt(t *testing.T) {
	v, _ := newTestVault(t, 3)

	filmsFolder(t, v, "/films", "Alien")
	elsewhere := filmsFolder(t, v, "/other", "Aliens")
	ctx := context.Background()
	if err := v.Mkdir(MainScope, "/plain"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	noPicture, _, err := v.Upload(ctx, MainScope, "/plain", "notes.txt", []byte("no picture here"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	for _, tc := range []struct{ name, dir, id string }{
		{"a file in another folder", "/films", elsewhere[0]},
		{"a file that has no picture", "/plain", noPicture.ID},
		{"a file that does not exist", "/films", "not-an-id"},
	} {
		if _, err := v.SetFolderArt(MainScope, tc.dir, tc.id); err == nil {
			t.Errorf("%s was accepted as a folder's picture", tc.name)
		}
	}
}

// The choice is stored by file ID against the folder's path, so both halves of
// it have to survive the things that move either one.
func TestAChosenFolderPictureSurvivesAMoveAndDiesWithItsFile(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	ids := filmsFolder(t, v, "/films", "Alien", "Aliens")
	if _, err := v.SetFolderArt(MainScope, "/films", ids[1]); err != nil {
		t.Fatalf("SetFolderArt: %v", err)
	}

	if err := v.MoveFolder(ctx, MainScope, "/films", "/cinema"); err != nil {
		t.Fatalf("MoveFolder: %v", err)
	}
	if got, _ := v.FolderArtFor(MainScope, "/cinema"); got.ID != ids[1] {
		t.Errorf("after moving the folder = %+v, want the chosen picture %s", got, ids[1])
	}

	// Moving the file itself deeper leaves it inside the folder, so the choice
	// still stands.
	if err := v.Mkdir(MainScope, "/cinema/extras"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, err := v.Move(ctx, ids[1], "/cinema/extras", ""); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if got, _ := v.FolderArtFor(MainScope, "/cinema"); got.ID != ids[1] {
		t.Errorf("after moving the file deeper = %+v, want it to still stand", got)
	}

	// Deleting it drops the choice rather than leaving the folder pointing at a
	// file that is gone — and the folder goes back to its icon rather than to
	// whatever else happens to be in there.
	if _, err := v.Delete(ctx, ids[1]); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, ok := v.FolderArtFor(MainScope, "/cinema"); ok {
		t.Errorf("after deleting the chosen file = %+v; want no picture", got)
	}
	if v.manifest.FolderArt != nil {
		t.Errorf("the dead choice is still in the index: %v", v.manifest.FolderArt)
	}
}

func TestFolderArtChoicesListFilmsFirst(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	filmsFolder(t, v, "/films", "Zulu", "Alien")
	plain, _, err := v.Upload(ctx, MainScope, "/films", "cover.jpg", []byte("not really a photo"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	storeThumb(t, v, plain.ID, "thumbnail-bytes")

	choices, truncated, err := v.FolderArtChoices(MainScope, "/films")
	if err != nil {
		t.Fatalf("FolderArtChoices: %v", err)
	}
	if truncated {
		t.Error("three files should not have been cut short")
	}

	var order []string
	for _, c := range choices {
		label := c.Name
		if c.Film {
			label = c.Title
		}
		order = append(order, label)
	}
	if want := "Alien Zulu cover.jpg"; strings.Join(order, " ") != want {
		t.Errorf("choices = %v, want films by title first, then the rest", order)
	}

	// A file with no picture is not something a folder can be drawn with.
	if _, _, err := v.Upload(ctx, MainScope, "/films", "notes.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	choices, _, err = v.FolderArtChoices(MainScope, "/films")
	if err != nil {
		t.Fatalf("FolderArtChoices: %v", err)
	}
	if len(choices) != 3 {
		t.Errorf("choices = %d, want the three files that have a picture", len(choices))
	}
}
