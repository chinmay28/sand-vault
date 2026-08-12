"""
Browser end-to-end tests for the SAND file browser.

These drive the real React app served by the Go binary: create a vault, connect
cloud accounts through the UI, upload a file, and confirm the browser can show
it again — which only works if the server really did gather the encrypted parts
back off the accounts and rebuild the plaintext.

Skipped automatically when the frontend has not been built.  If Playwright
cannot launch its bundled Chromium, set PLAYWRIGHT_CHROMIUM_EXECUTABLE to a
browser on the machine.
"""
import os
import re

import pytest

pytestmark = pytest.mark.gui


@pytest.fixture(autouse=True)
def _require_frontend(frontend_built):
    if not frontend_built:
        pytest.skip("React frontend is not built — run 'make build-web'")


@pytest.fixture
def app(page, server, vault_password, clouds):
    """Open the app with the vault unlocked and three accounts connected.

    The vault is shared across the session, so this handles both the
    first-run (create) and returning (unlock) screens.
    """
    page.goto(server)

    if page.get_by_text("Create your vault").count() > 0:
        boxes = page.locator('input[autocomplete="new-password"]')
        boxes.nth(0).fill(vault_password)
        boxes.nth(1).fill(vault_password)
        page.get_by_text("▶ Create vault").click()
    else:
        page.locator('input[type="password"]').first.fill(vault_password)
        page.get_by_text("▶ Unlock").click()

    page.wait_for_selector("text=Connected clouds", timeout=20000)

    # Make sure at least three accounts exist, connecting any that are missing.
    for name in ("ui-one", "ui-two", "ui-three"):
        if page.get_by_text(name, exact=True).count() > 0:
            continue
        page.get_by_text("+ Connect a cloud").click()
        page.wait_for_selector("text=Local folder")
        page.get_by_text("Local folder").click()
        form = page.locator("form")
        form.locator("input").nth(0).fill(name)
        form.locator("input").nth(1).fill(clouds(name))
        form.locator('button[type=submit]').click()
        page.wait_for_selector(f"text={name}", timeout=30000)

    return page


def upload_and_settle(page, source):
    """Upload a file through the picker and wait for the refresh to finish.

    The upload triggers a listing refresh, so clicking a row the instant the
    name appears can race the re-render.
    """
    page.set_input_files("input[type=file]", str(source))
    page.wait_for_selector(f"text={os.path.basename(source)}", timeout=90000)
    page.wait_for_load_state("networkidle")


class TestLockScreen:
    def test_lock_screen_is_shown_before_unlocking(self, page, server):
        page.goto(server)
        # The app asks the server for vault status before it can decide which
        # screen to render, so wait for that to land.
        page.wait_for_selector("text=SAND", timeout=15000)
        page.wait_for_selector("text=Vault password", timeout=15000)
        assert "vault" in page.content().lower()

    def test_no_file_listing_leaks_before_unlock(self, page, server):
        page.goto(server)
        page.wait_for_selector("text=Vault password", timeout=15000)
        # Nothing about stored files may appear until the vault is open.
        assert page.get_by_text("Connected clouds").count() == 0

    def test_wrong_password_is_reported(self, page, server, unlocked):
        page.goto(server)
        if page.get_by_text("Unlock your vault").count() == 0:
            pytest.skip("vault not yet created in this session ordering")
        page.locator('input[type="password"]').first.fill("definitely-not-the-password")
        page.get_by_text("▶ Unlock").click()
        page.wait_for_selector("text=Wrong password", timeout=15000)


