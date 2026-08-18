"""
Shared pytest fixtures for SAND end-to-end tests.

Fixtures
--------
sand_bin         — path to the compiled sand binary
design_pdf       — path to tests/design.pdf
server           — starts sand serve on a free port against a throwaway vault
frontend_built   — True if the full React app is served (not the placeholder)
vault_password   — the password the server fixture's vault is created with
clouds           — a factory for throwaway "cloud account" directories
unlocked         — a requests.Session with an initialized, unlocked vault
s3_stub          — a stand-in S3 endpoint holding a bucket with something in it
bucket_server    — a second sand serve whose only account is that bucket
"""
import os
import re
import shutil
import socket
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
import requests

VAULT_PASSWORD = "e2e-test-passphrase"

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
PROJECT_ROOT = os.path.dirname(TESTS_DIR)


def _find_sand_binary():
    for name in ("sand.exe", "sand"):
        path = os.path.join(PROJECT_ROOT, name)
        if os.path.isfile(path):
            return path
    raise FileNotFoundError(
        "sand binary not found — build it first with:\n"
        "  go build -o sand ./cmd/sand   (Linux/macOS)\n"
        "  go build -o sand.exe ./cmd/sand  (Windows)"
    )


def _find_free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


# ---------------------------------------------------------------------------
# Session-scoped fixtures
# ---------------------------------------------------------------------------

@pytest.fixture(scope="session")
def sand_bin():
    """Absolute path to the compiled sand binary."""
    return _find_sand_binary()


@pytest.fixture(scope="session")
def design_pdf():
    """Absolute path to the sample PDF included with the test suite."""
    path = os.path.join(TESTS_DIR, "design.pdf")
    assert os.path.isfile(path), f"design.pdf not found at {path}"
    return path


@pytest.fixture(scope="session")
def vault_dir():
    """A throwaway directory holding the test vault and its fake cloud accounts.

    Every server the suite starts is pointed at a vault in here, so a test run
    can never read or clobber the developer's real ~/.sand vault.
    """
    path = tempfile.mkdtemp(prefix="sand-e2e-")
    yield path
    shutil.rmtree(path, ignore_errors=True)


@pytest.fixture(scope="session")
def vault_password():
    return VAULT_PASSWORD


