# SSH, SFTP, and the machine you have a login on

Two features share one SSH client, and the first thing worth doing is keeping
them apart in your head:

|  | **Backend** — a shard destination | **Source** — a place you import from |
|---|---|---|
| What the far end sees | opaque encrypted shards | your actual files, in the clear |
| Who names the paths | SAND, from the archive ID | you, browsing |
| Access needed | read/write under one folder | read only |
| Implements | `provider.Provider` | `vault.Source` |
| Status | **built** — `internal/provider/sftp.go` | **built** — `internal/vault/source.go` |

They should stay two connected entries even when they point at the same box.
The cost is typing the host twice; what it buys is that an import source cannot
see the shard store, and a bug in the import path cannot write into it. Same
host, same key, two roots, two entries.

---

## Why SFTP and not something you install

The alternative was a daemon on the far end — MinIO or Garage speaking S3,
which `s3.go` already handles with no Go code at all. That is genuinely the
cheaper first move and it is worth knowing about. What it cannot do is reach a
box you were given a login on and nothing else: a Hetzner Storage Box, rsync.net,
a NAS, a Pi in somebody else's house. One SFTP backend covers all of them and
requires nothing to be installed anywhere.

It cost less than `docs/cloud-backends.md` estimated. `golang.org/x/crypto` was
already a direct dependency for Argon2 and HKDF, so `x/crypto/ssh` is a new
import path rather than a new module; the only genuinely new module is
`github.com/pkg/sftp` and its very small `kr/fs`. Still no CGO, still one static
binary.

---

## Host keys, which is the part to get right

Speaking the protocol instead of shelling out to `ssh` has one serious
consequence: **this code owns host key verification, and there is no
`~/.ssh/known_hosts` to fall back on.** Under the systemd unit both installers
write, SAND runs as a user with no home of its own and `ProtectHome=yes` in
force. There is no such file and there never will be. Whatever `internal/sftp`
does is the whole of the check.

This is exactly where `internal/git` gets to cheat and this cannot. Git is a
program that already knows how to reach every repository its user can reach —
an agent holding a passphrase, a credential helper, a host alias in
`~/.ssh/config` — and borrowing it buys all of that for free. There is no
equivalent program to borrow here: `sftp(1)` is an interactive client with no
machine-readable output worth parsing.

So: **trust on first use, and store what was learned.**

- The first connection to a host records its key fingerprint.
- Every connection after that requires the same one.
- A host answering with a different key is refused, with both fingerprints in
  the error, because SAND cannot tell a rebuilt VPS from an impostor and the
  person who owns the server can.
- Somebody who has the fingerprint out of band can paste it into the connect
  form and have the *first* connection checked too. It is validated when typed,
  so a typo is a message under the field rather than a host key mismatch on the
  first transfer — which reads like an attack.

The learned fingerprint is persisted through `CredentialRotator`, the same
mechanism Box and Microsoft use to write back a rotated refresh token. That
interface exists for "an option that changes as the backend is used", which is
precisely what a learned host key is. **A fingerprint learned and then
forgotten pins nothing** — the next connection learns again, and would learn an
impostor's key just as happily — so this wiring is the feature, not a detail of
it.

What the code will not do is connect without checking.
`ssh.InsecureIgnoreHostKey` appears in every SFTP example on the internet and it
quietly turns "encrypted to my server" into "encrypted to whoever answered".
There is no setting to switch it on, because a setting that can be switched on
is a setting somebody switches on to make an error go away.

**Known weakness, stated plainly:** a first connection to an impostor pins the
impostor. TOFU buys you that a host reached honestly once cannot afterwards be
impersonated, and nothing more. Pasting the fingerprint in is how you close it.

---

## Credentials

The key is **pasted into the vault**, not referenced by a path on disk.
`provider.Config` says a config is only ever written inside the encrypted vault
*because* `Options` holds credentials, so this is consistent with every other
backend, it survives a reinstall, and it travels in the recovery kit. A
passphrase-protected key is supported and the passphrase is held only while the
vault is open.

No agent support: the server is a daemon, there is nothing to forward to.
Password auth exists for boxes whose web console will not let you install a key,
and is not recommended for anything else.

One consequence worth designing around: a private key is multi-line, and an
HTML `<input>` silently drops the line breaks out of a pasted PEM block. That
is why `FieldSpec` grew a `Multiline` flag and `SpecFields` grew a textarea
branch — without it the field cannot be filled in at all. The textarea is not
masked: a key long enough to need one is also one you check by looking at, and
a wall of dots defeats the only reason the box is that big.

---

## Path confinement

`sftp.Under(root, rel)` is the single chokepoint, and it **refuses** rather than
clamps.

Anchoring a path to the root first — the usual trick, and what `local.go` does —
turns `../../etc/passwd` into a perfectly good path inside the root and hands it
back without a word. That is fine for a backend whose keys SAND generates and
wrong for a path a person typed, and the import feature makes it a path a person
typed.

The check is made twice over, in the spirit of `internal/git`'s "shut off twice
because the second line of defence is what survives somebody adding a call site
later":

