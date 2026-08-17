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
from playwright.sync_api import expect

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


def connect_cloud(page, name, clouds):
    """Connect one more local-folder account through the UI, if it is not
    already there. Moving something between clouds needs somewhere to move it
    to that it is not already on."""
    if page.get_by_text(name, exact=True).count() > 0:
        return
    page.get_by_text("+ Connect a cloud").click()
    page.wait_for_selector("text=Local folder")
    page.get_by_text("Local folder").click()
    form = page.locator("form")
    form.locator("input").nth(0).fill(name)
    form.locator("input").nth(1).fill(clouds(name))
    form.locator('button[type=submit]').click()
    page.wait_for_selector(f"text={name}", timeout=30000)


def open_vault_setting(page, label):
    """Open one of the vault's settings, which all live behind one menu.

    The accounts panel carries a single **Vault settings** button; the password,
    the default clouds, the film database key and the drive share the list it
    opens. Each still opens the dialog it always did, over the list rather than
    instead of it, so `close_vault_settings` is what puts the app back.
    """
    page.get_by_role("button", name=re.compile(r"^Vault settings")).click()
    menu = page.get_by_role("dialog", name="Vault settings")
    menu.wait_for(timeout=20000)
    menu.get_by_role("button", name=re.compile(rf"^{label}")).click()


def expect_vault_setting(page, label, pattern):
    """Assert what the settings list says a setting is currently set to.

    The standing of each setting — which clouds are the default, whether a film
    key is stored — lives on its line in that list rather than in the panel
    behind it, so reading one means opening the menu.
    """
    page.get_by_role("button", name=re.compile(r"^Vault settings")).click()
    menu = page.get_by_role("dialog", name="Vault settings")
    menu.wait_for(timeout=20000)
    expect(menu.get_by_role("button", name=re.compile(rf"^{label}"))).to_contain_text(
        pattern, timeout=20000)
    close_vault_settings(page)


def close_vault_settings(page):
    """Dismiss the settings list, which is still open under whatever it
    opened."""
    menu = page.get_by_role("dialog", name="Vault settings")
    if menu.count() == 0:
        return
    page.keyboard.press("Escape")
    menu.wait_for(state="detached", timeout=20000)


def select_clouds(page, names):
    """Leave exactly `names` selected in whichever cloud picker is open.

    Every selection starts from something — the vault's default, or a random
    handful — so a test that wants particular clouds has to clear what is there
    before choosing its own.
    """
    while page.get_by_role("checkbox", checked=True).count() > 0:
        page.get_by_role("checkbox", checked=True).first.click()
    for name in names:
        page.get_by_role("checkbox").filter(has_text=name).click()


def upload_and_settle(page, source, choose=None):
    """Upload a file through the picker and wait for the refresh to finish.

    Choosing files opens the destination dialog rather than starting the
    upload: which clouds a file is scattered over is a decision taken per
    upload, so nothing leaves the machine until it is confirmed.  `choose`
    names the accounts to end up selected; left out, whatever the dialog opened
    on is what the file goes to.

    The wait afterwards is for the file's own row rather than for its name: the
    progress card carries the name too, so anything looser returns while the
    parts are still being scattered.
    """
    name = os.path.basename(source)
    page.set_input_files("input[type=file]", str(source))

    confirm = page.get_by_role("button", name=re.compile(r"Upload to \d+ cloud"))
    confirm.wait_for(timeout=20000)
    if choose is not None:
        select_clouds(page, choose)
    confirm.click()

    page.wait_for_selector(f'button[title="Open"]:has-text("{name}")', timeout=90000)
    page.wait_for_load_state("networkidle")


def png_bytes(width, height, rgb):
    """A real PNG, so the browser has something it can decode into a thumbnail.

    Written by hand rather than with an image library: the suite's only job here
    is to give the upload a picture, and a dependency for three chunks of zlib
    is a dependency for three chunks of zlib.
    """
    import struct
    import zlib

    raw = b"".join(b"\x00" + bytes(rgb) * width for _ in range(height))

    def chunk(tag, data):
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body))

    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw))
            + chunk(b"IEND", b""))


def make_folder(page, name):
    """Create a folder in the current one and wait for it to be listed."""
    page.get_by_text("+ Folder").click()
    page.wait_for_selector("text=New folder")
    page.fill('input[placeholder="Folder name"]', name)
    page.locator('button[type=submit]:has-text("Create")').click()
    page.wait_for_selector(f"text={name}", timeout=20000)


def search_on_a_phone(page, query):
    """Type a query into the phone's search field, opening it first.

    A phone has no room for a field beside the toolbar, so the toolbar is one
    bar and the field takes its place while it is being typed into — which
    means there is nothing to type into until ⌕ has been tapped. On a desk the
    field is simply there, and TestSearch's own helper types straight into it.
    """
    page.get_by_label("Search", exact=True).click()
    page.get_by_label("Search files and folders").fill(query)


def listed_files(page):
    """The file names on screen, in the order the browser drew them.

    Read off the per-row Download control, whose spoken name has to say which
    file it belongs to and therefore names every row exactly once.
    """
    labels = page.eval_on_selector_all(
        'button[aria-label^="Download "]',
        "els => els.map((e) => e.getAttribute('aria-label'))",
    )
    return [label[len("Download "):] for label in labels]


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


class TestEditingAnAccount:
    """What a cloud is called, and the colour it wears.

    The colour is the same shade on the account's card and on every shard badge
    for a file it holds, which is what makes "which clouds is this file on" a
    question you answer by eye. Both are stored in the vault, so both survive a
    reload — and neither reaches the account itself.
    """

    def open_editor(self, app, name):
        # The innermost element holding both the account's name and its own Edit
        # button is the card: the panel around it holds every other one too.
        card = (app.locator("div")
                .filter(has=app.get_by_text(name, exact=True))
                .filter(has=app.get_by_role("button", name="Edit"))
                .last)
        card.get_by_role("button", name="Edit").click()
        app.wait_for_selector("text=Edit account", timeout=10000)

    def wait_for_stripe(self, app, name, colour):
        """Saving closes the dialog and asks for the account list again, so the
        card is repainted a beat later — poll rather than race it."""
        for _ in range(40):
            if self.stripe_colour(app, name) == colour:
                return
            app.wait_for_timeout(250)
        assert self.stripe_colour(app, name) == colour

    def stripe_colour(self, app, name):
        """The colour of the card's edge stripe, read off what is drawn."""
        return app.evaluate(
            """(name) => {
                const label = [...document.querySelectorAll('span')]
                  .find((el) => el.textContent === name)
                let el = label
                while (el && getComputedStyle(el).borderLeftWidth !== '3px') el = el.parentElement
                return el ? getComputedStyle(el).borderLeftColor : null
            }""",
            name,
        )

    def test_an_account_can_be_renamed_and_recoloured(self, app, clouds):
        connect_cloud(app, "ui-editable", clouds)

        self.open_editor(app, "ui-editable")
        app.get_by_label("Name").fill("ui-renamed")
        app.get_by_role("radio", name="Mint").click()
        app.get_by_role("button", name="Save").click()

        app.wait_for_selector("text=ui-renamed", timeout=20000)
        self.wait_for_stripe(app, "ui-renamed", "rgb(52, 211, 153)")

        # Stored in the vault rather than held in the tab: both survive a
        # reload, which is the only proof the server was told.
        app.reload()
        app.wait_for_selector("text=ui-renamed", timeout=20000)
        assert app.get_by_text("ui-editable", exact=True).count() == 0
        assert self.stripe_colour(app, "ui-renamed") == "rgb(52, 211, 153)"

        # The dialog opens on the colour that was chosen rather than on
        # whatever the account happens to be wearing.
        self.open_editor(app, "ui-renamed")
        expect(app.get_by_role("radio", name="Mint")).to_have_attribute("aria-checked", "true")

        # And handing the choice back to the browser sticks too. The account
        # may well keep the same shade — that is the automatic assignment doing
        # its job — so what is checked here is that the choice itself is gone.
        app.get_by_role("radio", name="Automatic").click()
        app.get_by_role("button", name="Save").click()
        app.wait_for_selector("text=Edit account", state="detached", timeout=20000)

        app.reload()
        app.wait_for_selector("text=ui-renamed", timeout=20000)
        self.open_editor(app, "ui-renamed")
        expect(app.get_by_role("radio", name="Automatic")).to_have_attribute("aria-checked", "true")

        # Put the name back before leaving. The vault is shared across this
        # session, and an account's folder on disk is named after what it was
        # called when it was connected — so a test that walks away from a
        # renamed account leaves every later test looking for parts in a
        # directory that does not exist.
        app.get_by_label("Name").fill("ui-editable")
        app.get_by_role("button", name="Save").click()
        app.wait_for_selector("text=ui-editable", timeout=20000)

    def test_the_full_palette_is_one_disclosure_away(self, app, clouds):
        """Twelve named colours are the shortlist. Every shade of every hue is
        behind a disclosure, and the dialog reopens on whatever was picked
        there rather than showing nothing selected."""
        connect_cloud(app, "ui-shaded", clouds)

        self.open_editor(app, "ui-shaded")
        expect(app.get_by_role("radio", name="Orchid deep")).to_have_count(0)

        app.get_by_role("button", name="All shades").click()
        app.get_by_role("radio", name="Orchid deep").click()
        app.get_by_role("button", name="Save").click()
        app.wait_for_selector("text=Edit account", state="detached", timeout=20000)

        self.wait_for_stripe(app, "ui-shaded", "rgb(217, 70, 239)")

        # A shade the shortlist does not show opens the palette on its own.
        self.open_editor(app, "ui-shaded")
        expect(app.get_by_role("radio", name="Orchid deep")).to_have_attribute(
            "aria-checked", "true")
        app.keyboard.press("Escape")

    def test_a_name_another_account_already_has_is_refused(self, app):
        self.open_editor(app, "ui-one")
        app.get_by_label("Name").fill("ui-two")
        app.get_by_role("button", name="Save").click()

        app.wait_for_selector("text=already connected", timeout=20000)
        # The dialog stays open on the failed edit rather than closing over it.
        assert app.get_by_text("Edit account").count() > 0
        app.keyboard.press("Escape")


