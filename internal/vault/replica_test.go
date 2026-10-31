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

func TestReplicaSurvivesADeferredPasswordChangeAndIsRemadeOntoTheNewKey(t *testing.T) {
	v, _, _ := replicaVault(t, 3, 1, archive.Scheme{})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "x.bin", 3000)
	replicate(t, v)
	v.mu.RLock()
	old := v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()

	// The files are left for later, and the replica with them: it stays on
	// the retired key, readable, until the worker re-makes it.
	if _, err := v.ChangePassword(context.Background(), testPassword, "a new password", false); err != nil {
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
	rekey, _ := v.RekeyStatus()
	if rekey.StaleReplicas != 1 {
		t.Errorf("the rekey status counts %d stale replicas, want 1: %+v", rekey.StaleReplicas, rekey)
	}
	if made := replicate(t, v); made != 1 {
		t.Errorf("made %d replicas after the key changed, want 1", made)
	}
	v.mu.RLock()
	fresh := v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()
	if fresh == nil || fresh.KeyID == old.KeyID {
		t.Errorf("replica after the change = %+v, want one on the new key", fresh)
	}
	if _, err := v.MigrateFiles(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	v.mu.RLock()
	retired := len(v.retired)
	v.mu.RUnlock()
	if retired != 0 {
		t.Errorf("%d retired keys still held once nothing names them", retired)
	}
	if rekey, _ := v.RekeyStatus(); rekey.StaleReplicas != 0 {
		t.Errorf("rekey status still counts %d stale replicas", rekey.StaleReplicas)
	}
}

func TestAFinishedPasswordChangeErasesTheReplicaLeftOnTheRetiredKey(t *testing.T) {
	v, _, replicaRoots := replicaVault(t, 3, 1, archive.Scheme{})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "x.bin", 3000)
	replicate(t, v)

	// Finishing now is a request to be rid of the previous key. The replica
	// is a whole-file copy under it, so it goes with the key rather than
	// keeping it alive until the worker happens to come round.
	if _, err := v.ChangePassword(context.Background(), testPassword, "a new password", true); err != nil {
		t.Fatal(err)
	}
	v.mu.RLock()
	r := v.manifest.ByID(entry.ID).Replica
	retired := len(v.retired)
	v.mu.RUnlock()
	if r != nil {
		t.Errorf("replica after a finished change = %+v, want it erased", r)
	}
	if retired != 0 {
		t.Errorf("%d retired keys still held after a finished password change", retired)
	}
	if got := sandObjectsIn(t, replicaRoots[0]); got != 0 {
		t.Errorf("%d replica objects left in the folder, sealed under the retired key", got)
	}
	// And the worker makes it again, on the new key.
	if made := replicate(t, v); made != 1 {
		t.Errorf("made %d replicas after the change, want 1", made)
	}
}

// pacedPasswordChange rotates the key and moves every file through the paced
// step rather than the finish-now pass, which is the path that leaves a
// replica on the retired key with its file gone from it.
func pacedPasswordChange(t *testing.T, v *Vault, old, next string) {
	t.Helper()
	ctx := context.Background()
	if _, err := v.ChangePasswordWithPlan(ctx, old, next, &RekeySettings{BytesPerDay: 1 << 40}); err != nil {
		t.Fatalf("ChangePasswordWithPlan: %v", err)
	}
	for i := 0; i < 50; i++ {
		step, err := v.RekeyStep(ctx, time.Now())
		if err != nil {
			t.Fatalf("RekeyStep: %v", err)
		}
		if step.Idle {
			return
		}
	}
	t.Fatal("the paced re-encryption never went idle")
}

