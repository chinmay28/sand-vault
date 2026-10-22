package vault

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/archive"
)

// replicaVault is a chunked vault whose accounts each have a replica folder
// beside their parts folder, and a policy naming the first n of them.
func replicaVault(t *testing.T, accounts, named int, scheme archive.Scheme) (*Vault, []string, []string) {
	t.Helper()
	v, roots := chunkedVault(t, accounts, 1024)
	ids := providerIDs(t, v)

	replicaRoots := make([]string, accounts)
	var chosen []ReplicaAccount
	for i := range roots {
		replicaRoots[i] = roots[i] + "-replica"
		if i < named {
			chosen = append(chosen, ReplicaAccount{ID: ids[i], ReplicaFolder: replicaRoots[i]})
		}
	}
	if err := v.SetReplication(ReplicationSettings{Accounts: chosen, Scheme: scheme}); err != nil {
		t.Fatalf("SetReplication: %v", err)
	}
	return v, roots, replicaRoots
}

func providerIDs(t *testing.T, v *Vault) []string {
	t.Helper()
	v.mu.RLock()
	defer v.mu.RUnlock()
	ids := make([]string, 0, len(v.providers))
	for _, cfg := range v.providers {
		ids = append(ids, cfg.ID)
	}
	return ids
}

func on(b bool) *bool { return &b }

// replicate runs the worker until it reports idle, failing on any warning.
func replicate(t *testing.T, v *Vault) int {
	t.Helper()
	made := 0
	for i := 0; i < 50; i++ {
		step, err := v.ReplicateStep(context.Background(), time.Now())
		if err != nil {
			t.Fatalf("ReplicateStep: %v", err)
		}
		if step.Warning != "" {
			t.Fatalf("ReplicateStep warned: %s", step.Warning)
		}
		if step.Idle {
			return made
		}
		if step.Done {
			made++
		}
	}
	t.Fatalf("the worker never went idle")
	return made
}

func readFrom(t *testing.T, v *Vault, id string, source ReadSource) ([]byte, error) {
	t.Helper()
	r, _, err := v.OpenReadSeeker(context.Background(), id, ReadFrom(source))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func sandObjectsIn(t *testing.T, root string) int {
	t.Helper()
	count := 0
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".sand") && info.Name() != "manifest.sand" {
			count++
		}
		return nil
	})
	return count
}

