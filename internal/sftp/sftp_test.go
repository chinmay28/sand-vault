package sftp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/sftp/sftptest"
)

// dialTest builds a Config pointed at a test server, with a key it accepts.
func dialTest(t *testing.T, s *sftptest.Server, key, hostKey string) Config {
	t.Helper()
	host, port := s.HostPort(t)
	return Config{
		Host:       host,
		Port:       port,
		User:       "sand",
		PrivateKey: key,
		HostKey:    hostKey,
	}
}

func TestDialRoundTrip(t *testing.T) {
	root := t.TempDir()
	server := sftptest.NewServer(t, root)
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub

	client, err := Dial(context.Background(), dialTest(t, server, key, ""))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	f, err := client.SFTP().Create("hello.txt")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Write([]byte("sand")); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	got, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatalf("reading what the server wrote: %v", err)
	}
	if string(got) != "sand" {
		t.Fatalf("round trip: got %q, want %q", got, "sand")
	}
}

// The pin is only worth something if a first connection reports what it
// learned, so the caller has something to store.
func TestDialLearnsHostKeyOnFirstUse(t *testing.T) {
	server := sftptest.NewServer(t, t.TempDir())
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub

	client, err := Dial(context.Background(), dialTest(t, server, key, ""))
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	learned := client.HostKey()
	client.Close()

	if learned != server.Fingerprint {
		t.Fatalf("learned %q, server presented %q", learned, server.Fingerprint)
	}
	if !strings.HasPrefix(learned, "SHA256:") {
		t.Fatalf("fingerprint %q is not in the form ssh-keygen prints", learned)
	}

	// And the fingerprint it learned has to be one it will accept back.
	again, err := Dial(context.Background(), dialTest(t, server, key, learned))
	if err != nil {
		t.Fatalf("dialling with the learned fingerprint: %v", err)
	}
	again.Close()
}

func TestDialRefusesChangedHostKey(t *testing.T) {
	server := sftptest.NewServer(t, t.TempDir())
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub

	// A well-formed fingerprint for some other key.
	other := sftptest.NewServer(t, t.TempDir())

	_, err := Dial(context.Background(), dialTest(t, server, key, other.Fingerprint))
	if err == nil {
		t.Fatal("dialled a host whose key does not match the pin")
	}

	var mismatch *HostKeyMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("got %T (%v), want a *HostKeyMismatchError", err, err)
	}
	if mismatch.Expected != other.Fingerprint {
		t.Errorf("expected fingerprint %q, want %q", mismatch.Expected, other.Fingerprint)
	}
	if mismatch.Got != server.Fingerprint {
		t.Errorf("presented fingerprint %q, want %q", mismatch.Got, server.Fingerprint)
	}
	// Both fingerprints belong in the message: the whole point is that the
	// person reading it decides whether this was a rebuild or an impostor.
	for _, want := range []string{other.Fingerprint, server.Fingerprint} {
		if !strings.Contains(mismatch.Error(), want) {
			t.Errorf("message does not name %q: %s", want, mismatch)
		}
	}
}

// A fingerprint pasted with the prefix stripped, or with base64 padding, is
// the same fingerprint. Anything else is not.
func TestHostKeyPinToleratesHowItWasWritten(t *testing.T) {
	server := sftptest.NewServer(t, t.TempDir())
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub

	bare := strings.TrimPrefix(server.Fingerprint, "SHA256:")
	for _, form := range []string{
		server.Fingerprint,
		bare,
		"  " + server.Fingerprint + "\n",
		server.Fingerprint + "=",
	} {
		client, err := Dial(context.Background(), dialTest(t, server, key, form))
		if err != nil {
			t.Errorf("fingerprint written as %q was refused: %v", form, err)
			continue
		}
		client.Close()
	}
}

func TestDialRejectsUnknownKey(t *testing.T) {
	server := sftptest.NewServer(t, t.TempDir())
	_, pub := sftptest.NewClientKey(t)
	server.Authorized = pub

	// A different key from the one the server knows.
	stranger, _ := sftptest.NewClientKey(t)
	if _, err := Dial(context.Background(), dialTest(t, server, stranger, "")); err == nil {
		t.Fatal("signed in with a key the server does not know")
	}
}

func TestDialReportsMissingSubsystem(t *testing.T) {
	server := sftptest.NewServer(t, t.TempDir())
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub
	server.NoSubsystem = true

	_, err := Dial(context.Background(), dialTest(t, server, key, ""))
	if err == nil {
		t.Fatal("opened an sftp session on a server with no sftp subsystem")
	}
	if !strings.Contains(err.Error(), "subsystem") && !strings.Contains(err.Error(), "SFTP") {
		t.Fatalf("error does not explain the failure: %v", err)
	}
}

func TestParseKeyPassphrases(t *testing.T) {
	sealed := sftptest.NewEncryptedClientKey(t, "correct horse")

	if _, err := parseKey(sealed, ""); !errors.Is(err, ErrPassphraseRequired) {
		t.Errorf("no passphrase: got %v, want ErrPassphraseRequired", err)
	}
	if _, err := parseKey(sealed, "wrong"); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("wrong passphrase: got %v, want ErrWrongPassphrase", err)
	}
	if _, err := parseKey(sealed, "correct horse"); err != nil {
		t.Errorf("right passphrase: %v", err)
	}
	if _, err := parseKey("not a key at all", ""); err == nil {
		t.Error("parsed a string that is not a key")
	}
}