func TestClearingOrPausingThePolicyErasesReplicasOnARetiredKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings func(ids []string, roots []string) ReplicationSettings
	}{
		{"cleared", func([]string, []string) ReplicationSettings { return ReplicationSettings{} }},
		{"paused", func(ids, roots []string) ReplicationSettings {
			return ReplicationSettings{Accounts: []ReplicaAccount{{ID: ids[0], ReplicaFolder: roots[0]}}, Paused: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _, replicaRoots := replicaVault(t, 3, 1, archive.Scheme{})
			if err := v.SetReplicate("/", on(true)); err != nil {
				t.Fatal(err)
			}
			kept := uploadSized(t, v, "/", "kept.bin", 2000)
			stale := uploadSized(t, v, "/", "stale.bin", 3000)
			replicate(t, v)
			// The paced plan moves the files; the worker re-makes one replica
			// onto the new key and never gets to the other — excluded here,
			// metered or simply not yet reached in life — which stays on the
			// retired key, and keeps it.
			pacedPasswordChange(t, v, testPassword, "a new password")
			if err := v.SetReplicate("/stale.bin", on(false)); err != nil {
				t.Fatal(err)
			}
			if made := replicate(t, v); made != 1 {
				t.Fatalf("made %d replicas after the change, want kept's alone", made)
			}
			v.mu.RLock()
			retiredBefore := len(v.retired)
			v.mu.RUnlock()
			if retiredBefore != 1 {
				t.Fatalf("%d retired keys held before the policy change, want the one stale's replica names", retiredBefore)
			}

			if err := v.SetReplication(tc.settings(providerIDs(t, v), replicaRoots)); err != nil {
				t.Fatalf("SetReplication: %v", err)
			}
			v.mu.RLock()
			keptReplica := v.manifest.ByID(kept.ID).Replica
			staleReplica := v.manifest.ByID(stale.ID).Replica
			retired := len(v.retired)
			v.mu.RUnlock()
			if keptReplica == nil {
				t.Error("the replica on the current key was dropped")
			}
			if staleReplica != nil {
				t.Errorf("the replica on the retired key is still recorded: %+v", staleReplica)
			}
			if retired != 0 {
				t.Errorf("%d retired keys still held with the policy %s", retired, tc.name)
			}
			if got := sandObjectsIn(t, replicaRoots[0]); keptReplica != nil && got != keptReplica.ChunkCount {
				t.Errorf("the replica folder holds %d objects, want only the current replica's %d", got, keptReplica.ChunkCount)
			}
		})
	}
}

func TestAPasswordChangeErasesReplicasStillOnTheKeyBeforeLast(t *testing.T) {
	v, _, replicaRoots := replicaVault(t, 3, 1, archive.Scheme{})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "x.bin", 3000)
	replicate(t, v)
	// The paced plan moves the file and nothing re-makes the replica, so the
	// retired key stays in the vault for the replica alone.
	pacedPasswordChange(t, v, testPassword, "a new password")
	v.mu.RLock()
	retired := len(v.retired)
	v.mu.RUnlock()
	if retired != 1 {
		t.Fatalf("%d retired keys held before the next change, want 1", retired)
	}

	// The next change is not refused for it, and does not carry it as a
	// third generation: the replica is erased and the key goes.
	report, err := v.ChangePassword(context.Background(), "a new password", "a third password", true)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if len(report.Warnings) > 0 {
		t.Errorf("the change warned: %v", report.Warnings)
	}
	v.mu.RLock()
	r := v.manifest.ByID(entry.ID).Replica
	retired = len(v.retired)
	v.mu.RUnlock()
	if r != nil {
		t.Errorf("replica after the second change = %+v, want it erased", r)
	}
	if retired != 0 {
		t.Errorf("%d retired keys held after the second change, want none", retired)
	}
	if got := sandObjectsIn(t, replicaRoots[0]); got != 0 {
		t.Errorf("%d replica objects left on the key before last", got)
	}
	if made := replicate(t, v); made != 1 {
		t.Errorf("made %d replicas afterwards, want 1", made)
	}
}

