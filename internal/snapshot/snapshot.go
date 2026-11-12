package snapshot

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

// Info is what a snapshot says about itself, sealed at the front of it so
// that it can be read without reading the rest.
type Info struct {
	Version int `json:"version"`

	// ID is the snapshot's own, random and in hex. The first eight digits are
	// in its name.
	ID string `json:"id"`

	// Path is the folder this is a snapshot of, as it was called where it was
	// taken, and Name is that folder's own name.
	Path string `json:"path"`
	Name string `json:"name"`

	TakenAt time.Time `json:"taken_at"`

	// What is inside, counted before it was packed.
	Files   int   `json:"files"`
	Folders int   `json:"folders"`
	Bytes   int64 `json:"bytes"`

	// Tool is what wrote it.
	Tool string `json:"tool,omitempty"`
}

// maxInfoSize bounds the description at the front of a snapshot. It is a few
// hundred bytes; a length beyond this is not a description.
const maxInfoSize = 1 << 20

// hashRecord is the PAX record a file's SHA-256 travels in.
const hashRecord = "SAND.sha256"

// Options is how a snapshot is written.
type Options struct {
	// PartSize is the most one part file may hold, header included. Zero is
	// no limit, and the snapshot is one file however big it is.
	PartSize int64

	// KDF is what turning the password into a key costs. The zero value is
	// DefaultKDF.
	KDF crypto.Argon2Params

	// Threads is how many threads the compressor may use. Zero is one.
	Threads int
}

// DefaultKDF is what a snapshot's password is stretched with: the costs a
// recovery kit is sealed under, for the reason a kit is. A snapshot is opened
// a handful of times in its life and may sit for years on storage somebody
// else runs, under a password a person chose — which is the case a memory-hard
// function exists for, and a second and a half is nothing at the only moments
// it is paid.
func DefaultKDF() crypto.Argon2Params { return crypto.KitArgon2Params() }

// File is one file going into a snapshot.
type File struct {
	// Path is where it sits under the folder being snapshotted: relative,
	// slash-separated, "2019/one.jpg".
	Path string
	Size int64
	Mod  time.Time

	// SHA256 is the file's own, in hex, when it is known. It rides along so
	// that a restore can tell a file it already has without reading it.
	SHA256 string
}

// Result is a snapshot that has been written.
type Result struct {
	Name  string   `json:"name"`
	Parts []string `json:"parts"`

	// Bytes is what the parts add up to — what the snapshot costs to keep.
	Bytes int64 `json:"bytes"`
}

// Writer packs a tree into a snapshot. Folders and files are added in the
// order they should come back; Close finishes it and Abort throws it away.
// A Writer is not safe for use from more than one goroutine.
type Writer struct {
	split *splitter
	seal  *sealer
	zw    *zstd.Encoder
	tw    *tar.Writer
}

// Create starts a snapshot in store, sealed under password.
//
// info describes it; its Version and ID are filled in here. Nothing is stored
// under a name until the first part is full or the snapshot is closed, so a
// Create that is abandoned before either leaves nothing behind.
func Create(ctx context.Context, store Store, password string, info Info, opts Options) (*Writer, error) {
	if password == "" {
		return nil, errors.New("a snapshot needs a password")
	}
	if opts.PartSize != 0 && opts.PartSize < MinPartSize {
		return nil, fmt.Errorf("a part cannot be smaller than %d bytes", MinPartSize)
	}
	kdf := opts.KDF
	if kdf.Time == 0 {
		kdf = DefaultKDF()
	}
	kdf.SaltLen, kdf.KeyLen = saltSize, 32

	header := partHeader{kdf: kdf}
	if _, err := io.ReadFull(rand.Reader, header.salt[:]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rand.Reader, header.id[:]); err != nil {
		return nil, err
	}
	if info.TakenAt.IsZero() {
		info.TakenAt = time.Now()
	}
	info.TakenAt = info.TakenAt.UTC().Truncate(time.Second)
	info.Version = Version
	info.ID = hex.EncodeToString(header.id[:])

	key := crypto.DeriveKey(password, header.salt[:], kdf)
	defer zero(key)

	split := &splitter{
		ctx:    ctx,
		store:  store,
		base:   baseName(info.TakenAt, header.id),
		header: header,
		max:    opts.PartSize,
	}
	seal, err := newSealer(split, key, header.id)
	if err != nil {
		return nil, err
	}

	// The description goes in ahead of the compressed stream and outside it,
	// so that reading it back is one frame and no decompressor.
	described, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	if _, err := seal.Write(binary.BigEndian.AppendUint32(nil, uint32(len(described)))); err != nil {
		return nil, err
	}
	if _, err := seal.Write(described); err != nil {
		return nil, err
	}

	threads := opts.Threads
	if threads < 1 {
		threads = 1
	}
	// One stream over the whole tree rather than one per file, at the level
	// that looks hardest: what a folder of similar files has in common is
	// then stored once, which a file compressed on its own can never find.
	zw, err := zstd.NewWriter(seal,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderConcurrency(threads))
	if err != nil {
		return nil, err
	}
	return &Writer{split: split, seal: seal, zw: zw, tw: tar.NewWriter(zw)}, nil
}