class TestPickingAFolder:
    """A local folder is chosen by walking to it, not by typing it out.

    The path belongs to the machine running SAND, which on a phone is not the
    machine holding the keyboard — so the dialog browses the server's own
    folders and hands back the one that was picked.
    """

    def open_local_form(self, app):
        app.get_by_text("+ Connect a cloud").click()
        app.wait_for_selector("text=Local folder")
        app.get_by_text("Local folder").click()
        return app.locator("form")

    def test_browsing_fills_the_path_in_and_connects(self, app, clouds):
        base = clouds("picker")
        os.makedirs(os.path.join(base, "drive"), exist_ok=True)

        form = self.open_local_form(app)
        # Typed in as a starting point; everything after this is clicking.
        form.locator("input").nth(1).fill(base)
        app.get_by_role("button", name="BROWSE…").click()

        app.wait_for_selector("text=Choose the directory", timeout=15000)
        app.get_by_text("drive", exact=True).click()
        app.get_by_label("New folder inside it (optional)").fill("parts")
        app.get_by_role("button", name="Use this folder").click()

        chosen = os.path.join(base, "drive", "parts")
        assert form.locator("input").nth(1).input_value() == chosen

        form.locator("input").nth(0).fill("picked-folder")
        form.locator("button[type=submit]").click()

        app.wait_for_selector("text=picked-folder", timeout=30000)
        # Connecting created the folder that was named but did not exist.
        assert os.path.isdir(chosen)

    def test_a_folder_that_is_not_there_yet_opens_at_its_nearest_parent(self, app, clouds):
        base = clouds("picker-missing")

        form = self.open_local_form(app)
        form.locator("input").nth(1).fill(os.path.join(base, "not-yet"))
        app.get_by_role("button", name="BROWSE…").click()
        app.wait_for_selector("text=Choose the directory", timeout=15000)

        # The picker opens on the folder that exists, keeping the rest as the
        # folder to create — so confirming gives back the path as typed.
        expect(app.get_by_label("New folder inside it (optional)")).to_have_value("not-yet")
        app.get_by_role("button", name="Use this folder").click()
        assert form.locator("input").nth(1).input_value() == os.path.join(base, "not-yet")

        app.keyboard.press("Escape")

    def test_escape_closes_the_picker_without_losing_the_form(self, app, clouds):
        form = self.open_local_form(app)
        form.locator("input").nth(0).fill("half-filled")
        app.get_by_role("button", name="BROWSE…").click()
        app.wait_for_selector("text=Choose the directory", timeout=15000)

        app.keyboard.press("Escape")
        app.wait_for_timeout(400)

        assert app.get_by_text("Choose the directory").count() == 0
        assert form.locator("input").nth(0).input_value() == "half-filled"

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

        row = app.locator('button[title="Where the shards live"]').first
        assert row.count() > 0

    def test_each_account_wears_a_colour_of_its_own(self, app, tmp_path):
        """A shard badge and the sidebar card for the account holding that shard
        are the same colour, and no two accounts share one — which is the whole
        of how a row says which three clouds a file went to."""
        source = tmp_path / "colours.txt"
        source.write_text("which cloud holds which part")

        upload_and_settle(app, source, choose=["ui-one", "ui-two", "ui-three"])

        row = app.locator('button[title="Open"]', has_text="colours.txt").locator("xpath=..")
        background = "el => getComputedStyle(el).backgroundColor"
        colours = {}

        for name in ("ui-one", "ui-two", "ui-three"):
            # The swatch is the only titled span before the account's name on
            # its card; the kind icon beside it carries no title.
            swatch = app.get_by_text(name, exact=True).first.locator(
                "xpath=preceding-sibling::span[@title]"
            )
            colours[name] = swatch.evaluate(background)

            badge = row.get_by_title(re.compile(rf"Shard \d on {name}$"))
            assert badge.count() == 1
            assert badge.evaluate(background) == colours[name]

        assert len(set(colours.values())) == 3

    def test_shard_inspector_names_the_accounts(self, app, tmp_path):
        source = tmp_path / "inspect.txt"
        source.write_text("where does this live")

        upload_and_settle(app, source)

        app.locator('button[title="Where the shards live"]').first.click()
        app.wait_for_selector("text=Where this file lives", timeout=20000)
        app.wait_for_selector("text=Enough parts are reachable", timeout=30000)

        body = app.content()
        assert "Shard 1" in body and "Shard 2" in body and "Shard 3" in body
        # Each part must name the account holding it.
        assert re.search(r"ui-(one|two|three)", body)

    def test_moving_a_file_to_other_clouds_moves_only_what_it_has_to(self, app, tmp_path, clouds):
        """The parts inspector prices the change before it happens, and moving
        one cloud out of three leaves the other two parts exactly where they
        are."""
        connect_cloud(app, "ui-four", clouds)

        source = tmp_path / "travel.txt"
        source.write_text("carried, not rebuilt")
        upload_and_settle(app, source, choose=["ui-one", "ui-two", "ui-three"])

        # Reached through the badges, beside the read-out of where the parts
        # are now — the row itself has only three controls' worth of room.
        travel = app.locator('button[title="Open"]', has_text="travel.txt").locator("xpath=..")
        travel.locator('button[title="Where the shards live"]').click()
        app.wait_for_selector("text=Where this file lives", timeout=20000)
        app.get_by_role("button", name=re.compile("Move to other clouds")).click()
        app.wait_for_selector("text=Move travel.txt", timeout=20000)

        # The clouds it is already on are what the dialog opens on, so asking
        # for them again is asking for nothing.
        app.wait_for_selector("text=Already there", timeout=20000)

        # Swap the third for the fourth: one shard, and only one, travels —
        # the count of clouds has not changed, so the scheme has not either and
        # nothing is rebuilt.
        select_clouds(app, ["ui-one", "ui-two", "ui-four"])
        app.wait_for_selector("text=1 shard to move", timeout=20000)

        app.get_by_role("button", name=re.compile("Move the shards")).click()
        app.wait_for_selector("text=Moved 1 shard", timeout=90000)
        app.get_by_role("button", name="Done").click()

        # The row now names the fourth cloud and no longer names the third.
        # Waited for rather than read straight off: closing the dialog is what
        # asks the listing to refresh.
        row = app.locator('button[title="Open"]', has_text="travel.txt").locator("xpath=..")
        expect(row.get_by_title(re.compile(r"Shard \d on ui-four$"))).to_have_count(1, timeout=20000)
        expect(row.get_by_title(re.compile(r"Shard \d on ui-three$"))).to_have_count(0)
        expect(row.get_by_title(re.compile(r"Shard \d on ui-(one|two)$"))).to_have_count(2)

        # And it still rebuilds.
        app.locator('button[title="Open"]', has_text="travel.txt").click()
        app.wait_for_selector("text=carried, not rebuilt", timeout=60000)

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
        assert app.locator('button[title="Where the shards live"]').first.is_visible()


