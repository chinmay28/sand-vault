package vault

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

// Where the read history lives between runs.
//
// Not in the vault file. Counting a read into that would mean a write on every
// chunk of every stream — hundreds of them for one film — to the one file that
// every other thing here depends on being intact, and a counter is not worth
// putting that in the path of. So the figures go in a sidecar of their own,
// beside the vault and named after it, written at most every half minute and
// whenever the vault is locked.
//
// It is encrypted, under a key derived from the vault's data key, for the same
// reason the index is: which clouds this vault is on, when it is read, and how
// much comes off each of them is exactly the shape of somebody's life that the
// rest of this design refuses to leave lying around. It also means the history
// is readable only while the vault is open, which is the right answer to "who
// can see it" and costs nothing — nobody reads their own statistics with the
// vault locked.
//
// Losing it is survivable by design. A sidecar that will not open — a password
// changed while the process was dying, a file truncated by a full disk — is
// reported and started again rather than treated as damage: the vault is not
// less recoverable for having forgotten how fast Dropbox was last March.
const (
	// readHistoryVersion is the format the current build writes. A file from a
	// later one is left alone rather than overwritten with less.
	readHistoryVersion = 1

	// readHistoryPurpose separates this key from every other use of the data
	// key. See crypto.DerivePurposeKey.
	readHistoryPurpose = "sand-read-history-v1"

	// dayFormat is how a bucket is named. Sortable as a string, which is what
	// lets a month be a prefix rather than a parse.
	dayFormat = "2006-01-02"

	// historyDays is how many daily buckets are kept. A year window needs at
	// most 366; the rest is so that the first days of January still have last
	// year's shape behind them. All time is a total of its own and does not
	// thin out with them.
	historyDays = 400

	// flushEvery is the most often the sidecar is written while reads are
	// happening. A crash loses that much counting and nothing else.
	flushEvery = 30 * time.Second
)

// readHistoryFile is the sidecar's contents. It is the recorder's own state,
// which is why the shapes are shared rather than translated: a file that has to
// be converted on the way in and out is a file that grows a second definition
// of what a day is.
type readHistoryFile struct {
	Version int                        `json:"version"`
	Since   time.Time                  `json:"since"`
	Names   map[string]readAccountName `json:"accounts"`
	Total   readBucket                 `json:"total"`
	Days    map[string]*readBucket     `json:"days"`
}

// readHistoryPath is the vault's own path with .reads on the end, so a vault
// moved or copied by hand takes its history with it if the mover wants it and
// leaves it behind if they do not.
func readHistoryPath(vaultPath string) string { return vaultPath + ".reads" }

