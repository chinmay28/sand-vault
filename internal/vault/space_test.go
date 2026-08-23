package vault

import (
	"strings"
	"testing"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// Three things might know how much room an account has left, and which one
// answered is part of the answer.
func TestSpaceForPrefersTheBindingLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		usage  provider.Usage
		quota  int64
		stored int64
		want   Space
	}{
		{
			name: "nothing knows anything",
			want: Space{},
		},
		{
			name:  "the backend reports",
			usage: provider.Usage{Used: 40, Total: 100, Free: 55},
			want:  Space{Free: 55, Used: 40, Total: 100, Source: SpaceReported},
		},
		{
			name:  "a capacity somebody typed, against a count",
			usage: provider.Usage{Used: 40, Total: 100, Measured: true, Declared: true},
			want:  Space{Free: 60, Used: 40, Total: 100, Source: SpaceDeclared},
		},
		{
			name:   "only a quota, which is what a silent backend gets",
			quota:  100,
			stored: 30,
			want:   Space{Free: 70, Used: 30, Total: 100, Source: SpaceQuota},
		},
		{
			// A drive with room to spare is not room SAND may use.
			name:   "the quota binds tighter than the drive",
			usage:  provider.Usage{Used: 40, Total: 1000, Free: 960},
			quota:  100,
			stored: 30,
			want:   Space{Free: 70, Used: 30, Total: 100, Source: SpaceQuota},
		},
		{
			// And the other way round: a generous quota does not conjure room
			// onto a drive that has none.
			name:   "the drive binds tighter than the quota",
			usage:  provider.Usage{Used: 990, Total: 1000, Free: 10},
			quota:  100000,
			stored: 30,
			want:   Space{Free: 10, Used: 990, Total: 1000, Source: SpaceReported},
		},
		{
			// Over is the state a backend's own figures cannot get into: they
			// reach zero, they do not go past it.
			name:   "past the quota, on a drive that is fine",
			usage:  provider.Usage{Used: 400, Total: 1000, Free: 600},
			quota:  100,
			stored: 130,
			want:   Space{Free: 0, Used: 130, Total: 100, Source: SpaceQuota, Over: 30},
		},
	} {
		got := spaceFor(tc.usage, tc.quota, tc.stored)
		if got != tc.want {
			t.Errorf("%s: spaceFor(%+v, %d, %d) = %+v, want %+v",
				tc.name, tc.usage, tc.quota, tc.stored, got, tc.want)
		}
		if got.Known() != (tc.want.Source != "") {
			t.Errorf("%s: known = %v", tc.name, got.Known())
		}
	}
}

// The room left travels with the account, so the picker and the CLI read the
// same figure the panel does.
func TestProviderStatusesReportTheRoomLeft(t *testing.T) {
	v, _ := newTestVault(t, 1)

	statuses, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	// A folder on a disk measures the disk under it, which is a real figure and
	// the account's own.
	if statuses[0].Space.Source != SpaceReported {
		t.Fatalf("a local folder reports space from %q: %+v", statuses[0].Space.Source, statuses[0].Space)
	}
	id := statuses[0].ID

	// A quota below what the disk reports takes over, because it is the line
	// that binds.
	quota := int64(4 << 10)
	if _, err := v.UpdateProvider(t.Context(), id, ProviderEdit{Quota: &quota}); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	after, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	space := after[0].Space
	if space.Source != SpaceQuota {
		t.Errorf("room left comes from %q, want the quota: %+v", space.Source, space)
	}
	if space.Total != quota {
		t.Errorf("room left is measured against %d, want the %d quota", space.Total, quota)
	}
	if space.Free != quota-after[0].Stored {
		t.Errorf("free = %d, want the %d quota less the %d stored",
			space.Free, quota, after[0].Stored)
	}
}

