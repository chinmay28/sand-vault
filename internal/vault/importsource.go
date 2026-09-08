package vault

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	sandsftp "github.com/chinmay28/sand-vault/internal/sftp"
)

// maxImportDepth bounds how deep the walk under a selected folder goes.
//
// It is the one bound on a selection. There is no cap on how many files a
// folder may bring: a person picking "my media drive" does not know what is
// under it, and an import that stopped at a round number and asked to be run
// again was a job done in instalments. What an unbounded selection costs is
// held elsewhere — the walk keeps a few words per file, the transfer holds
// one file at a time, and the summary is bounded on its own terms; see
// transferlines.go.
const maxImportDepth = 32

// ImportRequest is one pull from a source into a folder of this vault.
type ImportRequest struct {
	// Paths are what was selected, relative to the source's root. A folder
	// brings everything under it, keeping its shape: selecting "films" puts
	// films/2019/one.mp4 at <Dest>/films/2019/one.mp4.
	Paths []string

	// Dest is the vault folder they land in.
	Dest string

	// Accounts and Scheme are the upload's own choices, exactly as they are for
	// a file arriving from a browser.
	Accounts []string
	Scheme   archive.Scheme

	// Overwrite replaces a file of the same name rather than storing this one
	// beside it under a numbered name. It also overrides the skip below: a
	// deliberate re-import is how somebody says "fetch it again anyway".
	Overwrite bool

	// OnProgress, when set, is called as files move, so a caller holding the
	// request open can say where it is. It is called from the goroutine running
	// the import and must not block: see TransferProgress for what it carries and
	// why it is a view of a running request rather than state anybody keeps.
	OnProgress func(TransferProgress)
}

