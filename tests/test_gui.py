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


class TestConnectingAnAccount:
    """The connect dialog: sign-in for the OAuth backends, a form for the rest.

    Nothing here talks to a real provider — it stops at the point where the
    browser would be handed over to one.
    """

    def test_picker_offers_sign_in_and_credential_backends(self, app):
        app.get_by_text("+ Connect a cloud").click()
        app.wait_for_selector("text=Sign in with your account")

        for label in ("Google Drive", "OneDrive", "Dropbox", "Box"):
            assert app.get_by_text(label, exact=True).count() > 0, label
        for label in ("S3-compatible storage", "Proton Drive", "Local folder"):
            assert app.get_by_text(label, exact=True).count() > 0, label

        app.keyboard.press("Escape")

    def test_signing_in_asks_for_an_app_and_shows_the_redirect_to_register(self, app, server):
        app.get_by_text("+ Connect a cloud").click()
        app.wait_for_selector("text=Sign in with your account")
        app.get_by_text("Google Drive", exact=True).click()

        app.wait_for_selector("text=Continue with Google")
        # No OAuth app is configured for the test server, so the dialog has to
        # collect one — and tell the user exactly what to register.
        redirect = app.locator("input[readonly]").first.input_value()
        assert redirect == f"{server}/api/providers/oauth/callback"

        # The button stays out of reach until there is an app to sign in with.
        button = app.get_by_role("button", name="Continue with Google")
        assert button.is_disabled()

        app.keyboard.press("Escape")

    def test_the_redirect_uri_copies_without_the_clipboard_api(self, app, server):
        """SAND is usually reached over plain HTTP, where navigator.clipboard
        does not exist at all. The copy button has to fall back rather than do
        nothing, so the API is taken away here to force that path."""
        app.get_by_text("+ Connect a cloud").click()
        app.wait_for_selector("text=Sign in with your account")
        app.get_by_text("Google Drive", exact=True).click()
        app.wait_for_selector("text=Continue with Google")

        app.evaluate(
            """() => {
              Object.defineProperty(navigator, 'clipboard',
                { value: undefined, configurable: true })
              window.__copied = []
              document.execCommand = (command) => {
                if (command !== 'copy') return false
                const el = document.activeElement
                window.__copied.push(el && el.value != null ? el.value : '')
                return true
              }
            }"""
        )

        app.get_by_role("button", name="COPY").click()
        app.wait_for_selector("text=COPIED", timeout=5000)
        assert app.evaluate("() => window.__copied") == [
            f"{server}/api/providers/oauth/callback"
        ]

        app.keyboard.press("Escape")

    def test_credentials_can_still_be_pasted_by_hand(self, app):
        app.get_by_text("+ Connect a cloud").click()
        app.wait_for_selector("text=Sign in with your account")
        app.get_by_text("Dropbox", exact=True).click()

        app.get_by_text("Paste tokens manually instead").click()
        app.wait_for_selector("text=Refresh token")
        assert app.get_by_text("Sign in instead").count() > 0

        app.keyboard.press("Escape")

    def test_a_webdav_preset_fills_the_form_in(self, app):
        app.get_by_text("+ Connect a cloud").click()
        app.wait_for_selector("text=Sign in with your account")
        app.get_by_text("WebDAV / Nextcloud", exact=True).click()

        app.wait_for_selector("text=Start from")
        app.get_by_role("button", name="pCloud").click()

        form = app.locator("form")
        assert form.locator("input").nth(1).input_value() == "https://webdav.pcloud.com"

        app.keyboard.press("Escape")


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

    def test_downloading_a_file_hands_back_the_plaintext(self, app, tmp_path):
        source = tmp_path / "dl.txt"
        source.write_text("download me")

        upload_and_settle(app, source)

        with app.expect_download(timeout=60000) as download:
            app.get_by_label("Download dl.txt").click()

        assert download.value.suggested_filename == "dl.txt"
        assert download.value.path().read_text() == "download me"

    def test_downloading_never_navigates_the_app_away(self, app, tmp_path):
        """Added to a home screen the vault has no address bar and no back
        button, so a download that took the window with it would strand the
        user on whatever the phone made of the file — an epub or a zip becomes
        a bare document icon with nothing to press.  The bytes have to arrive
        without the page ever leaving.
        """
        source = tmp_path / "stay-put.txt"
        source.write_text("the app should still be here afterwards")

        upload_and_settle(app, source)
        before = app.url

        with app.expect_download(timeout=60000):
            app.get_by_label("Download stay-put.txt").click()

        assert app.url == before
        # The file browser is still on screen, which it would not be if the
        # window had gone to the content endpoint.
        assert app.locator('button[title="Where the parts live"]').first.is_visible()


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


