package vault

import (
	"bytes"
	"context"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/archive"
)

// shardFile is where one shard of one chunk sits on disk, found by looking
// in every local account for it.
func shardFile(t *testing.T, roots []string, e *Entry, chunk, part int) string {
	t.Helper()
	key := ChunkShardKey(e.ArchiveID, chunk, part)
	for _, root := range roots {
		candidate := filepath.Join(root, filepath.FromSlash(key))
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Fatalf("no account holds %s", key)
	return ""
}

func flipByte(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-20] ^= 0x01
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func noise(n int, seed int64) []byte {
	out := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(out)
	return out
}

func photo(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 80, 60))
	rng := rand.New(rand.NewSource(7))
	for y := 0; y < 60; y++ {
		for x := 0; x < 80; x++ {
			img.Set(x, y, color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(x), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func upload(t *testing.T, v *Vault, name string, data []byte) *Entry {
	t.Helper()
	entry, _, err := v.Upload(context.Background(), MainScope, "/", name, data, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload %s: %v", name, err)
	}
	v.AwaitBackupSync()
	return copyEntry(entry)
}

func checkOne(t *testing.T, v *Vault, id string) *FileCheck {
	t.Helper()
	v.chunks.clear()
	check, err := v.CheckFile(context.Background(), id)
	if err != nil {
		t.Fatalf("CheckFile: %v", err)
	}
	return check
}

// The bug the checker was written beside: one damaged shard used to fail the
// read of a file every other account could rebuild, whenever the damaged one
// was among the first k to answer. On local accounts every shard answers, so
// damaging a data shard is enough to have been fatal every time.
func TestReadSurvivesADamagedShard(t *testing.T) {
	v, roots := chunkedVault(t, 5, 4096)
	payload := noise(40_000, 1)
	entry := upload(t, v, "film.bin", payload)
	if entry.Scheme().Total-entry.Scheme().Data < 1 {
		t.Fatalf("scheme %s has no spare", entry.Scheme())
	}

	// Damage shard 1 on every chunk three different ways.
	flipByte(t, shardFile(t, roots, entry, 0, 1))
	if err := os.WriteFile(shardFile(t, roots, entry, 1, 1), nil, 0o600); err != nil { // emptied
		t.Fatal(err)
	}
	other, _ := os.ReadFile(shardFile(t, roots, entry, 3, 2)) // another chunk's shard, intact
	if err := os.WriteFile(shardFile(t, roots, entry, 2, 1), other, 0o600); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ { // the race is a race: run it enough to lose it
		v.chunks.clear()
		got, _, err := v.Fetch(context.Background(), entry.ID)
		if err != nil {
			t.Fatalf("Fetch with one shard damaged: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("the file read back differently")
		}
	}
}

func TestReadFailsOnceTooManyShardsAreDamaged(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	entry := upload(t, v, "a.bin", noise(3000, 2))
	flipByte(t, shardFile(t, roots, entry, 0, 1))
	flipByte(t, shardFile(t, roots, entry, 0, 3))
	v.chunks.clear()
	_, _, err := v.Fetch(context.Background(), entry.ID)
	if err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("Fetch = %v, want an error naming the damage", err)
	}
}

func TestCheckFindsASoundFileSound(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	entry := upload(t, v, "IMG_0001.jpg", photo(t))
	c := checkOne(t, v, entry.ID)
	if c.State != CheckSound || !c.HashOK || c.Good != 3 || c.Fix != "" {
		t.Fatalf("got %+v", c)
	}
	if c.Content == nil || c.Content.Format != "JPEG" {
		t.Errorf("content = %+v, want a sound JPEG", c.Content)
	}
}

func TestCheckNamesEachKindOfBadShardAndRepairPutsThemBack(t *testing.T) {
	v, roots := chunkedVault(t, 6, 4096)
	payload := noise(20_000, 3)
	stored, _, err := v.Upload(context.Background(), MainScope, "/", "doc.bin", payload,
		UploadOptions{Scheme: archive.Scheme{Data: 4, Total: 6}})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	v.AwaitBackupSync()
	entry := copyEntry(stored)
	scheme := entry.Scheme()
	if scheme.Total-scheme.Data < 2 {
		t.Fatalf("scheme %s cannot lose two shards", scheme)
	}

	flipByte(t, shardFile(t, roots, entry, 2, 1))
	for chunk := 0; chunk < entry.ChunkCount; chunk++ {
		if err := os.Remove(shardFile(t, roots, entry, chunk, 4)); err != nil {
			t.Fatal(err)
		}
	}

	c := checkOne(t, v, entry.ID)
	if c.State != CheckRepairable || c.Fix != CheckRebuild || !c.HashOK {
		t.Fatalf("got state %s fix %q: %s", c.State, c.Fix, c.Summary)
	}
	if c.Good != scheme.Total-2 || len(c.Faults) != 2 {
		t.Fatalf("good = %d, faults = %+v", c.Good, c.Faults)
	}
	damaged, missing := c.Faults[0], c.Faults[1]
	if damaged.Part != 1 || damaged.Kind != FaultDamaged || damaged.Chunks != 1 || damaged.FirstChunk != 2 {
		t.Errorf("part 1: %+v", damaged)
	}
	if missing.Part != 4 || missing.Kind != FaultMissing || missing.Chunks != entry.ChunkCount {
		t.Errorf("part 4: %+v", missing)
	}

	report, err := v.RepairChecked(context.Background(), []FileCheck{*c}, nil)
	if err != nil {
		t.Fatalf("RepairChecked: %v", err)
	}
	if report.Repaired != 1 {
		t.Fatalf("report = %+v", report)
	}
	after := checkOne(t, v, entry.ID)
	if after.State != CheckSound || after.Good != scheme.Total {
		t.Fatalf("after the repair: %+v", after)
	}
	got, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the repaired file does not read back: %v", err)
	}

	// The findings are about parts that are gone now, and are refused.
	again, err := v.RepairChecked(context.Background(), []FileCheck{*c}, nil)
	if err != nil || again.Deferred != 1 || again.Repaired != 0 {
		t.Fatalf("a stale repair: %+v, %v", again, err)
	}
}

func TestCheckFindsAShardThatOpensButDisagrees(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	entry := upload(t, v, "a.bin", noise(2500, 4))

	// A parity shard sealed properly — right key, right header — over the
	// wrong bytes: what a writer with a bug would leave, and what no read
	// that stops at k shards would ever look at.
	v.mu.RLock()
	key, err := v.dataKeyForLocked(entry.KeyID)
	v.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	var id [16]byte
	copy(id[:], mustHex(t, entry.ArchiveID))
	wrong := noise(2500, 5)
	plan, err := archive.PlanChunks(id, entry.Name, [32]byte(mustHex(t, entry.Hash)), uint64(entry.Size), 4096, entry.Scheme())
	if err != nil {
		t.Fatal(err)
	}
	forged, err := archive.EncodeChunk(plan, 0, wrong, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shardFile(t, roots, entry, 0, 3), forged.Parts[2], 0o600); err != nil {
		t.Fatal(err)
	}

	c := checkOne(t, v, entry.ID)
	if c.State != CheckRepairable || len(c.Faults) != 1 || c.Faults[0].Kind != FaultMismatched || c.Faults[0].Part != 3 {
		t.Fatalf("got %s: %+v", c.State, c.Faults)
	}
}

func TestCheckTellsDamageThatArrivedFromDamageInTheVault(t *testing.T) {
	v, _ := chunkedVault(t, 3, 4096)
	whole := photo(t)
	entry := upload(t, v, "cut.jpg", whole[:len(whole)/2])
	c := checkOne(t, v, entry.ID)
	if c.State != CheckDamagedAtSource || !c.HashOK || c.Fix != "" || len(c.Faults) != 0 {
		t.Fatalf("got %+v", c)
	}
	if !strings.Contains(c.Summary, "exactly what was uploaded") {
		t.Errorf("summary = %q", c.Summary)
	}
}

func TestCheckUnreadableOffersTheReplicaAndRepairUsesIt(t *testing.T) {
	v, roots, _ := replicaVault(t, 4, 3, archive.Scheme{Data: 2, Total: 3})
	ids := providerIDs(t, v)
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("a file worth keeping twice "), 400)
	entry := replicatedFile(t, v, ids[:3], "/keep.bin", payload)
	replicate(t, v)
	v.AwaitBackupSync()

	for chunk := 0; chunk < entry.ChunkCount; chunk++ {
		flipByte(t, shardFile(t, roots, entry, chunk, 1))
		flipByte(t, shardFile(t, roots, entry, chunk, 2))
	}

	c := checkOne(t, v, entry.ID)
	if c.State != CheckUnreadable || c.Fix != CheckFromReplica || c.Rebuilt {
		t.Fatalf("got state %s fix %q: %s", c.State, c.Fix, c.Summary)
	}
	report, err := v.RepairChecked(context.Background(), []FileCheck{*c}, nil)
	if err != nil || report.Repaired != 1 {
		t.Fatalf("RepairChecked: %+v, %v", report, err)
	}
	if after := checkOne(t, v, entry.ID); after.State != CheckSound {
		t.Fatalf("after: %+v", after)
	}
	got, _, err := v.Fetch(context.Background(), entry.ID)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("does not read back: %v", err)
	}
}

