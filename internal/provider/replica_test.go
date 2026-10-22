package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateReplicaFolder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    Kind
		parts   string
		replica string
		wantErr string // "" for valid
	}{
		{"none set is every old account", KindDropbox, "", "", ""},
		{"beside the parts folder", KindDropbox, "sand", "sand-replica", ""},
		{"slashes do not matter", KindWebDAV, "sand/", "/replica/", ""},
		{"a default parts folder counts", KindDropbox, "", "replica", ""}, // dropbox defaults to "sand"
		{"the same folder", KindBox, "sand", "sand", "is the parts folder"},
		{"the same folder with a slash", KindBox, "sand", "sand/", "is the parts folder"},
		{"inside the parts folder", KindOneDrive, "sand", "sand/replica", "inside the parts folder"},
		{"containing the parts folder", KindOneDrive, "a/sand", "a", "contains the parts folder"},
		{"a sibling with the parts folder as a prefix", KindS3, "sand", "sandbox", ""},
		{"parts at the root", KindS3, "", "replica", "parts are kept at the root"},
		{"parts at the root on drive", KindGDrive, "", "Vault replicas", "parts are kept at the root"},
		{"beside the parts folder on drive", KindGDrive, "SAND", "Vault replicas", ""},
		{"the replica folder is the root", KindGDrive, "SAND", "/", "cannot be the root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, _ := folderKey(tc.kind)
			cfg := Config{Name: "acct", Kind: tc.kind, Options: map[string]string{key: tc.parts}, ReplicaFolder: tc.replica}
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want ok", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateReplicaDirectoryOnDisk(t *testing.T) {
	base := t.TempDir()
	for _, tc := range []struct {
		name    string
		parts   string
		replica string
		wantErr string
	}{
		{"beside", filepath.Join(base, "sand"), filepath.Join(base, "replica"), ""},
		{"same", filepath.Join(base, "sand"), filepath.Join(base, "sand") + string(filepath.Separator), "is the parts directory"},
		{"inside", filepath.Join(base, "sand"), filepath.Join(base, "sand", "r"), "inside the parts directory"},
		{"containing", filepath.Join(base, "sand", "p"), filepath.Join(base, "sand"), "contains the parts directory"},
		{"prefix of the name is not inside", filepath.Join(base, "sand"), filepath.Join(base, "sand2"), ""},
		{"dot segments are cleaned", filepath.Join(base, "sand"), filepath.Join(base, "sand", "..", "sand"), "is the parts directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Name: "disk", Kind: KindLocal, Options: map[string]string{"path": tc.parts}, ReplicaFolder: tc.replica}
			err := cfg.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Validate = %v, want ok", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestOpenAtTheReplicasRoleWritesBesideTheParts(t *testing.T) {
	base := t.TempDir()
	cfg := Config{
		ID: "disk", Name: "disk", Kind: KindLocal,
		Options:       map[string]string{"path": filepath.Join(base, "sand")},
		ReplicaFolder: filepath.Join(base, "replica"),
	}
	ctx := context.Background()

	parts, err := Open(cfg, Parts)
	if err != nil {
		t.Fatal(err)
	}
	replicas, err := Open(cfg, Replicas)
	if err != nil {
		t.Fatal(err)
	}
	if err := parts.Put(ctx, "x-p1.sand", []byte("part")); err != nil {
		t.Fatal(err)
	}
	if err := replicas.Put(ctx, "x-p1.sand", []byte("replica")); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]string{
		filepath.Join(base, "sand", "x-p1.sand"):    "part",
		filepath.Join(base, "replica", "x-p1.sand"): "replica",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	listed, err := parts.List(ctx, "")
	if err != nil || len(listed) != 1 {
		t.Errorf("parts.List = %+v, %v; want the one part", listed, err)
	}
}

func TestOpenAtTheReplicasRoleRefusesAnAccountWithoutOne(t *testing.T) {
	cfg := Config{Name: "disk", Kind: KindLocal, Options: map[string]string{"path": t.TempDir()}}
	if _, err := Open(cfg, Replicas); err == nil || !strings.Contains(err.Error(), "no replica folder") {
		t.Fatalf("Open(Replicas) without a folder = %v, want a refusal", err)
	}
	if _, err := Open(cfg, Parts); err != nil {
		t.Fatalf("Open(Parts) without a replica folder = %v, want ok", err)
	}
}

func TestOpenAtTheReplicasRoleLeavesTheConfigAlone(t *testing.T) {
	cfg := Config{
		Name: "drive", Kind: KindGDrive,
		Options:       map[string]string{"client_id": "c", "client_secret": "s", "refresh_token": "r", "folder": "SAND", "folder_id": "pasted"},
		ReplicaFolder: "Vault replicas",
	}
	p, err := Open(cfg, Replicas)
	if err != nil {
		t.Fatal(err)
	}
	// The caller's options are untouched: the swap happened on a copy.
	if cfg.Options["folder"] != "SAND" || cfg.Options["folder_id"] != "pasted" {
		t.Errorf("Open changed the caller's options: %v", cfg.Options)
	}
	// And the provider opened at the replica folder by name, with the pasted
	// ID — which would have outranked the name — gone.
	drive, ok := p.(*gdriveProvider)
	if !ok {
		t.Fatalf("Open returned a %T", p)
	}
	if drive.folderName != "Vault replicas" || drive.folderID != "" {
		t.Errorf("drive opened at folderName=%q folderID=%q, want the replica folder and no ID", drive.folderName, drive.folderID)
	}
}

func TestPartsFolderFillsInTheBackendDefault(t *testing.T) {
	if got := (Config{Kind: KindDropbox}).PartsFolder(); got != "sand" {
		t.Errorf("Dropbox with no folder = %q, want the default sand", got)
	}
	if got := (Config{Kind: KindS3, Options: map[string]string{"prefix": "p/"}}).PartsFolder(); got != "p/" {
		t.Errorf("S3 prefix = %q, want it as typed", got)
	}
	if got := (Config{Kind: KindGDrive}).PartsFolder(); got != "" {
		t.Errorf("Drive with no folder = %q, want the root", got)
	}
}
