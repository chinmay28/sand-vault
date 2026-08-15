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
type writeFile struct {
	name string

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

func newWriteFile(ctx context.Context, v *vault.Vault, dir, base string) *writeFile {
	pr, pw := io.Pipe()
	f := &writeFile{name: base, pw: pw, done: make(chan uploadResult, 1)}

	go func() {
		// PUT to a path that already holds a file replaces it, which is what
		// WebDAV means by it.
		entry, warnings, err := v.UploadStream(ctx, dir, base, pr, vault.UploadOptions{Overwrite: true})
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
	f.mu.Unlock()

	// Closing the write end is what tells the upload the body has ended.
	f.pw.Close()
	result := <-f.done

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