@pytest.fixture(scope="session")
def server(sand_bin, vault_dir):
    """
    Start `sand serve` on a random free port against an isolated vault.
    Yields the base URL (e.g. 'http://127.0.0.1:54321').  The process is
    terminated when the test session ends.
    """
    port = _find_free_port()
    proc = subprocess.Popen(
        [
            sand_bin, "serve",
            "--port", str(port),
            "--bind", "127.0.0.1",
            "--vault", os.path.join(vault_dir, "vault.sand"),
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

    base_url = f"http://127.0.0.1:{port}"
    _wait_for_server(base_url, proc, port)

    yield base_url

    proc.terminate()
    proc.wait(timeout=5)


def _wait_for_server(base_url, proc, port):
    """Poll /api/health until the server answers, or give up after 10 s."""
    for _ in range(50):
        try:
            if requests.get(f"{base_url}/api/health", timeout=1).status_code == 200:
                return
        except Exception:
            pass
        time.sleep(0.2)
    proc.kill()
    raise RuntimeError(
        f"SAND server did not start within 10 s on port {port}.\n"
        f"stderr: {proc.stderr.read().decode()}"
    )


@pytest.fixture
def spawn_server(sand_bin, tmp_path):
    """Factory for an extra `sand serve` on a vault of its own.

    The session's `server` fixture shares one vault across the whole suite,
    which is exactly wrong for anything about a *new* machine: disaster
    recovery only runs into a vault that has never held a file, and it takes
    the vault over when it does. So those tests get their own process, their
    own port and their own vault file, all of which go away with the test.
    """
    started = []

    def _start(name="replacement"):
        port = _find_free_port()
        proc = subprocess.Popen(
            [
                sand_bin, "serve",
                "--port", str(port),
                "--bind", "127.0.0.1",
                "--vault", str(tmp_path / f"{name}.sand"),
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        started.append(proc)
        base_url = f"http://127.0.0.1:{port}"
        _wait_for_server(base_url, proc, port)
        return base_url

    yield _start

    for proc in started:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


LOST_VAULT_PASSWORD = "the-password-that-is-gone"
LOST_VAULT_FILES = ("ledger.csv", "notes.txt")


@pytest.fixture(scope="session")
def _lost_vault_template(sand_bin, tmp_path_factory):
    """Build the wreckage once: three cloud folders holding a vault whose
    machine then died.

    Session-scoped because building it is expensive in the way that matters
    here — a vault init and two uploads are four Argon2id derivations at 64 MB
    apiece, and doing that per test loaded the machine enough to make the
    unrelated browser tests race. Each test gets its own copy instead; see
    lost_vault, which is what tests actually ask for.
    """
    root = tmp_path_factory.mktemp("lost-vault")
    vault_file = root / "doomed.sand"
    clouds = []

    def run(*args):
        env = dict(os.environ)
        env["SAND_PASSWORD"] = LOST_VAULT_PASSWORD
        result = subprocess.run(
            [sand_bin, "--vault", str(vault_file), *args],
            capture_output=True, text=True, env=env,
        )
        assert result.returncode == 0, f"{args}\nstdout: {result.stdout}\nstderr: {result.stderr}"
        return result

    run("vault", "init", "--policy", "strict")
    for name in ("dead-one", "dead-two", "dead-three"):
        path = root / "clouds" / name
        os.makedirs(path, exist_ok=True)
        clouds.append(name)
        run("remote", "add", "local", "--name", name, "--set", f"path={path}")

    for name, body in (("ledger.csv", b"date,amount\n2026-08-01,42\n"), ("notes.txt", b"nothing here")):
        source = root / name
        source.write_bytes(body)
        run("put", str(source))

    # The machine dies here. The parts and the encrypted index are still sitting
    # on the accounts; the only thing that could read them is gone.
    os.remove(vault_file)
    return str(root / "clouds"), clouds


@pytest.fixture
def lost_vault(_lost_vault_template, tmp_path):
    """A private copy of that wreckage: (cloud_paths, password, filenames).

    Copied rather than shared, because recovering from these accounts writes to
    them — the recovering vault claims each one, replacing the backup the dead
    vault left with its own under a new password. Two tests over one set of
    folders would be two tests over one vault.
    """
    source, names = _lost_vault_template
    destination = tmp_path / "dead-clouds"
    shutil.copytree(source, destination)
    return [str(destination / name) for name in names], LOST_VAULT_PASSWORD, list(LOST_VAULT_FILES)


@pytest.fixture(scope="session")
def frontend_built(server):
    """
    True when the full React SPA is served.
    False when the placeholder page is present (frontend not yet compiled).
    """
    r = requests.get(server + "/", timeout=5)
    return "Build the frontend" not in r.text


# ---------------------------------------------------------------------------
# Vault fixtures — the connected-cloud mode
# ---------------------------------------------------------------------------

@pytest.fixture(scope="session")
def clouds(vault_dir):
    """Factory returning throwaway directories that stand in for cloud accounts.

    A local-folder provider is a real backend, so wiring three of them together
    exercises the same scatter/gather path a Drive or S3 account would take,
    without needing credentials in CI.
    """
    def _make(name):
        path = os.path.join(vault_dir, "clouds", name)
        os.makedirs(path, exist_ok=True)
        return path
    return _make


@pytest.fixture(scope="session")
def _vault_session():
    """The cookie jar shared by every API test."""
    return requests.Session()


@pytest.fixture
def unlocked(server, clouds, vault_password, _vault_session):
    """A requests.Session against a vault that is initialized, unlocked and
    wired to three separate accounts.

    Locking is deliberately global in SAND — the keys leave the process, so
    every session dies with them.  That means one test locking the vault (the
    GUI suite does exactly that) would strand every later test, so this fixture
    is function-scoped and re-establishes the session whenever it finds the
    vault closed.
    """
    session = _vault_session

    status = session.get(f"{server}/api/vault", timeout=10).json()
    if not status["unlocked"]:
        endpoint, expected = ("unlock", 200) if status["initialized"] else ("init", 201)
        r = session.post(
            f"{server}/api/vault/{endpoint}",
            json={"password": vault_password, "policy": "strict"} if endpoint == "init"
            else {"password": vault_password},
            headers={"Origin": server},
            timeout=60,
        )
        assert r.status_code == expected, r.text

    existing = {p["name"] for p in session.get(f"{server}/api/providers", timeout=30).json()["providers"]}
    for name in ("cloud-one", "cloud-two", "cloud-three"):
        if name in existing:
            continue
        r = session.post(
            f"{server}/api/providers",
            json={"kind": "local", "name": name, "options": {"path": clouds(name)}},
            headers={"Origin": server},
            timeout=30,
        )
        assert r.status_code == 201, r.text

    return session


# ---------------------------------------------------------------------------
# Browser fixtures
# ---------------------------------------------------------------------------

@pytest.fixture(scope="session")
def browser_type_launch_args(browser_type_launch_args):
    """Allow pointing Playwright at a pre-installed browser.

    Sandboxes and CI images often ship a Chromium that does not match the
    revision the Python Playwright package expects.  Setting
    PLAYWRIGHT_CHROMIUM_EXECUTABLE lets the GUI tests run against it instead of
    failing at launch.
    """
    executable = os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
    if executable and os.path.exists(executable):
        return {**browser_type_launch_args, "executable_path": executable}
    return browser_type_launch_args


# ---------------------------------------------------------------------------
# A bucket — the backend that reports no quota
# ---------------------------------------------------------------------------

# What is in the stand-in bucket: a part SAND could have written, and something
# that was already in there. The second one is the whole point — it is the
# figure the vault's own index cannot supply, and the reason counting a bucket
# is worth the listing it costs.
BUCKET_OBJECTS = [("vault/abc-p1.sand", 1_000_000), ("holiday.jpg", 4_000_000)]


class _S3Stub(BaseHTTPRequestHandler):
    """Enough of the S3 API to connect an account and count what is in it.

    The signature is not checked: what these tests are about is the figures the
    app draws from a listing, and SigV4 itself is covered by a Go test against
    a stub that does read the headers (TestS3SigV4AgainstStubEndpoint).
    """

    def log_message(self, *args):
        pass

    def do_GET(self):
        query = self.path.split("?", 1)[1] if "?" in self.path else ""
        if "list-type=2" not in query:
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return

        match = re.search(r"(?:^|&)prefix=([^&]*)", query)
        prefix = match.group(1).replace("%2F", "/") if match else ""
        rows = "".join(
            f"<Contents><Key>{key}</Key><Size>{size}</Size></Contents>"
            for key, size in BUCKET_OBJECTS if key.startswith(prefix)
        )
        payload = (
            '<?xml version="1.0"?><ListBucketResult>'
            "<IsTruncated>false</IsTruncated>" + rows + "</ListBucketResult>"
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/xml")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    do_HEAD = do_GET


@pytest.fixture(scope="session")
def s3_stub():
    """An S3 endpoint on a free port, holding a bucket with two objects."""
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _S3Stub)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{httpd.server_address[1]}"
    httpd.shutdown()


@pytest.fixture(scope="session")
def bucket_server(sand_bin, s3_stub, vault_password):
    """A second `sand serve`, with a bucket for an account and nothing else.

    Its own vault and its own process on purpose: the shared one is wired to
    three local folders, and the tests written against it count accounts and
    read where parts landed. An S3 account dropped into that vault would change
    both answers for everyone.
    """
    directory = tempfile.mkdtemp(prefix="sand-bucket-")
    port = _find_free_port()
    proc = subprocess.Popen(
        [
            sand_bin, "serve",
            "--port", str(port),
            "--bind", "127.0.0.1",
            "--vault", os.path.join(directory, "vault.sand"),
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

    base_url = f"http://127.0.0.1:{port}"
    _wait_for_server(base_url, proc, port)

    session = requests.Session()
    r = session.post(
        f"{base_url}/api/vault/init",
        json={"password": vault_password, "policy": "mirror"},
        headers={"Origin": base_url},
        timeout=60,
    )
    assert r.status_code == 201, r.text
    r = session.post(
        f"{base_url}/api/providers",
        json={"kind": "s3", "name": "b2-cold", "options": {
            "bucket": "shards",
            "region": "us-west-004",
            "access_key_id": "AKIAIOSFODNN7EXAMPLE",
            "secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
            "endpoint": s3_stub,
            "prefix": "vault/",
        }},
        headers={"Origin": base_url},
        timeout=30,
    )
    assert r.status_code == 201, r.text

    yield base_url

    proc.terminate()
    proc.wait(timeout=5)
    shutil.rmtree(directory, ignore_errors=True)
