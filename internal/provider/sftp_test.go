package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/sftp/sftptest"
)

// connectSFTP builds an SFTP backend pointed at a server started for the test,
// and returns it alongside the directory the server is serving so a test can
// look at what actually landed on disk.
func connectSFTP(t *testing.T, options map[string]string) (Provider, *sftptest.Server, string) {
	t.Helper()

	root := t.TempDir()
	server := sftptest.NewServer(t, root)
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub
	host, port := server.HostPort(t)

	opts := map[string]string{
		"host":        host,
		"port":        fmt.Sprint(port),
		"username":    "sand",
		"private_key": key,
		"path":        "parts",
	}
	for k, v := range options {
		opts[k] = v
	}

	p, err := New(Config{ID: "sftp-test", Kind: KindSFTP, Name: "vps", Options: opts})
	if err != nil {
		t.Fatalf("building the backend: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := p.(interface{ Close() error }); ok {
			closer.Close()
		}
	})
	return p, server, root
}

func TestSFTPRoundTrip(t *testing.T) {
	p, _, root := connectSFTP(t, nil)
	ctx := context.Background()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	const key = "archive/0/1.shard"
	want := []byte("encrypted nonsense")
	if err := p.Put(ctx, key, want); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := p.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("get returned %q, want %q", got, want)
	}

	info, err := p.Stat(ctx, key)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size != int64(len(want)) {
		t.Errorf("stat size %d, want %d", info.Size, len(want))
	}

	// And it landed under the configured folder rather than anywhere else.
	if _, err := os.Stat(filepath.Join(root, "parts", "archive", "0", "1.shard")); err != nil {
		t.Errorf("the shard is not where it should be on the server: %v", err)
	}

	if err := p.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := p.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: %v, want ErrNotFound", err)
	}
}

func TestSFTPMissingObject(t *testing.T) {
	p, _, _ := connectSFTP(t, nil)
	ctx := context.Background()

	if _, err := p.Get(ctx, "nothing/here"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get: %v, want ErrNotFound", err)
	}
	if _, err := p.Stat(ctx, "nothing/here"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stat: %v, want ErrNotFound", err)
	}
	// Deleting what is not there is not an error — the interface says so, and
	// the sweep that removes orphans relies on it.
	if err := p.Delete(ctx, "nothing/here"); err != nil {
		t.Errorf("delete of a missing object: %v", err)
	}
}

func TestSFTPPutOverwrites(t *testing.T) {
	p, _, _ := connectSFTP(t, nil)
	ctx := context.Background()

	const key = "a/b.shard"
	if err := p.Put(ctx, key, []byte("first")); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := p.Put(ctx, key, []byte("second")); err != nil {
		t.Fatalf("second put: %v", err)
	}

	got, err := p.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("got %q, want the second write", got)
	}
}

// A Put that finished leaves one object, not an object and the temporary file
// it was written under.
func TestSFTPPutLeavesNoTemporaries(t *testing.T) {
	p, _, root := connectSFTP(t, nil)
	ctx := context.Background()

	if err := p.Put(ctx, "a/b.shard", []byte("x")); err != nil {
		t.Fatalf("put: %v", err)
	}

	var stray []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), ".sand-tmp-") {
			stray = append(stray, path)
		}
		return nil
	})
	if len(stray) > 0 {
		t.Errorf("temporary files left behind: %v", stray)
	}
}

