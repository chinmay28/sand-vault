package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chinmay28/sand-vault/internal/checksums"
)

// Checksums: one per file, a ledger of them, and two jobs that use them.
//
// A file's checksum is the SHA-256 of its contents, and the index already has
// a place for it — Entry.Hash, taken from the bytes as they are uploaded. So
// this file adds no second record that could disagree with the first. What it
// adds is the three things around it:
//
// The ledger. checksums.gz, at the root of the vault, is every file's path and
// checksum as text (internal/checksums). It is an ordinary file in the vault —
// cut, sealed and scattered over the default clouds like any other, listed in
// the browser, downloadable — written from the index, and rewritten when the
// index has changed. It exists so that the record of what every file should
// hash to is something a person can take out of the vault and hold a download
// against, without this program.
//
// Verifying. VerifyChecksums reads every file under a folder, front to back,
// the way a download does, and holds what comes out to its checksum. It is
// the cheap corruption check: it fetches the k shards a read needs rather than
// all n, and looks at nothing but the hash — CheckFolder (verify.go) is the
// one that says which shard on which cloud is wrong.
//
// Filling in. Nearly every file has a checksum from the moment it is stored.
// The ones that do not came in some other way — recovered from the accounts
// when the index was lost, or stored by a build from before hashes were kept.
// ComputeChecksums reads those and records what they hash to; and a file with
// none that somebody opens and reads to the end has its checksum recorded
// from that read, so the gap closes by itself for anything in use.
//
// Both jobs are long reads of other people's servers, so both are paced: a
// ChecksumControl carries a byte rate that can be changed while the job runs,
// and a pause that holds it between reads without losing its place.

// ChecksumsPath is where a vault's ledger is kept, in its own tree.
const ChecksumsPath = "/" + checksums.Name

// ChecksumKind is which of the two jobs a run is.
type ChecksumKind string

const (
	ChecksumVerify  ChecksumKind = "verify"
	ChecksumCompute ChecksumKind = "compute"
)

// ChecksumControl paces a running checksum job. The zero value is not usable;
// make one with NewChecksumControl. Every method is safe to call from any
// goroutine while the job runs.
type ChecksumControl struct {
	rate   atomic.Int64 // bytes a second; zero or less is unlimited
	paused atomic.Bool

	mu   sync.Mutex
	next time.Time // the earliest the next read may go ahead
}

// NewChecksumControl is a control at the given rate, in bytes a second. Zero
// is as fast as the clouds answer.
func NewChecksumControl(bytesPerSecond int64) *ChecksumControl {
	c := &ChecksumControl{}
	c.SetRate(bytesPerSecond)
	return c
}

// SetRate changes the pace, from the next read on.
func (c *ChecksumControl) SetRate(bytesPerSecond int64) {
	c.rate.Store(max(bytesPerSecond, 0))
	// What was owed at the old rate is forgiven: a job slowed to a crawl and
	// then let go should go, not finish serving out the crawl first.
	c.mu.Lock()
	c.next = time.Time{}
	c.mu.Unlock()
}

// Rate is the pace in bytes a second, zero for unlimited.
func (c *ChecksumControl) Rate() int64 { return c.rate.Load() }

// Pause holds the job at its next read; Resume lets it go on.
func (c *ChecksumControl) Pause()       { c.paused.Store(true) }
func (c *ChecksumControl) Resume()      { c.paused.Store(false) }
func (c *ChecksumControl) Paused() bool { return c.paused.Load() }

// controlTick is how often a held job looks up: the longest a pause, a change
// of rate or a cancel goes unnoticed.
const controlTick = 100 * time.Millisecond

// wait accounts for n bytes just read: it holds while the job is paused, then
// for as long as the rate says those bytes should have taken. It returns the
// context's error if that is what ended the wait.
func (c *ChecksumControl) wait(ctx context.Context, n int) error {
	if c == nil {
		return ctx.Err()
	}
	for c.paused.Load() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(controlTick):
		}
	}

	rate := c.rate.Load()
	if rate <= 0 || n <= 0 {
		return ctx.Err()
	}
	c.mu.Lock()
	now := time.Now()
	if c.next.Before(now) {
		c.next = now
	}
	c.next = c.next.Add(time.Duration(float64(n) / float64(rate) * float64(time.Second)))
	until := c.next
	c.mu.Unlock()

	for {
		left := time.Until(until)
		if left <= 0 {
			return ctx.Err()
		}
		// Slept in slices, so a pause or a faster rate is felt mid-wait.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(left, controlTick)):
		}
		if c.rate.Load() != rate {
			return ctx.Err()
		}
	}
}

