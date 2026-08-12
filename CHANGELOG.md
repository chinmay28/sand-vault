# Changelog

Releases are `vMAJOR.MINOR.PATCH`, where the patch number is the repository's
commit count — `v2.0.42` is the 42nd commit on the 2.0 line. See
[`internal/version/version.go`](./internal/version/version.go).

Each section below is the body of the corresponding GitHub release. A heading
must name the tag exactly — a tag whose commit builds a different version is a
tag that shouldn't be published.

## v2.0 — the multi-cloud file browser

SAND stops being a batch tool and becomes a file browser over storage you don't
fully trust.

Before, you fed it files and got three zip archives back, then carried them to
three places yourself. Now you connect your cloud accounts once and it does the
carrying: every file you add is compressed, split into three encrypted parts,
and scattered across **separate** accounts. Opening a file gathers the parts
back and rebuilds it in memory. No single provider ever holds your data.

The old workflow is still there — `sand archive` and `sand restore` need no
vault and no accounts — and the `.media` part format is unchanged, so parts
written by v2 still restore with the v1 command.

### Connected cloud accounts

Five backends behind one small object-store interface: **local folder**,
**S3-compatible** (Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO),
**WebDAV** (Nextcloud, ownCloud, Box, Koofr, Fastmail), **Google Drive** and
**Dropbox**.

All of it is written on the standard library — SigV4 request signing included —
so there are no cloud SDKs, no CGO, and the deployable artifact is still one
static binary. Each backend declares the fields it needs, and the browser's
connect form is generated from that declaration, so adding a backend needs no
frontend change.

### The vault

A single encrypted file (`~/.sand/vault.sand`, or `/var/lib/sand/vault.sand`
under systemd) holding the index of what you've stored, where each part went,
and the credentials for every connected account. Filenames and folder structure
are themselves sensitive, so none of it is written in the clear.

File parts are encrypted under a random 256-bit data key that is itself wrapped
by your password. Changing your password re-wraps 32 bytes instead of
re-uploading everything, and part encryption doesn't inherit a weak password.

**Back this file up.** It is the only record of which account holds which part
of which file. The parts scattered across your providers cannot be rebuilt
without it.

### Placement is a security decision

Any two parts plus the key rebuild a file, so two parts on one account means
that account could rebuild it. The default **strict** policy never allows that,
and refuses to upload when only one account is connected. **Redundant** is
offered for people with fewer accounts and says plainly what it costs.

Uploads scatter concurrently and commit at two-of-three, rolling back the parts
that did land if fewer than two succeed. Reads race all three accounts and
finish on the first two to arrive, so an offline account costs nothing on the
read path.

### The browser

Lock screen, a sidebar of connected accounts with live status and how much each
is holding, breadcrumbs, drag-and-drop upload with progress, part badges
coloured per account, a per-part health read-out, and inline preview for
images, video, audio, PDF and text — each one rebuilt on demand.

It loads no external fonts, scripts or styles: opening your vault makes zero
third-party requests.

### Quick start

One command installs SAND as a hardened systemd service, building from source
or installing a prebuilt binary:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/sand/main/scripts/quickstart.sh | sudo bash
```

Re-run it to upgrade. It snapshots the vault before swapping code in, builds
the new version while the old one keeps serving, and rolls back — code *and*
vault — if the new one fails its health check.

### Versioning

The patch number is now the repository's commit count, assembled in one place
([`scripts/version.mjs`](./scripts/version.mjs)) and stamped into both the Go
binary and the web bundle, so the header, `sand version` and `/api/health` can
never disagree. A patch of `0` marks an unstamped build — including one made
from a shallow clone, which is detected rather than guessed around.

### Also

- `cmd/sand` is present. The Makefile and the e2e suite both referenced it, but
  it had never been committed, so the tree did not build.
- New CLI: `vault`, `remote`, `ls`, `put`, `get`, `mkdir`, `mv`, `rm`, `check`.
  `sand check --all` exits non-zero on a degraded or unrecoverable file, which
  makes it a reasonable cron job.
- `scripts/deploy-linux.sh` writes a unit the service can actually run — it
  previously combined `ProtectHome=yes` with no vault path, so the server had
  nowhere to write.
- Sessions with idle auto-lock, `SameSite=Strict` cookies and `Origin` checks
  on every write; stored HTML and SVG are served as downloads rather than
  rendered in the app's origin.

## v1.0 — split, encrypt, distribute

The original: compress with zstd, split in two, generate a third XOR redundancy
part, encrypt all three with AES-256-GCM under an Argon2id-derived key, and
write three zip archives. Any two of the three reconstruct the original. CLI
and an embedded web GUI, shipped as one static binary.
