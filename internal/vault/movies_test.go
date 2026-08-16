package vault

import (
	"context"
	"testing"

	"github.com/chinmay28/sand-vault/internal/movie"
)

func TestMovieLookupIsInheritedDownwards(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if err := v.Mkdir("/films/2019/Parasite (2019)"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if got := v.MovieLookupFor("/films"); got.Enabled {
		t.Fatalf("lookup = %+v, want off until it is asked for", got)
	}

	if err := v.SetMovieLookup("/films", true); err != nil {
		t.Fatalf("SetMovieLookup: %v", err)
	}

	// The folder it was set on, and everything under it, with the source
	// naming the folder that actually carries the setting.
	for _, dir := range []string{"/films", "/films/2019", "/films/2019/Parasite (2019)"} {
		got := v.MovieLookupFor(dir)
		if !got.Enabled || got.Source != "/films" {
			t.Errorf("MovieLookupFor(%q) = %+v, want on from /films", dir, got)
		}
	}
	if v.MovieLookupFor("/films").Inherited("/films") {
		t.Error("the folder the setting was made on reports itself as inheriting it")
	}
	if !v.MovieLookupFor("/films/2019").Inherited("/films/2019") {
		t.Error("a subfolder does not report the setting as inherited")
	}

	// And nothing beside it.
	if got := v.MovieLookupFor("/photos"); got.Enabled {
		t.Errorf("MovieLookupFor(/photos) = %+v, want off", got)
	}
	if got := v.MovieLookupFor("/"); got.Enabled {
		t.Errorf("MovieLookupFor(/) = %+v — the root must not inherit from below it", got)
	}
}

// Turning it off leaves the details alone: they describe files that have not
// changed, and deleting them would only mean fetching them again.
func TestTurningLookupOffKeepsWhatWasFound(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if err := v.Mkdir("/films"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(context.Background(), "/films", "The.Thing.1982.mkv",
		[]byte("pretend this is a film"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := v.SetMovieLookup("/films", true); err != nil {
		t.Fatalf("SetMovieLookup: %v", err)
	}
	if err := v.SetMovie(entry.ID, &movie.Info{TMDBID: 1091, Title: "The Thing", Year: 1982}); err != nil {
		t.Fatalf("SetMovie: %v", err)
	}

	if err := v.SetMovieLookup("/films", false); err != nil {
		t.Fatalf("SetMovieLookup off: %v", err)
	}
	if got := v.Movie(entry.ID); got == nil || got.Title != "The Thing" {
		t.Errorf("movie = %+v, want it kept", got)
	}
	if got := v.MovieLookupFor("/films"); got.Enabled {
		t.Errorf("lookup = %+v, want off", got)
	}

	// Forgetting one is its own action, and it is per file.
	if err := v.ForgetMovie(entry.ID); err != nil {
		t.Fatalf("ForgetMovie: %v", err)
	}
	if got := v.Movie(entry.ID); got != nil {
		t.Errorf("movie = %+v, want nothing", got)
	}
}

// A films folder that gets renamed is still a films folder — the setting is
// filed under the folder, so it has to travel with it.
func TestMovingAFolderCarriesItsLookupSetting(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if err := v.Mkdir("/films/2019"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := v.SetMovieLookup("/films", true); err != nil {
		t.Fatalf("SetMovieLookup: %v", err)
	}
	if err := v.MoveFolder(context.Background(), "/films", "/cinema"); err != nil {
		t.Fatalf("MoveFolder: %v", err)
	}

	if got := v.MovieLookupFor("/cinema/2019"); !got.Enabled || got.Source != "/cinema" {
		t.Errorf("MovieLookupFor(/cinema/2019) = %+v, want on from /cinema", got)
	}
	if got := v.MovieLookupFor("/films"); got.Enabled {
		t.Errorf("MovieLookupFor(/films) = %+v — the folder is not there any more", got)
	}
}

// A film outliving the file it described would sit in the index forever.
func TestDeletingTakesTheFilmWithIt(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if err := v.Mkdir("/films"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(context.Background(), "/films", "Alien.1979.mkv",
		[]byte("pretend this is a film"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := v.SetMovie(entry.ID, &movie.Info{TMDBID: 348, Title: "Alien", Year: 1979}); err != nil {
		t.Fatalf("SetMovie: %v", err)
	}

	if _, err := v.Delete(context.Background(), entry.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := v.Movie(entry.ID); got != nil {
		t.Errorf("movie = %+v, want it gone with the file", got)
	}
}

func TestDeletingAFolderTakesItsSettingAndFilms(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if err := v.Mkdir("/films/2019"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(context.Background(), "/films/2019", "Parasite.2019.mkv",
		[]byte("pretend this is a film"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := v.SetMovieLookup("/films", true); err != nil {
		t.Fatalf("SetMovieLookup: %v", err)
	}
	if err := v.SetMovie(entry.ID, &movie.Info{TMDBID: 496243, Title: "Parasite", Year: 2019}); err != nil {
		t.Fatalf("SetMovie: %v", err)
	}

	if _, err := v.Rmdir(context.Background(), "/films", true); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}
	if got := v.Movie(entry.ID); got != nil {
		t.Errorf("movie = %+v, want it gone with the folder", got)
	}
	if folders := v.MovieFolders(); len(folders) != 0 {
		t.Errorf("movie folders = %v, want none", folders)
	}
}

// The key is a credential the user set, not something derived from the
// password — so changing the password must not cost it, and it must not be
// readable without one.
func TestTheFilmDatabaseKeySurvivesAPasswordChange(t *testing.T) {
	v, _ := newTestVault(t, 3)

	if got := v.MovieAPIKey(); got != "" {
		t.Fatalf("MovieAPIKey = %q on a fresh vault", got)
	}
	if err := v.SetMovieAPIKey("  a-key-of-my-own  "); err != nil {
		t.Fatalf("SetMovieAPIKey: %v", err)
	}
	if got := v.MovieAPIKey(); got != "a-key-of-my-own" {
		t.Errorf("MovieAPIKey = %q, want it trimmed and stored", got)
	}

	if _, err := v.ChangePassword(context.Background(), testPassword,
		"a different passphrase entirely", true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if got := v.MovieAPIKey(); got != "a-key-of-my-own" {
		t.Errorf("after a password change MovieAPIKey = %q", got)
	}

	// And it reads back off disk under the new password, rather than only out
	// of the memory that never left.
	v.Lock()
	if got := v.MovieAPIKey(); got != "" {
		t.Errorf("a locked vault answered with %q", got)
	}
	if err := v.Unlock("a different passphrase entirely"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if got := v.MovieAPIKey(); got != "a-key-of-my-own" {
		t.Errorf("after unlocking MovieAPIKey = %q", got)
	}

	// Clearing it writes no settings section at all, rather than an encrypted
	// empty one.
	if err := v.SetMovieAPIKey(""); err != nil {
		t.Fatalf("clearing the key: %v", err)
	}
	if v.store.Settings != nil {
		t.Error("a vault with nothing set still carries a settings section")
	}
}
