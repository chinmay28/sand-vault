"""
Shared pytest fixtures for SAND end-to-end tests.

Fixtures
--------
sand_bin         — path to the compiled sand binary
design_pdf       — path to tests/design.pdf
server           — starts sand serve on a free port, yields base URL, stops on teardown
frontend_built   — True if the full React app is served (not the placeholder)
"""
import os
import socket
import subprocess
import time

import pytest
import requests

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
def server(sand_bin):
    """
    Start `sand serve` on a random free port.  Yields the base URL
    (e.g. 'http://127.0.0.1:54321').  The process is terminated when the
    test session ends.
    """
    port = _find_free_port()
    proc = subprocess.Popen(
        [sand_bin, "serve", "--port", str(port), "--bind", "127.0.0.1"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

    base_url = f"http://127.0.0.1:{port}"

    # Poll /api/health until ready (up to 10 s)
    for _ in range(50):
        try:
            r = requests.get(f"{base_url}/api/health", timeout=1)
            if r.status_code == 200:
                break
        except Exception:
            pass
        time.sleep(0.2)
    else:
        proc.kill()
        raise RuntimeError(
            f"SAND server did not start within 10 s on port {port}.\n"
            f"stderr: {proc.stderr.read().decode()}"
        )

    yield base_url

    proc.terminate()
    proc.wait(timeout=5)


@pytest.fixture(scope="session")
def frontend_built(server):
    """
    True when the full React SPA is served.
    False when the placeholder page is present (frontend not yet compiled).
    """
    r = requests.get(server + "/", timeout=5)
    return "Build the frontend" not in r.text