// AddFolder records a folder, so that one with nothing in it survives.
func (w *Writer) AddFolder(rel string, mod time.Time) error {
	rel, err := cleanRel(rel)
	if err != nil {
		return err
	}
	return w.tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     rel + "/",
		Mode:     0o755,
		ModTime:  mod,
		Format:   tar.FormatPAX,
	})
}

// AddFile packs one file, reading exactly f.Size bytes of r. A reader that
// yields fewer, or more, is an error and the snapshot should be aborted: the
// archive has already said how long the file is.
func (w *Writer) AddFile(f File, r io.Reader) error {
	rel, err := cleanRel(f.Path)
	if err != nil {
		return err
	}
	header := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     rel,
		Size:     f.Size,
		Mode:     0o644,
		ModTime:  f.Mod,
		Format:   tar.FormatPAX,
	}
	if f.SHA256 != "" {
		header.PAXRecords = map[string]string{hashRecord: f.SHA256}
	}
	if err := w.tw.WriteHeader(header); err != nil {
		return err
	}
	n, err := io.Copy(w.tw, r)
	if err != nil {
		return err
	}
	if n != f.Size {
		return fmt.Errorf("%s: read %d of %d bytes", f.Path, n, f.Size)
	}
	return nil
}

// Written is how many bytes of part files there are so far. It and Parts are
// the two things here safe to ask from another goroutine, or while a file is
// being added: they are what a progress bar reads.
func (w *Writer) Written() int64 { return w.split.written.Load() }

// Parts is how many part files have been started.
func (w *Writer) Parts() int { return int(w.split.started.Load()) }

// Close finishes the snapshot and stores its last part. On an error the
// caller should Abort: what has been stored is not a snapshot.
func (w *Writer) Close() (Result, error) {
	if err := w.tw.Close(); err != nil {
		return Result{}, err
	}
	if err := w.zw.Close(); err != nil {
		return Result{}, err
	}
	if err := w.seal.Close(); err != nil {
		return Result{}, err
	}
	if err := w.split.Close(); err != nil {
		return Result{}, err
	}
	return Result{Name: w.split.base, Parts: w.split.names, Bytes: w.split.bytes}, nil
}

// Abort throws the snapshot away, erasing the parts already stored. The
// context is for that erasing, and should not be the one the snapshot was
// being written under if that one has ended.
func (w *Writer) Abort(ctx context.Context) {
	w.zw.Close()
	w.split.abort(ctx)
}

// Item is one entry of a snapshot being read back.
type Item struct {
	// Path is relative to the folder the snapshot is of, as File.Path was.
	Path string
	Dir  bool
	Size int64
	Mod  time.Time

	// SHA256 is the file's, in hex, if the snapshot recorded one.
	SHA256 string

	// Body is the file's contents, good until the next call to Next. Nil for
	// a folder. It need not be read: Next skips what was left.
	Body io.Reader
}

// Reader reads a snapshot back, an item at a time, in the order they were
// added. A Reader is not safe for use from more than one goroutine.
type Reader struct {
	info Info
	join *joiner
	open *opener
	zr   *zstd.Decoder
	tr   *tar.Reader
}