1. Any `..` segment at all is refused, **before** anything is cleaned — cleaning
   first makes the refusal impossible to state, because `path.Clean` collapses
   `/../../etc/passwd` to `/etc/passwd` and the hops above the root are gone
   before they can be objected to.
2. The joined result is then checked against the root, which catches the sibling
   case: `/srv/sandbox` is not under `/srv/sand`, however much it looks like it.

For the import side there is a third rule, and it turned out to need real work
rather than a line: **do not follow a symlink out of the configured root.**
Browsing wants to be permissive and a symlink is the way permissiveness escapes.
See "The import half, as built" below for why the path check alone was not
enough and what `(*sftp.Client).resolve` does instead.

---

## Resume, and why it needs no job framework

The whole question of "pause and resume without losing progress" has already
been answered one layer down, and the answer generalises.

`archive/chunked.go` cuts a file into 16 MiB chunks, each split into shards
under deterministic keys. Resuming an interrupted scatter is therefore a
`List` and a set difference — not offset bookkeeping, not a partial-file
protocol. Kill the process at any point and the next run is correct, because
the keys are deterministic and `Put` is idempotent.

Import gets the same property one level up, for free:

> **A file already in the vault at the destination path, with the same size and
> hash, is not fetched again.**

Re-running an import *is* the resume mechanism. No job state, no partial-file
bookkeeping, correct if the server is killed mid-import.

This matters because **there is no background job framework**, and v1 should not
build one. Every handler in `internal/server` is synchronous under a
`contextWithTimeout` — uploads get 30 minutes — and the closest thing to
progress reporting anywhere is `rekey.go`'s `ProgressFunc`. A 200 GB media tree
does not fit in a request, and the honest answer is to scope v1 so it does not
have to:

- Import takes an explicit list of selected files.
- It runs synchronously and returns per-file `results[]`, exactly as
  `handleFilesUpload` already does, so a partial import is legible rather than
  mysterious.
- What was already imported is skipped.

The background-job version is a real v2 and deserves to be designed on its own
merits, not smuggled in under this feature.

---

## Copy, or track?

A genuine fork, and worth deciding deliberately. `internal/git` chose *track and
refresh*: a remote you keep a copy of and re-pull on demand, listed under
`/api/git`.

Recommendation: **one-shot copy for v1.** The tracked-directory version — "keep
this VPS folder mirrored into this vault folder" — reuses the scheduled-folder
machinery already behind `/api/automation` almost directly, so it is a clean
follow-on rather than a rewrite. Shipping the pull first also answers whether
anybody wants the mirror.

---

## Sending the other way

Export — a vault folder written back out to a VPS path — is nearly free once
the client exists: reverse the stream. It is deliberately *not* in v1, for one
reason worth stating loudly:

**Export writes plaintext onto the VPS.** The backend half is careful that the
far end only ever holds opaque shards; export is the direction that undoes that.
That asymmetry has to be visible in the UI, not merely true in the code.

---

## One VPS is one party

If the same box ends up as both a backend and an import source, two entries in
the UI must not become two independent accounts in the shard-spreading maths.

A vault's guarantee is that no single account holds anything meaningful, and
that is only as good as the independence of the accounts. One provider, one
card, one abuse desk, one subpoena. Two shards of the same chunk on one VPS
means that VPS alone rebuilds the file, and the guarantee is gone. This is the
same argument `cloud-backends.md` makes about three buckets at the same company
being one account wearing three hats — it just bites harder here, because
nothing stops you connecting the same host twice.

---

## What is built

```
internal/sftp/              dial, host-key policy (TOFU + pinning), key parsing,
                            connection pooling, path confinement, browsing
internal/sftp/sftptest/     an in-process sshd, so the code that talks to a
                            server is tested against a server
internal/provider/sftp.go   KindSFTP: Put/Get/Stat/Delete/List/Ping,
                            CredentialRotator, UsageReporter
internal/vault/source.go    a machine to import from: stored, connected, browsed
internal/vault/importsource.go   planning a selection and pulling it in
internal/server/handlers_remote.go   the five endpoints above
web/src/components/ImportFromMachine.jsx   connect, browse, pick, import
```

Notes on the backend that are not obvious from the interface:

- **Writes are atomic.** A shard goes to `.sand-tmp-<random>` in its
  destination directory and is renamed into place. This matters more than it
  does on local disk: what interrupts a network write is a dropped connection,
  which is common, rather than a crash, which is not. The rename prefers
  `posix-rename@openssh.com`, which is atomic and overwrites; SFTP v3's own
  rename leaves overwriting up to the server and OpenSSH refuses it. Servers
  without the extension fall back to remove-then-rename.
- **`List` hides `.sand-tmp-*`.** A half-written shard is not an object, and
  listing it would have the recovery scan try to read it as one.
- **Temp names are random, not counted.** Two SAND instances may share a folder
  — a laptop and a Pi pointed at the same box — and a collision would have one
  renaming the other's half-written file into place.
- **`Usage` is real.** OpenSSH ships `statvfs@openssh.com`, which is one round
  trip against the server's own bookkeeping, so this is a `UsageReporter` taken
  on every ping rather than a `UsageMeasurer` counted by listing. Free space
  comes from the non-root block count: a filesystem reserve SAND cannot spend is
  not free space to SAND. Servers without the extension simply draw no bar.
