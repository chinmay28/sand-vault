package davfs

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

const subPassword = "the sub vault's own password"

// newSharesServer serves the vault's share at /dav and the sub vaults' shares
// under /sub, the way the server mounts them.
func newSharesServer(t *testing.T, v *vault.Vault) *httptest.Server {
	t.Helper()
	main, err := Handler(v, Options{Prefix: "/dav"})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	subs := SubVaultHandler(v, Options{Prefix: "/sub"})
	mux := http.NewServeMux()
	mux.Handle("/dav", main)
	mux.Handle("/dav/", main)
	mux.Handle("/sub", subs)
	mux.Handle("/sub/", subs)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// doAs is do with the password chosen by the caller.
func doAs(t *testing.T, srv *httptest.Server, password, method, path string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.SetBasicAuth("anyone", password)
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return string(body)
}

// twoVaults is a vault with a file in the main vault and a sub vault called
// Taxes with a file of its own, so that a share can be shown to see one and
// not the other.
func twoVaults(t *testing.T) (*vault.Vault, vault.SubVaultInfo) {
	t.Helper()
	v := newTestVault(t)
	ctx := context.Background()

	sub, err := v.CreateSubVault("Taxes", subPassword)
	if err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}
	scope := vault.Scope(sub.ID)

	if err := v.Mkdir(vault.MainScope, "/Public"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := v.Upload(ctx, vault.MainScope, "/Public", "open.txt", []byte("nothing secret"), vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload to main: %v", err)
	}
	if err := v.Mkdir(scope, "/Papers"); err != nil {
		t.Fatalf("Mkdir in the sub vault: %v", err)
	}
	if _, _, err := v.Upload(ctx, scope, "/Papers", "p60.pdf", []byte("a payslip"), vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload to the sub vault: %v", err)
	}
	return v, sub
}

// A sub vault's share presents the sub vault and nothing else, and the vault's
// own share still presents the main vault and nothing else: mounting either
// isolates the other from it.
func TestSubVaultShareIsScopedToTheSubVault(t *testing.T) {
	v, sub := twoVaults(t)
	srv := newSharesServer(t, v)
	scope := vault.Scope(sub.ID)

	resp := doAs(t, srv, subPassword, "PROPFIND", "/sub/Taxes/", nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /sub/Taxes/ = %d, want 207", resp.StatusCode)
	}
	listing := readAll(t, resp)
	if !strings.Contains(listing, "/sub/Taxes/Papers/") {
		t.Errorf("the sub vault's share does not list its own folder with the full mount path:\n%s", listing)
	}
	if strings.Contains(listing, "Public") {
		t.Errorf("the sub vault's share lists the main vault's folder:\n%s", listing)
	}

	resp = doAs(t, srv, subPassword, "GET", "/sub/Taxes/Papers/p60.pdf", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET a sub vault file over its share = %d, want 200", resp.StatusCode)
	}
	if got := readAll(t, resp); got != "a payslip" {
		t.Errorf("GET returned %q, want the sub vault's file", got)
	}

	// The main vault's file is not under the sub vault's share at any path.
	resp = doAs(t, srv, subPassword, "GET", "/sub/Taxes/Public/open.txt", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET the main vault's file over the sub vault's share = %d, want 404", resp.StatusCode)
	}

	// A write over the sub vault's share lands in the sub vault.
	resp = doAs(t, srv, subPassword, "PUT", "/sub/Taxes/Papers/p45.pdf", bytes.NewReader([]byte("a leaving form")), nil)
	if resp.StatusCode > 299 {
		t.Fatalf("PUT over the sub vault's share = %d", resp.StatusCode)
	}
	if _, err := v.EntryByPath(scope, "/Papers/p45.pdf"); err != nil {
		t.Errorf("the PUT did not land in the sub vault: %v", err)
	}
	if _, err := v.EntryByPath(vault.MainScope, "/Papers/p45.pdf"); err == nil {
		t.Error("the PUT over the sub vault's share landed in the main vault")
	}

	// And the vault's own share, with the sub vault open, still shows none
	// of it.
	resp = doAs(t, srv, testPassword, "PROPFIND", "/dav/", nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /dav/ = %d, want 207", resp.StatusCode)
	}
	if listing := readAll(t, resp); strings.Contains(listing, "Papers") {
		t.Errorf("the vault's share lists a sub vault's folder:\n%s", listing)
	}
	resp = doAs(t, srv, testPassword, "GET", "/dav/Papers/p60.pdf", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET a sub vault file over the vault's share = %d, want 404", resp.StatusCode)
	}
}

// Each share answers to its own password and no other. The vault password
// opens the vault and does not open a sub vault, so it does not open a sub
// vault's share either; and a sub vault's password opens nothing but it.
func TestEachShareTakesOnlyItsOwnPassword(t *testing.T) {
	v, _ := twoVaults(t)
	srv := newSharesServer(t, v)

	for _, tc := range []struct {
		what     string
		password string
		path     string
	}{
		{"the vault password on a sub vault's share", testPassword, "/sub/Taxes/"},
		{"a sub vault's password on the vault's share", subPassword, "/dav/"},
		{"a wrong password on a sub vault's share", "not it", "/sub/Taxes/"},
	} {
		resp := doAs(t, srv, tc.password, "PROPFIND", tc.path, nil, map[string]string{"Depth": "1"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", tc.what, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no Basic challenge", tc.what)
		}
	}
}

// A sub vault is named in the address by its label, case ignored, or by its
// ID. Whichever is used, the listing carries that same mount path back.
func TestSubVaultShareAnswersToLabelAndID(t *testing.T) {
	v, sub := twoVaults(t)
	srv := newSharesServer(t, v)

	for _, name := range []string{"Taxes", "taxes", "TAXES", sub.ID} {
		resp := doAs(t, srv, subPassword, "PROPFIND", "/sub/"+name+"/", nil, map[string]string{"Depth": "1"})
		if resp.StatusCode != http.StatusMultiStatus {
			t.Errorf("PROPFIND /sub/%s/ = %d, want 207", name, resp.StatusCode)
			continue
		}
		if listing := readAll(t, resp); !strings.Contains(listing, "/sub/"+name+"/Papers/") {
			t.Errorf("PROPFIND /sub/%s/ does not carry its own mount path in the listing:\n%s", name, listing)
		}
	}
}

// A name that is no sub vault's is answered exactly as a wrong password is, so
// which sub vaults exist cannot be read off the status code. The root of the
// shares names nothing at all.
func TestUnknownSubVaultLooksLikeAWrongPassword(t *testing.T) {
	v, _ := twoVaults(t)
	srv := newSharesServer(t, v)

	resp := doAs(t, srv, "anything", "PROPFIND", "/sub/Nothing/", nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PROPFIND on a sub vault that does not exist = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("an unknown sub vault was refused without the Basic challenge a wrong password gets")
	}

	for _, root := range []string{"/sub", "/sub/"} {
		resp = doAs(t, srv, subPassword, "PROPFIND", root, nil, map[string]string{"Depth": "1"})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("PROPFIND %s = %d, want 404: there is nothing to list at the root of the shares", root, resp.StatusCode)
		}
	}
}

