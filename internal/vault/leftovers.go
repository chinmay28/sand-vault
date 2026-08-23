package vault

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Working files SAND left in its own directory, and getting rid of them.
//
// orphans.go asks the clouds what they are holding that no file wants. This
// asks the same question of the one disk SAND writes to itself — the folder the
// vault file lives in, /var/lib/sand under the service and ~/.sand on a
// desktop — and it is worth asking because the answer there is bigger. An
// abandoned part is a fraction of a file; an abandoned upload spool is the
// whole thing, in plaintext, and there is one per upload that was interrupted.
//
// **Where they come from.** A stream cannot say its own SHA-256 until its last
// byte, and every chunk of a stored file has to carry that hash (§7.1), so an
// upload arriving over the network is written to a temporary file beside the
// vault before a byte of it is sent anywhere (see UploadStream). The spool is
// removed on every path out of that function including failure — but not on the
// path that is not a path out of it at all: the process being killed, the
// machine losing power, the container being restarted mid-upload. Then the file
// stays, at the full size of whatever was being uploaded, and nothing ever goes
// back for it. Four interrupted films is thirty gigabytes of a disk somebody
// chose for being small.
//
// The same holds, in smaller amounts, for the conversion spool, the two atomic
// writes (the index and the read history each land through a temporary file
// beside themselves), and the workspace a repository is packed in.
//
// **What makes this safe.** Two facts have to hold before a file here is
// offered for deletion, and they cover the two ways of being wrong.
//
// A name only SAND writes. The pattern is strict and closed — the five names
// this package creates, each followed by the digits os.CreateTemp appends and
// nothing else. Anything else in the folder is not looked at, which is what
// keeps a vault directory somebody also keeps notes in from becoming a list of
// things to erase. It also means a leftover under a name SAND has stopped
// using goes unreported, and that is the right way round.
//
// Not in use. A spool being written looks exactly like a spool that was
// abandoned — same name, same shape, and the size of a growing one is not a
// signal, because an abandoned one is whatever size it reached. So the ones
// this process is writing are known exactly, held in Vault.spools and never
// reported at all; and for the ones another process might be writing, which
// this one cannot see, a file is only offered once nothing has written to it
// for leftoverSettling. A live spool is fed continuously from the network, so
// an idle hour is the difference between a copy in progress and a copy that
// stopped. Younger ones are still shown, with the reason beside them, because
// the disk they are filling is the thing somebody came here to understand.
//
// Nothing here reads or writes the index and nothing needs a password: these
// are SAND's own scratch files, not anybody's data. It is reported by the same
// scan as the orphans (see OrphanScan.Leftovers) because it is the same
// question asked of the last place it had not been asked of, and it is swept by
// its own call, because looking is safe and erasing is not.

// leftoverPattern matches the temporary names this package creates, and only
// those: os.CreateTemp and os.MkdirTemp replace the trailing "*" with decimal
// digits, so a name is one of ours when it is one of the five prefixes followed
// by digits to the end.
//
// Strict on purpose, in the same way partKeyPattern is. A name that does not
// match is not treated as SAND's at all, which costs a leftover going
// unreported and buys the guarantee that this never proposes deleting
// something SAND did not write.
var leftoverPattern = regexp.MustCompile(`^\.sand-(upload|convert|reads|vault|git)-[0-9]{1,20}$`)

// leftoverSettling is how long a file has to have been untouched before it is
// offered for deletion.
//
// The window is for the writer this process cannot see: the CLI and the service
// share one vault directory and are meant to be used interchangeably on a host,
// so `sand up` on a 40 GB film may well be filling a spool that the server's
// scan is looking at. What separates that from an abandoned one is not size or
// age — an upload can run for hours — but whether anything is still writing to
// it. An hour of silence is far longer than a stall on the slowest link that is
// still making progress, and costs nothing but a scan saying "not yet" about a
// file it will offer later.
const leftoverSettling = time.Hour

// leftoverPreview caps the rows a scan hands back, the way orphanArchivePreview
// does. The totals always count everything found.
const leftoverPreview = 200

// leftoverKinds says what each name is for, in the words somebody reading a
// list of files in their vault folder needs. The kind is what the pattern
// captured.
var leftoverKinds = map[string]string{
	"upload":  "an upload that was interrupted, spooled to disk before it could be sent",
	"convert": "a file being rebuilt into the chunked format when the process stopped",
	"reads":   "a half-written copy of the read history",
	"vault":   "a half-written copy of the vault index — the real one is untouched",
	"git":     "a workspace a repository was being packed in",
}