// ChecksumOptions observes and paces a checksum job.
type ChecksumOptions struct {
	// Control paces the job. Nil is unpaced and unpausable.
	Control *ChecksumControl

	// OnProgress hears where the job has got to. May be nil.
	OnProgress func(ChecksumProgress)
}

// ChecksumProgress is where a running checksum job has got to.
type ChecksumProgress struct {
	Kind  ChecksumKind `json:"kind"`
	Path  string       `json:"path"`
	File  int          `json:"file"`
	Files int          `json:"files"`

	// Bytes is how much has been read and hashed, of Total.
	Bytes int64 `json:"bytes"`
	Total int64 `json:"total"`

	// Flagged is how many files so far did not come out right.
	Flagged int `json:"flagged"`
}

// Checksum finding states.
const (
	ChecksumMismatch   = "mismatch"   // read back, and not to its checksum
	ChecksumUnreadable = "unreadable" // could not be read back at all
	ChecksumSkipped    = "skipped"    // not read, and Detail says why
)

// ChecksumFinding is one file a checksum job has something to say about.
type ChecksumFinding struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	State  string `json:"state"`
	Detail string `json:"detail"`

	// Expected is the checksum on record and Got what the file read back as,
	// for a mismatch.
	Expected string `json:"expected,omitempty"`
	Got      string `json:"got,omitempty"`
}

// checksumFindingLimit caps how many findings a report lists. The counts
// always cover every file.
const checksumFindingLimit = 1000

// ChecksumReport is what a checksum job did.
type ChecksumReport struct {
	Kind  ChecksumKind `json:"kind"`
	Vault Scope        `json:"vault,omitempty"`
	Path  string       `json:"path"`

	// Files is how many the job had to read and Checked how many it got to;
	// Bytes is what it read.
	Files   int   `json:"files"`
	Checked int   `json:"checked"`
	Bytes   int64 `json:"bytes"`

	// Verify: Matched read back to their checksum, Mismatched did not.
	// Compute: Computed had a checksum recorded.
	// Either: Unreadable could not be read, Skipped were not tried.
	Matched    int `json:"matched"`
	Mismatched int `json:"mismatched"`
	Computed   int `json:"computed"`
	Unreadable int `json:"unreadable"`
	Skipped    int `json:"skipped"`

	// NoChecksum is how many files under the folder a verify could not
	// check because they have none on record; they are not in Files.
	NoChecksum int `json:"no_checksum"`

	Findings  []ChecksumFinding `json:"findings"`
	Truncated bool              `json:"truncated,omitempty"`

	// Stopped says the job was cut short, and the counts cover only the
	// files it reached.
	Stopped bool `json:"stopped,omitempty"`

	// Ledger is the ledger as a compute left it, having written it.
	Ledger      *ChecksumLedger `json:"ledger,omitempty"`
	LedgerError string          `json:"ledger_error,omitempty"`
}

func (r *ChecksumReport) flag(f ChecksumFinding) {
	switch f.State {
	case ChecksumMismatch:
		r.Mismatched++
	case ChecksumUnreadable:
		r.Unreadable++
	case ChecksumSkipped:
		r.Skipped++
	}
	if len(r.Findings) < checksumFindingLimit {
		r.Findings = append(r.Findings, f)
	} else {
		r.Truncated = true
	}
}

func (r *ChecksumReport) flagged() int { return r.Mismatched + r.Unreadable + r.Skipped }

// ChecksumSurvey is what the two jobs would have to do under a folder, from
// the index alone.
type ChecksumSurvey struct {
	Path string `json:"path"`

	// Files and Bytes are every file under the folder; With and WithBytes
	// the ones that have a checksum, which is what a verify reads; Without
	// and WithoutBytes the ones that have none, which is what a compute does.
	Files        int   `json:"files"`
	Bytes        int64 `json:"bytes"`
	With         int   `json:"with"`
	WithBytes    int64 `json:"with_bytes"`
	Without      int   `json:"without"`
	WithoutBytes int64 `json:"without_bytes"`

	Ledger ChecksumLedger `json:"ledger"`
}

