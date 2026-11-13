# Sand Harbor documentation

Sand Harbor is a self-hosted home for files and media built on **SAND Vault**,
an encrypted, erasure-coded store spread across cloud accounts. **SAND TV**
plays its films on a Fire TV. Start with the [project README](../README.md) for
the overview and quick start. This page indexes everything else.

## User guide

Task-oriented: how to do something, and what to expect.

| Page | Covers |
|---|---|
| [Installation](guide/installation.md) | The Linux one-command installer, prebuilt releases, building from source, your first vault, upgrading and uninstalling |
| [Deployment](guide/deployment.md) | systemd, local disks under the sandboxed service, Windows, nginx and TLS |
| [Accounts](guide/accounts.md) | Supported backends, connecting by CLI or by signing in, names and colours, quotas, health checks, repairs |
| [Storing files](guide/storing-files.md) | What happens on upload and download, choosing m-of-n and clouds, files that went out short, replication, moving and renaming |
| [Web UI](guide/web-ui.md) | A tour of the browser app, including on a phone |
| [Folder tools](guide/folder-tools.md) | Organizing, folder stats, duplicates, corrupt files, checksums, snapshots |
| [Films](guide/films.md) | Film details and folder pictures |
| [Streaming](guide/streaming.md) | VLC, TVs, and mounting the vault as a drive over WebDAV |
| [SAND TV](guide/sand-tv.md) | Installing, pairing and using the Fire TV app |
| [Maintenance](guide/maintenance.md) | Self-maintaining folders, stray and lost parts, old versions, leftovers |
| [Vault management](guide/vault-management.md) | Changing the password, sub vaults, several machines |
| [Recovery](guide/recovery.md) | Losing the vault file, recovery kits, rebuilding a file by hand |
| [Repositories](guide/repositories.md) | Keeping mirrors of git repositories |
| [Sandy](guide/sandy.md) | The vault's archivist assistant |

## Reference

| Page | Covers |
|---|---|
| [CLI](reference/cli.md) | Every `sand` command and flag |
| [HTTP API](reference/api.md) | Every endpoint, its auth, request and response |
| [Configuration](reference/configuration.md) | Environment variables, flags, file locations, tuning knobs |

## Design

How it works inside, and why.

| Page | Covers |
|---|---|
| [Architecture](architecture.md) | System shape, main flows, package map, concurrency, what is not built |
| [Design choices](design-choices.md) | The key decisions, their rationale, and the alternatives rejected |
| [Security](security.md) | Threat model, what is and isn't protected, network exposure |
| [Vault](design/vault.md) | Vault file, keys, locking, manifest backups, recovery, sub vaults, several machines |
| [Index features](design/index.md) | Search, film details, organizing, folder maintenance, repositories |
| [Data pipeline](design/data-pipeline.md) | Storing, opening and seeking; the erasure code; the part format; sizing |
| [Cryptography](design/cryptography.md) | Argon2id, HKDF chunk keys, AES-GCM, integrity, checksums, snapshots |
| [Placement](design/placement.md) | Schemes, choosing accounts, object keys, re-placing data |
| [Providers](design/providers.md) | The backend interface, sign-in, quotas, health |
| [Server](design/server.md) | Partial success, errors, WebDAV, stream tickets, standalone mode |
| [SAND TV](design/sand-tv.md) | Pairing codes, discovery, the player |
| [Recovery kit](design/recovery-kit.md) | The offline kit that rebuilds a vault |
| [Paced password change](design/paced-rekey.md) | Re-keying a large vault without stopping it |
| [SFTP](design/sftp.md) | Machines you have a login on |
| [Streaming performance](design/streaming-performance.md) | Where the time goes when a UHD film plays |
| [Adding a backend](design/cloud-backends.md) | Candidate tiers and the recipe for a new provider |

## Contributing

- [Developer guide](developer-guide.md): build, test, release, project structure
- [CONTRIBUTING.md](../CONTRIBUTING.md) and the [CLA](../CLA.md)
- [Changelog](../CHANGELOG.md)