func TestSFTPList(t *testing.T) {
	p, _, root := connectSFTP(t, nil)
	ctx := context.Background()

	keys := []string{"one/a.shard", "one/b.shard", "two/c.shard"}
	for _, key := range keys {
		if err := p.Put(ctx, key, []byte(key)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	all, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, 0, len(all))
	for _, o := range all {
		got = append(got, o.Key)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(keys, ",") {
		t.Errorf("list returned %v, want %v", got, keys)
	}

	scoped, err := p.List(ctx, "one/")
	if err != nil {
		t.Fatalf("prefixed list: %v", err)
	}
	if len(scoped) != 2 {
		t.Errorf("list under one/ returned %d objects, want 2", len(scoped))
	}

	// A half-written shard from an interrupted Put is not an object. Listing
	// it would have the recovery scan try to read it as one.
	if err := os.WriteFile(filepath.Join(root, "parts", "one", ".sand-tmp-abcd"), []byte("half"), 0600); err != nil {
		t.Fatalf("planting a temp file: %v", err)
	}
	after, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("list after planting a temp file: %v", err)
	}
	for _, o := range after {
		if strings.Contains(o.Key, ".sand-tmp-") {
			t.Errorf("list returned a half-written shard: %s", o.Key)
		}
	}
}

// Listing an account nothing has been written to yet is an empty answer, not a
// failure: it is what every freshly connected account reports.
func TestSFTPListBeforeAnythingIsWritten(t *testing.T) {
	p, _, _ := connectSFTP(t, nil)

	got, err := p.List(context.Background(), "")
	if err != nil {
		t.Fatalf("list on an empty account: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("list returned %d objects on an empty account", len(got))
	}
}

// The key is machine-generated, so this is defence in depth rather than a
// likely input — but it is the whole of what keeps a backend inside its folder.
func TestSFTPRefusesKeysOutsideTheFolder(t *testing.T) {
	p, _, root := connectSFTP(t, nil)
	ctx := context.Background()

	for _, key := range []string{"../escaped", "../../etc/passwd", "a/../../escaped"} {
		if err := p.Put(ctx, key, []byte("nope")); err == nil {
			t.Errorf("put(%q) was allowed", key)
		}
		if _, err := p.Get(ctx, key); err == nil {
			t.Errorf("get(%q) was allowed", key)
		}
		if err := p.Delete(ctx, key); err == nil {
			t.Errorf("delete(%q) was allowed", key)
		}
	}

	// Nothing escaped onto the server's disk either.
	if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
		t.Errorf("a key escaped the configured folder: %v", err)
	}
}

// Trust on first use only pins anything if what was learned is handed back to
// be stored. The vault wires this sink up through CredentialRotator.
func TestSFTPLearnsAndReportsTheHostKey(t *testing.T) {
	p, server, _ := connectSFTP(t, nil)

	rotator, ok := p.(CredentialRotator)
	if !ok {
		t.Fatal("the sftp backend does not implement CredentialRotator, so a learned host key is never stored")
	}

	learned := make(chan map[string]string, 4)
	rotator.OnCredentialChange(func(updates map[string]string) { learned <- updates })

	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	select {
	case updates := <-learned:
		if updates["host_key"] != server.Fingerprint {
			t.Errorf("learned host key %q, want %q", updates["host_key"], server.Fingerprint)
		}
	default:
		t.Fatal("connecting to an unknown host did not report a host key to store")
	}
}

func TestSFTPRefusesAChangedHostKey(t *testing.T) {
	// A well-formed fingerprint belonging to some other machine.
	other := sftptest.NewServer(t, t.TempDir())

	p, _, _ := connectSFTP(t, map[string]string{"host_key": other.Fingerprint})

	err := p.Ping(context.Background())
	if err == nil {
		t.Fatal("connected to a host whose key does not match the pin")
	}
	if !strings.Contains(err.Error(), "host key") {
		t.Errorf("error does not say the host key is the problem: %v", err)
	}
}

func TestSFTPConfigValidation(t *testing.T) {
	base := map[string]string{
		"host": "example.com", "username": "sand", "private_key": "x", "path": "sand",
	}
	with := func(key, value string) Config {
		opts := map[string]string{}
		for k, v := range base {
			opts[k] = v
		}
		if value == "" {
			delete(opts, key)
		} else {
			opts[key] = value
		}
		return Config{Kind: KindSFTP, Options: opts}
	}

	// A fingerprint is checked when it is typed rather than on the first
	// transfer, where a typo would read exactly like an attack.
	if _, err := New(with("host_key", "MD5:ab:cd:ef")); err == nil {
		t.Error("an MD5 fingerprint was accepted as a host key")
	}
	if _, err := New(with("port", "not-a-port")); err == nil {
		t.Error("a non-numeric port was accepted")
	}
	if _, err := New(with("port", "70000")); err == nil {
		t.Error("a port above 65535 was accepted")
	}
	if _, err := New(with("host", "")); err == nil {
		t.Error("a missing host was accepted")
	}
	if _, err := New(with("username", "")); err == nil {
		t.Error("a missing username was accepted")
	}
}

// The connect dialog is drawn from the registry, so a field the form has to
// render specially is only rendered specially if the spec says so. A private
// key pasted into a single-line input loses its newlines and never parses.
func TestSFTPSpecMarksTheKeyMultiline(t *testing.T) {
	spec, ok := SpecFor(KindSFTP)
	if !ok {
		t.Fatal("the sftp backend is not registered")
	}
	for _, f := range spec.Fields {
		if f.Key != "private_key" {
			continue
		}
		if !f.Multiline {
			t.Error("the private key field is not marked multiline")
		}
		if !f.Secret {
			t.Error("the private key field is not marked secret")
		}
		return
	}
	t.Error("the sftp spec has no private_key field")
}
