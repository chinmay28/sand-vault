package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
	"github.com/chinmay28/sand-vault/internal/vault"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("an account did not answer") }

// A download that fails partway leaves the file that was there exactly as it
// was — it used to be truncated first and deleted after.
func TestWriteLocalFileKeepsTheExistingFileWhenTheBodyFails(t *testing.T) {
	target := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(target, []byte("the copy worth keeping"), 0600); err != nil {
		t.Fatal(err)
	}

	body := io.MultiReader(strings.NewReader("half of a new"), failingReader{})
	if _, err := writeLocalFile(target, body, true); err == nil {
		t.Fatal("a download whose body failed was reported as written")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the existing file is gone: %v", err)
	}
	if string(got) != "the copy worth keeping" {
		t.Errorf("the existing file was changed to %q", got)
	}
	if _, err := os.Stat(target + ".sand-tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the temporary file was left behind")
	}
}

func TestWriteLocalFileRefusesToReplaceWithoutForce(t *testing.T) {
	target := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(target, []byte("already here"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := writeLocalFile(target, strings.NewReader("new"), false); err == nil {
		t.Fatal("an existing file was replaced without --force")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the refusal does not say how to override it: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "already here" {
		t.Errorf("the existing file was changed to %q", got)
	}

	written, err := writeLocalFile(target, strings.NewReader("new"), true)
	if err != nil {
		t.Fatalf("with --force: %v", err)
	}
	if written != 3 {
		t.Errorf("written = %d, want 3", written)
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Errorf("after --force the file holds %q", got)
	}
	if _, err := os.Stat(target + ".sand-tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the temporary file was left behind")
	}
}

func TestWriteLocalFileWritesANewFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "fresh.bin")
	if _, err := writeLocalFile(target, strings.NewReader("brand new"), false); err != nil {
		t.Fatalf("writeLocalFile: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "brand new" {
		t.Errorf("the file holds %q", got)
	}
}

// The whole command, against a vault whose accounts have gone dark: the local
// file that `get -o` was pointed at has to survive.
func TestGetDoesNotDestroyTheLocalFileWhenTheVaultCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, "vault.sand")
	const password = "a perfectly ordinary password"

	v, err := vault.Open(vaultPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := v.Init(password, vault.PolicyStrict); err != nil {
		t.Fatalf("Init: %v", err)
	}
	roots := make([]string, 3)
	for i := range roots {
		roots[i] = filepath.Join(dir, "cloud", string(rune('a'+i)))
		if _, err := v.AddProvider(context.Background(), provider.Config{
			Kind:    provider.KindLocal,
			Name:    "cloud-" + string(rune('a'+i)),
			Options: map[string]string{"path": roots[i]},
		}); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}
	stored := bytes.Repeat([]byte("stored in the vault "), 200)
	if _, _, err := v.Upload(context.Background(), vault.MainScope, "/", "report.txt", stored, vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()
	v.AwaitReadHistory()
	v.Lock()

	// Two of the three accounts gone: a 2-of-3 file cannot be read.
	for _, root := range roots[1:] {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
	}

	target := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(target, []byte("the local copy"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SAND_PASSWORD", password)
	root := rootCmd()
	root.SetArgs([]string{"get", "/report.txt", "-o", target, "--force", "--vault", vaultPath})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err == nil {
		t.Fatal("get from a vault with two accounts gone succeeded")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the local file is gone: %v", err)
	}
	if string(got) != "the local copy" {
		t.Errorf("the local file was changed to %q", got)
	}
	if _, err := os.Stat(target + ".sand-tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the temporary file was left behind")
	}

	// And without --force it does not even try.
	root = rootCmd()
	root.SetArgs([]string{"get", "/report.txt", "-o", target, "--vault", vaultPath})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	err = root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("get over an existing file without --force = %v, want a refusal naming --force", err)
	}
}
