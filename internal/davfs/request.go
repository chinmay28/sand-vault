package davfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
)

// requestInfo is what the endpoint learns about a request before the WebDAV
// library takes it, carried in the request context for the filesystem to read
// back while serving it.
//
// The context is the only thing that reaches every FileSystem call the library
// makes on a request's behalf: handlePut opens a file and copies the body in
// without ever telling the filesystem how long the body was meant to be, and
// moveFiles asks for the destination to be removed without saying that a
// rename is about to put something in its place. Both of those facts decide
// whether a file is kept or lost, so they are worked out here, up front, and
// read back where they matter.
type requestInfo struct {
	method string

	// bodySize is what a PUT's Content-Length declares, or -1 when the request
	// does not say — chunked transfer encoding, where only the connection
	// dropping (which cancels the context) can tell a short body from a whole
	// one.
	bodySize int64

	// src and dst are a COPY's or MOVE's two ends, as clean vault paths. A
	// write landing at or under dst is traced back through them to the file
	// it is copying, whose size is what the write has to reach; and the file
	// already at dst is recognised through them when the library asks for it
	// to be removed ahead of the replacement.
	src, dst string

	mu sync.Mutex
	// aside is where the previous occupant of dst was moved to make room, or
	// "" when there was none. See FileSystem.setAside.
	aside    string
	asideDir bool
}

type requestKey struct{}

func withRequestInfo(ctx context.Context, info *requestInfo) context.Context {
	return context.WithValue(ctx, requestKey{}, info)
}

func requestInfoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestKey{}).(*requestInfo)
	return info
}

// stripPrefix takes the share's mount point off a request path the way the
// library does, so the two agree about which vault path a URL names.
func stripPrefix(prefix, p string) (string, bool) {
	if prefix == "" {
		return p, true
	}
	if rest := strings.TrimPrefix(p, prefix); len(rest) < len(p) {
		return rest, true
	}
	return "", false
}

// describe works out what a request is going to ask the filesystem for, from
// the headers alone, before anything is done. It returns nil for a request
// the filesystem needs nothing extra about, and for one whose paths the
// library is going to refuse anyway.
func describe(prefix string, r *http.Request) *requestInfo {
	switch r.Method {
	case http.MethodPut:
		return &requestInfo{method: r.Method, bodySize: r.ContentLength}

	case "COPY", "MOVE":
		raw, ok := stripPrefix(prefix, r.URL.Path)
		if !ok {
			return nil
		}
		src, err := cleanName(raw)
		if err != nil {
			return nil
		}
		u, err := url.Parse(r.Header.Get("Destination"))
		if err != nil {
			return nil
		}
		raw, ok = stripPrefix(prefix, u.Path)
		if !ok {
			return nil
		}
		dst, err := cleanName(raw)
		if err != nil {
			return nil
		}
		return &requestInfo{method: r.Method, src: src, dst: dst}
	}
	return nil
}

// expectedSize is how many bytes a write opened at clean has to receive before
// it may be committed, or -1 when nothing in the request says.
//
// For a PUT that is the declared Content-Length. For a COPY it is the size of
// the file being copied, found by mapping the destination path back onto the
// source: the library copies a collection by recursing with the same context,
// so a write under dst corresponds to the file at the same place under src.
func (fs *FileSystem) expectedSize(ctx context.Context, clean string) int64 {
	info := requestInfoFrom(ctx)
	if info == nil {
		return -1
	}
	switch info.method {
	case http.MethodPut:
		return info.bodySize
	case "COPY":
		source := ""
		switch {
		case clean == info.dst:
			source = info.src
		case strings.HasPrefix(clean, info.dst+"/"):
			source = info.src + clean[len(info.dst):]
		default:
			return -1
		}
		entry, err := fs.Vault.EntryByPath(mainVault, source)
		if err != nil {
			return -1
		}
		return entry.Size
	}
	return -1
}

// replaces reports whether removing clean is the library clearing the
// destination of this COPY or MOVE ahead of what is about to land there —
// rather than a DELETE, which means exactly what it says.
func (info *requestInfo) replaces(clean string) bool {
	return info != nil && (info.method == "COPY" || info.method == "MOVE") && clean == info.dst
}