// ChecksumLedger describes a vault's checksums.gz.
type ChecksumLedger struct {
	Path string `json:"path"`

	// Stored says the ledger is in the vault; ID, Size and WrittenAt are its
	// own row's, for a link to it and a line about how fresh it is.
	Stored    bool       `json:"stored"`
	ID        string     `json:"id,omitempty"`
	Size      int64      `json:"size,omitempty"`
	WrittenAt *time.Time `json:"written_at,omitempty"`

	// Entries is how many files have a checksum to put in it now.
	Entries int `json:"entries"`

	// Written says this call stored a new one, as against finding the one
	// already there up to date.
	Written bool `json:"written,omitempty"`
}

// isLedger reports whether an index row is the ledger itself, which is never
// listed in it or read by the jobs that use it.
func isLedger(e *Entry) bool { return e.Path() == ChecksumsPath }

// SurveyChecksums counts what a verify and a compute of dir would each read.
func (v *Vault) SurveyChecksums(scope Scope, dir string) (*ChecksumSurvey, error) {
	entries, err := v.Descendants(scope, dir)
	if err != nil {
		return nil, err
	}
	out := &ChecksumSurvey{Path: CleanDir(dir)}
	for _, e := range entries {
		if isLedger(e) {
			continue
		}
		out.Files++
		out.Bytes += e.Size
		if e.Hash != "" {
			out.With++
			out.WithBytes += e.Size
		} else {
			out.Without++
			out.WithoutBytes += e.Size
		}
	}
	ledger, err := v.ChecksumLedgerInfo(scope)
	if err != nil {
		return nil, err
	}
	out.Ledger = *ledger
	return out, nil
}

// ChecksumLedgerInfo describes a vault's ledger as it stands.
func (v *Vault) ChecksumLedgerInfo(scope Scope) (*ChecksumLedger, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}
	return ledgerInfoLocked(m), nil
}

func ledgerInfoLocked(m *Manifest) *ChecksumLedger {
	info := &ChecksumLedger{Path: ChecksumsPath}
	for _, e := range m.Entries {
		if isLedger(e) {
			continue
		}
		if e.Hash != "" {
			info.Entries++
		}
	}
	if e := m.ByPath(ChecksumsPath); e != nil {
		at := e.CreatedAt
		info.Stored, info.ID, info.Size, info.WrittenAt = true, e.ID, e.Size, &at
	}
	return info
}

// WriteChecksumLedger brings a vault's checksums.gz up to date with its index
// and stores it at the root, on the vault's default clouds.
//
// It is the index written out: every file that has a checksum, by its full
// path. The encoding is deterministic, so a ledger whose bytes would be the
// ones already stored is left alone — nothing is uploaded and nothing erased —
// which is what makes calling this on a timer cost nothing while the vault is
// not changing. A vault with no checksummed file and no ledger gets none.
func (v *Vault) WriteChecksumLedger(ctx context.Context, scope Scope) (*ChecksumLedger, error) {
	// One writer at a time: two would each replace the other's file.
	v.ledgerMu.Lock()
	defer v.ledgerMu.Unlock()

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	m, err := v.manifestForLocked(scope)
	if err != nil {
		v.mu.RUnlock()
		return nil, err
	}
	var lines []checksums.Entry
	for _, e := range m.Entries {
		if !isLedger(e) && checksums.ValidSum(e.Hash) {
			lines = append(lines, checksums.Entry{Path: e.Path(), Sum: e.Hash})
		}
	}
	stored := ""
	if e := m.ByPath(ChecksumsPath); e != nil {
		stored = e.Hash
	}
	info := ledgerInfoLocked(m)
	v.mu.RUnlock()

	if len(lines) == 0 && stored == "" {
		return info, nil
	}
	data, err := checksums.Encode(lines)
	if err != nil {
		return nil, fmt.Errorf("writing the checksum ledger: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) == stored {
		return info, nil
	}

	entry, _, err := v.Upload(ctx, scope, "/", checksums.Name, data, UploadOptions{Overwrite: true})
	if err != nil {
		return nil, fmt.Errorf("storing %s: %w", ChecksumsPath, err)
	}
	at := entry.CreatedAt
	return &ChecksumLedger{
		Path: ChecksumsPath, Stored: true, ID: entry.ID, Size: entry.Size,
		WrittenAt: &at, Entries: len(lines), Written: true,
	}, nil
}

// WriteChecksumLedgers brings the ledger of every open vault up to date: the
// main one and each sub vault that is unlocked. It is what a timer calls. A
// vault whose index has not been written since its ledger was last brought
// up to date is not even looked at.
func (v *Vault) WriteChecksumLedgers(ctx context.Context) error {
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return ErrLocked
	}
	stamps := map[Scope]time.Time{MainScope: v.manifest.UpdatedAt}
	for id, sub := range v.subs {
		stamps[Scope(id)] = sub.manifest.UpdatedAt
	}
	v.mu.RUnlock()

	var failed []error
	for scope, stamp := range stamps {
		v.ledgerMu.Lock()
		seen, ok := v.ledgerSeen[scope]
		v.ledgerMu.Unlock()
		if ok && seen.Equal(stamp) {
			continue
		}
		if _, err := v.WriteChecksumLedger(ctx, scope); err != nil {
			failed = append(failed, err)
			continue
		}
		// The stamp from before the write: storing the ledger is itself an
		// index write, so the next call looks once more, finds the bytes
		// unchanged, and only then goes quiet.
		v.ledgerMu.Lock()
		if v.ledgerSeen == nil {
			v.ledgerSeen = map[Scope]time.Time{}
		}
		v.ledgerSeen[scope] = stamp
		v.ledgerMu.Unlock()
	}
	return errors.Join(failed...)
}