// ImportResult is what became of one file that is worth a line: it failed, or
// it arrived with a warning. A partial import is legible because every file
// that did not simply arrive says so — the same bargain the browser upload
// handler strikes, minus the lines nobody reads.
type ImportResult struct {
	// Path is the file on the source, relative to its root.
	Path string `json:"path"`

	// Dest is where it went, or would have gone, in the vault.
	Dest string `json:"dest"`

	OK      bool `json:"ok,omitempty"`
	Skipped bool `json:"skipped,omitempty"`

	// Retimed says the file was already here and nothing was fetched, but the
	// copy here was stamped with the day it was imported rather than with the
	// file's own time, and that has been put right. See Retime.
	Retimed bool `json:"retimed,omitempty"`

	// Reason says why a file was skipped or retimed, and is empty otherwise.
	Reason string `json:"reason,omitempty"`

	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// worthALine says whether this result is one the summary lists. A file that
// arrived cleanly, or was already here, is counted instead: on a selection of
// two hundred thousand files those are the lines, and nobody reads them.
func (r ImportResult) worthALine() bool {
	return r.Error != "" || len(r.Warnings) > 0
}

// ImportSummary is the whole request's outcome: the counts, and a line for
// every file worth one.
type ImportSummary struct {
	// Results holds the files that failed and the files that arrived with a
	// warning, in the order they were reached, up to maxTransferLines of
	// them. Files that arrived cleanly and files already here are in the
	// counts below and nowhere else.
	Results  []ImportResult `json:"results"`
	Imported int            `json:"imported"`
	Skipped  int            `json:"skipped"`
	Failed   int            `json:"failed"`

	// Retimed counts the files that were already here and had their
	// modification time corrected to the source's — no bytes moved for any of
	// them. It is its own count rather than part of Skipped because it is the
	// one kind of "already there" that changed something.
	Retimed int `json:"retimed,omitempty"`

	// Omitted counts the lines Results had no room for. The counts above are
	// whole regardless; this says only that not every failure is listed.
	Omitted int `json:"omitted,omitempty"`
}

// importFile is one file the walk found: where it is on the source, and where
// it goes in the vault.
type importFile struct {
	remote string // relative to the source root
	dir    string // vault folder it lands in
	name   string
	size   int64
	mod    time.Time
}

// ImportFromSource pulls files off a source into a folder of this vault.
//
// # Resume
//
// A file already in the vault at its destination, the same size, and not
// touched on the source since it was imported, is not fetched again. That one
// rule is the whole of the resume story: **re-running an import is how you
// resume it.** There is no job state, nothing to reconcile, and no partial
// transfer to validate — kill the server in the middle and the next run is
// correct.
//
// It is sound rather than a heuristic, which is worth spelling out because the
// same rule elsewhere usually is a heuristic. UploadStream spools the whole
// file, scatters it, and only then commits an entry, so an interrupted import
// leaves *no* entry rather than half of one. The vault therefore holds either
// the complete file or nothing at all, and "is it already there" has a real
// answer that a size comparison can reach without transferring anything.
//
// The modification time is the guard on the one case a size alone would get
// wrong: a file replaced on the source by a different file of the same length.
// A source file newer than the import is fetched again rather than assumed.
//
// # Times
//
// A file keeps the modification time it had on the source, so a folder of
// photographs arrives dated when the photographs were taken rather than when
// the import ran. Files imported before that was true are put right on a
// re-run: a file already here, the same size, and untouched on the source since
// it was imported, has its recorded time corrected to the source's if the two
// disagree — no bytes move, and it is counted as retimed rather than skipped.
// A file whose times already agree is skipped in silence, as it always was.
// The corrections are gathered and written in runs rather than one at a time,
// because the index is sealed and written whole; see importRetimes.
//
// The granularity is worth being blunt about, because it is what the dialog has
// to say out loud: **a file resumes, a transfer does not.** Interrupting an
// 18 GB film halfway leaves no entry and no spool, and the next run starts it
// again from the first byte. That is the cost of committing whole files only,
// and it buys the property above — the vault holds the whole file or nothing,
// never something that has to be checked before it can be trusted. It is also
// why OnProgress exists: on a selection of one very large file, "nothing is
// lost" and "nothing is happening" used to look identical from outside.
//
// # Direction
//
// Bytes go source → this machine → the connected accounts. They are compressed,
// split, encrypted and scattered by the same code any other upload goes
// through, and they never pass through the browser.
func (v *Vault) ImportFromSource(ctx context.Context, scope Scope, id string, req ImportRequest) (ImportSummary, error) {
	client, source, err := v.connectSource(ctx, id)
	if err != nil {
		return ImportSummary{}, err
	}
	defer client.Close()

	// The destination is made if it is not there, along with anything above it.
	// An import names where it should land rather than picking from what
	// already exists, so requiring the folder first would mean creating it by
	// hand before every first import into a new place.
	made := map[string]bool{}
	if err := v.ensureFolder(scope, req.Dest, made); err != nil {
		return ImportSummary{}, err
	}
	dest, err := v.destinationLocked(scope, req.Dest)
	if err != nil {
		return ImportSummary{}, err
	}

	// The walk, said out loud as it goes. It is one round trip per folder and
	// it happens before any file has a name, so on a big selection it is a long
	// silence right where somebody is watching hardest — a count of what has
	// been found so far is the only thing there is to say, and it is enough to
	// tell a walk in progress from a hang.
	var found func(int)
	if req.OnProgress != nil {
		found = func(n int) {
			req.OnProgress(TransferProgress{Stage: StagePlanning, Files: n})
		}
	}
	files, err := planImport(client, source.Root, req.Paths, dest, found)
	if err != nil {
		return ImportSummary{}, err
	}

	var (
		summary ImportSummary
		lines   transferLines[ImportResult]
		retimes = importRetimes{v: v, scope: scope}
	)

	// A batch of times that could not be written is not a batch of files that
	// were retimed: Retime put the entries back as they were, so what was
	// counted as retimed has to become a failure with a line saying why.
	settle := func(failed []ImportResult) {
		for _, result := range failed {
			summary.Retimed--
			summary.Failed++
			lines.add(result)
		}
	}

	// Every folder the files hang off, made once each and before anything is
	// fetched. A folder that will not be made fails every file under it on its
	// own line, which is what says how much of the selection actually arrived.
	made[dest] = true
	for i, f := range files {
		if err := v.ensureFolder(scope, f.dir, made); err != nil {
			lines.add(ImportResult{
				Path:  f.remote,
				Dest:  path.Join(f.dir, f.name),
				Error: err.Error(),
			})
			summary.Failed++
			continue
		}

		// Fixed for this file, and filled in with a stage and a count as it
		// moves. The tallies are of the files before this one, so what the bar
		// says mid-flight is what the summary would say if it stopped here.
		at := TransferProgress{
			File: i + 1, Files: len(files),
			Path: f.remote, Dest: path.Join(f.dir, f.name), Name: f.name,
			Size:      f.size,
			Completed: summary.Imported, Skipped: summary.Skipped,
			Retimed: summary.Retimed, Failed: summary.Failed,
		}
		var report func(TransferStage, int64)
		if req.OnProgress != nil {
			report = func(stage TransferStage, done int64) {
				at.Stage, at.Done = stage, done
				req.OnProgress(at)
			}
			// Named before it is decided, not only before it is fetched. Asking
			// whether one file is already here is quick; asking it twenty
			// thousand times over an SFTP link is not, and until this was said
			// a re-import — where that is the entire job — reported nothing
			// from beginning to end and was indistinguishable from a hang.
			report(StageChecking, 0)
		}

		result := v.importOne(ctx, scope, client, source, f, req, report)
		switch {
		case result.OK:
			summary.Imported++
		case result.Retimed:
			summary.Retimed++
			retimes.add(FileTime{Path: result.Dest, Mod: f.mod}, result)
			settle(retimes.flushIfFull())
		case result.Skipped:
			summary.Skipped++
		default:
			summary.Failed++
		}
		if result.worthALine() {
			lines.add(result)
		}

		// A cancelled request stops here rather than working through the rest
		// of the selection with a dead context and reporting a failure per
		// file. What arrived is already committed, and re-running resumes.
		if ctx.Err() != nil {
			break
		}
	}

	// Whatever the last run of times has not landed yet, landing now: a
	// cancelled import flushes what it gathered before it stopped, for the same
	// reason it keeps the files that arrived.
	settle(retimes.flush())

	summary.Results, summary.Omitted = lines.lines, lines.omitted
	return summary, nil
}

// importOne fetches a single file, or says why it did not.
func (v *Vault) importOne(ctx context.Context, scope Scope, client *sandsftp.Client,
	source Source, f importFile, req ImportRequest, report func(TransferStage, int64)) ImportResult {

	result := ImportResult{Path: f.remote, Dest: path.Join(f.dir, f.name)}

	if !req.Overwrite {
		switch what, why := v.importDecision(scope, f); what {
		case leaveFile:
			result.Skipped, result.Reason = true, why
			return result
		case retimeFile:
			// Already here, byte for byte; only the time it is filed under is
			// wrong. Saying so is all that happens here — the correction itself
			// is an index write, and the writes are gathered by the caller
			// rather than made one per file. See importRetimes.
			result.Retimed, result.Reason = true, why
			return result
		}
	}

	// Said before the first byte moves, so the file being worked on appears the
	// moment it is picked up rather than once enough of it has arrived to
	// report. A skipped file is passed over in silence for the same reason: it
	// is over before there is anything to watch.
	if report != nil {
		report(StageFetching, 0)
	}

	remote, _, err := client.OpenUnder(source.Root, f.remote)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer remote.Close()

	// Straight from the SFTP handle into the vault: an *sftp.File is an
	// io.Reader, and UploadStream takes one. Nothing buffers the file, so a
	// 40 GB film costs the chunk window rather than 40 GB.
	//
	// Watching it is a counter in the middle of that stream and a callback out
	// of the scatter, which is why the two stages report separately: the reader
	// sees the file arrive, and only the upload can see it leave.
	var src io.Reader = remote
	// The file keeps the time it has on the machine it came from: it is the
	// same file, and it did not become new by being fetched.
	opts := UploadOptions{
		Overwrite: req.Overwrite, Accounts: req.Accounts, Scheme: req.Scheme,
		ModifiedAt: f.mod,
	}
	if report != nil {
		src = &progressReader{r: remote, say: func(done int64) { report(StageFetching, done) }}
		opts.OnScattered = func(done, _ int64) { report(StageScattering, done) }
	}

	_, warnings, err := v.UploadStream(ctx, scope, f.dir, f.name, src, opts)
	result.Warnings = warnings
	if err != nil {
		result.Error = err.Error()
		return result
	}

	result.OK = true
	return result
}

// importChoice is what to do about a file the vault may already hold.
type importChoice int

const (
	// fetchFile is the file being brought over: it is not here, or what is
	// here is not it.
	fetchFile importChoice = iota

	// leaveFile is the file being passed over: the same file is here already,
	// filed under the same time it has on the source.
	leaveFile

	// retimeFile is the same file being left where it is with its recorded
	// modification time put back to the source's. It is what a file imported
	// before the time was kept looks like on a re-run.
	retimeFile
)

// importDecision says what to do about one file of a selection, and what to say
// about it. See ImportFromSource for why a size comparison is enough to know
// the file is here, and what the modification time is doing in the answer.
func (v *Vault) importDecision(scope Scope, f importFile) (importChoice, string) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return fetchFile, ""
	}
	entry := m.ByPath(path.Join(f.dir, f.name))
	if entry == nil {
		return fetchFile, ""
	}
	if entry.Size != f.size {
		return fetchFile, ""
	}
	// Touched on the source since it was imported: a different file that
	// happens to be the same length, which is the one case a size alone gets
	// wrong. Compared against when the copy here arrived rather than against
	// the time it carries, because the time it carries is now the source's own
	// and would agree with itself.
	if f.mod.After(entry.CreatedAt) {
		return fetchFile, ""
	}
	// Here already, and unchanged where it came from — so nothing needs
	// fetching either way, and all that is left is whether the copy here is
	// filed under the file's own time or under the day it was imported.
	if f.mod.IsZero() || SameModTime(entry.ModifiedAt, f.mod) {
		return leaveFile, "already imported"
	}
	return retimeFile, "already imported; its modified time was put back to the source's"
}

