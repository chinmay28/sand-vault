"""
End-to-end tests for SAND's connected-cloud mode, driving the real binary.

Two surfaces are covered:
  * the HTTP API the file browser talks to
  * the `sand` CLI subcommands (vault / remote / put / get / ls / check)

Cloud accounts are stood up as local-folder providers.  That is a real backend
going through the same scatter/gather code an S3 bucket or a Drive account
would, so these tests exercise the whole path without needing credentials.
"""
import hashlib
import os
import shutil
import subprocess
import urllib.parse

import pytest
import requests


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def upload(session, server, name, content, path="/", overwrite=False, accounts=None):
    data = {"path": path, "overwrite": "true" if overwrite else "false"}
    if accounts:
        data["accounts"] = list(accounts)
    return session.post(
        f"{server}/api/files",
        files=[("files[]", (name, content, "application/octet-stream"))],
        data=data,
        headers={"Origin": server},
        timeout=120,
    )


def account_ids(session, server, *names):
    """The ids of accounts connected under the given names.

    Needed by any test that goes looking for a part on disk. Placement picks
    accounts at random, the browser suite shares this vault and connects
    accounts of its own — at paths of its own choosing — so a part can perfectly
    well land somewhere `clouds(name)` does not describe. Naming the accounts on
    the way in is what makes the file's location knowable on the way out.
    """
    r = session.get(f"{server}/api/providers", timeout=30)
    assert r.status_code == 200, r.text
    by_name = {p["name"]: p["id"] for p in r.json()["providers"]}
    missing = [n for n in names if n not in by_name]
    assert not missing, f"accounts not connected: {missing}"
    return [by_name[n] for n in names]


def listing(session, server, path="/"):
    r = session.get(f"{server}/api/files", params={"path": path}, timeout=30)
    assert r.status_code == 200, r.text
    return r.json()


def find_file(session, server, name, path="/"):
    for f in listing(session, server, path)["files"]:
        if f["name"] == name:
            return f
    raise AssertionError(f"{name} not found in {path}")


def cli(sand_bin, vault_dir, *args, password="e2e-test-passphrase", check=True):
    """Run a sand subcommand against the CLI's own vault."""
    env = dict(os.environ)
    env["SAND_PASSWORD"] = password
    result = subprocess.run(
        [sand_bin, "--vault", os.path.join(vault_dir, "cli-vault.sand"), *args],
        capture_output=True, text=True, env=env,
    )
    if check:
        assert result.returncode == 0, f"{args}\nstdout: {result.stdout}\nstderr: {result.stderr}"
    return result


# ---------------------------------------------------------------------------
# Vault lifecycle over HTTP
# ---------------------------------------------------------------------------