func TestReplicasCountTowardsAnAccountsLoad(t *testing.T) {
	v, _, _ := replicaVault(t, 3, 1, archive.Scheme{})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "x.bin", 3000)
	ids := providerIDs(t, v)

	before, err := v.ProviderStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	replicate(t, v)
	after, err := v.ProviderStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stored := func(statuses []ProviderStatus, id string) int64 {
		for _, s := range statuses {
			if s.ID == id {
				return s.Stored
			}
		}
		t.Fatalf("no status for %s", id)
		return 0
	}
	v.mu.RLock()
	r := v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()
	var replicaBytes int64
	for _, s := range r.Shards {
		replicaBytes += s.Size
	}
	if replicaBytes == 0 {
		t.Fatal("the replica weighs nothing")
	}
	if got, want := stored(after, ids[0]), stored(before, ids[0])+replicaBytes; got != want {
		t.Errorf("cloud-a stores %d after replicating, want %d: the replica is not counted", got, want)
	}
	for _, id := range ids[1:] {
		if stored(after, id) != stored(before, id) {
			t.Errorf("%s's load changed though no replica went to it", id)
		}
	}
}

func TestTheOrphanScanListsTheReplicaFoldersAndSweepsThem(t *testing.T) {
	ctx := context.Background()
	v, roots, replicaRoots := replicaVault(t, 3, 3, archive.Scheme{Data: 2, Total: 3})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "x.bin", 3000)
	replicate(t, v)
	ids := providerIDs(t, v)

	// A replica the index names is not an orphan, and the replica rows are
	// rows of their own.
	scan := orphanScan(t, v)
	if scan.Found {
		t.Fatalf("a current replica is reported as abandoned: %+v", scan.Items)
	}
	if len(scan.Accounts) != 6 {
		t.Fatalf("%d account rows, want one per root: three parts folders and three replica folders", len(scan.Accounts))
	}
	rows := map[string]OrphanAccount{}
	for _, a := range scan.Accounts {
		rows[a.ProviderID] = a
	}
	for _, id := range ids {
		parts, replicas := rows[id], rows[replicaID(id)]
		if parts.Replicas || parts.Objects == 0 {
			t.Errorf("parts row for %s = %+v", id, parts)
		}
		if !replicas.Replicas || !strings.HasSuffix(replicas.Name, "(replicas)") || replicas.Objects == 0 {
			t.Errorf("replica row for %s = %+v, want it flagged, labelled, and counting the replica's objects", id, replicas)
		}
	}

	// Forgotten without being erased — what a recovery, a disconnect and a
	// dropped key all do — and the objects are abandoned in the replica
	// folders, and nowhere else.
	v.mu.Lock()
	doomed := v.manifest.ByID(entry.ID).Replica
	v.manifest.ByID(entry.ID).Replica = nil
	if err := v.persistLocked(); err != nil {
		v.mu.Unlock()
		t.Fatal(err)
	}
	v.mu.Unlock()

	scan = orphanScan(t, v)
	if !scan.Found || scan.Archives != 3 || len(scan.Blocked) > 0 {
		t.Fatalf("scan after forgetting the replica = found %v, %d archives, blocked %v; want the replica's three", scan.Found, scan.Archives, scan.Blocked)
	}
	for _, item := range scan.Items {
		if item.ArchiveID != doomed.ArchiveID || !strings.HasSuffix(item.ProviderID, replicaSuffix) || !item.Deletable {
			t.Errorf("orphan row %+v, want the replica's archive in a replica folder, deletable", item)
		}
	}
	for _, a := range scan.Accounts {
		if !a.Replicas && a.Orphans > 0 {
			t.Errorf("the parts row for %s reports %d orphans: a replica object was mistaken for a part", a.Name, a.Orphans)
		}
	}

	partsBefore := objectsUnder(t, roots)
	report, err := v.SweepOrphans(ctx, nil, false, nil)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if report.Archives != 3 || len(report.Warnings) > 0 || len(report.Skipped) > 0 {
		t.Errorf("sweep = %+v, want the replica's three archives erased cleanly", report)
	}
	for _, root := range replicaRoots {
		if got := sandObjectsIn(t, root); got != 0 {
			t.Errorf("%s still holds %d objects after the sweep", root, got)
		}
	}
	if got := objectsUnder(t, roots); got != partsBefore {
		t.Errorf("the sweep touched the parts folders: %d objects, %d before", got, partsBefore)
	}
	if got, err := readFrom(t, v, entry.ID, ReadParts); err != nil || len(got) != 3000 {
		t.Errorf("the file does not read back after the sweep: %v", err)
	}
	if scan := orphanScan(t, v); scan.Found {
		t.Errorf("still %d abandoned archive(s) after the sweep", scan.Archives)
	}
}

