package sftp

import (
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"
)

// HostKeyMismatchError is returned when a host answers with a key other than
// the one it is pinned to.
//
// It carries both fingerprints because the two things this can mean are told
// apart by looking at them, and only the person who owns the server can do the
// telling: a rebuilt VPS regenerates its host keys and is expected to change,
// and somebody sitting between you and the server also changes. SAND cannot
// know which, so it refuses and shows both.
type HostKeyMismatchError struct {
	Host     string
	Expected string
	Got      string
	// Algorithm is the key type the server offered, e.g. "ssh-ed25519". A
	// change of algorithm alone is usually a server offering a second key
	// rather than a different machine, which is worth being able to see.
	Algorithm string
}

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf(
		"the host key for %s has changed — expected %s, got %s (%s). "+
			"If you rebuilt or migrated this server, forget the stored key and connect again "+
			"to accept the new one. If you did not, something is answering in its place: "+
			"do not reconnect until you know which",
		e.Host, e.Expected, e.Got, e.Algorithm)
}

// Fingerprint renders a public key the way ssh-keygen -l does, so a
// fingerprint SAND shows can be compared against one read off the server
// without any conversion in between.
func Fingerprint(key ssh.PublicKey) string { return ssh.FingerprintSHA256(key) }

// hostKeyChecker is the callback side of the pinning described in the package
// doc: it records what the server presented, and refuses anything that
// disagrees with what it was told to expect.
type hostKeyChecker struct {
	expected string

	// seen is the fingerprint the server presented, recorded whether or not
	// anything was expected. On a first connection this is what the caller
	// stores; on any other it equals expected.
	seen string
}

func (c *hostKeyChecker) check(hostname string, remote net.Addr, key ssh.PublicKey) error {
	got := Fingerprint(key)
	c.seen = got

	if c.expected == "" {
		// Trust on first use. Nothing to compare against, so this connection
		// is what establishes the pin — see the package doc for what that is
		// and is not worth.
		return nil
	}
	if subtleEqual(c.expected, got) {
		return nil
	}
	return &HostKeyMismatchError{
		Host:      hostname,
		Expected:  c.expected,
		Got:       got,
		Algorithm: key.Type(),
	}
}

// subtleEqual compares two fingerprints. They are public values, so this is
// about tolerating how they were written rather than about timing: a
// fingerprint pasted out of ssh-keygen output may carry the "SHA256:" prefix
// or may not, and may have picked up whitespace on the way through a form.
func subtleEqual(a, b string) bool {
	return normalizeFingerprint(a) == normalizeFingerprint(b)
}

// normalizeFingerprint reduces a fingerprint to the part that identifies it.
//
// Base64 is case-sensitive, so nothing here lowercases: two fingerprints
// differing only in case are two different keys.
func normalizeFingerprint(f string) string {
	f = strings.TrimSpace(f)
	f = strings.TrimPrefix(f, "SHA256:")
	// ssh-keygen pads its base64 out with "=" in some versions and not in
	// others; the padding carries no information.
	return strings.TrimRight(f, "=")
}

// NormalizeHostKey returns a fingerprint in the canonical form SAND stores,
// or an error if it does not look like one. Used when somebody pastes a
// fingerprint in rather than letting the first connection learn it.
func NormalizeHostKey(f string) (string, error) {
	trimmed := strings.TrimSpace(f)
	if trimmed == "" {
		return "", nil
	}
	body := normalizeFingerprint(trimmed)
	if body == "" {
		return "", fmt.Errorf("%q is not a host key fingerprint", f)
	}
	// A SHA-256 digest is 32 bytes, which is 43 base64 characters unpadded.
	// Anything else is a different kind of string — an MD5 fingerprint with
	// colons in it, or a whole public key pasted by mistake.
	if len(body) != 43 || strings.ContainsAny(body, " \t:") {
		return "", fmt.Errorf(
			"%q is not a SHA-256 host key fingerprint — SAND wants the form ssh-keygen -l prints, "+
				"like SHA256:abc…, which you can read off the server with "+
				"ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub", f)
	}
	return "SHA256:" + body, nil
}