class TestVaultLifecycle:
    def test_status_reports_an_unlocked_vault(self, server, unlocked):
        status = unlocked.get(f"{server}/api/vault", timeout=10).json()
        assert status["initialized"] is True
        assert status["unlocked"] is True
        assert status["policy"] in ("strict", "redundant")

    def test_anonymous_client_sees_a_locked_vault(self, server, unlocked):
        anon = requests.Session()
        status = anon.get(f"{server}/api/vault", timeout=10).json()
        assert status["initialized"] is True
        # No session cookie means no view inside, even though the process has
        # the keys in memory for someone else.
        assert status["unlocked"] is False
        assert "stats" not in status or status.get("stats") is None

    def test_anonymous_client_cannot_list_files(self, server, unlocked):
        anon = requests.Session()
        r = anon.get(f"{server}/api/files", params={"path": "/"}, timeout=10)
        assert r.status_code == 401
        assert r.json()["code"] == "LOCKED"

    def test_three_accounts_are_connected(self, server, unlocked):
        providers = unlocked.get(f"{server}/api/providers", timeout=30).json()["providers"]
        assert len(providers) >= 3
        assert all(p["online"] for p in providers), providers

    def test_provider_specs_describe_every_backend(self, server):
        specs = requests.get(f"{server}/api/providers/specs", timeout=10).json()["specs"]
        kinds = {s["kind"] for s in specs}
        assert {"local", "s3", "webdav", "gdrive", "dropbox",
                "onedrive", "box", "proton"} <= kinds
        for spec in specs:
            assert spec["label"] and spec["description"]
            assert isinstance(spec["fields"], list)

    def test_specs_say_which_backends_are_connected_by_signing_in(self, server):
        specs = requests.get(f"{server}/api/providers/specs", timeout=10).json()["specs"]
        by_kind = {s["kind"]: s for s in specs}

        for kind in ("gdrive", "onedrive", "dropbox", "box"):
            oauth = by_kind[kind].get("oauth")
            assert oauth, f"{kind} should offer a sign-in"
            assert oauth["sign_in_label"]
            # No client credentials are configured for the test server, so the
            # dialog has to collect one.
            assert oauth["configured"] is False

        assert by_kind["local"].get("oauth") is None
        assert by_kind["proton"].get("oauth") is None

        # The token endpoints and app credentials stay on the server.
        raw = requests.get(f"{server}/api/providers/specs", timeout=10).text
        assert "oauth2.googleapis.com" not in raw
        assert "client_secret_env" not in raw

    def test_sign_in_needs_a_session_and_a_backend_that_supports_it(self, server, unlocked):
        anonymous = requests.post(f"{server}/api/providers/oauth/start",
                                  json={"kind": "gdrive"}, timeout=10)
        assert anonymous.status_code == 401

        wrong_kind = unlocked.post(f"{server}/api/providers/oauth/start",
                                   json={"kind": "local"}, timeout=10)
        assert wrong_kind.status_code == 400
        assert "signing in" in wrong_kind.json()["error"]

        no_app = unlocked.post(f"{server}/api/providers/oauth/start",
                               json={"kind": "gdrive"}, timeout=10)
        assert no_app.status_code == 400
        assert no_app.json()["code"] == "OAUTH_NO_CLIENT"

    def test_sign_in_hands_back_a_consent_url_and_a_flow(self, server, unlocked):
        started = unlocked.post(f"{server}/api/providers/oauth/start", timeout=10, json={
            "kind": "gdrive",
            "client_id": "test-client.apps.googleusercontent.com",
            "client_secret": "test-secret",
            "redirect_uri": f"{server}/api/providers/oauth/callback",
        })
        assert started.status_code == 200, started.text
        body = started.json()

        auth = urllib.parse.urlparse(body["auth_url"])
        query = urllib.parse.parse_qs(auth.query)
        assert auth.netloc == "accounts.google.com"
        assert query["client_id"] == ["test-client.apps.googleusercontent.com"]
        assert query["redirect_uri"] == [f"{server}/api/providers/oauth/callback"]
        # Google only issues a refresh token when asked for offline access.
        assert query["access_type"] == ["offline"]
        assert query["code_challenge_method"] == ["S256"]

        status = unlocked.get(f"{server}/api/providers/oauth/{body['flow_id']}", timeout=10)
        assert status.json()["status"] == "pending"

        # A redirect carrying someone else's state is not ours to act on.
        forged = requests.get(f"{server}/api/providers/oauth/callback",
                              params={"code": "x", "state": "forged"}, timeout=10)
        assert forged.status_code == 400
        assert "expired" in forged.text

        # The provider declining is reported against the flow rather than lost.
        denied = requests.get(f"{server}/api/providers/oauth/callback", timeout=10, params={
            "state": query["state"][0],
            "error": "access_denied",
            "error_description": "the account holder said no",
        })
        assert denied.status_code == 200
        assert "SIGN-IN FAILED" in denied.text

        status = unlocked.get(f"{server}/api/providers/oauth/{body['flow_id']}", timeout=10).json()
        assert status["status"] == "error"
        assert "said no" in status["error"]


# ---------------------------------------------------------------------------
# Storing and retrieving through the API
# ---------------------------------------------------------------------------