func removeReadHistory(vaultPath string) error {
	if err := os.Remove(readHistoryPath(vaultPath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("forgetting the read history: %w", err)
	}
	return nil
}

// snapshotLocked copies what is counted into the shape that is written. The
// copy is deep, so the write happens off whatever the read path does next.
func (s *readStats) snapshot() (*readHistoryFile, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked()

	file := &readHistoryFile{
		Version: readHistoryVersion,
		Since:   s.since,
		Names:   make(map[string]readAccountName, len(s.names)),
		Total:   copyBucket(&s.total),
		Days:    make(map[string]*readBucket, len(s.days)),
	}
	for id, name := range s.names {
		file.Names[id] = name
	}
	for key, bucket := range s.days {
		day := copyBucket(bucket)
		file.Days[key] = &day
	}
	return file, s.rev
}

func copyBucket(b *readBucket) readBucket {
	out := readBucket{Races: b.Races, Shortfalls: b.Shortfalls}
	if len(b.Accounts) > 0 {
		out.Accounts = make(map[string]*readCounts, len(b.Accounts))
		for id, counts := range b.Accounts {
			c := *counts
			out.Accounts[id] = &c
		}
	}
	return out
}

// merge folds a loaded file into what is already counted. Additive rather than
// assigning, so a history read after a fetch has been recorded — which is what
// happens if a read somehow beats the unlock that loads it — keeps both.
func (s *readStats) merge(file *readHistoryFile) {
	if s == nil || file == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if !file.Since.IsZero() && (s.since.IsZero() || file.Since.Before(s.since)) {
		s.since = file.Since
	}
	for id, name := range file.Names {
		if _, already := s.names[id]; !already {
			s.names[id] = name
		}
	}
	s.total.add(&file.Total)
	for key, bucket := range file.Days {
		day := s.days[key]
		if day == nil {
			day = &readBucket{}
			s.days[key] = day
		}
		day.add(bucket)
	}
	// The day cache may now point at a bucket the merge replaced nothing of,
	// but the key is what it is looked up by, so only the pointer needs
	// re-taking. Clearing it is the cheapest way to say that.
	s.dayKey = ""
	s.pruneLocked()
}

// pruneLocked drops the daily buckets past the horizon. Nothing is lost that
// all time reports: every fetch was counted into the total when it happened.
func (s *readStats) pruneLocked() {
	if len(s.days) <= historyDays {
		return
	}
	keys := make([]string, 0, len(s.days))
	for key := range s.days {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys[:len(keys)-historyDays] {
		delete(s.days, key)
	}
}

// recorded reports whether there is anything at all to write. A vault nobody
// has read yet gets no sidecar rather than an empty one.
func (s *readStats) recorded() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.since.IsZero()
}

// abandonFlush clears the in-flight mark for a save that turned out not to be
// possible or not to be needed. The clock is moved on with it, so a vault that
// is locked while something is still being counted does not spawn a goroutine
// per shard to find that out again.
func (s *readStats) abandonFlush(at time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushing = false
	s.lastFlush = at
}

// pending reports whether anything has been counted that is not yet saved.
// Nothing else in the vault writes a file it has no changes for, and neither
// does this: locking a vault nobody read is not a reason to rewrite anything.
func (s *readStats) pending() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirty
}

// markFlushed clears the dirty flag if nothing was recorded since the snapshot
// the write was built from. rev is what snapshot returned.
func (s *readStats) markFlushed(rev uint64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushing = false
	s.lastFlush = at
	s.savedRev = rev
	if s.rev == rev {
		s.dirty = false
	}
}

func (s *readStats) maybeFlushLocked(now time.Time) {
	if !s.dirty || s.flush == nil || s.flushing {
		return
	}
	if !s.lastFlush.IsZero() && now.Sub(s.lastFlush) < flushEvery {
		return
	}
	s.flushing = true
	// On a goroutine of its own: this is called from the read path, holding
	// the recorder's lock, and a read has no business waiting on a disk write
	// of something nobody has asked to see.
	s.saving.Add(1)
	go func() {
		defer s.saving.Done()
		s.flush()
	}()
}

// loadReadHistoryLocked opens the sidecar under the current data key and folds
// it into what is counted. The caller holds the write lock, which is where the
// key is.
//
// Anything wrong with the file is said once and stepped over. The history is a
// convenience; refusing to open a vault because last month's figures will not
// decrypt would be the tail wagging the dog.
func (v *Vault) loadReadHistoryLocked() {
	file := openReadHistory(readHistoryPath(v.path), v.dataKey)
	if file != nil {
		v.reads.merge(file)
	}
}

// resealReadHistoryLocked rewrites the sidecar from one data key to another
// without touching what is being counted in memory.
//
// This is for the password change that arrives at a locked vault, where the
// file is the only copy of the figures and the key it is sealed under is about
// to stop existing. An unlocked vault does not come through here: it holds
// everything the file holds and more, and writing that out is both fresher and
// simpler.
func (v *Vault) resealReadHistoryLocked(oldKey, newKey []byte) {
	path := readHistoryPath(v.path)
	file := openReadHistory(path, oldKey)
	if file == nil {
		return
	}
	if err := writeReadHistory(v.path, file, newKey); err != nil {
		log.Printf("could not re-seal the read history under the new password: %v", err)
	}
}

