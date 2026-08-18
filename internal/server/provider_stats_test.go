package server

import (
	"net/http"
	"strings"
	"testing"
)

// The panel behind an account's usage line. It answers three questions the
// one-liner cannot: how much of the drive SAND is actually responsible for,
// what the parts belong to, and what would be stranded if the account went
// away.
func TestProviderStatsBreaksAnAccountDown(t *testing.T) {
	c := newTestClient(t)
	c.setup("break the account down", 3)
	ids := c.providerIDs()

	// Two kinds and two folders, so the breakdown has something to separate.
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/papers"})
	c.upload("holiday.jpg", "/", []byte(strings.Repeat("a photograph ", 400)))
	c.upload("notes.txt", "/papers", []byte(strings.Repeat("a document ", 100)))

	w, body := c.json(http.MethodGet, "/api/providers/stats/"+ids[0], nil)
	if w.Code != http.StatusOK {
		t.Fatalf("provider stats: %d %s", w.Code, w.Body.String())
	}
	stats := body["stats"].(map[string]any)

	if stats["id"] != ids[0] {
		t.Fatalf("stats are for %v, asked about %s", stats["id"], ids[0])
	}
	if online, _ := stats["online"].(bool); !online {
		t.Errorf("account reported offline: %v", stats["error"])
	}

	// A local folder knows the drive it is on, which is the figure that makes
	// "33.9 GB of parts" mean anything.
	usage := stats["usage"].(map[string]any)
	if usage["total"].(float64) <= 0 {
		t.Errorf("no drive capacity reported: %v", usage)
	}
	if usage["free"].(float64) <= 0 {
		t.Errorf("no free space reported: %v", usage)
	}

	// Both files were split across all three accounts, so this one holds a
	// part of each — and the vault total is bigger than this account's share.
	if stats["files"].(float64) != 2 {
		t.Errorf("files = %v, want 2", stats["files"])
	}
	if stored, total := stats["stored"].(float64), stats["vault_stored"].(float64); stored <= 0 || total <= stored {
		t.Errorf("share of the vault: %v of %v", stored, total)
	}
	// Nothing is stranded: two other accounts hold the rest of every part.
	if stats["sole"].(float64) != 0 {
		t.Errorf("sole = %v, want 0 with three accounts holding every file", stats["sole"])
	}

	kinds := labelsOf(t, stats["kinds"])
	if kinds["images"] == 0 || kinds["documents"] == 0 {
		t.Errorf("kinds = %v, want the photo and the note counted apart", kinds)
	}

	folders := labelsOf(t, stats["folders"])
	if folders["/"] == 0 || folders["/papers"] == 0 {
		t.Errorf("folders = %v, want both folders", folders)
	}

	// The largest list is weighed by what each file put *here*, and the photo
	// is the bigger of the two.
	largest := stats["largest"].([]any)
	if len(largest) != 2 {
		t.Fatalf("largest lists %d files, want 2", len(largest))
	}
	first := largest[0].(map[string]any)
	if first["path"] != "/holiday.jpg" {
		t.Errorf("heaviest file is %v, want /holiday.jpg", first["path"])
	}
	if first["bytes"].(float64) <= 0 || first["parts"].(float64) <= 0 {
		t.Errorf("heaviest file weighs nothing here: %v", first)
	}

	// One upload session, so one month, and it carries both files' parts.
	months := stats["months"].([]any)
	if len(months) != 1 {
		t.Fatalf("months = %d, want 1", len(months))
	}
	if months[0].(map[string]any)["parts"].(float64) <= 0 {
		t.Errorf("the month arrived empty: %v", months[0])
	}

	// No sub vaults here, so nothing is held back from the breakdown.
	if sub := stats["sub_vaults"].(map[string]any); sub["parts"].(float64) != 0 {
		t.Errorf("sub_vaults = %v with no sub vault in the file", sub)
	}
}

