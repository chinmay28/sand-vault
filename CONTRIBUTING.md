# Contributing to Sand Harbor

Thanks for wanting to contribute! Please read this before opening a pull
request. Sand Harbor is this repository: SAND Vault (the `sand` binary: CLI,
server, web UI, WebDAV) and SAND TV (the Fire TV app in `apps/firetv`).

## License and the CLA

Sand Harbor is licensed under the **GNU Affero General Public License v3.0**
(`AGPL-3.0-only`). See [`LICENSE`](./LICENSE).

By contributing, you agree to the [Contributor License Agreement](./CLA.md). In
short: you keep ownership of your work, but you grant the maintainer a broad
license — including the right to relicense your contribution under other terms
(such as a future commercial/dual license). This is what keeps it possible to
offer a commercial edition of Sand Harbor down the line. If you are contributing
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

The name and email must match the commit's author exactly, character for
character, so they have to match your git configuration
(`git config user.name` / `git config user.email`). If you change either, the
sign-off changes with it. To have every commit in a clone signed off, with or
without `-s`:

```bash
git config format.signOff true
```

A CI check (`.github/workflows/dco.yml`) verifies that **every** non-merge
commit in a pull request carries a sign-off matching its author, and the PR
cannot merge until it passes. If you forgot, sign off your existing commits in
one go rather than adding another commit:

```bash
git rebase --signoff <base-branch>   # e.g. origin/main
git push --force-with-lease
```

An amend or a rebase keeps the sign-off only if it signs off too (`-s`, or
`format.signOff`), so check `git log -1 --format=%B` afterwards.

## Development

See [`docs/architecture.md`](./docs/architecture.md) for the design: what the
vault is, why placement is a security decision, and how the layers are meant to
stay separated. [`docs/design-choices.md`](./docs/design-choices.md) records
the key decisions and the alternatives rejected, and
[`docs/developer-guide.md`](./docs/developer-guide.md) covers the build, the
dev loop, tests and releases in full. Before opening a PR:

```bash
make build        # web client (Vite) → Go binary with it embedded
make test         # Go unit tests
make test-e2e     # pytest: CLI, HTTP API, vault flow, browser
```

CI checks the sign-off, and builds and tests SAND TV when `apps/firetv` or
`internal/tvdiscover` changes. It does **not** run the Go tests or the e2e
suite, so run them yourself. `make test-deps` installs the e2e suite's Python
dependencies and Playwright's Chromium.

`go test` runs a subset of `go vet`. Run `go vet ./...` for the full set; there
is no other linter. Go code is `gofmt`-formatted. If you changed
`web/public/icon.svg`, run `make icons` and commit the PNGs (`make icons-check`
verifies them). If you changed `web/src`, commit the rebuilt
`internal/server/dist/` too: the binary embeds it, and it is committed so a
bare checkout builds without Node.

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
  `strict` placement policy. Any k parts plus the key rebuild the file (two, for
  the default 2-of-3), so doubling up can hand one provider everything.
- **Network calls never happen while the vault's write lock is held.** Snapshot
  what you need, release, do the I/O, re-take the lock to commit — and re-check
  that the vault is still unlocked before committing.
- **Nothing readable is written to the vault file.** Filenames and folder
  structure are as sensitive as the contents; the manifest is encrypted for that
  reason, and a test (`TestVaultFileLeaksNoFilenames`) asserts a plaintext
  filename never appears in it.
- **The version is assembled in one place** ([`scripts/version.mjs`](./scripts/version.mjs)).
  The Go binary renders it with `version.String()` and the web client with
  `web/src/version.js`, both fed from that script. Don't add another place that
  builds a version string.

Contributions touching cryptography, key handling, shard placement or the vault
format get read closely, and §5 of the [CLA](./CLA.md) applies to them.

## Pull requests

- Keep changes focused and described clearly. Commit subjects follow
  `type(scope): summary`, e.g. `feat(tv): …`, `fix(stream): …`.
- Add or update tests for behavior changes (Go: `*_test.go` beside the code
  under `internal/**` and `cmd/sand`; end-to-end: `tests/test_*.py`; SAND TV:
  `apps/firetv/app/src/test`).
- Make sure `make test` and `make test-e2e` pass.
- Ensure every commit is signed off (see above).
