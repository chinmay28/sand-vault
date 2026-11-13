package vault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/chinmay28/SandHarbor/internal/compress"
	"github.com/chinmay28/SandHarbor/internal/snapshot"
	"github.com/chinmay28/SandHarbor/internal/version"
)

// Snapshots of a folder: everything under it packed into one sealed archive,
// kept on a single connected cloud or on a machine, and brought back from
// there.
//
// It is the one thing here that stores a folder somewhere *whole*. Every file
// in the vault is cut into parts that mean nothing alone and scattered so
// that no account holds enough to rebuild it; a snapshot is the opposite
// bargain, made on purpose. It is one archive in one place, readable by
// anyone holding it and its password, with no vault needed — which is what
// makes it worth having when the vault is what has been lost, and is why it
// is sealed under a password of its own rather than the vault's key. The
// account that holds it still sees only ciphertext under a name that says
// when it was taken and nothing else: internal/provider's rule that a backend
// never sees plaintext holds here as everywhere.
//
// What is in one is the tree: every folder, empty ones included, and every
// file with its bytes, its modified time and its checksum. What is not is
// anything the vault knows *about* the tree — thumbnails, film details, which
// clouds a file's parts are on, the schedule a folder is looked after on.
// Those are facts of a vault, and a snapshot is a folder.
//
// The format, the sealing and the cutting into parts are internal/snapshot.
// This file is the two ends of it: reading a folder out of the vault into
// one, and writing one back in as uploads like any other.

// The sizes a part file may be.
const (
	// SnapshotMinPartSize is the smallest limit worth asking for.
	SnapshotMinPartSize = 1 << 20

	// SnapshotCloudMaxPartSize is the largest part a cloud account is given.
	//
	// A machine takes a part as a stream and a part there can be any size. A
	// backend takes an object as one slice of bytes (provider.Put), so a part
	// bound for a cloud is held in memory whole before it is sent — and some
	// backends copy it once more to wrap it in the request they send. The
	// ceiling is what keeps a snapshot of a 200 GB folder from asking a
	// Raspberry Pi for 200 GB.
	SnapshotCloudMaxPartSize = 256 << 20

	// SnapshotCloudDefaultPartSize is what a cloud gets when nobody says.
	SnapshotCloudDefaultPartSize = 64 << 20
)