// openReadHistory reads and decrypts the sidecar, or returns nil.
//
// Anything wrong with the file is said once and stepped over. The history is a
// convenience; refusing to open a vault because last month's figures will not
// decrypt would be the tail wagging the dog.
func openReadHistory(path string, dataKey []byte) *readHistoryFile {
	if dataKey == nil {
		return nil
	}

	sealed, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("could not read the read history at %s: %v", path, err)
		}
		return nil
	}
	if len(sealed) <= crypto.NonceSize {
		log.Printf("the read history at %s is too short to be one; starting again", path)
		return nil
	}

	key, err := crypto.DerivePurposeKey(dataKey, readHistoryPurpose)
	if err != nil {
		return nil
	}
	defer crypto.ZeroBytes(key)

	plain, err := crypto.Decrypt(key, sealed[:crypto.NonceSize], sealed[crypto.NonceSize:], []byte(readHistoryPurpose))
	if err != nil {
		// The likeliest cause by far is a password change that happened while
		// nothing could rewrite this: the data key it was sealed under is gone,
		// and so is the history it was keeping.
		log.Printf("the read history at %s does not open under this vault's key; starting again", path)
		return nil
	}
	defer crypto.ZeroBytes(plain)

	var file readHistoryFile
	if err := json.Unmarshal(plain, &file); err != nil {
		log.Printf("the read history at %s could not be read; starting again: %v", path, err)
		return nil
	}
	if file.Version > readHistoryVersion {
		log.Printf("the read history at %s was written by a later version of SAND; leaving it alone", path)
		return nil
	}
	return &file
}

// flushReadHistory writes the sidecar if anything has been counted since the
// last one. It is what the recorder calls when it decides enough time has
// passed.
func (v *Vault) flushReadHistory() {
	v.mu.RLock()
	defer v.mu.RUnlock()
	v.flushReadHistoryLocked()
}

// AwaitReadHistory waits for a save already under way. A process about to exit
// calls it so that the file is on disk before it goes — and so that nothing
// appears in a directory somebody is in the middle of removing.
func (v *Vault) AwaitReadHistory() {
	if v.reads == nil {
		return
	}
	v.reads.saving.Wait()
}

// flushReadHistoryLocked is the same with the vault's lock already held, which
// is how locking the vault and changing its password both get a last write in
// before the key they would need goes away.
func (v *Vault) flushReadHistoryLocked() { v.saveReadHistoryLocked(false) }

// saveReadHistoryLocked writes the sidecar. force writes it even with nothing
// new counted, which is what a password change needs: the figures are unchanged
// and the key they have to be sealed under is not.
func (v *Vault) saveReadHistoryLocked(force bool) {
	// Every way out of here that does not write has to say so, or the recorder
	// goes on believing a save is in flight and never starts another.
	if v.reads == nil {
		return
	}
	if v.dataKey == nil || !v.reads.recorded() || (!force && !v.reads.pending()) {
		v.reads.abandonFlush(time.Now())
		return
	}

	// One writer at a time, so that a save the recorder started and a save
	// prompted by locking the vault cannot land out of order and leave the
	// older of the two snapshots on disk.
	v.readsMu.Lock()
	defer v.readsMu.Unlock()

	file, rev := v.reads.snapshot()
	err := writeReadHistory(v.path, file, v.dataKey)
	v.reads.markFlushed(rev, time.Now())
	if err != nil {
		log.Printf("could not save the read history: %v", err)
	}
}

func writeReadHistory(vaultPath string, file *readHistoryFile, dataKey []byte) error {
	plain, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("serializing the read history: %w", err)
	}
	defer crypto.ZeroBytes(plain)

	key, err := crypto.DerivePurposeKey(dataKey, readHistoryPurpose)
	if err != nil {
		return err
	}
	defer crypto.ZeroBytes(key)

	nonce, err := crypto.GenerateNonce()
	if err != nil {
		return err
	}
	sealed, err := crypto.Encrypt(key, nonce, plain, []byte(readHistoryPurpose))
	if err != nil {
		return fmt.Errorf("sealing the read history: %w", err)
	}

	return writeFileAtomically(readHistoryPath(vaultPath), append(nonce, sealed...))
}

// writeFileAtomically replaces a file through a temporary one beside it, so an
// interrupted write leaves the previous contents rather than half of the new.
//
// The vault file has its own copy of this in writeStore, deliberately not
// shared: that path is the one thing here that must never be refactored for
// the convenience of something as disposable as a statistics file.
func writeFileAtomically(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".sand-reads-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file beside %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}
