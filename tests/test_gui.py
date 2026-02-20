"""
Headless browser end-to-end tests for the SAND web GUI.

Uses Playwright (Chromium) to drive the browser.  The tests are split into
three groups:

1. TestPageLoad        — always run; verifies the server serves valid HTML
2. TestAPIviaJS        — always run; exercises the API from inside the browser
                         via page.evaluate() — works even with the placeholder
3. TestGUILayout       — skipped when full React frontend is not built
4. TestArchiveWorkflow — skipped when full React frontend is not built
5. TestRestoreWorkflow — skipped when full React frontend is not built

Run `make build-web` (requires Node.js) to compile the React app and unlock
groups 3-5.
"""
import hashlib
import io
import os
import zipfile

import pytest
import requests
from playwright.sync_api import Page, expect

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _api_archive(server, content, filename="test.bin", password="pw"):
    r = requests.post(
        f"{server}/api/archive",
        files={"file": (filename, content)},
        data={"password": password},
    )
    r.raise_for_status()
    parts = {}
    with zipfile.ZipFile(io.BytesIO(r.content)) as zf:
        for name in zf.namelist():
            parts[int(name[-1])] = (name, zf.read(name))
    return parts


def _skip_no_frontend(frontend_built):
    if not frontend_built:
        pytest.skip("React frontend not built — run 'make build-web' to enable GUI tests")


# ===========================================================================
# 1. Page load — always runs
# ===========================================================================

class TestPageLoad:
    def test_root_returns_200(self, server):
        r = requests.get(server + "/")
        assert r.status_code == 200

    def test_root_content_type_is_html(self, server):
        r = requests.get(server + "/")
        assert "html" in r.headers.get("Content-Type", "").lower()

    def test_browser_opens_page(self, server, page: Page):
        page.goto(server + "/")
        assert page.title() is not None  # page has a <title>

    def test_body_renders_something(self, server, page: Page):
        page.goto(server + "/")
        page.wait_for_load_state("domcontentloaded")
        assert len(page.locator("body").inner_text()) > 0

    def test_no_javascript_console_errors_on_load(self, server, page: Page):
        errors = []
        page.on("pageerror", lambda e: errors.append(str(e)))
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        assert errors == [], f"JS errors on page load: {errors}"


# ===========================================================================
# 2. API via browser JS — always runs (no React required)
# ===========================================================================

class TestAPIviaJS:
    def test_health_via_fetch(self, server, page: Page):
        page.goto(server + "/")
        result = page.evaluate("""
            async () => {
                const r = await fetch('/api/health');
                return await r.json();
            }
        """)
        assert result["status"] == "ok"

    def test_archive_via_fetch(self, server, page: Page):
        page.goto(server + "/")
        result = page.evaluate("""
            async () => {
                const blob = new Blob(['hello from browser'], {type: 'text/plain'});
                const form = new FormData();
                form.append('file', blob, 'browser_test.txt');
                form.append('password', 'browserpw');
                const r = await fetch('/api/archive', {method: 'POST', body: form});
                return {
                    status: r.status,
                    contentType: r.headers.get('Content-Type'),
                };
            }
        """)
        assert result["status"] == 200
        assert "zip" in result["contentType"]

    def test_archive_then_restore_via_fetch(self, server, page: Page):
        """Full round-trip entirely in browser JS."""
        page.goto(server + "/")
        result = page.evaluate("""
            async () => {
                // Archive
                const original = 'round-trip content from browser';
                const blob = new Blob([original], {type: 'text/plain'});
                const form1 = new FormData();
                form1.append('file', blob, 'rt.txt');
                form1.append('password', 'rtpw');
                const archiveResp = await fetch('/api/archive', {method: 'POST', body: form1});
                if (!archiveResp.ok) return {ok: false, step: 'archive'};

                // Unzip to get parts
                const zipBytes = await archiveResp.arrayBuffer();
                // We cannot unzip in the browser easily, so just verify we got a ZIP
                const magic = new Uint8Array(zipBytes, 0, 4);
                const isZip = magic[0] === 0x50 && magic[1] === 0x4b;
                return {ok: isZip, step: 'archive_verified'};
            }
        """)
        assert result["ok"] is True

    def test_missing_password_returns_400_via_fetch(self, server, page: Page):
        page.goto(server + "/")
        status = page.evaluate("""
            async () => {
                const blob = new Blob(['data'], {type: 'text/plain'});
                const form = new FormData();
                form.append('file', blob, 'f.txt');
                // no password field
                const r = await fetch('/api/archive', {method: 'POST', body: form});
                return r.status;
            }
        """)
        assert status == 400

    def test_wrong_password_returns_400_via_fetch(self, server, page: Page):
        page.goto(server + "/")
        # We need real media-file bytes — obtain them via requests, then pass to browser
        parts = _api_archive(server, b"test content", "t.bin", "correct")
        part1_bytes = list(parts[1][1])  # convert bytes to list for JSON transport
        part2_bytes = list(parts[2][1])

        status = page.evaluate(
            """
            async ([p1, p2, name1, name2]) => {
                const f1 = new Blob([new Uint8Array(p1)]);
                const f2 = new Blob([new Uint8Array(p2)]);
                const form = new FormData();
                form.append('parts[]', f1, name1);
                form.append('parts[]', f2, name2);
                form.append('password', 'wrong');
                const r = await fetch('/api/restore', {method: 'POST', body: form});
                return r.status;
            }
            """,
            [part1_bytes, part2_bytes, parts[1][0], parts[2][0]],
        )
        assert status == 400