// Open opens a snapshot under its password, reading far enough to know the
// password is right and what the snapshot holds — the first megabyte or so of
// the first part, however big the snapshot is.
//
// An incomplete set is refused before anything is read. ErrPassword is the
// password being wrong, or the first part being damaged.
func Open(ctx context.Context, store Store, set Set, password string) (*Reader, error) {
	if !set.Complete {
		return nil, fmt.Errorf("%w: not all of its parts are there", ErrIncomplete)
	}
	join, err := openJoiner(ctx, store, set.Parts)
	if err != nil {
		return nil, err
	}

	header := join.first
	key := crypto.DeriveKey(password, header.salt[:], header.kdf)
	defer zero(key)

	open, err := newOpener(join, key, header.id)
	if err != nil {
		join.Close()
		return nil, err
	}

	var length [4]byte
	if _, err := io.ReadFull(open, length[:]); err != nil {
		join.Close()
		return nil, readError(err)
	}
	size := binary.BigEndian.Uint32(length[:])
	if size > maxInfoSize {
		join.Close()
		return nil, fmt.Errorf("%w: its description is not one", ErrDamaged)
	}
	described := make([]byte, size)
	if _, err := io.ReadFull(open, described); err != nil {
		join.Close()
		return nil, readError(err)
	}
	var info Info
	if err := json.Unmarshal(described, &info); err != nil {
		join.Close()
		return nil, fmt.Errorf("%w: its description does not read", ErrDamaged)
	}

	zr, err := zstd.NewReader(open, zstd.WithDecoderConcurrency(1))
	if err != nil {
		join.Close()
		return nil, err
	}
	return &Reader{info: info, join: join, open: open, zr: zr, tr: tar.NewReader(zr)}, nil
}

// Info is what the snapshot says about itself.
func (r *Reader) Info() Info { return r.info }

// Next is the next folder or file, or io.EOF once the snapshot has been read
// to its end — which is the only point at which it is known to be whole. A
// caller that stops early has read a prefix it can trust and no more.
func (r *Reader) Next() (*Item, error) {
	for {
		header, err := r.tr.Next()
		if err == io.EOF {
			return nil, r.finish()
		}
		if err != nil {
			return nil, readError(err)
		}

		// A name is only ever one this package wrote, but it is read from a
		// file somebody else has been holding: anything that is not a plain
		// path below the folder is refused rather than followed.
		rel, err := cleanRel(header.Name)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDamaged, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			return &Item{Path: rel, Dir: true, Mod: header.ModTime}, nil
		case tar.TypeReg:
			return &Item{
				Path:   rel,
				Size:   header.Size,
				Mod:    header.ModTime,
				SHA256: header.PAXRecords[hashRecord],
				Body:   r.tr,
			}, nil
		}
		// Nothing else is ever written, and nothing else is restored.
	}
}

// finish reads what follows the archive, which is nothing — and in doing so
// reaches the frame that says the stream is whole, or finds it missing.
func (r *Reader) finish() error {
	if _, err := io.Copy(io.Discard, r.zr); err != nil {
		return readError(err)
	}
	return io.EOF
}

// Close lets go of the part being read. It does not say whether the snapshot
// was whole: only reading it to io.EOF does.
func (r *Reader) Close() error {
	r.zr.Close()
	return r.join.Close()
}

// readError turns a failure somewhere under the archive into what it means.
// Ours pass through; a decompressor's or an archive's complaint about bytes
// that opened cleanly is damage to something sealed before it was damaged,
// which no key would have let through — so it is reported as what it is
// rather than dressed up.
func readError(err error) error {
	if errors.Is(err, ErrPassword) || errors.Is(err, ErrDamaged) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("reading the snapshot: %w", err)
}

// cleanRel checks a path is a plain one below the folder, and returns it
// without a trailing slash.
func cleanRel(rel string) (string, error) {
	trimmed := strings.TrimSuffix(rel, "/")
	if trimmed == "" || trimmed == "." || trimmed == ".." ||
		strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "../") ||
		strings.ContainsRune(trimmed, 0) || path.Clean(trimmed) != trimmed {
		return "", fmt.Errorf("%q is not a path inside the folder", rel)
	}
	return trimmed, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
