# Contributing to SAND Vault

Thanks for wanting to contribute! Please read this before opening a pull
request.

## License and the CLA

SAND Vault is licensed under the **GNU Affero General Public License v3.0**
(`AGPL-3.0-only`). See [`LICENSE`](./LICENSE).

By contributing, you agree to the [Contributor License Agreement](./CLA.md). In
short: you keep ownership of your work, but you grant the maintainer a broad
license — including the right to relicense your contribution under other terms
(such as a future commercial/dual license). This is what keeps it possible to
offer a commercial edition of SAND Vault down the line. If you are contributing
on behalf of an employer, make sure you have the right to do so.

## Signing off your commits (DCO)

You accept the CLA by adding a `Signed-off-by` line to **every** commit, which
also certifies the [Developer Certificate of Origin](https://developercertificate.org/).
The easiest way is to commit with the `-s` flag:

```bash
git commit -s -m "Your message"
```

This appends a line like:

```
Signed-off-by: Your Name <your.email@example.com>
```

The name and email must match your real identity and your git configuration
(`git config user.name` / `git config user.email`).

A CI check (`.github/workflows/dco.yml`) verifies that **every** commit in a pull
request is signed off, and the PR cannot merge until it passes. If you forgot,
sign off your existing commits in one go:

```bash
git rebase --signoff <base-branch>   # e.g. origin/main
git push --force-with-lease
```

## Development

See [`SAND_ARCHITECTURE.md`](./SAND_ARCHITECTURE.md) for the design — what the
vault is, why placement is a security decision, and how the layers are meant to
stay separated. Before opening a PR:

```bash
make build        # web client (Vite) → Go binary with it embedded
make test         # Go unit tests
make test-e2e     # pytest: CLI, HTTP API, vault flow, browser
```

`go vet` runs as part of `make test`; there is no separate linter. Go code is
`gofmt`-formatted.

If Playwright can't launch its bundled Chromium, point it at one you have:

```bash
PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chrome make test-e2e
```

## Things worth knowing before you change them

A few invariants are load-bearing, and a change that breaks one is a change to
the product's security promise rather than a refactor:

- **`internal/provider` must never see plaintext.** By the time bytes reach a
  backend they have already been compressed, split and sealed. Providers handle
  opaque blobs and object keys, nothing else.
- **No account may end up with two parts of one file** under the default
  `strict` placement policy. Any two parts plus the key rebuild the file, so
  doubling up hands one provider everything.
- **Network calls never happen while the vault's write lock is held.** Snapshot
  what you need, release, do the I/O, re-take the lock to commit — and re-check
  that the vault is still unlocked before committing.
- **Nothing readable is written to the vault file.** Filenames and folder
  structure are as sensitive as the contents; the manifest is encrypted for that
  reason, and a test asserts a plaintext filename never appears in it.
- **The version is assembled in one place** ([`scripts/version.mjs`](./scripts/version.mjs)).
  Don't add a second place that renders a version string.

Contributions touching cryptography, key handling, shard placement or the vault
format get read closely, and §5 of the [CLA](./CLA.md) applies to them.

## Pull requests

- Keep changes focused and described clearly.
- Add or update tests for behavior changes (Go: `internal/**/*_test.go`;
  end-to-end: `tests/test_*.py`).
- Make sure `make test` and `make test-e2e` pass.
- Ensure every commit is signed off (see above).