class TestStoreAndRetrieve:
    def test_upload_scatters_three_parts_over_three_accounts(self, server, unlocked):
        r = upload(unlocked, server, "scatter.bin", os.urandom(50_000))
        assert r.status_code == 201, r.text

        entry = r.json()["results"][0]["file"]
        assert len(entry["shards"]) == 3

        accounts = {s["provider_id"] for s in entry["shards"]}
        assert len(accounts) == 3, "each part must live on a different account"

    def test_download_returns_the_original_bytes(self, server, unlocked):
        payload = os.urandom(120_000)
        r = upload(unlocked, server, "exact.bin", payload)
        assert r.status_code == 201, r.text
        file_id = r.json()["results"][0]["file"]["id"]

        got = unlocked.get(f"{server}/api/files/{file_id}/content",
                           params={"download": "1"}, timeout=120)
        assert got.status_code == 200
        assert hashlib.sha256(got.content).hexdigest() == hashlib.sha256(payload).hexdigest()

    def test_stored_parts_are_not_the_plaintext(self, server, unlocked, clouds):
        marker = b"CANARY-PLAINTEXT-MARKER-9f3a"
        r = upload(unlocked, server, "canary.txt", marker + os.urandom(4000))
        assert r.status_code == 201, r.text

        # Walk every fake cloud account: none of them may contain the marker,
        # nor the filename, in the clear.
        root = os.path.dirname(clouds("cloud-one"))
        checked = 0
        for dirpath, _, filenames in os.walk(root):
            for filename in filenames:
                assert "canary" not in filename.lower(), f"filename leaked in {filename}"
                try:
                    with open(os.path.join(dirpath, filename), "rb") as fh:
                        blob = fh.read()
                except FileNotFoundError:
                    # A manifest backup from an earlier upload lands as a
                    # `.sand-tmp-*` file and is renamed into place; the walk can
                    # see one and reach it after the rename. What it becomes is
                    # checked on the next pass around this loop.
                    continue
                assert marker not in blob, f"plaintext found in {filename}"
                checked += 1
        assert checked > 0, "no shards were written"

    def test_file_survives_an_account_going_offline(self, server, unlocked, clouds, tmp_path):
        payload = os.urandom(80_000)
        # Pinned, so that the directory this moves away is really the one
        # holding part 1. Against an account connected elsewhere, clouds(name)
        # makes an empty folder and renaming it takes nothing offline — the test
        # would pass without having tested anything.
        accounts = account_ids(unlocked, server, "cloud-one", "cloud-two", "cloud-three")
        r = upload(unlocked, server, "resilient.bin", payload, accounts=accounts)
        entry = r.json()["results"][0]["file"]
        file_id = entry["id"]

        # Take the account holding part 1 offline by moving its directory away.
        part1 = next(s for s in entry["shards"] if s["part"] == 1)
        account_root = clouds(part1["provider_name"])
        stashed = str(tmp_path / "offline-account")
        os.rename(account_root, stashed)
        try:
            got = unlocked.get(f"{server}/api/files/{file_id}/content", timeout=120)
            assert got.status_code == 200, got.text
            assert got.content == payload
        finally:
            # The account may have been recreated underneath us: a manifest
            # backup running in the background makes its own directory. Put the
            # parts back into whatever is there rather than renaming onto it.
            if os.path.isdir(account_root):
                for name in os.listdir(stashed):
                    shutil.move(os.path.join(stashed, name), os.path.join(account_root, name))
                shutil.rmtree(stashed, ignore_errors=True)
            else:
                os.rename(stashed, account_root)

    def test_health_flags_a_missing_part(self, server, unlocked, clouds, tmp_path):
        # Pinned to the three accounts this suite created, because the test then
        # goes and deletes a part off disk: left to pick at random, placement can
        # land it on an account the browser suite connected somewhere else
        # entirely, and clouds(name) would describe the wrong folder.
        accounts = account_ids(unlocked, server, "cloud-one", "cloud-two", "cloud-three")
        r = upload(unlocked, server, "damaged.bin", os.urandom(20_000), accounts=accounts)
        entry = r.json()["results"][0]["file"]
        file_id = entry["id"]

        health = unlocked.get(f"{server}/api/files/{file_id}/health", timeout=60).json()
        assert health["recoverable"] is True
        assert all(s["present"] for s in health["shards"])

        # Remove exactly one part and re-check.
        shard = entry["shards"][0]
        victim = os.path.join(clouds(shard["provider_name"]), *shard["key"].split("/"))
        assert os.path.exists(victim), victim
        os.remove(victim)

        health = unlocked.get(f"{server}/api/files/{file_id}/health", timeout=60).json()
        assert health["recoverable"] is True, "two parts should still be enough"
        assert sum(1 for s in health["shards"] if not s["present"]) == 1

    def test_name_collision_does_not_overwrite(self, server, unlocked):
        upload(unlocked, server, "collide.txt", b"first")
        r = upload(unlocked, server, "collide.txt", b"second")
        assert r.status_code == 201, r.text
        assert r.json()["results"][0]["file"]["name"] == "collide (2).txt"

    def test_overwrite_replaces_in_place(self, server, unlocked):
        upload(unlocked, server, "replaceme.txt", b"old")
        r = upload(unlocked, server, "replaceme.txt", b"new", overwrite=True)
        assert r.status_code == 201, r.text

        entry = r.json()["results"][0]["file"]
        assert entry["name"] == "replaceme.txt"

        got = unlocked.get(f"{server}/api/files/{entry['id']}/content", timeout=60)
        assert got.content == b"new"

    def test_delete_removes_the_parts_from_every_account(self, server, unlocked, clouds):
        # Pinned: this asserts the parts are *absent* afterwards, and a path
        # pointing at an account connected somewhere else is absent whether the
        # delete worked or not.
        accounts = account_ids(unlocked, server, "cloud-one", "cloud-two", "cloud-three")
        r = upload(unlocked, server, "ephemeral.bin", os.urandom(9_000), accounts=accounts)
        entry = r.json()["results"][0]["file"]

        d = unlocked.delete(f"{server}/api/files/{entry['id']}",
                            headers={"Origin": server}, timeout=60)
        assert d.status_code == 200, d.text

        for shard in entry["shards"]:
            path = os.path.join(clouds(shard["provider_name"]), *shard["key"].split("/"))
            assert not os.path.exists(path), f"{shard['key']} survived the delete"

    def test_folders_and_navigation(self, server, unlocked):
        r = unlocked.post(f"{server}/api/folders", json={"path": "/reports/2024"},
                          headers={"Origin": server}, timeout=30)
        assert r.status_code == 201, r.text

        upload(unlocked, server, "q1.txt", b"first quarter", path="/reports/2024")

        top = listing(unlocked, server, "/reports")
        assert top["folders"] == ["2024"]

        nested = listing(unlocked, server, "/reports/2024")
        assert [f["name"] for f in nested["files"]] == ["q1.txt"]
        assert nested["parent"] == "/reports"

    def test_moving_a_folder_carries_the_files_and_not_the_parts(self, server, unlocked):
        """A folder is a path in the index, so moving one is an index change:
        the files answer to a new path and their parts never leave the accounts
        they were scattered to."""
        for path in ("/shelf/books", "/library"):
            unlocked.post(f"{server}/api/folders", json={"path": path},
                          headers={"Origin": server}, timeout=30)
        r = upload(unlocked, server, "novel.txt", b"a long story", path="/shelf/books")
        entry = r.json()["results"][0]["file"]
        before = {(s["part"], s["provider_id"], s["key"]) for s in entry["shards"]}

        # Every folder in the vault, which is what a destination picker draws.
        tree = unlocked.get(f"{server}/api/folders", timeout=30).json()["folders"]
        assert {"/", "/shelf", "/shelf/books", "/library"} <= set(tree)

        moved = unlocked.post(
            f"{server}/api/folders/move",
            json={"from": "/shelf/books", "to": "/library/books"},
            headers={"Origin": server}, timeout=60,
        )
        assert moved.status_code == 200, moved.text

        meta = unlocked.get(f"{server}/api/files/{entry['id']}", timeout=30).json()
        assert meta["path"] == "/library/books/novel.txt"
        assert {(s["part"], s["provider_id"], s["key"])
                for s in meta["file"]["shards"]} == before

        got = unlocked.get(f"{server}/api/files/{entry['id']}/content", timeout=60)
        assert got.content == b"a long story"

        # A folder cannot be moved inside itself, whatever the request says.
        refused = unlocked.post(
            f"{server}/api/folders/move",
            json={"from": "/library", "to": "/library/books/library"},
            headers={"Origin": server}, timeout=30,
        )
        assert refused.status_code >= 400

    def test_relocating_to_the_same_clouds_moves_nothing(self, server, unlocked):
        """Placement is a set of accounts, not a sequence — naming the three a
        file is already on, in a different order, is not a change."""
        r = upload(unlocked, server, "settled.txt", b"already where it belongs")
        entry = r.json()["results"][0]["file"]
        before = {(s["part"], s["provider_id"], s["key"]) for s in entry["shards"]}
        accounts = [s["provider_id"] for s in entry["shards"]][::-1]

        preview = unlocked.post(
            f"{server}/api/relocate",
            json={"id": entry["id"], "accounts": accounts, "preview": True},
            headers={"Origin": server}, timeout=60,
        )
        assert preview.status_code == 200, preview.text
        assert preview.json()["moves"] == 0
        assert preview.json()["unchanged"] == 1

        done = unlocked.post(
            f"{server}/api/relocate",
            json={"id": entry["id"], "accounts": accounts},
            headers={"Origin": server}, timeout=120,
        )
        assert done.status_code == 200, done.text
        assert done.json()["parts_moved"] == 0

        meta = unlocked.get(f"{server}/api/files/{entry['id']}", timeout=30).json()["file"]
        assert {(s["part"], s["provider_id"], s["key"]) for s in meta["shards"]} == before

    def test_move_keeps_the_parts_where_they_are(self, server, unlocked):
        r = upload(unlocked, server, "mover.txt", b"contents that must survive")
        entry = r.json()["results"][0]["file"]
        before = {(s["part"], s["key"]) for s in entry["shards"]}

        unlocked.post(f"{server}/api/folders", json={"path": "/moved"},
                      headers={"Origin": server}, timeout=30)
        moved = unlocked.post(
            f"{server}/api/files/{entry['id']}/move",
            json={"dir": "/moved", "name": "renamed.txt"},
            headers={"Origin": server}, timeout=30,
        )
        assert moved.status_code == 200, moved.text
        assert moved.json()["path"] == "/moved/renamed.txt"

        after = {(s["part"], s["key"]) for s in moved.json()["file"]["shards"]}
        assert before == after, "a rename must not re-upload the parts"

        got = unlocked.get(f"{server}/api/files/{entry['id']}/content", timeout=60)
        assert got.content == b"contents that must survive"


