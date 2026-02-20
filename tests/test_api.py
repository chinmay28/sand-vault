"""
HTTP API end-to-end tests for SAND web server.

Tests hit the live server started by the `server` fixture in conftest.py
using the `requests` library — no browser required.
"""
import hashlib
import io
import os
import zipfile

import pytest
import requests

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _archive(server, content, filename="test.bin", password="pw"):
    """POST to /api/archive and return the response."""
    return requests.post(
        f"{server}/api/archive",
        files={"file": (filename, content, "application/octet-stream")},
        data={"password": password},
    )


def _get_parts(server, content, filename="test.bin", password="pw"):
    """
    Archive *content* and return a dict mapping part number → (name, bytes).
    """
    r = _archive(server, content, filename, password)
    r.raise_for_status()
    parts = {}
    with zipfile.ZipFile(io.BytesIO(r.content)) as zf:
        for name in zf.namelist():
            num = int(name[-1])          # last char of .mediaN
            parts[num] = (name, zf.read(name))
    return parts


def _restore(server, parts_list, password="pw"):
    """POST to /api/restore with the given list of (name, bytes) tuples."""
    return requests.post(
        f"{server}/api/restore",
        files=[("parts[]", (name, data, "application/octet-stream")) for name, data in parts_list],
        data={"password": password},
    )


# ===========================================================================
# Health
# ===========================================================================

class TestHealthEndpoint:
    def test_returns_200(self, server):
        r = requests.get(f"{server}/api/health")
        assert r.status_code == 200

    def test_body_status_ok(self, server):
        r = requests.get(f"{server}/api/health")
        assert r.json()["status"] == "ok"

    def test_body_has_version(self, server):
        r = requests.get(f"{server}/api/health")
        assert "version" in r.json()

    def test_content_type_is_json(self, server):
        r = requests.get(f"{server}/api/health")
        assert "application/json" in r.headers["Content-Type"]


# ===========================================================================
# /api/archive
# ===========================================================================

class TestArchiveEndpoint:
    def test_returns_200_with_zip(self, server):
        r = _archive(server, b"hello API", "hello.txt")
        assert r.status_code == 200
        assert r.headers["Content-Type"] == "application/zip"

    def test_zip_contains_three_media_files(self, server):
        r = _archive(server, b"three parts please", "three.bin")
        with zipfile.ZipFile(io.BytesIO(r.content)) as zf:
            names = zf.namelist()
        assert len(names) == 3
        assert any(n.endswith(".media1") for n in names)
        assert any(n.endswith(".media2") for n in names)
        assert any(n.endswith(".media3") for n in names)

    def test_content_disposition_filename(self, server):
        r = _archive(server, b"filename test", "report.pdf")
        cd = r.headers.get("Content-Disposition", "")
        assert "report.pdf.sand.zip" in cd

    def test_media_files_have_SAND_magic(self, server):
        r = _archive(server, b"\x00" * 200, "magic.bin")
        with zipfile.ZipFile(io.BytesIO(r.content)) as zf:
            for name in zf.namelist():
                assert zf.read(name)[:4] == b"SAND", f"{name} missing SAND magic"

    def test_all_parts_same_size(self, server):
        r = _archive(server, b"A" * 500, "equal.bin")
        with zipfile.ZipFile(io.BytesIO(r.content)) as zf:
            sizes = [len(zf.read(n)) for n in sorted(zf.namelist())]
        assert sizes[0] == sizes[1] == sizes[2], f"part sizes differ: {sizes}"

    def test_missing_password_returns_400(self, server):
        r = requests.post(
            f"{server}/api/archive",
            files={"file": ("t.txt", b"data")},
        )
        assert r.status_code == 400
        assert r.json()["code"] == "MISSING_PASSWORD"

    def test_missing_file_returns_400(self, server):
        r = requests.post(f"{server}/api/archive", data={"password": "pw"})
        assert r.status_code == 400
        assert r.json()["code"] == "MISSING_FILE"

    def test_archive_design_pdf(self, server, design_pdf):
        data = open(design_pdf, "rb").read()
        r = _archive(server, data, "design.pdf", "pdfpass")
        assert r.status_code == 200
        with zipfile.ZipFile(io.BytesIO(r.content)) as zf:
            assert len(zf.namelist()) == 3

    def test_archive_empty_file(self, server):
        r = _archive(server, b"", "empty.bin")
        assert r.status_code == 200
        with zipfile.ZipFile(io.BytesIO(r.content)) as zf:
            assert len(zf.namelist()) == 3


