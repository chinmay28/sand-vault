# Sand Harbor

**Your files, kept safe in a harbor of clouds you don't have to trust.**

## Why sand

A grain of sand is small and means nothing on its own. Yet sand, refined into
silicon, is what every chip, every phone and every computer is made of: billions
of meaningless grains, arranged, become the machine.

Sand Harbor treats your files the same way. Each file is ground into grains:
small parts, compressed, encrypted and erasure-coded, each one noise on its own.
The grains are scattered across the clouds and disks you already have, each
piece of a file spread over different accounts. No provider holds anything it
can read, and losing one account loses no file. When you open the file, enough grains
are gathered back to make the whole again.

The harbor is where the grains are kept and where the apps are moored. It is
named after Sand Harbor on Lake Tahoe's Nevada shore, a cove of clear water
sheltered by granite boulders, whose beaches are that same granite worn down
into sand.

## The apps in the harbor

Sand Harbor is a self-hosted home for your files and media: a small family of
apps around one encrypted store.

| Component | What it is | Status |
|---|---|---|
| **SAND Vault** | The vault. It compresses every file, erasure-codes it into *n* parts, encrypts them, and puts each part on a **different** cloud account. Any *m* parts rebuild the file; fewer are noise. Ships as the `sand` binary: CLI, web UI, HTTP API and WebDAV share. | Available |
| **SAND TV** | A Fire TV app that plays a vault folder of films, unlocked on the TV with a four-character code. | Available |
| SAND Gallery, SAND Pod | Pictures and music, on the same vault. | Ideas |

The key that turns the grains back into files never leaves your machine. You
choose how many grains a file becomes (*n*) and how many bring it back (*m*):
the default is **2-of-3**, and codes range up to 20-of-30.

```
            web UI · CLI · SAND TV · WebDAV
                          │
                ┌─────────▼─────────┐
                │   sand (server)   │  index, keys and plaintext live only here
                └─────────┬─────────┘
           compress → erasure-code → encrypt
          ┌───────────┬───┴───────┬───────────┐
          ▼           ▼           ▼           ▼
       Drive        S3/R2      WebDAV     USB disk …   one part per account
```

## Quick start

**Linux (Ubuntu, Debian, Raspberry Pi).** One command installs SAND Vault as a
hardened systemd service on `http://<host>:8123`, and re-running it upgrades
in place, with a vault snapshot and automatic rollback:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand-vault/main/scripts/quickstart.sh | sudo bash
```

That builds from source. On a 64-bit Raspberry Pi (linux/arm64), add
`SAND_INSTALL=release` after `sudo` to install the prebuilt binary instead.
Then create the vault right away (`sudo -u sand …`; see the guide).

> The service listens on every interface by default. Put TLS in front of it,
> or set `HOST=127.0.0.1`. See [Installation](docs/guide/installation.md) and
> [Security](docs/security.md).

**From source** (Go 1.25+ and Node 18+):

```bash
make build
./sand vault init                                    # the password protects the index and credentials
./sand remote add local --name usb  --set path=/media/usb/sand
./sand remote add s3    --name r2   --set bucket=… --set endpoint=… \
                                    --set access_key_id=… --set secret_access_key=…
./sand remote add webdav --name nas --set url=… --set username=… --set password=…
./sand put ~/Documents/passport.pdf                  # or a whole folder
./sand ls
./sand get /passport.pdf -o restored.pdf
./sand serve --port 8123                             # web UI on http://127.0.0.1:8123
```

`./sand remote kinds` lists all 19 backends: Google Drive, OneDrive, Dropbox, Box,
Proton Drive, Zoho WorkDrive, S3-compatible, WebDAV/Nextcloud, SFTP, MEGA,
Filen, Internxt, Drime, Jottacloud, Sync.com, Tresorit, Icedrive and local
folders. The OAuth ones are quickest from the browser, via **Connect a cloud**.
Then [connect your accounts](docs/guide/accounts.md).

## What you get

- **Erasure-coded, encrypted storage** across accounts you already have, with
  per-upload or per-folder [m-of-n and placement](docs/guide/storing-files.md).
- **A browser file manager** with previews, film details, folder pictures,
  duplicates, corrupt-file checks, checksums and snapshots: see the
  [web UI](docs/guide/web-ui.md) and [folder tools](docs/guide/folder-tools.md).
- **Streaming** to VLC, [SAND TV](docs/guide/sand-tv.md), or any player that
  mounts the vault as a [WebDAV drive](docs/guide/streaming.md).
- **Self-maintenance**: scheduled checks and repairs, replication, stray and
  lost-part recovery, and [git repository mirrors](docs/guide/repositories.md).
  See [maintenance](docs/guide/maintenance.md).
- **Recovery** with your password alone: every account carries an encrypted
  copy of the index, and a recovery kit covers the worst case. See
  [recovery](docs/guide/recovery.md).
- **Sub vaults** with their own passwords, paced password changes, and one
  vault opened on several machines: see [vault management](docs/guide/vault-management.md).
- **Sandy**, an assistant that answers questions about your vault: see
  [Sandy](docs/guide/sandy.md).

## Project structure

```
cmd/sand/          the `sand` CLI: one file per command group
internal/
  vault/           control plane: encrypted index, accounts, placement, repair, recovery
  archive/ compress/ splitter/ crypto/ sandfile/
                   the pipeline: zstd → erasure code → AES-256-GCM → part files
  provider/        the 19 storage backends, plus OAuth sign-in
  server/          HTTP API, sessions and the embedded web UI (server/dist)
  davfs/           WebDAV view of an unlocked vault
  tvaccess/ tvdiscover/   SAND TV pairing codes and LAN discovery
  snapshot/ checksums/ filecheck/ movie/ thumb/ git/ sftp/ assistant/ tuning/ version/
web/               React web UI (built into internal/server/dist)
apps/firetv/       SAND TV, the Fire TV app (Kotlin/Gradle)
scripts/           installer, release build, deploy helpers, nginx template
tests/             end-to-end tests (pytest: CLI, API, browser)
docs/              everything else, starting at docs/README.md
```

The full tree with each package's role is in the [developer guide](docs/developer-guide.md#project-structure).

## Documentation

| Start here | Go deeper |
|---|---|
| [Installation](docs/guide/installation.md) · [Deployment](docs/guide/deployment.md) | [Architecture](docs/architecture.md) |
| [User guide](docs/README.md#user-guide) | [Component designs](docs/README.md#design) |
| [CLI reference](docs/reference/cli.md) · [HTTP API](docs/reference/api.md) | [Design choices](docs/design-choices.md) |
| [Configuration](docs/reference/configuration.md) | [Security model](docs/security.md) |
| [Changelog](CHANGELOG.md) | [Developer guide](docs/developer-guide.md) |

## License and contributing

Sand Harbor is free software under the **GNU AGPL v3.0** (`AGPL-3.0-only`); see
[LICENSE](LICENSE). If you run a *modified* server that other people use over a
network, you must offer them its source (AGPL §13). Running an unmodified build
for yourself carries no such obligation.

Contributions are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). Every commit
needs a DCO sign-off (`git commit -s`), which accepts the [CLA](CLA.md).