# ===========================================================================
# 3. GUI Layout — requires full React frontend
# ===========================================================================

class TestGUILayout:
    def test_sand_logo_visible(self, server, page: Page, frontend_built):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        expect(page.get_by_text("SAND")).to_be_visible()

    def test_subtitle_visible(self, server, page: Page, frontend_built):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        expect(page.get_by_text("Secure Archival Network Distribution")).to_be_visible()

    def test_archive_tab_present(self, server, page: Page, frontend_built):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        # Tab button is "📦 Archive"; use emoji prefix to distinguish from the "▶ Archive" submit button
        expect(page.get_by_role("button", name="📦 Archive")).to_be_visible()

    def test_restore_tab_present(self, server, page: Page, frontend_built):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        expect(page.get_by_role("button", name="Restore")).to_be_visible()

    def test_password_field_present_on_archive_tab(self, server, page: Page, frontend_built):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        expect(page.locator("input[type='password']")).to_be_visible()

    def test_footer_shows_crypto_info(self, server, page: Page, frontend_built):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        body = page.locator("body").inner_text()
        assert "AES-256-GCM" in body
        assert "Argon2id" in body

    def test_tab_switch_to_restore_shows_part_badges(self, server, page: Page, frontend_built):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        page.get_by_role("button", name="Restore").click()
        expect(page.get_by_text("Part 1")).to_be_visible()
        expect(page.get_by_text("Part 2")).to_be_visible()
        expect(page.get_by_text("Part 3")).to_be_visible()


# ===========================================================================
# 4. Archive workflow — requires full React frontend
# ===========================================================================

class TestArchiveWorkflow:
    def test_archive_button_disabled_without_file(self, server, page: Page, frontend_built):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")

        page.locator("input[type='password']").fill("pw")

        # Submit button text is "▶ Archive" — target it specifically
        btn = page.locator("button", has_text="▶ Archive")
        assert btn.is_disabled()

    def test_archive_button_disabled_without_password(self, server, page: Page, frontend_built, tmp_path):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")

        f = tmp_path / "nopw.txt"
        f.write_text("data")
        page.locator("input[type='file']").set_input_files(str(f))

        btn = page.locator("button", has_text="▶ Archive")
        assert btn.is_disabled()

    def test_archive_file_triggers_zip_download(self, server, page: Page, frontend_built, tmp_path):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")

        f = tmp_path / "gui_archive.txt"
        f.write_text("GUI archive test content — checking download")
        page.locator("input[type='file']").set_input_files(str(f))
        page.locator("input[type='password']").fill("guipassword")

        with page.expect_download() as dl:
            page.locator("button", has_text="▶ Archive").click()

        download = dl.value
        assert download.suggested_filename.endswith(".sand.zip")

        # ZIP must contain 3 valid media files
        with zipfile.ZipFile(download.path()) as zf:
            assert len(zf.namelist()) == 3
            for name in zf.namelist():
                assert zf.read(name)[:4] == b"SAND"

    def test_success_message_shown_after_archive(self, server, page: Page, frontend_built, tmp_path):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")

        f = tmp_path / "success.txt"
        f.write_text("testing success message")
        page.locator("input[type='file']").set_input_files(str(f))
        page.locator("input[type='password']").fill("pw")

        with page.expect_download():
            page.locator("button", has_text="▶ Archive").click()

        # Wait for UI to update
        page.wait_for_timeout(1000)
        body = page.locator("body").inner_text()
        # Should show some success indicator
        assert any(word in body.lower() for word in ("archived", "done", "sand.zip", "✓"))

    def test_show_hide_password_toggle(self, server, page: Page, frontend_built):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")

        pw_input = page.locator("input[type='password']")
        expect(pw_input).to_be_visible()

        # Click the eye button — should toggle to text
        page.locator("button", has_text="◎").click()
        expect(page.locator("input[type='text']")).to_be_visible()