class TestPdfPreview:
    """A PDF is drawn into the page by pdf.js rather than handed to the
    browser's own viewer, so the same document with the same controls comes up
    on a desktop and on a phone — where a framed PDF used to be a blank box and
    the app had to apologise instead of showing it.
    """

    def _upload(self, app, design_pdf, tmp_path, name):
        source = tmp_path / name
        source.write_bytes(open(design_pdf, "rb").read())
        upload_and_settle(app, source)

    def _open(self, app, name):
        app.locator(f'button[title="Open"]:has-text("{name}")').first.click()
        canvas = app.locator("[data-pdf-preview] canvas")
        # Nothing is drawn until the parts are gathered and the page rendered.
        expect(canvas).to_be_visible(timeout=90000)
        return canvas

    def _painted(self, canvas):
        """The drawn pixels, so a redraw can be told from a stale canvas."""
        return canvas.evaluate("el => el.toDataURL()")

    def test_a_pdf_renders_its_first_page(self, app, design_pdf, tmp_path):
        # A renderer reaching for a font pack or a character map it was never
        # given would say so here — and reaching for one over the network is
        # the third-party request this app promises never to make.
        noise = []
        app.on("console", lambda m: noise.append(m.text) if m.type == "error" else None)
        app.on("pageerror", lambda e: noise.append(str(e)))

        self._upload(app, design_pdf, tmp_path, "rendered.pdf")
        canvas = self._open(app, "rendered.pdf")

        box = canvas.bounding_box()
        assert box["width"] > 100 and box["height"] > 100, box
        # 13 pages, and the viewer says so — it read the document rather than
        # drawing whatever the first bytes happened to decode to.
        expect(app.get_by_text(re.compile(r"Page 1 / 13"))).to_be_visible()
        assert not noise, f"console errors while rendering: {noise}"

    def test_turning_a_page_draws_the_next_one(self, app, design_pdf, tmp_path):
        self._upload(app, design_pdf, tmp_path, "paged.pdf")
        canvas = self._open(app, "paged.pdf")
        first = self._painted(canvas)

        app.get_by_label("Next page").click()
        expect(app.get_by_text(re.compile(r"Page 2 / 13"))).to_be_visible(timeout=60000)
        app.wait_for_timeout(1200)

        assert self._painted(canvas) != first, "page 2 shows page 1's pixels"

        # And back, which is the same journey with the keyboard.
        app.keyboard.press("ArrowLeft")
        expect(app.get_by_text(re.compile(r"Page 1 / 13"))).to_be_visible(timeout=60000)

    def test_a_phone_gets_the_document_not_an_apology(self, app, design_pdf, tmp_path):
        self._upload(app, design_pdf, tmp_path, "phone.pdf")

        app.set_viewport_size(PHONE)
        app.wait_for_timeout(400)
        canvas = self._open(app, "phone.pdf")

        assert app.get_by_text("PDFs do not preview reliably").count() == 0
        # Fitted to the dialog rather than spilling out of it.
        box = canvas.bounding_box()
        assert box["width"] > 100, box
        assert box["width"] <= PHONE["width"], box
        assert not horizontal_overflow(app)

        # Zooming is what makes a dense page readable on a screen this size,
        # and the page it hands back is a bigger one, not the same one stretched.
        before = canvas.evaluate("el => el.width")
        app.get_by_label("Zoom in").click()
        app.wait_for_timeout(1500)
        assert canvas.evaluate("el => el.width") > before

        app.set_viewport_size({"width": 1280, "height": 900})


class TestStreamingToAPlayer:
    """Watching a film means handing VLC an address, and VLC has none of what
    authenticates this app.  The row mints a link standing for that one file,
    which is the only credential a player can actually be given — so the tests
    here follow the address the dialog shows with no cookie at all, the way VLC
    would.
    """

    def _open_dialog(self, app, tmp_path, name="clip.mp4"):
        source = tmp_path / name
        source.write_bytes(b"not really a film, but it streams like one")
        upload_and_settle(app, source)

        app.get_by_label(f"Stream {name}").click()
        app.get_by_role("heading", name="Stream to a player").wait_for(timeout=20000)
        address = app.get_by_label("Stream address")
        address.wait_for(timeout=20000)
        return source, address.input_value()

    def test_only_what_a_player_is_for_offers_one(self, app, tmp_path):
        film = tmp_path / "offered.mp4"
        film.write_bytes(b"a film")
        upload_and_settle(app, film)

        notes = tmp_path / "not-offered.txt"
        notes.write_text("a text file has nothing to stream")
        upload_and_settle(app, notes)

        assert app.get_by_label("Stream offered.mp4").count() == 1
        assert app.get_by_label("Stream not-offered.txt").count() == 0

    def test_the_address_plays_without_the_session(self, app, tmp_path, server):
        import requests

        source, address = self._open_dialog(app, tmp_path, "plays.mp4")

        assert address.startswith(server), f"address {address!r} is not on this origin"
        assert "/stream/" in address
        # The name is the last segment because a player picks its demuxer off
        # the extension before a single byte arrives.
        assert address.endswith("plays.mp4")

        # A bare requests call carries no session cookie, which is the whole
        # point: this is what VLC has.
        played = requests.get(address, timeout=30)
        assert played.status_code == 200
        assert played.content == source.read_bytes()

        # And it seeks, which is what makes it worth streaming rather than
        # downloading first.
        ranged = requests.get(address, headers={"Range": "bytes=4-9"}, timeout=30)
        assert ranged.status_code == 206
        assert ranged.content == source.read_bytes()[4:10]

        app.keyboard.press("Escape")

    def test_the_address_copies_without_the_clipboard_api(self, app, tmp_path):
        """The other half of the ask: getting the path onto the clipboard
        without transcribing it.  SAND is usually reached over plain HTTP,
        where navigator.clipboard does not exist, so the API is taken away here
        to force the fallback the copy button really runs on."""
        _, address = self._open_dialog(app, tmp_path, "copied.mp4")

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
        assert app.evaluate("() => window.__copied") == [address]

        app.keyboard.press("Escape")

    def test_a_desktop_is_handed_a_playlist_naming_the_address(self, app, tmp_path):
        """A desktop browser has no URL scheme to reach VLC through, so the
        handoff there is the playlist file VLC registers itself for."""
        _, address = self._open_dialog(app, tmp_path, "playlist.mp4")

        with app.expect_download(timeout=30000) as download:
            app.get_by_role("button", name="▶ Open in VLC").click()

        assert download.value.suggested_filename == "playlist.mp4.m3u"
        body = download.value.path().read_text()
        assert body.startswith("#EXTM3U")
        assert address in body

        app.keyboard.press("Escape")

    def test_locking_the_vault_voids_a_link_already_handed_out(self, app, tmp_path, server):
        import requests

        _, address = self._open_dialog(app, tmp_path, "voided.mp4")
        assert requests.get(address, timeout=30).status_code == 200

        app.keyboard.press("Escape")
        app.get_by_label("Lock vault").click()
        app.wait_for_selector("text=Vault password", timeout=20000)

        # The keys have left memory, so the link goes with them rather than
        # failing one request at a time.
        assert requests.get(address, timeout=30).status_code == 404


class TestChoosingClouds:
    """Which clouds a file is scattered over is a decision, not a detail.

    Every upload passes through the picker, which opens on the vault's default
    clouds — or, with none set, on three chosen at random — and can be changed
    before anything leaves the machine.
    """

    def _open_picker(self, app, tmp_path, name="picked.txt"):
        source = tmp_path / name
        source.write_text("choose where this lives")
        app.set_input_files("input[type=file]", str(source))
        app.wait_for_selector("text=/Upload to \\d+ cloud/", timeout=20000)
        return source

    def _set_default_clouds(self, app, accounts):
        """Make `accounts` — and only those — the vault's default clouds."""
        open_vault_setting(app, "Default clouds")
        dialog = app.get_by_role("heading", name="Default clouds")
        dialog.wait_for(timeout=20000)
        select_clouds(app, accounts)
        app.get_by_role("button", name="Save default").click()
        dialog.wait_for(state="detached", timeout=20000)
        close_vault_settings(app)

    def test_the_picker_offers_every_connected_cloud(self, app, tmp_path):
        self._open_picker(app, tmp_path, "offered.txt")

        for name in ("ui-one", "ui-two", "ui-three"):
            assert app.get_by_role("checkbox").filter(has_text=name).count() == 1
        # A file has three parts, so three clouds start selected however many
        # are connected — and the button says which it is going to.
        assert app.get_by_role("checkbox", checked=True).count() == 3
        assert app.get_by_role("button", name="↑ Upload to 3 clouds").count() == 1

    def test_nothing_is_uploaded_until_the_picker_is_confirmed(self, app, tmp_path):
        self._open_picker(app, tmp_path, "cancelled.txt")
        app.get_by_role("button", name="Cancel").click()

        app.wait_for_selector("text=/Upload to \\d+ cloud/", state="detached", timeout=20000)
        assert app.get_by_text("cancelled.txt").count() == 0

    def test_deselecting_a_cloud_keeps_the_file_off_it(self, app, tmp_path):
        source = tmp_path / "two-clouds.txt"
        source.write_text("only two clouds may hold this")

        upload_and_settle(app, source, choose=["ui-one", "ui-two"])

        # Each badge in the row names the account holding that part, so the
        # row itself says where the file did and did not go.
        row = app.locator('button[title="Open"]', has_text="two-clouds.txt").locator("xpath=..")
        assert row.get_by_title(re.compile(r"Shard \d on ui-three")).count() == 0
        assert row.get_by_title("Shard 3 not stored").count() == 1

    def test_a_default_is_marked_and_preselected(self, app, tmp_path):
        self._set_default_clouds(app, ["ui-one", "ui-two"])
        try:
            expect_vault_setting(app, "Default clouds", re.compile(r"2 of \d+"))
            # The accounts carrying the default say so on their own card.
            assert app.get_by_text("default", exact=True).count() == 2

            self._open_picker(app, tmp_path, "defaulted.txt")
            assert app.get_by_role("button", name="↑ Upload to 2 clouds").count() == 1
            assert app.get_by_text("Your default clouds are selected").count() == 1
            app.get_by_role("button", name="Cancel").click()
            app.wait_for_selector("text=/Upload to \\d+ cloud/", state="detached", timeout=20000)
        finally:
            # Back to picking per upload, so the rest of the suite is unaffected.
            open_vault_setting(app, "Default clouds")
            dialog = app.get_by_role("heading", name="Default clouds")
            dialog.wait_for(timeout=20000)
            app.get_by_role("button", name="Pick per upload").click()
            dialog.wait_for(state="detached", timeout=20000)
            close_vault_settings(app)

        expect_vault_setting(app, "Default clouds", "3 per upload")
        assert app.get_by_text("default", exact=True).count() == 0


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