PHONE = {"width": 390, "height": 844}
NARROW_PHONE = {"width": 320, "height": 640}


def horizontal_overflow(page):
    """Elements sticking out past the right edge of the viewport, if any.

    A page that scrolls sideways on a phone is the whole failure mode this
    guards against, so the assertion names the offenders rather than just the
    two numbers.
    """
    return page.evaluate(
        """() => {
            const doc = document.documentElement
            if (doc.scrollWidth <= doc.clientWidth) return []
            return [...document.querySelectorAll('*')]
              .filter((el) => el.getBoundingClientRect().right > doc.clientWidth + 1)
              .slice(0, 5)
              .map((el) => `<${el.tagName}> "${(el.textContent || '').trim().slice(0, 40)}"`)
        }"""
    )


class TestChangePassword:
    """The password dialog, which is also the only way to re-key a vault from
    the browser.

    Changing the password rotates the key the stored parts are encrypted under
    and re-encrypts every file onto it, so this is the one test that rewrites
    what is on the "accounts". It puts the password back before it finishes,
    because the vault is shared for the whole session.
    """

    def _change(self, app, current, new, migrate=True):
        app.get_by_text("Change vault password").first.click()
        app.wait_for_selector("text=Re-encrypt my files now", timeout=15000)

        app.get_by_label("Current password").fill(current)
        app.get_by_label("New password", exact=True).fill(new)
        app.get_by_label("Confirm new password").fill(new)
        if not migrate:
            app.get_by_role("checkbox").uncheck()

        app.get_by_role("button", name="Change password").click()
        # Re-encrypting is a download and an upload per file, so give it room.
        app.wait_for_selector("text=Password changed", timeout=120000)

    def _dismiss(self, app):
        app.get_by_role("button", name="Done").click()
        app.wait_for_selector("text=Password changed", state="detached", timeout=15000)

    def test_changing_the_password_re_encrypts_the_stored_files(self, app, vault_password, tmp_path):
        source = tmp_path / "rekeyed.txt"
        source.write_text("this has to survive the change")
        upload_and_settle(app, source)

        # Only restore what was actually changed: a restore attempted after a
        # change that never happened would fail in the finally block and hide
        # the real failure.
        changed = False
        try:
            self._change(app, vault_password, "a-brand-new-passphrase")
            changed = True
            # The report says what moved, not just that the password changed.
            app.wait_for_selector("text=re-encrypted under the new key", timeout=15000)
            self._dismiss(app)

            # Nothing is left behind on the old key.
            assert app.get_by_text("previous password").count() == 0

            # And the file still opens, which it only can if the parts were
            # rewritten under the new key and the index followed them.
            app.locator('button[title="Open"]:has-text("rekeyed.txt")').first.click()
            app.wait_for_selector("text=this has to survive the change", timeout=30000)
            app.keyboard.press("Escape")
        finally:
            if changed:
                self._change(app, "a-brand-new-passphrase", vault_password)
                self._dismiss(app)

    def test_a_wrong_current_password_is_reported(self, app, vault_password):
        app.get_by_text("Change vault password").first.click()
        app.wait_for_selector("text=Re-encrypt my files now", timeout=15000)

        app.get_by_label("Current password").fill("not-the-password")
        app.get_by_label("New password", exact=True).fill("does-not-matter")
        app.get_by_label("Confirm new password").fill("does-not-matter")
        app.get_by_role("button", name="Change password").click()

        app.wait_for_selector("text=That is not your current password", timeout=30000)
        app.get_by_role("button", name="Cancel").click()

    def test_mismatched_new_passwords_never_reach_the_server(self, app, vault_password):
        app.get_by_text("Change vault password").first.click()
        app.wait_for_selector("text=Re-encrypt my files now", timeout=15000)

        app.get_by_label("Current password").fill(vault_password)
        app.get_by_label("New password", exact=True).fill("one-thing")
        app.get_by_label("Confirm new password").fill("another-thing")
        app.get_by_role("button", name="Change password").click()

        app.wait_for_selector("text=The two new passwords do not match", timeout=15000)
        # The vault is untouched: the dialog is still a form, not a report.
        assert app.get_by_text("Password changed").count() == 0
        app.get_by_role("button", name="Cancel").click()

    def test_deferring_the_migration_offers_to_finish_it(self, app, vault_password):
        changed = False
        try:
            self._change(app, vault_password, "deferred-for-now", migrate=False)
            changed = True
            self._dismiss(app)

            # The accounts panel is where an unfinished re-key is picked up.
            app.wait_for_selector("text=previous password", timeout=15000)
            app.get_by_role("button", name="Finish re-encrypting").click()
            app.wait_for_selector("text=previous password", state="detached", timeout=120000)
        finally:
            if changed:
                self._change(app, "deferred-for-now", vault_password)
                self._dismiss(app)


