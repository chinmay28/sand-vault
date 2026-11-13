# Configuration reference

This page lists everything that configures SAND Vault from outside the vault:
global and `sand serve` flags, environment variables (the binary's and the
installer scripts'), the sizing knobs in `machine.json`, and where every file
lives. Settings kept *inside* the vault, such as default accounts, the health
schedule and folder policies, are covered on their own pages. Every command's
flags are in the [CLI reference](cli.md).

## Global flags

Every `sand` command takes these.

| Flag | Default | Meaning |
|---|---|---|
| `--vault <path>` | `$SAND_VAULT`, else `~/.sand/vault.sand` | The vault file |
| `--in <name-or-id>` | | Work inside a sub vault (asks for its password, or reads `SAND_SUB_PASSWORD`) |

## `sand serve`

| Flag | Default | Meaning |
|---|---|---|
| `--port` | `8123` | TCP port |
| `--bind` | `0.0.0.0` | Address to listen on. Any non-loopback bind is logged as a warning: put TLS in front ([Deployment](../guide/deployment.md#reverse-proxy-nginx--tls)) |
| `--idle-timeout` | `30m` | Lock the vault after this much inactivity. Health pings and folder schedules run only while it is unlocked |
| `--webdav` | off | Also serve the vault as a WebDAV share |
| `--webdav-path` | `/dav` | Where the vault's share is mounted |
| `--webdav-sub-path` | `/sub` | Where sub vault shares sit, one per sub vault at `<path>/<name>/`, each behind its own password |
| `--checksums-every` | `2m` | How often `checksums.gz` is brought up to date after the index changes. `0` writes it only on request |
| `--tv-discovery-port` | `8124` | UDP port SAND TV broadcasts to when looking for the server. `0` turns discovery off |
| `--profile` | `$SAND_PROFILE`, else `machine.json` | Pin the [sizing profile](#sizing-to-the-machine): `auto`, `pi5`, `workstation` or `custom` |

The server accepts at most 2 GiB per upload request. The web client sends
uploads in batches of up to 256 MB, and only a single file larger than that
goes up in a request of its own.

## Environment variables

### Read by the `sand` binary

| Variable | Read by | Meaning |
|---|---|---|
| `SAND_VAULT` | all commands | Vault path when `--vault` is not given |
| `SAND_PROFILE` | `sand serve` | Default for `--profile` |
| `SAND_INSTANCE` | all commands | The name this machine opens the vault under; defaults to the hostname. Set it when two machines share a hostname ([Vault management](../guide/vault-management.md)) |
| `SAND_OAUTH_REDIRECT` | `sand serve` | Pin the OAuth redirect URI, such as `https://vault.example.com/api/providers/oauth/callback` |
| `SAND_GOOGLE_CLIENT_ID`, `SAND_GOOGLE_CLIENT_SECRET` | `sand serve` | Your Google OAuth app, so sign-in skips asking for one |
| `SAND_MICROSOFT_CLIENT_ID`, `SAND_MICROSOFT_CLIENT_SECRET` | `sand serve` | The same for OneDrive (the secret is optional) |
| `SAND_DROPBOX_APP_KEY`, `SAND_DROPBOX_APP_SECRET` | `sand serve` | The same for Dropbox |
| `SAND_BOX_CLIENT_ID`, `SAND_BOX_CLIENT_SECRET` | `sand serve` | The same for Box |
| `SAND_ZOHO_CLIENT_ID`, `SAND_ZOHO_CLIENT_SECRET` | `sand serve` | The same for Zoho WorkDrive |
| `SAND_PROTON_STATE_DIR` | `protoncli` backend | Root for each Proton account's client cache and transient session. Defaults to `<user cache dir>/sand/proton`, which a sandboxed service cannot write to. The quick-start unit sets `/var/lib/sand/proton` |
| `SAND_SMART_REPORT` | `sand serve`, `sand smart-scan` | The SMART report path. Default `/var/lib/sand/smart.json` |
| `GOMEMLIMIT` | `sand serve` | If set, it is obeyed. Otherwise the server sets the Go memory limit to 85% of what the cgroup (or the machine) allows |
| `GIT_SSH_COMMAND` | git mirroring | If set, it is used as is. Otherwise git runs ssh with `-o BatchMode=yes`, so it never prompts |

`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`
and `SSL_CERT_DIR` are passed through to the Proton client.

### Passwords for scripts

Each command that needs a secret reads it from a variable if set. Otherwise it
prompts without echo on a terminal, or reads one line from stdin when there is
no terminal. Commands that need two secrets read one from each of two
variables.

| Variable | Used for |
|---|---|
| `SAND_PASSWORD` | The vault password, for every command that opens the vault. Also the new password for `sand vault init` |
| `SAND_SUB_PASSWORD` | A sub vault's password (`--in`, `sand sub new`, `sand sub passwd`, `sand sub import`) |
| `SAND_NEW_SUB_PASSWORD` | The new password in `sand sub passwd` |
| `SAND_BACKUP_PASSWORD` | The password of the vault being recovered (`sand vault recover`) or imported (`sand sub import`) |
| `SAND_SNAPSHOT_PASSWORD` | A snapshot's password (`sand snapshot take`, `sand snapshot restore`) |
| `SAND_KIT_CODE` | The recovery code, or password, that opens a recovery kit (`sand vault kit import`) |
| `SAND_OLD_PASSWORD` | The lost vault's password, tried when a kit's key is refused (`sand vault kit import`) |

`sand vault passwd` reads both the current and the new password from
`SAND_PASSWORD`. With the variable set, both are the same and nothing changes,
so pipe two lines on stdin instead.

### Installer variables

`scripts/quickstart.sh` reads these. Pass them between `sudo` and `bash`:
`curl … | sudo HOST=127.0.0.1 bash`.

| Variable | Default | Meaning |
|---|---|---|
| `SAND_INSTALL` | `source` | `source` builds here; `release` downloads the prebuilt binary (linux/arm64 only today) |
| `SAND_REPO` | `https://github.com/chinmay28/sand-vault.git` | Repository to clone |
| `SAND_REF` | `main` | Branch, tag or commit (source mode) |
| `SAND_RELEASE` | `latest` | Release tag (release mode) |
| `PORT` † | `8123` | `--port` |
| `HOST` † | `0.0.0.0` | `--bind` |
| `SAND_WEBDAV` † | `0` | `1` adds `--webdav` |
| `SAND_USER` † | `sand` | Service user |
| `SAND_PREFIX` † | `/opt/sand` | Install prefix. Not under `/home`, `/root` or `/run/user` |
| `SAND_DATA_DIR` † | `/var/lib/sand` | Vault, backups, Proton state, SMART report |
| `SAND_MOUNT_ROOTS` † | `/media:/run/media:/mnt:/srv` | Mount roots the unit makes writable. Empty grants none |
| `SAND_LOCAL_PATHS` | | Extra writable paths, colon-separated. Merged into `sand.service.d/10-local-paths.conf` |
| `SAND_FORCE_PATHS` | `0` | `1` lets `SAND_USER`, `SAND_PREFIX` or `SAND_DATA_DIR` differ from the installed unit. Nothing is moved |
| `INSTALL_NODE`, `INSTALL_GO` | `auto` | `never` refuses to install the toolchain |
| `BACKUP_KEEP` | `10` | Pre-upgrade vault snapshots kept |
| `INSTALL_PROTON` | `auto` | `never` skips Proton's client |
| `PROTON_CLI_URL` | | A prebuilt `proton-drive` binary to download instead of building |
| `PROTON_SDK_REPO`, `PROTON_SDK_REF` | `github.com/ProtonDriveApps/sdk`, `main` | What to build the client from |
| `PROTON_NPM_REGISTRY` | `https://registry.npmjs.org` | Where the client's dependencies come from |
| `PROTON_BUILD_TIMEOUT` | `45` | Minutes the client build may take |

† Remembered: on an upgrade, an unset variable keeps what the installed unit
uses. `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `GOPROXY`, `GOPRIVATE` and
`NODE_EXTRA_CA_CERTS` (if the service user can read it) are passed to the
build.

The other scripts:

| Script | Arguments and variables |
|---|---|
| `deploy-linux.sh` | `[binary] [port] [bind]` (defaults `./sand`, `8123`, `0.0.0.0`); `SAND_DATA_DIR`, `SAND_MOUNT_ROOTS`; `SAND_LOCAL_PATHS` replaces the drop-in |
| `allow-local-path.sh` | `<path>…`, `--list`, `--remove <path>…`; `SAND_SERVICE` (default `sand`), `SAND_USER` (for the write check) |
| `enable-disk-health.sh` | `--remove`; `SAND_BIN` (default `/opt/sand/src/sand`), `SAND_SMART_REPORT`, `SAND_SERVICE` |
| `deploy-windows.ps1` | `-Binary`, `-Port` (`8123`), `-Bind` (`0.0.0.0`) |

## Sizing to the machine

Several numbers depend on what the machine has: the size new uploads are cut
into, how many pieces an upload keeps in the air, how far a download reads
ahead, how much decrypted film is cached for a seeking player, and how big a
job a scheduled task takes on unattended. SAND Vault derives them at startup
from the CPU count and memory it is *allowed*. Under systemd that is the
cgroup's `MemoryMax` and `CPUQuota`; otherwise it is the machine's totals.

Changing them never changes what is stored. Every file records the chunk size
it was cut with, so a vault moved from a Pi to a workstation reads exactly as
before, and only new uploads are cut the new way. Every knob takes effect when
saved, with no restart. The pipeline these numbers feed is described in
[Data pipeline](../design/data-pipeline.md).

### Profiles

Set the profile in **Vault settings → This machine**:

| Profile | Sized for |
|---|---|
| **Automatic** (`auto`) | The machine detected. The default |
| **Raspberry Pi 5 · 16 GB** (`pi5`) | 4 CPUs, 16 GB, whatever is detected. Use it, for example, in a container that sees the host's memory |
| **Workstation · 64 GB** (`workstation`) | 24 CPUs, 64 GB |
| **Custom** (`custom`) | CPUs and memory you type, to leave room for other work on the box |

**Advanced** shows every knob with the profile's value beside it. A knob you
set is kept when the profile changes. **Reset to system defaults** returns to
Automatic with nothing set. `--profile` or `SAND_PROFILE` pins the profile from
the unit file. The page then says so, and lets you change the knobs but not
the profile. A pin to a profile that does not fit the machine (a workstation
pin on a Pi) is ignored with a log line.

What the presets come to:

| Knob (`machine.json` name) | Range | Pi 5 · 16 GB | Workstation · 64 GB |
|---|---|---|---|
| Chunk size (`chunk_size`) | 1–256 MiB | 16 MiB | 32 MiB |
| Upload window (`upload_window`), chunks in flight | 1–64 | 4 | 12 |
| Read-ahead (`read_ahead`), chunks | 0–32 | 1 | 6 |
| Chunk cache (`chunk_cache_bytes`), dropped on lock | 16 MiB–64 GiB | 1 GiB | 4 GiB |
| Compression (`compress_level`), 1 fastest to 4 smallest | 1–4 | 3 | 3 |
| Compression threads (`compress_threads`) | 1–64 | 2 | 8 |
| Transfer window (`transfer_window`): moves, checks, repacks | 1–64 | 4 | 8 |
| Erase window (`erase_window`): files deleted at once | 1–64 | 4 | 8 |
| Connections per cloud (`conns_per_host`), kept idle | 2–256 | 12 | 32 |
| Unattended rebuild limit (`rebuild_limit`) | 64 MiB–1 TiB | 4 GiB | 16 GiB |
| Unattended bundle limit (`bundle_limit`), git bundles | 64 MiB–1 TiB | 4 GiB | 16 GiB |

How Automatic scales them:

- the chunk size is 16 MiB below 32 GB of memory and 32 MiB from there up;
- the windows follow the CPU count, capped where consumer clouds start
  rate-limiting;
- the cache is a sixteenth of memory;
- the unattended limits are a quarter of memory.

A machine smaller than a Pi gets the Pi's floors, then shrinks the cache,
then the read-ahead, then the upload window until they fit.

**The memory rule.** Upload window × 2.5 × chunk size, plus (read-ahead + 1)
× chunk size, plus the cache, must stay under half the machine's memory. A
setting that breaks it is refused, with the total shown.

### `machine.json`

The choice is stored as `machine.json` in the vault's directory. It is plain
JSON with nothing secret in it, and you can edit it by hand. It belongs to the
machine, not the vault, so a vault restored from a recovery kit onto another
box does not bring a Pi's windows with it. Only the *decision* is stored; the
numbers are derived again on every start.

```json
{
  "profile": "custom",
  "machine": { "cpus": 4, "memory_bytes": 8589934592 },
  "overrides": { "chunk_size": 33554432, "read_ahead": 2 }
}
```

A missing file means Automatic. If the file cannot be read, or its settings
cannot be applied (a window set to zero, say), the server logs the reason and
runs on the profile's defaults.

Two things are deliberately not here. The password stretching (Argon2id,
64 MB) is stored in the vault and travels with it. And cloud rate limits do
not grow with the machine: a workstation compresses, encrypts and overlaps
round trips faster, but no cloud lets it make requests any faster.

## Where things live

**Beside the vault file** (`~/.sand/` by default; `/var/lib/sand/` under
systemd):

| Path | What |
|---|---|
| `vault.sand` | The encrypted index and every account's credentials, plus vault settings such as the film database key and the assistant's address. Back this up |
| `vault.sand.reads` | Read-speed history per cloud, by day for 400 days plus an all-time total. Encrypted under a key derived from the vault's, so it opens only with the vault. Safe to delete; counting starts again |
| `machine.json` | The [sizing choice](#machinejson). Written the first time it is changed |
| `.sand-vault-*` | A vault write in progress, renamed over `vault.sand` when complete |
| `.sand-upload-*` | An upload being spooled before it is scattered. The vault's disk needs room for the largest file in flight |
| `.sand-convert-*`, `.sand-git-*` | Working files for a format conversion and a git mirror. Cleared afterwards |

**On every connected account**, inside its folder or prefix:

| Path | What |
|---|---|
| `<archive-id>-c<chunk>-p<part>.sand` | One part of one chunk of a file. The archive ID is random and reveals nothing but the size |
| `<archive-id>-p<part>.sand` | A part of a file stored before chunking |
| `manifest.sand` | An encrypted copy of the index that opens with the vault password alone. It never holds account credentials or the film database key ([Recovery](../guide/recovery.md)) |

**On a quick-start install:**

| Path | What |
|---|---|
| `/var/lib/sand/backups/vault-<timestamp>.sand` | Pre-upgrade snapshots (`BACKUP_KEEP`) |
| `/var/lib/sand/proton/<account-id>/` | Proton client cache and transient session |
| `/var/lib/sand/smart.json` | The latest SMART report |
| `/opt/sand/src/`, `bin/`, `checkout/` | The managed clone, a release binary, or a copy of your checkout |
| `/opt/sand/quickstart.log` | Everything the installer has printed |
| `/etc/systemd/system/sand.service` | The unit. Rewritten on every install |
| `/etc/systemd/system/sand.service.d/10-local-paths.conf` | Local folder grants. Never rewritten by an upgrade |
| `/etc/systemd/system/sand-smart.{service,timer}` | The hourly disk scanner |
| `/usr/local/bin/proton-drive` | Link to Proton's client |

`deploy-linux.sh` installs the binary at `/usr/local/bin/sand` and uses the same
`/var/lib/sand`. On Windows, the NSSM service logs to `%ProgramData%\sand\`.
