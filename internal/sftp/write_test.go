package sftp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A file written under the root lands whole, under the name asked for, and
// nothing is left beside it.
func TestWriteUnderPutsTheFileInPlace(t *testing.T) {
	client, root, _ := browseFixture(t)

	n, err := client.WriteUnder(root, "out/notes.txt", strings.NewReader("hello"), WriteOptions{})
	if err != nil {
		t.Fatalf("WriteUnder: %v", err)
	}
	if n != 5 {
		t.Errorf("wrote %d bytes, want 5", n)
	}

	got, err := os.ReadFile(filepath.Join(root, "out", "notes.txt"))
	if err != nil {
		t.Fatalf("the file is not there: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("stored %q, want hello", got)
	}

	// No temporary name survives a finished write.
	entries, _ := os.ReadDir(filepath.Join(root, "out"))
	for _, e := range entries {
		if IsTempName(e.Name()) {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

// The name asked for is either the whole file or nothing: what arrives is
// written under a temporary name and only renamed once it is complete, so a
// write that fails partway leaves nothing under the real name.
func TestWriteUnderLeavesNothingHalfWritten(t *testing.T) {
	client, root, _ := browseFixture(t)

	broken := &failingReader{after: 10}
	_, err := client.WriteUnder(root, "film.mp4", broken, WriteOptions{})
	if err == nil {
		t.Fatal("a reader that failed was written as though it had not")
	}
	if _, statErr := os.Stat(filepath.Join(root, "film.mp4")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a half-written file was left under its real name: %v", statErr)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if IsTempName(e.Name()) {
			t.Errorf("the temporary file was not cleaned up: %s", e.Name())
		}
	}
}

// Without Overwrite a file already at the name is left exactly as it was, and
// the write says so rather than clobbering it.
func TestWriteUnderRefusesToOverwriteUnlessAsked(t *testing.T) {
	client, root, _ := browseFixture(t)
	write(t, filepath.Join(root, "a.txt"), "original")

	if _, err := client.WriteUnder(root, "a.txt", strings.NewReader("replacement"), WriteOptions{}); err == nil {
		t.Fatal("an existing file was overwritten without Overwrite")
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "original" {
		t.Errorf("the existing file was changed to %q", got)
	}

	if _, err := client.WriteUnder(root, "a.txt", strings.NewReader("replacement"), WriteOptions{Overwrite: true}); err != nil {
		t.Fatalf("overwriting on purpose failed: %v", err)
	}
	got, _ = os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "replacement" {
		t.Errorf("stored %q after an overwrite, want replacement", got)
	}
}

// The modification time asked for is the one the file ends up with, which is
// what lets a later export recognise its own copy.
func TestWriteUnderStampsTheModificationTime(t *testing.T) {
	client, root, _ := browseFixture(t)
	when := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

	if _, err := client.WriteUnder(root, "dated.txt", strings.NewReader("x"), WriteOptions{ModTime: when}); err != nil {
		t.Fatalf("WriteUnder: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "dated.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(when) {
		t.Errorf("modified at %v, want %v", info.ModTime(), when)
	}
}

// The write side goes through the same boundary the read side does: nothing
// outside the root can be written, however the path is spelled, and a link
// pointing out of it is not followed.
func TestWriteUnderCannotLeaveTheRoot(t *testing.T) {
	client, root, disk := browseFixture(t)

	for _, rel := range []string{"../escape.txt", "a/../../escape.txt", ""} {
		if _, err := client.WriteUnder(root, rel, strings.NewReader("x"), WriteOptions{}); err == nil {
			t.Errorf("writing %q was allowed", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(disk, "escape.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file was written above the root")
	}

	outside := filepath.Join(disk, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "door")); err != nil {
		t.Skipf("cannot make a symlink here: %v", err)
	}
	if _, err := client.WriteUnder(root, "door/leak.txt", strings.NewReader("x"), WriteOptions{}); err == nil {
		t.Error("a write through a link out of the root was allowed")
	}
	if _, err := os.Stat(filepath.Join(outside, "leak.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file was written through a link pointing outside the root")
	}
}

// MkdirUnder makes the whole path, and is content to find it already there.
func TestMkdirUnder(t *testing.T) {
	client, root, _ := browseFixture(t)

	made, err := client.MkdirUnder(root, "a/b/c")
	if err != nil {
		t.Fatalf("MkdirUnder: %v", err)
	}
	if made != filepath.ToSlash(filepath.Join(root, "a", "b", "c")) {
		t.Errorf("made %q", made)
	}
	if info, err := os.Stat(filepath.Join(root, "a", "b", "c")); err != nil || !info.IsDir() {
		t.Errorf("the folder is not there: %v", err)
	}
	if _, err := client.MkdirUnder(root, "a/b/c"); err != nil {
		t.Errorf("making a folder that exists failed: %v", err)
	}
	if _, err := client.MkdirUnder(root, "../above"); err == nil {
		t.Error("a folder above the root was allowed")
	}
}

// RenameOver replaces a file that is already there, by whichever route the
// server supports.
func TestRenameOverReplaces(t *testing.T) {
	client, root, _ := browseFixture(t)
	write(t, filepath.Join(root, "from.txt"), "new")
	write(t, filepath.Join(root, "to.txt"), "old")

	if err := RenameOver(client, filepath.ToSlash(filepath.Join(root, "from.txt")),
		filepath.ToSlash(filepath.Join(root, "to.txt"))); err != nil {
		t.Fatalf("RenameOver: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "to.txt"))
	if string(got) != "new" {
		t.Errorf("to.txt holds %q, want new", got)
	}
	if _, err := os.Stat(filepath.Join(root, "from.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the source of the rename is still there")
	}
}

func TestTempNames(t *testing.T) {
	a, b := TempName("/srv"), TempName("/srv")
	if a == b {
		t.Error("two temporary names collided")
	}
	if !strings.HasPrefix(a, "/srv/"+tempPrefix) {
		t.Errorf("temp name %q is not in the folder asked for", a)
	}
	if !IsTempName(filepath.Base(a)) {
		t.Errorf("%q is not recognised as a temporary name", a)
	}
	if IsTempName("notes.txt") || IsTempName(tempPrefix) {
		t.Error("an ordinary name was taken for a temporary one")
	}
}

// failingReader hands out a few bytes and then reports an error, the way a
// gathered file does when one of its clouds drops out halfway.
type failingReader struct {
	after int
	sent  int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.sent >= f.after {
		return 0, errors.New("the cloud went away")
	}
	n := copy(p, strings.Repeat("x", f.after-f.sent))
	f.sent += n
	return n, nil
}