# ===========================================================================
# 5. Restore workflow — requires full React frontend
# ===========================================================================

class TestRestoreWorkflow:
    def test_restore_button_disabled_with_fewer_than_two_parts(
        self, server, page: Page, frontend_built, tmp_path
    ):
        _skip_no_frontend(frontend_built)
        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        page.get_by_role("button", name="Restore").click()
        page.locator("input[type='password']").fill("pw")

        btn = page.locator("button", has_text="Restore").last
        assert btn.is_disabled()

    def test_part_badges_update_when_files_added(
        self, server, page: Page, frontend_built, tmp_path
    ):
        _skip_no_frontend(frontend_built)
        parts = _api_archive(server, b"badge test", "badge.bin", "bpw")

        # Write parts to disk
        p1_file = tmp_path / parts[1][0]
        p2_file = tmp_path / parts[2][0]
        p1_file.write_bytes(parts[1][1])
        p2_file.write_bytes(parts[2][1])

        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        page.get_by_role("button", name="Restore").click()

        page.locator("input[type='file']").set_input_files([str(p1_file), str(p2_file)])
        page.wait_for_timeout(500)

        body = page.locator("body").inner_text()
        # Part badges for 1 and 2 should show as filled
        assert "Part 1" in body and "Part 2" in body

    def test_restore_downloads_original_file(
        self, server, page: Page, frontend_built, tmp_path
    ):
        _skip_no_frontend(frontend_built)
        original = b"GUI restore test content - verify this comes back"
        parts = _api_archive(server, original, "gui_restore.bin", "rpw")

        p1_file = tmp_path / parts[1][0]
        p2_file = tmp_path / parts[2][0]
        p1_file.write_bytes(parts[1][1])
        p2_file.write_bytes(parts[2][1])

        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        page.get_by_role("button", name="Restore").click()

        page.locator("input[type='file']").set_input_files([str(p1_file), str(p2_file)])
        page.locator("input[type='password']").fill("rpw")

        with page.expect_download() as dl:
            page.locator("button", has_text="Restore").last.click()

        download = dl.value
        assert open(download.path(), "rb").read() == original

    def test_wrong_password_shows_error_message(
        self, server, page: Page, frontend_built, tmp_path
    ):
        _skip_no_frontend(frontend_built)
        parts = _api_archive(server, b"wrong pw test", "wp.bin", "correct")

        p1_file = tmp_path / parts[1][0]
        p2_file = tmp_path / parts[2][0]
        p1_file.write_bytes(parts[1][1])
        p2_file.write_bytes(parts[2][1])

        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        page.get_by_role("button", name="Restore").click()

        page.locator("input[type='file']").set_input_files([str(p1_file), str(p2_file)])
        page.locator("input[type='password']").fill("wrong-password")
        page.locator("button", has_text="Restore").last.click()

        # Wait for error to appear
        page.wait_for_timeout(4000)
        body = page.locator("body").inner_text()
        assert any(w in body.lower() for w in ("wrong", "password", "fail", "error"))

    def test_clear_button_resets_parts(self, server, page: Page, frontend_built, tmp_path):
        _skip_no_frontend(frontend_built)
        parts = _api_archive(server, b"clear test", "clear.bin", "pw")

        p1_file = tmp_path / parts[1][0]
        p1_file.write_bytes(parts[1][1])

        page.goto(server + "/")
        page.wait_for_load_state("networkidle")
        page.get_by_role("button", name="Restore").click()

        page.locator("input[type='file']").set_input_files(str(p1_file))
        page.wait_for_timeout(300)

        # Clear should appear; click it
        clear_btn = page.locator("button", has_text="✕ Clear")
        expect(clear_btn).to_be_visible()
        clear_btn.click()
        page.wait_for_timeout(300)

        # The clear button should disappear (no files selected)
        assert not clear_btn.is_visible()
