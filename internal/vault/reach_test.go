package vault

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// Which files a machine can read on its own, answered from the index.

func TestReachCountsPartsOnReachableAccountsAgainstTheFilesOwnCode(t *testing.T) {
	e := &Entry{DataShards: 2, TotalShards: 3, Shards: []Shard{
		{Part: 1, ProviderID: "cloud-1"}, {Part: 2, ProviderID: "cloud-2"}, {Part: 3, ProviderID: "disk"}}}

	if _, away := reachOf(e, map[string]bool{"cloud-1": true, "cloud-2": true}); away {
		t.Error("two of three parts on reachable clouds is a readable file")
	}
	reach, away := reachOf(e, map[string]bool{"cloud-1": true})
	if !away || reach.Here != 1 || reach.Needed != 2 {
		t.Errorf("one of two needed parts: away=%v reach=%+v", away, reach)
	}

	// A replica this machine can read makes the file readable, whatever the
	// parts say: a read falls back to it.
	e.Replica = &Replica{DataShards: 1, TotalShards: 1, Shards: []Shard{{Part: 1, ProviderID: "cloud-1"}}}
	if _, away := reachOf(e, map[string]bool{"cloud-1": true}); away {
		t.Error("a readable replica should put the file within reach")
	}
}

// The listing says which files this machine cannot read, and names the machine
// that can — from the index and the register, without a round trip.
func TestListingNamesTheMachineThatCanReadAnOutOfReachFile(t *testing.T) {
	v, _ := newTestVaultOn(t, "pi", 3)
	ctx := context.Background()

	whole, _, err := v.Upload(ctx, MainScope, "/", "whole.txt", []byte("readable here"), UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Two of the three accounts turn out to live on the pc: stamped so, as a
	// vault file copied from the pc would carry them.
	v.mu.Lock()
	for i := range v.providers[:2] {
		v.providers[i].Host = "pc"
	}
	away := []string{v.providers[0].ID, v.providers[1].ID}
	here := v.providers[2].ID
	v.manifest.Instances = map[string]*Instance{
		"pc": {Name: "pc", LastSeen: time.Now(), Accounts: []string{away[0], away[1], here}},
		// A machine that reaches only one of the parts cannot read it.
		"laptop": {Name: "laptop", LastSeen: time.Now().Add(-time.Hour), Accounts: []string{away[0]}},
	}
	v.mu.Unlock()

	listing, err := v.List(MainScope, "/")
	if err != nil {
		t.Fatal(err)
	}
	reach, ok := listing.Unreachable[whole.ID]
	if !ok {
		t.Fatalf("a file with two of its three parts on another machine is listed as readable: %+v", listing.Unreachable)
	}
	if reach.Here != 1 || reach.Needed != 2 {
		t.Errorf("reach %+v, want 1 of 2 here", reach)
	}
	if !reflect.DeepEqual(reach.Elsewhere, []string{"pc"}) {
		t.Errorf("elsewhere %v, want [pc]", reach.Elsewhere)
	}

	// Back on the pc, the same index lists nothing as out of reach.
	v.SetInstance("pc")
	listing, _ = v.List(MainScope, "/")
	if len(listing.Unreachable) != 0 {
		t.Errorf("on the pc: %+v, want nothing out of reach", listing.Unreachable)
	}
}

// A local account connected here carries this machine's name; one from before
// machines had names is claimed at unlock by the machine whose disk it is.
func TestLocalAccountsRememberTheMachineTheyLiveOn(t *testing.T) {
	v, roots := newTestVault(t, 1)
	cfgs, _ := v.Providers()
	if cfgs[0].Host != v.Instance() {
		t.Errorf("a folder connected here carries host %q, want %q", cfgs[0].Host, v.Instance())
	}

	// Forget the stamp, as a vault from an older build would have, and lock
	// and unlock: the folder is here, so this machine claims it.
	v.mu.Lock()
	v.providers[0].Host = ""
	if err := v.persistLocked(); err != nil {
		t.Fatal(err)
	}
	v.mu.Unlock()
	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatal(err)
	}
	cfgs, _ = v.Providers()
	if cfgs[0].Host != v.Instance() {
		t.Errorf("after unlock the folder carries host %q, want %q", cfgs[0].Host, v.Instance())
	}

	// On a machine where the folder does not exist, nobody claims it.
	v.mu.Lock()
	v.providers[0].Host = ""
	if err := v.persistLocked(); err != nil {
		t.Fatal(err)
	}
	v.mu.Unlock()
	// The parent goes, not the folder itself: a folder whose parent exists is
	// recreated by the backend, which is the unmounted-drive rule.
	parent := filepath.Dir(roots[0])
	if err := os.Rename(parent, parent+".away"); err != nil {
		t.Fatal(err)
	}
	v.Lock()
	if err := v.Unlock(testPassword); err != nil {
		t.Fatal(err)
	}
	cfgs, _ = v.Providers()
	if cfgs[0].Host != "" {
		t.Errorf("a folder that is not here was claimed by %q", cfgs[0].Host)
	}

	// An unclaimed folder is still tried, as it always was — a drive that is
	// merely unplugged is this machine's business — and reads as down.
	statuses, err := v.ProviderStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Online || statuses[0].Error == "" || statuses[0].Elsewhere != "" {
		t.Errorf("an unclaimed folder reports %+v, want offline with a reason", statuses[0])
	}

	// Plugged back in: the next ping that answers claims it.
	if err := os.Rename(parent+".away", parent); err != nil {
		t.Fatal(err)
	}
	statuses, err = v.ProviderStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !statuses[0].Online {
		t.Fatalf("the folder is back and still reports %+v", statuses[0])
	}
	cfgs, _ = v.Providers()
	if cfgs[0].Host != v.Instance() {
		t.Errorf("a folder that answered from here carries host %q, want %q", cfgs[0].Host, v.Instance())
	}
}