func TestSetReplicationChecksTheFoldersAndTheCode(t *testing.T) {
	v, roots := chunkedVault(t, 3, 1024)
	ids := providerIDs(t, v)

	for _, tc := range []struct {
		name string
		s    ReplicationSettings
		want string
	}{
		{"a folder inside the parts folder", ReplicationSettings{Accounts: []ReplicaAccount{{ID: ids[0], ReplicaFolder: filepath.Join(roots[0], "replica")}}}, "inside the parts directory"},
		{"the parts folder itself", ReplicationSettings{Accounts: []ReplicaAccount{{ID: ids[0], ReplicaFolder: roots[0]}}}, "is the parts directory"},
		{"no folder", ReplicationSettings{Accounts: []ReplicaAccount{{ID: ids[0]}}}, "needs a replica folder"},
		{"an unknown account", ReplicationSettings{Accounts: []ReplicaAccount{{ID: "nope", ReplicaFolder: "/tmp/x"}}}, "no connected account"},
		{"a split for one cloud", ReplicationSettings{Accounts: []ReplicaAccount{{ID: ids[0], ReplicaFolder: roots[0] + "-r"}}, Scheme: archive.Scheme{Data: 2, Total: 3}}, "whole copy"},
		{"a whole copy for three", ReplicationSettings{Accounts: []ReplicaAccount{{ID: ids[0], ReplicaFolder: roots[0] + "-r"}, {ID: ids[1], ReplicaFolder: roots[1] + "-r"}, {ID: ids[2], ReplicaFolder: roots[2] + "-r"}}, Scheme: archive.WholeCopy}, "choose a code"},
		{"a code of the wrong width", ReplicationSettings{Accounts: []ReplicaAccount{{ID: ids[0], ReplicaFolder: roots[0] + "-r"}, {ID: ids[1], ReplicaFolder: roots[1] + "-r"}}, Scheme: archive.Scheme{Data: 2, Total: 3}}, "names 3 clouds"},
		{"an account twice", ReplicationSettings{Accounts: []ReplicaAccount{{ID: ids[0], ReplicaFolder: roots[0] + "-r"}, {ID: ids[0], ReplicaFolder: roots[0] + "-r2"}}}, "listed twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := v.SetReplication(tc.s)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SetReplication = %v, want an error containing %q", err, tc.want)
			}
		})
	}

	// Nothing was applied by any refusal.
	policy, folders, err := v.Replication()
	if err != nil || policy != nil || len(folders) != 0 {
		t.Fatalf("after refusals Replication() = %+v, %v, %v; want nothing set", policy, folders, err)
	}

	// One account takes the whole copy by default; three take 2-of-3.
	if err := v.SetReplication(ReplicationSettings{Accounts: []ReplicaAccount{{ID: ids[2], ReplicaFolder: roots[2] + "-r"}}}); err != nil {
		t.Fatal(err)
	}
	policy, folders, _ = v.Replication()
	if !policy.Scheme().IsWholeCopy() || folders[ids[2]] != roots[2]+"-r" {
		t.Errorf("one account: policy %+v folders %v, want a whole copy in the named folder", policy, folders)
	}
	if err := v.SetReplication(ReplicationSettings{Accounts: []ReplicaAccount{
		{ID: ids[0], ReplicaFolder: roots[0] + "-r"}, {ID: ids[1], ReplicaFolder: roots[1] + "-r"}, {ID: ids[2], ReplicaFolder: roots[2] + "-r"},
	}, BytesPerDay: 5 << 30}); err != nil {
		t.Fatal(err)
	}
	policy, _, _ = v.Replication()
	if policy.Scheme() != (archive.Scheme{Data: 2, Total: 3}) || policy.BytesPerDay != 5<<30 {
		t.Errorf("three accounts: policy %+v, want 2-of-3 at 5 GB a day", policy)
	}

	// Clearing keeps the folders — replicas already made are read through
	// them — and drops the policy.
	if err := v.SetReplication(ReplicationSettings{}); err != nil {
		t.Fatal(err)
	}
	policy, folders, _ = v.Replication()
	if policy != nil || len(folders) != 3 {
		t.Errorf("cleared: policy %+v folders %v", policy, folders)
	}
}

func TestReplicaFlagsInheritDownTheTree(t *testing.T) {
	v, _ := chunkedVault(t, 3, 1024)
	for _, dir := range []string{"/photos", "/photos/2024", "/docs"} {
		if err := v.Mkdir(MainScope, dir); err != nil {
			t.Fatal(err)
		}
	}
	inPhotos := uploadSized(t, v, "/photos", "a.bin", 100)
	in2024 := uploadSized(t, v, "/photos/2024", "b.bin", 100)
	inDocs := uploadSized(t, v, "/docs", "c.bin", 100)

	if err := v.SetReplicate("/photos", on(true)); err != nil {
		t.Fatal(err)
	}
	if err := v.SetReplicate("/photos/2024", on(false)); err != nil {
		t.Fatal(err)
	}
	if err := v.SetReplicate("/docs/c.bin", on(true)); err != nil {
		t.Fatal(err)
	}
	if err := v.SetReplicate("/nowhere", on(true)); err == nil {
		t.Error("flagging a folder that does not exist was allowed")
	}

	v.mu.RLock()
	m := v.manifest
	got := map[string]bool{
		"photos": replicatedLocked(m, m.ByID(inPhotos.ID)),
		"2024":   replicatedLocked(m, m.ByID(in2024.ID)),
		"docs":   replicatedLocked(m, m.ByID(inDocs.ID)),
	}
	v.mu.RUnlock()
	if !got["photos"] || got["2024"] || !got["docs"] {
		t.Errorf("replicated = %v; want photos on, 2024 off (nearer folder), docs on (the file's own say)", got)
	}

	listing, err := v.List(MainScope, "/photos")
	if err != nil {
		t.Fatal(err)
	}
	if !listing.Replicate.Enabled || listing.Replicate.Source != "/photos" {
		t.Errorf("listing of /photos says %+v", listing.Replicate)
	}
	if listing.ReplicatedFolders["/photos/2024"] {
		t.Errorf("2024 is listed as replicated though it opted out: %v", listing.ReplicatedFolders)
	}
	root, _ := v.List(MainScope, "/")
	if !root.ReplicatedFolders["/photos"] || root.ReplicatedFolders["/docs"] {
		t.Errorf("root's replicated folders = %v, want photos only", root.ReplicatedFolders)
	}

	// Back to inheriting: the file follows /docs, which says nothing.
	if err := v.SetReplicate("/docs/c.bin", nil); err != nil {
		t.Fatal(err)
	}
	v.mu.RLock()
	stillOn := replicatedLocked(v.manifest, v.manifest.ByID(inDocs.ID))
	v.mu.RUnlock()
	if stillOn {
		t.Error("a file put back to inheriting still replicates")
	}
}