class TestMovingBetweenFolders:
    """Moving a file or a folder somewhere else in the vault.

    The other Move changes which clouds hold the shards and copies bytes to do
    it. This one is an index change: what has to be proved is that the file
    turns up in the new folder, comes back out of it whole, and that its shards
    never left the accounts they were scattered to.
    """

    def part_owners(self, page, name):
        """Which account holds each shard of a listed file, as its row says."""
        row = page.locator('button[title="Open"]', has_text=name).locator("xpath=..")
        return sorted(row.locator("span[title^='Shard ']").evaluate_all(
            "els => els.map((e) => e.getAttribute('title'))"))

    def test_a_file_moves_folder_without_its_parts_moving(self, app, tmp_path):
        make_folder(app, "move-from")
        make_folder(app, "move-into")
        app.get_by_text("move-from").first.click()
        app.wait_for_load_state("networkidle")

        source = tmp_path / "carried.txt"
        source.write_text("carried, not rebuilt")
        upload_and_settle(app, source)
        before = self.part_owners(app, "carried.txt")
        assert len(before) == 3

        app.locator('button[aria-label="Move carried.txt to another folder"]').click()
        dialog = app.get_by_role("dialog", name="Move to another folder")
        dialog.wait_for(timeout=20000)

        # It opens on the folder the file is in, which is nowhere to move to.
        expect(dialog.get_by_text("carried.txt is already here")).to_have_count(1)
        expect(dialog.get_by_role("button", name=re.compile("Move here"))).to_be_disabled()

        dialog.get_by_role("button", name="Up to /").click()
        dialog.get_by_role("button", name="Into /move-into").click()
        expect(dialog.get_by_text("1 item would move here")).to_have_count(1)

        dialog.get_by_role("button", name=re.compile("Move here")).click()
        app.wait_for_selector("text=1 moved to /move-into", timeout=30000)
        dialog.get_by_role("button", name="Done").click()

        # Gone from where it was, and readable where it went.
        app.wait_for_selector("text=This folder is empty", timeout=20000)
        app.locator('button[aria-label="Up"]').click()
        app.get_by_text("move-into").first.click()
        app.wait_for_selector("text=carried.txt", timeout=20000)
        assert self.part_owners(app, "carried.txt") == before

    def test_a_folder_takes_everything_in_it_and_cannot_go_inside_itself(self, app, tmp_path):
        make_folder(app, "nest-outer")
        make_folder(app, "nest-inner")
        app.get_by_text("nest-outer").first.click()
        app.wait_for_load_state("networkidle")

        source = tmp_path / "deep.txt"
        source.write_text("still here afterwards")
        upload_and_settle(app, source)
        app.locator('button[aria-label="Up"]').click()
        app.wait_for_selector("text=nest-inner", timeout=20000)

        app.locator('button[aria-label="Move nest-outer to another folder"]').click()
        dialog = app.get_by_role("dialog", name="Move to another folder")
        dialog.wait_for(timeout=20000)

        # The folder being moved is still drawn — the tree would disagree with
        # the listing behind it otherwise — but it is not somewhere to go.
        expect(dialog.get_by_role("button", name="Into /nest-outer")).to_be_disabled()

        dialog.get_by_role("button", name="Into /nest-inner").click()
        dialog.get_by_role("button", name=re.compile("Move here")).click()
        app.wait_for_selector("text=1 moved to /nest-inner", timeout=30000)
        dialog.get_by_role("button", name="Done").click()

        app.get_by_text("nest-inner").first.click()
        app.wait_for_selector("text=nest-outer", timeout=20000)
        app.get_by_text("nest-outer").first.click()
        app.wait_for_selector("text=deep.txt", timeout=20000)

    def test_a_selection_moves_in_one_go(self, app, tmp_path):
        make_folder(app, "bulk-from")
        make_folder(app, "bulk-into")
        app.get_by_text("bulk-from").first.click()
        app.wait_for_load_state("networkidle")

        for name in ("bulk-a.txt", "bulk-b.txt"):
            source = tmp_path / name
            source.write_text(name)
            upload_and_settle(app, source)

        app.locator('button[aria-label="Select files and folders"]').click()
        app.get_by_role("button", name="Select all").click()
        app.wait_for_selector("text=2 of 2 selected", timeout=10000)
        app.get_by_role("button", name=re.compile("^→ Folder")).click()

        dialog = app.get_by_role("dialog", name="Move 2 items")
        dialog.wait_for(timeout=20000)
        dialog.get_by_role("button", name="Up to /").click()
        dialog.get_by_role("button", name="Into /bulk-into").click()
        dialog.get_by_role("button", name=re.compile("Move here")).click()

        app.wait_for_selector("text=2 moved to /bulk-into", timeout=60000)
        dialog.get_by_role("button", name="Done").click()
        app.wait_for_selector("text=This folder is empty", timeout=20000)


class TestRenaming:
    """Renaming a file or a folder.

    The cheapest thing in the vault: a name is a field in the encrypted index,
    and a file's shards are named after the file rather than after its name — so
    what has to be proved is that the row answers to the new name and the shards
    are exactly where they were.
    """

    def shard_owners(self, page, name):
        """Which account holds each shard of a listed file, as its row says."""
        row = page.locator('button[title="Open"]', has_text=name).locator("xpath=..")
        return sorted(row.locator("span[title^='Shard ']").evaluate_all(
            "els => els.map((e) => e.getAttribute('title'))"))

    def test_a_file_is_renamed_without_its_shards_moving(self, app, tmp_path):
        make_folder(app, "rename-files")
        app.get_by_text("rename-files").first.click()
        app.wait_for_load_state("networkidle")

        source = tmp_path / "draft.txt"
        source.write_text("the contents do not change")
        upload_and_settle(app, source)
        before = self.shard_owners(app, "draft.txt")

        app.locator('button[aria-label="Actions for draft.txt"]').click()
        app.get_by_text("Rename", exact=True).click()
        dialog = app.get_by_role("dialog", name="Rename file")
        dialog.wait_for(timeout=20000)

        # It opens on the name with the stem selected, so typing replaces the
        # words and keeps the extension.
        field = dialog.get_by_label("New file name")
        assert field.input_value() == "draft.txt"
        assert app.evaluate("() => document.activeElement.selectionEnd") == len("draft")

        # A name is one segment; a path is a move, and the dialog says so
        # rather than quietly making a folder.
        field.fill("elsewhere/final.txt")
        expect(dialog.get_by_role("button", name="Rename")).to_be_disabled()
        expect(dialog.get_by_text("A name is one segment", exact=False)).to_have_count(1)

        field.fill("published.txt")
        dialog.get_by_role("button", name="Rename").click()

        app.wait_for_selector("text=published.txt", timeout=20000)
        assert app.get_by_text("draft.txt", exact=True).count() == 0
        assert self.shard_owners(app, "published.txt") == before

        # And the name is the vault's to refuse: a second file cannot take it.
        second = tmp_path / "other.txt"
        second.write_text("another one")
        upload_and_settle(app, second)
        app.locator('button[aria-label="Actions for other.txt"]').click()
        app.get_by_text("Rename", exact=True).click()
        dialog = app.get_by_role("dialog", name="Rename file")
        dialog.wait_for(timeout=20000)
        dialog.get_by_label("New file name").fill("published.txt")
        dialog.get_by_role("button", name="Rename").click()
        expect(dialog.get_by_text("already exists", exact=False)).to_have_count(1, timeout=20000)
        dialog.get_by_role("button", name="Cancel").click()
        assert app.get_by_text("other.txt", exact=True).count() > 0

    def test_a_folder_is_renamed_with_everything_in_it(self, app, tmp_path):
        make_folder(app, "rename-outer")
        app.get_by_text("rename-outer").first.click()
        app.wait_for_load_state("networkidle")
        source = tmp_path / "inside.txt"
        source.write_text("still here afterwards")
        upload_and_settle(app, source)
        app.locator('button[aria-label="Up"]').click()
        app.wait_for_selector("text=rename-outer", timeout=20000)

        app.locator('button[aria-label="Actions for rename-outer"]').click()
        app.get_by_text("Rename", exact=True).click()
        dialog = app.get_by_role("dialog", name="Rename folder")
        dialog.wait_for(timeout=20000)
        dialog.get_by_label("New folder name").fill("rename-renamed")
        dialog.get_by_role("button", name="Rename").click()

        app.wait_for_selector("text=rename-renamed", timeout=20000)
        assert app.get_by_text("rename-outer", exact=True).count() == 0

        # Everything inside came along.
        app.get_by_text("rename-renamed").first.click()
        app.wait_for_selector("text=inside.txt", timeout=20000)


