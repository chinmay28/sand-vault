package vault

import (
	"context"
	"testing"
	"time"
)

// put stores a file and hands back its entry, so a test about times does not
// have to say the same four arguments every time.
func put(t *testing.T, v *Vault, dir, name string, opts UploadOptions) *Entry {
	t.Helper()
	if dir != "/" && !v.FolderExists(MainScope, dir) {
		if err := v.Mkdir(MainScope, dir); err != nil {
			t.Fatalf("making %s: %v", dir, err)
		}
	}
	entry, _, err := v.Upload(context.Background(), MainScope, dir, name, []byte("some bytes"), opts)
	if err != nil {
		t.Fatalf("uploading %s: %v", name, err)
	}
	return entry
}

// The whole point of the field: a file keeps the age it had where it came from.
func TestUploadKeepsTheFilesOwnTime(t *testing.T) {
	v, _ := newTestVault(t, 3)
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)

	entry := put(t, v, "/", "hike.jpg", UploadOptions{ModifiedAt: taken})

	if !entry.ModifiedAt.Equal(taken) {
		t.Errorf("stored under %s, want the photograph's own time %s", entry.ModifiedAt, taken)
	}
	// CreatedAt is the other question, and its answer is still now: it is what
	// the import's resume check leans on.
	if time.Since(entry.CreatedAt) > time.Minute {
		t.Errorf("CreatedAt is %s, want the moment it landed here", entry.CreatedAt)
	}
}

// An upload told nothing about times behaves exactly as every upload did.
func TestUploadWithoutATimeIsStampedNow(t *testing.T) {
	v, _ := newTestVault(t, 3)

	entry := put(t, v, "/", "notes.txt", UploadOptions{})

	if time.Since(entry.ModifiedAt) > time.Minute {
		t.Errorf("stored under %s, want the moment it landed", entry.ModifiedAt)
	}
}

func TestRetimePutsAStoredTimeBack(t *testing.T) {
	v, _ := newTestVault(t, 3)
	entry := put(t, v, "/photos", "hike.jpg", UploadOptions{})
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)

	n, err := v.Retime(MainScope, []FileTime{{Path: "/photos/hike.jpg", Mod: taken}})
	if err != nil {
		t.Fatalf("Retime: %v", err)
	}
	if n != 1 {
		t.Fatalf("retimed %d files, want 1", n)
	}
	if !entry.ModifiedAt.Equal(taken) {
		t.Errorf("the entry says %s, want %s", entry.ModifiedAt, taken)
	}

	// Nothing else about the file moved: same identity, same parts.
	after := v.manifest.ByPath("/photos/hike.jpg")
	if after.ID != entry.ID || len(after.Shards) != len(entry.Shards) {
		t.Errorf("retiming changed more than the time: %+v", after)
	}

	// And it survives a reload, which is the only proof the index was written.
	reopened := reopen(t, v)
	got := reopened.manifest.ByPath("/photos/hike.jpg")
	if got == nil || !got.ModifiedAt.Equal(taken) {
		t.Errorf("after reopening the vault the time is %v, want %s", got, taken)
	}
}

// Asking twice is asking once: the second run finds the times already right
// and reports that it changed nothing.
func TestRetimeIsIdempotent(t *testing.T) {
	v, _ := newTestVault(t, 3)
	put(t, v, "/", "hike.jpg", UploadOptions{})
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)
	want := []FileTime{{Path: "/hike.jpg", Mod: taken}}

	if n, err := v.Retime(MainScope, want); err != nil || n != 1 {
		t.Fatalf("first Retime: %d, %v", n, err)
	}
	n, err := v.Retime(MainScope, want)
	if err != nil {
		t.Fatalf("second Retime: %v", err)
	}
	if n != 0 {
		t.Errorf("the second run reported %d changes, want none", n)
	}
}

