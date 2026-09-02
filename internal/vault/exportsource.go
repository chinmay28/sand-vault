package vault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	sandsftp "github.com/chinmay28/sand-vault/internal/sftp"
)

// Sending files back out to a machine, which is the one direction this package
// writes plaintext anywhere but a spool.
//
// Everything else SAND puts on somebody else's disk is a shard: encrypted,
// meaningless alone, under a name it invented. An export is the opposite on
// every count — the user's own files, whole and readable, under the names they
// carry in the vault, onto a folder the user picked. That asymmetry is the
// point of the feature and the thing to keep in view while changing it: the
// far end holds the clear text afterwards, and nothing here can make that
// otherwise. The UI says so out loud, and so does docs/sftp.md.
//
// What it shares with the import is the shape. A selection is planned from the
// index before a byte moves, files go one at a time, each lands whole or not at
// all, and re-running the same export is how an interrupted one resumes.

// ExportRequest is one push from a folder of this vault onto a source.
type ExportRequest struct {
	// Paths are what was picked, as paths in the vault: files, folders, or
	// both. A folder brings everything under it, keeping its shape — picking
	// /photos puts /photos/2019/one.jpg at <Dest>/photos/2019/one.jpg. The
	// root itself is allowed and puts its contents straight into Dest.
	Paths []string

	// Dest is the folder on the machine they land in, relative to the
	// source's root and made if it is not there. Empty is the root itself.
	Dest string

	// Overwrite replaces a file already at the name. Without it, a file that
	// is there is left alone and reported: as already exported when it is the
	// same file, and as in the way when it is not.
	Overwrite bool

	// OnProgress, when set, is called as files move — see TransferProgress.
	// It is called from the goroutine running the export and must not block.
	OnProgress func(TransferProgress)
}

