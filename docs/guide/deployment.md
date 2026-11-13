# Deploying Sand Vault

This page covers running `sand serve` as a long-lived service: the systemd
unit and its sandbox, giving Local folder accounts somewhere to write, disk
health, Windows under NSSM, and putting TLS in front with nginx. For getting a
binary in the first place, see [Installation](installation.md).

## Linux: systemd

There are two installers, and both write `/etc/systemd/system/sand.service`
with `/var/lib/sand` as the data directory:

| | `scripts/quickstart.sh` | `scripts/deploy-linux.sh` |
|---|---|---|
| Gets the binary | Builds from source, or downloads a release | Uses one you already built |
| Binary lives at | `/opt/sand/src/sand` (or `bin/`, `checkout/`) | `/usr/local/bin/sand` |
| Upgrade, snapshot, rollback | Yes | No: it rewrites the unit and restarts |
| Disk-health scanner | Installed | Run `enable-disk-health.sh` yourself |
| Proton client and its state dir | Built; `SAND_PROTON_STATE_DIR` set | Neither |
| Remembers settings on re-run | Yes | No: re-pass port and bind each time |

For most machines use the [quick start](installation.md#linux-quick-start-ubuntu-debian-raspberry-pi-os).
The small path, when you have a binary and only want it running:

```bash
make build
sudo ./scripts/deploy-linux.sh ./sand 8123 127.0.0.1    # binary, port, bind
systemctl status sand && journalctl -u sand -f
```

With no arguments it looks for `./sand`, then `dist/sand-*-linux-amd64`, and
binds `0.0.0.0:8123`. Create the vault as the service user, since the service
cannot read a root-owned file:

```bash
sudo -u sand /usr/local/bin/sand --vault /var/lib/sand/vault.sand vault init
```

Running the quick start later on the same host takes the service over. It
reads the user, port, bind and vault path back from the unit.

### What the unit does

The unit the quick start writes, trimmed:

```ini
[Service]
User=sand
ExecStart=/opt/sand/src/sand serve --port 8123 --bind 0.0.0.0 --vault /var/lib/sand/vault.sand
Environment=SAND_VAULT=/var/lib/sand/vault.sand
Environment=SAND_PROTON_STATE_DIR=/var/lib/sand/proton
Environment=SAND_SMART_REPORT=/var/lib/sand/smart.json
Restart=on-failure
MemoryMax=80%
MemorySwapMax=0
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
ReadWritePaths=/var/lib/sand
ReadWritePaths=-"/media"
ReadWritePaths=-"/run/media"
ReadWritePaths=-"/mnt"
ReadWritePaths=-"/srv"
```

- **`--vault` is not optional.** The `sand` user has no home and
  `ProtectHome=true` hides `/home`, so the default `~/.sand/vault.sand` would be
  unreachable.
- **`MemoryMax=80%` with no swap.** On a Raspberry Pi, a limit met by swapping
  to an SD card makes the whole box stop answering, SSH included. Being killed
  and restarted is better. At startup the server reads the cgroup's limit and
  sets `GOMEMLIMIT` to 85% of it, so the garbage collector works against the
  ceiling instead of growing towards it. It also sizes its buffers to that
  limit ([Sizing to the machine](../reference/configuration.md#sizing-to-the-machine)).
  To give it more room, run `systemctl edit sand`, set a new `MemoryMax=` and
  restart. Nothing else needs changing.
- **The sandbox.** The service holds every cloud credential, so it can write
  only to its data directory and the mount roots, and cannot open raw devices.

Change the unit with drop-ins (`systemctl edit sand`), not by editing the
file. The quick start rewrites `sand.service` on every run but leaves
`sand.service.d/` alone.

### Local folders under systemd

`ProtectSystem=strict` makes the filesystem read-only to the service apart
from its `ReadWritePaths=`. Both installers grant `/media`, `/run/media`, `/mnt`
and `/srv`, so an external disk or NAS mount in the usual place connects as a
**Local folder** with no extra step. The leading `-` means a root that does not
exist, or a drive that is not plugged in at boot, does not stop the service.
Set `SAND_MOUNT_ROOTS` (colon-separated) at install time to change the list.
Set it empty to grant nothing but the data directory.

A folder anywhere else, such as `/data/SandVault`, is read-only to the service.
Connecting it fails with:

```
… /data/SandVault is not writable: read-only file system — the sand service
runs under systemd with ProtectSystem=strict, which makes every path outside
its data directory and the usual mount roots (/media, /run/media, /mnt, /srv)
read-only to it. Grant it this one: sudo scripts/allow-local-path.sh
/data/SandVault (or add ReadWritePaths= to the unit) and reconnect.
```

Grant the path and reconnect:

```bash
sudo ./scripts/allow-local-path.sh /data/SandVault        # creates it if missing
sudo ./scripts/allow-local-path.sh --list                 # what the unit and drop-in grant
sudo ./scripts/allow-local-path.sh --remove /data/SandVault
```

It writes `/etc/systemd/system/sand.service.d/10-local-paths.conf` and
restarts the service. Grants survive upgrades. A path the unit already covers
is reported as a no-op. To grant at install time instead:

```bash
curl -fsSL …/quickstart.sh | sudo SAND_LOCAL_PATHS=/data/SandVault bash
```

The quick start *adds* `SAND_LOCAL_PATHS` to what the drop-in already grants.
`deploy-linux.sh` *replaces* the drop-in with what you name, so pass every
path you still want.

Two things the sandbox has no say in. The script checks both and the connect
error names them:

- **Ownership.** The service runs as `sand`. A drive your desktop mounted
  belongs to your login and is usually `0700`. Either
  `sudo chown -R sand:sand /media/you/Disk/SandVault`, or mount it with
  `uid=`, `gid=` and `umask=` (NTFS, exFAT).
- **A read-only mount.** On NTFS this is usually the dirty bit left by Windows
  fast startup or hibernation. Remount read-write first.

A drive plugged in after the service starts is picked up automatically. If a
granted path stops working after a re-plug, `sudo systemctl restart sand`. A
unit written before the mount roots were granted is refreshed by re-running
the quick start.

`sand serve` run from your own shell has none of this; it writes wherever your
user can.

### Disk health (SMART)

A Local folder account answers the hourly [health ping](accounts.md#are-the-clouds-still-there)
for as long as its filesystem mounts, which can be weeks after the disk under
it has started to fail. The disk's own SMART counters move earlier. The
service cannot read them (`PrivateDevices=true`), so a separate root-run
one-shot does: `sand smart-scan` asks `smartctl` about every physical disk and
writes `/var/lib/sand/smart.json`. During its health check the service joins
that report to each Local folder account through the mount table, and warns on
pending or reallocated sectors, failed self-tests, or a drive past its own
thresholds. SD cards and most USB sticks keep no SMART data and are recorded
as unsupported, not as healthy.

The quick start installs `sand-smart.service` and `sand-smart.timer` (hourly,
and five minutes after boot), plus `smartmontools` via apt, and runs the first
scan. For any other install:

```bash
sudo apt install smartmontools
sudo SAND_BIN=/usr/local/bin/sand ./scripts/enable-disk-health.sh   # deploy-linux.sh's binary
sudo ./scripts/enable-disk-health.sh --remove
```

`SAND_BIN` defaults to `/opt/sand/src/sand`, which is right only for a
source-mode quick start. `SAND_SMART_REPORT` moves the report; the service
reads the same variable. A report older than 24 hours counts as missing.

The check is on by default: **Cloud health → Disk health → On/Off** in the
accounts drawer. With it on and no report, each account on a physical disk
says so. To see what the scanner sees, run `sudo sand smart-scan --out /tmp/smart.json`.

## Windows: NSSM

```powershell
winget install nssm
make build
.\scripts\deploy-windows.ps1 -Binary .\sand.exe -Port 8123 -Bind 127.0.0.1
Get-Service sand
```

Run it from an elevated PowerShell. It registers `sand.exe serve --port … --bind …`
as an auto-start service named `sand`, with logs in
`%ProgramData%\sand\sand.log` and `sand-error.log`. With no `-Binary`, it looks
for `.\sand.exe`, then `.\dist\sand-*-windows-amd64.exe`. Re-running removes
the service and installs it again.

Gotchas:

- The service runs the binary **where it is**. Don't delete or move the build
  folder afterwards; copy `sand.exe` somewhere permanent first.
- The script passes no `--vault`, so the vault is `.sand\vault.sand` under the
  home of the account NSSM runs the service as. For LocalSystem that is the
  system profile. To pin it, set the variable on the service and restart:
  `nssm set sand AppEnvironmentExtra SAND_VAULT=C:\ProgramData\sand\vault.sand`.
- There is no upgrade, snapshot or rollback. Back up the vault file before
  replacing the binary.

## Reverse proxy (nginx + TLS)

The server speaks plain HTTP. Anywhere but loopback, put TLS in front of it:
the vault password, every decrypted file and (with `--webdav`) the password on
every WebDAV request otherwise cross the network in the clear.
`scripts/nginx-sand.conf` redirects HTTP to HTTPS and proxies to
`127.0.0.1:8123`:

```bash
sudo cp scripts/nginx-sand.conf /etc/nginx/sites-available/sand
sudo sed -i 's/sand.example.com/vault.your-domain/g' /etc/nginx/sites-available/sand
sudo ln -s /etc/nginx/sites-available/sand /etc/nginx/sites-enabled/
sudo certbot --nginx -d vault.your-domain
sudo nginx -t && sudo systemctl reload nginx
```

Then bind the service to loopback (`HOST=127.0.0.1` on the quick start, or
`127.0.0.1` as `deploy-linux.sh`'s third argument), so the plain port is not
also reachable.

The proxy forwards `X-Forwarded-Proto`. The server uses it to mark the session
cookie `Secure` when the browser is on HTTPS.

Two limits in the shipped config are tighter than the server's:

- **`client_max_body_size 512M`.** The browser sends uploads in batches of up
  to 256 MB, but a single file larger than that goes up in a request of its
  own. The server accepts up to 2 GiB per request, so nginx returns 413 for any
  file between 512 MB and 2 GiB. Raise the limit to `2g` to match the server.
  For larger files, use `sand put` or a [machine import](accounts.md#moving-your-own-files-to-a-machine).
- **`proxy_read_timeout` / `proxy_send_timeout 120s`.** A large upload, a long
  stream or a slow job can outlast two minutes. Raise both if transfers through
  the proxy are cut off.

Other ways to get TLS: **Tailscale Serve** gives you HTTPS on your tailnet
with no certificate to manage. Any reverse proxy works the same way as nginx.
For OAuth sign-in behind a proxy, also set `SAND_OAUTH_REDIRECT`
([Accounts](accounts.md#signing-in-to-google-drive-onedrive-dropbox-box-and-zoho)).

## Ports and firewalls

| Port | Protocol | What |
|---|---|---|
| 8123 | TCP | Web UI, API, WebDAV (`--port`) |
| 8124 | UDP | Sand TV discovery: the TV broadcasts here to find the server (`--tv-discovery-port`, `0` turns it off). It answers only on a bind a TV could reach, and authorizes nothing; pairing still needs a code. See [Sand TV](sand-tv.md). |

## Release builds

`./scripts/build-release.sh [version]` cross-compiles into `dist/`:
`sand-<version>-{linux-amd64,linux-arm64,darwin-amd64,darwin-arm64}` and
`sand-<version>-windows-amd64.exe`, plus `SHA256SUMS`. With no argument the
version comes from `scripts/version.mjs`, and it warns if the build is
unstamped (a shallow clone). The GitHub release itself publishes fewer files:
see [Installation](installation.md#prebuilt-release-instead-of-a-build) and the
[developer guide](../developer-guide.md).