// SnapshotTarget is where a snapshot is kept: one connected cloud, or a
// folder on one machine. Exactly one of Account and Machine is set.
type SnapshotTarget struct {
	// Account is a connected account's ID. The parts go in its folder beside
	// the vault's own, under names nothing else there has.
	Account string `json:"account,omitempty"`

	// Machine is a source's ID — a machine with an SSH login, as imports and
	// exports use — and Dir the folder under its root the parts are kept in.
	// Empty is the root itself.
	Machine string `json:"machine,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

func (t SnapshotTarget) validate() error {
	switch {
	case t.Account == "" && t.Machine == "":
		return errors.New("say where the snapshot is kept: a connected cloud or a machine")
	case t.Account != "" && t.Machine != "":
		return errors.New("a snapshot is kept in one place: a connected cloud or a machine, not both")
	case t.Account != "" && t.Dir != "":
		return errors.New("a cloud keeps snapshots in its own folder; only a machine takes one to put them in")
	}
	return nil
}

// SnapshotKind says which way a snapshot job is going.
type SnapshotKind string

const (
	SnapshotTake    SnapshotKind = "take"
	SnapshotRestore SnapshotKind = "restore"
)

// The stages a snapshot job reports.
const (
	SnapshotConnecting = "connecting"
	SnapshotOpening    = "opening"
	SnapshotPacking    = "packing"
	SnapshotRestoring  = "restoring"
)

// SnapshotProgress is where a snapshot job has got to.
type SnapshotProgress struct {
	Kind  SnapshotKind `json:"kind"`
	Stage string       `json:"stage"`

	// File is the one being worked on, counted from one, of Files; Path is
	// where it is in the vault.
	File  int    `json:"file"`
	Files int    `json:"files"`
	Path  string `json:"path,omitempty"`

	// Done is how many of the folder's own bytes have been through, of Bytes.
	Done  int64 `json:"done"`
	Bytes int64 `json:"bytes"`

	// Stored is how big the snapshot's part files are so far, and Parts how
	// many have been started. Taking only.
	Stored int64 `json:"stored,omitempty"`
	Parts  int   `json:"parts,omitempty"`

	// The tallies so far. Restoring only.
	Restored int `json:"restored,omitempty"`
	Skipped  int `json:"skipped,omitempty"`
	Kept     int `json:"kept,omitempty"`
	Failed   int `json:"failed,omitempty"`
}

// SnapshotLine is one file of a restore that is worth a line: it failed, or
// it was left alone because something else is under its name.
type SnapshotLine struct {
	Path   string `json:"path"`
	Kept   bool   `json:"kept,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// SnapshotReport is what a snapshot job answers with.
type SnapshotReport struct {
	Kind SnapshotKind `json:"kind"`

	// Path is the folder snapshotted, or the folder restored into.
	Path string `json:"path"`

	// Where names the place the snapshot is kept, as a person would.
	Where string `json:"where"`

	// Name is the snapshot's, and Parts its part files.
	Name  string   `json:"name"`
	Parts []string `json:"parts,omitempty"`

	Files   int   `json:"files"`
	Folders int   `json:"folders"`
	Bytes   int64 `json:"bytes"`

	// Stored is what the part files add up to.
	Stored int64 `json:"stored"`

	// PartSize is the limit the parts were cut to, zero for none.
	PartSize int64 `json:"part_size,omitempty"`

	// What a restore did. Restored arrived; Skipped were already here, byte
	// for byte; Kept are files left alone because a different file is under
	// the name; Failed did not arrive.
	Restored int `json:"restored,omitempty"`
	Skipped  int `json:"skipped,omitempty"`
	Kept     int `json:"kept,omitempty"`
	Failed   int `json:"failed,omitempty"`

	Results []SnapshotLine `json:"results,omitempty"`
	Omitted int            `json:"omitted,omitempty"`

	// Stopped says a restore was stopped before the end of the snapshot.
	// What had arrived by then is whole, and running it again carries on.
	Stopped bool `json:"stopped,omitempty"`

	// Of is what the snapshot said it was a snapshot of. Restoring only.
	Of *snapshot.Info `json:"of,omitempty"`
}

// SnapshotPlan is what a snapshot of one folder would hold, from the index
// alone.
type SnapshotPlan struct {
	Path    string `json:"path"`
	Files   int    `json:"files"`
	Folders int    `json:"folders"`
	Bytes   int64  `json:"bytes"`

	// Unconverted counts the files still in the pre-chunking format, which
	// cannot be read as a stream. Any at all and the snapshot is refused.
	Unconverted int `json:"unconverted,omitempty"`

	// The limits a part's size is held to, for whoever is drawing the form.
	MinPartSize          int64 `json:"min_part_size"`
	CloudMaxPartSize     int64 `json:"cloud_max_part_size"`
	CloudDefaultPartSize int64 `json:"cloud_default_part_size"`

	zip *FolderZipPlan
}

// Ready reports why this snapshot cannot be taken, or nil.
func (p *SnapshotPlan) Ready() error {
	if p.Unconverted > 0 {
		return fmt.Errorf("%d of the files under %s need converting first: %w",
			p.Unconverted, p.Path, ErrNeedsConversion)
	}
	if p.Files == 0 && p.Folders == 0 {
		return fmt.Errorf("nothing to snapshot: %s is empty", p.Path)
	}
	return nil
}

// PlanSnapshot works out what a snapshot of a folder would hold.
//
// It is the folder zip's plan — the same walk, the same copies of the index's
// rows, the same refusal of a file that cannot be streamed — because it is
// the same question: everything under here, in an order worth writing it in.
func (v *Vault) PlanSnapshot(scope Scope, dir string) (*SnapshotPlan, error) {
	zip, err := v.PlanFolderZip(scope, dir)
	if err != nil {
		return nil, err
	}
	return &SnapshotPlan{
		Path:                 zip.Path,
		Files:                zip.Files,
		Folders:              zip.Folders,
		Bytes:                zip.Bytes,
		Unconverted:          zip.Unconverted,
		MinPartSize:          SnapshotMinPartSize,
		CloudMaxPartSize:     SnapshotCloudMaxPartSize,
		CloudDefaultPartSize: SnapshotCloudDefaultPartSize,
		zip:                  zip,
	}, nil
}

// SnapshotRequest is one snapshot to take.
type SnapshotRequest struct {
	Target SnapshotTarget

	// Password seals it. It is the snapshot's own and is not kept: whoever
	// restores it types it again.
	Password string

	// PartSize is the most one part file may be, in bytes. Zero is no limit
	// on a machine, and SnapshotCloudDefaultPartSize on a cloud, where there
	// is always one.
	PartSize int64

	OnProgress func(SnapshotProgress)
}

// snapshotKDFParams is what a new snapshot's password is stretched with. A
// package variable for the reason kitKDFParams is one: tests seal cheaply,
// and opening always uses the costs the snapshot itself carries.
var snapshotKDFParams = snapshot.DefaultKDF()

// partSizeFor settles the limit a snapshot bound for a place is cut to.
func partSizeFor(requested int64, cloud bool) (int64, error) {
	switch {
	case requested < 0:
		return 0, errors.New("a part's size cannot be negative")
	case requested == 0 && cloud:
		return SnapshotCloudDefaultPartSize, nil
	case requested == 0:
		return 0, nil
	case requested < SnapshotMinPartSize:
		return 0, fmt.Errorf("a part cannot be smaller than %d MB", SnapshotMinPartSize>>20)
	case cloud && requested > SnapshotCloudMaxPartSize:
		return 0, fmt.Errorf("a part kept on a cloud cannot be larger than %d MB — it is held in memory whole on its way there",
			SnapshotCloudMaxPartSize>>20)
	}
	return requested, nil
}

// TakeSnapshot packs everything under a folder into a snapshot at the target.
//
// It is all or nothing. A file that cannot be read, a part the place will not
// take, a stop — any of them ends the run with the parts already written
// erased, because a snapshot quietly missing a file is worse than no
// snapshot: it is the thing somebody will reach for when the file is gone
// everywhere else. The index is read once, at the start; a file changed
// after that is snapshotted as it was, and one deleted after that fails the
// run.
//
// Bytes go the connected accounts → this machine → the target, a chunk at a
// time. Nothing is spooled: what is held is the chunk being read, the
// compressor's window, and — on a cloud — the part being filled.
func (v *Vault) TakeSnapshot(ctx context.Context, scope Scope, dir string, req SnapshotRequest) (*SnapshotReport, error) {
	if req.Password == "" {
		return nil, errors.New("a snapshot needs a password")
	}
	plan, err := v.PlanSnapshot(scope, dir)
	if err != nil {
		return nil, err
	}
	if err := plan.Ready(); err != nil {
		return nil, err
	}

	say := func(at SnapshotProgress) {
		if req.OnProgress != nil {
			at.Kind, at.Files, at.Bytes = SnapshotTake, plan.Files, plan.Bytes
			req.OnProgress(at)
		}
	}
	say(SnapshotProgress{Stage: SnapshotConnecting})

	place, err := v.openSnapshotPlace(ctx, req.Target)
	if err != nil {
		return nil, err
	}
	defer place.close()

	partSize, err := partSizeFor(req.PartSize, place.cloud)
	if err != nil {
		return nil, err
	}

	name := path.Base(plan.Path)
	if plan.Path == "/" {
		name = "vault"
	}
	w, err := snapshot.Create(ctx, place.store, req.Password, snapshot.Info{
		Path:    plan.Path,
		Name:    name,
		TakenAt: time.Now(),
		Files:   plan.Files,
		Folders: plan.Folders,
		Bytes:   plan.Bytes,
		Tool:    "sand " + version.String(),
	}, snapshot.Options{
		PartSize: partSize,
		KDF:      snapshotKDFParams,
		Threads:  compress.Current().Threads,
	})
	if err != nil {
		return nil, err
	}

	result, err := v.packSnapshot(ctx, plan, w, say)
	if err != nil {
		// On a context of its own: the one this ran under may be why it
		// stopped, and the parts still want erasing.
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		w.Abort(cleanup)
		cancel()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the snapshot was stopped, and what had been written of it was erased")
		}
		return nil, err
	}

	return &SnapshotReport{
		Kind:     SnapshotTake,
		Path:     plan.Path,
		Where:    place.label,
		Name:     result.Name,
		Parts:    result.Parts,
		Files:    plan.Files,
		Folders:  plan.Folders,
		Bytes:    plan.Bytes,
		Stored:   result.Bytes,
		PartSize: partSize,
	}, nil
}

