package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The Proton backend is a conversation with somebody else's binary, so what
// there is to test is the conversation: that the right commands are run, that
// what they print is read correctly, that a missing part is told from a broken
// account, and — the part with teeth — that the session is put on disk only for
// as long as a command takes and is picked back up when Proton rotates it.
//
// The stand-in below is a real client in every respect that matters here: it
// keeps files in a directory, refuses to do anything without a staged session,
// and answers in the shapes the SDK prints. What it is not is Proton, so
// nothing here can prove SAND talks to Proton correctly — only that it talks to
// the client the way the client's own source says it should be talked to.

// protonCLIStub writes a stand-in client and returns its path and the directory
// standing in for the account's Drive.
func protonCLIStub(t *testing.T) (binary, remote string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in client is a shell script")
	}

	dir := t.TempDir()
	remote = filepath.Join(dir, "drive")
	if err := os.MkdirAll(filepath.Join(remote, "my-files"), 0o700); err != nil {
		t.Fatalf("creating the stand-in drive: %v", err)
	}

	binary = filepath.Join(dir, "proton-drive")
	script := strings.ReplaceAll(protonCLIStubScript, "@@REMOTE@@", remote)
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the stand-in client: %v", err)
	}
	return binary, remote
}

// protonCLIStubScript stands in for `proton-drive`.
//
// It refuses every command unless the session file is where the client would
// look for it, which is what makes the staging tests mean anything: a backend
// that forgot to stage would fail every test here rather than quietly passing
// the ones that do not mention sessions.
const protonCLIStubScript = `#!/bin/sh
set -e
REMOTE="@@REMOTE@@"
SESSION="$PROTON_DRIVE_CACHE_DIR/auth-session.json"

fail() { echo "$1" >&2; exit 1; }

# uid_for invents a node UID for a name, in the shape the real client insists
# on: two 22-character words either side of a ~. The shape matters — the client
# decides a path segment is a UID by matching that pattern, and a segment that
# misses it is taken for a name and looked up the slow way.
uid_for() { h=$(printf '%s' "$1" | md5sum | cut -c1-22); printf '%s~%s' "$h" "$h"; }

node_json() {
	# $1 path on disk, $2 name
	if [ -d "$1" ]; then
		printf '{"uid":"%s","name":{"ok":true,"value":"%s"},"type":"folder","ownedBy":{"email":"someone@proton.me"}}' "$(uid_for "$2")" "$2"
	else
		size=$(wc -c < "$1" | tr -d ' ')
		printf '{"uid":"%s","name":{"ok":true,"value":"%s"},"type":"file","activeRevision":{"claimedSize":%s},"ownedBy":{"email":"someone@proton.me"}}' "$(uid_for "$2")" "$2" "$size"
	fi
}

# resolve turns a path into a file on disk, understanding a trailing node UID
# the way the client does — a UID is looked up directly, a name is walked to.
# Every resolution is recorded, so a test can tell which of the two happened
# and prove the folder walk was skipped rather than assume it.
resolve() {
	last="${1##*/}"
	case "$last" in
		*~*)
			printf 'uid %s\n' "$last" >> "$REMOTE/.calls"
			# A UID the test has declared dead stands for a node removed since
			# it was cached.
			if [ -f "$REMOTE/.dead-uids" ] && grep -qF "$last" "$REMOTE/.dead-uids"; then
				return 1
			fi
			for f in "$REMOTE/my-files/sand"/* "$REMOTE/trash"/*; do
				[ -e "$f" ] || continue
				if [ "$(uid_for "$(basename "$f")")" = "$last" ]; then printf '%s' "$f"; return 0; fi
			done
			return 1
			;;
		*)
			printf 'name %s\n' "$1" >> "$REMOTE/.calls"
			t="$REMOTE/$(echo "$1" | sed 's|^/||')"
			[ -e "$t" ] || return 1
			printf '%s' "$t"
			;;
	esac
}

if [ "$1" = "auth" ] && [ "$2" = "login" ]; then
	printf '{"signInUrl":"https://account.proton.me/authorize?stub"}\n'
	if [ -f "$REMOTE/.next-session" ]; then
		cat "$REMOTE/.next-session" > "$SESSION"
	else
		printf 'stub-session-v1' > "$SESSION"
	fi
	exit 0
fi

[ "$PROTON_DRIVE_CREDENTIALS_STORE" = "unsafe_file" ] || fail "credentials store not set to a file"
[ -f "$SESSION" ] || fail "You need to login first"

# A rotation, when the test asked for one: the client rewrites the session as
# it uses it, and SAND has to notice. The request arrives as a file rather than
# an environment variable because SAND builds the client's environment rather
# than passing its own through — which is the point of another test here.
if [ -f "$REMOTE/.rotate-to" ]; then cat "$REMOTE/.rotate-to" > "$SESSION"; fi

group="$1"; shift
[ "$group" = "filesystem" ] || fail "unknown command group $group"
action="$1"; shift

# Drop the flags; the paths are what is being checked here.
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
		target="$(resolve "$1")" || fail "ValidationError: Node not found: $1"
		node_json "$target" "$(basename "$target")"
		printf '\n'
		;;
	list)
		printf 'list %s\n' "$1" >> "$REMOTE/.calls"
		target="$REMOTE/$(echo "$1" | sed 's|^/||')"
		[ -d "$target" ] || fail "ValidationError: Node not found: $1"
		printf '[\n'
		first=1
		for entry in "$target"/*; do
			[ -e "$entry" ] || continue
			[ $first -eq 1 ] || printf ',\n'
			first=0
			node_json "$entry" "$(basename "$entry")"
		done
		printf '\n]\n'
		;;
	create-folder)
		parent="$REMOTE/$(echo "$1" | sed 's|^/||')"
		[ -d "$parent" ] || fail "ValidationError: Node not found: $1"
		mkdir "$parent/$2"
		;;
	upload)
		parent="$REMOTE/$(echo "$2" | sed 's|^/||')"
		[ -d "$parent" ] || fail "ValidationError: Node not found: $2"
		cp "$1" "$parent/$(basename "$1")"
		;;
	download)
		target="$(resolve "$1")" || fail "ValidationError: Node not found: $1"
		# Named after the node, not after what was asked for — which is what
		# makes a download by UID land under the part's own name.
		cp "$target" "$2/$(basename "$target")"
		;;
	trash)
		target="$(resolve "$1")" || fail "ValidationError: Node not found: $1"
		mkdir -p "$REMOTE/trash"
		mv "$target" "$REMOTE/trash/$(basename "$target")"
		printf '[\n{"uid":"%s","ok":true}\n]\n' "$(basename "$target")"
		;;
	delete)
		# The real client refuses a live path outright, and this refuses it the
		# same way. A stand-in that accepted one is why the tests passed while
		# the account said "You can permanently delete items only from trash."
		case "$1" in
			/trash/*) ;;
			*) fail "You can permanently delete items only from trash. Trash your files first." ;;
		esac
		name="$(basename "$1")"
		[ -e "$REMOTE/trash/$name" ] || fail "ValidationError: Trashed node not found"
		# A refusal the client reports per node — and exits 0 for, which is the
		# trap this arrangement exists to catch.
		if [ -f "$REMOTE/.refuse-delete" ]; then
			printf '[\n{"uid":"%s","ok":false,"error":{}}\n]\n' "$name"
			exit 0
		fi
		rm -rf "$REMOTE/trash/$name"
		printf '[\n{"uid":"%s","ok":true}\n]\n' "$name"
		;;
	*) fail "unknown action $action" ;;
esac
`

