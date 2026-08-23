package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/sftp/sftptest"
	"golang.org/x/crypto/ssh"
)

// generateKey asks the endpoint the connect form uses for a key pair.
func (c *testClient) generateKey(comment string) map[string]any {
	c.t.Helper()

	w, body := c.json(http.MethodPost, "/api/ssh/keypair", map[string]any{"comment": comment})
	if w.Code != http.StatusCreated {
		c.t.Fatalf("POST /api/ssh/keypair: %d %s", w.Code, w.Body.String())
	}
	return body
}

// The claim this endpoint makes is about what does *not* come back, so that is
// what the test is about: the browser is given a public key and a handle, and
// no part of the response is a private key.
func TestGenerateSSHKeyKeepsThePrivateHalf(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 1)

	body := c.generateKey("the vps")

	public, _ := body["public_key"].(string)
	if !strings.HasPrefix(public, "ssh-ed25519 ") {
		t.Fatalf("public_key = %q, want an ed25519 authorized_keys line", public)
	}
	if !strings.HasSuffix(public, " the vps") {
		t.Errorf("public_key = %q, want the comment on the end", public)
	}
	if fingerprint, _ := body["fingerprint"].(string); !strings.HasPrefix(fingerprint, "SHA256:") {
		t.Errorf("fingerprint = %q, want the form ssh-keygen -l prints", fingerprint)
	}

	handle, _ := body["handle"].(string)
	if !strings.HasPrefix(handle, generatedKeyPrefix) {
		t.Errorf("handle = %q, want one this server will recognise", handle)
	}

	// Whatever else the response grows, none of it may be the key itself.
	if strings.Contains(strings.ToUpper(fmt.Sprint(body)), "PRIVATE KEY") {
		t.Errorf("a private key came back to the browser: %v", body)
	}
	if _, ok := body["private_key"]; ok {
		t.Errorf("the response carries a private_key field: %v", body)
	}
}

// And the pair is a pair: the half handed out signs in with the half kept.
// Everything between — the handle, the swap on the way past the handler — is
// only worth anything if this holds end to end.
func TestConnectSourceWithAGeneratedKey(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	generated := c.generateKey("")
	installed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(generated["public_key"].(string)))
	if err != nil {
		t.Fatalf("the public half does not parse as an authorized_keys line: %v", err)
	}

	disk := t.TempDir()
	server := sftptest.NewServer(t, disk)
	server.Authorized = installed
	host, port := server.HostPort(t)

	root := filepath.Join(disk, "share")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("making the root: %v", err)
	}
	seedRemote(t, filepath.Join(root, "notes.txt"), "hello")

	// The form sends the handle back in the field a pasted key would have
	// filled in. Nothing else about the request is different.
	source := c.connectRemote(map[string]any{
		"name":        "vps",
		"host":        host,
		"port":        port,
		"user":        "sand",
		"private_key": generated["handle"],
		"root":        filepath.ToSlash(root),
	})

	// Stored, and stored as a key rather than as the handle.
	if key, _ := source["private_key"].(string); key == generated["handle"] {
		t.Fatal("the handle was stored as if it were the key")
	}

	_, listing := c.json(http.MethodGet, "/api/remote/"+source["id"].(string)+"/files", nil)
	entries, _ := listing["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("browsing with a generated key: %v", listing)
	}
}

// A connect that fails is fixed by editing the form and pressing the button
// again, so the handle has to survive the failure. Consuming it on first use
// would turn every typo into "generate a new key and install it again".
func TestAGeneratedKeySurvivesAFailedConnect(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	generated := c.generateKey("")
	installed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(generated["public_key"].(string)))
	if err != nil {
		t.Fatalf("parsing the public half: %v", err)
	}

	disk := t.TempDir()
	server := sftptest.NewServer(t, disk)
	server.Authorized = installed
	host, port := server.HostPort(t)

	root := filepath.Join(disk, "share")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("making the root: %v", err)
	}

	req := map[string]any{
		"name":        "vps",
		"host":        host,
		"port":        port,
		"user":        "sand",
		"private_key": generated["handle"],
		"root":        "/nowhere/at/all",
	}
	if w, _ := c.json(http.MethodPost, "/api/remote", req); w.Code == http.StatusCreated {
		t.Fatal("a folder that does not exist was accepted")
	}

	req["root"] = filepath.ToSlash(root)
	c.connectRemote(req)
}

// A form older than the key it refers to has to say so. Left unrecognised the
// handle would be handed to the SSH client as a PEM block, and the answer
// would be "this does not look like a private key" — true, and no help at all.
func TestAnUnknownKeyHandleIsRefusedLegibly(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	req, _ := remoteFixture(t, "vps")
	req["private_key"] = generatedKeyPrefix + "long-since-forgotten"

	w, body := c.json(http.MethodPost, "/api/remote", req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/remote with a dead handle: %d, want 400", w.Code)
	}
	if code, _ := body["code"].(string); code != "KEY_EXPIRED" {
		t.Errorf("code = %q, want KEY_EXPIRED", code)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "expired") {
		t.Errorf("error = %q, want it to say the key expired", message)
	}
}

// The same swap on the other SSH path: a connected account whose backend is a
// machine you have a login on.
func TestConnectSFTPAccountWithAGeneratedKey(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 1)

	generated := c.generateKey("")
	installed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(generated["public_key"].(string)))
	if err != nil {
		t.Fatalf("parsing the public half: %v", err)
	}

	disk := t.TempDir()
	server := sftptest.NewServer(t, disk)
	server.Authorized = installed
	host, port := server.HostPort(t)

	w, body := c.json(http.MethodPost, "/api/providers", map[string]any{
		"kind": "sftp",
		"name": "the vps",
		"options": map[string]string{
			"host":        host,
			"port":        fmt.Sprint(port),
			"username":    "sand",
			"private_key": generated["handle"].(string),
			"path":        "sand",
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("connecting an sftp account with a generated key: %d %v", w.Code, body)
	}

	// The account is only connected if the backend answered, and the backend
	// only answers if the key SAND kept matches the one installed above.
	account, _ := body["provider"].(map[string]any)
	if account["kind"] != "sftp" {
		t.Fatalf("stored account: %v", body)
	}

	// Shut down here rather than left to the test's cleanup. An SFTP account
	// holds an open SSH session, the in-process server will not stop while one
	// is live, and cleanups run last-registered-first — so the server's would
	// go first and wait forever. Drain the manifest push that connecting
	// kicked off, then lock, which is what closes every live backend.
	c.server.vault.AwaitBackupSync()
	c.server.vault.Lock()
}

// Minting keys is not something to offer whoever asks. It is one step from
// being a credential, and this server is reachable from the rest of the
// network on most installs.
func TestGenerateSSHKeyNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 1)
	c.cookies = nil

	w, _ := c.json(http.MethodPost, "/api/ssh/keypair", map[string]any{})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/ssh/keypair without a session: %d, want 401", w.Code)
	}
}

// The oldest waiting key loses when the cap is reached, so a client stuck in a
// loop cannot grow the map without bound.
func TestGeneratedKeysAreCapped(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 1)

	first := c.generateKey("")["handle"].(string)
	for i := 0; i < maxGeneratedKeys; i++ {
		c.generateKey("")
	}

	if _, ok := c.server.generatedKeys.privateKey(first); ok {
		t.Error("the store grew past its cap instead of dropping the oldest key")
	}
	if got := len(c.server.generatedKeys.keys); got > maxGeneratedKeys {
		t.Errorf("%d keys waiting, cap is %d", got, maxGeneratedKeys)
	}
}