// ensureFolder creates a destination folder once, remembering what it has made.
func (v *Vault) ensureFolder(scope Scope, dir string, made map[string]bool) error {
	dir = CleanDir(dir)
	if made[dir] {
		return nil
	}
	// Every folder above it too: Mkdir makes one level, and a selection three
	// deep needs the ones in between.
	var parents []string
	for at := dir; at != "/" && at != "."; at = path.Dir(at) {
		parents = append([]string{at}, parents...)
	}
	for _, folder := range parents {
		if made[folder] || v.FolderExists(scope, folder) {
			made[folder] = true
			continue
		}
		if err := v.Mkdir(scope, folder); err != nil {
			return fmt.Errorf("could not make the folder %s: %w", folder, err)
		}
		made[folder] = true
	}
	return nil
}

// planImport expands a selection into the list of files to fetch, walking any
// folder in it.
//
// Done up front, before a byte moves, so that a path the caller should not have
// asked for is refused before anything is transferred rather than halfway
// through — the same order handleFilesUpload works in.
//
// The walk reads every entry of every folder. The browser's listing is cut at
// sftp.MaxEntries because a page has no use for more; a plan cut the same way
// would leave the files past the cut out of the import with nothing to say
// they were, so the walk asks for the whole directory — see sftp.ReadDirAll.
//
// found, when set, is told how many files the walk has reached, every
// planEvery of them and once at the end. It is the only thing a caller can be
// told during a walk — there is no file being worked on yet — and on a folder
// of ten thousand it is the difference between a dialog that is thinking and a
// dialog that has died.
func planImport(client *sandsftp.Client, root string, paths []string, dest string,
	found func(int)) ([]importFile, error) {

	var (
		files []importFile
		seen  = map[string]bool{}
		said  int
	)

	// Said every planEvery files rather than per file, and against what was
	// last said rather than against the count itself, so a folder full of paths
	// already reached does not report the same number twice.
	say := func() {
		if found != nil && len(files)-said >= planEvery {
			said = len(files)
			found(said)
		}
	}

	var walk func(rel, destDir string, depth int) error
	walk = func(rel, destDir string, depth int) error {
		if depth > maxImportDepth {
			return fmt.Errorf("%s is nested deeper than %d folders", rel, maxImportDepth)
		}

		listing, err := client.ReadDirAll(root, rel)
		if err != nil {
			return err
		}
		for _, entry := range listing.Entries {
			// A link that cannot be followed is not a file to fetch. It was
			// already listed with a reason attached when it was browsed.
			if entry.Unreachable {
				continue
			}
			childRel := path.Join(rel, entry.Name)
			if entry.Dir {
				if err := walk(childRel, path.Join(destDir, entry.Name), depth+1); err != nil {
					return err
				}
				continue
			}
			addImport(&files, seen, importFile{
				remote: childRel,
				dir:    CleanDir(destDir),
				name:   entry.Name,
				size:   entry.Size,
				mod:    entry.ModTime,
			})
			say()
		}
		return nil
	}

	for _, raw := range paths {
		rel := sandsftp.CleanPath(strings.TrimPrefix(strings.TrimSpace(raw), "/"))
		if rel == "" {
			return nil, fmt.Errorf("cannot import the whole of a source by naming nothing: pick what to bring")
		}
		// Refused here as well as inside the client, because this is where the
		// answer is still "which of the things you asked for" rather than an
		// error attached to one file.
		if _, err := sandsftp.Under(root, rel); err != nil {
			return nil, err
		}

		info, err := client.StatUnder(root, rel)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			// The folder itself becomes a folder in the vault, so its shape
			// survives the trip.
			if err := walk(rel, path.Join(dest, path.Base(rel)), 1); err != nil {
				return nil, err
			}
			continue
		}
		addImport(&files, seen, importFile{
			remote: rel,
			dir:    CleanDir(dest),
			name:   path.Base(rel),
			size:   info.Size(),
			mod:    info.ModTime(),
		})
		say()
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("nothing to import: the selection holds no files")
	}
	// The whole count, whatever the throttle above was holding, so the last
	// thing said about the walk is what the walk actually found.
	if found != nil && len(files) != said {
		found(len(files))
	}
	return files, nil
}

