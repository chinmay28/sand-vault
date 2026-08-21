package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
)

// The disaster the API is for: a machine dies, SAND is reinstalled, and the
// clouds are connected back to a vault that has never seen them. These tests
// play that out over HTTP exactly as the browser does.

// reconnected returns a client on a brand new vault wired to the account
// folders an earlier one was using — a reinstall, which gives every account a
// fresh internal ID while the data on it stays put.
func reconnected(t *testing.T, password string, roots []string) *testClient {
	t.Helper()

	c := newTestClient(t)
	w, _ := c.json(http.MethodPost, "/api/vault/init", map[string]any{"password": password})
	if w.Code != http.StatusCreated {
		t.Fatalf("init the replacement vault: %d %s", w.Code, w.Body.String())
	}
	for i, root := range roots {
		w, body := c.json(http.MethodPost, "/api/providers", map[string]any{
			"kind":    "local",
			"name":    fmt.Sprintf("reconnected%d", i),
			"options": map[string]string{"path": root},
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("reconnect account %d: %d %v", i, w.Code, body)
		}
	}
	return c
}

// lostVault stores two files across three account folders, pushes the index
// backup out to them, and then throws the vault away.
func lostVault(t *testing.T, password string) []string {
	t.Helper()

	c := newTestClient(t)
	roots := c.setup(password, 3)
	c.upload("ledger.csv", "/", []byte("date,amount\n2026-08-01,42\n"))
	c.upload("notes.txt", "/", []byte("nothing to see here"))

	// The backup is pushed in the background on every index change; wait for it
	// rather than racing the very thing the recovery reads.
	c.server.vault.AwaitBackupSync()
	if warnings, err := c.server.vault.SyncManifestBackup(t.Context(), false); err != nil {
		t.Fatalf("SyncManifestBackup: %v (%v)", err, warnings)
	}
	c.server.vault.Lock()
	return roots
}

func TestRecoveryScanTellsAFreshVaultThereIsSomethingThere(t *testing.T) {
	roots := lostVault(t, "the password that is gone")
	c := reconnected(t, "a brand new password", roots)

	w, body := c.json(http.MethodGet, "/api/vault/recovery", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("scan: %d %s", w.Code, w.Body.String())
	}
	if available, _ := body["available"].(bool); !available {
		t.Fatalf("scan found nothing to recover: %v", body)
	}
	if empty, _ := body["vault_empty"].(bool); !empty {
		t.Error("the replacement vault holds no files, so it should report itself empty")
	}
	if parts, _ := body["parts"].(float64); parts == 0 {
		t.Error("scan counted no stored parts on the reconnected accounts")
	}

	sources, _ := body["sources"].([]any)
	if len(sources) != 3 {
		t.Fatalf("scan reported %d accounts, want 3", len(sources))
	}
	for _, raw := range sources {
		source := raw.(map[string]any)
		if backup, _ := source["backup"].(bool); !backup {
			t.Errorf("%v is not reported as holding a backup", source["name"])
		}
		if foreign, _ := source["foreign"].(bool); !foreign {
			t.Errorf("%v holds another vault's backup and should say so", source["name"])
		}
	}
}

func TestRecoveryScanIsQuietWhenThereIsNothingToRecover(t *testing.T) {
	c := newTestClient(t)
	c.setup("a password", 2)

	w, body := c.json(http.MethodGet, "/api/vault/recovery", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("scan: %d %s", w.Code, w.Body.String())
	}
	if available, _ := body["available"].(bool); available {
		t.Errorf("a vault should not be offered its own accounts back: %v", body)
	}
}

func TestRecoveryOverHTTPRebuildsTheVault(t *testing.T) {
	const lostPassword = "the password that is gone"
	roots := lostVault(t, lostPassword)
	c := reconnected(t, "a brand new password", roots)

	// The dry run comes first, because a recovery is only as complete as the
	// accounts that were reconnected and finding that out afterwards is worse.
	w, body := c.json(http.MethodPost, "/api/vault/recovery", map[string]any{
		"password": lostPassword,
		"dry_run":  true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	report := body["report"].(map[string]any)
	if got, _ := report["recoverable"].(float64); got != 2 {
		t.Fatalf("preview says %v of the 2 files would come back: %v", got, report)
	}
	if _, ok := body["backup_at"]; !ok {
		t.Error("the preview should say when the backup it read was written")
	}
	// A dry run changes nothing.
	if _, listing := c.json(http.MethodGet, "/api/files?path=/", nil); len(listing["files"].([]any)) != 0 {
		t.Fatalf("the dry run put files in the vault: %v", listing)
	}

	w, body = c.json(http.MethodPost, "/api/vault/recovery", map[string]any{"password": lostPassword})
	if w.Code != http.StatusOK {
		t.Fatalf("recover: %d %s", w.Code, w.Body.String())
	}
	report = body["report"].(map[string]any)
	if got, _ := report["recoverable"].(float64); got != 2 {
		t.Fatalf("recovery brought back %v of 2 files: %v", got, report)
	}
	if lost, _ := report["lost"].(float64); lost != 0 {
		t.Errorf("report says %v files were lost, want none", lost)
	}

	// The tree is back, and so are the contents — which is what adopting the
	// lost vault's data key buys.
	_, listing := c.json(http.MethodGet, "/api/files?path=/", nil)
	files := listing["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("recovered listing holds %d files: %v", len(files), listing)
	}
	id := files[0].(map[string]any)["id"].(string)
	content := c.do(http.MethodGet, "/api/files/"+id+"/content", nil, "")
	if content.Code != http.StatusOK || content.Body.Len() == 0 {
		t.Fatalf("reading a recovered file: %d %s", content.Code, content.Body.String())
	}

	// And the offer is withdrawn, because there is nothing left to offer.
	if _, scan := c.json(http.MethodGet, "/api/vault/recovery", nil); scan["available"].(bool) {
		t.Error("a recovered vault is still being offered the same recovery")
	}
}

func TestRecoveryOverHTTPReportsWhatItCouldNotBringBack(t *testing.T) {
	const lostPassword = "the password that is gone"
	roots := lostVault(t, lostPassword)

	// One cloud of three, which is one short of the two parts it takes to
	// rebuild anything.
	c := reconnected(t, "a brand new password", roots[:1])

	w, body := c.json(http.MethodPost, "/api/vault/recovery", map[string]any{
		"password": lostPassword,
		"dry_run":  true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}

	var report struct {
		Files            int   `json:"files"`
		Recoverable      int   `json:"recoverable"`
		Lost             int   `json:"lost"`
		LostBytes        int64 `json:"lost_bytes"`
		Bytes            int64 `json:"bytes"`
		RecoverableBytes int64 `json:"recoverable_bytes"`
		Missing          []struct {
			Path       string   `json:"path"`
			PartsFound int      `json:"parts_found"`
			Accounts   []string `json:"accounts"`
		} `json:"missing"`
		MissingAccounts []struct {
			Name     string `json:"name"`
			Files    int    `json:"files"`
			Blocking bool   `json:"blocking"`
		} `json:"missing_accounts"`
	}
	raw, _ := json.Marshal(body["report"])
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decoding the report: %v", err)
	}

	if report.Files != 2 || report.Recoverable != 0 || report.Lost != 2 {
		t.Fatalf("report = %+v, want 2 files and neither of them openable", report)
	}
	if report.LostBytes != report.Bytes || report.RecoverableBytes != 0 {
		t.Errorf("report weighs %d of %d bytes as lost, want all of it", report.LostBytes, report.Bytes)
	}
	if len(report.Missing) != 2 {
		t.Fatalf("report names %d missing files, want both: %+v", len(report.Missing), report.Missing)
	}
	for _, file := range report.Missing {
		if file.PartsFound != 1 {
			t.Errorf("%s: found %d parts, want the 1 on the single reconnected account",
				file.Path, file.PartsFound)
		}
		if len(file.Accounts) != 2 {
			t.Errorf("%s: blames %v, want the two clouds still to be connected",
				file.Path, file.Accounts)
		}
	}
	if len(report.MissingAccounts) != 2 {
		t.Fatalf("report names %d accounts to reconnect, want 2: %+v",
			len(report.MissingAccounts), report.MissingAccounts)
	}
	for _, account := range report.MissingAccounts {
		if !account.Blocking || account.Files != 2 {
			t.Errorf("%+v should be reported as blocking both files", account)
		}
	}
}

// What the report's advice amounts to over HTTP: recover with one cloud,
// connect the other two, and finish without being asked for a password again.
func TestRecoveryResumesOnceTheRestOfTheCloudsAreBack(t *testing.T) {
	const lostPassword = "the password that is gone"
	roots := lostVault(t, lostPassword)
	c := reconnected(t, "a brand new password", roots[:1])

	w, body := c.json(http.MethodPost, "/api/vault/recovery", map[string]any{"password": lostPassword})
	if w.Code != http.StatusOK {
		t.Fatalf("recover: %d %s", w.Code, w.Body.String())
	}
	if lost, _ := body["report"].(map[string]any)["lost"].(float64); lost != 2 {
		t.Fatalf("first pass should have stranded both files: %v", body["report"])
	}

	// The vault is not empty any more, so the prompt changes shape: there is
	// nothing left to adopt, and something left to reach.
	_, scan := c.json(http.MethodGet, "/api/vault/recovery", nil)
	if available, _ := scan["available"].(bool); available {
		t.Error("a vault holding files cannot adopt another snapshot and should not be offered one")
	}
	if resumable, _ := scan["resumable"].(bool); !resumable {
		t.Fatalf("scan should offer to resume: %v", scan)
	}
	if stranded, _ := scan["stranded"].(float64); stranded != 2 {
		t.Errorf("scan says %v files are stranded, want 2", stranded)
	}

	// The two clouds turn up.
	for i, root := range roots[1:] {
		w, resp := c.json(http.MethodPost, "/api/providers", map[string]any{
			"kind":    "local",
			"name":    fmt.Sprintf("late%d", i),
			"options": map[string]string{"path": root},
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("connect a late account: %d %v", w.Code, resp)
		}
	}

	w, body = c.json(http.MethodPost, "/api/vault/recovery/resume", map[string]any{"dry_run": true})
	if w.Code != http.StatusOK {
		t.Fatalf("resume preview: %d %s", w.Code, w.Body.String())
	}
	if lost, _ := body["report"].(map[string]any)["lost"].(float64); lost != 0 {
		t.Fatalf("the preview should reach everything now: %v", body["report"])
	}

	w, body = c.json(http.MethodPost, "/api/vault/recovery/resume", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}
	report := body["report"].(map[string]any)
	if lost, _ := report["lost"].(float64); lost != 0 {
		t.Fatalf("resume left %v files unreachable: %v", lost, report)
	}
	if relocated, _ := report["relocated"].(float64); relocated == 0 {
		t.Error("the parts on the late accounts had to be re-pointed at them")
	}

	// A file that could not be opened before this now opens.
	_, listing := c.json(http.MethodGet, "/api/files?path=/", nil)
	id := listing["files"].([]any)[0].(map[string]any)["id"].(string)
	content := c.do(http.MethodGet, "/api/files/"+id+"/content", nil, "")
	if content.Code != http.StatusOK || content.Body.Len() == 0 {
		t.Fatalf("reading a file the resume brought back: %d %s", content.Code, content.Body.String())
	}

	// And there is nothing left to offer.
	_, scan = c.json(http.MethodGet, "/api/vault/recovery", nil)
	if resumable, _ := scan["resumable"].(bool); resumable {
		t.Errorf("everything is reachable, so nothing should be offered: %v", scan)
	}
}

// Recovery gets the files back on the dead vault's key; reclaiming is what
// takes them off it, and what lets the user say where they should live instead.
func TestReclaimingARecoveredVaultOverHTTP(t *testing.T) {
	const lostPassword = "the password that is gone"
	roots := lostVault(t, lostPassword)
	c := reconnected(t, "a brand new password", roots)

	w, _ := c.json(http.MethodPost, "/api/vault/recovery", map[string]any{"password": lostPassword})
	if w.Code != http.StatusOK {
		t.Fatalf("recover: %d %s", w.Code, w.Body.String())
	}

	// The vault says, and keeps saying, that its key is not its own.
	_, status := c.json(http.MethodGet, "/api/vault", nil)
	stats := status["stats"].(map[string]any)
	if inherited, _ := stats["inherited_key"].(bool); !inherited {
		t.Fatalf("a recovered vault should report an inherited key: %v", stats)
	}

	// A cloud the dead vault never used, to prove the selection is honoured.
	elsewhere := filepath.Join(t.TempDir(), "somewhere-new")
	w, resp := c.json(http.MethodPost, "/api/providers", map[string]any{
		"kind": "local", "name": "somewhere-new", "options": map[string]string{"path": elsewhere},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("connect a new account: %d %v", w.Code, resp)
	}
	fresh := resp["provider"].(map[string]any)["id"].(string)

	ids := c.providerIDs()
	chosen := []string{fresh}
	for _, id := range ids {
		if id != fresh && len(chosen) < 3 {
			chosen = append(chosen, id)
		}
	}

	w, body := c.json(http.MethodPost, "/api/vault/reclaim", map[string]any{"accounts": chosen})
	if w.Code != http.StatusOK {
		t.Fatalf("reclaim: %d %s", w.Code, w.Body.String())
	}
	if migrated, _ := body["migrated"].(float64); migrated != 2 {
		t.Fatalf("reclaim moved %v of the 2 files: %v", migrated, body)
	}
	if remaining, _ := body["remaining"].(float64); remaining != 0 {
		t.Errorf("reclaim left %v files on the old key", remaining)
	}

	// The warning is gone, and nothing is waiting on a migration.
	_, status = c.json(http.MethodGet, "/api/vault", nil)
	stats = status["stats"].(map[string]any)
	if inherited, _ := stats["inherited_key"].(bool); inherited {
		t.Error("the key is this vault's own now and should not report as inherited")
	}
	if pending, _ := stats["pending_migration"].(float64); pending != 0 {
		t.Errorf("%v files are still on an older key", pending)
	}

	// Every part is on a cloud that was chosen, and the files still open.
	_, listing := c.json(http.MethodGet, "/api/files?path=/", nil)
	files := listing["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	for _, raw := range files {
		file := raw.(map[string]any)
		shards := file["shards"].([]any)
		if len(shards) != 3 {
			t.Errorf("%v has %d parts, want a full set", file["name"], len(shards))
		}
		for _, s := range shards {
			id := s.(map[string]any)["provider_id"].(string)
			if !slices.Contains(chosen, id) {
				t.Errorf("%v: a part landed on %v, which was not chosen",
					file["name"], s.(map[string]any)["provider_name"])
			}
		}
	}
	id := files[0].(map[string]any)["id"].(string)
	content := c.do(http.MethodGet, "/api/files/"+id+"/content", nil, "")
	if content.Code != http.StatusOK || content.Body.Len() == 0 {
		t.Fatalf("reading a reclaimed file: %d %s", content.Code, content.Body.String())
	}
}

func TestReclaimRefusesACloudSelectionThatCannotHoldAFile(t *testing.T) {
	const lostPassword = "the password that is gone"
	roots := lostVault(t, lostPassword)
	c := reconnected(t, "a brand new password", roots)

	w, _ := c.json(http.MethodPost, "/api/vault/recovery", map[string]any{"password": lostPassword})
	if w.Code != http.StatusOK {
		t.Fatalf("recover: %d %s", w.Code, w.Body.String())
	}

	w, body := c.json(http.MethodPost, "/api/vault/reclaim",
		map[string]any{"accounts": []string{c.providerIDs()[0]}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("reclaim onto one cloud = %d %v, want it refused", w.Code, body)
	}

	// Refused before anything rotated, so the vault is exactly as it was.
	_, status := c.json(http.MethodGet, "/api/vault", nil)
	stats := status["stats"].(map[string]any)
	if inherited, _ := stats["inherited_key"].(bool); !inherited {
		t.Error("a refused reclaim changed the key anyway")
	}
	if pending, _ := stats["pending_migration"].(float64); pending != 0 {
		t.Errorf("a refused reclaim left %v files mid-migration", pending)
	}
}

func TestRecoveryRejectsTheWrongPassword(t *testing.T) {
	roots := lostVault(t, "the password that is gone")
	c := reconnected(t, "a brand new password", roots)

	// The new vault's own password is exactly the wrong guess someone makes
	// here, and it has to fail cleanly rather than half-recover anything.
	w, body := c.json(http.MethodPost, "/api/vault/recovery", map[string]any{
		"password": "a brand new password",
	})
	if w.Code != http.StatusUnauthorized || body["code"] != "WRONG_PASSWORD" {
		t.Fatalf("recover with the wrong password = %d %v, want 401 WRONG_PASSWORD", w.Code, body)
	}
	if _, listing := c.json(http.MethodGet, "/api/files?path=/", nil); len(listing["files"].([]any)) != 0 {
		t.Fatalf("a refused recovery put files in the vault: %v", listing)
	}
}

func TestRecoveryRefusesAVaultThatIsInUse(t *testing.T) {
	const lostPassword = "the password that is gone"
	roots := lostVault(t, lostPassword)
	c := reconnected(t, "a brand new password", roots)

	// Adopting a snapshot replaces the data key, so a vault with files of its
	// own must not be recovered into: those files would be stranded.
	c.upload("mine.txt", "/", []byte("written on the new machine"))

	w, body := c.json(http.MethodPost, "/api/vault/recovery", map[string]any{"password": lostPassword})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("recover into a used vault = %d %v, want it refused", w.Code, body)
	}

	// And the offer is off the table for as long as that is true.
	if _, scan := c.json(http.MethodGet, "/api/vault/recovery", nil); scan["available"].(bool) {
		t.Error("a vault holding files should not be offered a recovery it cannot run")
	}
}

func TestRecoveryNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("a password", 2)
	c.cookies = nil

	for _, call := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/vault/recovery", nil},
		{http.MethodPost, "/api/vault/recovery", map[string]any{"password": "guess"}},
		{http.MethodPost, "/api/vault/recovery/resume", nil},
	} {
		w, _ := c.json(call.method, call.path, call.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", call.method, call.path, w.Code)
		}
	}
}

// A local-folder account is a directory, and the recovery path has to survive
// one that was pointed somewhere that no longer exists — the second half of
// "the machine died", where a sync folder came back empty.
func TestRecoveryScanReportsAnAccountItCannotRead(t *testing.T) {
	roots := lostVault(t, "the password that is gone")
	gone := filepath.Join(t.TempDir(), "never-restored")
	c := reconnected(t, "a brand new password", append(append([]string{}, roots...), gone))

	w, body := c.json(http.MethodGet, "/api/vault/recovery", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("scan: %d %s", w.Code, w.Body.String())
	}
	// The accounts that did come back are still enough, so the offer stands.
	if available, _ := body["available"].(bool); !available {
		t.Fatalf("one unreadable account should not withdraw the offer: %v", body)
	}

	sources, _ := body["sources"].([]any)
	if len(sources) != 4 {
		t.Fatalf("scan reported %d accounts, want all 4", len(sources))
	}
	var empty int
	for _, raw := range sources {
		source := raw.(map[string]any)
		if backup, _ := source["backup"].(bool); !backup {
			empty++
		}
	}
	if empty != 1 {
		t.Errorf("%d accounts reported nothing, want only the one that was never restored", empty)
	}
}

// An account that comes back after a recovery is still holding the index of
// the vault that died, and the guard protecting somebody else's backup would go
// on refusing this vault's forever. Forcing is how the report's repair claims
// it — see backupRequest.Force.
func TestForcedBackupClaimsAnAccountHoldingAnotherVaultsIndex(t *testing.T) {
	roots := lostVault(t, "the password that is gone")

	// Only two of the three come back, so the third keeps the dead vault's
	// copy through the recovery's own forced push.
	c := reconnected(t, "a brand new password", roots[:2])

	w, _ := c.json(http.MethodPost, "/api/vault/recovery", map[string]any{
		"password": "the password that is gone",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("recover: %d %s", w.Code, w.Body.String())
	}
	c.server.vault.AwaitBackupSync()

	// The straggler arrives late — the account the report would offer a repair
	// for. An ordinary push leaves it alone, because what is on it was written
	// by a vault this one cannot open.
	w, _ = c.json(http.MethodPost, "/api/providers", map[string]any{
		"kind": "local", "name": "late", "options": map[string]string{"path": roots[2]},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("connect the straggler: %d %s", w.Code, w.Body.String())
	}
	c.server.vault.AwaitBackupSync()

	before, err := c.server.vault.ScanForRecovery(t.Context())
	if err != nil {
		t.Fatalf("ScanForRecovery: %v", err)
	}
	foreign := 0
	for _, src := range before.Sources {
		if src.Backup && src.Foreign {
			foreign++
		}
	}
	if foreign == 0 {
		// Not a skip: this is the whole premise. If the straggler is not
		// holding the dead vault's index, the claim below proves nothing and
		// the test has quietly stopped testing anything.
		t.Fatal("the late account is not holding another vault's index, so there is nothing to claim")
	}

	// Forcing claims it.
	w, _ = c.json(http.MethodPost, "/api/vault/backup", map[string]any{
		"enabled": true, "force": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("forced backup: %d %s", w.Code, w.Body.String())
	}
	c.server.vault.AwaitBackupSync()

	after, err := c.server.vault.ScanForRecovery(t.Context())
	if err != nil {
		t.Fatalf("ScanForRecovery after the claim: %v", err)
	}
	for _, src := range after.Sources {
		if src.Backup && src.Foreign {
			t.Errorf("%s still holds another vault's index after a forced claim", src.Name)
		}
	}
}

// A refused *enable* must keep carrying its reason. The forced claim is
// forgiving of a refusal — there is simply nothing to write — and letting that
// tolerance leak into the enable path would answer 200 to a request that had
// just erased every copy it was asked to create.
func TestBackupEnableStillReportsARefusal(t *testing.T) {
	c := newTestClient(t)
	// Redundant placement with two accounts puts enough parts of a file on one
	// of them to rebuild it, which is the one configuration a backup refuses:
	// the data key would be sitting beside the parts it opens.
	c.setup("a password", 2)
	w, _ := c.json(http.MethodPost, "/api/vault/policy", map[string]any{"policy": "redundant"})
	if w.Code != http.StatusOK {
		t.Fatalf("switch to redundant: %d %s", w.Code, w.Body.String())
	}

	w, body := c.json(http.MethodPost, "/api/vault/backup", map[string]any{"enabled": true})
	if w.Code == http.StatusOK {
		t.Fatalf("a refused enable answered 200 with %v", body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Errorf("the refusal carries no reason: %v", body)
	}

	// The forced claim, on the same vault, is not an error: it wrote nothing
	// because there was nothing to write, and it says so rather than failing.
	w, body = c.json(http.MethodPost, "/api/vault/backup", map[string]any{
		"enabled": true, "force": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("a forced claim on a refusing vault: %d %s", w.Code, w.Body.String())
	}
	warnings, _ := body["warnings"].([]any)
	if len(warnings) == 0 {
		t.Error("the forced claim said nothing about why it wrote nothing")
	}
}
