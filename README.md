# SAND — Secure Archival Network Distribution

**A file browser over storage you don't fully trust.**

Connect the cloud accounts you already have. SAND compresses every file you add,
splits it into three parts, encrypts them, and puts each part on a **different**
account. Any two parts rebuild the original; any one on its own is noise. Open a
file in the browser and SAND fetches the parts back, reassembles them in memory,
and shows you the file.

No single provider ever holds your data — only a fragment that means nothing
without a fragment held by someone else, plus a key that never leaves your
machine.

Ships as a **single static Go binary** with a CLI and an embedded web UI.

```
┌ SAND ─────────────────────────────────────────────────────────────────┐
│ CONNECTED CLOUDS  │  ▣ / photos                        [+ Folder] [↑] │
│ ● drive-personal  ├───────────────────────────────────────────────────┤
│   gdrive · 41 pts │  NAME              SIZE      MODIFIED      PARTS  │
│ ● r2-cold         │  📁 2024                                          │
│   s3 · 41 pts     │  🖼 hike.jpg       4.2 MB    Aug 3 10:22   ①②③   │
│ ● nas-backup      │  📕 lease.pdf      880 KB    Jul 29 16:04  ①②③   │
│   webdav · 41 pts │  🎬 clip.mp4       88 MB     Jul 12 09:41  ①②③   │
└───────────────────┴───────────────────────────────────────────────────┘
   each ①②③ is coloured by the account holding that part
```

---

## Quick start on Linux (Ubuntu / Raspberry Pi)

Install SAND as a hardened **systemd service** with one command:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand/main/scripts/quickstart.sh | sudo bash
```

(or, from a checkout: `sudo ./scripts/quickstart.sh`)

It installs Node 22 and Go if needed (both build-time only), creates a dedicated
`sand` system user, compiles the web client and the static server binary, and
runs it under systemd on `http://127.0.0.1:8080`.