// An account holding the only copy of something is the number worth knowing
// before disconnecting it — the same count the disconnect guard refuses on,
// said before the refusal rather than during it.
func TestProviderStatsCountsWhatOnlyThisAccountHolds(t *testing.T) {
	c := newTestClient(t)
	c.setup("what only this holds", 3)
	ids := c.providerIDs()

	// Sent to two accounts, so a 2-of-3 file has both of its data parts on
	// them: losing either strands it.
	c.uploadTo("pinned.txt", "/", []byte(strings.Repeat("pinned ", 200)), ids[:2])

	_, body := c.json(http.MethodGet, "/api/providers/stats/"+ids[0], nil)
	stats := body["stats"].(map[string]any)
	if stats["sole"].(float64) != 1 {
		t.Errorf("sole = %v, want the one file that needs this account", stats["sole"])
	}

	// The third account holds none of it and strands nothing.
	_, spare := c.json(http.MethodGet, "/api/providers/stats/"+ids[2], nil)
	idle := spare["stats"].(map[string]any)
	if idle["files"].(float64) != 0 || idle["sole"].(float64) != 0 {
		t.Errorf("the unused account claims %v files, %v of them sole", idle["files"], idle["sole"])
	}
}

func TestProviderStatsRejectsAnUnknownAccount(t *testing.T) {
	c := newTestClient(t)
	c.setup("unknown account", 2)

	w, _ := c.json(http.MethodGet, "/api/providers/stats/no-such-account", nil)
	if w.Code == http.StatusOK {
		t.Fatalf("stats for an account that is not connected: %s", w.Body.String())
	}
}

// labelsOf reduces a breakdown to label → bytes, which is all the assertions
// above care about.
func labelsOf(t *testing.T, raw any) map[string]float64 {
	t.Helper()

	out := map[string]float64{}
	rows, ok := raw.([]any)
	if !ok {
		return out
	}
	for _, row := range rows {
		slice := row.(map[string]any)
		out[slice["label"].(string)] = slice["bytes"].(float64)
	}
	return out
}

// A capacity is the one figure in this panel that cannot be fetched from
// anywhere: a bucket reports no quota, so somebody types what they know. It
// arrives written the way it is read, and is stored with the account rather
// than sent anywhere near the backend.
func TestDeclaredCapacityIsTypedAndKept(t *testing.T) {
	c := newTestClient(t)
	c.setup("declare a capacity", 1)
	id := c.providerIDs()[0]

	w, body := c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{"capacity": "10 GB"})
	if w.Code != http.StatusOK {
		t.Fatalf("declaring a capacity: %d %s", w.Code, w.Body.String())
	}
	if got := body["provider"].(map[string]any)["capacity"].(float64); int64(got) != 10<<30 {
		t.Errorf("capacity = %v, want 10 GB in bytes", got)
	}

	// Still there after a round trip through the account list, and nothing
	// about the name or the colour moved with it.
	_, listed := c.json(http.MethodGet, "/api/providers", nil)
	account := listed["providers"].([]any)[0].(map[string]any)
	if int64(account["capacity"].(float64)) != 10<<30 {
		t.Errorf("capacity did not survive: %v", account["capacity"])
	}

	if w, _ := c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{"capacity": "lots"}); w.Code != http.StatusBadRequest {
		t.Errorf("a capacity of \"lots\" was accepted: %d", w.Code)
	}

	if w, _ := c.json(http.MethodPatch, "/api/providers/"+id, map[string]any{"capacity": ""}); w.Code != http.StatusOK {
		t.Fatalf("clearing the capacity: %d %s", w.Code, w.Body.String())
	}
	_, cleared := c.json(http.MethodGet, "/api/providers", nil)
	if capacity := cleared["providers"].([]any)[0].(map[string]any)["capacity"]; capacity != nil {
		t.Errorf("capacity = %v after being cleared", capacity)
	}
}

// Counting is for the backends with no other way of answering. A local folder
// has statfs, so it says it needs no count and refuses one.
func TestMeasuringIsOnlyForBackendsThatCannotSay(t *testing.T) {
	c := newTestClient(t)
	c.setup("count what cannot be asked", 1)
	id := c.providerIDs()[0]

	_, listed := c.json(http.MethodGet, "/api/providers", nil)
	if measurable := listed["providers"].([]any)[0].(map[string]any)["measurable"]; measurable != nil {
		t.Errorf("a local folder says it needs counting: %v", measurable)
	}

	if w, _ := c.json(http.MethodPost, "/api/providers/"+id+"/measure", nil); w.Code == http.StatusOK {
		t.Error("a local folder was counted rather than asked")
	}
}