// packSnapshot writes a plan's folders and files into a snapshot and closes
// it.
func (v *Vault) packSnapshot(ctx context.Context, plan *SnapshotPlan, w *snapshot.Writer,
	say func(SnapshotProgress)) (snapshot.Result, error) {

	rel := func(p string) string {
		return strings.TrimPrefix(strings.TrimPrefix(p, plan.Path), "/")
	}

	// Folders first and every one of them, for the reason the zip gives: a
	// folder with nothing in it is still a folder somebody made.
	now := time.Now()
	for _, f := range plan.zip.folders {
		if err := w.AddFolder(rel(f), now); err != nil {
			return snapshot.Result{}, fmt.Errorf("packing the folder %s: %w", f, err)
		}
	}

	var before int64
	for i := range plan.zip.entries {
		e := &plan.zip.entries[i]
		if err := ctx.Err(); err != nil {
			return snapshot.Result{}, err
		}
		at := SnapshotProgress{Stage: SnapshotPacking, File: i + 1, Path: e.Path()}
		report := func(done int64) {
			at.Done, at.Stored, at.Parts = before+done, w.Written(), w.Parts()
			say(at)
		}
		report(0)

		body, _, err := v.OpenSequential(ctx, e.ID)
		if err != nil {
			return snapshot.Result{}, fmt.Errorf("opening %s: %w", e.Path(), err)
		}
		err = w.AddFile(snapshot.File{
			Path:   rel(e.Path()),
			Size:   e.Size,
			Mod:    e.ModifiedAt,
			SHA256: e.Hash,
		}, &progressReader{r: body, say: report})
		if err != nil {
			return snapshot.Result{}, fmt.Errorf("packing %s: %w", e.Path(), err)
		}
		before += e.Size
	}

	result, err := w.Close()
	if err != nil {
		return snapshot.Result{}, fmt.Errorf("finishing the snapshot: %w", err)
	}
	return result, nil
}