- **Connections are pooled.** A scatter writes every shard of a chunk at once,
  and an SSH handshake is a round trip plus a key exchange. One session carries
  the lot, and is re-dialled when it dies.
- **The vault now closes providers that hold something.** `resetLiveCache` and
  `forgetProvider` call `Close` on any backend implementing `io.Closer`.
  Nothing needed it before — HTTP backends share a transport — but an SSH
  session left open sits on the far end until sshd's own timeout notices, and a
  vault locked and unlocked a few times would leave a trail of them.

---

## The import half, as built

```
GET    /api/remote                    configured sources
POST   /api/remote                    add one (host, user, key, root, fingerprint)
PATCH  /api/remote/{id}               edit one
DELETE /api/remote/{id}               forget one
GET    /api/remote/{id}/files?path=   one directory
POST   /api/remote/{id}/import        {paths[], dest, accounts, scheme} → results[]
```

The seam that makes the import itself small was already there:

```go
// internal/vault/stream.go
func (v *Vault) UploadStream(ctx, scope, dir, name string, r io.Reader, opts UploadOptions)
```

An SFTP file handle **is** an `io.Reader`. So an import is: open the remote
file, hand the reader to `UploadStream`, done — compressed, split, encrypted and
scattered by the code that already does it. Bytes go VPS → SAND → clouds.
They never touch the browser, never hit `MaxUploadSize`, and never spool through
a multipart parser.

Notes on what the implementation settled that the design left open:

- **Sources live in the sealed settings section**, not one of their own. That
  section is the part of the vault that is *not* replicated to the connected
  accounts, which is exactly right for an SSH private key: the argument the
  film-database key is kept there for applies with more force to a key that
  opens a shell. The consequence, stated in the code and worth repeating: a
  source does not travel in a recovery kit. A rebuilt install reconnects its
  accounts and has to be told about its sources again — which is the right
  trade, since a kit exists to restore access to the data and a source holds
  none of it.
- **Symlinks needed more than a path check.** `Under` is lexical: it stops
  `../..` and cannot stop `ln -s / everything`, because the escape is not
  written in the path. So `(*sftp.Client).resolve` walks every component below
  the root, follows a link only if its target lands back inside, and caps the
  hops. That walk is done in SAND rather than left to the server's own
  `SSH_FXP_REALPATH`, because the spec is loose enough that servers differ on
  whether they resolve links at all — and a boundary that holds against some
  servers is not a boundary. The test that caught this is
  `TestReadDirWillNotFollowALinkOutOfTheRoot`.
- **A link out of the root is listed, not hidden**, with a reason attached and
  no checkbox. A directory that appears to hold fewer files than `ls` shows
  reads as a bug rather than as a rule.
- **The host-key pin is not cleared by an ordinary edit.** An edit form that
  does not mention the fingerprint is not asking to trust a stranger, so
  `UpdateSource` takes a separate `relearnHostKey` argument. Forgetting a pin
  is the one edit here that weakens something, which makes it worth being
  unable to do by accident.
- **Connections are not pooled on the source side**, which is the opposite of
  the backend and not an inconsistency: a scatter writes every shard of a chunk
  at once and would pay a handshake per shard, while browsing is one round trip
  a click and an import is one long request that dials once and reads every
  file over the same session. Neither wants a connection held between requests.
- **The skip is sound, not a heuristic.** `UploadStream` spools, scatters and
  only then commits, so an interrupted import leaves *no* entry rather than
  half of one — the vault holds the complete file or nothing. A size match at
  the destination is therefore a real answer, with the source's modification
  time as the guard on the one case size alone gets wrong: a file replaced by a
  different file of the same length.

Build order:

1. ~~client + host keys~~ — done
2. ~~`KindSFTP` backend~~ — done
3. ~~browse~~ — done
4. ~~import~~ — done
5. export — still open, and still the direction that writes plaintext

---

## Setting up the far end

Nothing needs installing, but three things are worth doing on the server:

```bash
# A key of its own, not the one you log in with.
ssh-keygen -t ed25519 -f ~/.ssh/sand -C sand-vault

# A user of its own, with a home SAND can write into and no shell.
sudo useradd -m -s /usr/sbin/nologin sand
sudo -u sand mkdir -p ~sand/.ssh && sudo -u sand tee -a ~sand/.ssh/authorized_keys < ~/.ssh/sand.pub

# The fingerprint to paste into the connect form, so even the first
# connection is checked.
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

Worth considering beyond that: put the box on a **WireGuard** network and have
sshd listen only on the tunnel interface. Nothing is then exposed to the public
internet at all, there is no certificate story, and a port scan finds a closed
machine. It is more setup per client and a dramatically smaller attack surface.

And the point that survives all of it: **transport encryption protects the wire,
not the disk.** Your hosting provider can image the volume. The reason that does
not matter is that shards are encrypted before they leave your machine, and this
package only ever sees opaque blobs — the far end is untrusted by construction,
which is the guarantee actually worth advertising.