// Leftover is one working file in the vault's own directory that nothing is
// using any more.
type Leftover struct {
	// Name is the file's name, not its path. The folder is the vault's own and
	// is reported once, on the scan.
	Name string `json:"name"`

	// Kind is which of SAND's temporary names this is — upload, convert,
	// reads, vault or git — and What says in a line what that means.
	Kind string `json:"kind"`
	What string `json:"what"`

	// Dir is set for the workspace a repository is packed in, which is a
	// directory; everything else here is a single file. Bytes is what it
	// weighs, walked for a directory.
	Dir   bool  `json:"dir"`
	Bytes int64 `json:"bytes"`

	// Modified is when anything last wrote to it — the newest write anywhere
	// inside, for a directory. It is the evidence the settling window is read
	// from, and the only thing on a row that says how long this has been going
	// on.
	Modified time.Time `json:"modified"`

	// Deletable is false while the file might still be being written, and
	// Reason says so. A row like that is reported and never swept.
	Deletable bool   `json:"deletable"`
	Reason    string `json:"reason,omitempty"`
}

// LeftoverScan is what the vault's own directory is holding for nobody.
type LeftoverScan struct {
	// Dir is the folder that was looked in, so a report can name it.
	Dir string `json:"dir"`

	// Found is the prompt condition: at least one leftover, whether or not it
	// has settled long enough to be offered.
	Found bool `json:"found"`

	// Files and Bytes count everything found; Deletable and DeletableBytes the
	// subset a sweep would erase now.
	Files          int   `json:"files"`
	Bytes          int64 `json:"bytes"`
	Deletable      int   `json:"deletable"`
	DeletableBytes int64 `json:"deletable_bytes"`

	// Items are the rows, heaviest first and capped; ItemsTruncated is how
	// many did not fit.
	Items          []Leftover `json:"items"`
	ItemsTruncated int        `json:"items_truncated,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// LeftoverSweepReport says what a sweep of the vault's directory did.
type LeftoverSweepReport struct {
	Deleted int   `json:"deleted"`
	Bytes   int64 `json:"bytes"`

	// Skipped names what was asked for and left alone, and why — a file that
	// has been written to since the scan ran is what this exists for.
	Skipped []string `json:"skipped,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// ScanForLeftovers lists the working files in the vault's own directory that
// nothing is using.
//
// One directory read, plus a walk of any repository workspace, and nothing
// else: no account is asked anything and no key is needed, so this answers on a
// locked vault as readily as on an open one. Nothing is written.
func (v *Vault) ScanForLeftovers() *LeftoverScan {
	scan := v.scanForLeftovers()
	if len(scan.Items) > leftoverPreview {
		scan.ItemsTruncated = len(scan.Items) - leftoverPreview
		scan.Items = scan.Items[:leftoverPreview]
	}
	return scan
}

// scanForLeftovers is ScanForLeftovers with every row it found, however many
// that is. The sweep works from this rather than from the trimmed answer, so
// that erasing what a capped list did not have room to show still erases it.
func (v *Vault) scanForLeftovers() *LeftoverScan {
	v.mu.RLock()
	dir := filepath.Dir(v.path)
	v.mu.RUnlock()

	scan := &LeftoverScan{Dir: dir, Items: []Leftover{}}

	entries, err := os.ReadDir(dir)
	if err != nil {
		// A vault that has never been written has no directory yet, which is
		// not a fault and not worth a warning.
		if !os.IsNotExist(err) {
			scan.Warnings = append(scan.Warnings, fmt.Sprintf(
				"could not look in %s for working files left behind: %s", dir, err))
		}
		return scan
	}

	held := v.heldSpools()
	now := time.Now()

	for _, entry := range entries {
		name := entry.Name()
		match := leftoverPattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		// Being written by this very process. Not a leftover by any reading,
		// so it is not reported at all rather than reported and withheld —
		// a scan that says nothing is adrift should mean it.
		if held[filepath.Join(dir, name)] {
			continue
		}

		size, modified, err := weigh(filepath.Join(dir, name))
		if err != nil {
			// Gone between the listing and the stat is the ordinary way a
			// spool ends, and says nothing worth saying.
			if !os.IsNotExist(err) {
				scan.Warnings = append(scan.Warnings, fmt.Sprintf(
					"could not measure %s: %s", name, err))
			}
			continue
		}

		item := Leftover{
			Name:      name,
			Kind:      match[1],
			What:      leftoverKinds[match[1]],
			Dir:       entry.IsDir(),
			Bytes:     size,
			Modified:  modified,
			Deletable: true,
		}
		if idle := now.Sub(modified); idle < leftoverSettling {
			item.Deletable = false
			item.Reason = "something wrote to it " + roughly(idle) +
				" ago, so an upload may still be running"
		}

		scan.Files++
		scan.Bytes += item.Bytes
		if item.Deletable {
			scan.Deletable++
			scan.DeletableBytes += item.Bytes
		}
		scan.Items = append(scan.Items, item)
	}
	scan.Found = scan.Files > 0

	// Heaviest first, for the reason the orphan rows are: the question being
	// answered is how much room this gives back, so the rows that answer it
	// should not be the ones the cap cuts.
	sort.Slice(scan.Items, func(i, j int) bool {
		if scan.Items[i].Bytes != scan.Items[j].Bytes {
			return scan.Items[i].Bytes > scan.Items[j].Bytes
		}
		return scan.Items[i].Name < scan.Items[j].Name
	})
	return scan
}

// SweepLeftovers erases the working files a scan reported.
//
// names are the rows somebody ticked; empty means everything the vault
// currently considers safe to erase, which is what a command line asking to
// tidy up means. A name is only ever acted on if the fresh scan below names it
// too, so nothing outside the vault's own directory and nothing under a name
// SAND does not write can be reached from here however the request was built.
//
// The scan is re-run rather than trusted from the caller, for the reason
// SweepOrphans re-runs its own: between being shown a figure and agreeing to
// it, an upload may have started, and a spool that has been written to since is
// no longer a leftover. One that has is skipped and reported, never erased.
func (v *Vault) SweepLeftovers(names []string, dryRun bool) *LeftoverSweepReport {
	scan := v.scanForLeftovers()
	report := &LeftoverSweepReport{Warnings: scan.Warnings}

	deletable := map[string]Leftover{}
	for _, item := range scan.Items {
		if item.Deletable {
			deletable[item.Name] = item
		}
	}

	wanted := make([]Leftover, 0, len(deletable))
	if len(names) == 0 {
		for _, item := range deletable {
			wanted = append(wanted, item)
		}
	} else {
		for _, name := range names {
			item, ok := deletable[filepath.Base(name)]
			if !ok {
				report.Skipped = append(report.Skipped, fmt.Sprintf(
					"%s is no longer a working file this vault is finished with, so it was left alone",
					filepath.Base(name)))
				continue
			}
			wanted = append(wanted, item)
		}
	}
	sort.Slice(wanted, func(i, j int) bool { return wanted[i].Name < wanted[j].Name })

	for _, item := range wanted {
		if dryRun {
			report.Deleted++
			report.Bytes += item.Bytes
			continue
		}
		path := filepath.Join(scan.Dir, item.Name)
		var err error
		if item.Dir {
			err = os.RemoveAll(path)
		} else {
			err = os.Remove(path)
		}
		if err != nil && !os.IsNotExist(err) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s could not be erased: %s", item.Name, err))
			continue
		}
		report.Deleted++
		report.Bytes += item.Bytes
	}
	return report
}