// ListSnapshots answers with the snapshots a place holds, newest first, read
// from the names of the files there. An interrupted one is listed too, marked
// incomplete: it is taking up room, and this is where it is found.
func (v *Vault) ListSnapshots(ctx context.Context, target SnapshotTarget) ([]snapshot.Set, error) {
	place, err := v.openSnapshotPlace(ctx, target)
	if err != nil {
		return nil, err
	}
	defer place.close()

	sets, err := snapshot.List(ctx, place.store)
	if err != nil {
		return nil, err
	}
	if sets == nil {
		sets = []snapshot.Set{}
	}
	return sets, nil
}

// InspectSnapshot opens one snapshot far enough to say what it holds — which
// is also how a password is tried before a restore is started on it.
func (v *Vault) InspectSnapshot(ctx context.Context, target SnapshotTarget, name, password string) (*snapshot.Info, error) {
	place, err := v.openSnapshotPlace(ctx, target)
	if err != nil {
		return nil, err
	}
	defer place.close()

	set, err := snapshot.Find(ctx, place.store, name)
	if err != nil {
		return nil, err
	}
	r, err := snapshot.Open(ctx, place.store, set, password)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	info := r.Info()
	return &info, nil
}

// DeleteSnapshot erases every part of one snapshot from where it is kept.
func (v *Vault) DeleteSnapshot(ctx context.Context, target SnapshotTarget, name string) error {
	place, err := v.openSnapshotPlace(ctx, target)
	if err != nil {
		return err
	}
	defer place.close()

	set, err := snapshot.Find(ctx, place.store, name)
	if err != nil {
		return err
	}
	return snapshot.Remove(ctx, place.store, set)
}