class TestContentSafety:
    def test_html_is_served_as_a_download_not_rendered(self, server, unlocked):
        r = upload(unlocked, server, "payload.html", b"<script>alert(1)</script>")
        file_id = r.json()["results"][0]["file"]["id"]

        got = unlocked.get(f"{server}/api/files/{file_id}/content", timeout=60)
        assert got.headers["Content-Disposition"].startswith("attachment")
        assert got.headers.get("X-Content-Type-Options") == "nosniff"

    def test_decrypted_content_is_not_cacheable(self, server, unlocked):
        r = upload(unlocked, server, "private.txt", b"sensitive")
        file_id = r.json()["results"][0]["file"]["id"]

        got = unlocked.get(f"{server}/api/files/{file_id}/content", timeout=60)
        assert "no-store" in got.headers.get("Cache-Control", "")

    def test_cross_origin_writes_are_refused(self, server, unlocked):
        r = unlocked.post(
            f"{server}/api/folders",
            json={"path": "/injected"},
            headers={"Origin": "http://attacker.example"},
            timeout=30,
        )
        assert r.status_code == 403
        assert r.json()["code"] == "CROSS_ORIGIN"

    def test_provider_credentials_are_never_returned(self, server, unlocked):
        body = unlocked.get(f"{server}/api/providers", timeout=30).text
        # Local providers have no secrets, so add one with a secret field and
        # confirm the listing redacts it.
        unlocked.post(
            f"{server}/api/providers",
            json={
                "kind": "webdav",
                "name": "unreachable-dav",
                "options": {
                    "url": "http://127.0.0.1:1/dav",
                    "username": "u",
                    "password": "SUPER-SECRET-DAV-PASSWORD",
                },
            },
            headers={"Origin": server}, timeout=30,
        )
        body += unlocked.get(f"{server}/api/providers", timeout=30).text
        assert "SUPER-SECRET-DAV-PASSWORD" not in body


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

