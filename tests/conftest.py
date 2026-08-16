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
"""
import os
import shutil
import socket
import subprocess
import tempfile
import time

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


@pytest.fixture
def lost_vault(sand_bin, tmp_path):
    """Three cloud folders holding a vault whose machine then died.

    Returns (cloud_paths, password, filenames). The vault file is deleted before
    the fixture hands anything back, which is the whole scenario: the parts and
    the encrypted index are still sitting on the accounts, and the only thing
    that could read them is gone.
    """
    password = "the-password-that-is-gone"
    vault_file = tmp_path / "doomed.sand"
    clouds = []

    def run(*args):
        env = dict(os.environ)
        env["SAND_PASSWORD"] = password
        result = subprocess.run(
            [sand_bin, "--vault", str(vault_file), *args],
            capture_output=True, text=True, env=env,
        )
        assert result.returncode == 0, f"{args}\nstdout: {result.stdout}\nstderr: {result.stderr}"
        return result

    run("vault", "init", "--policy", "strict")
    for name in ("dead-one", "dead-two", "dead-three"):
        path = tmp_path / "dead-clouds" / name
        os.makedirs(path, exist_ok=True)
        clouds.append(str(path))
        run("remote", "add", "local", "--name", name, "--set", f"path={path}")

    names = []
    for name, body in (("ledger.csv", b"date,amount\n2026-08-01,42\n"), ("notes.txt", b"nothing here")):
        source = tmp_path / name
        source.write_bytes(body)
        run("put", str(source))
        names.append(name)

    os.remove(vault_file)
    return clouds, password, names


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