// addImport appends a file unless the selection already reached it — picking a
// folder and a file inside it is an easy thing to do in a list with checkboxes,
// and importing it twice would put a numbered copy beside it.
func addImport(files *[]importFile, seen map[string]bool, f importFile) {
	if seen[f.remote] {
		return
	}
	seen[f.remote] = true
	*files = append(*files, f)
}

// planEvery is how many files the walk finds before it says so again.
//
// The walk is a few words per file and no I/O of its own between directories,
// so it can find thousands a second on a fast source; a report per file would
// be a lock and a wake-up per file to move a number nobody reads that closely.
// A couple of hundred keeps the count visibly moving on a slow walk without
// making a fast one pay for it.
const planEvery = 250

// retimeBatchSize is how many times are put back in one index write.
//
// A time correction changes nothing but a field on an entry, and the index is
// sealed and written whole — so the cost of correcting one time and of
// correcting five hundred is very nearly the same write. Done one file at a
// time, a re-import of a folder of twenty thousand photographs meant twenty
// thousand whole-index writes and was, absurdly, far slower than the import
// that fetched them.
//
// Not unbounded, because a batch in hand has not landed: the bigger it is, the
// more time corrections a kill in the middle throws away. Five hundred is two
// orders of magnitude off the per-file cost and still a fraction of a second's
// work to lose.
const retimeBatchSize = 500