@pytest.fixture(scope="module", autouse=True)
def cli_vault(sand_bin, vault_dir):
    """A separate vault for the CLI tests, wired to three accounts."""
    cli(sand_bin, vault_dir, "vault", "init", "--policy", "strict")
    for name in ("cli-a", "cli-b", "cli-c"):
        path = os.path.join(vault_dir, "cli-clouds", name)
        cli(sand_bin, vault_dir, "remote", "add", "local", "--name", name, "--set", f"path={path}")
    return vault_dir


class TestCLI:
    def test_remote_list_shows_online_accounts(self, sand_bin, vault_dir):
        result = cli(sand_bin, vault_dir, "remote", "list")
        for name in ("cli-a", "cli-b", "cli-c"):
            assert name in result.stdout
        assert result.stdout.count("online") >= 3

    def test_remote_edit_renames_and_recolours_an_account(self, sand_bin, vault_dir, tmp_path):
        """An account's label and its colour are yours to change, and neither
        touches the parts it is holding."""
        path = os.path.join(vault_dir, "cli-clouds", "cli-edit")
        cli(sand_bin, vault_dir, "remote", "add", "local", "--name", "cli-edit",
            "--set", f"path={path}")

        source = tmp_path / "on-the-edited-cloud.bin"
        payload = os.urandom(20_000)
        source.write_bytes(payload)
        cli(sand_bin, vault_dir, "put", str(source), "--accounts", "cli-a,cli-b,cli-edit")

        def parts_on(directory):
            """The stored parts, ignoring the manifest backup and the scratch
            files an atomic write leaves behind."""
            return {n for n in os.listdir(directory)
                    if n.endswith(".sand") and n != "manifest.sand"}

        stored = parts_on(path)

        cli(sand_bin, vault_dir, "remote", "edit", "cli-edit",
            "--name", "cli-edited", "--color", "#38BDF8")

        listed = cli(sand_bin, vault_dir, "remote", "list").stdout
        row = next(line for line in listed.splitlines() if "cli-edited" in line)
        assert "#38bdf8" in row, row
        assert "cli-edit " not in listed

        # The file still says where its parts are, under the account's new name.
        ls_row = next(line for line in cli(sand_bin, vault_dir, "ls", "-l").stdout.splitlines()
                      if "on-the-edited-cloud.bin" in line)
        assert "cli-edited" in ls_row, ls_row

        # Not one part moved — the index changed, so the account's copy of the
        # manifest is rewritten, and nothing else on it is touched.
        assert parts_on(path) == stored
        out = tmp_path / "still-here.bin"
        cli(sand_bin, vault_dir, "get", "/on-the-edited-cloud.bin", "-o", str(out))
        assert out.read_bytes() == payload

        # A colour handed back, and a name another account already answers to.
        cli(sand_bin, vault_dir, "remote", "edit", "cli-edited", "--color", "auto")
        assert "auto" in next(line for line in
                              cli(sand_bin, vault_dir, "remote", "list").stdout.splitlines()
                              if "cli-edited" in line)

        clash = cli(sand_bin, vault_dir, "remote", "edit", "cli-edited",
                    "--name", "CLI-A", check=False)
        assert clash.returncode != 0
        assert "already connected" in (clash.stderr + clash.stdout)

    def test_put_get_round_trip(self, sand_bin, vault_dir, tmp_path):
        source = tmp_path / "cli-round-trip.bin"
        payload = os.urandom(64_000)
        source.write_bytes(payload)

        cli(sand_bin, vault_dir, "put", str(source))

        out = tmp_path / "restored.bin"
        cli(sand_bin, vault_dir, "get", "/cli-round-trip.bin", "-o", str(out))
        assert out.read_bytes() == payload

    def test_ls_shows_where_each_part_landed(self, sand_bin, vault_dir, tmp_path):
        source = tmp_path / "spread.txt"
        source.write_text("spread me around")
        cli(sand_bin, vault_dir, "put", str(source))

        result = cli(sand_bin, vault_dir, "ls")
        assert "spread.txt" in result.stdout
        assert "p1:" in result.stdout and "p2:" in result.stdout and "p3:" in result.stdout

    def test_mkdir_and_put_into_folder(self, sand_bin, vault_dir, tmp_path):
        source = tmp_path / "nested.txt"
        source.write_text("in a folder")

        cli(sand_bin, vault_dir, "mkdir", "/cli-docs")
        cli(sand_bin, vault_dir, "put", str(source), "--path", "/cli-docs")

        result = cli(sand_bin, vault_dir, "ls", "/cli-docs")
        assert "nested.txt" in result.stdout

    def test_mv_moves_a_folder_with_everything_under_it(self, sand_bin, vault_dir, tmp_path):
        source = tmp_path / "carried.txt"
        source.write_text("carried, not rebuilt")

        cli(sand_bin, vault_dir, "mkdir", "/cli-from/deep")
        cli(sand_bin, vault_dir, "mkdir", "/cli-to")
        cli(sand_bin, vault_dir, "put", str(source), "--path", "/cli-from/deep")

        # A destination that already exists means "inside it", the same way it
        # does when the thing being moved is a file.
        cli(sand_bin, vault_dir, "mv", "/cli-from", "/cli-to")

        assert "carried.txt" in cli(sand_bin, vault_dir, "ls", "/cli-to/cli-from/deep").stdout
        gone = cli(sand_bin, vault_dir, "ls", "/cli-from", check=False)
        assert gone.returncode != 0

        # The parts never moved, so the file still rebuilds from the accounts
        # it was scattered to in the first place.
        out = tmp_path / "carried-back.txt"
        cli(sand_bin, vault_dir, "get", "/cli-to/cli-from/deep/carried.txt", "-o", str(out))
        assert out.read_text() == "carried, not rebuilt"

        inside_itself = cli(sand_bin, vault_dir, "mv", "/cli-to", "/cli-to/cli-from/cli-to",
                            check=False)
        assert inside_itself.returncode != 0

    def test_check_reports_healthy_files(self, sand_bin, vault_dir, tmp_path):
        source = tmp_path / "healthy.txt"
        source.write_text("all good")
        cli(sand_bin, vault_dir, "put", str(source))

        result = cli(sand_bin, vault_dir, "check", "/healthy.txt")
        assert "ok" in result.stdout
        assert result.stdout.count("✓") == 3

    def test_check_detects_a_missing_part(self, sand_bin, vault_dir, tmp_path):
        clouds_root = os.path.join(vault_dir, "cli-clouds")

        def shard_paths():
            """The stored parts only.

            Every account also carries a copy of the index, rewritten in the
            background whenever the index changes — an upload, a rename, a
            deletion — so counting *files* rather than parts means counting
            whatever backup happened to land while this test was measuring.
            The atomic write that puts it there leaves a scratch file behind
            for an instant too.
            """
            found = set()
            for dirpath, _, filenames in os.walk(clouds_root):
                for filename in filenames:
                    if filename == "manifest.sand" or not filename.endswith(".sand"):
                        continue
                    found.add(os.path.join(dirpath, filename))
            return found

        before = shard_paths()

        source = tmp_path / "fragile.txt"
        source.write_text("one part will vanish")
        cli(sand_bin, vault_dir, "put", str(source))

        # Whatever appeared on the accounts belongs to the file just uploaded.
        new_shards = sorted(shard_paths() - before)
        assert len(new_shards) == 3, new_shards

        assert "ok" in cli(sand_bin, vault_dir, "check", "/fragile.txt").stdout

        os.remove(new_shards[0])

        result = cli(sand_bin, vault_dir, "check", "/fragile.txt", check=False)
        # A degraded file still reads, but check exits non-zero so it can gate
        # a scheduled integrity sweep.
        assert result.returncode != 0
        assert "degraded" in result.stdout
        assert "✗" in result.stdout

        # And it must still be retrievable from the two surviving parts.
        out = tmp_path / "recovered.txt"
        cli(sand_bin, vault_dir, "get", "/fragile.txt", "-o", str(out))
        assert out.read_text() == "one part will vanish"

    def test_relocate_moves_only_the_part_that_has_to(self, sand_bin, vault_dir, tmp_path):
        """Swapping one cloud out of three copies one part across, byte for
        byte, and leaves the other two exactly where they are."""
        fourth = os.path.join(vault_dir, "cli-clouds", "cli-d")
        cli(sand_bin, vault_dir, "remote", "add", "local", "--name", "cli-d",
            "--set", f"path={fourth}", check=False)

        leaving = os.path.join(vault_dir, "cli-clouds", "cli-c")

        def parts_on(directory):
            """The stored parts in an account, ignoring the manifest backup and
            the scratch files an atomic write leaves behind."""
            return {n for n in os.listdir(directory)
                    if n.endswith(".sand") and n != "manifest.sand"}

        untouched = parts_on(leaving)

        source = tmp_path / "relocatable.bin"
        payload = os.urandom(40_000)
        source.write_bytes(payload)
        cli(sand_bin, vault_dir, "put", str(source), "--accounts", "cli-a,cli-b,cli-c")
        assert parts_on(leaving) > untouched, "the put did not reach cli-c"

        dry = cli(sand_bin, vault_dir, "relocate", "/relocatable.bin",
                  "--accounts", "cli-a,cli-b,cli-d", "--dry-run")
        assert "1 part(s) to move" in dry.stdout, dry.stdout
        assert "cli-c → cli-d" in dry.stdout, dry.stdout
        # A dry run really is dry.
        assert parts_on(leaving) > untouched

        cli(sand_bin, vault_dir, "relocate", "/relocatable.bin",
            "--accounts", "cli-a,cli-b,cli-d")

        listed = cli(sand_bin, vault_dir, "ls").stdout
        row = next(line for line in listed.splitlines() if "relocatable.bin" in line)
        assert "cli-d" in row and "cli-c" not in row, row

        # cli-c is back to exactly what it held before the file existed: the
        # part it lost was erased, and nothing else was disturbed.
        assert parts_on(leaving) == untouched

        out = tmp_path / "relocated-back.bin"
        cli(sand_bin, vault_dir, "get", "/relocatable.bin", "-o", str(out))
        assert out.read_bytes() == payload

    def test_rm_removes_the_file(self, sand_bin, vault_dir, tmp_path):
        source = tmp_path / "doomed.txt"
        source.write_text("delete me")
        cli(sand_bin, vault_dir, "put", str(source))

        cli(sand_bin, vault_dir, "rm", "/doomed.txt")
        result = cli(sand_bin, vault_dir, "ls")
        assert "doomed.txt" not in result.stdout

    def test_wrong_password_is_rejected(self, sand_bin, vault_dir):
        result = cli(sand_bin, vault_dir, "ls", password="not-the-password", check=False)
        assert result.returncode != 0
        assert "wrong password" in result.stderr.lower()

    def test_vault_status_summarizes_the_vault(self, sand_bin, vault_dir):
        result = cli(sand_bin, vault_dir, "vault", "status")
        assert "Accounts:" in result.stdout
        assert "Placement policy: strict" in result.stdout

    def test_remote_kinds_documents_every_backend(self, sand_bin, vault_dir):
        result = cli(sand_bin, vault_dir, "remote", "kinds")
        for kind in ("local", "s3", "webdav", "gdrive", "dropbox",
                     "onedrive", "box", "proton"):
            assert kind in result.stdout