class TestFolderPictures:
    """A folder can be given a picture of something inside it.

    Nothing is picked for you — a folder keeps its icon until somebody says
    otherwise. Nothing is stored to do it either: the folder points at a file
    that already has a thumbnail and draws that file's own picture, which is why
    choosing, changing and unchoosing are all free.
    """

    def picture(self, page, name):
        """The address of the picture a folder row is drawn with, or None."""
        row = page.locator('button[title="Open folder"]', has_text=name).locator("xpath=..")
        img = row.locator("img")
        return img.first.get_attribute("src") if img.count() else None

    def test_a_folder_wears_a_picture_only_once_one_is_picked(self, app, tmp_path):
        make_folder(app, "art-library")
        app.get_by_text("art-library").first.click()
        app.wait_for_load_state("networkidle")
        make_folder(app, "art-trilogy")
        app.get_by_text("art-trilogy").first.click()
        app.wait_for_load_state("networkidle")

        for name, rgb in (("art-one.png", (200, 60, 40)),
                          ("art-two.png", (40, 90, 200)),
                          ("art-three.png", (230, 170, 30))):
            source = tmp_path / name
            source.write_bytes(png_bytes(120, 180, rgb))
            upload_and_settle(app, source)

        app.locator('button[aria-label="Up"]').click()
        app.wait_for_selector("text=art-trilogy", timeout=20000)

        # Nothing was picked for it, so it is still a folder icon — but the
        # control to give it a picture is there, because there is something to
        # choose from.
        assert self.picture(app, "art-trilogy") is None
        picker = app.locator('button[aria-label="Choose the picture for art-trilogy"]')
        assert picker.count() == 1

        picker.click()
        dialog = app.get_by_role("dialog", name="Folder picture")
        dialog.wait_for(timeout=20000)
        expect(dialog.get_by_text("No picture", exact=False)).to_have_count(1)

        choices = dialog.get_by_role("button", name=re.compile("^Draw this folder with"))
        expect(choices).to_have_count(3, timeout=10000)
        choices.first.click()
        app.wait_for_selector("text=art-trilogy", timeout=20000)
        app.wait_for_timeout(800)

        chosen = self.picture(app, "art-trilogy")
        assert chosen is not None, "the choice did not reach the row"

        # It stays put across a redraw, being recorded rather than guessed.
        app.get_by_role("button", name="Refresh").click()
        app.wait_for_timeout(600)
        assert self.picture(app, "art-trilogy") == chosen

        # And taking it away puts the icon back rather than some other picture.
        app.locator('button[aria-label="Choose the picture for art-trilogy"]').click()
        dialog = app.get_by_role("dialog", name="Folder picture")
        dialog.wait_for(timeout=20000)
        dialog.get_by_role("button", name="Use no picture").click()
        app.wait_for_timeout(800)
        assert self.picture(app, "art-trilogy") is None

    def test_a_folder_with_nothing_picturable_is_not_offered_a_picture(self, app, tmp_path):
        make_folder(app, "art-plain")
        app.get_by_text("art-plain").first.click()
        app.wait_for_load_state("networkidle")
        source = tmp_path / "notes.txt"
        source.write_text("no picture here")
        upload_and_settle(app, source)

        app.locator('button[aria-label="Up"]').click()
        app.wait_for_selector("text=art-plain", timeout=20000)

        assert self.picture(app, "art-plain") is None
        # Nothing inside has a picture, so nothing offers to choose one.
        assert app.locator('button[aria-label="Choose the picture for art-plain"]').count() == 0


class TestNavigationControls:
    """Back, Forward and Up — the trail of folders walked through.

    The breadcrumb only ever pointed up the tree it is showing. These three
    remember where you have been, which is the difference between a listing and
    a file manager.
    """

    def test_back_and_forward_walk_the_trail(self, app):
        make_folder(app, "nav-trail")

        # Nothing has been walked yet, so neither arrow leads anywhere.
        expect(app.locator('button[aria-label="Back"]')).to_be_disabled()
        expect(app.locator('button[aria-label="Forward"]')).to_be_disabled()

        app.get_by_text("nav-trail").first.click()
        app.wait_for_load_state("networkidle")
        expect(app.locator('button[aria-label="Back"]')).to_be_enabled()
        expect(app.locator('button[aria-label="Forward"]')).to_be_disabled()

        app.locator('button[aria-label="Back"]').click()
        app.wait_for_selector("text=nav-trail", timeout=20000)
        expect(app.locator('button[aria-label="Back"]')).to_be_disabled()
        # Going back is what gives Forward somewhere to go.
        expect(app.locator('button[aria-label="Forward"]')).to_be_enabled()

        app.locator('button[aria-label="Forward"]').click()
        app.wait_for_selector("text=This folder is empty", timeout=20000)

    def test_up_climbs_out_of_a_folder(self, app):
        make_folder(app, "nav-outer")
        app.get_by_text("nav-outer").first.click()
        app.wait_for_load_state("networkidle")
        make_folder(app, "nav-inner")
        app.get_by_text("nav-inner").first.click()
        app.wait_for_load_state("networkidle")

        app.locator('button[aria-label="Up"]').click()
        app.wait_for_selector("text=nav-inner", timeout=20000)
        app.locator('button[aria-label="Up"]').click()
        app.wait_for_selector("text=nav-outer", timeout=20000)

        # The root has nowhere above it.
        expect(app.locator('button[aria-label="Up"]')).to_be_disabled()

    def test_alt_arrows_do_the_same_from_the_keyboard(self, app):
        make_folder(app, "nav-keys")
        app.get_by_text("nav-keys").first.click()
        app.wait_for_load_state("networkidle")

        app.keyboard.press("Alt+ArrowLeft")
        app.wait_for_selector("text=nav-keys", timeout=20000)
        expect(app.locator('button[aria-label="Back"]')).to_be_disabled()

        app.keyboard.press("Alt+ArrowRight")
        app.wait_for_load_state("networkidle")
        expect(app.locator('button[aria-label="Forward"]')).to_be_disabled()

    def test_a_folder_that_goes_away_drops_the_browser_at_the_root(self, app):
        """A folder deleted from under the browser is not a step to walk back
        into, so falling back to the root replaces it rather than adding to the
        trail — and Up, which only the root disables, says which one we are on."""
        make_folder(app, "nav-doomed")
        app.get_by_text("nav-doomed").first.click()
        app.wait_for_selector("text=This folder is empty", timeout=20000)

        app.evaluate(
            """async () => {
                await fetch('/api/folders?path=' + encodeURIComponent('/nav-doomed')
                            + '&recursive=1', {method: 'DELETE', credentials: 'same-origin'})
            }"""
        )
        app.get_by_role("button", name="Refresh").click()

        expect(app.locator('button[aria-label="Up"]')).to_be_disabled(timeout=20000)
        assert app.get_by_text("nav-doomed").count() == 0


class TestViewAndSort:
    """Rows or tiles, and in what order — both remembered between visits."""

    def test_the_grid_draws_the_same_entries(self, app, tmp_path):
        make_folder(app, "view-grid")
        app.get_by_text("view-grid").first.click()
        app.wait_for_load_state("networkidle")

        source = tmp_path / "tiled.txt"
        source.write_text("a tile")
        upload_and_settle(app, source)

        app.locator('button[aria-label="Show as a grid"]').click()
        app.wait_for_timeout(400)
        assert app.get_by_text("tiled.txt").count() > 0
        # The row's paired controls are gone; the tile carries the same menu.
        assert app.locator('button[aria-label="Actions for tiled.txt"]').count() == 1

        app.locator('button[aria-label="Show as a list"]').click()
        app.wait_for_selector('button[aria-label="Download tiled.txt"]', timeout=20000)

    def test_the_view_survives_a_reload(self, app, tmp_path):
        app.locator('button[aria-label="Show as a grid"]').click()
        app.wait_for_timeout(300)

        app.reload()
        app.wait_for_selector("text=Connected clouds", timeout=20000)
        # It opens back on the grid, so the toggle offers the list.
        app.wait_for_selector('button[aria-label="Show as a list"]', timeout=20000)

        app.locator('button[aria-label="Show as a list"]').click()
        app.wait_for_timeout(300)

    def test_sorting_by_size_reverses_on_a_second_choice(self, app, tmp_path):
        make_folder(app, "view-sort")
        app.get_by_text("view-sort").first.click()
        app.wait_for_load_state("networkidle")

        for name, size in (("small.txt", 10), ("medium.txt", 2000), ("large.txt", 60000)):
            source = tmp_path / name
            source.write_text("x" * size)
            upload_and_settle(app, source)

        assert listed_files(app) == ["large.txt", "medium.txt", "small.txt"]

        app.locator('button[aria-label="Sort"]').click()
        app.wait_for_selector("text=Sort by", timeout=10000)
        # The sheet row's spoken name leads with its arrow, so match inside it.
        app.get_by_role("button", name=re.compile("Size")).click()
        app.wait_for_timeout(300)
        # Size opens on the biggest first, which is what anyone sorting by it
        # is looking for.
        assert listed_files(app) == ["large.txt", "medium.txt", "small.txt"]

        app.locator('button[aria-label="Sort"]').click()
        app.wait_for_selector("text=Sort by", timeout=10000)
        # The sheet row's spoken name leads with its arrow, so match inside it.
        app.get_by_role("button", name=re.compile("Size")).click()
        app.wait_for_timeout(300)
        assert listed_files(app) == ["small.txt", "medium.txt", "large.txt"]