// weigh returns what a leftover occupies and when anything last wrote to it.
//
// A directory is walked rather than stat'd, for both figures: its own size says
// nothing about what is inside it, and its own modification time stops moving
// as soon as the last entry is created, while a pack still being built goes on
// writing into the files underneath. The newest write anywhere inside is the
// one the settling window has to be measured from.
func weigh(path string) (int64, time.Time, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	if !info.IsDir() {
		return info.Size(), info.ModTime(), nil
	}

	size, newest := int64(0), info.ModTime()
	err = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A workspace being torn down under the walk is not a reason to
			// refuse to report the rest of it.
			return nil //nolint:nilerr // partial is better than nothing here
		}
		sub, err := entry.Info()
		if err != nil {
			return nil
		}
		if sub.Mode().IsRegular() {
			size += sub.Size()
		}
		if sub.ModTime().After(newest) {
			newest = sub.ModTime()
		}
		return nil
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	return size, newest, nil
}

// roughly renders a duration the way a sentence about it would say it. Only
// ever used on the settling window, so minutes and hours are the whole
// vocabulary it needs.
func roughly(d time.Duration) string {
	switch minutes := int(d.Minutes()); {
	case minutes < 1:
		return "moments"
	case minutes == 1:
		return "a minute"
	case minutes < 60:
		return fmt.Sprintf("%d minutes", minutes)
	default:
		return "an hour"
	}
}

// ---------------------------------------------------------------------------
// The files this process is writing right now
// ---------------------------------------------------------------------------

// holdSpool records that this process is writing path, so that a scan running
// alongside it does not mistake a file being filled for one that was
// abandoned. Every caller must release it again — discardSpool does both for
// the ordinary case of throwing the file away.
func (v *Vault) holdSpool(path string) {
	v.spoolMu.Lock()
	defer v.spoolMu.Unlock()
	if v.spools == nil {
		v.spools = map[string]bool{}
	}
	v.spools[path] = true
}

// releaseSpool forgets a held file. Called on every path out of the operation
// holding it, including the ones that leave the file behind: a spool that
// outlives the upload it belonged to is exactly what this scan is for.
func (v *Vault) releaseSpool(path string) {
	v.spoolMu.Lock()
	defer v.spoolMu.Unlock()
	delete(v.spools, path)
}

// discardSpool closes a temporary file, removes it and stops holding it — the
// three things every path out of a spooled operation does.
func (v *Vault) discardSpool(f *os.File) {
	if f == nil {
		return
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	v.releaseSpool(name)
}

// heldSpools is the set of paths this process is writing, as of now.
func (v *Vault) heldSpools() map[string]bool {
	v.spoolMu.Lock()
	defer v.spoolMu.Unlock()
	held := make(map[string]bool, len(v.spools))
	for path := range v.spools {
		held[path] = true
	}
	return held
}