// RecordChecksum records what a file's contents hash to, for a file that has
// no checksum on record. It reports whether it recorded anything.
//
// It never replaces a checksum: one that is there was taken from the bytes as
// they were stored, and a later read disagreeing with it is a finding about
// the read, not a correction. archiveID is the placement the bytes were read
// from; a file rewritten since is left for the next read.
func (v *Vault) RecordChecksum(id, archiveID, sum string) (bool, error) {
	if !checksums.ValidSum(sum) {
		return false, fmt.Errorf("%q is not a SHA-256 checksum", sum)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return false, ErrLocked
	}
	_, e, ok := v.scopeOfEntryLocked(id)
	if !ok || e.Hash != "" || e.ArchiveID != archiveID {
		return false, nil
	}
	e.Hash = sum
	if err := v.persistLocked(); err != nil {
		e.Hash = ""
		return false, err
	}
	return true, nil
}

// hashBuffer is how much is read between looks at the control.
const hashBuffer = 256 << 10

// hashEntry reads one file front to back and returns what it hashes to, the
// placement it was read from and the checksum on record for it at that
// moment. onBytes hears each read.
func (v *Vault) hashEntry(ctx context.Context, id string, ctl *ChecksumControl, onBytes func(n int)) (sum, archiveID, recorded string, err error) {
	// The sequential reader: one chunk held at a time, and nothing put in
	// the shared cache a player beside this is using.
	r, entry, err := v.OpenSequential(ctx, id)
	if err != nil {
		return "", "", "", err
	}
	digest := sha256.New()
	buf := make([]byte, hashBuffer)
	var read int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			digest.Write(buf[:n])
			read += int64(n)
			if onBytes != nil {
				onBytes(n)
			}
			if werr := ctl.wait(ctx, n); werr != nil {
				return "", "", "", werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", "", err
		}
	}
	if read != entry.Size {
		return "", "", "", fmt.Errorf("it read back as %d bytes and the index records %d", read, entry.Size)
	}
	return hex.EncodeToString(digest.Sum(nil)), entry.ArchiveID, entry.Hash, nil
}

// VerifyChecksums reads every file at or below dir that has a checksum and
// holds it to that checksum. It writes nothing.
func (v *Vault) VerifyChecksums(ctx context.Context, scope Scope, dir string, opts ChecksumOptions) (*ChecksumReport, error) {
	return v.runChecksums(ctx, ChecksumVerify, scope, dir, opts)
}

// ComputeChecksums reads every file at or below dir that has no checksum,
// records what each hashes to, and brings the ledger up to date.
func (v *Vault) ComputeChecksums(ctx context.Context, scope Scope, dir string, opts ChecksumOptions) (*ChecksumReport, error) {
	return v.runChecksums(ctx, ChecksumCompute, scope, dir, opts)
}

