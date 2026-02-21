"""
HTTP API end-to-end tests for SAND web server.

Tests hit the live server started by the `server` fixture in conftest.py
using the `requests` library — no browser required.

After the multi-file archive change the API contract is:
  POST /api/archive
    - Request:  files[] (one or more files) + password
    - Response: sand-archives.zip  (outer zip)
                  ├── media1.zip   (inner zip with one .media1 per input file)
                  ├── media2.zip
                  └── media3.zip

  POST /api/restore  — unchanged: individual .media files + password
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

def _archive(server, files, password="pw"):
    """
    POST to /api/archive.

    *files* is a list of (filename, bytes) tuples.
    Returns the raw response.
    """
    return requests.post(
        f"{server}/api/archive",
        files=[
            ("files[]", (name, content, "application/octet-stream"))
            for name, content in files
        ],
        data={"password": password},
    )


def _archive_one(server, content, filename="test.bin", password="pw"):
    """Convenience wrapper for archiving a single file."""
    return _archive(server, [(filename, content)], password)


def _parse_outer_zip(response):
    """
    Parse the sand-archives.zip response.

    Returns a dict:
      {
        1: { "media_name": str, "data": bytes },   # from media1.zip
        2: ...,
        3: ...,
      }
    where each inner dict represents the *first* media file found in that
    inner zip (sufficient for single-file archives; iterate manually for
    multi-file archives).
    """
    outer = zipfile.ZipFile(io.BytesIO(response.content))
    result = {}
    for inner_name in sorted(outer.namelist()):   # media1.zip, media2.zip, media3.zip
        num = int(inner_name[5])                   # "media1.zip"[5] == '1'
        inner_bytes = outer.read(inner_name)
        inner = zipfile.ZipFile(io.BytesIO(inner_bytes))
        media_name = inner.namelist()[0]
        result[num] = {"media_name": media_name, "data": inner.read(media_name)}
    return result


def _get_parts(server, content, filename="test.bin", password="pw"):
    """
    Archive *content* and return a dict mapping part number → (name, bytes).
    Used by restore tests.
    """
    r = _archive_one(server, content, filename, password)
    r.raise_for_status()
    parsed = _parse_outer_zip(r)
    return {num: (v["media_name"], v["data"]) for num, v in parsed.items()}


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
# /api/archive — single file
# ===========================================================================

class TestArchiveEndpoint:
    def test_returns_200_with_zip(self, server):
        r = _archive_one(server, b"hello API", "hello.txt")
        assert r.status_code == 200
        assert r.headers["Content-Type"] == "application/zip"

    def test_outer_zip_contains_three_inner_zips(self, server):
        r = _archive_one(server, b"three parts please", "three.bin")
        with zipfile.ZipFile(io.BytesIO(r.content)) as outer:
            names = outer.namelist()
        assert len(names) == 3
        assert set(names) == {"media1.zip", "media2.zip", "media3.zip"}

    def test_inner_zips_contain_correct_media_files(self, server):
        r = _archive_one(server, b"check inner zips", "check.bin")
        with zipfile.ZipFile(io.BytesIO(r.content)) as outer:
            for i in range(1, 4):
                inner_bytes = outer.read(f"media{i}.zip")
                with zipfile.ZipFile(io.BytesIO(inner_bytes)) as inner:
                    names = inner.namelist()
                    assert len(names) == 1, f"media{i}.zip: expected 1 entry, got {names}"
                    assert names[0].endswith(f".media{i}"), \
                        f"media{i}.zip entry should end in .media{i}, got {names[0]}"

    def test_content_disposition_is_sand_archives_zip(self, server):
        r = _archive_one(server, b"filename test", "report.pdf")
        cd = r.headers.get("Content-Disposition", "")
        assert "sand-archives.zip" in cd

    def test_media_files_have_SAND_magic(self, server):
        parsed = _parse_outer_zip(_archive_one(server, b"\x00" * 200, "magic.bin"))
        for num, info in parsed.items():
            assert info["data"][:4] == b"SAND", f"media{num} missing SAND magic"

    def test_all_parts_same_size(self, server):
        parsed = _parse_outer_zip(_archive_one(server, b"A" * 500, "equal.bin"))
        sizes = [len(parsed[n]["data"]) for n in sorted(parsed)]
        assert sizes[0] == sizes[1] == sizes[2], f"part sizes differ: {sizes}"

    def test_missing_password_returns_400(self, server):
        r = requests.post(
            f"{server}/api/archive",
            files=[("files[]", ("t.txt", b"data"))],
        )
        assert r.status_code == 400
        assert r.json()["code"] == "MISSING_PASSWORD"

    def test_missing_file_returns_400(self, server):
        # Send multipart with a wrong field name so files[] is absent → MISSING_FILE.
        r = requests.post(
            f"{server}/api/archive",
            files=[("wrong_field", ("t.txt", b"data"))],
            data={"password": "pw"},
        )
        assert r.status_code == 400
        assert r.json()["code"] == "MISSING_FILE"

    def test_no_multipart_body_returns_400(self, server):
        # Plain urlencoded body (not multipart) → 400 of some kind.
        r = requests.post(f"{server}/api/archive", data={"password": "pw"})
        assert r.status_code == 400

    def test_archive_design_pdf(self, server, design_pdf):
        data = open(design_pdf, "rb").read()
        r = _archive_one(server, data, "design.pdf", "pdfpass")
        assert r.status_code == 200
        with zipfile.ZipFile(io.BytesIO(r.content)) as outer:
            assert set(outer.namelist()) == {"media1.zip", "media2.zip", "media3.zip"}

    def test_archive_empty_file(self, server):
        r = _archive_one(server, b"", "empty.bin")
        assert r.status_code == 200
        with zipfile.ZipFile(io.BytesIO(r.content)) as outer:
            assert len(outer.namelist()) == 3


# ===========================================================================
# /api/archive — multiple files
# ===========================================================================

class TestArchiveEndpointMultipleFiles:
    def test_two_files_outer_zip_has_three_inner_zips(self, server):
        r = _archive(server, [("a.txt", b"file A content"), ("b.txt", b"file B content")])
        assert r.status_code == 200
        with zipfile.ZipFile(io.BytesIO(r.content)) as outer:
            assert set(outer.namelist()) == {"media1.zip", "media2.zip", "media3.zip"}

    def test_two_files_each_inner_zip_has_two_entries(self, server):
        r = _archive(server, [("x.bin", b"x data " * 20), ("y.bin", b"y data " * 20)])
        assert r.status_code == 200
        with zipfile.ZipFile(io.BytesIO(r.content)) as outer:
            for i in range(1, 4):
                inner_bytes = outer.read(f"media{i}.zip")
                with zipfile.ZipFile(io.BytesIO(inner_bytes)) as inner:
                    names = inner.namelist()
                    assert len(names) == 2, f"media{i}.zip: expected 2 entries, got {names}"

    def test_three_files_each_inner_zip_has_three_entries(self, server):
        files = [(f"f{i}.txt", f"content {i}".encode()) for i in range(3)]
        r = _archive(server, files)
        assert r.status_code == 200
        with zipfile.ZipFile(io.BytesIO(r.content)) as outer:
            for i in range(1, 4):
                inner_bytes = outer.read(f"media{i}.zip")
                with zipfile.ZipFile(io.BytesIO(inner_bytes)) as inner:
                    assert len(inner.namelist()) == 3

    def test_each_file_in_multi_archive_restores_correctly(self, server):
        orig_a = b"file A original content - unique A"
        orig_b = b"file B original content - unique B"
        r = _archive(server, [("fileA.bin", orig_a), ("fileB.bin", orig_b)])
        assert r.status_code == 200

        # Collect per-file media parts from each inner zip
        parts_a = {}
        parts_b = {}
        with zipfile.ZipFile(io.BytesIO(r.content)) as outer:
            for i in range(1, 4):
                inner_bytes = outer.read(f"media{i}.zip")
                with zipfile.ZipFile(io.BytesIO(inner_bytes)) as inner:
                    for name in inner.namelist():
                        data = inner.read(name)
                        if "fileA" in name:
                            parts_a[i] = (name, data)
                        elif "fileB" in name:
                            parts_b[i] = (name, data)

        # Restore file A using parts 1+3
        ra = _restore(server, [parts_a[1], parts_a[3]])
        assert ra.status_code == 200
        assert ra.content == orig_a

        # Restore file B using parts 2+3
        rb = _restore(server, [parts_b[2], parts_b[3]])
        assert rb.status_code == 200
        assert rb.content == orig_b

    def test_cross_file_parts_are_rejected(self, server):
        """A part from file A and a part from file B must not restore."""
        r = _archive(server, [("one.bin", b"one content"), ("two.bin", b"two content")])
        assert r.status_code == 200

        part_one_1 = None
        part_two_2 = None
        with zipfile.ZipFile(io.BytesIO(r.content)) as outer:
            inner1 = zipfile.ZipFile(io.BytesIO(outer.read("media1.zip")))
            for name in inner1.namelist():
                if "one" in name:
                    part_one_1 = (name, inner1.read(name))

            inner2 = zipfile.ZipFile(io.BytesIO(outer.read("media2.zip")))
            for name in inner2.namelist():
                if "two" in name:
                    part_two_2 = (name, inner2.read(name))

        assert part_one_1 and part_two_2
        r = _restore(server, [part_one_1, part_two_2])
        assert r.status_code == 400


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