class TestSelection:
    """Picking several rows and acting on all of them at once."""

    def test_ticks_only_appear_once_selecting(self, app, tmp_path):
        make_folder(app, "pick-ticks")
        app.get_by_text("pick-ticks").first.click()
        app.wait_for_load_state("networkidle")

        source = tmp_path / "pickable.txt"
        source.write_text("pick me")
        upload_and_settle(app, source)

        assert app.get_by_role("checkbox").count() == 0

        app.locator('button[aria-label="Select files and folders"]').click()
        app.wait_for_selector("text=Nothing selected", timeout=10000)
        # One over the listing to take the lot, one on the row.
        assert app.get_by_role("checkbox").count() == 2

        app.get_by_role("button", name="Done").click()
        app.wait_for_timeout(300)
        assert app.get_by_role("checkbox").count() == 0

    def test_shift_takes_the_run_between_two_ticks(self, app, tmp_path):
        make_folder(app, "pick-run")
        app.get_by_text("pick-run").first.click()
        app.wait_for_load_state("networkidle")

        for name in ("run-a.txt", "run-b.txt", "run-c.txt"):
            source = tmp_path / name
            source.write_text(name)
            upload_and_settle(app, source)

        app.locator('button[aria-label="Select files and folders"]').click()
        app.wait_for_selector("text=Nothing selected", timeout=10000)

        # The first tick is the select-everything one above the columns.
        app.get_by_role("checkbox").nth(1).click()
        app.get_by_role("checkbox").nth(3).click(modifiers=["Shift"])
        app.wait_for_selector("text=3 of 3 selected", timeout=10000)

    def test_select_all_then_delete_empties_the_folder(self, app, tmp_path):
        make_folder(app, "pick-doomed")
        app.get_by_text("pick-doomed").first.click()
        app.wait_for_load_state("networkidle")

        for name in ("doomed-a.txt", "doomed-b.txt"):
            source = tmp_path / name
            source.write_text(name)
            upload_and_settle(app, source)

        app.locator('button[aria-label="Select files and folders"]').click()
        app.get_by_role("button", name="Select all").click()
        app.wait_for_selector("text=2 of 2 selected", timeout=10000)

        app.get_by_role("button", name=re.compile("^✕ Delete")).click()
        dialog = app.get_by_role("dialog", name="Delete 2 items?")
        dialog.wait_for(timeout=10000)

        # Backing out leaves both where they were.
        dialog.get_by_role("button", name="Cancel").click()
        app.wait_for_timeout(300)
        assert app.get_by_text("doomed-a.txt").count() > 0

        app.get_by_role("button", name=re.compile("^✕ Delete")).click()
        dialog = app.get_by_role("dialog", name="Delete 2 items?")
        dialog.wait_for(timeout=10000)
        dialog.get_by_role("button", name="Delete 2").click()
        app.wait_for_selector("text=2 deleted", timeout=60000)
        # Two "Done" buttons are on the page — the dialog's and the selection
        # bar's behind it — so this one is asked for by the dialog it is in.
        app.get_by_role("dialog").get_by_role("button", name="Done").click()

        app.wait_for_selector("text=This folder is empty", timeout=20000)


class TestSearch:
    """The search box, which reaches past the folder the browser is standing in.

    The index it queries only exists inside the open vault, so a hit here is
    proof the server answered from decrypted metadata — no account can be asked
    what it holds.
    """

    def _search(self, app, query):
        box = app.get_by_label("Search files and folders")
        box.fill(query)
        # Results are debounced, so wait for the count line rather than racing it.
        app.wait_for_selector("text=/(match|matches|No matches) for/", timeout=20000)
        return box

    def test_finds_a_file_stored_in_another_folder(self, app, tmp_path):
        app.get_by_text("+ Folder").click()
        app.wait_for_selector("text=New folder")
        app.fill('input[placeholder="Folder name"]', "search-folder")
        app.locator('button[type=submit]:has-text("Create")').click()
        app.wait_for_selector("text=search-folder", timeout=20000)

        app.get_by_text("search-folder").first.click()
        source = tmp_path / "buried-treasure.txt"
        source.write_text("found me")
        upload_and_settle(app, source)

        # Back at the root, where the file is not listed at all.
        app.get_by_text("▣ /").click()
        expect(app.get_by_text("buried-treasure.txt")).to_have_count(0)

        self._search(app, "buried")
        app.wait_for_selector("text=buried-treasure.txt", timeout=20000)
        # The result says which folder it was found in.
        assert app.get_by_text("in /search-folder").count() > 0

        # And it still opens from there, rebuilt out of its scattered parts.
        app.locator('button[title="Open"]').first.click()
        app.wait_for_selector("text=found me", timeout=60000)

    def test_a_folder_is_a_result_too(self, app):
        app.get_by_text("+ Folder").click()
        app.wait_for_selector("text=New folder")
        app.fill('input[placeholder="Folder name"]', "findable-folder")
        app.locator('button[type=submit]:has-text("Create")').click()
        app.wait_for_selector("text=findable-folder", timeout=20000)

        self._search(app, "findable")
        app.get_by_text("findable-folder").first.click()
        # Clicking a folder hit walks into it and ends the search.
        app.wait_for_load_state("networkidle")
        assert app.get_by_label("Search files and folders").input_value() == ""

    def test_a_query_that_matches_nothing_says_so(self, app):
        self._search(app, "no-such-thing-anywhere")
        app.wait_for_selector('text=Nothing matches "no-such-thing-anywhere"', timeout=20000)

    def test_clearing_the_search_returns_to_the_listing(self, app, tmp_path):
        source = tmp_path / "still-listed.txt"
        source.write_text("here")
        upload_and_settle(app, source)

        self._search(app, "no-such-thing-anywhere")
        app.wait_for_selector("text=Nothing matches", timeout=20000)
        expect(app.get_by_text("still-listed.txt")).to_have_count(0)

        app.get_by_label("Clear the search").click()
        app.wait_for_selector("text=still-listed.txt", timeout=20000)


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
        open_vault_setting(app, "Password")
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
        close_vault_settings(app)

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
        open_vault_setting(app, "Password")
        app.wait_for_selector("text=Re-encrypt my files now", timeout=15000)

        app.get_by_label("Current password").fill("not-the-password")
        app.get_by_label("New password", exact=True).fill("does-not-matter")
        app.get_by_label("Confirm new password").fill("does-not-matter")
        app.get_by_role("button", name="Change password").click()

        app.wait_for_selector("text=That is not your current password", timeout=30000)
        app.get_by_role("button", name="Cancel").click()
        close_vault_settings(app)

    def test_mismatched_new_passwords_never_reach_the_server(self, app, vault_password):
        open_vault_setting(app, "Password")
        app.wait_for_selector("text=Re-encrypt my files now", timeout=15000)

        app.get_by_label("Current password").fill(vault_password)
        app.get_by_label("New password", exact=True).fill("one-thing")
        app.get_by_label("Confirm new password").fill("another-thing")
        app.get_by_role("button", name="Change password").click()

        app.wait_for_selector("text=The two new passwords do not match", timeout=15000)
        # The vault is untouched: the dialog is still a form, not a report.
        assert app.get_by_text("Password changed").count() == 0
        app.get_by_role("button", name="Cancel").click()
        close_vault_settings(app)

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