func TestWholeCopyOnOneCloudReadsBackAndStaysOutOfTheParts(t *testing.T) {
	v, roots, replicaRoots := replicaVault(t, 3, 1, archive.Scheme{})
	// The policy named cloud-a alone; the replica folder lives beside its
	// parts folder.
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("whole copy "), 500)
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "film.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	partsBefore := objectsUnder(t, roots)

	if made := replicate(t, v); made != 1 {
		t.Fatalf("made %d replicas, want 1", made)
	}

	v.mu.RLock()
	r := v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()
	if r == nil {
		t.Fatal("no replica recorded")
	}
	if !r.Scheme().IsWholeCopy() || len(r.Shards) != 1 || r.Hash != entry.Hash || r.ArchiveID == entry.ArchiveID {
		t.Errorf("replica = %+v; want a whole copy, one shard, the file's hash, its own archive ID", r)
	}
	if r.Shards[0].ProviderID != providerIDs(t, v)[0] || strings.Contains(r.Shards[0].ProviderID, "#") {
		t.Errorf("replica shard names account %q, want cloud-a's real ID", r.Shards[0].ProviderID)
	}
	if got := objectsUnder(t, roots); got != partsBefore {
		t.Errorf("the parts folders hold %d objects after replicating, %d before: a replica landed among the parts", got, partsBefore)
	}
	if got := sandObjectsIn(t, replicaRoots[0]); got != r.ChunkCount {
		t.Errorf("cloud-a's replica folder holds %d objects, want one per chunk (%d)", got, r.ChunkCount)
	}
	for _, other := range replicaRoots[1:] {
		if sandObjectsIn(t, other) != 0 {
			t.Errorf("%s holds replica objects though it is not in the policy", other)
		}
	}

	for _, source := range []ReadSource{ReadParts, ReadReplica, ReadEither} {
		got, err := readFrom(t, v, entry.ID, source)
		if err != nil || !bytes.Equal(got, payload) {
			t.Errorf("reading from %s: %v (%d bytes)", source, err, len(got))
		}
	}

	// A second pass has nothing to do.
	if made := replicate(t, v); made != 0 {
		t.Errorf("a second pass made %d replicas, want none", made)
	}
}