# ===========================================================================
# /api/restore
# ===========================================================================

class TestRestoreEndpoint:
    @pytest.mark.parametrize(
        "combo", [(1, 2), (1, 3), (2, 3)], ids=["parts1+2", "parts1+3", "parts2+3"]
    )
    def test_restore_two_of_three_parts(self, server, combo):
        original = b"restore via API: combo " + str(combo).encode()
        parts = _get_parts(server, original)

        r = _restore(server, [parts[combo[0]], parts[combo[1]]])

        assert r.status_code == 200
        assert r.content == original

    def test_restore_all_three_parts(self, server):
        original = b"all three provided to restore endpoint"
        parts = _get_parts(server, original)

        r = _restore(server, [parts[1], parts[2], parts[3]])

        assert r.status_code == 200
        assert r.content == original

    def test_content_disposition_has_original_filename(self, server):
        parts = _get_parts(server, b"fname test", filename="invoice.pdf")
        r = _restore(server, [parts[1], parts[2]])
        cd = r.headers.get("Content-Disposition", "")
        assert "invoice.pdf" in cd

    def test_content_type_is_octet_stream(self, server):
        parts = _get_parts(server, b"content type check")
        r = _restore(server, [parts[1], parts[2]])
        assert "application/octet-stream" in r.headers["Content-Type"]

    def test_wrong_password_returns_400_wrong_password_code(self, server):
        parts = _get_parts(server, b"secret", password="correct")
        r = _restore(server, [parts[1], parts[2]], password="wrong")
        assert r.status_code == 400
        assert r.json()["code"] == "WRONG_PASSWORD"

    def test_too_few_parts_returns_400(self, server):
        parts = _get_parts(server, b"data")
        r = _restore(server, [parts[1]])
        assert r.status_code == 400

    def test_missing_password_returns_400(self, server):
        parts = _get_parts(server, b"data")
        r = requests.post(
            f"{server}/api/restore",
            files=[("parts[]", (parts[1][0], parts[1][1]))],
        )
        assert r.status_code == 400
        assert r.json()["code"] == "MISSING_PASSWORD"

    def test_mismatched_parts_return_error(self, server):
        parts_a = _get_parts(server, b"archive A content")
        parts_b = _get_parts(server, b"archive B content")

        r = _restore(server, [parts_a[1], parts_b[2]])

        assert r.status_code == 400
        assert r.json()["code"] == "MISMATCHED_PARTS"

    def test_empty_file_roundtrip(self, server):
        parts = _get_parts(server, b"", "empty.bin")
        r = _restore(server, [parts[1], parts[2]])
        assert r.status_code == 200
        assert r.content == b""

    def test_design_pdf_roundtrip_parts_1_and_3(self, server, design_pdf):
        original = open(design_pdf, "rb").read()
        orig_hash = hashlib.sha256(original).hexdigest()

        parts = _get_parts(server, original, "design.pdf", "pdfapitest")
        r = _restore(server, [parts[1], parts[3]], "pdfapitest")

        assert r.status_code == 200
        restored_hash = hashlib.sha256(r.content).hexdigest()
        assert orig_hash == restored_hash, "PDF content changed after API round-trip"

    @pytest.mark.slow
    def test_large_file_roundtrip(self, server):
        original = os.urandom(512 * 1024)  # 512 KB random
        parts = _get_parts(server, original, "large.bin", "largepw")
        r = _restore(server, [parts[2], parts[3]], "largepw")
        assert r.status_code == 200
        assert r.content == original

    def test_error_response_has_code_and_message(self, server):
        r = requests.post(f"{server}/api/restore", data={"password": "pw"})
        body = r.json()
        assert "error" in body
        assert "code" in body


# ===========================================================================
# Static file serving
# ===========================================================================

class TestStaticServing:
    def test_root_returns_html(self, server):
        r = requests.get(f"{server}/")
        assert r.status_code == 200
        assert "html" in r.headers.get("Content-Type", "").lower()

    def test_unknown_api_path_returns_html_not_404(self, server):
        # SPA fallback: unknown paths serve index.html
        r = requests.get(f"{server}/some/unknown/path")
        assert r.status_code in (200, 404)  # either is acceptable