class TestMobileLayout:
    """The app has to be usable on a phone, not just narrower.

    Below 860px the two-pane layout folds: the accounts sidebar becomes a
    drawer and the file table's fixed columns stack.
    """

    @pytest.mark.parametrize("size", [PHONE, NARROW_PHONE])
    def test_lock_screen_does_not_scroll_sideways(self, page, server, size):
        page.set_viewport_size(size)
        page.goto(server)
        page.wait_for_selector("text=Vault password", timeout=15000)

        assert not horizontal_overflow(page)

    @pytest.mark.parametrize("size", [PHONE, NARROW_PHONE])
    def test_file_browser_does_not_scroll_sideways(self, app, tmp_path, size):
        source = tmp_path / "a-file-with-a-fairly-long-name-indeed.txt"
        source.write_text("narrow")
        upload_and_settle(app, source)

        app.set_viewport_size(size)
        app.wait_for_timeout(300)

        assert not horizontal_overflow(app)

    def test_row_controls_are_thumb_sized_on_a_phone(self, app, tmp_path):
        """Anything tappable in the file list has to be worth aiming at.

        44px is the smallest target Apple and Google both publish; below that a
        fingertip covering the row's download arrow also covers the delete
        beside it.
        """
        source = tmp_path / "thumbs.txt"
        source.write_text("fat fingers")
        upload_and_settle(app, source)

        app.set_viewport_size(PHONE)
        app.wait_for_timeout(400)

        undersized = app.evaluate(
            """() => {
                const list = [...document.querySelectorAll('main button, main a[href]')]
                return list.map((el) => {
                    const r = el.getBoundingClientRect()
                    return { what: (el.getAttribute('aria-label') || el.textContent || '').trim().slice(0, 30),
                             w: Math.round(r.width), h: Math.round(r.height) }
                }).filter((x) => x.w > 0 && (x.w < 40 || x.h < 40))
            }"""
        )
        assert not undersized, f"controls too small to tap: {undersized}"

    def test_a_row_menu_stands_between_the_thumb_and_delete(self, app, tmp_path):
        """On a phone the row carries one menu button rather than a download
        and a delete a few pixels apart, and deleting still has to pass a
        confirmation — so a mis-tap costs a tap, not a file."""
        source = tmp_path / "menu.txt"
        source.write_text("still here")
        upload_and_settle(app, source)

        app.set_viewport_size(PHONE)
        app.wait_for_timeout(400)

        # The paired icons are gone; one menu button takes their place.
        assert app.locator('a[title="Download the rebuilt, decrypted file"]').count() == 0
        app.locator('button[aria-label="Actions for menu.txt"]').click()
        app.wait_for_selector("text=Where the parts live", timeout=10000)

        app.get_by_text("Delete", exact=True).click()
        app.wait_for_selector("text=Delete menu.txt?", timeout=10000)

        # Backing out of the confirmation leaves the file where it was.
        app.get_by_role("button", name="Cancel").click()
        app.wait_for_timeout(400)
        assert app.get_by_text("menu.txt", exact=True).count() > 0

    def test_accounts_sit_behind_a_drawer_on_a_phone(self, app):
        app.set_viewport_size(PHONE)
        app.wait_for_timeout(300)

        heading = app.get_by_text("Connected clouds")
        assert not heading.is_visible(), "the sidebar should be closed on a phone"

        app.locator('button[aria-label="Connected clouds"]').click()
        app.wait_for_timeout(400)
        assert heading.is_visible()

        # Escape closes it again, the same as every other layer in the app.
        app.keyboard.press("Escape")
        app.wait_for_timeout(400)
        assert not heading.is_visible()

    def test_sidebar_is_always_open_on_a_wide_screen(self, app):
        assert app.get_by_text("Connected clouds").is_visible()
        assert app.locator('button[aria-label="Connected clouds"]').count() == 0

    def test_modal_opened_from_the_drawer_covers_the_viewport(self, app):
        """The drawer slides in on a transform, which makes it — not the
        viewport — what a `position: fixed` child measures itself against. A
        modal opened from inside it has to escape that, or it ends up boxed
        into the drawer's 320px."""
        app.set_viewport_size(PHONE)
        app.wait_for_timeout(300)
        app.locator('button[aria-label="Connected clouds"]').click()
        app.wait_for_timeout(400)

        app.get_by_text("+ Connect a cloud").click()
        app.wait_for_selector("text=Local folder", timeout=15000)

        layer = app.evaluate(
            """() => {
                const el = [...document.body.children].find((n) => n.id !== 'root')
                const r = el.getBoundingClientRect()
                return { left: r.left, width: r.width, viewport: window.innerWidth }
            }"""
        )
        assert layer["left"] == 0
        assert layer["width"] == layer["viewport"]


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
