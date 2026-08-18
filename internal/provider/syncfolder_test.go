package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncFoldersAreRegistered walks the table the way the connect dialog
// does: every service has to arrive as a complete spec, since the dialog, the
// CLI and the folder picker are all built from nothing else.
func TestSyncFoldersAreRegistered(t *testing.T) {
	if len(syncFolders) < 6 {
		t.Fatalf("the synced-folder table has shrunk to %d services", len(syncFolders))
	}

	seen := map[Kind]bool{}
	orders := map[int]Kind{}
	for _, service := range syncFolders {
		spec, ok := SpecFor(service.kind)
		if !ok {
			t.Errorf("%s is not registered", service.kind)
			continue
		}
		if seen[service.kind] {
			t.Errorf("%s appears twice in the table", service.kind)
		}
		seen[service.kind] = true

		if other, clash := orders[spec.Order]; clash {
			t.Errorf("%s and %s both sort at %d", service.kind, other, spec.Order)
		}
		orders[spec.Order] = service.kind

		if spec.Label == "" || spec.Description == "" || spec.DocsURL == "" {
			t.Errorf("%s: incomplete spec %+v", service.kind, spec)
		}
		if spec.OAuth != nil {
			t.Errorf("%s: a synced folder has nothing to sign in to", service.kind)
		}
		if len(spec.Fields) != 1 {
			t.Fatalf("%s: %d fields, want one folder", service.kind, len(spec.Fields))
		}
		field := spec.Fields[0]
		if field.Key != "path" || !field.Required || !field.Directory {
			t.Errorf("%s: field = %+v, want a required, browsable path", service.kind, field)
		}
		// The machine SAND runs on is rarely the one the account holder is
		// holding, so the field has to arrive with a guess rather than expect a
		// path from memory.
		if field.Default == "" || field.Placeholder == "" || field.Help == "" {
			t.Errorf("%s: field arrives empty-handed: %+v", service.kind, field)
		}
		if !strings.Contains(field.Label, " ") {
			t.Errorf("%s: field label %q should name the service's own folder", service.kind, field.Label)
		}
	}
}

// TestSyncFolderDefaultPrefersAFolderThatExists: the guess is only worth
// making if it lands on the client's real folder when there is one.
func TestSyncFolderDefaultPrefersAFolderThatExists(t *testing.T) {
	home := t.TempDir()
	second := filepath.Join(home, "Second")
	if err := os.MkdirAll(second, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	service := syncFolder{
		folders: func(string) []string {
			return []string{filepath.Join(home, "First"), second}
		},
	}
	if got, want := service.defaultPath(), filepath.Join(second, "sand"); got != want {
		t.Errorf("defaultPath = %q, want the folder that exists (%q)", got, want)
	}

	// With nothing on disk it still has to say what kind of answer it wants.
	absent := syncFolder{
		folders: func(string) []string { return []string{filepath.Join(home, "First")} },
	}
	if got, want := absent.defaultPath(), filepath.Join(home, "First", "sand"); got != want {
		t.Errorf("defaultPath with no client installed = %q, want %q", got, want)
	}

	if got := (syncFolder{folders: func(string) []string { return nil }}).defaultPath(); got != "" {
		t.Errorf("defaultPath with no candidates = %q, want empty", got)
	}
}

// TestSyncFolderPingNeedsTheClientsFolder is the check that separates these
// backends from a plain directory: a folder the client does not sync would
// accept every part and upload none of them.
func TestSyncFolderPingNeedsTheClientsFolder(t *testing.T) {
	home := t.TempDir()
	synced := filepath.Join(home, "MEGA")
	if err := os.MkdirAll(synced, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A new folder inside the one the client syncs is the normal way to
	// connect one, and gets created.
	inside := filepath.Join(synced, "sand")
	p, err := New(Config{Kind: KindMega, Options: map[string]string{"path": inside}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping inside the synced folder: %v", err)
	}
	if info, err := os.Stat(inside); err != nil || !info.IsDir() {
		t.Errorf("the folder was not created: %v", err)
	}

	// A path whose parent is missing means the client is not on this machine.
	orphan := filepath.Join(home, "Nowhere", "sand")
	p, err = New(Config{Kind: KindMega, Options: map[string]string{"path": orphan}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = p.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping accepted a folder no client syncs")
	}
	if !strings.Contains(err.Error(), "MEGA") || !strings.Contains(err.Error(), filepath.Join(home, "Nowhere")) {
		t.Errorf("error names neither the client nor the missing folder: %v", err)
	}
}

// TestSyncFolderRoundTrip: past the form, these are the folder backend, and
// have to behave like it.
func TestSyncFolderRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sand")
	p, err := New(Config{Kind: KindProton, Name: "proton", Options: map[string]string{"path": root}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	payload := []byte("encrypted shard bytes")
	if err := p.Put(ctx, "abc123-p1.sand", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := p.Get(ctx, "abc123-p1.sand")
	if err != nil || string(got) != string(payload) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	info, err := p.Stat(ctx, "abc123-p1.sand")
	if err != nil || info.Size != int64(len(payload)) {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	objects, err := p.List(ctx, "abc")
	if err != nil || len(objects) != 1 || objects[0].Key != "abc123-p1.sand" {
		t.Fatalf("List = %+v, %v", objects, err)
	}
	if err := p.Delete(ctx, "abc123-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(ctx, "abc123-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if p.Config().Kind != KindProton {
		t.Errorf("the provider forgot which service it is: %+v", p.Config())
	}
}

// TestSyncFolderKindsAreDistinct guards the copy-paste failure the table
// exists to prevent: two services sharing a kind would silently replace one
// another in the registry, and an account connected to one would come back
// pointed at the other.
func TestSyncFolderKindsAreDistinct(t *testing.T) {
	for _, service := range syncFolders {
		spec, ok := SpecFor(service.kind)
		if !ok {
			t.Fatalf("%s is not registered", service.kind)
		}
		if spec.Label != service.label {
			t.Errorf("%s resolves to %q, not %q", service.kind, spec.Label, service.label)
		}
	}
}