func TestDialWithPassword(t *testing.T) {
	server := sftptest.NewServer(t, t.TempDir())
	server.Password = "hunter2"
	host, port := server.HostPort(t)

	client, err := Dial(context.Background(), Config{
		Host: host, Port: port, User: "sand", Password: "hunter2",
	})
	if err != nil {
		t.Fatalf("password sign-in: %v", err)
	}
	client.Close()
}

func TestDialNeedsCredentials(t *testing.T) {
	server := sftptest.NewServer(t, t.TempDir())
	host, port := server.HostPort(t)

	if _, err := Dial(context.Background(), Config{Host: host, Port: port, User: "sand"}); err == nil {
		t.Fatal("dialled with no credentials at all")
	}
}

func TestPoolReusesOneConnection(t *testing.T) {
	server := sftptest.NewServer(t, t.TempDir())
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub

	pool := NewPool(dialTest(t, server, key, ""))
	defer pool.Close()

	var learned []string
	pool.OnHostKeyLearned(func(f string) { learned = append(learned, f) })

	first, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	second, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if first != second {
		t.Error("pool dialled twice for two gets")
	}

	// Learned once, on the connection that learned it — not again on every
	// get, or the vault would be rewritten on every shard.
	if len(learned) != 1 || learned[0] != server.Fingerprint {
		t.Errorf("host key learned %v, want exactly [%s]", learned, server.Fingerprint)
	}
}

func TestPoolRedialsAfterTheConnectionDies(t *testing.T) {
	server := sftptest.NewServer(t, t.TempDir())
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub

	pool := NewPool(dialTest(t, server, key, ""))
	defer pool.Close()

	first, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	first.Close()

	second, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("get after the connection died: %v", err)
	}
	if second == first {
		t.Error("pool handed back the connection that had been closed")
	}
	if _, err := second.SFTP().Getwd(); err != nil {
		t.Errorf("redialled connection is not usable: %v", err)
	}
}

func TestCleanPath(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"   ":             "",
		"/":               "/",
		"/srv/media/":     "/srv/media",
		"/srv//media":     "/srv/media",
		"/srv/./media":    "/srv/media",
		"/srv/x/../media": "/srv/media",
		`\srv\media`:      "/srv/media",
		"relative/path":   "relative/path",
		".":               "",
	}
	for in, want := range cases {
		if got := CleanPath(in); got != want {
			t.Errorf("CleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnderRefusesEscapes(t *testing.T) {
	const root = "/srv/sand"

	ok := map[string]string{
		"a":     "/srv/sand/a",
		"/a":    "/srv/sand/a",
		"a/b/c": "/srv/sand/a/b/c",
		"":      "/srv/sand",
		"./a":   "/srv/sand/a",
		"a//b":  "/srv/sand/a/b",
	}
	for in, want := range ok {
		got, err := Under(root, in)
		if err != nil {
			t.Errorf("Under(%q, %q): %v", root, in, err)
			continue
		}
		if got != want {
			t.Errorf("Under(%q, %q) = %q, want %q", root, in, got, want)
		}
	}

	// A parent hop is refused wherever it appears, including the ones that
	// would have landed back inside the root anyway — a rule with an exception
	// is a rule somebody eventually finds the exception to.
	for _, in := range []string{
		"..",
		"../etc/passwd",
		"a/../../etc/passwd",
		"/../../etc/passwd",
		"a/../b",
		`..\..\etc`,
	} {
		if got, err := Under(root, in); err == nil {
			t.Errorf("Under(%q, %q) = %q, want a refusal", root, in, got)
		}
	}

	// A sibling directory whose name starts with the root's is not under it.
	if got, err := Under("/srv/sand", "../sandbox/x"); err == nil {
		t.Errorf("Under escaped into a sibling: %q", got)
	}
}

func TestNormalizeHostKey(t *testing.T) {
	server := sftptest.NewServer(t, t.TempDir())
	bare := strings.TrimPrefix(server.Fingerprint, "SHA256:")

	for _, in := range []string{server.Fingerprint, bare, " " + bare + " "} {
		got, err := NormalizeHostKey(in)
		if err != nil {
			t.Errorf("NormalizeHostKey(%q): %v", in, err)
			continue
		}
		if got != server.Fingerprint {
			t.Errorf("NormalizeHostKey(%q) = %q, want %q", in, got, server.Fingerprint)
		}
	}

	if got, err := NormalizeHostKey(""); err != nil || got != "" {
		t.Errorf(`NormalizeHostKey("") = %q, %v; want "", nil`, got, err)
	}

	// An MD5 fingerprint and a whole public key are the two things people
	// paste by mistake, and neither is a SHA-256 fingerprint.
	for _, in := range []string{
		"MD5:ab:cd:ef:12:34:56:78:90:ab:cd:ef:12:34:56:78:90",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample user@host",
		"nonsense",
	} {
		if _, err := NormalizeHostKey(in); err == nil {
			t.Errorf("NormalizeHostKey(%q) was accepted", in)
		}
	}
}