// A mount outlives whoever opened the sub vault in the app, so the credential
// it keeps sending is what opens a shut sub vault — and a wrong one leaves it
// shut.
func TestSubVaultShareOpensAShutSubVault(t *testing.T) {
	v, sub := twoVaults(t)
	srv := newSharesServer(t, v)

	if err := v.LockSubVault(sub.ID); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	resp := doAs(t, srv, "not it", "GET", "/sub/Taxes/Papers/p60.pdf", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET with a wrong password against a shut sub vault = %d, want 401", resp.StatusCode)
	}
	if v.SubVaultUnlocked(sub.ID) {
		t.Fatal("a wrong password opened the sub vault")
	}

	resp = doAs(t, srv, subPassword, "GET", "/sub/Taxes/Papers/p60.pdf", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET against a shut sub vault = %d, want 200 after its password opens it", resp.StatusCode)
	}
	if !v.SubVaultUnlocked(sub.ID) {
		t.Error("the sub vault is still shut")
	}
}

// A sub vault's password cannot open the vault around it, and its files are on
// accounts the vault password seals — so while the vault is locked the share
// is unavailable rather than asking for a password that would change nothing.
func TestSubVaultShareIsUnavailableWhileTheVaultIsLocked(t *testing.T) {
	v, sub := twoVaults(t)
	srv := newSharesServer(t, v)

	v.Lock()
	resp := doAs(t, srv, subPassword, "GET", "/sub/Taxes/Papers/p60.pdf", nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET over a sub vault's share while the vault is locked = %d, want 503", resp.StatusCode)
	}
	if v.Unlocked() {
		t.Fatal("a sub vault's password unlocked the vault")
	}

	// The vault's own share brings the vault back, as it always has; the sub
	// vault is shut by the lock and its own share opens it again.
	resp = doAs(t, srv, testPassword, "PROPFIND", "/dav/", nil, map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /dav/ with the vault password = %d, want 207", resp.StatusCode)
	}
	if v.SubVaultUnlocked(sub.ID) {
		t.Fatal("unlocking the vault opened the sub vault, which only its own password should")
	}
	resp = doAs(t, srv, subPassword, "GET", "/sub/Taxes/Papers/p60.pdf", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET over the sub vault's share once the vault is open = %d, want 200", resp.StatusCode)
	}
}

// The remembered credential must not outlive the sub vault being locked in the
// app: what lets a request back in afterwards is the password being checked
// again, not the cache.
func TestSubVaultVerifierDoesNotCacheThroughALock(t *testing.T) {
	v, sub := twoVaults(t)
	auth, err := subVaultVerifier(v, sub.ID, DefaultVerifyTTL)
	if err != nil {
		t.Fatalf("subVaultVerifier: %v", err)
	}

	if err := auth.check(subPassword); err != nil {
		t.Fatalf("check: %v", err)
	}
	if err := v.LockSubVault(sub.ID); err != nil {
		t.Fatalf("LockSubVault: %v", err)
	}

	if err := auth.check(subPassword); err != nil {
		t.Fatalf("check after locking: %v", err)
	}
	if !v.SubVaultUnlocked(sub.ID) {
		t.Error("the cached answer was used instead of re-opening the sub vault")
	}

	// And the vault password, which is a wrong password here, is refused
	// against an open sub vault too — not waved through because it is open.
	if err := auth.check(testPassword); err == nil {
		t.Error("the vault password was accepted by a sub vault's share")
	}
}