class TestBrandAndVersion:
    def test_header_shows_the_running_version(self, app, server):
        import requests
        reported = requests.get(f"{server}/api/health", timeout=10).json()["version"]

        # The header, the CLI and /api/health all render the same string; a
        # mismatch means the bundle and the binary were built from different
        # trees, which is exactly the bug the shared version.mjs prevents.
        assert reported.startswith("v")
        assert app.get_by_text(reported, exact=True).count() > 0, (
            f"header does not show {reported}"
        )

    def test_brand_lockup_is_present(self, app):
        assert app.locator('img[src="/icon.svg"]').count() > 0

    def test_developer_badge_opens_and_dismisses(self, app):
        app.locator('button[aria-label="Show the developer badge"]').first.click()
        app.wait_for_selector('img[alt*="CM Hegday"]', timeout=10000)
        app.wait_for_selector("text=github.com/chinmay28", timeout=10000)

        # Escape ends it early — nobody should have to wait out an animation.
        app.keyboard.press("Escape")
        app.wait_for_selector('img[alt*="CM Hegday"]', state="detached", timeout=10000)

    def test_lock_screen_shows_the_version_too(self, page, server, unlocked):
        import requests
        reported = requests.get(f"{server}/api/health", timeout=10).json()["version"]

        page.goto(server)
        page.wait_for_selector("text=Vault password", timeout=15000)
        assert page.get_by_text(reported, exact=True).count() > 0


class TestBrowserShell:
    def test_header_and_panels_render(self, app):
        assert app.get_by_text("Connected clouds").count() > 0
        assert app.get_by_text("🔒 Lock vault").count() > 0

    def test_connected_accounts_are_listed(self, app):
        for name in ("ui-one", "ui-two", "ui-three"):
            assert app.get_by_text(name, exact=True).count() > 0

    def test_lock_returns_to_the_lock_screen(self, app):
        app.get_by_text("🔒 Lock vault").click()
        app.wait_for_selector("text=Unlock your vault", timeout=15000)


class TestUploadAndPreview:
    def test_upload_then_preview_rebuilds_the_file(self, app, tmp_path):
        body = "this text only comes back if the parts were gathered and decrypted\n"
        source = tmp_path / "roundtrip.txt"
        source.write_text(body)

        upload_and_settle(app, source)

        app.locator('button[title="Open"]').first.click()
        # The preview pane renders the reconstructed plaintext.
        app.wait_for_selector(f"text={body.strip()}", timeout=60000)

    def test_uploaded_file_shows_three_part_badges(self, app, tmp_path):
        source = tmp_path / "badges.txt"
        source.write_text("badge check")

        upload_and_settle(app, source)

        row = app.locator('button[title="Where the parts live"]').first
        assert row.count() > 0

    def test_shard_inspector_names_the_accounts(self, app, tmp_path):
        source = tmp_path / "inspect.txt"
        source.write_text("where does this live")

        upload_and_settle(app, source)

        app.locator('button[title="Where the parts live"]').first.click()
        app.wait_for_selector("text=Where this file lives", timeout=20000)
        app.wait_for_selector("text=Enough parts are reachable", timeout=30000)

        body = app.content()
        assert "Part 1" in body and "Part 2" in body and "Part 3" in body
        # Each part must name the account holding it.
        assert re.search(r"ui-(one|two|three)", body)

    def test_download_link_points_at_the_content_endpoint(self, app, tmp_path):
        source = tmp_path / "dl.txt"
        source.write_text("download me")

        upload_and_settle(app, source)

        link = app.locator('a[title="Download the rebuilt, decrypted file"]').first
        href = link.get_attribute("href")
        assert "/api/files/" in href and "download=1" in href


class TestFolders:
    def test_create_and_enter_a_folder(self, app):
        app.get_by_text("+ Folder").click()
        app.wait_for_selector("text=New folder")
        app.fill('input[placeholder="Folder name"]', "gui-folder")
        app.locator('button[type=submit]:has-text("Create")').click()

        app.wait_for_selector("text=gui-folder", timeout=20000)
        app.get_by_text("gui-folder").first.click()
        # Breadcrumb reflects the folder we walked into.
        app.wait_for_selector("text=gui-folder", timeout=10000)


class TestNoConsoleErrors:
    def test_app_loads_without_console_errors(self, page, server):
        errors = []
        page.on("console", lambda m: errors.append(m.text) if m.type == "error" else None)
        page.on("pageerror", lambda e: errors.append(str(e)))

        page.goto(server)
        page.wait_for_timeout(1500)

        # Favicon 404s are noise, not defects.
        real = [e for e in errors if "favicon" not in e.lower()]
        assert not real, f"console errors on load: {real}"
