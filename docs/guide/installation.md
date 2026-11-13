# Installing SAND Vault

This page covers getting the `sand` binary onto a machine and creating your
first vault: the Linux quick-start script (built from source or from a
prebuilt release), building it yourself, and upgrading or removing it. For
running it as a service on other platforms or behind TLS, see
[Deployment](deployment.md).

SAND Vault is one static Go binary with the web client embedded in it. Node
and Go are needed only to *build* it; the running server needs neither.

## Linux quick start (Ubuntu, Debian, Raspberry Pi OS)

One command, run as root, installs SAND Vault as a hardened systemd service:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand-vault/main/scripts/quickstart.sh | sudo bash
```

It needs systemd, and installs anything else it is missing through `apt-get`.
In order, it:

1. installs `curl`, `ca-certificates` and `git`, plus Node 22 and Go if they
   are missing or too old (Node 18+, Go 1.25+; build-time only);
2. creates a `sand` system user with no home and no shell;
3. clones `main` into `/opt/sand/src` and builds the web client and the binary
   there, while any running version keeps serving;
4. creates `/var/lib/sand` (the vault and its backups) and, on an upgrade,
   stops the service and snapshots the vault;
5. writes `/etc/systemd/system/sand.service` and starts it on port 8123;
6. installs the hourly disk-health scanner (`sand-smart.timer`, needs
   `smartmontools`; see [Disk health](deployment.md#disk-health-smart));
7. waits up to a minute for `/api/health` to answer, and rolls back if it
   does not (see [Upgrading](#upgrading));
8. last, builds Proton's `proton-drive` client for the `protoncli` backend
   (see [Proton's client](#protons-client)).

When it finishes it prints the address, the vault path and the commands to
manage the service:

```bash
systemctl status sand
journalctl -u sand -f
```

Everything it prints is also appended to `/opt/sand/quickstart.log`. A dropped
SSH session does not stop the install, so look there for how it ended.

> **It listens on every interface by default.** `HOST` defaults to `0.0.0.0`,
> so the service is reachable from your network as soon as it starts. This
> server is the one component that ever holds plaintext: it takes your vault
> password over the wire and sends decrypted files back. On plain HTTP both are
> in the clear. Until a vault exists, `POST /api/vault/init` answers anyone who
> can reach the port, so create yours straight away. Install with
> `HOST=127.0.0.1` to keep it on loopback until TLS is in front of it
> ([nginx + TLS](deployment.md#reverse-proxy-nginx--tls) or Tailscale Serve).
> See [Security](../security.md).

### Options

Pass variables between `sudo` and `bash`:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand-vault/main/scripts/quickstart.sh \
  | sudo HOST=127.0.0.1 PORT=9000 SAND_WEBDAV=1 bash
```

The common ones:

| Variable | Default | What it does |
|---|---|---|
| `HOST` | `0.0.0.0` | Bind address |
| `PORT` | `8123` | Port |
| `SAND_WEBDAV` | `0` | `1` also serves the vault as a WebDAV share at `/dav/`, and each sub vault at `/sub/<name>/` ([Streaming](streaming.md)) |
| `SAND_INSTALL` | `source` | `release` downloads a prebuilt binary instead of building |
| `SAND_REF` | `main` | Branch, tag or commit to build (source mode) |
| `SAND_RELEASE` | `latest` | Release tag to install (release mode) |
| `SAND_DATA_DIR` | `/var/lib/sand` | Vault, backups, Proton state, SMART report |
| `SAND_LOCAL_PATHS` | | Extra folders a Local folder account may write to ([Local folders](deployment.md#local-folders-under-systemd)) |

The full list, including the toolchain, path and Proton variables, is in
[Configuration](../reference/configuration.md#installer-variables).

**Settings are remembered.** `PORT`, `HOST`, `SAND_WEBDAV`, `SAND_USER`,
`SAND_PREFIX`, `SAND_DATA_DIR` and `SAND_MOUNT_ROOTS` are read back from the
installed unit. On an upgrade, leaving one unset keeps what the service runs
with now, so the bare one-liner never moves a loopback install back onto every
interface or points the service at a different vault. Naming a *different*
user, prefix or data directory from the installed one is refused, with both
values printed, unless you add `SAND_FORCE_PATHS=1`. Even then nothing is
moved: the service is just pointed at the new path.

The prefix cannot be under `/home`, `/root` or `/run/user`. The unit sets
`ProtectHome=true`, so the service could not see its own binary there.

### Prebuilt release instead of a build

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand-vault/main/scripts/quickstart.sh \
  | sudo SAND_INSTALL=release bash
```

This skips Node, Go and the source tree. It takes seconds on a Raspberry Pi
rather than minutes. It asks the GitHub API for the release (the latest, or
`SAND_RELEASE=<tag>`), downloads `sand-<tag>-linux-<arch>` to
`/opt/sand/bin/sand`, and checks it against the release's `SHA256SUMS` before
anything is swapped in. A mismatch stops the install. A release with no
checksum entry installs with a warning.

> **Releases currently publish `linux/arm64` only.** The release workflow
> (`.github/workflows/release.yml`) attaches `sand-<tag>-linux-arm64`,
> `sand-tv.apk` and `SHA256SUMS`, and nothing else. On x86-64 a release install
> stops with "no sand-…-linux-amd64 in release"; use the default source
> install there. 32-bit ARM has no prebuilt binary either.

Both modes install the same thing under the same unit and data directory, so
you can switch by re-running with a different `SAND_INSTALL`.

### Installing from a checkout

Running the script from a clone, instead of piping it from `curl`, deploys
that clone *as it stands*, uncommitted changes included:

```bash
sudo ./scripts/quickstart.sh
```

The checkout is copied to `/opt/sand/checkout` and built there. Nothing in
your working copy is written to or chowned. The script never pulls in this
mode, and warns if the clone is behind `origin/main`. Piping from `curl` while
standing inside a clone still deploys `main`.

### Proton's client

Step 8 builds Proton's `proton-drive` command-line client and links it as
`/usr/local/bin/proton-drive`. The [`protoncli`](accounts.md#proton-drive)
backend needs it. The step runs after the service is up and healthy, so a slow
or failed build costs only that backend. It has a time limit
(`PROTON_BUILD_TIMEOUT`, 45 minutes by default) and prints progress every half
minute.

- `PROTON_CLI_URL=<url>` installs a
  [prebuilt client](https://proton.me/download/drive/cli/index.html) instead.
  On a Raspberry Pi that takes seconds rather than twenty minutes.
- `INSTALL_PROTON=never` skips the step.
- The build needs `bun`, which has no 32-bit build, so a 32-bit ARM userland
  skips this step with a message.
- The SDK's lockfiles point at Proton's own package mirror, which refuses or
  stalls from outside Proton. The installer rewrites them to
  `PROTON_NPM_REGISTRY` (default `https://registry.npmjs.org`).

Re-running the script retries a build that did not finish.

## Building from source

You need Go 1.25 or later (`go.mod` pins the toolchain, which Go fetches
itself) and Node.js 18 or later.

```bash
git clone https://github.com/chinmay28/sand-vault.git
cd sand-vault
make build          # web client, then the binary that embeds it
./sand version
```

The output is `./sand` (or `sand.exe` on Windows). It is static (`CGO_ENABLED=0`)
and can be copied to any machine of the same OS and architecture.

Clone with full history. The version's patch number is the commit count, so a
`--depth 1` clone builds a binary that reports patch `0`. `--filter=blob:none`
is fine. To cross-compile every platform (Linux amd64/arm64, macOS
amd64/arm64, Windows amd64) into `dist/` with a `SHA256SUMS`:

```bash
./scripts/build-release.sh
```

The [developer guide](../developer-guide.md) covers the rest: the dev loop,
tests and how releases are cut.

## Your first vault

The vault file holds the index of every stored file and the credentials for
every connected account. Its password is the only way to open it: **there is
no reset**.

**In the browser.** Open `http://<host>:8123`. With no vault yet, the lock
screen offers **▶ Create vault**. Then connect accounts with
**+ Connect a cloud** ([Accounts](accounts.md)).

**From the command line**, on a machine where you run `sand` yourself:

```bash
./sand vault init                      # asks for the new password twice
./sand remote kinds                    # every backend and the settings it takes

./sand remote add local  --name usb-drive --set path=/media/usb/sand
./sand remote add s3     --name r2-cold \
    --set bucket=sand-shards \
    --set endpoint=https://<account>.r2.cloudflarestorage.com \
    --set access_key_id=… --set secret_access_key=…
./sand remote add webdav --name nextcloud \
    --set url=https://cloud.example.com/remote.php/dav/files/alice \
    --set username=alice --set password=…

./sand put ~/Documents/passport.pdf    # --accounts and --scheme pick where and how
./sand put ~/Photos                    # a folder keeps its shape
./sand ls
#   passport.pdf  2.1 MB  2026-08-12 09:14  2-of-3 s1:usb-drive s2:r2-cold s3:nextcloud
./sand get /passport.pdf -o ./restored.pdf

./sand serve                           # then open http://127.0.0.1:8123
```

Connect at least two accounts, and three for the default 2-of-3 code. With
the default `strict` placement policy, no account ever holds two parts of the
same file (`sand vault init --policy redundant` changes that). See
[Storing files](storing-files.md).

**Against the service's vault.** The service runs as `sand` and its vault is
`/var/lib/sand/vault.sand`. Run CLI commands against it as that user, so the
files it creates are ones the service can read:

```bash
sudo -u sand /opt/sand/src/sand --vault /var/lib/sand/vault.sand vault init
sudo -u sand /opt/sand/src/sand --vault /var/lib/sand/vault.sand remote kinds
```

The binary is at `/opt/sand/bin/sand` after a release install, and at
`/opt/sand/checkout/sand` after a checkout install. Set `SAND_PASSWORD` to skip
the prompt in scripts. Without a terminal, the password is read as one line
from stdin.

**Back up the vault file.** It is the only map of which account holds which
part of which file. The encrypted copy of the index on every account
(`manifest.sand`) makes losing it recoverable with your password, and a
recovery kit makes it recoverable without even that ([Recovery](recovery.md)).
Keep a copy anyway.

## Upgrading

Run the same command again. Installs and upgrades never touch stored files:

- The vault lives in `/var/lib/sand`, outside anything the script rewrites.
- The new build compiles, or the new binary downloads, while the old one keeps
  serving. A failed build leaves the running service untouched.
- Before swapping anything in, the script stops the service and copies the
  vault to `/var/lib/sand/backups/vault-<timestamp>.sand`. It keeps the newest
  ten (`BACKUP_KEEP`).
- If the new version fails its health check, the script rolls back to the
  previous commit (source mode) or the previous binary (release mode),
  restores the pre-upgrade vault snapshot, and restarts.

A source install made from a checkout has no previous commit to return to.
If it fails its health check, the script stops with the snapshot path and does
not roll back.

The script stops before replacing anything if the old server outlives
systemd's `SIGKILL`. A process that survives that is stuck in the kernel on
I/O, usually a mount that has stopped answering. The script lists the mounts
that did not answer and what to do about them.

Read [`CHANGELOG.md`](../../CHANGELOG.md) before an upgrade; breaking changes
are called out there.

## Uninstalling

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand-vault/main/scripts/quickstart.sh \
  | sudo bash -s -- --uninstall
```

This removes:

- the `sand` service, its unit and its drop-ins (including Local folder grants);
- the `sand-smart` service, its timer and `smart.json`;
- the `/usr/local/bin/proton-drive` link;
- what the installer put under `/opt/sand`.

It **keeps**:

- `/var/lib/sand`: the vault, its backups and Proton's client state;
- the `sand` user;
- any folder a Local folder account wrote to;
- the checkout you ran it from, and Node, Go and git.

It prints the command that deletes the vault and the user, for you to run if
you mean it.

A custom `SAND_PREFIX`, `SAND_DATA_DIR` or `SAND_USER` is read back from the
unit; pass them only if the unit is already gone. The script refuses to delete
anything from a prefix that holds neither its marker file (`.sand-prefix`) nor
the binary the unit runs. That way a mistyped `SAND_PREFIX=/usr/local` cannot
take `/usr/local/bin` with it. Running it twice, or on a machine that never had
SAND Vault, does nothing and succeeds.