func TestReplicaCutAcrossCloudsStandsInWhenThePartsAreGone(t *testing.T) {
	v, roots, replicaRoots := replicaVault(t, 3, 3, archive.Scheme{Data: 2, Total: 3})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("spread "), 700)
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "doc.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replicate(t, v)

	v.mu.RLock()
	r := v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()
	if r == nil || r.Scheme() != (archive.Scheme{Data: 2, Total: 3}) || len(r.Shards) != 3 {
		t.Fatalf("replica = %+v, want 2-of-3 on three accounts", r)
	}
	for _, root := range replicaRoots {
		if sandObjectsIn(t, root) != r.ChunkCount {
			t.Errorf("%s holds %d replica objects, want %d", root, sandObjectsIn(t, root), r.ChunkCount)
		}
	}

	// Two of the three parts folders go dark. The parts cannot rebuild the
	// file; the replica, in folders of its own, can.
	emptyCloud(t, roots[0])
	emptyCloud(t, roots[1])
	v.chunks.clear()
	if _, err := readFrom(t, v, entry.ID, ReadParts); err == nil {
		t.Fatal("the parts still read with two of three accounts emptied")
	}
	for _, source := range []ReadSource{ReadReplica, ReadEither} {
		got, err := readFrom(t, v, entry.ID, source)
		if err != nil || !bytes.Equal(got, payload) {
			t.Errorf("reading from %s with the parts gone: %v (%d bytes)", source, err, len(got))
		}
	}

	// And the replica itself survives one of its three accounts going.
	emptyCloud(t, replicaRoots[2])
	v.chunks.clear()
	if got, err := readFrom(t, v, entry.ID, ReadReplica); err != nil || !bytes.Equal(got, payload) {
		t.Errorf("reading the replica with one of its accounts emptied: %v", err)
	}
}

