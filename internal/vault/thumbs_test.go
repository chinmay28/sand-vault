package vault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// storeThumb puts a recognizable stand-in for a JPEG against a file. Nothing
// in the vault decodes a thumbnail — normalizing it into a real image happens
// before it gets here — so the bytes only have to come back unchanged.
func storeThumb(t *testing.T, v *Vault, id string, body string) []byte {
	t.Helper()

	thumb := []byte(body)
	if err := v.SetThumb(context.Background(), id, thumb); err != nil {
		t.Fatalf("SetThumb: %v", err)
	}
	return thumb
}

func TestThumbRoundTrip(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "photo.jpg", []byte("not really a photo"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if _, err := v.Thumb(ctx, entry.ID); !errors.Is(err, ErrNoThumb) {
		t.Fatalf("Thumb before storing = %v, want ErrNoThumb", err)
	}

	want := storeThumb(t, v, entry.ID, "thumbnail-bytes")

	got, err := v.Thumb(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Thumb: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Thumb = %q, want %q", got, want)
	}

	if ids := v.ThumbIDs(MainScope, "/"); len(ids) != 1 || ids[0] != entry.ID {
		t.Errorf("ThumbIDs = %v, want [%s]", ids, entry.ID)
	}

	listing, err := v.List(MainScope, "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Thumbs) != 1 || listing.Thumbs[0] != entry.ID {
		t.Errorf("listing thumbs = %v, want [%s]", listing.Thumbs, entry.ID)
	}
}

// The pack has to survive the vault being closed and reopened, because that is
// the whole point of storing it on the accounts rather than in memory.
func TestThumbSurvivesLockAndUnlock(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "photo.jpg", []byte("not really a photo"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	want := storeThumb(t, v, entry.ID, "thumbnail-bytes")

	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	got, err := v.Thumb(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Thumb after unlock: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Thumb = %q, want %q — the pack should have been gathered from the accounts", got, want)
	}
}

// One pack per folder is the design; several files must share it rather than
// each replacing the last.
func TestThumbPackHoldsTheWholeFolder(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	ids := map[string][]byte{}
	for i := 0; i < 4; i++ {
		entry, _, err := v.Upload(ctx, MainScope, "/", fmt.Sprintf("photo-%d.jpg", i), []byte("x"), UploadOptions{})
		if err != nil {
			t.Fatalf("Upload %d: %v", i, err)
		}
		ids[entry.ID] = storeThumb(t, v, entry.ID, fmt.Sprintf("thumb-%d", i))
	}

	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	for id, want := range ids {
		got, err := v.Thumb(ctx, id)
		if err != nil {
			t.Fatalf("Thumb %s: %v", id, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Thumb %s = %q, want %q", id, got, want)
		}
	}

	v.mu.RLock()
	packs := len(v.manifest.Thumbs)
	v.mu.RUnlock()
	if packs != 1 {
		t.Errorf("stored %d packs for one folder, want 1", packs)
	}
}

func TestDeleteDropsTheThumbnail(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	keep, _, err := v.Upload(ctx, MainScope, "/", "keep.jpg", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	doomed, _, err := v.Upload(ctx, MainScope, "/", "doomed.jpg", []byte("y"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	storeThumb(t, v, keep.ID, "keep-thumb")
	storeThumb(t, v, doomed.ID, "doomed-thumb")

	if _, err := v.Delete(ctx, doomed.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := v.Thumb(ctx, doomed.ID); err == nil {
		t.Error("the deleted file still has a thumbnail")
	}
	if _, err := v.Thumb(ctx, keep.ID); err != nil {
		t.Errorf("deleting one file cost another its thumbnail: %v", err)
	}
	if ids := v.ThumbIDs(MainScope, "/"); len(ids) != 1 || ids[0] != keep.ID {
		t.Errorf("ThumbIDs = %v, want [%s]", ids, keep.ID)
	}
}

func TestMoveCarriesTheThumbnail(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "photo.jpg", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	want := storeThumb(t, v, entry.ID, "thumbnail-bytes")

	if err := v.Mkdir(MainScope, "/album"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, err := v.Move(ctx, entry.ID, "/album", ""); err != nil {
		t.Fatalf("Move: %v", err)
	}

	got, err := v.Thumb(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Thumb after move: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Thumb = %q, want %q", got, want)
	}
	if ids := v.ThumbIDs(MainScope, "/"); len(ids) != 0 {
		t.Errorf("the old folder still lists %v", ids)
	}
	if ids := v.ThumbIDs(MainScope, "/album"); len(ids) != 1 || ids[0] != entry.ID {
		t.Errorf("ThumbIDs(/album) = %v, want [%s]", ids, entry.ID)
	}
}

func TestRmdirDropsThePack(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	if err := v.Mkdir(MainScope, "/album/summer"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entry, _, err := v.Upload(ctx, MainScope, "/album/summer", "photo.jpg", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	storeThumb(t, v, entry.ID, "thumbnail-bytes")

	if _, err := v.Rmdir(ctx, MainScope, "/album", true, nil); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}

	v.mu.RLock()
	packs := len(v.manifest.Thumbs)
	v.mu.RUnlock()
	if packs != 0 {
		t.Errorf("%d packs left after the folder was removed, want 0", packs)
	}
}

// Thumbnails are derived data sealed under the key being retired, so a
// password change drops them rather than paying to re-encrypt them. What
// matters is that the vault stays coherent afterwards: nothing promises a
// picture that can no longer be drawn.
func TestChangePasswordDropsThumbnails(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "photo.jpg", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	storeThumb(t, v, entry.ID, "thumbnail-bytes")

	if _, err := v.ChangePassword(ctx, testPassword, "a whole new password", true); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	if ids := v.ThumbIDs(MainScope, "/"); len(ids) != 0 {
		t.Errorf("ThumbIDs = %v, want none after a password change", ids)
	}
	if _, err := v.Thumb(ctx, entry.ID); !errors.Is(err, ErrNoThumb) {
		t.Errorf("Thumb = %v, want ErrNoThumb", err)
	}

	// And the file itself is still readable, which is the part that would
	// really hurt to get wrong.
	if _, _, err := v.Fetch(ctx, entry.ID); err != nil {
		t.Fatalf("Fetch after password change: %v", err)
	}

	// A fresh thumbnail can be stored under the new key.
	want := storeThumb(t, v, entry.ID, "new-thumbnail")
	got, err := v.Thumb(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Thumb after re-storing: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Thumb = %q, want %q", got, want)
	}
}

func TestSetThumbRejectsOversized(t *testing.T) {
	v, _ := newTestVault(t, 3)
	ctx := context.Background()

	entry, _, err := v.Upload(ctx, MainScope, "/", "photo.jpg", []byte("x"), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := v.SetThumb(ctx, entry.ID, make([]byte, MaxThumbBytes+1)); err == nil {
		t.Fatal("expected an error for an oversized thumbnail, got none")
	}
}

func TestPackEncodingRoundTrip(t *testing.T) {
	items := map[string][]byte{
		"a": []byte("first"),
		"b": {},
		"c": bytes.Repeat([]byte{0xff, 0x00}, 5000),
	}

	got, err := decodePack(encodePack(items))
	if err != nil {
		t.Fatalf("decodePack: %v", err)
	}
	if len(got) != len(items) {
		t.Fatalf("decoded %d entries, want %d", len(got), len(items))
	}
	for id, want := range items {
		if !bytes.Equal(got[id], want) {
			t.Errorf("entry %s = %q, want %q", id, got[id], want)
		}
	}

	// Encoding is order-independent, so re-storing the same thumbnails does
	// not scatter a different archive for no reason.
	if !bytes.Equal(encodePack(items), encodePack(map[string][]byte{
		"c": items["c"], "a": items["a"], "b": items["b"],
	})) {
		t.Error("encodePack is not stable across map iteration order")
	}
}

func TestDecodePackRejectsTruncated(t *testing.T) {
	blob := encodePack(map[string][]byte{"a": []byte("first")})
	if _, err := decodePack(blob[:len(blob)-2]); err == nil {
		t.Fatal("expected an error for a truncated pack, got none")
	}
}