func TestRetimeLeavesTheRestAlone(t *testing.T) {
	v, _ := newTestVault(t, 3)
	kept := put(t, v, "/", "other.txt", UploadOptions{})
	put(t, v, "/", "hike.jpg", UploadOptions{})
	was := kept.ModifiedAt
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)

	n, err := v.Retime(MainScope, []FileTime{
		// Nothing is stored here: an ordinary answer, not an error — that is
		// the file about to be uploaded.
		{Path: "/gone.jpg", Mod: taken},
		// No time to offer.
		{Path: "/other.txt"},
		{Path: "/hike.jpg", Mod: taken},
	})
	if err != nil {
		t.Fatalf("Retime: %v", err)
	}
	if n != 1 {
		t.Errorf("retimed %d files, want only the one named with a time", n)
	}
	if !kept.ModifiedAt.Equal(was) {
		t.Errorf("a file named without a time was restamped: %s", kept.ModifiedAt)
	}
}

// The times being compared come from filesystems that keep whole seconds and
// from a browser that keeps milliseconds, so a difference below a second is
// not a difference anybody is claiming.
func TestRetimeIgnoresSubSecondDifferences(t *testing.T) {
	v, _ := newTestVault(t, 3)
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)
	put(t, v, "/", "hike.jpg", UploadOptions{ModifiedAt: taken})

	n, err := v.Retime(MainScope, []FileTime{
		{Path: "/hike.jpg", Mod: taken.Add(750 * time.Millisecond)},
	})
	if err != nil {
		t.Fatalf("Retime: %v", err)
	}
	if n != 0 {
		t.Errorf("a difference of 750ms counted as a change")
	}
}

func TestSameModTime(t *testing.T) {
	base := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)
	cases := map[string]struct {
		a, b time.Time
		want bool
	}{
		"the same instant":     {base, base, true},
		"a different zone":     {base, base.In(time.FixedZone("east", 3600)), true},
		"under a second apart": {base, base.Add(999 * time.Millisecond), true},
		"a second apart":       {base, base.Add(time.Second), false},
		"years apart":          {base, base.AddDate(1, 0, 0), false},
	}
	for name, c := range cases {
		if got := SameModTime(c.a, c.b); got != c.want {
			t.Errorf("%s: SameModTime = %v, want %v", name, got, c.want)
		}
	}
}

func TestExistingFilesAnswersSizeAndTime(t *testing.T) {
	v, _ := newTestVault(t, 3)
	taken := time.Date(2019, 7, 14, 10, 22, 0, 0, time.UTC)
	entry := put(t, v, "/photos", "hike.jpg", UploadOptions{ModifiedAt: taken})

	got, err := v.ExistingFiles(MainScope, []string{"/photos/hike.jpg", "/photos/gone.jpg"})
	if err != nil {
		t.Fatalf("ExistingFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("answered about %d files, want only the stored one: %+v", len(got), got)
	}
	have := got["/photos/hike.jpg"]
	if have.Size != entry.Size || !have.Modified.Equal(taken) {
		t.Errorf("answered %+v, want size %d at %s", have, entry.Size, taken)
	}
	// When it arrived is the other half of the answer, and is what says whether
	// a file offered with a newer time is the same file or a replacement.
	if !have.Created.Equal(entry.CreatedAt) {
		t.Errorf("answered created %s, want %s", have.Created, entry.CreatedAt)
	}
}

// A file does not become new by being called something else or by sitting
// somewhere else, which is the same rule the rest of this file is about — and
// the one that filing a folder by date leans on entirely: a sort that restamped
// what it moved would file every file under today the second time it ran.
func TestMovingAFileKeepsTheTimeItCameWith(t *testing.T) {
	ctx := context.Background()
	v, _ := newTestVault(t, 3)
	if err := v.Mkdir(MainScope, "/photos/2019"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	taken := time.Date(2019, time.July, 4, 11, 30, 0, 0, time.UTC)
	entry, _, err := v.Upload(ctx, MainScope, "/photos", "hike.jpg", []byte("photo"),
		UploadOptions{ModifiedAt: taken})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	moved, err := v.Move(ctx, entry.ID, "/photos/2019", "july hike.jpg")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if !SameModTime(moved.ModifiedAt, taken) {
		t.Errorf("after moving and renaming the file reads as %v, want %v", moved.ModifiedAt, taken)
	}
}
