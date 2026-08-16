package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"mime"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Stream tickets: a link something outside the browser can open.
//
// Handing a film to VLC means handing it an address, and VLC has none of what
// authenticates the app. The session cookie is HttpOnly and SameSite=Strict, so
// it is neither readable by the page nor carried by another program; the WebDAV
// share is authenticated by the vault password itself, which is not a thing to
// put in a URL and on the clipboard. Both are the wrong shape for "play this
// one file over there".
//
// So a ticket: 32 random bytes standing for one file, minted by a session that
// has already unlocked the vault, and good for that file and nothing else. It
// is a bearer token — anyone holding the link can play that file until it
// expires — which is why it names a single file rather than the vault, expires
// on its own, and dies the moment the vault locks.

// DefaultStreamTTL is how long a stream link survives without being used.
//
// Long enough to finish a film and come back to it, and it slides forward on
// every request, so a link in use never expires underneath the player holding
// it. A link that is put down does.
const DefaultStreamTTL = 12 * time.Hour

// streamTicket is one minted link: the file it plays, and when it stops.
type streamTicket struct {
	fileID string
	expiry time.Time
}

// streamStore holds the live tickets. It is deliberately memory-only: a link
// that outlived the process would be a link that outlived the unlocked vault it
// was minted from.
type streamStore struct {
	ttl time.Duration

	mu      sync.Mutex
	tickets map[string]streamTicket
}

func newStreamStore(ttl time.Duration) *streamStore {
	if ttl <= 0 {
		ttl = DefaultStreamTTL
	}
	return &streamStore{ttl: ttl, tickets: map[string]streamTicket{}}
}

// issue mints a ticket for one file and returns the token and its deadline.
func (s *streamStore) issue(fileID string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expiry := time.Now().Add(s.ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets[token] = streamTicket{fileID: fileID, expiry: expiry}
	return token, expiry, nil
}

// lookup resolves a token to the file it plays and pushes its deadline out,
// exactly as a request on a session extends that session.
func (s *streamStore) lookup(token string) (string, bool) {
	if token == "" {
		return "", false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for existing, ticket := range s.tickets {
		if now.After(ticket.expiry) {
			delete(s.tickets, existing)
			continue
		}
		// Compared in constant time and found by walking the map rather than
		// indexing it, so the lookup takes the same shape whichever token
		// arrives. The map holds a handful of entries; a film's range requests
		// are answered from cloud accounts, not from here.
		if subtle.ConstantTimeCompare([]byte(existing), []byte(token)) == 1 {
			ticket.expiry = now.Add(s.ttl)
			s.tickets[existing] = ticket
			return ticket.fileID, true
		}
	}
	return "", false
}

// clear drops every ticket. Locking the vault takes the keys out of memory, so
// nothing minted before it can read anything after it — the links are voided
// rather than left to fail one request at a time.
func (s *streamStore) clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets = map[string]streamTicket{}
}

// streamPath is where a ticket plays from.
//
// The file's own name is the last segment, and it is there for the player: VLC
// and everything like it pick a demuxer off the extension before a byte
// arrives, so an address ending in an opaque token is one a player will guess
// wrong about. Escaped, because a stored name is whatever the file was called.
func streamPath(token, name string) string {
	return "/stream/" + token + "/" + url.PathEscape(name)
}

// streamLinkResponse is what the browser gets back to hand on.
//
// The path is relative: the browser turns it into an address against the origin
// it reached this server on, which is the host that will work from wherever the
// player is running. A name the server guessed for itself might resolve
// nowhere else, and a tailnet or a reverse proxy makes that near certain.
type streamLinkResponse struct {
	URL       string    `json:"url"`
	Name      string    `json:"name"`
	MIME      string    `json:"mime,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	ExpiresIn int       `json:"expires_in"`
}

// handleFileStreamLink mints a link for one stored file.
func (s *Server) handleFileStreamLink(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	// Resolved now rather than at play time, so a file that is not there is a
	// refused request in the app instead of a link that fails in VLC.
	entry, err := v.Entry(r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}

	token, expiry, err := s.streams.issue(entry.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not mint a stream link", "INTERNAL_ERROR")
		return
	}

	writeJSON(w, http.StatusCreated, streamLinkResponse{
		URL:       streamPath(token, entry.Name),
		Name:      entry.Name,
		MIME:      entry.MIME,
		ExpiresAt: expiry,
		ExpiresIn: int(time.Until(expiry).Round(time.Second).Seconds()),
	})
}

// handleStream plays a ticket's file, with ranges, to whatever asked.
//
// No session is required — the ticket is the credential, and the caller is a
// player that has no cookie to offer. Everything else is the content endpoint's
// bargain: read at an offset so seeking into a film costs the chunks that range
// covers, never a shared cache, and nothing that could execute in this origin
// is served inline.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	fileID, ok := s.streams.lookup(r.PathValue("token"))
	if !ok {
		writeError(w, http.StatusNotFound, "this stream link has expired", "NO_TICKET")
		return
	}

	v, err := s.Vault()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "VAULT_ERROR")
		return
	}
	if !v.Unlocked() {
		writeError(w, http.StatusUnauthorized, "vault is locked", "LOCKED")
		return
	}

	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()

	reader, entry, err := v.OpenReadSeeker(ctx, fileID)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	// A file still stored whole is rebuilt onto disk to be read at an offset;
	// closing is what lets go of that copy.
	defer reader.Close()

	// A player has no browser session keeping the vault awake, which is the
	// same hole a mounted share has: without this the vault would lock itself
	// halfway through a film.
	s.noteExternalActivity()

	contentType := entry.MIME
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	disposition := "inline"
	if isRiskyInline(contentType) {
		disposition = "attachment"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType(disposition, map[string]string{"filename": entry.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("ETag", `"`+entry.Hash+`"`)

	http.ServeContent(w, r, entry.Name, entry.ModifiedAt, reader)
}
