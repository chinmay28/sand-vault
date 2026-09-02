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
import time

import pytest
import requests
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

    open_accounts(page)

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


def open_accounts(page, timeout=20000):
    """Bring the connected-clouds sidebar out, and wait until it is there.

    The panel starts minimised at every width — on a desktop as well as on a
    phone — so anything that reaches into it (an account card, "+ Connect a
    cloud", the vault settings tile) has to open it first. Idempotent: called
    with the sidebar already out it does nothing, so it is safe to put in front
    of any helper that needs the panel.

    Doubles as the "the vault is unlocked and the browser has rendered" wait it
    replaced: the ☰ is only in the header once there is a vault open behind it.
    """
    toggle = page.locator('button[aria-label="Connected clouds"]').first
    toggle.wait_for(state="visible", timeout=timeout)

    heading = page.get_by_text("Connected clouds", exact=True).first
    if not heading.is_visible():
        toggle.click()
    heading.wait_for(state="visible", timeout=timeout)
    return page


def connect_cloud(page, name, clouds):
    """Connect one more local-folder account through the UI, if it is not
    already there. Moving something between clouds needs somewhere to move it
    to that it is not already on."""
    open_accounts(page)
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
    open_accounts(page)
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


# The app has two file inputs, because whether an input asks for files or for a
# folder is a property of the input and not of the click. They are told apart
# here the way the browser tells them apart.
FILE_INPUT = "input[type=file]:not([webkitdirectory])"
FOLDER_INPUT = "input[webkitdirectory]"


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
    page.set_input_files(FILE_INPUT, str(source))

    confirm = page.get_by_role("button", name=re.compile(r"Upload to \d+ cloud"))
    confirm.wait_for(timeout=20000)
    if choose is not None:
        select_clouds(page, choose)
    confirm.click()

    page.wait_for_selector(f'button[title="Open"]:has-text("{name}")', timeout=90000)
    page.wait_for_load_state("networkidle")


def clickable(page, locator):
    """Whether a click aimed at this element would actually reach it.

    A dialog drawn underneath the panel that opened it is still on the page,
    still laid out and still "visible" by every measure short of the one that
    matters: the browser hands the click to whatever is on top, which is the
    panel's backdrop. Asking the document what is at the point tells the two
    apart, and says so in the assertion rather than as a click that times out
    for no stated reason.
    """
    box = locator.bounding_box()
    if box is None:
        return False
    return locator.evaluate(
        """(el, point) => {
             const top = document.elementFromPoint(point[0], point[1])
             return !!top && (el === top || el.contains(top))
           }""",
        [box["x"] + box["width"] / 2, box["y"] + box["height"] / 2],
    )


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


def open_sub_vaults_panel(page):
    """Open the sub vaults panel, which lives behind the settings menu."""
    open_vault_setting(page, "Sub vaults")
    panel = page.get_by_role("dialog", name="Sub vaults")
    panel.wait_for(timeout=20000)
    return panel


