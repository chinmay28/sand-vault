# Developer guide

How to build SAND Vault (the `sand` binary) and SAND TV (the Fire TV app),
run them while you work on them, test them, version and release them, and find
your way around the repository. For what the system is and why it is shaped
this way, read [architecture.md](architecture.md) and
[design-choices.md](design-choices.md) first.

## Prerequisites

| Tool | Version | Needed for |
|---|---|---|
| Go | 1.25 or newer (`go.mod` says `go 1.25.0`) | the binary, Go tests |
| Node.js | 18 or newer (CI uses 22) | the web client, `scripts/version.mjs`, `make icons` |
| git | any, with **full history** | the version number (see [Versioning](#versioning)) |
| Python 3 + pip | for `tests/` | the end-to-end suite (pytest, Playwright, requests) |
| JDK 17 + Android SDK | only for `apps/firetv` | SAND TV |

There is no CGO, so a plain Go toolchain is all the binary needs. The
committed web build means `go build ./cmd/sand` works from a bare checkout
with no Node at all; you only need Node to change the web client or to stamp a
real version.

## Building

All targets are in the [`Makefile`](../Makefile).

| Target | What it does |
|---|---|
| `make build` | `build-web`, then `build-go` |
| `make build-web` | `cd web && npm ci && npm run build`. Vite writes into `internal/server/dist/` |
| `make build-go` | `CGO_ENABLED=0 go build -trimpath` of `./cmd/sand`, with `-ldflags "-s -w -X …/internal/version.Patch=… -X …/internal/version.Date=…"`. Output is `sand`, or `sand.exe` on Windows |
| `make version` | Prints the version this tree would build as (`node scripts/version.mjs`) |
| `make icons` | Redraws the home-screen PNGs in `web/public/` from `web/public/icon.svg` |
| `make icons-check` | Fails if a committed PNG no longer matches `icon.svg` |
| `make release` | `build-web`, then `scripts/build-release.sh` (all five platforms into `dist/`) |
| `make clean` | Removes `sand`, `sand.exe`, coverage files, `web/node_modules`, `web/dist`. Leaves `internal/server/dist` alone on purpose (see below) |
| `make dev` | `build-go`, then `./sand serve --port 8123` |
| `make test`, `test-cover`, `test-e2e`, `test-e2e-slow`, `test-all`, `test-deps` | See [Tests](#tests) |

`make build-go` takes about 20 seconds from cold on a 4-core machine. A bare
`go build ./cmd/sand` also works; it just stamps nothing, so the binary
reports patch `0` (`sand version` prints `SAND Vault v2026.11.0`).

## The development loop

Run the Go server and Vite's dev server side by side:

```bash
# Terminal 1: the Go server, against a throwaway vault
make build-go
./sand serve --port 8123 --vault /tmp/dev-vault.sand

# Terminal 2: the web client with hot reload
cd web && npm ci && npm run dev     # http://localhost:5173
```

Vite proxies `/api` to `http://localhost:8123` (`web/vite.config.js`). Only
`/api` is proxied, so the pages and routes the Go server serves itself
(`/tv/`, `/stream/…`, `/dav`) have to be opened on port 8123, not 5173.

Always point a development server at a throwaway vault, with `--vault` (a
global flag on every command) or `SAND_VAULT`. The default is
`~/.sand/vault.sand`, which is your real one. For accounts, connect a couple
of **Local folder** accounts under `/tmp`: that runs the real scatter/gather
path with no credentials. `SAND_PASSWORD` saves typing the password into CLI
commands against the same vault.

The Go server serves whatever is in `internal/server/dist/`. After a web change
you want to see on port 8123 (or ship), run `make build-web` and rebuild the
binary.

### The embedded web build, and why it is committed

The Go binary embeds the built web client with `//go:embed all:dist` in
`internal/server/server.go`. The SAND TV page is a second embed,
`//go:embed all:tvui` in `internal/server/tvui.go`: plain JavaScript with no
build step, edited in place.

`internal/server/dist/` is committed, so that `go build` and
`scripts/quickstart.sh` work from a bare checkout without Node. `go:embed`
refuses a pattern that matches nothing, so deleting the directory breaks the
next `make build-go` until `make build-web` has run. That is why `make clean`
does not touch it. To get the committed copy back:
`git checkout -- internal/server/dist`.

A change under `web/src` should be committed together with the rebuilt
`internal/server/dist/`, as the existing feature commits are.

### Icons and fonts

The app's mark is drawn once, in `web/public/icon.svg`.
`scripts/make-icons.mjs` rasterises it into `apple-touch-icon.png`,
`icon-192.png`, `icon-512.png` and `icon-maskable-512.png`, using only Node's
standard library. Change the SVG, run `make icons`, and commit the PNGs. Never
edit the PNGs by hand. `make icons-check` verifies them.

The wordmark's script face (`web/fonts/wordmark-script.woff2`, a five-glyph
subset of Caveat under the OFL) is embedded into `index.html` at build time by
a Vite plugin, so the page makes no font request. `scripts/make-wordmark-font.py`
rebuilds the subset; see `web/fonts/README.md`.

## Versioning

Versions are `vYEAR.MONTH.PATCH`, a calendar version:

- **YEAR and MONTH** are the month of the commit being built: its committer
  date, in the committer's own timezone. Nobody bumps them. A rebuild of an old
  commit still reports what it shipped as.
- **PATCH** is the repository's commit count (`git rev-list --count HEAD`).
- The month is not zero-padded (`v2026.8.311`), which keeps the string valid
  semver.

There is no compatibility promise in the numbers. Breaking changes are called
out in [`CHANGELOG.md`](../CHANGELOG.md).

The parts are assembled in one place, [`scripts/version.mjs`](../scripts/version.mjs):

```bash
node scripts/version.mjs            # v2026.11.229
node scripts/version.mjs --patch    # 229
node scripts/version.mjs --date     # 2026-11
```

Two consumers read it, so they cannot disagree:

- The Go binary, through `-ldflags -X` into `internal/version.Patch` and
  `internal/version.Date`. `version.String()` is the one Go rendering: `sand
  version`, `/api/health` and the TV discovery reply all use it.
- The web bundle, through `web/build-version.js` and Vite's `define`
  (`__APP_VERSION__`, read by `web/src/version.js`).

SAND TV is stamped by the release workflow with `-PsandVersionName` (the
version) and `-PsandVersionCode` (the patch). A local build is `dev` / `1`.

**Patch 0 means an unstamped build.** That is what a bare `go build` gives, or
any build with no git. A shallow clone gives it too: `version.mjs` refuses to
report the count of a shallow history, since `--depth 1` would quietly produce
`v2026.8.1`. To build a real version, clone with full history (or
`--filter=blob:none`), or run `git fetch --unshallow`.

## Tests

```bash
make test           # go test -v -count=1 ./...
make test-cover     # the same, plus coverage.html
make test-deps      # pip install -r tests/requirements.txt; playwright install chromium
make test-e2e       # make build, then pytest -m "not slow" in tests/
make test-e2e-slow  # make build, then the whole pytest suite
make test-all       # test + test-e2e
```

**Go unit tests** sit beside the code they test (`*_test.go` in the same
package). `go test` runs a subset of `go vet`; run `go vet ./...` yourself for
the full set. Two packages exist only to be tested against:
`internal/provider/s3test` (an in-memory, versioning S3 endpoint) and
`internal/sftp/sftptest` (an in-process sshd with the SFTP subsystem).

**The end-to-end suite** in `tests/` drives the real binary. It looks for
`sand` (or `sand.exe`) at the repository root, which is why `test-e2e` builds
first.

| File | Covers |
|---|---|
| `test_cli.py` | Standalone `sand archive` / `sand restore` |
| `test_api.py` | The HTTP API with `requests`, no browser |
| `test_vault.py` | Connected-cloud mode, over the API and the CLI (`vault`, `remote`, `put`, `get`, `ls`, `check`) |
| `test_gui.py` | The React app, driven with Playwright. Skipped when the web client is not built |
| `conftest.py` | Fixtures: a server on a free port against a throwaway vault, throwaway "cloud accounts" as local folders, an S3 stand-in |

The suite stands up its accounts as local-folder providers, so it exercises the
real scatter/gather path, including a read with an account taken offline,
without credentials. It also walks every fake account asserting that no
plaintext or filename ever lands there. Tests marked `slow` (large files) are
skipped by `test-e2e`.

If Playwright cannot launch its bundled Chromium, point it at one you have:

```bash
PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chrome make test-e2e
```

**SAND TV's tests** are JVM unit tests in
`apps/firetv/app/src/test/java/com/sandvault/tv/`: container sniffing against
the samples in `src/test/resources/containers/`, playback statistics, player
and stats text, and server-address parsing.

```bash
cd apps/firetv
./gradlew testDebugUnitTest   # unit tests
./gradlew lintDebug           # Android lint
./gradlew assembleRelease     # APK in app/build/outputs/apk/release/
```

### What CI runs

| Workflow | When | What |
|---|---|---|
| [`dco.yml`](../.github/workflows/dco.yml) | Every pull request | Every non-merge commit must carry a `Signed-off-by` matching its author exactly |
| [`firetv.yml`](../.github/workflows/firetv.yml) | PRs and pushes to `main` touching `apps/firetv/**`, `internal/tvdiscover/**` or the workflow; manual | SAND TV: `testDebugUnitTest`, `lintDebug`, `assembleRelease`, a check that `libffmpegJNI.so` is in the APK for `armeabi-v7a` and `arm64-v8a`, and the APK as an artifact |
| [`release.yml`](../.github/workflows/release.yml) | Tags `v*`, pushes to `main`, manual | See [Releases](#releases) |

**CI does not run the Go tests, `go vet`, the e2e suite or
`make icons-check`.** Run `make test` and `make test-e2e` yourself before
opening a pull request.

## Releases

Releases are cut by [`release.yml`](../.github/workflows/release.yml), and the
changelog is the gate:

1. The workflow checks out full history (`fetch-depth: 0`) and computes the
   version with `scripts/version.mjs`. Patch 0 is refused. A tag whose name is
   not the version its commit builds is refused.
2. It extracts the section of `CHANGELOG.md` headed exactly `## <version>`.
   On a push to `main` with no such section, the commit is not a release and
   the job stops quietly. On a tag or a manual run, a missing section is an
   error.
3. It builds the web client, `sand-<version>-linux-arm64`, and `sand-tv.apk`
   (stamped with the version), writes `SHA256SUMS`, and publishes a GitHub
   release whose body is that changelog section. A release that already exists
   is left alone.

So to release, land a commit that adds a `## vYEAR.MONTH.PATCH` section naming
the version that commit will build (`make version` on top of `main`, plus one
for the commit itself). The APK keeps the same name every release, so
`…/releases/latest/download/sand-tv.apk` is a stable address to type into a
TV's Downloader app.

**`CHANGELOG.md` conventions:** newest first; one `## vYEAR.MONTH.PATCH`
section per release, which is the release body verbatim; a **Downloads** line
naming the assets; then `###` subsections per change. Releases before
`v2026.8` used `vMAJOR.MINOR.PATCH` and keep their original numbers.

**Building for every platform yourself:** `make release` (or
`./scripts/build-release.sh [version]`) builds the web client and
cross-compiles `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`
and `windows/amd64` into `dist/`, plus `SHA256SUMS`. It warns when the patch is
0. The published release carries only the linux-arm64 binary and the APK.

Installing the result is covered in [guide/installation.md](guide/installation.md)
and [guide/deployment.md](guide/deployment.md).

## Project structure

```
SandHarbor/
├── cmd/
│   └── sand/                 the `sand` CLI and server entry point (cobra): serve, vault,
│                             sub, remote, ls/find/put/get/rm/mkdir/mv/relocate/check,
│                             replicate, snapshot, checksums, automation, git, archive/restore …
├── internal/
│   ├── archive/              the per-file pipeline: compress → erasure-code → seal into
│   │                         .sand parts and back; whole-file and chunked formats, k-of-n schemes
│   ├── assistant/            Sandy: a chat model in front of tools that read the index
│   ├── checksums/            reads and writes the vault's checksum ledger (checksums.gz)
│   ├── compress/             the zstd step: each chunk compressed before it is cut
│   ├── crypto/               Argon2id, AES-256-GCM, HKDF chunk and purpose keys
│   ├── davfs/                an unlocked vault as a WebDAV filesystem (/dav, /sub/<name>/)
│   ├── filecheck/            is a file's content a well-formed file of its claimed kind
│   ├── git/                  the small part of git SAND Vault needs: ls-remote, mirror, bundle
│   ├── movie/                file name → film guess, and the film database client
│   ├── provider/             the object-store interface and every backend, OAuth sign-in,
│   │   │                     usage, disk SMART, replica roots
│   │   └── s3test/           an in-memory S3 endpoint with versioning, for tests
│   ├── sandfile/             the on-disk format of a single encrypted part
│   ├── server/               the HTTP API, sessions, stream tickets, the TV routes,
│   │   │                     and the embedded web client
│   │   ├── dist/             the built web client (committed; written by `make build-web`)
│   │   └── tvui/             the SAND TV page served at /tv/ (plain JS, no build step)
│   ├── sftp/                 the SSH/SFTP client for machines you have a login on
│   │   └── sftptest/         an in-process sshd serving SFTP, for tests
│   ├── snapshot/             a folder packed into one sealed, split archive
│   ├── splitter/             the erasure coder (systematic Cauchy Reed–Solomon over GF(2^8))
│   │                         and the legacy split/XOR used by formats 1–3
│   ├── thumb/                normalizes browser-made previews into JPEGs the vault stores
│   ├── tuning/               sizes SAND Vault to the machine it runs on
│   ├── tvaccess/             hands a television the key to one folder (pairing codes, tokens)
│   ├── tvdiscover/           lets a TV on the same network find the server over UDP
│   ├── vault/                the control plane: encrypted index, accounts, placement,
│   │                         scatter/gather, backup, recovery, sub vaults, maintenance
│   └── version/              the application version, stamped at link time
├── web/                      the React web client (Vite)
│   ├── src/                  App.jsx, index.jsx, and modules: api.js (HTTP client),
│   │   │                     upload.js, thumbs.js, stream.js, navigation.js, view.js,
│   │   │                     theme.js, oauth.js, version.js, …
│   │   └── components/       one file per screen or dialog: LockScreen, FileBrowser,
│   │                         AccountsPanel, ConnectCloud, PreviewModal, FilmDetails, ui.jsx …
│   ├── public/               icon.svg, the PNGs drawn from it, manifest.json, developer badge
│   ├── fonts/                the wordmark's script face and its licence
│   ├── build-version.js      feeds scripts/version.mjs into the bundle
│   └── vite.config.js        build into internal/server/dist; dev proxy for /api
├── apps/
│   └── firetv/               SAND TV, a Kotlin Android app (Gradle, AGP 8.7, minSdk 22)
│       └── app/src/
│           ├── main/java/com/sandvault/tv/   MainActivity (WebView around /tv/), Discovery,
│           │                                 PlayerActivity (Media3), PlaybackStats, StatsText …
│           ├── main/assets/                  the connect screen shown before a server is found
│           ├── main/res/                     launcher icon, banner, theme, network security config
│           └── test/                         JVM unit tests and sample media containers
├── scripts/
│   ├── quickstart.sh         one-command Linux install / upgrade / rollback / uninstall (systemd)
│   ├── deploy-linux.sh       install an already-built binary as a systemd service
│   ├── deploy-windows.ps1    install on Windows as a background service
│   ├── allow-local-path.sh   let the sandboxed service write to a local folder
│   ├── enable-disk-health.sh install the SMART scanner the disk health check reads
│   ├── nginx-sand.conf       reverse proxy with TLS
│   ├── build-release.sh      cross-compile release binaries for five platforms
│   ├── version.mjs           the one place the version is assembled
│   ├── make-icons.mjs        draw the home-screen PNGs from icon.svg
│   └── make-wordmark-font.py subset the wordmark's script face
├── tests/                    pytest end-to-end suite: CLI, API, vault flow, browser
├── docs/                     this documentation
├── CHANGELOG.md  CONTRIBUTING.md  CLA.md  LICENSE (AGPL-3.0-only)  Makefile
└── go.mod  go.sum
```

The layering rule the tree exists to keep: **`internal/provider` never sees
plaintext**. By the time bytes reach a backend they have been compressed,
split and sealed. See [architecture.md](architecture.md) for the layers and the
package map.

## Code conventions

What the code itself does consistently:

- **Small packages with a doc comment that says why.** Each `internal/`
  package opens with a `// Package x …` comment stating its one job and its
  reasoning. Comments throughout explain the reason for a choice, not the
  mechanics.
- **Tests beside the code**, in the same package. Golden values pin anything
  whose output is a format (`internal/crypto/hkdf_test.go`). Fakes are real
  servers run in-process (`s3test`, `sftptest`) rather than mocks.
- **Standard library first.** Every cloud backend is hand-written on
  `net/http`; there are no cloud SDKs. The direct dependencies are
  `google/uuid`, `klauspost/compress`, `spf13/cobra` and `golang.org/x/*`,
  plus `pkg/sftp`. A new module needs a good reason.
- **No CGO.** `CGO_ENABLED=0` must keep producing a static binary.
- **No network I/O under the vault lock.** Snapshot under `mu`, release, do
  the I/O, re-take the lock to commit, and re-check that the vault is still
  unlocked.
- **Optional capabilities are interfaces the caller checks for**
  (`UsageReporter`, `Versioner`, `PingBudgeter`, …) rather than methods every
  backend must stub.
- **`gofmt`-clean.** There is no other linter.
- **The web client is plain JSX** with inline styles (no stylesheet, no
  TypeScript) and loads nothing from a third party: fonts are embedded at
  build time.
- **Commit subjects** follow `type(scope): summary (#PR)`, e.g.
  `feat(tv): …`, `fix(stream): …`, `docs: …`.

## Contributing

Read [`CONTRIBUTING.md`](../CONTRIBUTING.md). In short:

- Sand Harbor is AGPL-3.0-only, and contributing means accepting the
  [CLA](../CLA.md).
- **Every commit must be signed off**, with a `Signed-off-by` trailer that
  matches the commit's author exactly. `dco.yml` fails the pull request
  otherwise, and the PR cannot merge.

  ```bash
  git commit -s
  git config format.signOff true      # sign off every commit in this clone
  ```

  To fix commits already made without it, rewrite them rather than adding
  another commit:

  ```bash
  git rebase --signoff origin/main
  git push --force-with-lease
  ```

  An amend or rebase keeps the sign-off only if it also signs off, so check
  `git log -1 --format=%B` afterwards.
- Before opening a PR: `make build`, `make test`, `make test-e2e` (CI runs
  none of these), and `make icons-check` if you touched `icon.svg`.
- Changes to cryptography, key handling, shard placement or the vault format
  are read closely, and §5 of the CLA applies to them.