func TestUpdateProviderKeepsThePartsFolderOutOfTheReplicaFolder(t *testing.T) {
	v, _, replicaRoots := replicaVault(t, 3, 1, archive.Scheme{})
	id := providerIDs(t, v)[0]

	for _, tc := range []struct{ name, path, want string }{
		{"onto the replica folder", replicaRoots[0], "is the parts directory"},
		{"inside the replica folder", filepath.Join(replicaRoots[0], "parts"), "contains the parts directory"},
		{"around the replica folder", filepath.Dir(replicaRoots[0]), "is inside the parts directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.UpdateProvider(context.Background(), id, ProviderEdit{Options: map[string]string{"path": tc.path}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("moving the parts %s: err = %v, want one containing %q", tc.name, err, tc.want)
			}
		})
	}
	// Nothing was applied by any refusal.
	cfgs, _ := v.Providers()
	if cfgs[0].Options["path"] == replicaRoots[0] {
		t.Error("a refused edit was stored")
	}
	// And a move to a folder of its own is still allowed.
	elsewhere := filepath.Join(t.TempDir(), "moved")
	if _, err := v.UpdateProvider(context.Background(), id, ProviderEdit{Options: map[string]string{"path": elsewhere}}); err != nil {
		t.Fatalf("moving the parts beside the replicas: %v", err)
	}
}

func TestDropReplicaKeepsTheObjectsWhenTheIndexCannotBeWritten(t *testing.T) {
	v, _, replicaRoots := replicaVault(t, 3, 1, archive.Scheme{})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "x.bin", 3000)
	replicate(t, v)
	v.mu.RLock()
	r := v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()

	// The vault file cannot be replaced: a directory with something in it
	// sits where it was, which the atomic rename refuses.
	v.AwaitBackupSync()
	if err := os.Remove(v.path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(v.path, "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := v.DropReplica(context.Background(), entry.ID); err == nil {
		t.Fatal("DropReplica succeeded with the index unwritable")
	}
	// The index still names the replica, so its objects must still be there.
	v.mu.RLock()
	still := v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()
	if still != r {
		t.Errorf("replica after the failed drop = %+v, want the one before it", still)
	}
	if got := sandObjectsIn(t, replicaRoots[0]); got != r.ChunkCount {
		t.Errorf("the replica folder holds %d objects after the failed drop, want all %d: the index names a replica that is gone", got, r.ChunkCount)
	}
}

func TestAFileWithNoRecordedHashIsReplicatedOnceAndThenCurrent(t *testing.T) {
	v, _, _ := replicaVault(t, 3, 1, archive.Scheme{})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "old.bin", 3000)

	// A file stored before hashes were recorded, or one that has been through
	// a conversion or a re-encryption since, carries none in the index.
	v.mu.Lock()
	v.manifest.ByID(entry.ID).Hash = ""
	if err := v.persistLocked(); err != nil {
		v.mu.Unlock()
		t.Fatal(err)
	}
	v.mu.Unlock()

	if made := replicate(t, v); made != 1 {
		t.Fatalf("made %d replicas, want 1", made)
	}
	status, _ := v.ReplicationStatus(time.Now())
	if status.Replicated != 1 || status.Pending != 0 {
		t.Errorf("after replicating a hashless file, status = %+v; want it counted current", status)
	}
	// The worker must not make it again: before the fix every step remade
	// it, because the replica's hash could never equal a hash of "".
	if made := replicate(t, v); made != 0 {
		t.Errorf("a second pass made %d replicas of a file that did not change", made)
	}

	v.mu.RLock()
	e := v.manifest.ByID(entry.ID)
	r := e.Replica
	v.mu.RUnlock()
	if r == nil || r.Hash == "" {
		t.Fatalf("replica = %+v, want one with the hash it was cut from", r)
	}
	// The hash verified on the way through is the file's own, and the entry
	// now carries it like a file stored today does.
	if e.Hash != entry.Hash || r.Hash != e.Hash {
		t.Errorf("entry hash %q, replica hash %q, want both the file's %q", e.Hash, r.Hash, entry.Hash)
	}
}