// RestoreRequest is one snapshot to bring back.
type RestoreRequest struct {
	Target SnapshotTarget

	// Name is the snapshot's, as a listing gives it.
	Name     string
	Password string

	// Replace writes a snapshot's file over a different file already under
	// its name. Without it such a file is left alone and reported.
	Replace bool

	OnProgress func(SnapshotProgress)
}

// RestoreSnapshot unpacks a snapshot into a folder of this vault.
//
// The snapshot's tree lands *in* dest: a snapshot of /photos restored into
// /photos puts everything back where it was, and restored into /old it
// becomes /old/2019/... — the folder that was snapshotted is not recreated
// by name, so restoring where it came from does not nest it inside itself.
//
// Every file arrives as an upload like any other: compressed, cut, sealed
// under this vault's key and scattered over its default clouds. Nothing of
// where the file's parts used to be is in a snapshot, and nothing of it is
// wanted — the vault restoring it may not be the vault that took it.
//
// # What is already here
//
// A file already at its place with the same bytes is passed over, which is
// what makes a restore safe to repeat and is the whole of how one resumes:
// run it again. A *different* file under the name is left alone and reported
// unless Replace is set; nothing is ever stored beside it under a numbered
// name, because a restore that scattered "report (2).pdf" through a tree
// would leave it less like the snapshot than it found it.
//
// One file failing does not stop the rest. A snapshot that turns out damaged
// partway does: what was restored before that point is whole and stays, and
// the error says how far it got.
func (v *Vault) RestoreSnapshot(ctx context.Context, scope Scope, dest string, req RestoreRequest) (*SnapshotReport, error) {
	say := func(at SnapshotProgress) {
		if req.OnProgress != nil {
			at.Kind = SnapshotRestore
			req.OnProgress(at)
		}
	}
	say(SnapshotProgress{Stage: SnapshotConnecting})

	place, err := v.openSnapshotPlace(ctx, req.Target)
	if err != nil {
		return nil, err
	}
	defer place.close()

	set, err := snapshot.Find(ctx, place.store, req.Name)
	if err != nil {
		return nil, err
	}
	say(SnapshotProgress{Stage: SnapshotOpening})
	r, err := snapshot.Open(ctx, place.store, set, req.Password)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	info := r.Info()

	made := map[string]bool{}
	if err := v.ensureFolder(scope, dest, made); err != nil {
		return nil, err
	}
	dest, err = v.destinationLocked(scope, dest)
	if err != nil {
		return nil, err
	}

	report := &SnapshotReport{
		Kind:   SnapshotRestore,
		Path:   dest,
		Where:  place.label,
		Name:   set.Name,
		Parts:  set.Parts,
		Stored: set.Bytes,
		Of:     &info,
	}
	var (
		lines transferLines[SnapshotLine]
		done  int64
	)
	finish := func() *SnapshotReport {
		report.Results, report.Omitted = lines.lines, lines.omitted
		return report
	}

	for {
		if ctx.Err() != nil {
			report.Stopped = true
			return finish(), nil
		}
		item, err := r.Next()
		if err == io.EOF {
			return finish(), nil
		}
		if err != nil {
			if ctx.Err() != nil {
				report.Stopped = true
				return finish(), nil
			}
			return nil, fmt.Errorf("%w — %d of its %d files had been restored before that, and are whole",
				err, report.Restored+report.Skipped, info.Files)
		}

		full := path.Join(dest, item.Path)
		if item.Dir {
			if err := v.ensureFolder(scope, full, made); err != nil {
				lines.add(SnapshotLine{Path: full, Error: err.Error()})
			} else {
				report.Folders++
			}
			continue
		}

		report.Files++
		at := SnapshotProgress{
			Stage: SnapshotRestoring, File: report.Files, Files: info.Files, Path: full,
			Done: done, Bytes: info.Bytes,
			Restored: report.Restored, Skipped: report.Skipped, Kept: report.Kept, Failed: report.Failed,
		}
		say(at)

		line, outcome := v.restoreOne(ctx, scope, full, item, req.Replace, func(n int64) {
			at.Done = done + n
			say(at)
		})
		done += item.Size
		report.Bytes += item.Size
		switch outcome {
		case restoreArrived:
			report.Restored++
		case restoreSame:
			report.Skipped++
		case restoreKept:
			report.Kept++
			lines.add(line)
		default:
			// A file that failed because the run was stopped under it is not
			// a failure of the file.
			if ctx.Err() != nil {
				report.Files--
				report.Bytes -= item.Size
				report.Stopped = true
				return finish(), nil
			}
			report.Failed++
			lines.add(line)
		}
	}
}