**Or skip the build entirely** and install the prebuilt binary from the latest
[release](https://github.com/chinmay28/sand/releases) — no Node, no Go, no
source tree, seconds instead of minutes on a Raspberry Pi:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand/main/scripts/quickstart.sh \
  | sudo SAND_INSTALL=release bash
```

The download's checksum is verified before anything is swapped in, and
`SAND_RELEASE=v2.0.42` pins a specific release instead of the latest. Releases
publish **`linux/amd64`** and **`linux/arm64`**; anything else builds from
source (the default), which works everywhere. Both modes install the same thing
— one static binary with the web client embedded, under the same unit and the
same data directory — so you can switch between them by re-running with a
different `SAND_INSTALL`.

**Re-run it any time to upgrade — installs and upgrades are non-disruptive and
never lose data:**

- The vault lives at a stable path **outside** the source tree
  (`/var/lib/sand/`), so rebuilding or pulling can't clobber it.
- Each upgrade quiesces the service, **snapshots the vault** to a timestamped
  backup, then swaps code in. The new build compiles while the old version
  keeps serving, so a failed build leaves the running app untouched.
- After restart it polls `/api/health`; if the new version is unhealthy it
  **rolls back** — to the previous commit when it built from source, to the
  previous binary when it installed a release — and **restores the pre-upgrade
  vault snapshot**.

Override defaults with env vars (`PORT`, `HOST`, `SAND_INSTALL`, `SAND_REF`,
`SAND_RELEASE`, `SAND_DATA_DIR`, `SAND_PREFIX`, `SAND_USER`, …). Manage it with
`systemctl status sand` and `journalctl -u sand -f`.

> **`HOST` defaults to `127.0.0.1`, not `0.0.0.0`.** SAND's server is the one
> component that ever holds plaintext — it rebuilds decrypted files in memory
> and takes your vault password over the wire. Put TLS in front (Tailscale
> Serve, or `scripts/nginx-sand.conf`) before widening it.

---

## Quick start from source

```bash
# 1. Build (requires Go 1.22+ and Node.js 18+)
make build

# 2. Create your vault — this password protects the file index and your
#    cloud credentials. There is no recovery if you lose it.
./sand vault init

# 3. Connect at least two accounts (three for full redundancy)
./sand remote add local  --name usb-drive --set path=/media/usb/sand
./sand remote add s3     --name r2-cold \
    --set bucket=sand-shards \
    --set endpoint=https://<account>.r2.cloudflarestorage.com \
    --set access_key_id=… --set secret_access_key=…
./sand remote add webdav --name nextcloud \
    --set url=https://cloud.example.com/remote.php/dav/files/alice \
    --set username=alice --set password=…

# 4. Store and retrieve
./sand put ~/Documents/passport.pdf
./sand ls
#   passport.pdf  2.1 MB  Aug 12 09:14  p1:usb-drive p2:r2-cold p3:nextcloud
./sand get /passport.pdf -o ./restored.pdf

# 5. Or use the browser
./sand serve --port 8080
# → http://127.0.0.1:8080
```

Run `./sand remote kinds` to see every backend and the settings it needs.

---

## How It Works

### Storing

```
file → zstd compress → split in half (p1, p2) → p3 = p1 XOR p2
     → encrypt each part with AES-256-GCM (Argon2id-derived key)
     → PUT each part to a different account, in parallel
```

An upload commits once **at least two** parts have landed — the minimum that can
still be rebuilt. If fewer than two succeed, the ones that did land are deleted
rather than left as orphans.

### Retrieving

```
request all three parts at once → the first two to arrive win, the rest cancelled
     → decrypt → XOR-reconstruct → decompress → verify SHA-256 → your file
```

Reads are a race, so a slow or offline account costs you nothing — it just loses.

### Reconstruction

| p1 | p2 | p3 | Method |
|:--:|:--:|:--:|:---|
| ✓ | ✓ | — | concat(p1, p2) |
| ✓ | — | ✓ | p2 = p1 ⊕ p3, then concat |
| — | ✓ | ✓ | p1 = p2 ⊕ p3, then concat |
| at most one | | | unrecoverable |

---

## Supported Backends

| Kind | Works with | What you need |
|---|---|---|
| `local` | Any directory — external disk, NAS mount, sync folder | A path |
| `s3` | Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO | Bucket, keys, endpoint for non-AWS |
| `webdav` | Nextcloud, ownCloud, Box, Koofr, Fastmail | URL, username, app password |
| `gdrive` | Google Drive | OAuth client ID/secret + refresh token |
| `dropbox` | Dropbox | App key/secret + refresh token (or an access token) |

All five are built on the standard library — no cloud SDKs, no CGO, still one
static binary.

> **Google Drive and Dropbox** take a refresh token you obtain yourself; SAND
> does not ship registered OAuth app credentials. The connect dialog links to
> each provider's documentation.

---

## Placement Policy

Where parts go is a security decision: any two parts plus the key rebuild the
file, so two parts on one account means that account could rebuild it.

**`strict`** (default) — one part per account, never two on the same one.

- 3+ accounts → all three parts placed separately. Full redundancy, and no
  single provider can reconstruct anything.
- 2 accounts → parts 1 and 2 only. Recoverable and confidential, but no spare.
- 1 account → refused.

**`redundant`** — always store all three, doubling up when there are fewer than
three accounts. Survives an account going dark, but the doubled-up account holds
enough to rebuild.

```bash
./sand vault policy              # show
./sand vault policy redundant    # change (applies to new uploads)
```

---

## CLI Reference

### Vault

```
sand vault init [--policy strict|redundant]   Create the vault
sand vault status                             What's stored, where, how much
sand vault passwd                             Change password (nothing re-uploads)
sand vault policy [strict|redundant]          Show or set placement policy
```

### Accounts

```
sand remote kinds                             List backends and their settings
sand remote add <kind> --name N --set k=v …   Connect an account (pings first)
sand remote list                              Status, parts held, bytes stored
sand remote test <name-or-id>                 Re-check reachability
sand remote remove <name-or-id> [--force]     Disconnect
```

### Files

```
sand ls [path] [-l]                24 B  Aug 12 09:14  p1:acct-a p2:acct-b p3:acct-c
sand put <file>... [--path /dir] [--overwrite]
sand get <path-or-id> [-o out]     Rebuild and decrypt
sand mkdir <path>
sand mv <path> <new-path>          Index only — parts never move
sand rm <path> [-r]                Erases every part from every account
sand check [path] [--all]          Verify parts are still there; non-zero if not
```

`sand check --all` exits non-zero when anything is degraded or unrecoverable,
which makes it a reasonable cron job.

### Server

```
sand serve [--port 8080] [--bind 127.0.0.1] [--idle-timeout 30m] [--vault PATH]
```

### Standalone mode (no vault, no accounts)

The original SAND workflow, kept because it needs no state at all:

```bash
sand archive report.pdf photos.zip --output-dir ./out
# → media1.zip  media2.zip  media3.zip   (store each somewhere different)

sand restore --parts report.pdf.media1,report.pdf.media3 --output-dir .
# → report.pdf, byte-identical
```

### Passwords in scripts

Every command prompts without echo. Set `SAND_PASSWORD` to run unattended, or
pipe the password on stdin.

---

## Web UI

`sand serve` puts a file browser at `http://127.0.0.1:8080`:

- **Lock screen** — nothing can be listed or fetched until the vault is open
- **Sidebar** — every connected account, live status, how many parts it holds,
  quota where the provider reports it
- **Connect dialog** — generated from each backend's own field spec, so new
  backends appear without frontend changes
- **Browser** — folders, breadcrumbs, drag-and-drop upload with progress
- **Part badges** — `①②③` coloured per account; click for a live per-part
  health read-out
- **Preview** — images, video, audio, PDF and text render inline, rebuilt on
  demand; anything else downloads
- **Auto-lock** — the vault re-locks after the idle timeout

The UI loads no external fonts, scripts or styles. Opening your vault makes zero
third-party requests.

---

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/health` | Liveness |
| GET | `/api/vault` | Initialized? Unlocked? Stats |
| POST | `/api/vault/init` · `/unlock` · `/lock` | Vault lifecycle |
| POST | `/api/vault/password` · `/policy` | Change password / placement |
| GET | `/api/providers/specs` | Backend descriptions for the connect form |
| GET · POST | `/api/providers` | List / connect accounts |
| POST | `/api/providers/{id}/test` | Re-check an account |
| DELETE | `/api/providers/{id}` | Disconnect (`?force=1`) |
| GET | `/api/files?path=` | List a folder |
| POST | `/api/files` | Upload (`files[]`, `path`, `overwrite`) |
| GET | `/api/files/{id}/content` | Rebuild and stream (`?download=1`) |
| GET | `/api/files/{id}/health` | Per-part reachability |
| POST | `/api/files/{id}/move` | Rename / move |
| DELETE | `/api/files/{id}` | Erase every part |
| POST · DELETE | `/api/folders` | Create / delete folders |
| POST | `/api/archive` · `/api/restore` | Standalone mode |

Uploads report per-file results, so one failure in a batch doesn't sink the rest:

```json
{
  "stored": 1,
  "results": [
    { "name": "a.pdf", "ok": true, "file": { "…": "…" } },
    { "name": "b.mov", "ok": false, "error": "stored only 1 of 3 parts…" }
  ]
}
```

Sessions use a `SameSite=Strict` cookie and every write is `Origin`-checked, so
another site in your browser can't drive the local API.

---

## Security

| Threat | Mitigation |
|---|---|
| One cloud account compromised | Attacker holds one encrypted part — useless. Guaranteed by `strict` placement. |
| Two accounts compromised | Still needs the vault file *and* your password |
| Vault file stolen | Every section AES-256-GCM sealed under an Argon2id key — yields neither credentials nor filenames |
| Provider tampers with a part | GCM tag fails; the other two rebuild the file |
| Part swapping between files | Header bound as GCM associated data |
| Silent bit rot | Whole-file SHA-256 verified on every rebuild |
| An account disappears | Any two parts suffice; `sand check --all` finds damage early |
| Another site in your browser | `SameSite=Strict` + `Origin` checks |
| Stored HTML/SVG executing in the app | Forced to `attachment`, `nosniff`, restrictive CSP |
| Plaintext cached by a proxy | `Cache-Control: private, no-store` |
| Walking away from the machine | Idle timeout re-locks the vault |
| Metadata leaking to a provider | Object keys derived only from a random ID |

**Two keys, not one.** File parts are encrypted under a random 256-bit data key,
which is itself wrapped by your password. So changing your password re-wraps 32
bytes instead of re-uploading everything, and part encryption doesn't inherit a
weak password.

### What SAND does not protect against

- **A compromised machine.** SAND sees plaintext and holds keys while unlocked.
- **Weak passwords.** Argon2id slows a guess; it doesn't fix `hunter2`.
- **Two providers *and* your password** — that's the recovery path, and also the
  attack. Use genuinely independent accounts; two buckets in one AWS account is
  not distribution.
- **Losing your password.** There is no recovery.

⚠️ **`--bind` off loopback** sends your password and rebuilt plaintext over the
network in the clear. The server warns you. Put TLS in front of it
(`scripts/nginx-sand.conf`).

---

## Building from Source

| Tool | Minimum | Install |
|---|---|---|
| Go | 1.22 | https://go.dev/dl/ |
| Node.js | 18 | https://nodejs.org |

```bash
make build        # frontend + binary
make build-web    # frontend only → internal/server/dist/
make build-go     # binary only
make version      # print the version this tree would build as
make release      # cross-compile all platforms → dist/
```

Output is `sand` on Linux/macOS, `sand.exe` on Windows.

### Versioning

`vMAJOR.MINOR.PATCH`, where **the patch number is the repository's commit
count** — every commit is a patch release, so `v2.0.311` is the 311th commit on
the 2.0 line.

- `MAJOR`/`MINOR` are source constants in
  [`internal/version/version.go`](./internal/version/version.go). Bump them by
  hand.
- `PATCH` only exists at build time, so it is stamped in: `-ldflags -X` for the
  Go binary, Vite's `define` for the web bundle. Both read
  [`scripts/version.mjs`](./scripts/version.mjs), so the header, `sand version`
  and `/api/health` can never disagree.

A patch of `0` means an unstamped build — no git, or a **shallow clone**, which
`version.mjs` detects and refuses to guess around rather than shipping a build
that quietly calls itself `v2.0.1`. Anything building a release needs the full
commit graph (`fetch-depth: 0`, or `--filter=blob:none` rather than
`--depth 1`).

---

## Testing

```bash
make test          # Go unit tests
make test-cover    # …with an HTML coverage report
make test-e2e      # Python e2e: CLI, HTTP API, vault flow, browser
make test-all      # everything
```

The e2e suite stands up throwaway "cloud accounts" as local-folder providers, so
it exercises the real scatter/gather path — including reading a file back with an
account deliberately taken offline — without needing credentials in CI. It also
walks every fake account asserting no plaintext or filename ever appears there.

If Playwright can't launch its bundled Chromium, point it at one you have:

```bash
PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chrome make test-e2e
```

---

## Deployment

### Linux — systemd

Use [`scripts/quickstart.sh`](#quick-start-on-linux-ubuntu--raspberry-pi): it
fetches or builds, installs, upgrades in place, and rolls back a bad upgrade.

If you already have a binary and only want it running,
`scripts/deploy-linux.sh` is the small path:

```bash
make build
sudo ./scripts/deploy-linux.sh ./sand 8080 127.0.0.1
systemctl status sand && journalctl -u sand -f
```

Both write the same unit and use the same data directory
(`/var/lib/sand`), so they can be used interchangeably on one host.

### Windows — NSSM

```powershell
winget install nssm
make build
.\scripts\deploy-windows.ps1 -Binary .\sand.exe -Port 8080 -Bind 127.0.0.1
Get-Service sand
```

### Reverse proxy (nginx + TLS)

```bash
sudo cp scripts/nginx-sand.conf /etc/nginx/sites-available/sand
sudo ln -s /etc/nginx/sites-available/sand /etc/nginx/sites-enabled/
sudo certbot --nginx -d sand.example.com
sudo nginx -t && sudo systemctl reload nginx
```

### Release builds

```bash
./scripts/build-release.sh 2.0.0     # dist/ with all platforms + SHA256SUMS
```

---

## Project Structure

```
sand/
├── cmd/sand/                    # CLI: serve, vault, remote, ls/put/get/rm, archive/restore
├── internal/
│   ├── archive/                 # encode.go — the in-memory pipeline both modes share
│   ├── crypto/                  # Argon2id + AES-256-GCM
│   ├── compress/                # zstd
│   ├── splitter/                # split, XOR, reconstruct
│   ├── mediafile/               # binary .media part format
│   ├── provider/                # local, s3 (SigV4), webdav, gdrive, dropbox
│   ├── vault/                   # encrypted store, manifest, placement, scatter/gather
│   └── server/                  # sessions, handlers, embedded SPA
├── web/src/                     # React file browser
│   ├── api.js  theme.js  App.jsx
│   └── components/              # LockScreen, AccountsPanel, FileBrowser, PreviewModal, ui
│   ├── public/                  # app icon + developer badge
│   └── build-version.js         # feeds the version into the bundle
├── internal/version/            # MAJOR/MINOR; PATCH stamped at link time
├── tests/                       # pytest e2e: CLI, API, vault flow, browser
├── scripts/
│   ├── quickstart.sh            # one-command systemd install / upgrade / rollback
│   ├── version.mjs              # the one place the version is assembled
│   ├── build-release.sh         # cross-compile all platforms
│   ├── deploy-linux.sh          # install an already-built binary
│   └── nginx-sand.conf          # reverse-proxy template
├── CHANGELOG.md                 # release notes, one section per tag
├── SAND_ARCHITECTURE.md         # the full design document
├── CONTRIBUTING.md              # how to contribute + the DCO sign-off
├── CLA.md                       # contributor license agreement
├── LICENSE                      # AGPL-3.0-only
└── Makefile
```

---

## Development

```bash
# Terminal 1 — Go server
make build-go && ./sand serve --port 8080

# Terminal 2 — hot-reload frontend (proxies /api/* to :8080)
cd web && npm run dev     # → http://localhost:5173
```

Use `--vault /tmp/dev-vault.sand` (or `SAND_VAULT`) while developing so you never
touch your real vault.

---

## Where Things Live

| Path | What |
|---|---|
| `~/.sand/vault.sand` | Encrypted index + cloud credentials. **Back this up.** Without it, the parts scattered across your accounts are unrecoverable. |
| `sand/<archive-id>/pN.media` | How parts appear on each account. The ID is random and reveals nothing. |

Override the vault location with `--vault` or `SAND_VAULT`.

---

## License

SAND is free software licensed under the **GNU Affero General Public License
v3.0** (`AGPL-3.0-only`). See [LICENSE](./LICENSE) for the full text.

The AGPL is a strong copyleft license: anyone who distributes SAND — or **runs a
modified version as a network service** — must make the complete corresponding
source available under the same license. Copyright in the project is held by
Chinmay Manjunath, who may also offer SAND under separate commercial terms.

> **Note for operators (AGPL §13):** if you run a modified SAND server that
> other people interact with over a network, you must offer those users the
> corresponding source of your modified version. Running an unmodified build
> for yourself — the normal case — carries no such obligation.

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](./CONTRIBUTING.md). By
contributing you agree to the [Contributor License Agreement](./CLA.md), which
lets the project be offered under both the AGPL and possible future commercial
terms. Sign off your commits with `git commit -s`.