func TestReplicaIsRemadeWhenTheFileChangesAndErasedWithIt(t *testing.T) {
	v, _, replicaRoots := replicaVault(t, 3, 1, archive.Scheme{})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	first, _, err := v.Upload(context.Background(), MainScope, "/", "notes.bin", bytes.Repeat([]byte("v1 "), 1000), UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replicate(t, v)
	v.mu.RLock()
	old := v.manifest.ByID(first.ID).Replica
	v.mu.RUnlock()

	// Overwritten: the entry keeps its path, the bytes change, and the
	// replica is of bytes nothing holds any more.
	second, _, err := v.Upload(context.Background(), MainScope, "/", "notes.bin", bytes.Repeat([]byte("v2 "), 1500), UploadOptions{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	status, _ := v.ReplicationStatus(time.Now())
	if status.Pending != 1 {
		t.Errorf("after an overwrite, pending = %d, want 1", status.Pending)
	}
	replicate(t, v)
	v.mu.RLock()
	fresh := v.manifest.ByID(second.ID).Replica
	v.mu.RUnlock()
	if fresh == nil || fresh.ArchiveID == old.ArchiveID || fresh.Hash != second.Hash {
		t.Fatalf("replica after overwrite = %+v, want a fresh one cut from the new bytes", fresh)
	}
	if got := sandObjectsIn(t, replicaRoots[0]); got != fresh.ChunkCount {
		t.Errorf("replica folder holds %d objects, want only the new replica's %d: the old one was not erased", got, fresh.ChunkCount)
	}
	if got, err := readFrom(t, v, second.ID, ReadReplica); err != nil || !bytes.Equal(got, bytes.Repeat([]byte("v2 "), 1500)) {
		t.Errorf("the fresh replica does not read back: %v", err)
	}

	// Deleting the file takes its replica with it.
	if _, err := v.Delete(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	if got := sandObjectsIn(t, replicaRoots[0]); got != 0 {
		t.Errorf("replica folder holds %d objects after the file was deleted", got)
	}
}

func TestDropReplicaErasesItAndTheWorkerMakesItAgain(t *testing.T) {
	v, _, replicaRoots := replicaVault(t, 3, 1, archive.Scheme{})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "x.bin", 3000)
	replicate(t, v)
	if _, err := v.DropReplica(context.Background(), entry.ID); err != nil {
		t.Fatal(err)
	}
	if got := sandObjectsIn(t, replicaRoots[0]); got != 0 {
		t.Errorf("%d replica objects left after DropReplica", got)
	}
	if _, err := readFrom(t, v, entry.ID, ReadReplica); err == nil {
		t.Error("a dropped replica still reads")
	}
	if made := replicate(t, v); made != 1 {
		t.Errorf("the worker made %d replicas for a file that lost its one, want 1", made)
	}
}

func TestDisconnectingAnAccountTrimsTheReplicaAndThePolicy(t *testing.T) {
	v, _, _ := replicaVault(t, 3, 3, archive.Scheme{Data: 2, Total: 3})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "x.bin", 3000)
	replicate(t, v)
	ids := providerIDs(t, v)

	if err := v.RemoveProvider(ids[2], true); err != nil {
		t.Fatal(err)
	}
	v.mu.RLock()
	r := v.manifest.ByID(entry.ID).Replica
	policy := v.manifest.Replication
	v.mu.RUnlock()
	if r == nil || len(r.Shards) != 2 || !r.Recoverable() {
		t.Errorf("replica after one account left = %+v, want two shards that still rebuild", r)
	}
	if policy != nil {
		t.Errorf("a 2-of-3 policy with two accounts left stands as %+v, want it cleared", policy)
	}
	if got, err := readFrom(t, v, entry.ID, ReadReplica); err != nil || len(got) != 3000 {
		t.Errorf("the trimmed replica does not read back: %v", err)
	}

	if err := v.RemoveProvider(ids[1], true); err != nil {
		t.Fatal(err)
	}
	v.mu.RLock()
	r = v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()
	if r != nil {
		t.Errorf("a replica with one shard of 2-of-3 is still recorded: %+v", r)
	}
}

func TestReplicationIsPacedByTheDailyBudget(t *testing.T) {
	v, roots := chunkedVault(t, 3, 1024)
	ids := providerIDs(t, v)
	if err := v.SetReplication(ReplicationSettings{
		Accounts:    []ReplicaAccount{{ID: ids[0], ReplicaFolder: roots[0] + "-r"}},
		BytesPerDay: 24 * 2000, // an hour's burst is 2000 bytes, less than one file
	}); err != nil {
		t.Fatal(err)
	}
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	uploadSized(t, v, "/", "a.bin", 3000)
	uploadSized(t, v, "/", "b.bin", 3000)

	now := time.Now()
	step, err := v.ReplicateStep(context.Background(), now)
	if err != nil || !step.Done {
		t.Fatalf("first step = %+v, %v; want a replica made", step, err)
	}
	step, err = v.ReplicateStep(context.Background(), now)
	if err != nil || step.Done || step.Wait <= 0 {
		t.Fatalf("second step in the same instant = %+v, %v; want a wait for the meter", step, err)
	}
	status, _ := v.ReplicationStatus(now)
	if status.Replicated != 1 || status.Pending != 1 || status.ETA <= 0 {
		t.Errorf("status = %+v, want one made, one pending with an ETA", status)
	}
	step, err = v.ReplicateStep(context.Background(), now.Add(2*time.Hour))
	if err != nil || !step.Done {
		t.Fatalf("step two hours on = %+v, %v; want the second replica made", step, err)
	}
}

func TestReplicaSurvivesAPasswordChangeAndIsRemadeOntoTheNewKey(t *testing.T) {
	v, _, _ := replicaVault(t, 3, 1, archive.Scheme{})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "x.bin", 3000)
	replicate(t, v)
	v.mu.RLock()
	old := v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()

	// Parts move at once; the replica stays on the retired key, readable,
	// until the worker re-makes it.
	if _, err := v.ChangePassword(context.Background(), testPassword, "a new password", true); err != nil {
		t.Fatal(err)
	}
	v.chunks.clear()
	if got, err := readFrom(t, v, entry.ID, ReadReplica); err != nil || len(got) != 3000 {
		t.Fatalf("the replica on the retired key does not read back: %v", err)
	}
	status, _ := v.ReplicationStatus(time.Now())
	if status.Stale != 1 {
		t.Errorf("after a password change the replica is not counted stale: %+v", status)
	}
	if made := replicate(t, v); made != 1 {
		t.Errorf("made %d replicas after the key changed, want 1", made)
	}
	v.mu.RLock()
	fresh := v.manifest.ByID(entry.ID).Replica
	retired := len(v.retired)
	v.mu.RUnlock()
	if fresh == nil || fresh.KeyID == old.KeyID {
		t.Errorf("replica after the change = %+v, want one on the new key", fresh)
	}
	if retired != 0 {
		t.Errorf("%d retired keys still held once nothing names them", retired)
	}
}