func TestCheckUnreadableWithoutAReplicaOffersNothing(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	entry := upload(t, v, "a.bin", noise(3000, 6))
	flipByte(t, shardFile(t, roots, entry, 0, 1))
	flipByte(t, shardFile(t, roots, entry, 0, 2))
	c := checkOne(t, v, entry.ID)
	if c.State != CheckUnreadable || c.Fix != "" || len(c.Faults) != 2 {
		t.Fatalf("got %+v", c)
	}
}

func TestCheckFolderCountsAndNarrowsByKind(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	upload(t, v, "good.jpg", photo(t))
	bad := upload(t, v, "bad.jpg", photo(t))
	upload(t, v, "notes.txt", []byte("some notes"))
	flipByte(t, shardFile(t, roots, bad, 0, 2))
	v.chunks.clear()

	survey, err := v.SurveyCheck(MainScope, "/")
	if err != nil || survey.Files != 3 {
		t.Fatalf("survey = %+v, %v", survey, err)
	}

	var last CheckProgress
	report, err := v.CheckFolder(context.Background(), MainScope, "/", CheckOptions{
		Kinds:      []string{"images"},
		OnProgress: func(p CheckProgress) { last = p },
	})
	if err != nil {
		t.Fatalf("CheckFolder: %v", err)
	}
	if report.Files != 2 || report.Checked != 2 || report.Sound != 1 || report.Repairable != 1 || report.Fixable != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Flagged) != 1 || report.Flagged[0].ID != bad.ID {
		t.Fatalf("flagged = %+v", report.Flagged)
	}
	if last.Files != 2 || last.Bytes != last.Total {
		t.Errorf("last progress = %+v", last)
	}

	// A check that is stopped says so, and reports what it reached.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped, err := v.CheckFolder(ctx, MainScope, "/", CheckOptions{})
	if err != nil || !stopped.Stopped || stopped.Checked != 0 {
		t.Fatalf("stopped = %+v, %v", stopped, err)
	}
}

func TestCheckNeverWrites(t *testing.T) {
	v, roots := chunkedVault(t, 3, 4096)
	entry := upload(t, v, "a.bin", noise(9000, 8))
	flipByte(t, shardFile(t, roots, entry, 1, 3))
	before := snapshotTree(t, roots)
	checkOne(t, v, entry.ID)
	if after := snapshotTree(t, roots); after != before {
		t.Error("a check changed what is on the accounts")
	}
}

func snapshotTree(t *testing.T, roots []string) string {
	t.Helper()
	var b strings.Builder
	for _, root := range roots {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.HasSuffix(path, "-p1.sand") ||
				err == nil && !info.IsDir() && strings.Contains(path, "-c0") {
				data, _ := os.ReadFile(path)
				b.WriteString(path)
				b.Write(sha256Sum(data))
			}
			return nil
		})
	}
	return b.String()
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	out, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