// protonCLITestProvider builds a backend pointed at a stand-in client, already
// signed in.
func protonCLITestProvider(t *testing.T, options map[string]string) (*protonCLIProvider, string) {
	t.Helper()
	binary, remote := protonCLIStub(t)

	opts := map[string]string{
		"binary":    binary,
		"folder":    "/my-files/sand",
		"state_dir": filepath.Join(t.TempDir(), "state"),
		"session":   "stub-session-v1",
	}
	for k, v := range options {
		opts[k] = v
	}

	p, err := newProtonCLIProvider(Config{ID: "acct", Kind: KindProtonCLI, Options: opts})
	if err != nil {
		t.Fatalf("newProtonCLIProvider: %v", err)
	}
	return p.(*protonCLIProvider), remote
}

// TestProtonCLIRoundTrip is the whole object-store surface against a client
// that behaves: a folder made on demand, a part stored, found, read back,
// listed and removed.
func TestProtonCLIRoundTrip(t *testing.T) {
	p, remote := protonCLITestProvider(t, nil)
	ctx := context.Background()

	// The folder does not exist yet, so this is also the test that connecting
	// an account creates one rather than failing.
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := os.Stat(filepath.Join(remote, "my-files", "sand")); err != nil {
		t.Fatalf("Ping did not create the folder: %v", err)
	}

	part := []byte("encrypted part bytes")
	if err := p.Put(ctx, "abc-c0000000-p1.sand", part); err != nil {
		t.Fatalf("Put: %v", err)
	}

	info, err := p.Stat(ctx, "abc-c0000000-p1.sand")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(part)) {
		t.Fatalf("Stat size = %d, want %d", info.Size, len(part))
	}
	if info.Key != "abc-c0000000-p1.sand" {
		t.Fatalf("Stat key = %q", info.Key)
	}

	got, err := p.Get(ctx, "abc-c0000000-p1.sand")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(part) {
		t.Fatalf("Get returned %q, want %q", got, part)
	}

	listed, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Key != "abc-c0000000-p1.sand" {
		t.Fatalf("List = %+v", listed)
	}

	if err := p.Delete(ctx, "abc-c0000000-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Stat(ctx, "abc-c0000000-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
	}
}

