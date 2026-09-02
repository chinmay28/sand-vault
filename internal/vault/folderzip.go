package vault

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// A folder handed back as one archive.
//
// Downloading a file rebuilds it from its parts; downloading a folder used to
// mean doing that once per file, each into its own browser download. This
// does it as one zip, streamed: the archive is written to whoever asked as
// it is assembled, each file gathered from the clouds a chunk at a time and
// copied straight into it. At no point does more than a chunk window of the
// folder exist on this machine — which is what lets a 200 GB folder leave a
// Raspberry Pi with 4 GB, and why the zip is *stored* rather than deflated.
// The files were compressed with zstd before they were ever split, so there
// is nothing left for deflate to find, and on a Pi the CPU it would spend
// finding that out is the ceiling on how fast the archive leaves.
//
// The plan is read from the index alone and the refusals are made on it,
// before a byte is written: a folder with nothing in it, or a file still in
// the pre-chunking format, which cannot be read at an offset and is not going
// to be rebuilt in memory to be put in a zip. Once the archive has started
// there is no way to say no — a 200 in flight cannot become a 409 — so
// everything that can be said no to is said before.

// FolderZipPlan is what one folder's archive would hold.
type FolderZipPlan struct {
	Path string `json:"path"`

	// Name is what the archive is called — the folder's own name, or "vault"
	// for the root — and every path inside it is rooted at that name, so
	// unpacking it yields a folder rather than a heap.
	Name string `json:"name"`

	Files   int   `json:"files"`
	Folders int   `json:"folders"`
	Bytes   int64 `json:"bytes"`

	// Unconverted counts the files under here still stored in the
	// pre-chunking format. Any at all and the archive is refused — see Ready.
	Unconverted int `json:"unconverted,omitempty"`

	// The files and the folders, in the order they are written. Not
	// serialized: the plan is what the browser is told, and ten thousand
	// paths are not what it needs to hear.
	entries []Entry
	folders []string
}

// Ready reports why this archive cannot be built, or nil.
func (p *FolderZipPlan) Ready() error {
	if p.Unconverted > 0 {
		return fmt.Errorf("%d of the files under %s need converting first: %w",
			p.Unconverted, p.Path, ErrNeedsConversion)
	}
	if p.Files == 0 {
		return fmt.Errorf("nothing to download: %s holds no files", p.Path)
	}
	return nil
}

// PlanFolderZip works out what an archive of a folder would hold, from the
// index alone.
func (v *Vault) PlanFolderZip(scope Scope, dir string) (*FolderZipPlan, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}
	dir = CleanDir(dir)
	if !m.FolderExists(dir) {
		return nil, fmt.Errorf("no such folder: %s", dir)
	}

	name := path.Base(dir)
	if dir == "/" {
		name = "vault"
	}
	plan := &FolderZipPlan{Path: dir, Name: name}

	for _, f := range m.AllFolders() {
		if below(f, dir) {
			plan.folders = append(plan.folders, f)
		}
	}
	sort.Strings(plan.folders)
	plan.Folders = len(plan.folders)

	// Copied out of the index, so a rename or a delete while the archive
	// streams changes the index and not the archive.
	for _, e := range m.Descendants(dir) {
		snapshot := *e
		snapshot.Shards = append([]Shard(nil), e.Shards...)
		plan.entries = append(plan.entries, snapshot)
		plan.Files++
		plan.Bytes += e.Size
		if !e.Chunked() {
			plan.Unconverted++
		}
	}
	sort.Slice(plan.entries, func(i, j int) bool {
		return plan.entries[i].Path() < plan.entries[j].Path()
	})
	return plan, nil
}

// WriteFolderZip streams a folder to w as a zip archive.
//
// The context is the reader's: a browser that gives up halfway stops the
// gathers it started, rather than leaving them running against three clouds
// for a download nobody will receive.
func (v *Vault) WriteFolderZip(ctx context.Context, scope Scope, dir string, w io.Writer) error {
	plan, err := v.PlanFolderZip(scope, dir)
	if err != nil {
		return err
	}
	if err := plan.Ready(); err != nil {
		return err
	}
	return v.writeZip(ctx, plan, w)
}

// writeZip is the archive itself, built from a plan already checked.
func (v *Vault) writeZip(ctx context.Context, plan *FolderZipPlan, w io.Writer) error {
	// Everything inside the archive hangs off the folder's own name.
	inside := func(p string) string {
		rel := strings.TrimPrefix(strings.TrimPrefix(p, plan.Path), "/")
		return path.Join(plan.Name, rel)
	}

	zw := zip.NewWriter(w)
	now := time.Now()

	// Folders first, and every folder rather than only the ones with files
	// in them, so an empty folder survives the trip — it is a folder the
	// user made, and an archive that lost it would not be the folder.
	for _, f := range plan.folders {
		if err := ctx.Err(); err != nil {
			return err
		}
		header := &zip.FileHeader{Name: inside(f) + "/", Method: zip.Store, Modified: now}
		header.SetMode(0o755)
		if _, err := zw.CreateHeader(header); err != nil {
			return fmt.Errorf("writing the folder %s: %w", f, err)
		}
	}

	for i := range plan.entries {
		e := &plan.entries[i]
		if err := ctx.Err(); err != nil {
			return err
		}

		header := &zip.FileHeader{
			Name:               inside(e.Path()),
			Method:             zip.Store,
			Modified:           e.ModifiedAt,
			UncompressedSize64: uint64(e.Size),
		}
		header.SetMode(0o644)
		fw, err := zw.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("writing %s: %w", e.Path(), err)
		}

		// Straight from the vault into the archive, one pass and nothing
		// kept: each read costs the chunk it is in and nothing more.
		body, _, err := v.OpenSequential(ctx, e.ID)
		if err != nil {
			return fmt.Errorf("opening %s: %w", e.Path(), err)
		}
		n, err := io.Copy(fw, body)
		if err != nil {
			return fmt.Errorf("reading %s: %w", e.Path(), err)
		}
		if n != e.Size {
			return fmt.Errorf("%s: read %d of %d bytes", e.Path(), n, e.Size)
		}
	}

	if err := zw.Close(); err != nil {
		return fmt.Errorf("finishing the archive: %w", err)
	}
	return nil
}