// The quota is stored with the account, like its name and its colour: nothing
// about it reaches the backend, and it survives a lock.
func TestUpdateProviderStoresTheQuota(t *testing.T) {
	v, _ := newTestVault(t, 1)

	statuses, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	id := statuses[0].ID

	quota := int64(200 << 30)
	if _, err := v.UpdateProvider(t.Context(), id, ProviderEdit{Quota: &quota}); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	fresh := reopen(t, v)
	after, err := fresh.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses after reopening: %v", err)
	}
	if after[0].Quota != quota {
		t.Errorf("quota after reopening = %d, want %d", after[0].Quota, quota)
	}
	// A capacity and a quota are different questions, and setting one does not
	// answer the other.
	if after[0].Capacity != 0 {
		t.Errorf("setting a quota declared a capacity of %d", after[0].Capacity)
	}

	cleared := int64(0)
	if _, err := fresh.UpdateProvider(t.Context(), id, ProviderEdit{Quota: &cleared}); err != nil {
		t.Fatalf("clearing the quota: %v", err)
	}
	again, err := fresh.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	if again[0].Quota != 0 {
		t.Errorf("quota = %d after being cleared", again[0].Quota)
	}

	negative := int64(-1)
	if _, err := fresh.UpdateProvider(t.Context(), id, ProviderEdit{Quota: &negative}); err == nil {
		t.Error("an account was given a negative quota")
	}
}

// Crossing the line is warned about; being over it is not warned about again,
// because a batch of four hundred files would say it four hundred times.
func TestUploadWarnsOnceOnCrossingAQuota(t *testing.T) {
	v, _ := newTestVault(t, 3)

	statuses, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	id := statuses[0].ID

	// Small enough that the first file goes past it, since a 2-of-3 cut leaves
	// about half the file on each account.
	quota := int64(64)
	if _, err := v.UpdateProvider(t.Context(), id, ProviderEdit{Quota: &quota}); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}

	body := []byte(strings.Repeat("a receipt ", 200))
	first, warnings, err := v.Upload(t.Context(), MainScope, "/", "first.txt", body, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	crossed := matching(warnings, "past the")
	if len(crossed) != 1 {
		t.Fatalf("the upload that crossed the quota warned %d times: %v", len(crossed), warnings)
	}
	if !strings.Contains(crossed[0], statuses[0].Name) {
		t.Errorf("the warning does not name the account: %q", crossed[0])
	}

	// The second file lands on an account that is already over, which is a
	// state the card and the picker show rather than something to say again.
	second, more, err := v.Upload(t.Context(), MainScope, "/", "second.txt", body, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if again := matching(more, "past the"); len(again) != 0 {
		t.Errorf("an account already over its quota warned again: %v", again)
	}

	// And it is a warning, not a wall: both files are stored and readable.
	for _, entry := range []*Entry{first, second} {
		if got, _, err := v.Fetch(t.Context(), entry.ID); err != nil {
			t.Errorf("%s did not store past the quota: %v", entry.Name, err)
		} else if len(got) != len(body) {
			t.Errorf("%s came back %d bytes, want %d", entry.Name, len(got), len(body))
		}
	}

	// The account now says it is over, which is where the state lives.
	over, err := v.ProviderStatuses(t.Context())
	if err != nil {
		t.Fatalf("ProviderStatuses: %v", err)
	}
	for _, st := range over {
		if st.ID != id {
			continue
		}
		if st.Space.Over <= 0 {
			t.Errorf("the account over its quota reports %+v", st.Space)
		}
		if st.Space.Free != 0 {
			t.Errorf("an account over its quota has %d free", st.Space.Free)
		}
	}
}

// An account with no quota is not warned about however much lands on it.
func TestUploadSaysNothingWithoutAQuota(t *testing.T) {
	v, _ := newTestVault(t, 3)

	_, warnings, err := v.Upload(t.Context(), MainScope, "/", "quiet.txt",
		[]byte(strings.Repeat("nothing to say ", 200)), UploadOptions{})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := matching(warnings, "quota"); len(got) != 0 {
		t.Errorf("an account with no quota was warned about: %v", got)
	}
}

func matching(warnings []string, needle string) []string {
	var out []string
	for _, w := range warnings {
		if strings.Contains(w, needle) {
			out = append(out, w)
		}
	}
	return out
}