// TestProtonCLIMissingObject checks the one distinction an object store cannot
// get wrong: a part that is not there, versus an account that is not working.
// The vault repairs the first and gives up on the second.
func TestProtonCLIMissingObject(t *testing.T) {
	p, _ := protonCLITestProvider(t, nil)
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if _, err := p.Get(ctx, "missing.sand"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
	if _, err := p.Stat(ctx, "missing.sand"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat = %v, want ErrNotFound", err)
	}
	// Deleting something already gone is the operation succeeding, not failing:
	// the interface promises Delete is idempotent, and a sweep that retries a
	// half-finished delete depends on it.
	if err := p.Delete(ctx, "missing.sand"); err != nil {
		t.Fatalf("Delete of a missing key = %v, want nil", err)
	}
}

// TestProtonCLIDeleteTrashesThenPurges is the shape Proton actually requires.
// Its client refuses to permanently delete a live file — "You can permanently
// delete items only from trash" — so a part is trashed and then purged, and a
// part left in the trash goes on spending the account's quota.
func TestProtonCLIDeleteTrashesThenPurges(t *testing.T) {
	p, remote := protonCLITestProvider(t, nil)
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := p.Put(ctx, "part.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := p.Delete(ctx, "part.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(remote, "my-files", "sand", "part.sand")); !os.IsNotExist(err) {
		t.Error("the part is still in the folder")
	}
	// And gone from the trash too. Trashing alone would take it out of SAND's
	// folder and leave it costing the account until somebody emptied the trash
	// by hand.
	if _, err := os.Stat(filepath.Join(remote, "trash", "part.sand")); !os.IsNotExist(err) {
		t.Error("the part was left sitting in the Proton trash")
	}
}

// TestProtonCLIDeletePurgesAPartLeftInTheTrash checks that a delete interrupted
// after the trash step finishes the job next time, rather than leaving the part
// spending quota for good.
func TestProtonCLIDeletePurgesAPartLeftInTheTrash(t *testing.T) {
	p, remote := protonCLITestProvider(t, nil)
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// The state a half-finished delete leaves: out of the folder, in the trash.
	if err := os.MkdirAll(filepath.Join(remote, "trash"), 0o700); err != nil {
		t.Fatalf("preparing the trash: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remote, "trash", "part.sand"), []byte("x"), 0o600); err != nil {
		t.Fatalf("preparing the trash: %v", err)
	}

	if err := p.Delete(ctx, "part.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(remote, "trash", "part.sand")); !os.IsNotExist(err) {
		t.Error("a part already in the trash was not purged")
	}
}

// TestProtonCLIDeleteNoticesARefusal is the one that would otherwise pass
// silently. Unlike upload, the trash and delete commands report a refusal as an
// "ok": false in their JSON and exit zero anyway — so a backend reading the
// exit status alone would tell the vault a part was deleted while it still sits
// on the account.
func TestProtonCLIDeleteNoticesARefusal(t *testing.T) {
	p, remote := protonCLITestProvider(t, nil)
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := p.Put(ctx, "part.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.WriteFile(filepath.Join(remote, ".refuse-delete"), nil, 0o600); err != nil {
		t.Fatalf("arming the refusal: %v", err)
	}

	err := p.Delete(ctx, "part.sand")
	if err == nil {
		t.Fatal("a refused delete was reported as success")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Delete error = %v, want it to say the client refused", err)
	}
}

// TestProtonCLINodeResults checks the parsing on its own, including the shape
// the client actually emits — an Error serializes to {} through JSON.stringify,
// so there is no message to report, only the refusal.
func TestProtonCLINodeResults(t *testing.T) {
	if err := protonCLINodeResults(`[` + "\n" + `{"uid":"a","ok":true}` + "\n" + `]`); err != nil {
		t.Errorf("all-ok results reported an error: %v", err)
	}
	if err := protonCLINodeResults(""); err != nil {
		t.Errorf("empty output reported an error: %v", err)
	}
	if err := protonCLINodeResults(`[{"uid":"a","ok":true},{"uid":"b","ok":false,"error":{}}]`); err == nil {
		t.Error("a refusal among the results was not noticed")
	}
	// Silence must not read as success.
	if err := protonCLINodeResults("not json at all"); err == nil {
		t.Error("unparseable output was taken for success")
	}
}

// protonCLICalls is every path the stand-in client was asked to resolve, and
// how — "uid …" or "name …" — which is what makes the caching testable rather
// than merely asserted.
func protonCLICalls(t *testing.T, remote string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(remote, ".calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the call log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func protonCLICountCalls(calls []string, prefix string) int {
	n := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// TestProtonCLIReadsByUIDAfterTheFirst is the point of the cache. Addressing a
// part by name makes the client walk the folder and decrypt every name in it,
// twice, on every single read; addressing it by UID does not. One listing
// answers for every part at once, so the walk should happen once and never
// again.
func TestProtonCLIReadsByUIDAfterTheFirst(t *testing.T) {
	p, remote := protonCLITestProvider(t, nil)
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	for _, key := range []string{"one.sand", "two.sand", "three.sand"} {
		if err := p.Put(ctx, key, []byte(key)); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}

	// Everything above is setup; the reads are what is being measured.
	if err := os.Remove(filepath.Join(remote, ".calls")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clearing the call log: %v", err)
	}

	for _, key := range []string{"one.sand", "two.sand", "three.sand"} {
		got, err := p.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if string(got) != key {
			t.Fatalf("Get %s returned %q", key, got)
		}
	}

	calls := protonCLICalls(t, remote)
	// One listing, to learn where everything is. Not one per read.
	if listings := protonCLICountCalls(calls, "list "); listings != 1 {
		t.Errorf("the folder was listed %d times for three reads, want 1: %v", listings, calls)
	}
	// And every download addressed by UID, so the client never walked.
	if byUID := protonCLICountCalls(calls, "uid "); byUID != 3 {
		t.Errorf("%d of 3 reads went by UID: %v", byUID, calls)
	}
	if byName := protonCLICountCalls(calls, "name /my-files/sand/"); byName != 0 {
		t.Errorf("%d reads still walked the folder by name: %v", byName, calls)
	}
}

// TestProtonCLIStaleUIDFallsBackToTheName checks the failure this cache makes
// possible: a UID that named a node somebody has since removed. The read must
// find the part anyway, by the name that cannot go stale.
func TestProtonCLIStaleUIDFallsBackToTheName(t *testing.T) {
	p, remote := protonCLITestProvider(t, nil)
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := p.Put(ctx, "part.sand", []byte("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Warm the cache, then declare the UID it learned dead.
	if _, err := p.List(ctx, ""); err != nil {
		t.Fatalf("List: %v", err)
	}
	uid := p.uids["part.sand"]
	if uid == "" {
		t.Fatal("listing did not fill the UID cache")
	}
	if err := os.WriteFile(filepath.Join(remote, ".dead-uids"), []byte(uid), 0o600); err != nil {
		t.Fatalf("killing the uid: %v", err)
	}

	got, err := p.Get(ctx, "part.sand")
	if err != nil {
		t.Fatalf("Get with a stale UID: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("Get returned %q", got)
	}
	// And the dead UID is not kept to fail again on the next read.
	if p.uids["part.sand"] == uid {
		t.Error("the stale UID was kept after it failed")
	}
}

// TestProtonCLIDeleteForgetsTheUID checks that removing a part takes its UID
// with it. A UID left behind would send the next read after a node in the bin.
func TestProtonCLIDeleteForgetsTheUID(t *testing.T) {
	p, _ := protonCLITestProvider(t, nil)
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := p.Put(ctx, "part.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := p.List(ctx, ""); err != nil {
		t.Fatalf("List: %v", err)
	}
	if p.uids["part.sand"] == "" {
		t.Fatal("listing did not fill the UID cache")
	}

	if err := p.Delete(ctx, "part.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if uid, ok := p.uids["part.sand"]; ok {
		t.Errorf("the UID %q outlived the part", uid)
	}
}

// TestProtonCLIListOfMissingFolderIsEmpty checks that an account connected but
// never written to reports nothing stored rather than an error. A folder that
// is not there yet is not a broken account.
func TestProtonCLIListOfMissingFolderIsEmpty(t *testing.T) {
	p, _ := protonCLITestProvider(t, nil)

	listed, err := p.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List before the folder exists: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("List = %+v, want nothing", listed)
	}
}

// TestProtonCLISessionIsNotLeftOnDisk is the point of the whole staging
// arrangement: the session is in the vault, and on disk only while a command
// runs.
func TestProtonCLISessionIsNotLeftOnDisk(t *testing.T) {
	p, _ := protonCLITestProvider(t, nil)
	ctx := context.Background()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := os.Stat(p.sessionPath()); !os.IsNotExist(err) {
		t.Fatalf("the session was left at %s after the command finished", p.sessionPath())
	}

	if err := p.Put(ctx, "part.sand", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(p.sessionPath()); !os.IsNotExist(err) {
		t.Fatalf("the session was left behind after a Put")
	}
}

// TestProtonCLIRotatedSessionReachesTheVault checks the half of the staging
// that would otherwise fail silently and late: Proton rotates the session as it
// is used, and a rotation that is not written back leaves the account working
// until the token it still holds expires, then signed out for good.
func TestProtonCLIRotatedSessionReachesTheVault(t *testing.T) {
	p, remote := protonCLITestProvider(t, nil)

	var rotated []map[string]string
	p.OnCredentialChange(func(update map[string]string) {
		rotated = append(rotated, update)
	})

	if err := os.WriteFile(filepath.Join(remote, ".rotate-to"), []byte("stub-session-v2"), 0o600); err != nil {
		t.Fatalf("asking the stand-in client to rotate: %v", err)
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if len(rotated) == 0 {
		t.Fatal("the rotated session was never published to the vault")
	}
	if got := rotated[len(rotated)-1]["session"]; got != "stub-session-v2" {
		t.Fatalf("published session = %q, want the rotated one", got)
	}
	// And the backend goes on using the new one rather than staging the old
	// one again on the next call.
	if p.session != "stub-session-v2" {
		t.Fatalf("in-memory session = %q, want the rotated one", p.session)
	}
}

// TestProtonCLIWithoutSession checks that an account nobody has signed in says
// so, and says where to. This is the message on a freshly connected account, so
// it has to name the way out.
func TestProtonCLIWithoutSession(t *testing.T) {
	p, _ := protonCLITestProvider(t, map[string]string{"session": ""})

	err := p.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping with no session succeeded")
	}
	if !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("Ping error = %v, want it to say the account is not signed in", err)
	}
	if !strings.Contains(err.Error(), "sand remote proton login") {
		t.Fatalf("Ping error = %v, want it to name the command that fixes it", err)
	}
}

// TestProtonCLIMissingBinary checks the message somebody gets on a machine
// where SAND was installed without Proton's client — the reason the backend is
// offered everywhere rather than hidden where it cannot run.
func TestProtonCLIMissingBinary(t *testing.T) {
	p, _ := protonCLITestProvider(t, map[string]string{
		"binary": filepath.Join(t.TempDir(), "nowhere", "proton-drive"),
	})

	err := p.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping with no client succeeded")
	}
	if !strings.Contains(err.Error(), "synced folder") {
		t.Fatalf("error = %v, want it to offer the synced-folder alternative", err)
	}
}

// TestProtonCLIRejectsKeysThatEscapeTheFolder checks that a key cannot reach
// out of the folder the account was pointed at. Keys are flat filenames, so one
// with a separator in it never came from the vault.
func TestProtonCLIRejectsKeysThatEscapeTheFolder(t *testing.T) {
	p, _ := protonCLITestProvider(t, nil)
	ctx := context.Background()

	for _, key := range []string{"../escape.sand", "nested/part.sand", "", ".", ".."} {
		if err := p.Put(ctx, key, []byte("x")); err == nil {
			t.Fatalf("Put accepted key %q", key)
		}
		if _, err := p.Get(ctx, key); err == nil {
			t.Fatalf("Get accepted key %q", key)
		}
		if err := p.Delete(ctx, key); err == nil {
			t.Fatalf("Delete accepted key %q", key)
		}
	}
}

// TestProtonCLIMeasureUsage checks the counted figure, which is the only one
// available: Proton's client has no quota command, so an account's usage is the
// sum of a listing exactly as a bucket's is.
func TestProtonCLIMeasureUsage(t *testing.T) {
	p, _ := protonCLITestProvider(t, nil)
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := p.Put(ctx, "one.sand", []byte("12345")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := p.Put(ctx, "two.sand", []byte("678")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	usage, err := p.MeasureUsage(ctx)
	if err != nil {
		t.Fatalf("MeasureUsage: %v", err)
	}
	if usage.Used != 8 {
		t.Fatalf("Used = %d, want 8", usage.Used)
	}
	if !usage.Measured {
		t.Fatal("usage should be labelled as counted rather than reported")
	}
	// Nothing here knows the plan's size, and a guessed total would be drawn as
	// a bar somebody would believe.
	if usage.Total != 0 {
		t.Fatalf("Total = %d, want no quota claimed", usage.Total)
	}
}

// TestProtonCLIAccount checks that a freshly connected account can name itself.
func TestProtonCLIAccount(t *testing.T) {
	p, _ := protonCLITestProvider(t, nil)

	who, err := p.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if who != "someone@proton.me" {
		t.Fatalf("Account = %q", who)
	}
}

// TestProtonCLICreatesNestedFolders checks that a folder several levels below
// the account's own files is made in full, rather than an account refusing to
// connect until somebody makes the parents by hand.
func TestProtonCLICreatesNestedFolders(t *testing.T) {
	p, remote := protonCLITestProvider(t, map[string]string{
		"folder": "/my-files/backups/sand/parts",
	})

	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := os.Stat(filepath.Join(remote, "my-files", "backups", "sand", "parts")); err != nil {
		t.Fatalf("nested folder not created: %v", err)
	}
}

// TestProtonCLIFolderNormalization checks that a folder typed the way people
// type folders reaches the client in the one shape it accepts.
func TestProtonCLIFolderNormalization(t *testing.T) {
	cases := map[string]string{
		"/my-files/sand":   "/my-files/sand",
		"my-files/sand":    "/my-files/sand",
		"/my-files/sand/":  "/my-files/sand",
		" /my-files/sand ": "/my-files/sand",
		"//my-files//sand": "/my-files/sand",
		"/":                "",
		"":                 "",
	}
	for input, want := range cases {
		if got := protonCLINormalizeFolder(input); got != want {
			t.Errorf("normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestProtonCLISignIn walks the sign-in: a URL to put in front of somebody, and
// a session that comes back ready to store.
func TestProtonCLISignIn(t *testing.T) {
	binary, remote := protonCLIStub(t)
	if err := os.WriteFile(filepath.Join(remote, ".next-session"), []byte("freshly-signed-in"), 0o600); err != nil {
		t.Fatalf("priming the stand-in client: %v", err)
	}

	var shown string
	options, err := ProtonCLISignIn(context.Background(), Config{
		Kind: KindProtonCLI,
		Options: map[string]string{
			"binary":    binary,
			"state_dir": filepath.Join(t.TempDir(), "state"),
		},
	}, func(url string) { shown = url })
	if err != nil {
		t.Fatalf("ProtonCLISignIn: %v", err)
	}
	if !strings.HasPrefix(shown, "https://account.proton.me/") {
		t.Fatalf("sign-in URL = %q", shown)
	}
	if options["session"] != "freshly-signed-in" {
		t.Fatalf("session = %q", options["session"])
	}
	// The folder and the binary belong to whoever asked for the sign-in.
	// Handing them back would write this run's defaults into the account as
	// though somebody had chosen them.
	if _, ok := options["folder"]; ok {
		t.Fatalf("the sign-in returned a folder nobody chose: %+v", options)
	}
	if _, ok := options["state_dir"]; ok {
		t.Fatalf("the sign-in returned a state directory nobody chose: %+v", options)
	}
}

// TestProtonCLISignInOfAnUnsavedAccountUsesItsOwnDirectory checks that two
// accounts connected one after the other do not sign in on top of each other.
// An account has no ID until it is stored, so the directory derived from one
// cannot be used before then.
func TestProtonCLISignInOfAnUnsavedAccountUsesItsOwnDirectory(t *testing.T) {
	binary, remote := protonCLIStub(t)
	t.Setenv("SAND_PROTON_STATE_DIR", filepath.Join(t.TempDir(), "shared"))

	seen := map[string]bool{}
	for _, session := range []string{"first-account", "second-account"} {
		if err := os.WriteFile(filepath.Join(remote, ".next-session"), []byte(session), 0o600); err != nil {
			t.Fatalf("priming the stand-in client: %v", err)
		}
		options, err := ProtonCLISignIn(context.Background(), Config{
			Kind:    KindProtonCLI,
			Options: map[string]string{"binary": binary},
		}, nil)
		if err != nil {
			t.Fatalf("ProtonCLISignIn: %v", err)
		}
		if options["session"] != session {
			t.Fatalf("session = %q, want %q — the sign-in read another account's",
				options["session"], session)
		}
		seen[options["session"]] = true
	}
	if len(seen) != 2 {
		t.Fatalf("the two sign-ins produced %d distinct sessions", len(seen))
	}
}

// TestProtonCLIStateDirIsPerAccount checks the same thing one level down: the
// directory an account falls back to is named after the account.
func TestProtonCLIStateDirIsPerAccount(t *testing.T) {
	t.Setenv("SAND_PROTON_STATE_DIR", "/var/lib/sand/proton")

	first := protonCLIStateDir("aaaa")
	second := protonCLIStateDir("bbbb")
	if first == second {
		t.Fatalf("two accounts share the state directory %s", first)
	}
	if !strings.HasPrefix(first, "/var/lib/sand/proton") {
		t.Fatalf("state dir = %s, want it under the configured root", first)
	}
}

// TestProtonCLINodeParsing checks the two things read out of the client's JSON
// that are easy to get wrong: a name that is a result rather than a string, and
// a size that is the part's rather than the account's.
func TestProtonCLINodeParsing(t *testing.T) {
	var node protonCLINode
	raw := `{"name":{"ok":true,"value":"part.sand"},"type":"file",
		"activeRevision":{"claimedSize":1024},"totalStorageSize":99999,
		"ownedBy":{"email":"someone@proton.me"}}`
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if node.name() != "part.sand" {
		t.Fatalf("name = %q", node.name())
	}
	// totalStorageSize is every revision, encrypted, which is the account's
	// business. A health check comparing it against the part SAND wrote would
	// find a mismatch every time.
	if node.size() != 1024 {
		t.Fatalf("size = %d, want the part's claimed size", node.size())
	}

	// A node whose name will not decrypt still lists. It is not a part SAND
	// wrote, so it is skipped rather than guessed at.
	var broken protonCLINode
	if err := json.Unmarshal([]byte(`{"name":{"ok":false},"type":"file"}`), &broken); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if broken.name() != "" {
		t.Fatalf("undecryptable name = %q, want it skipped", broken.name())
	}
}

// TestProtonCLIRegisteredSpec checks the backend is offered, and offered
// distinguishably from the synced-folder Proton beside it — the two are one
// service and somebody has to be able to tell which is which.
func TestProtonCLIRegisteredSpec(t *testing.T) {
	spec, ok := SpecFor(KindProtonCLI)
	if !ok {
		t.Fatal("the Proton client backend is not registered")
	}
	folder, ok := SpecFor(KindProton)
	if !ok {
		t.Fatal("the Proton synced-folder backend is not registered")
	}
	if spec.Label == folder.Label {
		t.Fatalf("both Proton backends are called %q", spec.Label)
	}
	if spec.Order >= folder.Order {
		t.Fatalf("the client backend sorts below the folder one (%d vs %d)", spec.Order, folder.Order)
	}

	var session FieldSpec
	for _, field := range spec.Fields {
		if field.Key == "session" {
			session = field
		}
	}
	if !session.Secret {
		t.Fatal("the session must be a secret, or it leaves the vault in an API response")
	}

	// And it must actually be redacted, since that is what stops a session
	// reaching the browser.
	redacted := Config{Kind: KindProtonCLI, Options: map[string]string{"session": "a-real-session"}}.Redacted()
	if redacted.Options["session"] != RedactedSecret {
		t.Fatalf("session leaves the vault as %q", redacted.Options["session"])
	}
}

// TestProtonCLIEnvironIsBuiltNotInherited checks that the client is told where
// its state goes rather than left to work it out. A stray variable from
// whoever started SAND would otherwise point one account at another's session.
func TestProtonCLIEnvironIsBuiltNotInherited(t *testing.T) {
	t.Setenv("PROTON_DRIVE_CACHE_DIR", "/somewhere/else")
	t.Setenv("PROTON_DRIVE_CREDENTIALS_STORE", "keychain")

	p, _ := protonCLITestProvider(t, nil)
	env := map[string]string{}
	for _, entry := range p.environ() {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}

	if env["PROTON_DRIVE_CACHE_DIR"] != p.stateDir {
		t.Fatalf("cache dir = %q, want this account's %q", env["PROTON_DRIVE_CACHE_DIR"], p.stateDir)
	}
	if env["PROTON_DRIVE_CREDENTIALS_STORE"] != "unsafe_file" {
		t.Fatalf("credentials store = %q, want the staged file", env["PROTON_DRIVE_CREDENTIALS_STORE"])
	}
	// The service user has no home, and a client that fell back to one would
	// write outside everything the systemd unit grants it.
	if env["HOME"] != p.stateDir {
		t.Fatalf("HOME = %q, want this account's state directory", env["HOME"])
	}
}

// TestProtonCLIErrorsAreReadable checks that the client's stderr — a rule, a
// message and a stack trace — becomes one sentence. An account's status line
// has room for a sentence.
func TestProtonCLIErrorsAreReadable(t *testing.T) {
	stderr := "===============================================\n" +
		"Trace: ValidationError: Node not found: /my-files/sand/x.sand\n" +
		"    at getNode (/app/paths.ts:317:15)\n" +
		"    at async action (/app/commandFileSystemInfo.ts:9:5)\n"

	err := protonCLIError([]string{"filesystem", "info"}, stderr, fmt.Errorf("exit status 1"))
	if !protonCLIIsNotFound(err) {
		t.Fatalf("error = %v, want it recognised as a missing node", err)
	}
	if strings.Contains(err.Error(), "at getNode") {
		t.Fatalf("the stack trace reached the message: %v", err)
	}

	// Only the client's own phrasing counts as "not there". Delete reports a
	// missing object as success, so a failure read as not-found would tell the
	// vault a part was deleted while it sits on the account still.
	for _, stderr := range []string{
		"Trace: AccountApiError: session not found\n",
		"Trace: Error: getaddrinfo ENOTFOUND drive.proton.me\n",
		"Trace: Error: volume not found for this account\n",
	} {
		if protonCLIIsNotFound(protonCLIError([]string{"filesystem", "delete"}, stderr,
			fmt.Errorf("exit status 1"))) {
			t.Errorf("a failure was mistaken for a missing object: %q", stderr)
		}
	}

	signedOut := protonCLIError([]string{"filesystem", "list"},
		"Trace: AuthRequiredError: You need to login first\n", fmt.Errorf("exit status 1"))
	if !strings.Contains(signedOut.Error(), "signed out") {
		t.Fatalf("error = %v, want it to say the account is signed out", signedOut)
	}
}
