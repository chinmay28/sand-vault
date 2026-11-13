# Security

What Sand Vault protects against, what it doesn't, and what you take on by
exposing the server. The mechanisms are described in
[design/cryptography.md](design/cryptography.md),
[design/placement.md](design/placement.md) and
[design/server.md](design/server.md).

In the tables below a file is cut **k-of-n**: n shards on n different accounts,
any k of which rebuild it. The default is 2-of-3. See
[design/placement.md](design/placement.md#schemes-and-how-wide-a-file-is-spread).

## Crypto in one paragraph

Every stored shard is sealed with AES-256-GCM, with its cleartext header (archive
ID, shard number, chunk index, k and n) as associated data. Shard keys come from
a random 256-bit **data key** through HKDF-SHA256, salted per file and separated
per chunk. The data key is wrapped by your password through Argon2id (64 MiB, 3
passes, 4 threads), so part encryption doesn't inherit a weak password. Changing
the password mints a new data key and re-encrypts every file onto it. Re-wrapping
the old key would leave every part readable to anyone with the old password and
an old copy of the vault file. The index (folder tree, placement map, account
credentials) lives in the vault file, every section of which is sealed under
the password. Object keys on the clouds are derived only from a random archive
ID.

## Threat model

| Threat | Mitigation |
|---|---|
| **One cloud account compromised** | The attacker holds one shard per file (under `strict`, placement guarantees it), plus a copy of the index backup they can't open. A shard on its own is ciphertext of 1/k of a file's compressed bytes |
| **One account compromised *and* the password guessed** | The index backup opens: the tree, the placement map, the data key, and the 1/k slice of each file that account holds. A whole file still needs k accounts |
| **k accounts compromised** | Enough shards, but no key. That needs the vault file, or an index backup *and* the password |
| **Vault file stolen** | Every section is sealed under an Argon2id key. It yields neither cloud credentials nor filenames |
| **A provider tampers with a shard** | The GCM tag fails, the shard is treated as lost, and any k of the others rebuild the file |
| **Shards swapped between files, renumbered, or replayed at another offset** | The header, including the chunk index, is bound as associated data |
| **A provider reading your filenames** | Names, hashes and sizes are sealed inside each shard (since format version 2), not in its header |
| **Silent bit rot** | The whole-file SHA-256 is checked on every full rebuild. `sand check --deep` reads every shard and finds damage before you need the file |
| **Accounts disappear** | Any k shards suffice, so n − k accounts can go. A background health check notices an account that stops answering |
| **Brute force on the vault file** | Argon2id, 64 MiB, 3 passes. Kits and snapshots use 512 MiB and 8 passes |
| **Another site in your browser** | `SameSite=Strict`, `HttpOnly` session cookie, plus an `Origin` check on every write (`403 CROSS_ORIGIN`) |
| **Stored HTML or SVG executing in the app** | Risky types forced to `attachment`, `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'; sandbox` on file content |
| **Plaintext cached by a proxy** | `Cache-Control: private, no-store` on rebuilt content |
| **Walking away from the machine** | The vault re-locks after the idle timeout (30 minutes by default), which zeroes the keys, drops the decrypted chunk cache, and voids every session and stream link |
| **A stream link leaking** | It plays one file only, lives in memory only, and dies when the vault locks |
| **Metadata leaking to a provider** | Object keys are derived only from a random ID. A provider learns how many objects you store and how big they are |
| **Third-party tracking** | The UI loads no external fonts, scripts or styles. Opening the vault makes no third-party requests |
| **A film database learning your library** | Off until a folder is opted in by name. One lookup per film, carrying a title guessed from the file name and nothing else about the file. The answer is stored so it is never asked twice |

## What it does not protect against

- **A compromised machine.** The machine running `sand serve` sees plaintext and
  holds the keys while unlocked. Malware or a keylogger on it defeats everything
  here. While a stream upload is in progress, its plaintext also sits briefly in
  a spool file beside the vault.
- **Weak passwords.** Argon2id slows a guess. It doesn't fix `hunter2`.
- **k providers *and* your password.** That is the documented recovery path, and
  it is also the attack. The model assumes the accounts are independent: two
  buckets in one AWS account, or three services you sign into with the same
  Google account, are one party wearing several hats.
- **Losing the password.** There is no recovery. Neither the vault file nor any
  index backup can be decrypted, and the parts on your accounts stay
  meaningless. A [recovery kit](design/recovery-kit.md) is the way to plan for
  this, and it is itself a thing to keep safe.
- **The index backup itself.** Replicating the index to your accounts is what
  makes a lost vault file survivable, and it is also a new thing to steal.
  `sand vault backup --disable` erases every copy and puts you back to "the vault
  file is the only way in", with everything that implies if you lose it. Once
  film details are on, the backup carries the matched titles, which add nothing
  an attacker who could read `Some.Film.1982…mkv` from the same index didn't
  already have.
- **A whole-copy replica.** A replica on a single account is the whole sealed
  file on one cloud (`sand replicate set` with one `--cloud`). That account plus
  the data key yields the whole file, not 1/k of it. It is allowed only because
  you chose it, and you are told so.
- **The `redundant` policy.** With fewer accounts than shards it doubles up, so
  one account can hold k shards.
- **Traffic analysis.** A provider sees when you upload and how big each shard is.
- **A film database knowing what it was asked.** Storing the answer in the vault
  isn't the same as the question never being asked. TMDB sees a title guessed
  from a file name, and your address, once per film, for the folders you opted
  in. Nothing about the file goes with it, and nothing goes at all for a folder
  that never asked. See [guide/films.md](guide/films.md).
- **The assistant's model server.** [Sandy](guide/sandy.md) sends file and film
  names from your index to the chat model you configure. It is meant to be a
  model you run yourself on your own network. Pointing it elsewhere sends those
  names there.
- **Exports and imports over SFTP.** Sending files to a machine writes plaintext
  there, deliberately. See [design/sftp.md](design/sftp.md).

## Binding and exposure

`--bind` defaults to `0.0.0.0`, so a fresh install is reachable from other
machines on your network without a second step. That is a deliberate usability
choice with a real cost:

- Off loopback, **the vault password and every rebuilt file cross the network
  unencrypted** unless something in front of the server adds TLS.
- `POST /api/vault/unlock` is reachable without a session and is **not rate
  limited**. Only Argon2id's cost stands between an attacker on your network and
  a guessing loop.
- The **WebDAV share** (`--webdav`, off by default) carries the password on every
  request, so on a plain listener it is in the clear far more often than the
  browser's single sign-in. A correct password against a locked vault unlocks it.
- **Stream and zip links** are bearer credentials in a URL. Anyone who sees the
  link can fetch that one file or folder until it expires or the vault locks.
- **TV discovery** answers a UDP broadcast on the local network (port set by
  `--tv-discovery-port`, `0` turns it off). It authorizes nothing; pairing still
  needs a short code shown in the browser. See [design/sand-tv.md](design/sand-tv.md).

The server logs a warning on every non-loopback bind, and again when the WebDAV
share is on without TLS. To close the exposure:

- put TLS in front (`scripts/nginx-sand.conf`, or Tailscale Serve), or
- bind to loopback: `sand serve --bind 127.0.0.1`.

Deployment details are in [guide/deployment.md](guide/deployment.md).

## Hardening of the service unit

The systemd unit the installers write runs the server as its own user with
`ProtectHome=yes`, `PrivateTmp` and `PrivateDevices=true`. As a result:

- Disk SMART data is read by a separate root one-shot (`sand smart-scan`) rather
  than the network-facing service.
- Upload spools and conversions go beside the vault, not into a tmpfs `/tmp`.
- The SFTP backend owns host-key verification itself (trust on first use, or a
  fingerprint you paste), because there is no `~/.ssh/known_hosts` to consult.
  See [design/sftp.md](design/sftp.md#host-keys-which-is-the-part-to-get-right).
