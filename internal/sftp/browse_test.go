package sftp

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/sftp/sftptest"
)

// browseFixture starts a server over a tree and returns a client, the root the
// source is scoped to (relative to what the server serves), and the directory
// on disk behind it.
func browseFixture(t *testing.T) (*Client, string, string) {
	t.Helper()

	disk := t.TempDir()
	server := sftptest.NewServer(t, disk)
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub
	host, port := server.HostPort(t)

	client, err := Dial(context.Background(), Config{
		Host: host, Port: port, User: "sand", PrivateKey: key,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	// The server drops us in `disk`, and paths on the wire are absolute, so the
	// source's root is the absolute path of the scoped subdirectory.
	root := filepath.Join(disk, "media")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("making the root: %v", err)
	}
	return client, filepath.ToSlash(root), disk
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func names(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func find(t *testing.T, entries []Entry, name string) Entry {
	t.Helper()
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry named %q in %v", name, names(entries))
	return Entry{}
}

func TestReadDirListsAndOrders(t *testing.T) {
	client, root, _ := browseFixture(t)

	write(t, filepath.Join(root, "zebra.txt"), "z")
	write(t, filepath.Join(root, "Apple.txt"), "a")
	write(t, filepath.Join(root, "movies", "one.mp4"), "film")
	if err := os.MkdirAll(filepath.Join(root, "Books"), 0755); err != nil {
		t.Fatal(err)
	}

	listing, err := client.ReadDir(root, "")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if !listing.AtRoot {
		t.Error("listing the root did not say so")
	}
	if listing.Path != "" {
		t.Errorf("root listed as %q, want the empty path", listing.Path)
	}

	// Folders first, then case-folded by name.
	want := []string{"Books", "movies", "Apple.txt", "zebra.txt"}
	if got := names(listing.Entries); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order: got %v, want %v", got, want)
	}

	if size := find(t, listing.Entries, "zebra.txt").Size; size != 1 {
		t.Errorf("zebra.txt size %d, want 1", size)
	}
	if !find(t, listing.Entries, "movies").Dir {
		t.Error("movies is not marked as a folder")
	}
}

func TestReadDirDescendsAndComesBack(t *testing.T) {
	client, root, _ := browseFixture(t)
	write(t, filepath.Join(root, "a", "b", "deep.txt"), "x")

	listing, err := client.ReadDir(root, "a/b")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if listing.Path != "a/b" {
		t.Errorf("path %q, want a/b", listing.Path)
	}
	if listing.Parent != "a" {
		t.Errorf("parent %q, want a", listing.Parent)
	}
	if listing.AtRoot {
		t.Error("a/b reported as the root")
	}

	// One level up from the top is the root, which is "" and not a path above it.
	top, err := client.ReadDir(root, "a")
	if err != nil {
		t.Fatalf("read dir a: %v", err)
	}
	if top.Parent != "" {
		t.Errorf("parent of a is %q, want the root", top.Parent)
	}
}

// Nothing outside the source's folder can be listed, however the path is
// written. The browse endpoint takes this straight off a query string.
func TestReadDirCannotLeaveTheRoot(t *testing.T) {
	client, root, disk := browseFixture(t)
	write(t, filepath.Join(disk, "secret.txt"), "not yours")

	for _, rel := range []string{"..", "../", "../secret.txt", "a/../../", `..\..`} {
		if _, err := client.ReadDir(root, rel); err == nil {
			t.Errorf("ReadDir(%q) was allowed", rel)
		}
	}
}

// The rule browsing rests on: a link is followed only if it lands inside the
// root. Without it, one `ln -s / everything` turns a scoped source into a file
// browser for the whole machine.
func TestReadDirWillNotFollowALinkOutOfTheRoot(t *testing.T) {
	client, root, disk := browseFixture(t)
	write(t, filepath.Join(disk, "outside", "secret.txt"), "not yours")
	write(t, filepath.Join(root, "inside", "fine.txt"), "yours")

	if err := os.Symlink(filepath.Join(disk, "outside"), filepath.Join(root, "escape")); err != nil {
		t.Skipf("this filesystem will not make symlinks: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "inside"), filepath.Join(root, "shortcut")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "gone"), filepath.Join(root, "dangling")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	listing, err := client.ReadDir(root, "")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	escape := find(t, listing.Entries, "escape")
	if !escape.Symlink {
		t.Error("escape is not marked as a link")
	}
	if !escape.Unreachable {
		t.Error("a link pointing outside the root was offered as followable")
	}
	if !strings.Contains(escape.Reason, "outside") {
		t.Errorf("reason %q does not say the link leaves the folder", escape.Reason)
	}

	// Listed rather than hidden: a directory that appears to hold fewer files
	// than ls shows reads as a bug rather than as a rule.
	if len(names(listing.Entries)) != 4 {
		t.Errorf("entries %v, want all four listed", names(listing.Entries))
	}

	shortcut := find(t, listing.Entries, "shortcut")
	if shortcut.Unreachable {
		t.Errorf("a link inside the root was refused: %s", shortcut.Reason)
	}
	if !shortcut.Dir {
		t.Error("a link to a folder is not marked as a folder")
	}

	if !find(t, listing.Entries, "dangling").Unreachable {
		t.Error("a link to nothing was offered as followable")
	}

	// And descending through the escaping link is refused too, not merely
	// discouraged in the listing.
	if _, err := client.ReadDir(root, "escape"); err == nil {
		t.Error("descending through an escaping link was allowed")
	}
}

func TestReadDirTruncatesAVeryLargeDirectory(t *testing.T) {
	client, root, _ := browseFixture(t)

	crowded := filepath.Join(root, "crowded")
	if err := os.MkdirAll(crowded, 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxEntries+10; i++ {
		write(t, filepath.Join(crowded, "f"+strings.Repeat("0", 5-len(itoa(i)))+itoa(i)), "x")
	}

	listing, err := client.ReadDir(root, "crowded")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(listing.Entries) != MaxEntries {
		t.Errorf("listed %d entries, want %d", len(listing.Entries), MaxEntries)
	}
	if !listing.Truncated {
		t.Error("a directory that was cut short did not say so")
	}
}

// The cap is on the page, not the directory: a walk that has to find every
// file asks for all of them and gets all of them.
func TestReadDirAllIsNotCut(t *testing.T) {
	client, root, _ := browseFixture(t)

	crowded := filepath.Join(root, "crowded")
	if err := os.MkdirAll(crowded, 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxEntries+10; i++ {
		write(t, filepath.Join(crowded, "f"+strings.Repeat("0", 5-len(itoa(i)))+itoa(i)), "x")
	}

	listing, err := client.ReadDirAll(root, "crowded")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(listing.Entries) != MaxEntries+10 {
		t.Errorf("listed %d entries, want every one of %d", len(listing.Entries), MaxEntries+10)
	}
	if listing.Truncated {
		t.Error("a complete listing said it was cut short")
	}
	// Still a listing and not a raw directory read: the same order, and the
	// same refusal to step outside the root.
	if listing.Entries[0].Name != "f00000" || listing.Entries[MaxEntries+9].Name != "f02009" {
		t.Errorf("entries are not sorted: first %q, last %q",
			listing.Entries[0].Name, listing.Entries[MaxEntries+9].Name)
	}
	if _, err := client.ReadDirAll(root, "../"); err == nil {
		t.Error("ReadDirAll climbed out of the root")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestOpenUnder(t *testing.T) {
	client, root, disk := browseFixture(t)
	write(t, filepath.Join(root, "films", "one.mp4"), "the film")
	write(t, filepath.Join(disk, "secret.txt"), "not yours")

	f, info, err := client.OpenUnder(root, "films/one.mp4")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if info.Size() != int64(len("the film")) {
		t.Errorf("size %d, want %d", info.Size(), len("the film"))
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "the film" {
		t.Errorf("read %q, want %q", got, "the film")
	}
}

func TestOpenUnderRefusesWhatIsNotAFile(t *testing.T) {
	client, root, disk := browseFixture(t)
	write(t, filepath.Join(root, "films", "one.mp4"), "x")
	write(t, filepath.Join(disk, "secret.txt"), "not yours")

	if _, _, err := client.OpenUnder(root, "films"); err == nil {
		t.Error("opened a folder as a file")
	}
	if _, _, err := client.OpenUnder(root, "../secret.txt"); err == nil {
		t.Error("opened a file outside the root")
	}
	if _, _, err := client.OpenUnder(root, "films/missing.mp4"); err == nil {
		t.Error("opened a file that is not there")
	}

	// A link out of the root is refused before it is opened, not after: Stat
	// follows links, so a check on the opened file would already have decided.
	if err := os.Symlink(filepath.Join(disk, "secret.txt"), filepath.Join(root, "leak")); err != nil {
		t.Skipf("this filesystem will not make symlinks: %v", err)
	}
	if _, _, err := client.OpenUnder(root, "leak"); err == nil {
		t.Error("opened a link pointing outside the root")
	}
}