// setAside moves whatever is at clean out of the way under a temporary name in
// the same folder, instead of erasing it.
//
// The library implements Overwrite: T as "delete the destination, then put the
// new thing there" — which for a filesystem on disk is fine, because a failed
// rename leaves the source where it was and the deletion is cheap to undo from
// a backup. For the vault a delete erases parts from every account, and there
// is no undo. If the rename then fails, or the copy's body comes up short, the
// file that was meant to be replaced is simply gone while the one meant to
// replace it is still where it started. Moving it aside instead costs one index
// write, and finishReplacement decides afterwards whether it goes or comes
// back.
func (fs *FileSystem) setAside(ctx context.Context, info *requestInfo, clean string) error {
	dir, base := split(clean)

	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	asideName := fmt.Sprintf(".%s.sand-replaced-%s", base, hex.EncodeToString(nonce[:]))
	asidePath := path.Join(dir, asideName)

	if fs.Vault.FolderExists(mainVault, clean) {
		if err := fs.Vault.MoveFolder(ctx, mainVault, clean, asidePath); err != nil {
			return vaultError(err)
		}
		info.mu.Lock()
		info.aside, info.asideDir = asidePath, true
		info.mu.Unlock()
		return nil
	}

	entry, err := fs.Vault.EntryByPath(mainVault, clean)
	if err != nil {
		return vaultError(err)
	}
	if _, err := fs.Vault.Move(ctx, entry.ID, "", asideName); err != nil {
		return vaultError(err)
	}
	info.mu.Lock()
	info.aside, info.asideDir = asidePath, false
	info.mu.Unlock()
	return nil
}

// finishReplacement settles what was set aside once the request has been
// answered: erased if the replacement landed, put back if it did not.
//
// It runs after the response has gone out, on a context of its own, because
// the request's context is done by then and neither outcome is something to
// stop halfway. Putting the file back only happens into an empty slot: a
// collection copy that failed partway leaves its half-made folder at dst, and
// erasing that to make room would be the very thing this exists to avoid, so
// the file stays under its aside name and the log says where.
func (fs *FileSystem) finishReplacement(info *requestInfo, landed bool) {
	info.mu.Lock()
	aside, isDir := info.aside, info.asideDir
	info.mu.Unlock()
	if aside == "" {
		return
	}
	ctx := context.Background()

	if landed {
		var err error
		if isDir {
			_, err = fs.Vault.Rmdir(ctx, mainVault, aside, true, nil)
		} else if entry, lookup := fs.Vault.EntryByPath(mainVault, aside); lookup == nil {
			_, err = fs.Vault.Delete(ctx, entry.ID)
		} else {
			err = lookup
		}
		if err != nil {
			log.Printf("webdav %s %s: the replaced copy is still at %s: %v", info.method, info.dst, aside, err)
		}
		return
	}

	occupied := fs.Vault.FolderExists(mainVault, info.dst)
	if _, err := fs.Vault.EntryByPath(mainVault, info.dst); err == nil {
		occupied = true
	}
	if occupied {
		log.Printf("webdav %s %s: failed, and something is already at the destination — the file it was to replace is kept at %s",
			info.method, info.dst, aside)
		return
	}

	var err error
	if isDir {
		err = fs.Vault.MoveFolder(ctx, mainVault, aside, info.dst)
	} else if entry, lookup := fs.Vault.EntryByPath(mainVault, aside); lookup == nil {
		_, base := split(info.dst)
		_, err = fs.Vault.Move(ctx, entry.ID, "", base)
	} else {
		err = lookup
	}
	if err != nil {
		log.Printf("webdav %s %s: failed, and the file it was to replace could not be put back from %s: %v",
			info.method, info.dst, aside, err)
	}
}

// statusRecorder notes the status a handler answered with, which is how the
// endpoint learns whether a COPY or MOVE landed: the library reports that to
// the client and to nobody else.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) landed() bool {
	return s.status >= 200 && s.status < 300
}