func TestADegradedReplicaIsRefusedAndThePreviousOneKept(t *testing.T) {
	ctx := context.Background()
	v, _, replicaRoots := replicaVault(t, 3, 3, archive.Scheme{Data: 2, Total: 3})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry := uploadSized(t, v, "/", "x.bin", 3000)
	if made := replicate(t, v); made != 1 {
		t.Fatalf("made %d replicas, want 1", made)
	}
	v.mu.RLock()
	old := v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()

	// cloud-c's replica folder stops taking writes: a file sits where the
	// directory was, so every Put there fails while the account still
	// answers. A password change left for later puts the replica on the
	// retired key, which makes it one to make again.
	if err := os.RemoveAll(replicaRoots[2]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replicaRoots[2], []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ChangePassword(ctx, testPassword, "a new password", false); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	step, err := v.ReplicateStep(ctx, now)
	if err != nil {
		t.Fatalf("ReplicateStep: %v", err)
	}
	if step.Done || step.Warning == "" {
		t.Fatalf("step with one replication cloud down = %+v; want it refused with a warning, not a 2-of-3 replica on two clouds", step)
	}
	v.mu.RLock()
	r := v.manifest.ByID(entry.ID).Replica
	v.mu.RUnlock()
	if r == nil || r.ArchiveID != old.ArchiveID {
		t.Fatalf("replica after the refused attempt = %+v, want the previous one (%s) kept", r, old.ArchiveID)
	}
	for i, root := range replicaRoots[:2] {
		if got := sandObjectsIn(t, root); got != old.ChunkCount {
			t.Errorf("replica folder %d holds %d objects, want the previous replica's %d and nothing from the refused attempt", i, got, old.ChunkCount)
		}
	}
	v.chunks.clear()
	if got, err := readFrom(t, v, entry.ID, ReadReplica); err != nil || len(got) != 3000 {
		t.Errorf("the kept replica does not read back: %v", err)
	}

	// The file is left alone for a while rather than tried again on the next
	// tick, which is what bounds the bandwidth while the cloud is down.
	status, _ := v.ReplicationStatus(now)
	if status.LeftAlone != 1 || status.Stale != 1 {
		t.Errorf("status = %+v, want the file stale and left alone", status)
	}
	again, err := v.ReplicateStep(ctx, now)
	if err != nil || again.Done || again.Warning != "" || again.Wait <= 0 {
		t.Errorf("the next step = %+v, %v; want a wait, not another attempt", again, err)
	}
}

// The health check asks after the replica as it asks after the parts: each
// shard looked for in its account's replica folder, and each placement said
// to be readable or not on its own.
func TestHealthReportsTheReplicaBesideTheParts(t *testing.T) {
	v, roots, replicaRoots := replicaVault(t, 3, 3, archive.Scheme{Data: 2, Total: 3})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("watched "), 700)
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "doc.bin", payload, UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Before the worker has been round, there is no replica to report.
	health, err := v.Health(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Replica != nil {
		t.Fatalf("a file not yet replicated reports a replica: %+v", health.Replica)
	}

	replicate(t, v)
	health, err = v.Health(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	r := health.Replica
	if r == nil {
		t.Fatal("a replicated file reports no replica")
	}
	if !r.Recoverable || r.Scheme != "2-of-3" || r.Needed != 2 || r.Spare != 1 || r.Whole || r.PreviousKey {
		t.Errorf("replica health = %+v, want a readable 2-of-3 with one spare", r.PlacementHealth)
	}
	if len(r.Shards) != 3 {
		t.Fatalf("replica shards = %d, want 3", len(r.Shards))
	}
	for _, s := range r.Shards {
		if !s.Present || s.Size <= 0 {
			t.Errorf("replica shard %d on %s: present=%v size=%d error=%q", s.Part, s.ProviderName, s.Present, s.Size, s.Error)
		}
		if strings.Contains(s.ProviderID, replicaSuffix) {
			t.Errorf("replica shard names the suffixed account id %q", s.ProviderID)
		}
	}
	if r.ChunkCount <= 1 || r.ChunksSampled == 0 {
		t.Errorf("a chunked replica reports chunk_count=%d chunks_sampled=%d", r.ChunkCount, r.ChunksSampled)
	}
	if r.CreatedAt.IsZero() {
		t.Error("the replica's creation time is missing")
	}

	// A replica folder emptied is a replica shard gone, and only that: the
	// parts on the same account are in their own folder and still answer.
	emptyCloud(t, replicaRoots[0])
	health, err = v.Health(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Health after emptying a replica folder: %v", err)
	}
	if !health.Recoverable || health.Spare != 1 {
		t.Errorf("the parts changed when a replica folder was emptied: %+v", health.PlacementHealth)
	}
	for _, s := range health.Shards {
		if !s.Present {
			t.Errorf("part %d on %s reported missing: %s", s.Part, s.ProviderName, s.Error)
		}
	}
	gone := 0
	for _, s := range health.Replica.Shards {
		if !s.Present {
			gone++
			if s.ProviderID != entry.Shards[0].ProviderID && s.Error == "" {
				t.Errorf("a missing replica shard carries no reason: %+v", s)
			}
		}
	}
	if gone != 1 || !health.Replica.Recoverable || health.Replica.Spare != 0 {
		t.Errorf("after emptying one replica folder: %d gone, recoverable=%v spare=%d; want 1, true, 0",
			gone, health.Replica.Recoverable, health.Replica.Spare)
	}

	// And the other way round: the parts folders go, the replica stands.
	emptyCloud(t, roots[1])
	emptyCloud(t, roots[2])
	health, err = v.Health(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Health after emptying the parts: %v", err)
	}
	if health.Recoverable {
		t.Error("the parts read as recoverable with two of three folders emptied")
	}
	if !health.Replica.Recoverable {
		t.Error("the replica stopped reading when the parts folders were emptied")
	}
}

// A single-account replica is the whole sealed file, and says so.
func TestHealthReportsAWholeCopyReplica(t *testing.T) {
	v, _, _ := replicaVault(t, 2, 1, archive.WholeCopy)
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "doc.bin", bytes.Repeat([]byte("whole "), 400), UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replicate(t, v)

	health, err := v.Health(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	r := health.Replica
	if r == nil || !r.Whole || !r.Recoverable || len(r.Shards) != 1 || r.Scheme != "1-of-1" || r.Spare != 0 {
		t.Fatalf("whole-copy replica health = %+v", r)
	}
}

// An account that still holds the parts but has lost its replica folder is
// named for what it is, not reported as disconnected.
func TestHealthNamesAnAccountWithoutAReplicaFolder(t *testing.T) {
	v, _, _ := replicaVault(t, 3, 3, archive.Scheme{Data: 2, Total: 3})
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	entry, _, err := v.Upload(context.Background(), MainScope, "/", "doc.bin", bytes.Repeat([]byte("held "), 500), UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replicate(t, v)

	ids := providerIDs(t, v)
	v.mu.Lock()
	for i := range v.providers {
		if v.providers[i].ID == ids[0] {
			v.providers[i].ReplicaFolder = ""
		}
	}
	v.mu.Unlock()
	v.forgetProvider(ids[0])

	health, err := v.Health(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	var found bool
	for _, s := range health.Replica.Shards {
		if s.ProviderID != ids[0] {
			continue
		}
		found = true
		if s.Present || !strings.Contains(s.Error, "no replica folder") {
			t.Errorf("replica shard on the account without a folder: present=%v error=%q", s.Present, s.Error)
		}
	}
	if !found {
		t.Fatal("the replica has no shard on the first account")
	}
	for _, s := range health.Shards {
		if !s.Present {
			t.Errorf("part %d on %s reported missing: %s", s.Part, s.ProviderName, s.Error)
		}
	}
}