def make_sub_vault(page, name, password):
    """Make one through the panel, which walks into it once it exists."""
    panel = open_sub_vaults_panel(page)
    panel.get_by_role("button", name="+ New sub vault").click()

    dialog = page.get_by_role("dialog", name="New sub vault")
    dialog.wait_for(timeout=20000)
    boxes = dialog.locator("form").locator("input")
    boxes.nth(0).fill(name)
    boxes.nth(1).fill(password)
    boxes.nth(2).fill(password)
    dialog.get_by_role("button", name="Create").click()

    # Making one opens it, and the root crumb names which vault you are
    # standing in — an unqualified "/" would be two different trees.
    page.wait_for_selector(f"text=🔒 {name} /", timeout=30000)
    page.wait_for_load_state("networkidle")


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

    def test_the_catalogue_names_the_services_behind_a_protocol(self, app):
        """The picker offers "S3-compatible storage", which is a true label and
        no answer to *can it hold my Google Cloud Storage bucket*. The
        catalogue behind it is where that question is answered, without the
        picker growing to forty entries."""
        app.get_by_text("+ Connect a cloud").click()
        app.wait_for_selector("text=Sign in with your account")
        assert app.get_by_text("Google Cloud Storage", exact=True).count() == 0

        app.get_by_text("Not here?").click()
        catalogue = app.get_by_text("Every cloud SAND can hold parts on", exact=True)
        expect(catalogue).to_be_visible()
        for label in ("Google Cloud Storage", "Seafile", "Storj"):
            assert app.get_by_text(label, exact=True).count() > 0, label

        # A search answers the question asked, rather than handing back the
        # whole backend the answer happens to live in.
        app.get_by_placeholder("wasabi, seafile, nextcloud…").fill("wasabi")
        expect(app.get_by_text("Wasabi", exact=True)).to_be_visible()
        expect(app.get_by_text("Storj", exact=True)).to_have_count(0)

        # Escape closes the window on top and leaves the picker underneath it,
        # which is the whole reason it is a window of its own.
        app.keyboard.press("Escape")
        expect(catalogue).to_have_count(0)
        expect(app.get_by_text("Sign in with your account")).to_be_visible()
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
    """The two halves of the edit dialog.

    One is what a cloud is called and the colour it wears. The colour is the
    same shade on the account's card and on every shard badge for a file it
    holds, which is what makes "which clouds is this file on" a question you
    answer by eye. Both are stored in the vault, so both survive a reload — and
    neither reaches the account itself.

    The other is how the account connects, which does reach it: an edit there is
    checked against the backend before it is stored, so an account cannot be
    edited into one that no longer answers.
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
        # A reload puts the sidebar back the way it opens: folded away. The
        # card being looked at is inside it.
        open_accounts(app)
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
        open_accounts(app)
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

    def test_a_quota_caps_what_sand_may_fill(self, app, clouds, tmp_path):
        """The line you draw through a cloud, and what it changes.

        A capacity is how big an account is; a quota is how much of it SAND may
        fill. It is offered on every account — a cloud reporting terabytes free
        is still one you might only want a slice of — and the room left becomes
        whichever of the two leaves less."""
        connect_cloud(app, "ui-capped", clouds)

        self.open_editor(app, "ui-capped")
        # Small enough to be under whatever the drive under it reports, on any
        # machine the suite runs on.
        app.get_by_label("Quota").fill("16 MB")
        app.get_by_role("button", name="Save").click()
        app.wait_for_selector("text=Edit account", state="detached", timeout=20000)

        # The card keeps the drive's own bar — the folder sits on a real disk
        # and that is a real figure — and gains the line that binds.
        app.wait_for_selector("text=16 MB left under your quota", timeout=20000)

        # Stored in the vault, not held in the tab.
        app.reload()
        open_accounts(app)
        app.wait_for_selector("text=16 MB left under your quota", timeout=20000)

        # And the picker ranks the cloud by that figure rather than the drive's,
        # saying whose number it is.
        source = tmp_path / "capped.txt"
        source.write_text("where does this fit")
        app.set_input_files(FILE_INPUT, str(source))
        app.wait_for_selector("text=/Upload to \\d+ cloud/", timeout=20000)
        row = app.get_by_role("checkbox").filter(has_text="ui-capped")
        assert "16 MB free (quota)" in row.inner_text(), row.inner_text()
        app.get_by_role("button", name="Cancel").click()
        app.wait_for_selector("text=/Upload to \\d+ cloud/", state="detached", timeout=20000)

        # The dialog reopens on the figure that was typed.
        self.open_editor(app, "ui-capped")
        expect(app.get_by_label("Quota")).to_have_value("16 MB")

        # Cleared, the account goes back to the drive's own answer.
        app.get_by_label("Quota").fill("")
        app.get_by_role("button", name="Save").click()
        app.wait_for_selector("text=Edit account", state="detached", timeout=20000)
        app.wait_for_selector("text=16 MB left under your quota", state="detached", timeout=20000)

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

    def test_the_settings_it_connects_with_can_be_changed(self, app, clouds):
        """The other half of the dialog: what the account actually reaches.

        For a folder backend the setting is the folder, which stands in here for
        the rotated access key or re-pasted token a cloud account needs — the
        same edit, checked the same way. What makes it different from a rename
        is that it reaches the backend: settings SAND cannot connect with are
        refused rather than stored, and the account is left on the ones that
        still work.
        """
        connect_cloud(app, "ui-rewired", clouds)
        moved = clouds("ui-rewired-moved")

        self.open_editor(app, "ui-rewired")
        app.get_by_role("tab", name="How it connects").click()
        app.wait_for_selector("text=Save and reconnect", timeout=20000)

        # Somewhere no folder can be made: the edit is refused, the account
        # stays on the folder it has, and the dialog says why rather than
        # closing over it.
        app.get_by_label("Directory *").fill("/proc/1/sand-cannot-go-here")
        app.get_by_role("button", name="Save and reconnect").click()
        app.wait_for_selector("text=do not reach", timeout=30000)
        assert app.get_by_text("Edit account").count() > 0

        # And one that can. Saving connects with it before storing it, so the
        # dialog closing is the account really answering on the new setting.
        app.get_by_label("Directory *").fill(moved)
        app.get_by_role("button", name="Save and reconnect").click()
        app.wait_for_selector("text=Edit account", state="detached", timeout=30000)

        # Stored in the vault, so it survives a reload — and it is the same
        # account throughout, not a second one beside it.
        app.reload()
        open_accounts(app)
        app.wait_for_selector("text=ui-rewired", timeout=20000)
        assert app.get_by_text("ui-rewired", exact=True).count() == 1

        self.open_editor(app, "ui-rewired")
        app.get_by_role("tab", name="How it connects").click()
        expect(app.get_by_label("Directory *")).to_have_value(moved)
        app.keyboard.press("Escape")


class TestAccountStats:
    """How full an account is, and how much of that is SAND's doing.

    A local folder is the case that made this worth drawing: a directory on a
    5 TB disk holding 34 GB of parts says nothing on its own about whether
    there is room for the next file, because the disk is shared with everything
    else on the machine. Clouds have the same problem in a milder form — a
    Drive is mostly photographs somebody put there by hand.
    """

    USED = re.compile(r"[\d.]+ [KMGT]?B / [\d.]+ [KMGT]?B used")

    def card(self, app, name):
        return (app.locator("div")
                .filter(has=app.get_by_text(name, exact=True))
                .filter(has=app.get_by_role("button", name="Stats"))
                .last)

    def test_a_local_folder_shows_the_drive_it_sits_on(self, app):
        card = self.card(app, "ui-one")
        text = card.inner_text()

        assert self.USED.search(text), f"no drive figures on the card: {text!r}"
        assert "free" in text

        # And the bar under the figures is drawn from them: a segment for what
        # SAND holds, one for everything else on the drive.
        bar = card.locator("div").filter(has_not=app.locator("button")).last
        assert bar.count() == 1

    def test_the_stats_panel_breaks_the_account_down(self, app, tmp_path):
        # Named to sort after the files earlier classes open by position: the
        # vault is shared across the session, and a file landing at the top of
        # the listing changes what "the first row" means for everyone.
        source = tmp_path / "stats-breakdown.txt"
        source.write_text("something to weigh" * 500)
        upload_and_settle(app, source, choose=["ui-one", "ui-two", "ui-three"])

        self.card(app, "ui-one").get_by_role("button", name="Stats").click()

        app.wait_for_selector("text=Capacity", timeout=30000)
        app.wait_for_selector("text=SAND's parts", timeout=30000)
        app.wait_for_selector("text=What SAND keeps here", timeout=30000)

        body = app.locator('div[role="dialog"]').inner_text()
        # The drive's three figures, each named rather than left to a colour.
        assert "everything else on it" in body
        assert "free" in body
        # What the parts belong to, and where they came from.
        assert "documents" in body
        assert "/stats-breakdown.txt" in body

    def test_the_panel_says_when_an_account_is_the_only_copy(self, app, tmp_path):
        """The count that matters before disconnecting one: the disconnect
        guard refuses on exactly this, and the panel says it beforehand."""
        source = tmp_path / "stats-pinned.txt"
        source.write_text("only two clouds hold this")
        upload_and_settle(app, source, choose=["ui-one", "ui-two"])

        self.card(app, "ui-one").get_by_role("button", name="Stats").click()
        app.wait_for_selector("text=could not be rebuilt without this account", timeout=30000)
        app.keyboard.press("Escape")


class TestABucketWithNoQuota:
    """The account that cannot answer either question about itself.

    S3 has no quota call — it never has, and Backblaze's own API adds none — so
    a bucket has always shown what SAND put there and nothing to measure it
    against. Both halves are answerable, and neither comes from the service:
    what is in the bucket is counted by listing it, and how big the bucket is
    said to be is typed by whoever pays for it.

    Its own server, because the vault behind it holds one S3 account and the
    shared one holds three local folders that other tests count.
    """

    def open_vault(self, page, bucket_server, vault_password):
        page.goto(bucket_server)
        if page.get_by_text("Create your vault").count() > 0:
            boxes = page.locator('input[autocomplete="new-password"]')
            boxes.nth(0).fill(vault_password)
            boxes.nth(1).fill(vault_password)
            page.get_by_text("▶ Create vault").click()
        else:
            page.locator('input[type="password"]').first.fill(vault_password)
            page.get_by_text("▶ Unlock").click()
        open_accounts(page)
        return page

    def test_the_panel_counts_the_bucket_it_cannot_ask(self, page, bucket_server, vault_password):
        """Opening Stats on a bucket counts it, once, and says both that it
        counted and that the count has nothing to be measured against."""
        app = self.open_vault(page, bucket_server, vault_password)
        app.get_by_role("button", name="Stats").first.click()

        app.wait_for_selector("text=counted by listing it", timeout=30000)
        app.wait_for_selector("text=Count again", timeout=30000)

        body = app.locator('div[role="dialog"]').inner_text()
        # The two objects in the bucket: one that could be a part of ours, one
        # that was already there. Nothing of this vault has landed on it, so
        # every byte counted belongs to somebody else.
        assert "everything else on it" in body
        assert "4.8 MB" in body, body
        # And no bar, because there is no whole to draw one against.
        assert "ROOM LEFT\n—" in body or "—\nROOM LEFT" in body, body
        app.keyboard.press("Escape")

    def test_a_declared_capacity_turns_the_count_into_a_bar(self, page, bucket_server, vault_password):
        """The figure the backend cannot supply is typed in Edit, and the
        account card gets the line every other account has."""
        app = self.open_vault(page, bucket_server, vault_password)

        # The count first, since that is the half a capacity is measured
        # against: a denominator with no numerator would draw the account as
        # empty, so the bar waits for something to have looked.
        app.get_by_role("button", name="Stats").first.click()
        app.wait_for_selector("text=Count again", timeout=30000)
        app.keyboard.press("Escape")

        app.get_by_role("button", name="Edit").first.click()
        app.wait_for_selector("text=Edit account", timeout=20000)
        app.get_by_label("Capacity").fill("10 GB")
        app.get_by_role("button", name="Save").click()

        app.wait_for_selector("text=10 GB used", timeout=30000)
        card = app.get_by_text("b2-cold", exact=True).first.locator("xpath=ancestor::div[3]")
        assert "free" in card.inner_text()

        # And the panel behind it now names the whole as the account holder's
        # own figure rather than the service's.
        app.get_by_role("button", name="Stats").first.click()
        app.wait_for_selector("text=the capacity you set for this account", timeout=30000)
        body = app.locator('div[role="dialog"]').inner_text()
        assert "free" in body and "10 GB" in body, body
        app.keyboard.press("Escape")

        # Cleared, the account goes back to saying nothing about its size.
        app.get_by_role("button", name="Edit").first.click()
        app.wait_for_selector("text=Edit account", timeout=20000)
        app.get_by_label("Capacity").fill("")
        app.get_by_role("button", name="Save").click()
        app.wait_for_timeout(1500)
        assert "10 GB used" not in app.get_by_text("b2-cold", exact=True).first.locator(
            "xpath=ancestor::div[3]").inner_text()


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

    @pytest.mark.slow
    def test_shard_inspector_says_how_many_chunks_a_file_has(self, app, tmp_path):
        """One row per shard, naming that shard's first chunk — which is fine
        for a file that is thousands of objects, as long as the dialog says how
        many chunks there are and which one the row is naming."""
        # Over the 16 MiB chunk length, so the file really is stored as more
        # than one chunk and the rows really are naming the first of them.
        source = tmp_path / "chunky.bin"
        source.write_bytes(b"one chunk is not the whole file." * (17 * 1024 * 1024 // 32))

        upload_and_settle(app, source)

        row = app.locator('button[title="Open"]', has_text="chunky.bin").locator("xpath=..")
        row.locator('button[title="Where the shards live"]').click()

        inspector = app.get_by_role("dialog", name="Where this file lives")
        inspector.wait_for(timeout=20000)
        expect(inspector).to_contain_text("Enough parts are reachable", timeout=60000)

        # How many there are, said before the list and again on every row, so
        # that a key ending `-c0000000-p1.sand` cannot be read as the whole of
        # what that account is holding.
        expect(inspector).to_contain_text("across 2 chunks")
        expect(inspector).to_contain_text("stored as 2 chunks")
        assert inspector.inner_text().count("chunk 1 of 2") >= 3

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
        app.set_input_files(FILE_INPUT, str(source))
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

    def test_every_row_says_how_much_more_fits(self, app, tmp_path):
        """The figure that makes the choice a choice.

        What a cloud is already holding is the same number on a drive with room
        to spare and one with none, so each row says the room left beside it. A
        local folder reports the drive under it, so all three answer here."""
        self._open_picker(app, tmp_path, "room.txt")
        try:
            for name in ("ui-one", "ui-two", "ui-three"):
                row = app.get_by_role("checkbox").filter(has_text=name)
                assert re.search(r"[\d.]+ [KMGT]?B free", row.inner_text()), row.inner_text()
        finally:
            app.get_by_role("button", name="Cancel").click()
            app.wait_for_selector("text=/Upload to \\d+ cloud/", state="detached", timeout=20000)

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


class TestUploadingMoreThanOneRequestCarries:
    """A choice too big for one request goes up as several.

    Everything picked used to go in a single multipart body: a folder of a
    hundred photos was one request that had to arrive whole before any of it
    was stored, one failure lost all of it, and every thumbnail in it was made
    before a byte was sent — which on a folder of any size took the tab down
    with it, and a tab that is gone has nowhere to put an error. Now it goes in
    batches, and what these prove is that the seam does not lose anything: the
    same files arrive, in the right folder, and it really did take more than
    one request to put them there.
    """

    def files(self, tmp_path, count):
        """`count` small files, named so the last one to arrive is knowable."""
        made = []
        for i in range(count):
            f = tmp_path / f"page-{i:02d}.txt"
            f.write_text(f"page {i}")
            made.append(str(f))
        return made

    def test_every_file_arrives_and_it_took_several_requests(self, app, tmp_path):
        make_folder(app, "batched")
        app.get_by_text("batched", exact=True).first.click()
        app.wait_for_load_state("networkidle")

        # More than the per-request file count the client cuts at (upload.js:
        # BATCH_FILES), so the choice cannot go up as one request.
        sources = self.files(tmp_path, 30)

        posts = []
        app.on("request", lambda r: (
            posts.append(r.url)
            if r.method == "POST" and "/api/files" in r.url else None))

        app.set_input_files(FILE_INPUT, sources)
        confirm = app.get_by_role("button", name=re.compile(r"Upload to \d+ cloud"))
        confirm.wait_for(timeout=20000)
        confirm.click()

        app.wait_for_selector('button[aria-label="Download page-29.txt"]', timeout=180000)
        app.wait_for_load_state("networkidle")

        # All thirty, and nothing tipped out into the folder above.
        assert sorted(listed_files(app)) == sorted(os.path.basename(f) for f in sources)
        assert len(posts) > 1, "thirty files went up as one request — the batching is gone"

        # And they are real files on the clouds, not index rows: one of them
        # rebuilds off the parts.
        app.locator('button[title="Open"]', has_text="page-17.txt").click()
        app.wait_for_selector("text=page 17", timeout=60000)
        app.keyboard.press("Escape")


class TestUploadingAFolder:
    """A folder can be uploaded, not only the files inside one.

    A browser will not hand over a folder: it hands over the files, each
    carrying the path it had inside the folder that was chosen, and the shape
    has to be rebuilt on the other side. What these prove is that it is — that
    what comes back is the folder that was picked and not its contents tipped
    out flat into whatever was on screen.
    """

    def tree(self, tmp_path, name):
        """A small folder with depth, and two files sharing a name across it.

        The repeated name is the point: flattening a tree is not a subtle bug
        when two files called cover.txt land in the same folder, and it is
        invisible when every name is unique.
        """
        root = tmp_path / name
        (root / "2024" / "summer").mkdir(parents=True)
        (root / "2023").mkdir(parents=True)
        (root / "hike.txt").write_text("a ridge")
        (root / "2024" / "summer" / "cover.txt").write_text("summer cover")
        (root / "2023" / "cover.txt").write_text("last year's cover")
        return root

    def upload(self, app, source, choose=None):
        app.set_input_files(FOLDER_INPUT, str(source))
        confirm = app.get_by_role("button", name=re.compile(r"Upload to \d+ cloud"))
        confirm.wait_for(timeout=20000)
        if choose is not None:
            select_clouds(app, choose)
        confirm.click()
        app.wait_for_selector(
            f'button[title="Open folder"]:has-text("{os.path.basename(source)}")', timeout=90000)
        app.wait_for_load_state("networkidle")

    def test_the_folder_arrives_as_a_folder(self, app, tmp_path):
        source = self.tree(tmp_path, "gui-tree")
        self.upload(app, source)

        # The folder itself, in the folder it was uploaded into.
        app.get_by_text("gui-tree", exact=True).first.click()
        app.wait_for_selector('button[aria-label="Download hike.txt"]', timeout=20000)
        assert listed_files(app) == ["hike.txt"]
        for year in ("2023", "2024"):
            assert app.get_by_text(year, exact=True).count() >= 1

        # And the depth below it, with the two cover.txt kept apart by the
        # folders they arrived in rather than collided into one.
        app.get_by_text("2024", exact=True).first.click()
        app.get_by_text("summer", exact=True).first.click()
        app.wait_for_selector('button[aria-label="Download cover.txt"]', timeout=20000)
        assert listed_files(app) == ["cover.txt"]

        # The file rebuilds off the clouds, which is the only proof the parts
        # really went out and came back.
        app.locator('button[title="Open"]', has_text="cover.txt").click()
        app.wait_for_selector("text=summer cover", timeout=60000)
        app.keyboard.press("Escape")

    def test_the_picker_names_the_folder_rather_than_its_files(self, app, tmp_path):
        source = self.tree(tmp_path, "gui-named")
        app.set_input_files(FOLDER_INPUT, str(source))
        app.wait_for_selector("text=/Upload to \\d+ cloud/", timeout=20000)

        # "Upload gui-named", not "Upload 3 files": what was chosen was a
        # folder, and the count belongs underneath it.
        expect(app.get_by_role("heading", name="Upload gui-named")).to_be_visible()
        assert app.get_by_text(re.compile(r"3 files · ")).count() == 1

        app.get_by_role("button", name="Cancel").click()
        app.wait_for_selector("text=/Upload to \\d+ cloud/", state="detached", timeout=20000)
        assert app.get_by_text("gui-named", exact=True).count() == 0

    def test_upload_offers_files_or_a_folder(self, app):
        app.get_by_role("button", name="↑ Upload").click()
        sheet = app.get_by_role("dialog", name="Upload into this folder")
        sheet.wait_for(timeout=20000)

        expect(app.get_by_text("Everything inside it, however deep", exact=False)).to_be_visible()
        assert app.get_by_text("One or several, picked by hand").count() == 1

        app.keyboard.press("Escape")
        sheet.wait_for(state="detached", timeout=20000)


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


class TestWhatAFolderIsHolding:
    """A folder's menu leads with the figures its row cannot show.

    A file's row says how big it is; a folder's cannot, because a folder's size
    is the sum of the levels below it. The menu is where somebody has asked, so
    that is where it is counted.
    """

    def test_the_menu_says_what_is_under_the_folder(self, app, tmp_path):
        make_folder(app, "held-outer")
        app.get_by_text("held-outer").first.click()
        app.wait_for_load_state("networkidle")

        top = tmp_path / "top.txt"
        top.write_text("x" * 4096)
        upload_and_settle(app, top)

        # One level down, so the figures have something the listing cannot show.
        make_folder(app, "held-inner")
        app.get_by_text("held-inner").first.click()
        app.wait_for_load_state("networkidle")
        deep = tmp_path / "deep.txt"
        deep.write_text("y" * 8192)
        upload_and_settle(app, deep)

        # Back out to where the folder itself is a row: one level for the inner
        # folder, one for the outer.
        app.locator('button[aria-label="Up"]').click()
        app.wait_for_selector("text=held-inner", timeout=20000)
        app.locator('button[aria-label="Up"]').click()
        app.wait_for_selector('button[aria-label="Actions for held-outer"]', timeout=20000)

        app.locator('button[aria-label="Actions for held-outer"]').click()
        sheet = app.get_by_role("dialog", name="held-outer")
        sheet.wait_for(timeout=20000)

        # Both files, however deep they sit — not just the one in the folder.
        # The labels are drawn uppercase by the stylesheet; the text is not.
        expect(sheet.get_by_text("files", exact=True)).to_have_count(1, timeout=20000)
        expect(sheet.get_by_text("2", exact=True)).to_have_count(1)

        # What it costs on the accounts is the other figure, and the larger one:
        # a file cut two-of-three is stored one and a half times over.
        expect(sheet.get_by_text("across the clouds", exact=False).first).to_be_visible()
        expect(sheet.get_by_text("1 folder inside", exact=False).first).to_be_visible()
        expect(sheet.get_by_text("clouds", exact=True)).to_have_count(1)

        # And the choices are all still there under it.
        expect(sheet.get_by_text("Delete folder", exact=True)).to_have_count(1)

    def test_an_empty_folder_says_so_instead_of_counting_zeroes(self, app):
        make_folder(app, "held-empty")

        app.locator('button[aria-label="Actions for held-empty"]').click()
        sheet = app.get_by_role("dialog", name="held-empty")
        sheet.wait_for(timeout=20000)

        expect(sheet.get_by_text("nothing in here yet", exact=False).first).to_be_visible(timeout=20000)
        expect(sheet.get_by_text("files", exact=True)).to_have_count(0)


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
        open_accounts(app)
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


class TestReadSpeed:
    """Reading a file is a race, and the panel next to the settings is where it
    is scored.

    Every account holding a shard is asked at once and the first to answer
    rebuild the file; the rest are cut off mid-download. Nothing else in the app
    says a word about who keeps winning, which is why a cloud can stop pulling
    its weight without anything looking wrong.
    """

    def _open(self, app):
        open_accounts(app)
        app.get_by_role("button", name=re.compile(r"^Read speed")).click()
        panel = app.get_by_role("dialog", name="Read speed")
        panel.wait_for(timeout=20000)
        return panel

    def test_the_board_names_the_clouds_that_answered(self, app, tmp_path):
        body = "the clouds race each other for this one\n"
        source = tmp_path / "raced.txt"
        source.write_text(body)

        upload_and_settle(app, source, choose=["ui-one", "ui-two", "ui-three"])

        # Opening it is the race: three accounts asked, the quickest answers
        # used, and the rest cancelled. By name rather than by position — the
        # vault is shared across the session, so the first row is whatever
        # sorts first rather than the file this test just stored.
        app.locator('button[title="Open"]', has_text="raced.txt").click()
        app.wait_for_selector(f"text={body.strip()}", timeout=60000)
        app.keyboard.press("Escape")

        panel = self._open(app)
        # One race or many: the label under the figure is written for what it
        # counts, so the assertion has to be too.
        expect(panel.get_by_text(re.compile(r"^races?$"))).to_be_visible(timeout=20000)

        # The three charts, each answering a different question about the same
        # race: who carried it, how long each account took, and what became of
        # every shard it was asked for.
        for heading in ("Share of the reads", "How long an answer takes", "Who answers"):
            expect(panel.get_by_role("heading", name=heading)).to_be_visible()
        # The outcome bars are read through their key, so identity never rests
        # on the colour of a 8px bar alone.
        for entry in ("won", "too late", "cut off", "failed"):
            expect(panel.get_by_text(entry, exact=True).first).to_be_visible()

        # Every connected account is on the board, including any that won
        # nothing — an account winning none of its races is the finding this
        # panel exists for, and leaving it out would hide exactly that.
        for name in ("ui-one", "ui-two", "ui-three"):
            expect(panel.get_by_text(name, exact=True).first).to_be_visible()
        expect(panel.get_by_text(re.compile(r"\d+ won")).first).to_be_visible()

        app.keyboard.press("Escape")
        panel.wait_for(state="detached", timeout=20000)

    def test_starting_again_empties_the_board(self, app):
        panel = self._open(app)
        panel.get_by_role("button", name="Start again").click()

        # Nothing is stored, so a cleared board is a board with no races on it
        # rather than one showing zeroes against the old figures.
        expect(panel.get_by_text("No reads yet")).to_be_visible(timeout=20000)

        app.keyboard.press("Escape")
        panel.wait_for(state="detached", timeout=20000)


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

        heading = app.get_by_text("Connected clouds", exact=True)
        assert not heading.is_visible(), "the sidebar should be closed on a phone"

        app.locator('button[aria-label="Connected clouds"]').click()
        app.wait_for_timeout(400)
        assert heading.is_visible()

        # Escape closes it again, the same as every other layer in the app.
        app.keyboard.press("Escape")
        app.wait_for_timeout(400)
        assert not heading.is_visible()

    def test_sidebar_starts_minimised_on_a_wide_screen(self, app):
        """Room for both panes is not a reason to spend it on the sidebar.

        The clouds start folded away on a desktop exactly as they do on a
        phone, and the same ☰ brings them out — and puts them back, which is
        the one thing the phone's drawer does differently (it has an ✕ of its
        own, because the drawer covers the header).
        """
        # The fixture opened the panel for everything that comes after it, so
        # a reload is what a first look at the app actually looks like.
        app.reload()
        toggle = app.locator('button[aria-label="Connected clouds"]')
        toggle.wait_for(state="visible", timeout=20000)

        heading = app.get_by_text("Connected clouds", exact=True)
        assert not heading.is_visible(), "the sidebar should start minimised"
        # Folded away, not merely scrolled off: nothing in it is tabbable.
        assert app.get_by_text("+ Connect a cloud").count() > 0
        assert not app.get_by_text("+ Connect a cloud").is_visible()

        toggle.click()
        app.wait_for_timeout(400)
        assert heading.is_visible()

        toggle.click()
        app.wait_for_timeout(400)
        assert not heading.is_visible()

    def test_the_sidebar_folds_back_from_inside_on_a_wide_screen(self, app):
        open_accounts(app)
        heading = app.get_by_text("Connected clouds", exact=True)
        assert heading.is_visible()

        app.locator('button[aria-label="Minimise"]').click()
        app.wait_for_timeout(400)
        assert not heading.is_visible()

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
        open_accounts(page)

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


class TestDiscardingAFoundVault:
    """The old install nobody wants back.

    An account reconnected out of tidiness still carries the index of whichever
    vault last used it, so the sub vaults panel offers to import it — and goes
    on offering, since nothing about that account will ever change on its own.
    The trash button beside the offer is how somebody says the files behind it
    are not wanted.

    What it erases is that index and nothing else. The parts stay on the
    account, which is why the dialog says so rather than implying a cleanup it
    is not doing: what changes is that they stop being another vault's, so the
    stray-parts sweep will offer them with a size on them.
    """

    def first_run(self, page, base_url, cloud):
        """A fresh vault on a new machine, with one dead cloud wired back up."""
        page.goto(base_url)
        page.wait_for_selector("text=Create your vault", timeout=20000)
        boxes = page.locator('input[autocomplete="new-password"]')
        boxes.nth(0).fill("a-brand-new-passphrase")
        boxes.nth(1).fill("a-brand-new-passphrase")
        page.get_by_text("▶ Create vault").click()
        open_accounts(page)

        page.get_by_text("+ Connect a cloud").click()
        page.wait_for_selector("text=Local folder", timeout=15000)
        page.get_by_text("Local folder").click()
        form = page.locator("form")
        form.locator("input").nth(0).fill("tidied-up")
        form.locator("input").nth(1).fill(cloud)
        form.locator("button[type=submit]").click()
        page.wait_for_selector("text=tidied-up", timeout=30000)

        # An empty vault beside a foreign index is the recovery prompt's
        # business first. Saying no to it is what leaves somebody in front of
        # the panel this is about.
        not_now = page.get_by_role("button", name="Not now")
        if not_now.count() > 0:
            not_now.click()
            page.wait_for_timeout(300)

    def test_the_trash_button_erases_the_index_it_was_offering(
        self, page, spawn_server, lost_vault,
    ):
        clouds, _, _ = lost_vault
        self.first_run(page, spawn_server("case-discard"), clouds[0])

        open_vault_setting(page, "Sub vaults")
        panel = page.get_by_role("dialog", name="Sub vaults")
        panel.wait_for(timeout=20000)

        # The row, with both of the things that can be done to it.
        row = panel.get_by_text("holds another vault’s index")
        expect(row).to_be_visible(timeout=30000)
        expect(panel.get_by_role("button", name="Import")).to_be_visible()
        panel.get_by_role("button", name="🗑").click()

        # In front of the panel that opened it, and plain about the half it is
        # not doing.
        dialog = page.get_by_role("dialog", name=re.compile(r"^Forget the vault on"))
        dialog.wait_for(timeout=20000)
        expect(dialog.get_by_text(re.compile(r"stay on the account as parts"))).to_be_visible()
        dialog.get_by_role("button", name="Erase the index").click()

        # Gone from the panel, and the panel says so in the way it says it when
        # there was never anything there.
        expect(panel.get_by_text("holds another vault’s index")).to_have_count(0, timeout=30000)
        expect(panel.get_by_text("Nothing but this vault’s own")).to_be_visible(timeout=30000)

        # And it stays gone: a fresh scan is the same answer, not a row that
        # comes back the moment anyone looks again.
        panel.get_by_role("button", name=re.compile(r"^Scan accounts for vaults")).click()
        expect(panel.get_by_text("Nothing but this vault’s own")).to_be_visible(timeout=30000)


class TestSubVaultDeletion:
    """Throwing away a vault that is inside the vault.

    Deleting one erases every part it owns from every account, and no backup
    undoes it — the backups carry the sub vault sealed under the password being
    thrown away. So it asks first, and the asking is a dialog opened from
    inside the sub vaults panel, which is itself a dialog. It has to be drawn
    in front of the panel that opened it: behind, it is still on the page and
    still visible, and the only thing that notices is the click, which lands on
    the panel's backdrop instead. From the outside that is a trash button that
    does nothing at all.
    """

    def test_deleting_one_asks_first_and_takes_its_contents_with_it(self, app, tmp_path):
        make_sub_vault(app, "doomed-vault", "doomed-vault-passphrase")

        # A file put inside it, so what is being erased is not an empty record.
        source = tmp_path / "sealed.txt"
        source.write_text("erased with the sub vault that held it")
        upload_and_settle(app, source, choose=["ui-one", "ui-two", "ui-three"])

        # Back out to the vault the sub vault sits in.
        app.locator('button[aria-label="Back"]').click()
        app.wait_for_selector("text=▣ /", timeout=20000)
        # Nothing in a sub vault is listed by the vault holding it, which is
        # what makes the delete the only way its file can go.
        expect(app.locator('button[aria-label="Download sealed.txt"]')).to_have_count(
            0, timeout=20000)

        # Listed, with the count the vault keeps for it while it is shut.
        panel = open_sub_vaults_panel(app)
        expect(panel.get_by_text("doomed-vault", exact=True)).to_be_visible(timeout=20000)
        expect(panel).to_contain_text("1 file", timeout=20000)

        # The trash button, and the confirmation it is supposed to raise.
        panel.get_by_title("Delete this sub vault and erase everything in it").click()
        confirm = app.get_by_role("dialog", name="Delete doomed-vault?")
        confirm.wait_for(timeout=20000)

        # It says what is about to go, and it is in front of the panel that
        # asked for it — which is the whole of the bug this guards.
        expect(confirm).to_contain_text("1 file", timeout=20000)
        erase = confirm.get_by_role("button", name="Delete and erase")
        assert clickable(app, erase), (
            "the confirmation is drawn behind the sub vaults panel — a click "
            "aimed at it lands on the panel's backdrop, so the trash button "
            "reads as one that does nothing")

        # Backing out erases nothing.
        confirm.get_by_role("button", name="Cancel").click()
        confirm.wait_for(state="detached", timeout=20000)
        expect(panel.get_by_text("doomed-vault", exact=True)).to_be_visible(timeout=20000)

        # Going through with it takes the sub vault and the file in it.
        panel.get_by_title("Delete this sub vault and erase everything in it").click()
        confirm = app.get_by_role("dialog", name="Delete doomed-vault?")
        confirm.wait_for(timeout=20000)
        confirm.get_by_role("button", name="Delete and erase").click()

        confirm.wait_for(state="detached", timeout=60000)
        expect(panel.get_by_text("doomed-vault", exact=True)).to_have_count(0, timeout=30000)
        expect(panel.get_by_text("No sub vaults yet.")).to_be_visible(timeout=20000)

        # And the vault it was in is left holding none.
        app.keyboard.press("Escape")
        panel.wait_for(state="detached", timeout=20000)
        close_vault_settings(app)
        expect_vault_setting(app, "Sub vaults", "None")


class TestSubVaultUnlock:
    """Opening a shut sub vault, which asks for its password once.

    The panel that lists them hands the app the row that was unlocked and the
    app walks into it — but the row is the app's own copy of the list, and the
    fresh one is still on its way back from the server, so the copy handed over
    still says locked. Trusting it over the unlock that just happened put the
    very same dialog straight back up over the one that had closed: from the
    outside, a password typed correctly, a dialog that did not move, and a
    second go at it before anything opened.
    """

    def test_one_password_opens_it(self, app):
        make_sub_vault(app, "shut-vault", "shut-vault-passphrase")

        # Out of it and shut again, which is the state the unlock starts from.
        app.locator('button[aria-label="Back"]').click()
        app.wait_for_selector("text=▣ /", timeout=20000)

        panel = open_sub_vaults_panel(app)
        panel.get_by_role("button", name="Lock", exact=True).click()
        unlock = panel.get_by_role("button", name="Unlock", exact=True)
        unlock.wait_for(timeout=20000)
        unlock.click()

        dialog = app.get_by_role("dialog", name="Unlock shut-vault")
        dialog.wait_for(timeout=20000)
        dialog.locator('input[type="password"]').fill("shut-vault-passphrase")
        dialog.get_by_role("button", name="Unlock").click()

        # The one password is the whole of it: the dialog goes, and what is
        # behind it is the inside of the sub vault rather than the same dialog
        # a second time.
        expect(dialog).to_have_count(0, timeout=30000)
        app.wait_for_selector("text=🔒 shut-vault /", timeout=30000)

        # And it does not come back when the refreshed status lands, either —
        # which is the moment the stale copy used to be replaced by.
        app.wait_for_load_state("networkidle")
        expect(app.get_by_role("dialog", name="Unlock shut-vault")).to_have_count(
            0, timeout=20000)


class TestSubVaultVisibility:
    """Which sub vaults are drawn at the top of the vault, one at a time.

    Showing them was one answer for all of them, and that is not how anybody
    holds them: the sub vault you want in front of you at the root — the one
    you are working out of this week — is rarely the one you would rather
    nobody standing behind you saw named on screen. Each row carries its own
    tick, the box underneath answers for the ones nothing has been said about,
    and the whole of it is a preference of this browser: nothing here unlocks
    anything, and nothing here puts a sub vault on a mounted drive.
    """

    def test_one_can_be_shown_while_another_stays_out_of_sight(self, app):
        make_sub_vault(app, "seen-vault", "seen-vault-passphrase")
        app.locator('button[aria-label="Back"]').click()
        app.wait_for_selector("text=▣ /", timeout=20000)

        make_sub_vault(app, "unseen-vault", "unseen-vault-passphrase")
        app.locator('button[aria-label="Back"]').click()
        app.wait_for_selector("text=▣ /", timeout=20000)

        # Nothing is drawn at the root until it is asked for, which is where
        # this starts: a fresh browser has said nothing about either of them.
        expect(app.get_by_title("Open seen-vault")).to_have_count(0, timeout=20000)
        expect(app.get_by_title("Open unseen-vault")).to_have_count(0)

        # One tick, on one row.
        panel = open_sub_vaults_panel(app)
        panel.get_by_label("Show seen-vault at the top of the vault").check()
        app.keyboard.press("Escape")
        panel.wait_for(state="detached", timeout=20000)
        close_vault_settings(app)

        # And that one is on screen while its sibling is not, which is the
        # whole of what a per-sub-vault tick is for.
        expect(app.get_by_title("Open seen-vault")).to_be_visible(timeout=20000)
        expect(app.get_by_title("Open unseen-vault")).to_have_count(0)

        # It is a preference of this browser rather than a thing the app is
        # holding in memory, so a reload is the only proof it was written down.
        app.reload()
        open_accounts(app)
        expect(app.get_by_title("Open seen-vault")).to_be_visible(timeout=20000)
        expect(app.get_by_title("Open unseen-vault")).to_have_count(0)

    def test_the_box_underneath_answers_for_all_of_them(self, app):
        make_sub_vault(app, "blanket-vault", "blanket-vault-passphrase")
        app.locator('button[aria-label="Back"]').click()
        app.wait_for_selector("text=▣ /", timeout=20000)

        make_sub_vault(app, "blanket-other", "blanket-other-passphrase")
        app.locator('button[aria-label="Back"]').click()
        app.wait_for_selector("text=▣ /", timeout=20000)

        panel = open_sub_vaults_panel(app)
        blanket = panel.get_by_label(
            "Show them at the top of the vault, locked ones included")
        panel.get_by_label("Show blanket-vault at the top of the vault").check()

        # One of several ticked is neither yes nor no, and the box says so
        # rather than sitting empty over a sub vault that is on screen.
        assert blanket.evaluate("box => box.indeterminate"), (
            "the box that answers for all of them reads as a flat no while one "
            "of them is ticked — an unticked box over a sub vault that is "
            "drawn at the root")

        # Clicking it out of that state is an answer for the lot.
        blanket.check()
        assert not blanket.evaluate("box => box.indeterminate")
        app.keyboard.press("Escape")
        panel.wait_for(state="detached", timeout=20000)
        close_vault_settings(app)

        expect(app.get_by_title("Open blanket-vault")).to_be_visible(timeout=20000)
        expect(app.get_by_title("Open blanket-other")).to_be_visible(timeout=20000)

        # And unticking it takes them all back off, individual ticks included:
        # a box saying "all of them" over a list where one is still crossed out
        # would be answering a question nobody asked.
        panel = open_sub_vaults_panel(app)
        panel.get_by_label(
            "Show them at the top of the vault, locked ones included").uncheck()
        app.keyboard.press("Escape")
        panel.wait_for(state="detached", timeout=20000)
        close_vault_settings(app)

        expect(app.get_by_title("Open blanket-vault")).to_have_count(0, timeout=20000)
        expect(app.get_by_title("Open blanket-other")).to_have_count(0)


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
        app.set_input_files(FILE_INPUT, str(source))
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
        app.set_input_files(FILE_INPUT, str(source))
        app.wait_for_selector("text=/Upload to \\d+ cloud/", timeout=20000)

        select_clouds(app, ["ui-one", "ui-two", "ui-three", "ui-four"])
        app.wait_for_selector("text=4 clouds names no scheme", timeout=20000)
        expect(app.get_by_role("button", name=re.compile("Upload to 4 clouds"))).to_be_disabled()

        app.get_by_role("button", name="Cancel").click()
        app.wait_for_selector("text=/Upload to \\d+ cloud/", state="detached", timeout=20000)
        assert app.get_by_text("between.txt").count() == 0

    def test_widening_a_file_to_six_clouds_is_offered_not_greyed_out(
        self, app, tmp_path, clouds
    ):
        """Going from three clouds to six rebuilds the file rather than
        carrying its shards, and rebuilding is work: the button has to be
        pressable. It moves no single shard, so a gate that counts only shards
        moved and erased would grey out the one change that costs the most."""
        for name in ("ui-four", "ui-five", "ui-six"):
            connect_cloud(app, name, clouds)

        source = tmp_path / "widened.txt"
        source.write_text("three clouds, then six")
        upload_and_settle(app, source, choose=["ui-one", "ui-two", "ui-three"])

        row = app.locator('button[title="Open"]', has_text="widened.txt").locator("xpath=..")
        row.locator('button[title="Where the shards live"]').click()
        app.wait_for_selector("text=Where this file lives", timeout=20000)
        app.get_by_role("button", name=re.compile("Move to other clouds")).click()
        app.wait_for_selector("text=Move widened.txt", timeout=20000)

        select_clouds(app, ["ui-one", "ui-two", "ui-three", "ui-four", "ui-five", "ui-six"])
        # The estimate says rebuild rather than move — no shard travels — and
        # the button is live all the same.
        app.wait_for_selector("text=1 file to rebuild", timeout=20000)
        move = app.get_by_role("button", name=re.compile("Move the shards"))
        expect(move).to_be_enabled(timeout=20000)

        move.click()
        app.wait_for_selector("text=Rebuilt 1 file", timeout=120000)
        app.get_by_role("button", name="Done").click()

        # Six shards now, one per cloud, and it still rebuilds.
        row = app.locator('button[title="Open"]', has_text="widened.txt").locator("xpath=..")
        for name in ("ui-one", "ui-two", "ui-three", "ui-four", "ui-five", "ui-six"):
            expect(row.get_by_title(re.compile(rf"Shard \d on {name}$"))).to_have_count(
                1, timeout=20000
            )

        app.locator('button[title="Open"]', has_text="widened.txt").click()
        app.wait_for_selector("text=three clouds, then six", timeout=60000)
        app.keyboard.press("Escape")


# ---------------------------------------------------------------------------
# The files that went out short
# ---------------------------------------------------------------------------

def _shorten(page, base, password, tmp_path, files):
    """Fill a fresh vault with `files` files that are each one part short.

    Set up over HTTP behind the open app rather than through the UI: what is
    being tested is the line at the foot of the accounts panel and the dialog
    behind it, and uploading a file and then disconnecting one of the clouds it
    went to is scaffolding. A forced disconnect forgets the shard records
    pointing at the account, which leaves exactly the state an upload lands in
    when one cloud was not answering — the file is there, still readable, one
    part short of the spread it asked for.

    Four accounts, uploads onto the first three, and the first disconnected: a
    part goes off every file, and the fourth cloud is left free to move onto.
    """
    session = requests.Session()
    headers = {"Origin": base}
    r = session.post(f"{base}/api/vault/unlock", json={"password": password},
                     headers=headers, timeout=60)
    assert r.status_code == 200, r.text

    ids = []
    for i in range(4):
        path = tmp_path / "short-clouds" / f"cloud-{i}"
        path.mkdir(parents=True, exist_ok=True)
        r = session.post(f"{base}/api/providers",
                         json={"kind": "local", "name": f"short-{i}",
                               "options": {"path": str(path)}},
                         headers=headers, timeout=60)
        assert r.status_code == 201, r.text
        ids.append(r.json()["provider"]["id"])

    names = []
    for i in range(files):
        name = f"shortened{i:02d}.txt"
        names.append(name)
        r = session.post(
            f"{base}/api/files",
            files={"files[]": (name, b"one of its parts never landed")},
            data=[("path", "/"), ("accounts", ids[0]), ("accounts", ids[1]), ("accounts", ids[2])],
            headers=headers, timeout=120,
        )
        assert r.status_code == 201, r.text

    r = session.delete(f"{base}/api/providers/{ids[0]}?force=1", headers=headers, timeout=60)
    assert r.status_code == 200, r.text

    # All of it happened while the page was looking away.
    page.reload()
    open_accounts(page)
    return names


class TestFilesMissingAPart:
    """The count at the foot of the accounts panel, and the way into it.

    A part goes missing when the cloud meant to hold it was not answering as
    the file was scattered. Nothing fails: the upload succeeds, the file reads
    back, and it is one cloud worse off than it asked to be for good, because
    nothing ever goes back to finish it. The panel has always counted those.
    These tests are about the count being a door — which files, and the choice
    of other clouds for each of them, without leaving the panel.

    Own server and own vault per test, because making a file short means
    disconnecting an account and the shared session vault is what every other
    test in this file stands on.
    """

    PASSWORD = "one-part-short-passphrase"

    def new_vault(self, page, base):
        """Create the vault through the first-run screen, which is also what
        gives the browser a session."""
        page.goto(base)
        page.wait_for_selector("text=Create your vault", timeout=20000)
        boxes = page.locator('input[autocomplete="new-password"]')
        boxes.nth(0).fill(self.PASSWORD)
        boxes.nth(1).fill(self.PASSWORD)
        page.get_by_text("▶ Create vault").click()
        open_accounts(page)

    def test_the_count_is_a_button_that_lists_the_files(self, page, spawn_server, tmp_path):
        base = spawn_server("short-one")
        self.new_vault(page, base)
        names = _shorten(page, base, self.PASSWORD, tmp_path, files=1)

        link = page.get_by_role("button", name=re.compile(r"^1 file missing a spare part"))
        expect(link).to_be_visible(timeout=30000)
        link.click()

        dialog = page.get_by_role("dialog", name="Files missing a part")
        dialog.wait_for(timeout=20000)
        expect(dialog.get_by_text(names[0])).to_be_visible(timeout=20000)
        # What is wrong with it, on the row: the count, and the empty square
        # where the part that never landed should be.
        expect(dialog.get_by_text("1 part missing")).to_be_visible()
        expect(dialog.get_by_title(re.compile(r"^Part \d was never stored$"))).to_have_count(1)

    def test_a_row_opens_the_picker_on_the_spread_the_file_asked_for(
        self, page, spawn_server, tmp_path
    ):
        base = spawn_server("short-move")
        self.new_vault(page, base)
        names = _shorten(page, base, self.PASSWORD, tmp_path, files=1)

        page.get_by_role("button", name=re.compile(r"missing a spare part")).click()
        dialog = page.get_by_role("dialog", name="Files missing a part")
        dialog.wait_for(timeout=20000)
        dialog.get_by_role("button", name=re.compile("Clouds")).first.click()

        # The relocation dialog, opened on the two clouds the file still has
        # plus one it does not — three, which is the spread it was uploaded
        # with. Opening on the two would open on 2-of-2, which is a narrower
        # file than the one being repaired.
        move = page.get_by_role("dialog", name=re.compile(rf"^Move {names[0]}"))
        move.wait_for(timeout=20000)
        expect(page.get_by_role("checkbox", checked=True)).to_have_count(3, timeout=20000)

        # A file short a part cannot be carried across into a full one — the
        # missing part is on no account to copy from — so it is rebuilt.
        page.wait_for_selector("text=1 file to rebuild", timeout=20000)
        button = move.get_by_role("button", name=re.compile("Move the shards"))
        expect(button).to_be_enabled(timeout=20000)
        button.click()
        page.wait_for_selector("text=Rebuilt 1 file", timeout=120000)
        page.get_by_role("button", name="Done").click()

        # Whole again, so the count at the foot of the panel is gone with it.
        expect(page.get_by_text("missing a spare part")).to_have_count(0, timeout=30000)

    def test_a_long_list_is_paged(self, page, spawn_server, tmp_path):
        base = spawn_server("short-many")
        self.new_vault(page, base)
        _shorten(page, base, self.PASSWORD, tmp_path, files=27)

        page.get_by_role("button", name=re.compile(r"^27 files missing a spare part")).click()
        dialog = page.get_by_role("dialog", name="Files missing a part")
        dialog.wait_for(timeout=20000)

        expect(dialog.get_by_text("1–25 of 27")).to_be_visible(timeout=20000)
        expect(dialog.get_by_text("shortened00.txt")).to_be_visible()
        expect(dialog.get_by_text("shortened26.txt")).to_have_count(0)
        expect(dialog.get_by_role("button", name=re.compile("Newer"))).to_be_disabled()

        dialog.get_by_role("button", name=re.compile("Older")).click()
        expect(dialog.get_by_text("26–27 of 27")).to_be_visible(timeout=20000)
        expect(dialog.get_by_text("shortened26.txt")).to_be_visible()
        expect(dialog.get_by_text("shortened00.txt")).to_have_count(0)
        expect(dialog.get_by_role("button", name=re.compile("Older"))).to_be_disabled()


class TestOrphanedParts:
    """Parts a delete could not reach, noticed by the app on its own.

    Its own server, port and vault file, because the scenario is a vault whose
    accounts have been shuffled underneath it: a cloud disconnected, a file
    deleted without it, and the same storage connected back as a new account
    carrying parts that nothing will ever go looking for again. Nobody in that
    position knows to go looking for a sweep command, which is the whole reason
    the app says so first.
    """

    PASSWORD = "the-orphan-test-passphrase"

    def build_vault(self, sand_bin, tmp_path, name):
        """A vault with three clouds, one abandoned part, and the server not yet
        started. Returns the cloud folders."""
        import subprocess

        vault_file = str(tmp_path / f"{name}.sand")

        def run(*args):
            env = dict(os.environ)
            env["SAND_PASSWORD"] = self.PASSWORD
            result = subprocess.run(
                [sand_bin, "--vault", vault_file, *args],
                capture_output=True, text=True, env=env,
            )
            assert result.returncode == 0, f"{args}\nstdout: {result.stdout}\nstderr: {result.stderr}"
            return result

        run("vault", "init", "--policy", "strict")
        clouds = []
        for cloud in ("orph-one", "orph-two", "orph-three"):
            path = str(tmp_path / "orph-clouds" / cloud)
            os.makedirs(path, exist_ok=True)
            clouds.append(path)
            run("remote", "add", "local", "--name", cloud, "--set", f"path={path}")

        for filename, body in (("doomed.txt", b"deleted while a cloud was away"),
                               ("kept.txt", b"still very much stored")):
            source = tmp_path / filename
            source.write_bytes(body)
            run("put", str(source), "--accounts", "orph-one,orph-two,orph-three")

        # The cloud goes away, the file is deleted without it, and the same
        # folder is wired back up as a new account.
        run("remote", "rm", "orph-one", "--force")
        run("rm", "/doomed.txt")
        run("remote", "add", "local", "--name", "orph-one-again", "--set", f"path={clouds[0]}")
        return clouds

    def parts_in(self, directory):
        return {n for n in os.listdir(directory)
                if n.endswith(".sand") and n != "manifest.sand"}

    def unlock(self, page, base_url):
        page.goto(base_url)
        page.locator('input[type="password"]').first.fill(self.PASSWORD)
        page.get_by_text("▶ Unlock").click()
        open_accounts(page)

    def test_the_app_says_so_without_being_asked(self, page, sand_bin, tmp_path, spawn_server):
        self.build_vault(sand_bin, tmp_path, "orphans-notice")
        self.unlock(page, spawn_server("orphans-notice"))

        # Nobody asked for this. The scan runs because the set of connected
        # clouds is what it is, and the banner names the room rather than the
        # file — what each abandoned archive was is not knowable.
        expect(page.get_by_text(re.compile(r"no file in this vault points at"))).to_be_visible(timeout=30000)

    def test_the_panel_erases_what_it_offered(self, page, sand_bin, tmp_path, spawn_server):
        clouds = self.build_vault(sand_bin, tmp_path, "orphans-sweep")
        before = self.parts_in(clouds[0])
        assert len(before) == 2, before

        self.unlock(page, spawn_server("orphans-sweep"))
        page.wait_for_selector("text=no file in this vault points at", timeout=30000)
        page.get_by_role("button", name="Take a look").click()

        expect(page.get_by_text("Parts nothing points at")).to_be_visible(timeout=15000)
        # The account that was reconnected is the one named, and the button says
        # what agreeing to it buys.
        expect(page.get_by_text("orph-one-again").first).to_be_visible()
        erase = page.get_by_role("button", name=re.compile(r"^Erase 1 object"))
        expect(erase).to_be_visible()
        erase.click()

        expect(page.get_by_text(re.compile(r"1 object erased across 1 archive"))).to_be_visible(timeout=30000)
        assert len(self.parts_in(clouds[0])) == len(before) - 1

        # And the notice is gone once the panel is shut, because there is
        # nothing left to notice.
        page.get_by_role("button", name="Done").click()
        page.wait_for_timeout(500)
        assert page.get_by_text(re.compile(r"no file in this vault points at")).count() == 0

    def test_the_settings_menu_is_the_way_back_in(self, page, sand_bin, tmp_path, spawn_server):
        """The notice is dismissible, and dismissing it used to be final.

        The banner is news: it is only there while there is something to say,
        and it goes when it is waved away. That left the panel behind it with
        no door at all — somebody who dismissed it, or who wanted to check
        before the app volunteered anything, had nothing to click. **Vault
        settings → Stray parts** is that door, and it runs its own scan rather
        than reusing whatever the app happened to have seen.
        """
        clouds = self.build_vault(sand_bin, tmp_path, "orphans-settings")
        before = self.parts_in(clouds[0])
        assert len(before) == 2, before

        self.unlock(page, spawn_server("orphans-settings"))
        notice = page.get_by_text(re.compile(r"no file in this vault points at"))
        expect(notice).to_be_visible(timeout=30000)

        # Waved away, and gone for good — the app will not raise it again until
        # the set of connected clouds changes.
        page.get_by_role("button", name="Dismiss").last.click()
        expect(notice).to_have_count(0)

        open_vault_setting(page, "Stray parts")
        strays = page.get_by_role("dialog", name="Stray parts")
        strays.wait_for(timeout=20000)

        # The scan starts when the line is clicked, not when the menu opened,
        # and it reports both halves of what one listing turns up. This vault
        # has one of each: the deleted file's part, which nothing points at,
        # and the kept file's, which the disconnect stopped the index naming.
        expect(strays.get_by_text(re.compile(r"belongs to no file in this vault"))).to_be_visible(
            timeout=30000)
        expect(strays.get_by_text(re.compile(r"on your clouds unrecorded"))).to_be_visible()

        strays.get_by_role("button", name="Take a look").click()
        expect(page.get_by_text("Parts nothing points at")).to_be_visible(timeout=15000)
        page.get_by_role("button", name=re.compile(r"^Erase 1 object")).click()
        expect(page.get_by_text(re.compile(r"1 object erased across 1 archive"))).to_be_visible(
            timeout=30000)
        assert len(self.parts_in(clouds[0])) == len(before) - 1

        # Closing a panel that changed something lands back on a fresh scan
        # rather than on the figures it was opened with — the sweep is gone
        # from it and the repair, which nothing has been done about, is not.
        page.get_by_role("button", name="Done").click()
        expect(strays.get_by_text(re.compile(r"on your clouds unrecorded"))).to_be_visible(
            timeout=30000)
        expect(strays.get_by_text(re.compile(r"belongs to no file in this vault"))).to_have_count(0)

        strays.get_by_role("button", name="Put them back").click()
        expect(page.get_by_text("Parts your files have lost track of")).to_be_visible(timeout=15000)
        page.get_by_role("button", name=re.compile(r"^Put 1 shard back")).click()
        expect(page.get_by_text(re.compile(r"1 shard recorded again"))).to_be_visible(timeout=30000)

        page.get_by_role("button", name="Done").click()
        expect(strays.get_by_text(re.compile(r"Nothing adrift"))).to_be_visible(timeout=30000)

        strays.get_by_role("button", name="Done").click()
        close_vault_settings(page)

    def test_a_vault_waiting_to_be_recovered_is_not_offered_a_sweep(
        self, page, spawn_server, lost_vault,
    ):
        """The state where the offer would be a disaster: every part on the
        accounts is unaccounted for, and every one of them is what a recovery
        needs. The recovery prompt is what belongs here, not a tidy-up."""
        clouds, _, _ = lost_vault
        base_url = spawn_server("orphans-recovery")

        page.goto(base_url)
        page.wait_for_selector("text=Create your vault", timeout=20000)
        boxes = page.locator('input[autocomplete="new-password"]')
        boxes.nth(0).fill("a-brand-new-passphrase")
        boxes.nth(1).fill("a-brand-new-passphrase")
        page.get_by_text("▶ Create vault").click()
        open_accounts(page)

        page.get_by_text("+ Connect a cloud").click()
        page.wait_for_selector("text=Local folder", timeout=15000)
        page.get_by_text("Local folder").click()
        form = page.locator("form")
        form.locator("input").nth(0).fill("back-one")
        form.locator("input").nth(1).fill(clouds[0])
        form.locator("button[type=submit]").click()

        expect(page.get_by_text("Sand files detected")).to_be_visible(timeout=30000)
        assert page.get_by_text(re.compile(r"no file in this vault points at")).count() == 0

    def test_the_vaults_own_folder_is_scanned_too(self, page, sand_bin, tmp_path, spawn_server):
        """The same question, asked of the disk the vault file is on.

        An upload is spooled to disk in full before it is sent — every chunk
        has to carry the whole file's hash, and a stream only gives that up at
        its last byte — and the spool is deleted on every way out of an upload
        except the one that is not a way out at all: the process being killed.
        What is left is the whole file that was being uploaded, sitting in
        /var/lib/sand for nobody, and until now nothing ever looked there.
        """
        self.build_vault(sand_bin, tmp_path, "orphans-leftovers")

        # What a killed upload leaves: SAND's own temporary name, some size,
        # and nothing writing to it. Backdated out of the settling window,
        # which is the only thing separating this from an upload in progress.
        spool = tmp_path / ".sand-upload-1611628659"
        spool.write_bytes(b"S" * 4096)
        stale = time.time() - 3 * 60 * 60
        os.utime(spool, (stale, stale))

        self.unlock(page, spawn_server("orphans-leftovers"))

        open_vault_setting(page, "Stray parts")
        strays = page.get_by_role("dialog", name="Stray parts")
        strays.wait_for(timeout=20000)

        # Said as room on this machine, beside what the clouds are holding.
        expect(strays.get_by_text(re.compile(r"working files sit in this vault"))).to_be_visible(
            timeout=30000)

        strays.get_by_role("button", name="Tidy up").click()
        expect(page.get_by_text("Working files left behind")).to_be_visible(timeout=15000)
        expect(page.get_by_text(".sand-upload-1611628659")).to_be_visible()

        page.get_by_role("button", name=re.compile(r"^Erase 1 file")).click()
        expect(page.get_by_text(re.compile(r"1 working file erased"))).to_be_visible(timeout=30000)
        assert not spool.exists()

        # And the vault file itself, in the same folder under a name that is
        # not SAND's scratch, is exactly where it was.
        assert (tmp_path / "orphans-leftovers.sand").exists()

        page.get_by_role("button", name="Done").click()
        strays.get_by_role("button", name="Done").click()
        close_vault_settings(page)


class TestMislaidShards:
    """The repair for a disconnect, offered by the app on its own.

    A cloud is disconnected — the case in the wild is an OAuth client that no
    longer exists — which drops every index record naming it while the parts
    stay on the account. Connect the storage back and the file still says it is
    missing a spare part, with the spare sitting on a connected cloud. Nobody
    would think to go looking for that, which is why the app says so first.
    """

    PASSWORD = "the-mislaid-test-passphrase"

    def build_vault(self, sand_bin, tmp_path, name, reconnect=True):
        """A vault whose one file has lost a shard record to a disconnect.

        With reconnect=False the cloud is left disconnected, so the app has to
        discover the mislaid shard when it is wired back up in the browser.
        """
        import subprocess

        vault_file = str(tmp_path / f"{name}.sand")

        def run(*args):
            env = dict(os.environ)
            env["SAND_PASSWORD"] = self.PASSWORD
            result = subprocess.run(
                [sand_bin, "--vault", vault_file, *args],
                capture_output=True, text=True, env=env,
            )
            assert result.returncode == 0, f"{args}\nstdout: {result.stdout}\nstderr: {result.stderr}"
            return result

        run("vault", "init", "--policy", "strict")
        clouds = {}
        for cloud in ("ms-a", "ms-b", "ms-c"):
            path = str(tmp_path / "ms-clouds" / cloud)
            os.makedirs(path, exist_ok=True)
            clouds[cloud] = path
            run("remote", "add", "local", "--name", cloud, "--set", f"path={path}")

        source = tmp_path / "important.txt"
        source.write_bytes(b"the file that lost a spare")
        run("put", str(source), "--accounts", "ms-a,ms-b,ms-c")

        run("remote", "rm", "ms-a", "--force")
        if reconnect:
            run("remote", "add", "local", "--name", "ms-a-again", "--set", f"path={clouds['ms-a']}")
        return clouds

    def parts_in(self, directory):
        return {n for n in os.listdir(directory)
                if n.endswith(".sand") and n != "manifest.sand"}

    def unlock(self, page, base_url):
        page.goto(base_url)
        page.locator('input[type="password"]').first.fill(self.PASSWORD)
        page.get_by_text("▶ Unlock").click()
        open_accounts(page)

    def test_the_app_offers_to_put_them_back(self, page, sand_bin, tmp_path, spawn_server):
        clouds = self.build_vault(sand_bin, tmp_path, "mislaid-notice")
        before = {name: self.parts_in(path) for name, path in clouds.items()}

        self.unlock(page, spawn_server("mislaid-notice"))

        # Nobody asked. The banner leads on the thing that got worse, and on
        # the fact that fixing it is free.
        expect(page.get_by_text(re.compile(r"nothing pointing at"))).to_be_visible(timeout=30000)
        expect(page.get_by_text(re.compile(r"moves no data"))).to_be_visible()

        page.get_by_role("button", name="Put them back").click()
        expect(page.get_by_text("Parts your files have lost track of")).to_be_visible(timeout=15000)
        expect(page.get_by_text("/important.txt").first).to_be_visible()

        page.get_by_role("button", name=re.compile(r"^Put 1 shard back")).click()
        expect(page.get_by_text(re.compile(r"1 shard recorded again"))).to_be_visible(timeout=30000)
        expect(page.get_by_text(re.compile(r"No data was transferred"))).to_be_visible()

        # The index changed and the clouds did not.
        for name, path in clouds.items():
            assert self.parts_in(path) == before[name], f"{name} was written to"

        page.get_by_role("button", name="Done").click()
        page.wait_for_timeout(500)
        assert page.get_by_text(re.compile(r"nothing pointing at")).count() == 0

    def test_connecting_the_cloud_back_is_what_finds_them(self, page, sand_bin, tmp_path, spawn_server):
        """The scenario as it actually happens.

        The cloud is gone and the app has nothing to say — the parts are on
        storage it cannot reach. Wire that storage back up and it arrives as a
        new account with a new id, which is exactly the moment the vault stops
        being able to work out for itself that these are the parts it lost. So
        the app looks, right then, without being asked.
        """
        clouds = self.build_vault(sand_bin, tmp_path, "mislaid-reconnect", reconnect=False)
        self.unlock(page, spawn_server("mislaid-reconnect"))

        # Nothing to report yet: the parts are on a cloud that is not connected.
        page.wait_for_timeout(1500)
        assert page.get_by_text(re.compile(r"nothing pointing at")).count() == 0

        page.get_by_text("+ Connect a cloud").click()
        page.wait_for_selector("text=Local folder", timeout=15000)
        page.get_by_text("Local folder").click()
        form = page.locator("form")
        form.locator("input").nth(0).fill("ms-a-again")
        form.locator("input").nth(1).fill(clouds["ms-a"])
        form.locator("button[type=submit]").click()
        page.wait_for_selector("text=ms-a-again", timeout=30000)

        # Connecting it back is the trigger. Nobody asked for a scan.
        expect(page.get_by_text(re.compile(r"nothing pointing at"))).to_be_visible(timeout=30000)
        page.get_by_role("button", name="Put them back").click()
        page.get_by_role("button", name=re.compile(r"^Put 1 shard back")).click()
        expect(page.get_by_text(re.compile(r"1 shard recorded again"))).to_be_visible(timeout=30000)


class TestCloudHealthLine:
    """Whether the clouds are still answering, said at the foot of the panel.

    The figure is not news the app waits to be asked for: the server pings
    every account on a schedule of its own, and this line is where the answer
    is read. What these tests are about is the line telling the truth on both
    sides of a cloud going dark — and the panel behind it naming which one, and
    holding the schedule.

    Own server and own vault per test, because one of them switches a cloud off
    and the shared session vault is what every other test in this file stands
    on.
    """

    PASSWORD = "still-answering-passphrase"

    def new_vault(self, page, base):
        page.goto(base)
        page.wait_for_selector("text=Create your vault", timeout=20000)
        boxes = page.locator('input[autocomplete="new-password"]')
        boxes.nth(0).fill(self.PASSWORD)
        boxes.nth(1).fill(self.PASSWORD)
        page.get_by_text("▶ Create vault").click()
        open_accounts(page)

    def connect(self, base, tmp_path, count, prefix):
        """Wire up `count` local folders behind the open app, and hand back the
        paths so a test can switch one off."""
        session = requests.Session()
        headers = {"Origin": base}
        r = session.post(f"{base}/api/vault/unlock", json={"password": self.PASSWORD},
                         headers=headers, timeout=60)
        assert r.status_code == 200, r.text

        paths = []
        for i in range(count):
            path = tmp_path / f"{prefix}-clouds" / f"cloud-{i}" / "store"
            path.mkdir(parents=True, exist_ok=True)
            r = session.post(f"{base}/api/providers",
                             json={"kind": "local", "name": f"{prefix}-{i}",
                                   "options": {"path": str(path)}},
                             headers=headers, timeout=60)
            assert r.status_code == 201, r.text
            paths.append(path)
        return session, paths

    @staticmethod
    def switch_off(path):
        """Make a connected folder unreachable, the way a NAS that has been
        switched off is: the account is still configured and there is nothing
        usable at the end of the path.

        The folder is moved aside and a plain file left in its place, rather
        than deleted — a rename is one atomic step, and the vault is pushing
        its index backup into that folder in the background while this runs.
        """
        os.rename(path, str(path) + "-moved")
        with open(path, "w") as blocked:
            blocked.write("not a folder")

    def test_the_line_says_every_cloud_is_answering(self, page, spawn_server, tmp_path):
        base = spawn_server("health-ok")
        self.new_vault(page, base)
        self.connect(base, tmp_path, 3, "ok")

        page.get_by_role("button", name="Re-check every account").click()
        expect(page.get_by_text("3 clouds healthy")).to_be_visible(timeout=30000)

    def test_a_cloud_that_stops_answering_is_counted_and_named(
        self, page, spawn_server, tmp_path
    ):
        base = spawn_server("health-dark")
        self.new_vault(page, base)
        _, paths = self.connect(base, tmp_path, 3, "dark")

        self.switch_off(paths[1])

        page.get_by_role("button", name="Re-check every account").click()
        line = page.get_by_role("button", name=re.compile(r"1 of 3 unhealthy"))
        expect(line).to_be_visible(timeout=30000)

        # And the line is a door: which cloud, and what it actually said.
        line.click()
        dialog = page.get_by_role("dialog", name="Cloud health")
        dialog.wait_for(timeout=20000)
        expect(dialog.get_by_text("dark-1", exact=True)).to_be_visible(timeout=20000)
        expect(dialog.get_by_text(re.compile(r"^Not answering"))).to_be_visible()
        expect(dialog.get_by_text("not a directory")).to_be_visible()

    def test_the_schedule_is_set_from_the_panel(self, page, spawn_server, tmp_path):
        base = spawn_server("health-schedule")
        self.new_vault(page, base)
        session, _ = self.connect(base, tmp_path, 2, "sched")

        page.get_by_role("button", name="Re-check every account").click()
        page.get_by_role("button", name=re.compile(r"2 clouds healthy")).click(timeout=30000)

        dialog = page.get_by_role("dialog", name="Cloud health")
        dialog.wait_for(timeout=20000)
        dialog.get_by_role("button", name="6 hours").click()

        # Not merely lit in the dialog — the vault is what keeps this, and the
        # server is what acts on it.
        def schedule():
            return session.get(f"{base}/api/providers/health", timeout=30).json()[
                "health"]["schedule"]

        for _ in range(30):
            if schedule()["interval_minutes"] == 360:
                break
            page.wait_for_timeout(500)
        assert schedule() == {"enabled": True, "interval_minutes": 360}

        # Off is a real answer, and the line stops promising a freshness
        # nothing is maintaining.
        dialog.get_by_role("button", name="Off").click()
        expect(page.get_by_text("checks off")).to_be_visible(timeout=20000)
        assert schedule()["enabled"] is False



class TestDownloadingAFolder:
    """A folder comes back as one zip, streamed as it is built.

    A file is fetched and handed over as a blob; a folder is not, because a
    folder can be far larger than a page can hold. The dialog mints a link and
    the browser saves straight from it, and the page never goes anywhere.
    """

    def test_a_folder_downloads_as_one_zip(self, app, tmp_path):
        import zipfile

        make_folder(app, "zipped")
        app.get_by_text("zipped").first.click()
        app.wait_for_load_state("networkidle")

        one = tmp_path / "one.txt"
        one.write_text("first file")
        upload_and_settle(app, one)
        two = tmp_path / "two.txt"
        two.write_text("second file, a little longer")
        upload_and_settle(app, two)

        app.locator('button[aria-label="Up"]').click()
        app.wait_for_selector('button[aria-label="Actions for zipped"]', timeout=20000)
        app.locator('button[aria-label="Actions for zipped"]').click()
        sheet = app.get_by_role("dialog", name="zipped")
        sheet.wait_for(timeout=20000)
        sheet.get_by_text("Download as zip", exact=True).click()

        dialog = app.get_by_role("dialog", name="Download zipped")
        dialog.wait_for(timeout=20000)
        # It says what the archive will hold before a byte of it is fetched.
        expect(dialog.get_by_text("2 files", exact=True)).to_be_visible(timeout=20000)

        before = app.url
        with app.expect_download(timeout=60000) as download:
            dialog.get_by_role("button", name=re.compile(r"Save zipped\.zip")).click()

        assert download.value.suggested_filename == "zipped.zip"
        with zipfile.ZipFile(download.value.path()) as archive:
            names = sorted(archive.namelist())
            assert names == ["zipped/one.txt", "zipped/two.txt"]
            assert archive.read("zipped/one.txt") == b"first file"
            assert archive.read("zipped/two.txt") == b"second file, a little longer"
            # Stored, not deflated: the files were compressed before they were
            # ever split, and a Pi's CPU is not where a download should go.
            assert all(info.compress_type == zipfile.ZIP_STORED for info in archive.infolist())

        # The app is still where it was: a download never navigates.
        assert app.url == before
        expect(dialog.get_by_text("Your browser is saving it", exact=False)).to_be_visible()

    def test_a_home_screen_app_hands_the_zip_to_the_browser(self, app, tmp_path):
        """Added to a home screen the vault has no browser around it, and iOS
        ignores the download attribute there: pointing the app's own window at
        the archive leaves it on a "Code.zip" screen with no way back. So a
        standalone app opens the address in the system browser instead, and
        the app itself never moves.
        """
        make_folder(app, "handed")
        app.get_by_text("handed").first.click()
        app.wait_for_load_state("networkidle")
        one = tmp_path / "handed.txt"
        one.write_text("handed over")
        upload_and_settle(app, one)
        app.locator('button[aria-label="Up"]').click()
        app.wait_for_selector('button[aria-label="Actions for handed"]', timeout=20000)

        # Safari's flag for a home-screen app, which is how the page tells.
        app.evaluate("Object.defineProperty(navigator, 'standalone', { value: true })")

        app.locator('button[aria-label="Actions for handed"]').click()
        app.get_by_role("dialog", name="handed").get_by_text("Download as zip", exact=True).click()
        dialog = app.get_by_role("dialog", name="Download handed")
        dialog.wait_for(timeout=20000)
        expect(dialog.get_by_text("1 file", exact=True)).to_be_visible(timeout=20000)

        before = app.url
        with app.context.expect_page(timeout=20000) as opened:
            dialog.get_by_role("button", name=re.compile(r"Save handed\.zip")).click()
        # The new window is pointed at an attachment, so it never commits a
        # URL of its own; that it opened at all is the handoff.
        popup = opened.value
        popup.close()

        # The app is exactly where it was, and says where the download went.
        assert app.url == before
        expect(dialog.get_by_text("Handed to your browser", exact=False)).to_be_visible()

    def test_an_empty_folder_is_refused_rather_than_zipped(self, app):
        make_folder(app, "zip-empty")
        app.locator('button[aria-label="Actions for zip-empty"]').click()
        sheet = app.get_by_role("dialog", name="zip-empty")
        sheet.wait_for(timeout=20000)
        sheet.get_by_text("Download as zip", exact=True).click()

        dialog = app.get_by_role("dialog", name="Download zip-empty")
        dialog.wait_for(timeout=20000)
        expect(dialog.get_by_text("holds no files", exact=False)).to_be_visible(timeout=20000)


class TestMovingFilesWithAMachine:
    """One button, both directions.

    There is no machine to connect in this suite — that takes an sshd, and the
    Go tests bring their own — so what is checked here is the shape: the
    dialog opens from the toolbar and from a folder's menu, offers both ways,
    and says out loud which way writes plaintext.
    """

    def test_the_toolbar_button_offers_both_directions(self, app):
        app.get_by_role("button", name="⇅ Machine").click()
        dialog = app.get_by_role("dialog", name="A machine you have a login on")
        dialog.wait_for(timeout=20000)

        expect(dialog.get_by_text("Bring files into", exact=False)).to_be_visible()
        bring = dialog.get_by_role("button", name=re.compile("BRING IN"))
        send = dialog.get_by_role("button", name=re.compile("SEND OUT"))
        expect(bring).to_have_attribute("aria-pressed", "true")
        expect(send).to_have_attribute("aria-pressed", "false")
        # The direction that writes plaintext says so where it is chosen.
        expect(send).to_contain_text("in the clear")

        send.click()
        expect(dialog.get_by_text("Send files from", exact=False)).to_be_visible()
        expect(send).to_have_attribute("aria-pressed", "true")
        expect(dialog.get_by_text("No machines yet", exact=False)).to_be_visible()

        app.keyboard.press("Escape")
        expect(dialog).to_have_count(0)

    def test_a_folder_menu_opens_the_dialog_ready_to_send(self, app):
        make_folder(app, "outbound")
        app.locator('button[aria-label="Actions for outbound"]').click()
        sheet = app.get_by_role("dialog", name="outbound")
        sheet.wait_for(timeout=20000)
        sheet.get_by_text("Send to a machine", exact=True).click()

        dialog = app.get_by_role("dialog", name="A machine you have a login on")
        dialog.wait_for(timeout=20000)
        expect(dialog.get_by_text("Send files from /outbound", exact=False)).to_be_visible()
        expect(dialog.get_by_role("button", name=re.compile("SEND OUT"))).to_have_attribute("aria-pressed", "true")
        app.keyboard.press("Escape")