// ExportResult is what became of one file that is worth a line: it failed,
// or it was left alone for a reason a second run will not clear. A partial
// export reads as what it is because every file that did not simply go says
// so; the ones that did are counted.
type ExportResult struct {
	// Path is the file in the vault; Dest is where it went on the machine,
	// relative to the source's root.
	Path string `json:"path"`
	Dest string `json:"dest"`
	Size int64  `json:"size"`

	OK      bool `json:"ok,omitempty"`
	Skipped bool `json:"skipped,omitempty"`

	// Reason says why a file was skipped, and is empty otherwise.
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// worthALine says whether this result is one the summary lists. A file that
// went, or that was already there as the same file, is counted instead; a
// file skipped for any other reason is listed, because that is the one skip
// a second run will not resolve and the person has to be told about.
func (r ExportResult) worthALine() bool {
	return r.Error != "" || (r.Skipped && r.Reason != alreadyThere)
}

// ExportSummary is the whole request's outcome: the counts, and a line for
// every file worth one.
type ExportSummary struct {
	// Results holds the files that failed and the files left alone for a
	// reason Replace would have to clear, in the order they were reached, up
	// to maxTransferLines of them. Files that went and files already there
	// are in the counts below and nowhere else.
	Results  []ExportResult `json:"results"`
	Exported int            `json:"exported"`
	Skipped  int            `json:"skipped"`
	Failed   int            `json:"failed"`

	// Bytes is how much was actually written, skipped files excluded.
	Bytes int64 `json:"bytes"`

	// Omitted counts the lines Results had no room for. The counts above are
	// whole regardless; this says only that not every line is listed.
	Omitted int `json:"omitted,omitempty"`
}

// exportFile is one file the plan found: a copy of its index record, and
// where it goes on the machine.
type exportFile struct {
	entry  Entry
	remote string // relative to the source's root
}

// ExportToSource writes files out of this vault onto a source.
//
// # Resume
//
// A file already on the machine at its destination, the same size, and not
// older than the copy in the vault, is not sent again. That is the whole of
// the resume story, exactly as it is for imports: **re-running an export is
// how you resume it.** It is sound for the same reason the import's rule is —
// a file is written under a temporary name and renamed into place only once
// every byte is there (see sftp.WriteUnder), so the machine holds either the
// whole file or nothing under that name, and "is it already there" has a real
// answer a size can reach. The modification time, stamped from the vault's
// own record, guards the one case a size alone gets wrong: a file replaced in
// the vault by a different one of the same length.
//
// A file that is there and is *not* the same one is left alone and reported,
// unless Overwrite was asked for. Nothing here clobbers by default: the far
// end is the user's own folder, and a name that is taken is more likely to be
// something they want than something to write over.
//
// # Direction
//
// Bytes go the connected accounts → this machine → the source, gathered a chunk
// at a time and written as they arrive. Nothing is spooled and nothing passes
// through the browser, so a 40 GB film costs the chunk window rather than
// 40 GB — which is what lets this run on a machine with a few gigabytes to its
// name. A file still in the pre-chunking format cannot be read that way, and
// is reported as needing conversion rather than rebuilt in memory.
func (v *Vault) ExportToSource(ctx context.Context, scope Scope, id string, req ExportRequest) (ExportSummary, error) {
	source, err := v.source(id)
	if err != nil {
		return ExportSummary{}, err
	}

	// The destination is checked against the root before anything is
	// planned or dialled, in the same spirit planImport refuses a bad path
	// before a byte moves: this is where the answer can still be "no" to the
	// whole request rather than an error attached to every file.
	dest := sandsftp.CleanPath(strings.TrimPrefix(strings.TrimSpace(req.Dest), "/"))
	if _, err := sandsftp.Under(source.Root, dest); err != nil {
		return ExportSummary{}, err
	}

	files, err := v.planExport(scope, req.Paths, dest)
	if err != nil {
		return ExportSummary{}, err
	}

	client, err := sandsftp.Dial(ctx, source.dialConfig())
	if err != nil {
		return ExportSummary{}, err
	}
	defer client.Close()

	// Made up front, along with anything above it, so an export aimed at a
	// folder that is not there yet does not need a step first — the same
	// courtesy an import extends to its vault folder.
	if _, err := client.MkdirUnder(source.Root, dest); err != nil {
		return ExportSummary{}, err
	}

	var (
		summary ExportSummary
		lines   transferLines[ExportResult]
	)
	for i, f := range files {
		// Fixed for this file and filled in as it moves. The tallies are of
		// the files before this one, so what the bar says mid-flight is what
		// the summary would say if it stopped here.
		at := TransferProgress{
			File: i + 1, Files: len(files),
			Path: f.entry.Path(), Dest: f.remote, Name: f.entry.Name,
			Size:      f.entry.Size,
			Completed: summary.Exported, Skipped: summary.Skipped, Failed: summary.Failed,
		}
		var report func(TransferStage, int64)
		if req.OnProgress != nil {
			report = func(stage TransferStage, done int64) {
				at.Stage, at.Done = stage, done
				req.OnProgress(at)
			}
		}

		result, sent := v.exportOne(ctx, client, source.Root, f, req, report)
		switch {
		case result.OK:
			summary.Exported++
			summary.Bytes += sent
		case result.Skipped:
			summary.Skipped++
		default:
			summary.Failed++
		}
		if result.worthALine() {
			lines.add(result)
		}

		// A cancelled request stops here rather than working through the rest
		// with a dead context and reporting a failure per file. What landed is
		// whole, and re-running resumes.
		if ctx.Err() != nil {
			break
		}
	}
	summary.Results, summary.Omitted = lines.lines, lines.omitted
	return summary, nil
}

// exportOne sends a single file, or says why it did not, and reports how many
// bytes it wrote.
func (v *Vault) exportOne(ctx context.Context, client *sandsftp.Client, root string,
	f exportFile, req ExportRequest, report func(TransferStage, int64)) (ExportResult, int64) {

	result := ExportResult{Path: f.entry.Path(), Dest: f.remote, Size: f.entry.Size}

	// A file stored whole has no seams to read it through, and rebuilding it
	// in memory is exactly what this path exists not to do. Convert is the
	// answer, and the line says so.
	if !f.entry.Chunked() {
		result.Error = fmt.Sprintf("%s: %v", f.entry.Path(), ErrNeedsConversion)
		return result, 0
	}

	if !req.Overwrite {
		skip, why, err := alreadyExported(client, root, f)
		if err != nil {
			result.Error = err.Error()
			return result, 0
		}
		if skip {
			result.Skipped, result.Reason = true, why
			return result, 0
		}
	}

	// Said before the first byte moves, so the file being worked on appears
	// the moment it is picked up rather than once enough of it has gone to
	// report.
	if report != nil {
		report(StageSending, 0)
	}

	body, _, err := v.OpenSequential(ctx, f.entry.ID)
	if err != nil {
		result.Error = err.Error()
		return result, 0
	}

	// The reader is counted rather than the writer, on purpose: the SFTP
	// file's own ReadFrom is what keeps many packets in flight, and wrapping
	// the far end would hide it. See sftp.WriteUnder.
	var src io.Reader = body
	if report != nil {
		src = &progressReader{r: body, say: func(done int64) { report(StageSending, done) }}
	}

	n, err := client.WriteUnder(root, f.remote, src, sandsftp.WriteOptions{
		Overwrite: req.Overwrite,
		ModTime:   f.entry.ModifiedAt,
	})
	if err != nil {
		// Appeared between the check above and the write: somebody else's
		// file, and left alone for the same reason.
		if errors.Is(err, sandsftp.ErrExists) {
			result.Skipped, result.Reason = true, inTheWay
			return result, 0
		}
		result.Error = err.Error()
		return result, n
	}
	if n != f.entry.Size {
		result.Error = fmt.Sprintf("sent %d of %d bytes", n, f.entry.Size)
		return result, n
	}

	result.OK = true
	return result, n
}

// inTheWay is why a file was left alone: something else is already under
// its name.
const inTheWay = "a different file is already there under this name — tick Replace to overwrite it"

// alreadyThere is why a file was left alone when nothing is wrong: the same
// file is on the machine already. The one skip that is a count and not a line.
const alreadyThere = "already there"

// alreadyExported reports whether this file is on the machine already, and
// what to say about it. See ExportToSource for why a size and a time are
// enough.
func alreadyExported(client *sandsftp.Client, root string, f exportFile) (bool, string, error) {
	info, err := client.StatUnder(root, f.remote)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "", nil
		}
		// A link pointing out of the root, a folder that cannot be read: not
		// a file to skip, and not one to write over either.
		return false, "", err
	}
	if info.IsDir() {
		return false, "", fmt.Errorf("a folder is in the way at %s", f.remote)
	}
	if info.Size() != f.entry.Size {
		return true, inTheWay, nil
	}
	// SFTP keeps whole seconds, so the vault's own time is compared at the
	// same resolution — otherwise a file stamped a moment ago would read as
	// older than itself.
	if info.ModTime().Before(f.entry.ModifiedAt.Truncate(time.Second)) {
		return true, "the copy there is older than the one in the vault — tick Replace to overwrite it", nil
	}
	return true, alreadyThere, nil
}

