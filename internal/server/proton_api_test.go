package server

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// protonStubClient writes a stand-in for Proton's client — enough of one to
// sign in and to let a connect succeed afterwards.
func protonStubClient(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in client is a shell script")
	}

	dir := t.TempDir()
	drive := filepath.Join(dir, "drive", "my-files")
	if err := os.MkdirAll(drive, 0o700); err != nil {
		t.Fatalf("creating the stand-in drive: %v", err)
	}

	binary := filepath.Join(dir, "proton-drive")
	script := `#!/bin/sh
set -e
DRIVE="` + filepath.Join(dir, "drive") + `"
SESSION="$PROTON_DRIVE_CACHE_DIR/auth-session.json"

if [ "$1" = "auth" ] && [ "$2" = "login" ]; then
	printf '{"signInUrl":"https://account.proton.me/authorize?stub"}\n'
	printf 'a-real-session' > "$SESSION"
	exit 0
fi

[ -f "$SESSION" ] || { echo "You need to login first" >&2; exit 1; }

action="$2"
shift 2
args=""
while [ $# -gt 0 ]; do
	case "$1" in
		--json|--skip-thumbnails) ;;
		--file-conflict-strategy|--folder-conflict-strategy) shift ;;
		*) args="$args $1" ;;
	esac
	shift
done
# shellcheck disable=SC2086
set -- $args

case "$action" in
	info)
		target="$DRIVE/$(echo "$1" | sed 's|^/||')"
		[ -e "$target" ] || { echo "ValidationError: Node not found: $1" >&2; exit 1; }
		printf '{"name":{"ok":true,"value":"%s"},"type":"folder","ownedBy":{"email":"someone@proton.me"}}\n' "$(basename "$1")"
		;;
	create-folder)
		mkdir -p "$DRIVE/$(echo "$1" | sed 's|^/||')/$2"
		;;
	list) printf '[\n\n]\n' ;;
	*) echo "unexpected action $action" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the stand-in client: %v", err)
	}
	return binary
}

// awaitFlow polls a sign-in until it reaches a state worth looking at, the way
// the browser does.
func awaitFlow(t *testing.T, c *testClient, flowID string, want func(map[string]any) bool) map[string]any {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for {
		_, status := c.json(http.MethodGet, "/api/providers/oauth/"+flowID, nil)
		if want(status) {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sign-in never got there: %v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestProtonSignInConnectsAnAccount is the browser's whole path: start a
// sign-in, be given a link to follow, and turn what comes back into a connected
// account — using the same status and complete endpoints every other sign-in
// uses, which is the point of building it on the flow store.
func TestProtonSignInConnectsAnAccount(t *testing.T) {
	binary := protonStubClient(t)

	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)
	t.Setenv("SAND_PROTON_STATE_DIR", filepath.Join(t.TempDir(), "proton"))

	w, start := c.json(http.MethodPost, "/api/providers/proton/signin", map[string]any{
		"options": map[string]string{"binary": binary, "folder": "/my-files/sand"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %v", w.Code, start)
	}
	flowID, _ := start["flow_id"].(string)
	if flowID == "" {
		t.Fatalf("no flow: %v", start)
	}

	// The link arrives a moment after the flow, because it comes out of a
	// client that has to be run to produce it.
	shown := awaitFlow(t, c, flowID, func(status map[string]any) bool {
		return status["sign_in_url"] != nil
	})
	if url, _ := shown["sign_in_url"].(string); !strings.HasPrefix(url, "https://account.proton.me/") {
		t.Fatalf("sign_in_url = %v", shown["sign_in_url"])
	}

	ready := awaitFlow(t, c, flowID, func(status map[string]any) bool {
		return status["status"] == "ready" || status["status"] == "error"
	})
	if ready["status"] != "ready" {
		t.Fatalf("sign-in finished as %v", ready)
	}
	if ready["account"] != "someone@proton.me" {
		t.Errorf("account = %v, want the signed-in address", ready["account"])
	}

	// And the ordinary complete endpoint turns it into an account.
	w, done := c.json(http.MethodPost, "/api/providers/oauth/complete", map[string]any{
		"flow_id": flowID,
		"name":    "proton",
		"options": map[string]string{"binary": binary, "folder": "/my-files/sand"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("complete: %d %v", w.Code, done)
	}

	account, _ := done["provider"].(map[string]any)
	if account["kind"] != "protoncli" {
		t.Fatalf("connected a %v", account["kind"])
	}
	// The session must never come back out of the vault in an API response.
	options, _ := account["options"].(map[string]any)
	if session, ok := options["session"].(string); ok && session != "" && session == "a-real-session" {
		t.Fatal("the Proton session was handed back to the browser in the clear")
	}
}

// TestProtonSignInWithoutAClientSaysSo checks that a machine with no Proton
// client is told before a flow is started, rather than being made to poll its
// way to the same news.
func TestProtonSignInWithoutAClientSaysSo(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	w, body := c.json(http.MethodPost, "/api/providers/proton/signin", map[string]any{
		"options": map[string]string{"binary": filepath.Join(t.TempDir(), "nowhere", "proton-drive")},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("start with no client: %d %v", w.Code, body)
	}
	if body["code"] != "PROTON_NO_CLIENT" {
		t.Errorf("error code = %v", body["code"])
	}
}

// TestProtonSignInIsBoundToItsSession checks that a flow another browser
// started cannot be polled or spent here. The sign-in produces a credential,
// so it belongs to the session that asked for it.
func TestProtonSignInIsBoundToItsSession(t *testing.T) {
	binary := protonStubClient(t)

	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)
	t.Setenv("SAND_PROTON_STATE_DIR", filepath.Join(t.TempDir(), "proton"))

	w, start := c.json(http.MethodPost, "/api/providers/proton/signin", map[string]any{
		"options": map[string]string{"binary": binary},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %v", w.Code, start)
	}
	flowID, _ := start["flow_id"].(string)

	// A second browser, unlocking the same vault, gets its own session — and no
	// view of somebody else's half-finished sign-in.
	other := &testClient{t: t, handler: c.handler, origin: c.origin}
	if w, _ := other.json(http.MethodPost, "/api/vault/unlock",
		map[string]any{"password": "correct horse battery staple"}); w.Code != http.StatusOK {
		t.Fatalf("unlock in a second session: %d", w.Code)
	}

	if w, body := other.json(http.MethodGet, "/api/providers/oauth/"+flowID, nil); w.Code != http.StatusNotFound {
		t.Errorf("another session read the flow: %d %v", w.Code, body)
	}
	// And above all cannot spend it: the sign-in produces a credential, and it
	// belongs to whoever asked for it.
	w, body := other.json(http.MethodPost, "/api/providers/oauth/complete", map[string]any{
		"flow_id": flowID, "name": "stolen", "options": map[string]string{},
	})
	if w.Code != http.StatusNotFound {
		t.Errorf("another session spent the flow: %d %v", w.Code, body)
	}
}