// An account another machine's register claims is reported as living there.
func TestAccountOnAnotherMachineIsReportedNotPinged(t *testing.T) {
	v, _ := newTestVaultOn(t, "pi", 2)
	v.mu.Lock()
	v.providers[0].Host = "pc"
	v.mu.Unlock()

	statuses, err := v.ProviderStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Elsewhere != "pc" || statuses[0].Online || statuses[0].Error != "" {
		t.Errorf("status %+v, want elsewhere=pc and nothing else said", statuses[0])
	}
	if !statuses[1].Online {
		t.Errorf("the folder that is here answered %+v", statuses[1])
	}
	health, _ := v.CheckClouds(context.Background())
	if health.Unhealthy != 0 || health.Elsewhere != 1 || health.Healthy != 1 {
		t.Errorf("health counts healthy=%d unhealthy=%d elsewhere=%d, want 1/0/1",
			health.Healthy, health.Unhealthy, health.Elsewhere)
	}

	insts, err := v.Instances()
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 1 || !insts[0].Here || insts[0].Name != "pi" || len(insts[0].Accounts) != 1 {
		t.Errorf("instances %+v, want this machine alone, reaching one account", insts)
	}
}

// The register entry this machine writes names the accounts it can reach.
func TestIndexWriteRegistersThisMachineAndItsAccounts(t *testing.T) {
	v, _ := newTestVaultOn(t, "study", 2)
	v.mu.Lock()
	v.providers[1].Host = "elsewhere"
	err := v.persistLocked()
	own := v.manifest.Instances["study"]
	wantID := v.providers[0].ID
	v.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if own == nil || !reflect.DeepEqual(own.Accounts, []string{wantID}) || own.LastSeen.IsZero() {
		t.Errorf("register entry %+v, want this machine reaching %s", own, wantID)
	}
}

func TestInstanceNameHonoursTheEnvironment(t *testing.T) {
	t.Setenv("SAND_INSTANCE", "  bench  ")
	if got := InstanceName(); got != "bench" {
		t.Errorf("InstanceName() = %q, want bench", got)
	}
	t.Setenv("SAND_INSTANCE", "")
	if got := InstanceName(); got == "" {
		t.Error("InstanceName() is empty without the variable")
	}
}

// A stat that hangs is not waited on.
func TestDirExistsWithinGivesUpOnASlowFolder(t *testing.T) {
	dir := t.TempDir()
	if !dirExistsWithin(dir, time.Second) {
		t.Error("an existing folder was not seen")
	}
	if dirExistsWithin(filepath.Join(dir, "missing"), time.Second) {
		t.Error("a missing folder was seen")
	}
	if dirExistsWithin("", time.Second) {
		t.Error("an empty path was seen")
	}
	_ = provider.KindLocal
}
