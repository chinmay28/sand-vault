package davfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/chinmay28/sand-vault/internal/vault"
)

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// readFile is a stored file opened for reading. The handler serves it with
// http.ServeContent, which seeks to size up the file and then again for each
// range asked of it — so this has to be a real ReadSeeker, and behind it the
// vault's own random-access read.
type readFile struct {
	entry *vault.Entry
	body  io.ReadSeeker
}

func newReadFile(ctx context.Context, v *vault.Vault, entry *vault.Entry) (*readFile, error) {
	body, opened, err := v.OpenReadSeeker(ctx, entry.ID)
	if err != nil {
		return nil, vaultError(err)
	}
	return &readFile{entry: opened, body: body}, nil
}

func (f *readFile) Read(p []byte) (int, error)                { return f.body.Read(p) }
func (f *readFile) Seek(off int64, whence int) (int64, error) { return f.body.Seek(off, whence) }
func (f *readFile) Close() error                              { return nil }
func (f *readFile) Write([]byte) (int, error)                 { return 0, os.ErrPermission }
func (f *readFile) Stat() (os.FileInfo, error)                { return fileInfo{entry: f.entry}, nil }

func (f *readFile) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// writeFile is a file being stored. The handler copies the PUT body into it and
// then closes it, so the bytes are piped straight into the vault's streaming
// upload as they arrive rather than collected first.
//
// Nothing is committed until Close: the upload scatters as it goes, but the
// index only learns about the file once the whole body has been read. A client
// that hangs up midway leaves nothing behind, because the scatter erases what
// it wrote when it cannot finish.
//
// Whether the whole body arrived is this file's question to answer, not the
// handler's. The library's handlePut copies the body in and then closes the
// file whether or not the copy succeeded, and a close is the one signal the
// upload has for "that was all of it" — so a body cut off at 3 GB of 4 would
// otherwise be hashed, committed and, since a PUT replaces, put in place of
// the whole file it was meant to update. Close therefore compares what was
// written against what the request promised (see expectedSize) and ends the
// upload with an error, not an EOF, when they disagree.
type writeFile struct {
	name string
	ctx  context.Context

	// expected is how many bytes the request said would come, or -1 when it
	// did not say.
	expected int64

	pw   *io.PipeWriter
	done chan uploadResult

	mu      sync.Mutex
	written int64
	closed  bool
	result  uploadResult
}

type uploadResult struct {
	entry    *vault.Entry
	warnings []string
	err      error
}

// newWriteFile begins storing a file. A non-nil prefix is read into the upload
// ahead of anything written, which is how appending works: the file already
// stored goes in first, the new bytes follow, and neither is held in memory.
//
// expected is the number of bytes the caller will write before closing, or -1
// if that is not known; it counts only what is written, not the prefix.
func newWriteFile(ctx context.Context, v *vault.Vault, dir, base string, prefix io.Reader, expected int64) *writeFile {
	pr, pw := io.Pipe()
	f := &writeFile{name: base, ctx: ctx, expected: expected, pw: pw, done: make(chan uploadResult, 1)}

	var source io.Reader = pr
	if prefix != nil {
		source = io.MultiReader(prefix, pr)
	}

	go func() {
		// PUT to a path that already holds a file replaces it, which is what
		// WebDAV means by it.
		entry, warnings, err := v.UploadStream(ctx, mainVault, dir, base, source, vault.UploadOptions{Overwrite: true})
		// Draining matters when the upload gives up early: without it the
		// handler's io.Copy would block forever on a pipe nobody reads.
		if err != nil {
			pr.CloseWithError(err)
		} else {
			pr.Close()
		}
		f.done <- uploadResult{entry: entry, warnings: warnings, err: err}
	}()

	return f
}

func (f *writeFile) Write(p []byte) (int, error) {
	n, err := f.pw.Write(p)
	f.mu.Lock()
	f.written += int64(n)
	f.mu.Unlock()
	return n, err
}

// Close finishes the upload and reports whether it committed. The handler
// checks this, so a failed scatter becomes a failed PUT rather than a silent
// loss.
func (f *writeFile) Close() error {
	f.mu.Lock()
	if f.closed {
		defer f.mu.Unlock()
		return f.result.err
	}
	f.closed = true
	written := f.written
	f.mu.Unlock()

	// Closing the write end is what tells the upload the body has ended — and
	// how it is closed is what tells it whether the body was whole. A plain
	// Close is a clean EOF, which the upload takes as the whole file; closing
	// with an error reaches it as a failed read, so it stores nothing and the
	// file that was there stays.
	//
	// A context already done is the client gone: the connection dropped
	// partway, which is the only way a chunked body ever announces that it
	// was cut short. A count that does not match the declared size is the
	// same thing told differently.
	var short error
	switch {
	case f.ctx.Err() != nil:
		short = f.ctx.Err()
	case f.expected >= 0 && written != f.expected:
		short = fmt.Errorf("the body ended after %d of %d bytes", written, f.expected)
	}

	if short != nil {
		f.pw.CloseWithError(short)
	} else {
		f.pw.Close()
	}
	result := <-f.done
	if short != nil {
		// Reported in preference to what the upload said about the read it
		// was handed, which is this same error in spooling terms.
		result.err = short
	}

	f.mu.Lock()
	f.result = result
	f.mu.Unlock()

	if result.err != nil {
		return fmt.Errorf("storing %s: %w", f.name, result.err)
	}
	return nil
}

// Stat answers before the upload has committed, because the handler asks for it
// between the last write and the close in order to build an ETag. It reports
// what has been written so far, which is the whole body by then.
func (f *writeFile) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.result.entry != nil {
		return fileInfo{entry: f.result.entry}, nil
	}
	return pendingInfo{name: f.name, size: f.written}, nil
}

func (f *writeFile) Read([]byte) (int, error)           { return 0, os.ErrPermission }
func (f *writeFile) Seek(int64, int) (int64, error)     { return 0, os.ErrInvalid }
func (f *writeFile) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }

// pendingInfo describes a file that is still being written.
type pendingInfo struct {
	name string
	size int64
}

func (p pendingInfo) Name() string       { return p.name }
func (p pendingInfo) Size() int64        { return p.size }
func (p pendingInfo) Mode() os.FileMode  { return 0o644 }
func (p pendingInfo) ModTime() time.Time { return time.Now().UTC() }
func (p pendingInfo) IsDir() bool        { return false }
func (p pendingInfo) Sys() any           { return nil }
