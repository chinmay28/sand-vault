package provider

import "testing"

// Which backends live on one machine is a fact every other machine opening the
// vault leans on: it decides which accounts are pinged, and whose parts count
// as readable from here. So the list is pinned.
func TestLocalKindsAreTheOnesReachedThroughThisMachine(t *testing.T) {
	local := map[Kind]bool{
		KindLocal: true, KindICloud: true, KindProtonCLI: true,
		KindProton: true, KindMega: true, KindJottacloud: true,
		KindSyncCom: true, KindTresorit: true, KindIcedrive: true,
	}
	for _, spec := range Specs() {
		if got, want := IsLocal(spec.Kind), local[spec.Kind]; got != want {
			t.Errorf("IsLocal(%s) = %v, want %v", spec.Kind, got, want)
		}
	}
	if IsLocal(Kind("no-such-backend")) {
		t.Error("an unknown kind reads as local; it has to read as a cloud, which changes nothing")
	}
}

// A host survives the trip to the browser: Redacted strips secrets, and the
// machine an account lives on is not one.
func TestRedactedKeepsHost(t *testing.T) {
	cfg := Config{Kind: KindLocal, Host: "study-pc", Options: map[string]string{"path": "/mnt/x"}}
	if got := cfg.Redacted().Host; got != "study-pc" {
		t.Errorf("Redacted().Host = %q, want study-pc", got)
	}
}
