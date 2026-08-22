package vault

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/sftp/sftptest"
)

// testSource starts a server, seeds a tree under it, and returns a Source
// pointed at the scoped folder plus the directory on disk behind it.
func testSource(t *testing.T, name string) (Source, string) {
	t.Helper()

	disk := t.TempDir()
	server := sftptest.NewServer(t, disk)
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub
	host, port := server.HostPort(t)

	root := filepath.Join(disk, "share")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("making the root: %v", err)
	}

	return Source{
		Name:       name,
		Host:       host,
		Port:       port,
		User:       "sand",
		PrivateKey: key,
		Root:       filepath.ToSlash(root),
	}, root
}

func seed(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestAddSourceLearnsTheHostKeyAndRedactsSecrets(t *testing.T) {
	v, _ := newTestVault(t, 3)
	src, _ := testSource(t, "vps")

	added, err := v.AddSource(context.Background(), src)
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if added.ID == "" {
		t.Error("the stored source has no id")
	}
	// A source stored without a pin would pin nothing: the next connection
	// would learn whatever answered.
	if !strings.HasPrefix(added.HostKey, "SHA256:") {
		t.Errorf("host key %q was not learned on the connection that stored it", added.HostKey)
	}
	if added.PrivateKey != provider.RedactedSecret {
		t.Errorf("private key came back as %q rather than redacted", added.PrivateKey)
	}

	listed, err := v.Sources()
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d sources, want 1", len(listed))
	}
	if listed[0].PrivateKey != provider.RedactedSecret {
		t.Error("a listed source hands out its private key")
	}

	// The real key is still there for the code that has to connect with it.
	full, err := v.source(added.ID)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if full.PrivateKey == provider.RedactedSecret || full.PrivateKey == "" {
		t.Error("the stored source lost its private key")
	}
}

func TestAddSourceRefusesWhatItCannotReach(t *testing.T) {
	v, _ := newTestVault(t, 3)

	src, _ := testSource(t, "vps")
	src.Root = "/nowhere/at/all"
	if _, err := v.AddSource(context.Background(), src); err == nil {
		t.Error("stored a source whose folder does not exist")
	}

	unreachable, _ := testSource(t, "unreachable")
	unreachable.Port = 1
	if _, err := v.AddSource(context.Background(), unreachable); err == nil {
		t.Error("stored a source it could not connect to")
	}

	incomplete, _ := testSource(t, "incomplete")
	incomplete.PrivateKey, incomplete.Password = "", ""
	if _, err := v.AddSource(context.Background(), incomplete); err == nil {
		t.Error("stored a source with no way to sign in")
	}
}

func TestAddSourceRefusesADuplicateName(t *testing.T) {
	v, _ := newTestVault(t, 3)
	first, _ := testSource(t, "vps")
	if _, err := v.AddSource(context.Background(), first); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	second, _ := testSource(t, "VPS")
	if _, err := v.AddSource(context.Background(), second); err == nil {
		t.Error("two sources were allowed the same name")
	}
}

// Renaming a source must not wipe its key, and must not quietly stop pinning
// its host key either.
func TestUpdateSourceKeepsSecretsAndThePin(t *testing.T) {
	v, _ := newTestVault(t, 3)
	src, _ := testSource(t, "vps")

	added, err := v.AddSource(context.Background(), src)
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	// Exactly what an edit form sends back: everything redacted stays
	// redacted, and the fingerprint is not sent at all.
	edits := added
	edits.Name = "the vps"
	edits.HostKey = ""

	updated, err := v.UpdateSource(context.Background(), added.ID, edits, false)
	if err != nil {
		t.Fatalf("UpdateSource: %v", err)
	}
	if updated.Name != "the vps" {
		t.Errorf("name %q, want %q", updated.Name, "the vps")
	}
	if updated.HostKey != added.HostKey {
		t.Errorf("renaming changed the pin from %q to %q", added.HostKey, updated.HostKey)
	}

	full, err := v.source(added.ID)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if full.PrivateKey == provider.RedactedSecret || full.PrivateKey == "" {
		t.Error("renaming wiped the private key")
	}
}

func TestUpdateSourceCanRelearnAHostKey(t *testing.T) {
	v, _ := newTestVault(t, 3)
	src, _ := testSource(t, "vps")

	added, err := v.AddSource(context.Background(), src)
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	// A pin belonging to some other machine: without relearn this is refused,
	// which is the whole point of the pin.
	other, _ := testSource(t, "other")
	stray, err := v.AddSource(context.Background(), other)
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	wrong := added
	wrong.HostKey = stray.HostKey
	if _, err := v.UpdateSource(context.Background(), added.ID, wrong, false); err == nil {
		t.Error("a source was repointed at a host whose key does not match its pin")
	}

	// Asked for explicitly, the pin is dropped and the connection learns again.
	relearned, err := v.UpdateSource(context.Background(), added.ID, added, true)
	if err != nil {
		t.Fatalf("relearning: %v", err)
	}
	if relearned.HostKey != added.HostKey {
		t.Errorf("relearned %q, want the same machine's %q", relearned.HostKey, added.HostKey)
	}
}

func TestRemoveSource(t *testing.T) {
	v, _ := newTestVault(t, 3)
	src, _ := testSource(t, "vps")

	added, err := v.AddSource(context.Background(), src)
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if err := v.RemoveSource(added.ID); err != nil {
		t.Fatalf("RemoveSource: %v", err)
	}
	listed, err := v.Sources()
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("%d sources left after removing the only one", len(listed))
	}
	if err := v.RemoveSource(added.ID); err == nil {
		t.Error("removing a source twice was not an error the second time")
	}
}

// A source is a credential, so it has to survive a lock and a password change
// like the connected accounts do.
func TestSourcesSurviveALockAndAPasswordChange(t *testing.T) {
	v, _ := newTestVault(t, 3)
	src, _ := testSource(t, "vps")

	added, err := v.AddSource(context.Background(), src)
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	if _, err := v.ChangePassword(context.Background(), testPassword, "a-different-password", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	v.Lock()
	if _, err := v.Sources(); err == nil {
		t.Error("a locked vault handed out its sources")
	}
	if err := v.Unlock("a-different-password"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	listed, err := v.Sources()
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != added.ID {
		t.Fatalf("sources after a password change: %+v", listed)
	}

	full, err := v.source(added.ID)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if full.PrivateKey == "" {
		t.Error("the private key did not survive the password change")
	}
	if full.HostKey != added.HostKey {
		t.Error("the host key pin did not survive the password change")
	}
}

func TestBrowseSource(t *testing.T) {
	v, _ := newTestVault(t, 3)
	src, root := testSource(t, "vps")
	seed(t, filepath.Join(root, "films", "one.mp4"), "film")
	seed(t, filepath.Join(root, "notes.txt"), "hello")

	added, err := v.AddSource(context.Background(), src)
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	listing, err := v.BrowseSource(context.Background(), added.ID, "")
	if err != nil {
		t.Fatalf("BrowseSource: %v", err)
	}
	if !listing.AtRoot || len(listing.Entries) != 2 {
		t.Fatalf("root listing: %+v", listing)
	}
	if listing.Entries[0].Name != "films" || !listing.Entries[0].Dir {
		t.Errorf("folders do not come first: %+v", listing.Entries)
	}

	// And the boundary holds through the vault, not only inside the client.
	if _, err := v.BrowseSource(context.Background(), added.ID, "../.."); err == nil {
		t.Error("browsing left the source's folder")
	}
}