// restoreOutcome is what became of one file of a restore.
type restoreOutcome int

const (
	restoreFailed restoreOutcome = iota
	restoreArrived
	restoreSame
	restoreKept
)

// restoreOne puts one file of a snapshot in the vault, or says why not.
func (v *Vault) restoreOne(ctx context.Context, scope Scope, full string, item *snapshot.Item,
	replace bool, report func(int64)) (SnapshotLine, restoreOutcome) {

	line := SnapshotLine{Path: full}
	dir, name := path.Dir(full), path.Base(full)

	here := v.heldAt(scope, full, item)
	switch {
	case here == heldSame:
		return line, restoreSame
	case here == heldDifferent && !replace:
		line.Kept = true
		line.Reason = "a different file is already here under this name — tick Replace to write the snapshot's over it"
		return line, restoreKept
	}

	if err := v.ensureFolder(scope, dir, map[string]bool{}); err != nil {
		line.Error = err.Error()
		return line, restoreFailed
	}
	_, _, err := v.UploadStream(ctx, scope, dir, name,
		&progressReader{r: item.Body, say: report},
		UploadOptions{Overwrite: here == heldDifferent, ModifiedAt: item.Mod})
	if err != nil {
		line.Error = err.Error()
		return line, restoreFailed
	}
	return line, restoreArrived
}

// held is what the vault has at a path a snapshot wants to restore to.
type held int

const (
	heldNothing held = iota
	heldSame
	heldDifferent
)

// heldAt compares a snapshot's file with whatever is at its place.
//
// The same file is the same length with the same checksum. Where either side
// has no checksum on record — a file stored before checksums were kept, and
// never read through since — the modified time stands in for it, since a
// restore stamps the file with the time the snapshot carried.
func (v *Vault) heldAt(scope Scope, full string, item *snapshot.Item) held {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return heldNothing
	}
	entry := m.ByPath(full)
	if entry == nil {
		return heldNothing
	}
	if entry.Size != item.Size {
		return heldDifferent
	}
	if entry.Hash != "" && item.SHA256 != "" {
		if entry.Hash == item.SHA256 {
			return heldSame
		}
		return heldDifferent
	}
	if SameModTime(entry.ModifiedAt, item.Mod) {
		return heldSame
	}
	return heldDifferent
}