// runChecksums is both jobs: they differ in which files they read and in what
// they do with the hash.
//
// A cancelled context ends it with what it had done, marked Stopped, rather
// than with an error — the files a compute got to have their checksums, and
// the files a verify got to were verified.
func (v *Vault) runChecksums(ctx context.Context, kind ChecksumKind, scope Scope, dir string, opts ChecksumOptions) (*ChecksumReport, error) {
	found, err := v.Descendants(scope, dir)
	if err != nil {
		return nil, err
	}
	report := &ChecksumReport{Kind: kind, Vault: scope, Path: CleanDir(dir), Findings: []ChecksumFinding{}}

	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return nil, ErrLocked
	}
	var entries []*Entry
	var total int64
	for _, e := range found {
		if isLedger(e) {
			continue
		}
		has := e.Hash != ""
		if kind == ChecksumVerify && !has {
			report.NoChecksum++
			continue
		}
		if kind == ChecksumCompute && has {
			continue
		}
		entries = append(entries, copyEntry(e))
		total += e.Size
	}
	v.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path() < entries[j].Path() })
	report.Files = len(entries)

	var done int64
	for i, entry := range entries {
		if ctx.Err() != nil {
			report.Stopped = true
			break
		}
		var within int64
		say := func() {
			if opts.OnProgress != nil {
				opts.OnProgress(ChecksumProgress{
					Kind: kind, Path: entry.Path(), File: i + 1, Files: len(entries),
					Bytes: done + within, Total: total, Flagged: report.flagged(),
				})
			}
		}
		say()
		// Held here too, so a job paused between files does not open the
		// next one first.
		if err := opts.Control.wait(ctx, 0); err != nil {
			report.Stopped = true
			break
		}

		finding := ChecksumFinding{ID: entry.ID, Path: entry.Path(), Size: entry.Size}
		if !entry.Chunked() {
			finding.State = ChecksumSkipped
			finding.Detail = "it is stored in the format SAND used before chunking — convert it first (sand vault convert)"
			report.Checked++
			report.flag(finding)
			continue
		}

		sum, archiveID, recorded, err := v.hashEntry(ctx, entry.ID, opts.Control, func(n int) {
			within += int64(n)
			say()
		})
		switch {
		case errors.Is(err, ErrLocked):
			return nil, err
		case err != nil && ctx.Err() != nil:
			// Cut off mid-file: half a read says nothing about the file.
			report.Stopped = true
		case err != nil:
			finding.State, finding.Detail = ChecksumUnreadable, err.Error()
			report.flag(finding)
		case kind == ChecksumVerify && recorded == "":
			// Lost its checksum since the list was drawn; nothing to hold it to.
			report.NoChecksum++
		case kind == ChecksumVerify && sum != recorded:
			finding.State = ChecksumMismatch
			finding.Expected, finding.Got = recorded, sum
			finding.Detail = "it reads back, but not to the checksum on record — " +
				"Find and fix corrupt files says which part on which cloud is wrong"
			report.flag(finding)
		case kind == ChecksumVerify:
			report.Matched++
		default:
			recordedNow, err := v.RecordChecksum(entry.ID, archiveID, sum)
			switch {
			case errors.Is(err, ErrLocked):
				return nil, err
			case err != nil:
				finding.State, finding.Detail = ChecksumUnreadable, fmt.Sprintf("its checksum could not be recorded: %v", err)
				report.flag(finding)
			case recordedNow:
				report.Computed++
			default:
				// Given one, rewritten or deleted while it was being read.
				// Whichever it was, there is nothing left for this job to do.
				finding.State = ChecksumSkipped
				finding.Detail = "it changed while it was being read, so nothing was recorded — run this again"
				report.flag(finding)
			}
		}
		if report.Stopped {
			break
		}
		report.Checked++
		report.Bytes += entry.Size
		done += entry.Size
	}

	sort.SliceStable(report.Findings, func(i, j int) bool { return report.Findings[i].Path < report.Findings[j].Path })

	// A compute changes what the ledger should say, so it leaves the ledger
	// saying it — even when stopped, for the files it got to.
	if kind == ChecksumCompute && report.Computed > 0 {
		ledger, err := v.WriteChecksumLedger(context.WithoutCancel(ctx), scope)
		if err != nil {
			report.LedgerError = err.Error()
		}
		report.Ledger = ledger
	}
	return report, nil
}

// DropChecksum forgets the checksum on record for a file, leaving it as a
// file recovered without its index is.
//
// It exists for tests outside this package, which have no other way to make
// a file with no checksum: every upload records one. The service never calls
// this.
func (v *Vault) DropChecksum(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return ErrLocked
	}
	_, e, ok := v.scopeOfEntryLocked(id)
	if !ok {
		return fmt.Errorf("no such file: %s", id)
	}
	e.Hash = ""
	return v.persistLocked()
}