// A MOVE whose destination is another share is a move between vaults, which the
// share does not do: the destination has to be under the same mount.
func TestMoveBetweenSharesIsRefused(t *testing.T) {
	v, sub := twoVaults(t)
	srv := newSharesServer(t, v)
	scope := vault.Scope(sub.ID)

	resp := doAs(t, srv, subPassword, "MOVE", "/sub/Taxes/Papers/p60.pdf", nil,
		map[string]string{"Destination": srv.URL + "/dav/p60.pdf"})
	if resp.StatusCode < 400 {
		t.Fatalf("MOVE from a sub vault's share into the vault's = %d, want a refusal", resp.StatusCode)
	}
	if _, err := v.EntryByPath(scope, "/Papers/p60.pdf"); err != nil {
		t.Errorf("the refused MOVE took the file out of the sub vault: %v", err)
	}
	if _, err := v.EntryByPath(vault.MainScope, "/p60.pdf"); err == nil {
		t.Error("the refused MOVE put the file in the main vault")
	}
}

// Renaming a sub vault moves its share to the new name, and the old name
// answers as any unknown one does.
func TestSubVaultShareFollowsARename(t *testing.T) {
	v, sub := twoVaults(t)
	srv := newSharesServer(t, v)

	resp := doAs(t, srv, subPassword, "PROPFIND", "/sub/Taxes/", nil, map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /sub/Taxes/ = %d, want 207", resp.StatusCode)
	}

	if err := v.RenameSubVault(sub.ID, "Receipts"); err != nil {
		t.Fatalf("RenameSubVault: %v", err)
	}

	resp = doAs(t, srv, subPassword, "PROPFIND", "/sub/Taxes/", nil, map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PROPFIND on the old name after a rename = %d, want 401", resp.StatusCode)
	}
	resp = doAs(t, srv, subPassword, "PROPFIND", "/sub/Receipts/", nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND on the new name after a rename = %d, want 207", resp.StatusCode)
	}
	if listing := readAll(t, resp); !strings.Contains(listing, "/sub/Receipts/Papers/") {
		t.Errorf("the renamed share does not carry its new mount path:\n%s", listing)
	}
	// The ID still answers, and the stale endpoint for the old name is gone.
	resp = doAs(t, srv, subPassword, "PROPFIND", "/sub/"+sub.ID+"/", nil, map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("PROPFIND by ID after a rename = %d, want 207", resp.StatusCode)
	}
}

// Two sub vaults swapping names must not leave either answering with the
// other's tree.
func TestSubVaultSharesRebuiltWhenNamesSwap(t *testing.T) {
	v, sub := twoVaults(t)
	srv := newSharesServer(t, v)
	ctx := context.Background()

	other, err := v.CreateSubVault("Health", "the other sub vault's password")
	if err != nil {
		t.Fatalf("CreateSubVault: %v", err)
	}
	if _, _, err := v.Upload(ctx, vault.Scope(other.ID), "/", "scan.pdf", []byte("an x-ray"), vault.UploadOptions{}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Both shares built under their first names.
	if resp := doAs(t, srv, subPassword, "PROPFIND", "/sub/Taxes/", nil, map[string]string{"Depth": "1"}); resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /sub/Taxes/ = %d", resp.StatusCode)
	}
	if resp := doAs(t, srv, "the other sub vault's password", "PROPFIND", "/sub/Health/", nil, map[string]string{"Depth": "1"}); resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /sub/Health/ = %d", resp.StatusCode)
	}

	for _, step := range []struct{ id, label string }{
		{sub.ID, "Swapping"}, {other.ID, "Taxes"}, {sub.ID, "Health"},
	} {
		if err := v.RenameSubVault(step.id, step.label); err != nil {
			t.Fatalf("RenameSubVault: %v", err)
		}
	}

	// /sub/Taxes/ is now the other sub vault: its password, its file.
	resp := doAs(t, srv, "the other sub vault's password", "PROPFIND", "/sub/Taxes/", nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /sub/Taxes/ after the swap = %d, want 207", resp.StatusCode)
	}
	listing := readAll(t, resp)
	if !strings.Contains(listing, "scan.pdf") || strings.Contains(listing, "Papers") {
		t.Errorf("/sub/Taxes/ after the swap does not present the sub vault now called Taxes:\n%s", listing)
	}
	if resp := doAs(t, srv, subPassword, "PROPFIND", "/sub/Taxes/", nil, map[string]string{"Depth": "1"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the first sub vault's password still opens /sub/Taxes/ after the swap: %d", resp.StatusCode)
	}
}
