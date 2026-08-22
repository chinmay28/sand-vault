package vault

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
	sandsftp "github.com/chinmay28/sand-vault/internal/sftp"
)

// MaxImportFiles bounds how many files one import request may pull.
//
// A person selecting a folder does not know how many files are under it, and
// "import my media drive" is a request that can mean two hundred thousand of
// them. The cap is what keeps one request from becoming an operation nobody can
// see the end of, and it is reported rather than applied silently — an import
// that quietly stopped at a round number would look exactly like an import that
// finished.
const MaxImportFiles = 2000

// maxImportDepth bounds how deep the walk under a selected folder goes.
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
}

// ImportResult is what became of one file. One line per file, so a partial
// import is legible rather than mysterious — the same bargain the browser
// upload handler strikes.
type ImportResult struct {
	// Path is the file on the source, relative to its root.
	Path string `json:"path"`

	// Dest is where it went, or would have gone, in the vault.
	Dest string `json:"dest"`

	OK      bool `json:"ok,omitempty"`
	Skipped bool `json:"skipped,omitempty"`

	// Reason says why a file was skipped, and is empty otherwise.
	Reason string `json:"reason,omitempty"`

	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	File     *Entry   `json:"file,omitempty"`
}

// ImportSummary is the whole request's outcome.
type ImportSummary struct {
	Results  []ImportResult `json:"results"`
	Imported int            `json:"imported"`
	Skipped  int            `json:"skipped"`
	Failed   int            `json:"failed"`

	// Truncated says the selection held more than MaxImportFiles and the rest
	// was not attempted. Re-running the import picks up where this left off,
	// because what already arrived is skipped — see ImportFromSource.
	Truncated bool `json:"truncated,omitempty"`
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

	files, truncated, err := planImport(client, source.Root, req.Paths, dest)
	if err != nil {
		return ImportSummary{}, err
	}

	summary := ImportSummary{
		Results:   make([]ImportResult, 0, len(files)),
		Truncated: truncated,
	}

	// Every folder the files hang off, made once each and before anything is
	// fetched. A folder that will not be made fails every file under it on its
	// own line, which is what says how much of the selection actually arrived.
	made[dest] = true
	for _, f := range files {
		if err := v.ensureFolder(scope, f.dir, made); err != nil {
			summary.Results = append(summary.Results, ImportResult{
				Path:  f.remote,
				Dest:  path.Join(f.dir, f.name),
				Error: err.Error(),
			})
			summary.Failed++
			continue
		}

		result := v.importOne(ctx, scope, client, source, f, req)
		switch {
		case result.OK:
			summary.Imported++
		case result.Skipped:
			summary.Skipped++
		default:
			summary.Failed++
		}
		summary.Results = append(summary.Results, result)

		// A cancelled request stops here rather than working through the rest
		// of the selection with a dead context and reporting a failure per
		// file. What arrived is already committed, and re-running resumes.
		if ctx.Err() != nil {
			break
		}
	}
	return summary, nil
}

// importOne fetches a single file, or says why it did not.
func (v *Vault) importOne(ctx context.Context, scope Scope, client *sandsftp.Client,
	source Source, f importFile, req ImportRequest) ImportResult {

	result := ImportResult{Path: f.remote, Dest: path.Join(f.dir, f.name)}

	if !req.Overwrite {
		if existing, why := v.alreadyImported(scope, f); existing {
			result.Skipped, result.Reason = true, why
			return result
		}
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
	entry, warnings, err := v.UploadStream(ctx, scope, f.dir, f.name, remote, UploadOptions{
		Overwrite: req.Overwrite,
		Accounts:  req.Accounts,
		Scheme:    req.Scheme,
	})
	result.Warnings = warnings
	if err != nil {
		result.Error = err.Error()
		return result
	}

	result.OK = true
	result.File = entry
	return result
}

// alreadyImported reports whether this file is in the vault already, and what
// to say about it. See ImportFromSource for why a size comparison is enough.
func (v *Vault) alreadyImported(scope Scope, f importFile) (bool, string) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return false, ""
	}
	entry := m.ByPath(path.Join(f.dir, f.name))
	if entry == nil {
		return false, ""
	}
	if entry.Size != f.size {
		return false, ""
	}
	// Newer on the source than the copy here: a different file that happens to
	// be the same length, which is the one case a size alone gets wrong.
	if f.mod.After(entry.CreatedAt) {
		return false, ""
	}
	return true, "already imported"
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
func planImport(client *sandsftp.Client, root string, paths []string, dest string) ([]importFile, bool, error) {
	var (
		files     []importFile
		truncated bool
		seen      = map[string]bool{}
	)

	var walk func(rel, destDir string, depth int) error
	walk = func(rel, destDir string, depth int) error {
		if len(files) >= MaxImportFiles {
			truncated = true
			return nil
		}
		if depth > maxImportDepth {
			return fmt.Errorf("%s is nested deeper than %d folders", rel, maxImportDepth)
		}

		listing, err := client.ReadDir(root, rel)
		if err != nil {
			return err
		}
		if listing.Truncated {
			truncated = true
		}
		for _, entry := range listing.Entries {
			if len(files) >= MaxImportFiles {
				truncated = true
				return nil
			}
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
		}
		return nil
	}

	for _, raw := range paths {
		rel := sandsftp.CleanPath(strings.TrimPrefix(strings.TrimSpace(raw), "/"))
		if rel == "" {
			return nil, false, fmt.Errorf("cannot import the whole of a source by naming nothing: pick what to bring")
		}
		// Refused here as well as inside the client, because this is where the
		// answer is still "which of the things you asked for" rather than an
		// error attached to one file.
		if _, err := sandsftp.Under(root, rel); err != nil {
			return nil, false, err
		}

		info, err := client.StatUnder(root, rel)
		if err != nil {
			return nil, false, err
		}
		if info.IsDir() {
			// The folder itself becomes a folder in the vault, so its shape
			// survives the trip.
			if err := walk(rel, path.Join(dest, path.Base(rel)), 1); err != nil {
				return nil, false, err
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
	}

	if len(files) == 0 && !truncated {
		return nil, false, fmt.Errorf("nothing to import: the selection holds no files")
	}
	return files, truncated, nil
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