// planExport expands a selection into the list of files to send, walking any
// folder in it, from the index alone.
//
// Under one read lock rather than a lookup per path, so the plan is of one
// moment of the index — and copied out of it, so that a rename or a delete
// while the export runs changes the index and not the plan.
func (v *Vault) planExport(scope Scope, paths []string, dest string) ([]exportFile, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}

	var (
		files []exportFile
		seen  = map[string]bool{}
	)
	add := func(e *Entry, remote string) {
		if seen[e.ID] {
			// Picking a folder and a file inside it is an easy thing to do in
			// a list with checkboxes, and sending it twice would be a numbered
			// copy on the far end or a refusal, depending on the server.
			return
		}
		seen[e.ID] = true
		snapshot := *e
		snapshot.Shards = append([]Shard(nil), e.Shards...)
		files = append(files, exportFile{entry: snapshot, remote: remote})
	}

	for _, raw := range paths {
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("cannot export by naming nothing: pick what to send")
		}
		p := CleanDir(raw)

		if e := m.ByPath(p); e != nil {
			add(e, path.Join(dest, e.Name))
			continue
		}
		if !m.FolderExists(p) {
			return nil, fmt.Errorf("no such file or folder: %s", p)
		}

		// The folder itself becomes a folder on the machine, so its shape
		// survives the trip — except the root, which has no name to become,
		// and whose contents go straight into the destination.
		base := path.Base(p)
		if p == "/" {
			base = ""
		}
		under := m.Descendants(p)
		sort.Slice(under, func(i, j int) bool { return under[i].Path() < under[j].Path() })
		for _, e := range under {
			rel := strings.TrimPrefix(strings.TrimPrefix(e.Dir, p), "/")
			add(e, path.Join(dest, base, rel, e.Name))
		}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("nothing to export: the selection holds no files")
	}
	return files, nil
}
