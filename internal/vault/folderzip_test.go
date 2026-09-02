package vault

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
)

// unpack reads an archive back into name → contents, with folders listed as
// names ending in a slash.
func unpack(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			out[f.Name] = ""
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("opening %s in the archive: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("reading %s in the archive: %v", f.Name, err)
		}
		out[f.Name] = string(body)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The archive holds the folder's whole tree, rooted at the folder's own name,
// empty folders included, and every file reads back byte for byte.
func TestFolderZipHoldsTheTree(t *testing.T) {
	v, _ := newTestVault(t, 3)
	store(t, v, "/photos", "a.jpg", "aaa")
	store(t, v, "/photos/2019", "b.jpg", "bbbb")
	store(t, v, "/elsewhere", "c.txt", "not in the folder")
	if err := v.Mkdir(MainScope, "/photos/empty"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var buf bytes.Buffer
	if err := v.WriteFolderZip(context.Background(), MainScope, "/photos", &buf); err != nil {
		t.Fatalf("WriteFolderZip: %v", err)
	}

	got := unpack(t, buf.Bytes())
	want := []string{"photos/2019/", "photos/2019/b.jpg", "photos/a.jpg", "photos/empty/"}
	if keys := sortedKeys(got); strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("archive holds %v, want %v", keys, want)
	}
	if got["photos/a.jpg"] != "aaa" || got["photos/2019/b.jpg"] != "bbbb" {
		t.Errorf("contents came back as %v", got)
	}
}

// Stored, not deflated: the files were compressed before they were split,
// and a Pi's CPU is not where the download should be spent.
func TestFolderZipStoresRatherThanDeflates(t *testing.T) {
	v, _ := newTestVault(t, 3)
	store(t, v, "/docs", "a.txt", strings.Repeat("compressible ", 200))

	var buf bytes.Buffer
	if err := v.WriteFolderZip(context.Background(), MainScope, "/docs", &buf); err != nil {
		t.Fatalf("WriteFolderZip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Method != zip.Store {
			t.Errorf("%s was written with method %d, want Store", f.Name, f.Method)
		}
	}
}

// The root has no name of its own, so its archive is called "vault".
func TestFolderZipOfTheRoot(t *testing.T) {
	v, _ := newTestVault(t, 3)
	store(t, v, "/", "a.txt", "a")
	store(t, v, "/deep", "b.txt", "b")

	plan, err := v.PlanFolderZip(MainScope, "/")
	if err != nil {
		t.Fatalf("PlanFolderZip: %v", err)
	}
	if plan.Name != "vault" || plan.Files != 2 || plan.Folders != 1 || plan.Bytes != 2 {
		t.Errorf("plan: %+v", plan)
	}

	var buf bytes.Buffer
	if err := v.WriteFolderZip(context.Background(), MainScope, "/", &buf); err != nil {
		t.Fatalf("WriteFolderZip: %v", err)
	}
	got := unpack(t, buf.Bytes())
	want := []string{"vault/a.txt", "vault/deep/", "vault/deep/b.txt"}
	if keys := sortedKeys(got); strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("archive holds %v, want %v", keys, want)
	}
}

// A file stored before chunking cannot be streamed, and the plan says so up
// front — before a byte of archive has been sent, since a 200 in flight has
// no way to become a 409.
func TestFolderZipRefusesAPreChunkingFile(t *testing.T) {
	v, _ := newTestVault(t, 3)
	store(t, v, "/", "new.txt", "chunked")
	addWholeEntry(t, v, "old-1", "old.bin", []byte("stored whole"))

	plan, err := v.PlanFolderZip(MainScope, "/")
	if err != nil {
		t.Fatalf("PlanFolderZip: %v", err)
	}
	if plan.Unconverted != 1 {
		t.Errorf("plan counted %d unconverted files, want 1", plan.Unconverted)
	}
	if err := plan.Ready(); !errors.Is(err, ErrNeedsConversion) {
		t.Errorf("Ready = %v, want ErrNeedsConversion", err)
	}

	var buf bytes.Buffer
	err = v.WriteFolderZip(context.Background(), MainScope, "/", &buf)
	if !errors.Is(err, ErrNeedsConversion) {
		t.Errorf("WriteFolderZip = %v, want ErrNeedsConversion", err)
	}
	if buf.Len() != 0 {
		t.Error("bytes were written before the refusal")
	}
}

// A folder with nothing in it is refused rather than handed back as an
// archive of nothing.
func TestFolderZipRefusesAnEmptyFolder(t *testing.T) {
	v, _ := newTestVault(t, 3)
	if err := v.Mkdir(MainScope, "/empty"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := v.Mkdir(MainScope, "/empty/inside"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	plan, err := v.PlanFolderZip(MainScope, "/empty")
	if err != nil {
		t.Fatalf("PlanFolderZip: %v", err)
	}
	if err := plan.Ready(); err == nil {
		t.Error("an archive of nothing was allowed")
	}
	if _, err := v.PlanFolderZip(MainScope, "/nowhere"); err == nil {
		t.Error("a folder that is not there was planned")
	}
}

// A reader that goes away stops the archive rather than leaving the gathers
// it started running against every cloud.
func TestFolderZipStopsWhenTheReaderGoesAway(t *testing.T) {
	v, _ := newTestVault(t, 3)
	store(t, v, "/big", "a.bin", strings.Repeat("a", 100_000))
	store(t, v, "/big", "b.bin", strings.Repeat("b", 100_000))

	ctx, cancel := context.WithCancel(context.Background())
	// Give up as soon as the first bytes arrive, the way a closed tab does.
	sink := &cancellingWriter{cancel: cancel}
	err := v.WriteFolderZip(ctx, MainScope, "/big", sink)
	if err == nil {
		t.Fatal("the archive finished after its reader gave up")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("stopped with %v, want the cancellation", err)
	}
}

// cancellingWriter pulls its context the first time anything is written.
type cancellingWriter struct {
	cancel context.CancelFunc
	n      int
}

func (w *cancellingWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	w.cancel()
	return len(p), nil
}
