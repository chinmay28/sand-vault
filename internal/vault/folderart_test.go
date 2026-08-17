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

	if err := v.Mkdir(dir); err != nil {
		t.Fatalf("Mkdir %s: %v", dir, err)
	}
	ids := make([]string, 0, len(titles))
	for _, title := range titles {
		entry, _, err := v.Upload(ctx, dir, title+".mkv", []byte("pretend this is "+title), UploadOptions{})
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

func TestFolderArtIsAPosterFromInsideAndStaysPut(t *testing.T) {
	v, _ := newTestVault(t, 3)

	ids := filmsFolder(t, v, "/films", "Alien", "Aliens", "Alien 3")
	held := map[string]bool{}
	for _, id := range ids {
		held[id] = true
	}

	art, ok := v.FolderArtFor("/films")
	if !ok {
		t.Fatal("a folder of films is drawn with nothing")
	}
	if !held[art.ID] {
		t.Errorf("art = %+v, want one of the films inside it", art)
	}
	if !art.Film {
		t.Error("the picture is a film's poster and should say so — the grid lays posters out two-by-three")
	}
	if art.Chosen {
		t.Error("nobody picked it, so it must not claim to have been picked")
	}

	// Asked again it answers the same, because a folder that changed its face
	// on every listing would be worse than the icon it replaced.
	for i := 0; i < 5; i++ {
		if again, _ := v.FolderArtFor("/films"); again.ID != art.ID {
			t.Fatalf("the pick moved on attempt %d: %s then %s", i, art.ID, again.ID)
		}
	}
}

// A folder's picture comes from anywhere beneath it: a library of films holds
// folders, not films, and it still has to have a face.
func TestFolderArtReachesIntoSubfolders(t *testing.T) {
	v, _ := newTestVault(t, 3)

	ids := filmsFolder(t, v, "/library/batman", "Batman Begins", "The Dark Knight")

	art, ok := v.FolderArtFor("/library")
	if !ok {
		t.Fatal("a folder whose films are one level down is drawn with nothing")
	}
	if art.ID != ids[0] && art.ID != ids[1] {
		t.Errorf("art = %+v, want a film from the folder underneath", art)
	}
}

// Two folders holding films should not both show whichever one happens to sort
// first — the point of the pick being seeded by the folder as well as the file.
func TestFolderArtDiffersBetweenFolders(t *testing.T) {
	v, _ := newTestVault(t, 3)

	// The same three titles in both, so only the folder can tell them apart.
	filmsFolder(t, v, "/one", "A", "B", "C")
	filmsFolder(t, v, "/two", "A", "B", "C")

	listing, err := v.List("/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.FolderArt) != 2 {
		t.Fatalf("folder art = %+v, want a picture for each folder", listing.FolderArt)
	}
	for _, dir := range []string{"/one", "/two"} {
		if listing.FolderArt[dir].ID == "" {
			t.Errorf("%s is drawn with nothing", dir)
		}
	}
}

func TestFolderArtFallsBackToAnyPicture(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if err := v.Mkdir("/photos"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(ctx, "/photos", "hike.jpg", []byte("not really a photo"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Nothing has a picture yet, so neither does the folder.
	if _, ok := v.FolderArtFor("/photos"); ok {
		t.Error("a folder with no pictures inside it should keep its icon")
	}

	storeThumb(t, v, entry.ID, "thumbnail-bytes")
	art, ok := v.FolderArtFor("/photos")
	if !ok || art.ID != entry.ID {
		t.Fatalf("art = %+v, %v; want the photograph", art, ok)
	}
	if art.Film {
		t.Error("a photograph is not a poster and must not be laid out as one")
	}
}

func TestChoosingAFolderPicture(t *testing.T) {
	v, _ := newTestVault(t, 3)

	ids := filmsFolder(t, v, "/films", "Alien", "Aliens", "Alien 3")
	automatic, _ := v.FolderArtFor("/films")

	// Pick one that is not the one the vault chose, so the change is visible.
	var wanted string
	for _, id := range ids {
		if id != automatic.ID {
			wanted = id
			break
		}
	}

	art, err := v.SetFolderArt("/films", wanted)
	if err != nil {
		t.Fatalf("SetFolderArt: %v", err)
	}
	if art.ID != wanted || !art.Chosen {
		t.Errorf("art = %+v, want %s, chosen", art, wanted)
	}
	if got, _ := v.FolderArtFor("/films"); got.ID != wanted {
		t.Errorf("the choice did not stick: %+v", got)
	}

	// Handing it back means the vault picks again, and picks what it did before.
	if _, err := v.SetFolderArt("/films", ""); err != nil {
		t.Fatalf("SetFolderArt(clear): %v", err)
	}
	if got, _ := v.FolderArtFor("/films"); got.ID != automatic.ID || got.Chosen {
		t.Errorf("after clearing = %+v, want the automatic pick %s", got, automatic.ID)
	}
}

func TestAFolderPictureHasToBeSomethingInsideIt(t *testing.T) {
	v, _ := newTestVault(t, 3)

	filmsFolder(t, v, "/films", "Alien")
	elsewhere := filmsFolder(t, v, "/other", "Aliens")
	ctx := context.Background()
	if err := v.Mkdir("/plain"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	noPicture, _, err := v.Upload(ctx, "/plain", "notes.txt", []byte("no picture here"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	for _, tc := range []struct{ name, dir, id string }{
		{"a file in another folder", "/films", elsewhere[0]},
		{"a file that has no picture", "/plain", noPicture.ID},
		{"a file that does not exist", "/films", "not-an-id"},
	} {
		if _, err := v.SetFolderArt(tc.dir, tc.id); err == nil {
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
	if _, err := v.SetFolderArt("/films", ids[1]); err != nil {
		t.Fatalf("SetFolderArt: %v", err)
	}

	if err := v.MoveFolder(ctx, "/films", "/cinema"); err != nil {
		t.Fatalf("MoveFolder: %v", err)
	}
	if got, _ := v.FolderArtFor("/cinema"); got.ID != ids[1] || !got.Chosen {
		t.Errorf("after moving the folder = %+v, want the chosen picture %s", got, ids[1])
	}

	// Moving the file itself deeper leaves it inside the folder, so the choice
	// still stands.
	if err := v.Mkdir("/cinema/extras"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, err := v.Move(ctx, ids[1], "/cinema/extras", ""); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if got, _ := v.FolderArtFor("/cinema"); got.ID != ids[1] {
		t.Errorf("after moving the file deeper = %+v, want it to still stand", got)
	}

	// Deleting it drops the choice rather than leaving the folder pointing at
	// a file that is gone.
	if _, err := v.Delete(ctx, ids[1]); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, ok := v.FolderArtFor("/cinema")
	if !ok || got.ID != ids[0] || got.Chosen {
		t.Errorf("after deleting the chosen file = %+v, %v; want the vault to pick again", got, ok)
	}
	if v.manifest.FolderArt != nil {
		t.Errorf("the dead choice is still in the index: %v", v.manifest.FolderArt)
	}
}

func TestFolderArtChoicesListFilmsFirst(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	filmsFolder(t, v, "/films", "Zulu", "Alien")
	plain, _, err := v.Upload(ctx, "/films", "cover.jpg", []byte("not really a photo"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	storeThumb(t, v, plain.ID, "thumbnail-bytes")

	choices, truncated, err := v.FolderArtChoices("/films")
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
	if _, _, err := v.Upload(ctx, "/films", "notes.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	choices, _, err = v.FolderArtChoices("/films")
	if err != nil {
		t.Fatalf("FolderArtChoices: %v", err)
	}
	if len(choices) != 3 {
		t.Errorf("choices = %d, want the three files that have a picture", len(choices))
	}
}