// importRetimes gathers the files an import found already here, and filed under
// the wrong time, and puts their times back a batch at a time.
//
// It is a buffer in front of Retime and nothing else: the files it holds are in
// the vault, whole, and were in it before this import started. What is pending
// is the correction of a field on each of them.
//
// Losing a pending batch is therefore not losing anything a re-run does not put
// right — the same bargain the rest of an import makes about being interrupted.
// A batch that cannot be *written*, though, is a different matter: Retime puts
// the entries back as they were, so those files were not retimed, and flush
// hands them back so the summary can say so rather than counting a correction
// that is not there.
type importRetimes struct {
	v     *Vault
	scope Scope

	want []FileTime

	// held is the result line for each of want, in the same order, kept so a
	// write that fails can name the files it failed for.
	held []ImportResult
}

// add remembers one file whose recorded time should be the source's.
func (b *importRetimes) add(at FileTime, result ImportResult) {
	b.want = append(b.want, at)
	b.held = append(b.held, result)
}

// flushIfFull writes the batch once it is worth a write, and does nothing
// before that. It answers as flush does.
func (b *importRetimes) flushIfFull() []ImportResult {
	if len(b.want) < retimeBatchSize {
		return nil
	}
	return b.flush()
}

// flush puts the held times back, and answers with the files whose times did
// not land — none, unless the index could not be written, in which case it is
// all of them, each with the error on it. The batch is empty either way: a
// write that failed will not go better for being tried again with the next
// five hundred behind it.
func (b *importRetimes) flush() []ImportResult {
	if len(b.want) == 0 {
		return nil
	}
	want, held := b.want, b.held
	b.want, b.held = nil, nil

	if _, err := b.v.Retime(b.scope, want); err != nil {
		failed := make([]ImportResult, 0, len(held))
		for _, result := range held {
			result.Retimed, result.Reason = false, ""
			result.Error = err.Error()
			failed = append(failed, result)
		}
		return failed
	}
	return nil
}