# ---------------------------------------------------------------------------
# Losing the vault file
# ---------------------------------------------------------------------------

class TestDisasterRecovery:
    """The vault file is gone and all that is left is the accounts and a password.

    These drive the real binary end to end: a vault is built in its own
    directory, the vault file is deleted outright, and everything is rebuilt
    from what the accounts are holding.
    """

    def build_vault(self, sand_bin, root, password="dr-passphrase"):
        """Create a vault with three accounts and two files in a folder."""
        vault = os.path.join(root, "cli-vault.sand")
        env_pw = {"password": password}

        cli(sand_bin, root, "vault", "init", **env_pw)
        clouds = []
        for name in ("dr-a", "dr-b", "dr-c"):
            path = os.path.join(root, "dr-clouds", name)
            clouds.append(path)
            cli(sand_bin, root, "remote", "add", "local",
                "--name", name, "--set", f"path={path}", **env_pw)

        payload = os.urandom(120_000)
        source = os.path.join(root, "ledger.bin")
        with open(source, "wb") as f:
            f.write(payload)
        notes = os.path.join(root, "notes.txt")
        with open(notes, "w") as f:
            f.write("board minutes")

        cli(sand_bin, root, "mkdir", "/finance", **env_pw)
        cli(sand_bin, root, "put", source, "--path", "/finance", **env_pw)
        cli(sand_bin, root, "put", notes, **env_pw)
        cli(sand_bin, root, "vault", "backup", **env_pw)
        return vault, clouds, payload

    def test_every_account_holds_a_manifest(self, sand_bin, tmp_path):
        root = str(tmp_path / "case1")
        os.makedirs(root)
        _, clouds, _ = self.build_vault(sand_bin, root)

        for cloud in clouds:
            manifest = os.path.join(cloud, "manifest.sand")
            assert os.path.exists(manifest), f"{cloud} holds no manifest backup"
            # It is an encrypted envelope, not a readable index.
            blob = open(manifest, "rb").read()
            assert b"SAND-MANIFEST" in blob
            assert b"ledger.bin" not in blob
            assert b"finance" not in blob

    def test_manifest_ls_rebuilds_the_tree_without_a_vault(self, sand_bin, tmp_path):
        root = str(tmp_path / "case2")
        os.makedirs(root)
        vault, clouds, _ = self.build_vault(sand_bin, root)
        os.remove(vault)

        env = dict(os.environ)
        env["SAND_PASSWORD"] = "dr-passphrase"
        result = subprocess.run(
            [sand_bin, "manifest", "ls", os.path.join(clouds[0], "manifest.sand"), "--long"],
            capture_output=True, text=True, env=env,
        )
        assert result.returncode == 0, result.stderr
        assert "/finance/ledger.bin" in result.stdout
        assert "/notes.txt" in result.stdout
        assert "part 1" in result.stdout and "part 3" in result.stdout

    def test_parts_plus_manifest_and_password_rebuild_a_file(self, sand_bin, tmp_path):
        root = str(tmp_path / "case3")
        os.makedirs(root)
        vault, clouds, payload = self.build_vault(sand_bin, root)
        os.remove(vault)

        # Collect the parts of the larger file the way someone would after
        # downloading what their accounts hold.
        parts = []
        for cloud in clouds:
            for name in sorted(os.listdir(cloud)):
                if name == "manifest.sand":
                    continue
                path = os.path.join(cloud, name)
                if os.path.getsize(path) > 10_000:  # the 120KB file, not the note
                    parts.append(path)
        assert len(parts) >= 2

        out = str(tmp_path / "rescued")
        env = dict(os.environ)
        env["SAND_PASSWORD"] = "dr-passphrase"
        result = subprocess.run(
            [sand_bin, "restore",
             "--parts", ",".join(parts[:2]),
             "--manifest", os.path.join(clouds[0], "manifest.sand"),
             "--preserve-tree", "--output-dir", out],
            capture_output=True, text=True, env=env,
        )
        assert result.returncode == 0, result.stderr
        rebuilt = os.path.join(out, "finance", "ledger.bin")
        assert open(rebuilt, "rb").read() == payload

    def test_a_fresh_vault_recovers_the_whole_index(self, sand_bin, tmp_path):
        root = str(tmp_path / "case4")
        os.makedirs(root)
        vault, clouds, payload = self.build_vault(sand_bin, root)
        os.remove(vault)

        # A new machine: new vault, new password, same accounts reconnected.
        new_root = str(tmp_path / "case4-new")
        os.makedirs(new_root)
        cli(sand_bin, new_root, "vault", "init", password="a-different-password")
        for i, cloud in enumerate(clouds):
            cli(sand_bin, new_root, "remote", "add", "local",
                "--name", f"back-{i}", "--set", f"path={cloud}",
                password="a-different-password")

        env = dict(os.environ)
        env["SAND_PASSWORD"] = "a-different-password"
        env["SAND_BACKUP_PASSWORD"] = "dr-passphrase"
        recover = subprocess.run(
            [sand_bin, "--vault", os.path.join(new_root, "cli-vault.sand"),
             "vault", "recover", "--from", "back-0"],
            capture_output=True, text=True, env=env,
        )
        assert recover.returncode == 0, recover.stderr
        # Both halves of the count, because the report leads on what came back
        # against what was there rather than on a bare total.
        assert "Recovered 2 of 2 file(s)" in recover.stdout
        assert "Not recovered" not in recover.stdout

        # The tree is back...
        result = cli(sand_bin, new_root, "ls", "/finance", password="a-different-password")
        assert "ledger.bin" in result.stdout

        # ...and so are the contents, under the new vault's password.
        out = str(tmp_path / "case4-out.bin")
        cli(sand_bin, new_root, "get", "/finance/ledger.bin", "-o", out,
            password="a-different-password")
        assert open(out, "rb").read() == payload