class TestVaultSettings:
    """Everything set on the vault itself is behind one button.

    The panel used to carry a tile per setting, which meant every new setting
    took another bite out of the drawer and settings that did not fit simply
    were not offered. One list holds them all, each line saying where that
    setting stands, and each still opening the dialog it always did.
    """

    def test_one_button_opens_the_lot(self, app):
        app.get_by_role("button", name=re.compile(r"^Vault settings")).click()
        menu = app.get_by_role("dialog", name="Vault settings")
        menu.wait_for(timeout=20000)

        for label in ("Default clouds", "Password", "Film key"):
            assert menu.get_by_role("button", name=re.compile(rf"^{label}")).count() == 1

        close_vault_settings(app)

    def test_a_setting_opens_over_the_list_and_closes_back_onto_it(self, app):
        open_vault_setting(app, "Film key")
        app.wait_for_selector("text=Film database key", timeout=20000)

        # Escape closes the dialog on top and only that one, so cancelling out
        # of a setting puts you back on the list you chose it from.
        app.keyboard.press("Escape")
        app.wait_for_selector("text=Film database key", state="detached", timeout=20000)
        assert app.get_by_role("dialog", name="Vault settings").count() == 1

        close_vault_settings(app)

    def test_a_folder_points_at_the_key_rather_than_asking_for_it(self, app):
        """The key belongs to the vault, so a folder's film dialog only says
        whether there is one."""
        app.get_by_role("button", name="Film details for this folder").click()
        app.wait_for_selector("text=Film details", timeout=20000)

        assert app.locator(
            'input[placeholder="Paste your TMDB key or read token"]').count() == 0
        assert app.get_by_text("Vault settings → Film key").count() == 1

        app.keyboard.press("Escape")


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

    @pytest.mark.parametrize("size", [PHONE, NARROW_PHONE])
    def test_search_results_do_not_scroll_sideways(self, app, tmp_path, size):
        app.get_by_text("+ Folder").click()
        app.wait_for_selector("text=New folder")
        app.fill('input[placeholder="Folder name"]', "a-folder-with-a-long-name")
        app.locator('button[type=submit]:has-text("Create")').click()
        app.wait_for_selector("text=a-folder-with-a-long-name", timeout=20000)
        app.get_by_text("a-folder-with-a-long-name").first.click()

        source = tmp_path / "a-narrow-screens-worst-nightmare.txt"
        source.write_text("narrow")
        upload_and_settle(app, source)

        app.set_viewport_size(size)
        app.wait_for_timeout(300)
        # A result carries its folder as well as its name, which is the widest
        # a row in this app ever gets.
        search_on_a_phone(app, "nightmare")
        app.wait_for_selector("text=a-narrow-screens-worst-nightmare.txt", timeout=20000)

        assert not horizontal_overflow(app)

    def test_the_phone_heads_the_listing_with_the_folder(self, app, tmp_path):
        """The phone's toolbar is a heading, not a row of arrows.

        The desk's controls do not survive a 390px screen — half that row is
        empty and the trail takes a line of its own to draw a slash — so the
        phone names the folder instead and says underneath what it holds and
        how many separate accounts it is spread over. That last number is the
        thing only this app can answer, and it is counted off the folder in
        front of you rather than the vault.
        """
        # Counted in a folder of its own: the vault the suite shares has been
        # filled by every test before this one, and what is being checked is
        # that the heading counts the folder in front of you rather than all of
        # it.
        make_folder(app, "headed-folder")
        app.get_by_text("headed-folder").first.click()
        app.wait_for_load_state("networkidle")

        source = tmp_path / "headed.txt"
        source.write_text("counted")
        upload_and_settle(app, source, choose=["ui-one", "ui-two", "ui-three"])

        app.set_viewport_size(PHONE)
        app.wait_for_timeout(400)

        # The desk's separate arrows are gone; the heading carries the name.
        assert app.get_by_label("Up", exact=True).count() == 0
        heading = app.locator('button[aria-label^="headed-folder"]')
        expect(heading).to_have_count(1)
        # One file, scattered over the three accounts it was given — and not a
        # word about everything sitting outside this folder.
        assert "1 file" in heading.inner_text()
        assert "3 clouds" in heading.inner_text()

        # And everything the desk keeps on its toolbar is one tap away here.
        for label in ("Search", "Sort", "New folder", "Select files and folders"):
            assert app.get_by_label(label, exact=True).count() == 1

    def test_the_folder_name_drops_the_trail_on_a_phone(self, app):
        """Walking back up is a menu rather than a row of crumbs — four
        folders deep there is no room to spell the trail out, and the menu is
        fewer taps than the crumbs were anyway."""
        make_folder(app, "trail-folder")
        app.get_by_text("trail-folder").first.click()
        app.wait_for_load_state("networkidle")

        app.set_viewport_size(PHONE)
        app.wait_for_timeout(400)

        app.get_by_label("trail-folder — open the folder trail").click()
        app.wait_for_selector("text=Where you are now", timeout=10000)
        # The root is on it, and choosing it walks back out. Read inside the
        # sheet: the wordmark in the header is called Vault too.
        app.get_by_role("dialog").get_by_text("Vault", exact=True).click()
        app.wait_for_selector('button[aria-label="The root of the vault"]', timeout=10000)

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

        # Sized inline rather than by the `pointer: coarse` rule, which a
        # resized desktop browser never matches — and which an inline style
        # would silently outrank anyway.
        undersized = app.evaluate(
            """() => {
                const list = [...document.querySelectorAll('button, a[href]')]
                return list.map((el) => {
                    const r = el.getBoundingClientRect()
                    return { what: (el.getAttribute('aria-label') || el.textContent || '').trim().slice(0, 30),
                             w: Math.round(r.width), h: Math.round(r.height) }
                }).filter((x) => x.w > 0 && (x.w < 44 || x.h < 44))
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
        assert app.get_by_label("Download menu.txt").count() == 0
        app.locator('button[aria-label="Actions for menu.txt"]').click()
        app.wait_for_selector("text=Where the shards live", timeout=10000)

        # Downloading from the sheet still hands the bytes back without taking
        # the window with it — the sheet closes, the app stays put.
        before = app.url
        with app.expect_download(timeout=60000) as download:
            app.get_by_text("Download", exact=True).click()
        assert download.value.suggested_filename == "menu.txt"
        assert app.url == before
        app.wait_for_timeout(400)

        # The shard badges are only a read-out on a phone, so the inspector they
        # open on a desktop has to be reachable from the menu instead.
        app.locator('button[aria-label="Actions for menu.txt"]').click()
        app.wait_for_selector("text=Where the shards live", timeout=10000)
        app.get_by_text("Where the shards live").click()
        app.wait_for_selector("text=Where this file lives", timeout=20000)
        app.keyboard.press("Escape")
        app.wait_for_timeout(400)

        app.locator('button[aria-label="Actions for menu.txt"]').click()
        app.wait_for_selector("text=Where the shards live", timeout=10000)
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




# The dialog's own connect button, told apart from the accounts panel's
# "+ Connect a cloud" sitting behind the modal.
DIALOG_CONNECT = re.compile(r"^\+ Connect (another|a missing) cloud$")


class TestDisasterRecovery:
    """The machine died and the clouds came back, one at a time.

    Every test here gets its own server, port and vault file, because that is
    the scenario: a replacement machine that has never held a file, connecting
    accounts that are still carrying the vault it lost. The app is supposed to
    notice that on its own — nobody in this situation knows to go looking for a
    recovery command — and then to walk the rest of the way, since one cloud is
    never enough to rebuild anything from.
    """

    def new_machine(self, page, base_url, password):
        """Create the replacement vault through the first-run screen."""
        page.goto(base_url)
        page.wait_for_selector("text=Create your vault", timeout=20000)
        boxes = page.locator('input[autocomplete="new-password"]')
        boxes.nth(0).fill(password)
        boxes.nth(1).fill(password)
        page.get_by_text("▶ Create vault").click()
        page.wait_for_selector("text=Connected clouds", timeout=20000)

    def fill_cloud_form(self, page, name, path):
        """Complete whichever connect-a-cloud dialog is already open."""
        page.wait_for_selector("text=Local folder", timeout=15000)
        page.get_by_text("Local folder").click()
        form = page.locator("form")
        form.locator("input").nth(0).fill(name)
        form.locator("input").nth(1).fill(path)
        form.locator("button[type=submit]").click()

    def reconnect(self, page, name, path):
        """Wire a recovered cloud back up from the accounts panel."""
        page.get_by_text("+ Connect a cloud").click()
        self.fill_cloud_form(page, name, path)
        page.wait_for_selector(f"text={name}", timeout=30000)

    def reconnect_from_dialog(self, page, name, path):
        """Wire one up from inside the recovery dialog, which is where it asks.

        The button is the dialog's own, so the accounts panel behind it is not
        involved — and what happens next is the point of the test: the dialog
        re-checks by itself.
        """
        page.get_by_role("button", name=DIALOG_CONNECT).click()
        self.fill_cloud_form(page, name, path)

    def dismiss_prompt(self, page):
        """Close the recovery prompt if it has opened over the accounts panel.

        Keyed on the button rather than the title: the banner underneath says
        "Sand files detected" too, and it is still there once the dialog is
        shut — which is the point of it.
        """
        not_now = page.get_by_role("button", name="Not now")
        if not_now.count() == 0:
            return
        not_now.click()
        page.wait_for_timeout(300)

    def test_connecting_a_cloud_prompts_to_recover(self, page, spawn_server, lost_vault):
        clouds, _, _ = lost_vault
        self.new_machine(page, spawn_server("case-prompt"), "a-brand-new-passphrase")

        # Nothing has been asked for. Connecting the first cloud is enough for
        # the app to find a vault on it and say so.
        self.reconnect(page, "back-one", clouds[0])
        expect(page.get_by_text("Sand files detected")).to_be_visible(timeout=30000)
        expect(page.get_by_text(re.compile(r"stored parts?"))).to_be_visible()

    def test_one_cloud_is_asked_for_the_next_rather_than_a_password(self, page, spawn_server, lost_vault):
        """A file needs two of its three parts, and one account holds one.

        So the dialog does not open on a password box it cannot use yet: it
        asks for the cloud that would make a recovery possible at all.
        """
        clouds, _, _ = lost_vault
        self.new_machine(page, spawn_server("case-asks"), "a-brand-new-passphrase")

        self.reconnect(page, "back-one", clouds[0])
        page.wait_for_selector("text=Sand files detected", timeout=30000)

        expect(page.get_by_text(re.compile(r"one cloud on its own carries no whole file"))).to_be_visible()
        expect(page.get_by_role("button", name=DIALOG_CONNECT)).to_be_visible()
        assert page.locator('input[type="password"]').count() == 0

    def test_the_dialog_connects_the_next_cloud_itself(self, page, spawn_server, lost_vault):
        """Connecting from inside the dialog is what turns the prompt into a
        password box: the second cloud is what makes a recovery possible."""
        clouds, lost_password, names = lost_vault
        self.new_machine(page, spawn_server("case-guided"), "a-brand-new-passphrase")

        self.reconnect(page, "back-one", clouds[0])
        page.wait_for_selector("text=Sand files detected", timeout=30000)

        self.reconnect_from_dialog(page, "back-two", clouds[1])

        # The dialog looked again on its own and moved on.
        password_box = page.locator('input[type="password"]')
        expect(password_box.first).to_be_visible(timeout=60000)
        expect(page.get_by_role("button", name="Recover", exact=True)).to_be_visible()

        password_box.first.fill(lost_password)
        page.get_by_role("button", name="Recover", exact=True).click()
        page.wait_for_selector("text=Recovery complete", timeout=60000)
        page.get_by_role("button", name="Open the vault").click()

        for name in names:
            expect(page.get_by_text(name, exact=True).first).to_be_visible(timeout=30000)

    def test_the_last_cloud_finishes_the_recovery_by_itself(self, page, spawn_server, lost_vault):
        """The whole point of asking: hand it the cloud it asked for and it
        gets on with it, rather than making you press the same button again."""
        clouds, lost_password, _ = lost_vault
        self.new_machine(page, spawn_server("case-auto"), "a-brand-new-passphrase")

        # Two clouds and a password: enough to rebuild everything but the spare
        # parts, so a recovery now leaves the third account's parts unreachable.
        self.reconnect(page, "back-one", clouds[0])
        page.wait_for_selector("text=Sand files detected", timeout=30000)
        self.reconnect_from_dialog(page, "back-two", clouds[1])

        password_box = page.locator('input[type="password"]')
        expect(password_box.first).to_be_visible(timeout=60000)
        password_box.first.fill(lost_password)

        # Handing it the third cloud is the assent — no further clicking.
        self.reconnect_from_dialog(page, "back-three", clouds[2])
        page.wait_for_selector("text=Recovery complete", timeout=90000)
        expect(page.get_by_text(re.compile(r"are back in\s+your vault"))).to_be_visible()

    def test_a_partial_recovery_says_what_is_still_missing(self, page, spawn_server, lost_vault):
        clouds, lost_password, _ = lost_vault
        self.new_machine(page, spawn_server("case-partial"), "a-brand-new-passphrase")

        # Two of three: every file comes back, with no spare part left and the
        # third account's parts still out of reach.
        self.reconnect(page, "back-one", clouds[0])
        page.wait_for_selector("text=Sand files detected", timeout=30000)
        self.dismiss_prompt(page)
        self.reconnect(page, "back-two", clouds[1])
        self.dismiss_prompt(page)

        page.get_by_role("button", name="Attempt recovery").click()
        page.wait_for_selector("text=Sand files detected", timeout=30000)
        page.locator('input[type="password"]').first.fill(lost_password)

        page.get_by_role("button", name="Check what is there").click()
        expect(page.get_by_text("files openable")).to_be_visible(timeout=60000)
        expect(page.get_by_text("with no spare part")).to_be_visible()

        page.get_by_role("button", name="Recover", exact=True).click()
        page.wait_for_selector("text=Recovery complete", timeout=60000)
        page.get_by_role("button", name="Open the vault").click()

        # The third cloud turns up later, and the banner offers to finish —
        # which needs no password, the key having been adopted already.
        expect(page.get_by_role("button", name="Finish recovery")).to_be_visible(timeout=30000)
        self.reconnect(page, "back-three", clouds[2])

        page.get_by_role("button", name="Finish recovery").first.click()
        page.wait_for_selector("text=Finish the recovery", timeout=30000)
        assert page.locator('input[type="password"]').count() == 0

        page.get_by_role("button", name="Finish recovery").last.click()
        page.wait_for_selector("text=Recovery complete", timeout=60000)
        expect(page.get_by_text("files openable")).to_be_visible()

    def test_a_lost_file_is_reported_and_then_recovered_from_the_report(self, page, spawn_server, lost_vault):
        """One cloud is not enough, and someone may recover from it anyway.

        The report says which accounts would change that, and connecting one
        from the report itself picks the recovery back up where it stopped.
        """
        clouds, lost_password, _ = lost_vault
        self.new_machine(page, spawn_server("case-report"), "a-brand-new-passphrase")

        self.reconnect(page, "back-one", clouds[0])
        page.wait_for_selector("text=Sand files detected", timeout=30000)

        # Force the premature attempt the dialog is steering away from, by
        # giving it the second cloud and then recovering before the third.
        self.reconnect_from_dialog(page, "back-two", clouds[1])
        password_box = page.locator('input[type="password"]')
        expect(password_box.first).to_be_visible(timeout=60000)
        password_box.first.fill(lost_password)
        page.get_by_role("button", name="Recover", exact=True).click()
        page.wait_for_selector("text=Recovery complete", timeout=60000)

        # Everything is openable, but the third account still holds spare parts,
        # so the vault says so rather than calling the job done.
        page.get_by_role("button", name="Open the vault").click()
        expect(page.get_by_role("button", name="Finish recovery")).to_be_visible(timeout=30000)

    def test_recovered_files_are_re_encrypted_onto_chosen_clouds(self, page, spawn_server, lost_vault):
        """Recovery adopts the dead vault's key, which its password still opens.

        The app says so and keeps saying so, and re-encrypting is where the
        user also gets to say which clouds the files should live on — the ones
        a recovery lands on were chosen on a machine that is gone.
        """
        clouds, lost_password, names = lost_vault
        base = spawn_server("case-reclaim")
        self.new_machine(page, base, "a-brand-new-passphrase")

        self.reconnect(page, "back-one", clouds[0])
        page.wait_for_selector("text=Sand files detected", timeout=30000)
        self.reconnect_from_dialog(page, "back-two", clouds[1])
        password_box = page.locator('input[type="password"]')
        expect(password_box.first).to_be_visible(timeout=60000)
        password_box.first.fill(lost_password)
        self.reconnect_from_dialog(page, "back-three", clouds[2])
        page.wait_for_selector("text=Recovery complete", timeout=90000)

        # Said at the end of the recovery...
        expect(page.get_by_text(re.compile(r"on the key of the vault they came from"))).to_be_visible()
        page.get_by_role("button", name="Open the vault").click()

        # ...and standing in the accounts panel, because the transfer it takes
        # to fix is the whole vault twice over and rarely wanted right now.
        reclaim = page.get_by_role("button", name="Re-encrypt under your key")
        expect(reclaim).to_be_visible(timeout=30000)
        reclaim.click()
        page.wait_for_selector("text=Re-encrypt under your own key", timeout=15000)

        # Pick the clouds: everything is being gathered and scattered anyway.
        select_clouds(page, ["back-one", "back-two", "back-three"])
        page.get_by_role("button", name="Re-encrypt and move").click()
        page.wait_for_selector("text=These files are yours now", timeout=120000)
        page.get_by_role("button", name="Done").click()

        # The warning is gone and the files still open.
        expect(page.get_by_role("button", name="Re-encrypt under your key")).to_have_count(0, timeout=30000)
        for name in names:
            expect(page.get_by_text(name, exact=True).first).to_be_visible(timeout=30000)

class TestWiderSchemes:
    """Six clouds and nine are the same choice as three, made wider.

    These run last on purpose. They connect accounts, and a connected account
    stays connected for the rest of the session — every random three-cloud pick
    after them would be drawing from a larger pool than the test that wrote it
    expected.
    """

    def test_six_clouds_cut_the_file_four_of_six(self, app, tmp_path, clouds):
        """Picking six clouds chooses a different erasure code, and the picker
        says which before a byte leaves the machine."""
        for name in ("ui-four", "ui-five", "ui-six"):
            connect_cloud(app, name, clouds)

        source = tmp_path / "wide.txt"
        source.write_text("cut four of six")
        app.set_input_files("input[type=file]", str(source))
        app.wait_for_selector("text=/Upload to \\d+ cloud/", timeout=20000)

        select_clouds(app, ["ui-one", "ui-two", "ui-three", "ui-four", "ui-five", "ui-six"])
        # The dialog names the scheme and what it buys, not just the count.
        app.wait_for_selector("text=4-of-6", timeout=20000)
        app.get_by_role("button", name="↑ Upload to 6 clouds").click()
        app.wait_for_selector('button[title="Open"]:has-text("wide.txt")', timeout=90000)
        app.wait_for_load_state("networkidle")

        # Six shards, one per cloud, and the row says the scheme.
        row = app.locator('button[title="Open"]', has_text="wide.txt").locator("xpath=..")
        for name in ("ui-one", "ui-two", "ui-three", "ui-four", "ui-five", "ui-six"):
            expect(row.get_by_title(re.compile(rf"Shard \d on {name}$"))).to_have_count(1)

        # And it still rebuilds.
        app.locator('button[title="Open"]', has_text="wide.txt").click()
        app.wait_for_selector("text=cut four of six", timeout=60000)
        app.keyboard.press("Escape")

    def test_a_count_with_no_scheme_cannot_be_uploaded(self, app, tmp_path, clouds):
        """Four clouds names no code, so the picker refuses it and says which
        counts it does have — rather than silently dropping one."""
        connect_cloud(app, "ui-four", clouds)

        source = tmp_path / "between.txt"
        source.write_text("four is not a scheme")
        app.set_input_files("input[type=file]", str(source))
        app.wait_for_selector("text=/Upload to \\d+ cloud/", timeout=20000)

        select_clouds(app, ["ui-one", "ui-two", "ui-three", "ui-four"])
        app.wait_for_selector("text=4 clouds names no scheme", timeout=20000)
        expect(app.get_by_role("button", name=re.compile("Upload to 4 clouds"))).to_be_disabled()

        app.get_by_role("button", name="Cancel").click()
        app.wait_for_selector("text=/Upload to \\d+ cloud/", state="detached", timeout=20000)
        assert app.get_by_text("between.txt").count() == 0
