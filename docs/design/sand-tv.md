# SAND TV design

How SAND TV works: the grant a four-character code stands for, why four
characters are safe, how a TV finds the server, how the app is put together, and
the API between them. Installing and using it is in the
[SAND TV guide](../guide/sand-tv.md).

## Shape

```
 browser (session)              sand serve                         Fire TV
 ─────────────────              ──────────                         ───────
 POST /api/tv/codes ──────────▶ tvaccess.Store (memory only)
   {path, minutes}               grant: code → folder, scope, expiry
                       ◀──────── "K7PX"
                                tvdiscover on UDP 8124 ◀──────── broadcast SAND-TV-DISCOVER/1
                                                       ────────▶ {service, name, port, version}
                                POST /api/tv/pair {code} ◀────── MainActivity (WebView on /tv/)
                                  → 256-bit device token ──────▶ kept in the page's localStorage
                                /api/tv/library, items, poster ◀── Bearer token
                                POST /api/tv/items/{id}/play ◀──
                                  → /stream/<ticket bound to K7PX> ─▶ PlayerActivity (Media3)
```

The rule that runs through all of it: **the TV is never given the vault.** A TV
has a remote with arrows on it. Typing the vault password with it would be
miserable, and handing it the password would give it every folder, every sub
vault's door, and the settings that delete files. What it wants is narrower, the
films in one folder for the evening, so that is all it gets.

## Codes and grants (`internal/tvaccess`)

The owner, signed in on a computer, asks for a code for a folder (in the main
vault or an open sub vault) and a length of time. The server keeps a **grant**:
the code, the folder, the vault scope, a label, and an expiry.

| Limit | Value | Source |
|---|---|---|
| Alphabet | `ABCDEFGHJKLMNPQRSTUVWXYZ23456789` (32 letters, no `0`/`O`, `1`/`I`) | `tvaccess.Alphabet` |
| Length | 4 characters, about a million codes | `CodeLength` |
| Lifetime | 5 minutes to 365 days, default 4 hours, or `forever` | `MinTTL`, `MaxTTL`, `DefaultTTL` |
| Live codes | At most 32 | `MaxGrants` |
| TVs per code | At most 8 | `MaxDevicesPerGrant` |

**Pairing** trades the code once for a 32-byte random **device token**, which the
TV sends as `Authorization: Bearer …` from then on. The server stores only the
token's SHA-256. The token is good for exactly what the code was good for: that
folder, until the code expires or is revoked.

**Everything is in memory.** A code that outlived the process would outlive the
decision to hand it out. Restarting the server is how an owner who has lost track
of what they shared starts over. A forever code therefore means "until revoked or
restarted".

### Why four characters are safe

A million codes is safe only if guessing is slow, so `Redeem` counts failures per
client address and across all clients (`DefaultThrottle`: **5 per client, 30
overall, in a sliding 10-minute window**). Past either limit, every pairing
attempt is refused, including the right code, with `429` and `Retry-After`, until
the window slides past.

At the global ceiling, an attacker on your network guessing all day gets about
4,320 tries, against 32⁴ ≈ 1.05 million codes: well under a one-in-two-hundred
chance of hitting a code that is live that day. The cost is that a guesser can
stop a real TV from pairing for ten minutes, which is the right way round.

### What a token can reach

- **Only videos under the folder.** Every path a TV sends is resolved beneath the
  grant's folder (`confine`), and paths sent back start at the folder, so the TV
  never learns where it sits in the vault. A file ID outside the folder, in
  another vault, or not a video is answered **not found** rather than forbidden,
  so a TV cannot probe for IDs that exist elsewhere.
- **Sidecar subtitles, and nothing else.** When a video is played, the server
  looks among its siblings for a subtitle file whose name is the video's name,
  then optionally a dot and a tag, then `.srt`, `.vtt`, `.ass` or `.ssa`
  (`tv_subtitles.go`). Each gets its own stream link. No other file in the folder
  is reachable.
- **Nothing that writes.** There are no TV routes for uploads, deletes, settings
  or film lookups.
- **No unlocking.** `requireTV` checks the token but not the lock, so
  `GET /api/tv/session` can still answer `locked: true` and the TV can say so.
  Everything that reads the vault is refused by the vault itself as
  `401 LOCKED`.

### Activity and the auto-lock

A TV has no browser session, so TV requests that read the library count as use of
the vault (`tvActive`), and the vault does not lock while someone browses for a
film. `GET /api/tv/session` is the exception: it is what the TV polls while the
vault is locked, so it must not keep anything open. Streams follow the ordinary
stream rules (see [Server design](server.md)): a playing film counts as use and a
paused one does not.

### Revocation reaches a playing film

A stream link minted for a TV carries the code it was minted under
(`streamTicket.grant`). Each request on that link, and the running stream itself,
checks `tv.Live(code)`. Revoking a code therefore stops a film already playing,
not only the next one. Locking the vault voids every stream link as usual, but
leaves pairings alone, so the TV resumes once the vault is unlocked.

## Discovery (`internal/tvdiscover`)

Typing `192.168.1.20:8123` with a D-pad is the step that would make pairing a
chore, so the app broadcasts instead.

- **Query:** the exact bytes `SAND-TV-DISCOVER/1`, sent to UDP port **8124** (the
  web port plus one, so the pair are easy to open together).
- **Answer:** JSON `{service: "sand-vault", name, port, secure?, version}`. The
  name is the machine's hostname. The address is wherever the answer came from.
- **Limits:** it answers only the exact query, so stray packets are not echoed,
  and only from private, loopback, link-local or CGNAT (`100.64.0.0/10`, used by
  Tailscale) addresses, so a server on a public address cannot be used to reflect
  traffic. It is not started at all on a loopback bind.
- **Authorizes nothing.** It says only what a port scan would: that a server is
  here. Pairing still needs a code.

`sand serve --tv-discovery-port 0` turns it off. A port that will not open is
logged and nothing more; the TV can still be pointed at the address by hand.

On the app side, `Discovery.find()` broadcasts on every interface's broadcast
address plus the limited broadcast, collects answers for two seconds, and holds a
Wi-Fi multicast lock, because some drivers drop broadcast replies without one.

## The TV page (`internal/server/tvui`)

The screens are the server's, served at `/tv/` (`tvui.go`): plain ES2017 with no
build step, embedded as it stands, because it runs on whatever browser engine a TV
has. It is served with a strict CSP (`script-src 'self'`, `connect-src 'self'`,
`frame-ancestors 'none'`) and `Cache-Control: no-cache`, so a TV picks up new
screens on its first load after an upgrade.

Because the screens come from the server, they update with SAND Vault rather than
with the app. The page holds the device token and each film's resume position in
`localStorage`. Position is the TV's own memory of what was watched; a film past
92% counts as watched. Without the app, films play in the page's `<video>`.

## The app (`apps/firetv`)

A small Kotlin app (`com.sandvault.tv`, minSdk 22 for Fire OS 5, targetSdk 34),
listed by the Fire TV launcher through `LEANBACK_LAUNCHER`.

| File | Role |
|---|---|
| `MainActivity.kt` | A full-screen WebView. Before there is a server it shows the bundled `assets/connect.html`, which lists discovered servers and takes a typed address; afterwards it shows `/tv/` on the chosen server. Navigation is held to that server and the app's own assets, because whatever page is showing holds the bridge. Back is offered to the page first. |
| `ServerAddress.kt` | Normalizes what was typed (`192.168.1.20`, `host:8123`, `http://host:8123/tv/`) to one origin; http and port 8123 unless given. |
| `Discovery.kt` | The broadcast described above. |
| `PlayerActivity.kt` | Media3 ExoPlayer on the stream link, with its own TV controls. Reports where it stopped when it closes. |
| `PlayerWidgets.kt` | The controls: pills, menus, the seek bar, the Stats overlay. Drawn instead of Media3's, which are laid out edge to edge for a phone and ran off a TV's cropped edge. |
| `PlayerText.kt` | Track labels, codec names, times, and the seek acceleration (10s, 30s, 1m, 3m, 5m as a key is held). |
| `PlaybackStats.kt` | Works out what is limiting playback (below). |
| `StatsText.kt` | Words the Stats panel. |
| `NetworkLink.kt` | Wi-Fi band, link rate and signal, or Ethernet rate, asked the old way that Fire OS 5 also answers. |
| `AppInfo.kt` | The app's version name, as the release stamped it. |

`PlaybackStats`, `StatsText`, `PlayerText` and `ServerAddress` are plain Kotlin
with unit tests of their own.

**Network security.** `network_security_config.xml` permits cleartext, because a
home server is usually plain HTTP on a private address, for which no certificate
can be had. What crosses the LAN is the same stream a browser on the LAN would
get. An `https://` address works when there is a TLS proxy.

### The bridge

`MainActivity.Bridge` is exposed to both pages as `window.SandTVNative`:

| Method | Used by | Does |
|---|---|---|
| `playWith(json)` | TV page | Open the native player with `{url, title, startMs, itemId, subtitles}` |
| `play(url, title, startMs, itemId)` | TV page | The same, without subtitles; kept for older pages |
| `changeServer()` | TV page | Forget the server and go back to the connect page |
| `appVersion()` | both | The app's version, shown in the header, the About panel and Info |
| `discover()` | connect page | Run discovery and report the servers found |
| `connect(typed)` | connect page | Check and save an address, then open its `/tv/` |
| `savedServer()` | connect page | The server last used |

The app calls back into the page through `window.sandTV`: `back()` returns true
if the page handled Back, and `onPlaybackStopped(itemId, posMs, durMs)` lets the
page offer to resume. The connect page gets its answers through
`window.sandConnect`: `onDiscovered` and `onConnectFailed(message)`. (The comment block at the top of `tv.js` lists only `play`
and `changeServer`; the table above is the full set.)

### Player internals

- **Renderers.** `DefaultRenderersFactory` with `EXTENSION_RENDERER_MODE_ON` and
  decoder fallback: the TV's own decoders and HDMI passthrough first, then
  Jellyfin's FFmpeg decoder (`org.jellyfin.media3:media3-ffmpeg-decoder`) for
  audio they cannot do: DTS, TrueHD, and AC-3/E-AC-3 on sticks without them.
  FFmpeg is audio only, since its video renderer is an unfinished stub, so video
  is always the hardware's. Media3 is pinned to 1.7.1, matched to the decoder
  build.
- **ABIs.** `armeabi-v7a` and `arm64-v8a` only; Fire TVs are ARM.
- **Buffer.** The load control and media source are Media3's defaults, built
  explicitly only so the Stats panel can see into them. The default buffer is
  about 144 MB. A `TransferListener` counts the film's bytes, but not the
  subtitles', as they arrive.
- **Remembered settings.** Zoom, subtitle scale and the last audio and text
  languages are kept in the app's preferences and applied to the next film.

### How Stats decides

`PlaybackStats` samples the player every 500 ms and judges over the last 10
seconds:

| Verdict | Condition |
|---|---|
| Measuring | Less than 3 s seen since opening or seeking |
| TV, video | At least 5 s buffered, not waiting, and at least 5 frames dropped and ≥ 2% of frames rendered |
| TV, audio | At least 5 s buffered, not waiting, and any audio underrun |
| Network | Fetching ≥ 80% of the time and film arriving below 97% of the playing speed; or waiting for data while fetching flat out |
| Keeping up | Anything else |

The key figure is **arrival**: seconds of film buffered per second spent
fetching. The film's bitrate is not known in advance (an MKV does not declare
one), so it is measured as bytes per second of film buffered. A full buffer stops
fetching, so only time spent fetching says anything about the link. Decoder
verdicts require a cushion because frames dropped while waiting for data are the
network's fault. The panel cannot tell Wi-Fi from the PC's uplink or the PC
itself; [Streaming performance](streaming-performance.md) has the checks that
can.

### Tested containers

`ContainerSupportTest` runs the `sniff()` of each of Media3's container extractors
over a one-second ffmpeg clip per container (`app/src/test/resources/containers`)
and asserts which extractor claims it:

| Fixture | Claimed by |
|---|---|
| mp4, m4v, mov, 3gp | `Mp4Extractor` |
| mkv, webm | `MatroskaExtractor` |
| avi | `AviExtractor` |
| ts | `TsExtractor` |
| mpg (MPEG-2 PS), vob | `PsExtractor` |
| flv | `FlvExtractor` |
| ogv, wmv, m2ts, MPEG-1 `.mpg` | nothing, so the player cannot open them |

An MPEG-1 `.mpg` has the same extension as a playable MPEG-2 one, so the TV page
cannot warn about it before Play; the player reports it instead. The page's own
pre-Play warning (`UNPLAYABLE` in `tv.js`) also covers ISO and RealMedia, which
have no fixture.

### Builds

| Workflow | When | What |
|---|---|---|
| `.github/workflows/firetv.yml` | PRs and pushes to `main` touching `apps/firetv/**` or `internal/tvdiscover/**` | Unit tests (`testDebugUnitTest`, printing the container matrix), `lintDebug`, `assembleRelease`, a check that `libffmpegJNI.so` is in the APK for both ABIs (an APK without it builds fine and plays DTS in silence), and the APK uploaded as the `sand-tv-apk` artifact |
| `.github/workflows/release.yml` | Each release | Builds with `-PsandVersionName=<version>` and `-PsandVersionCode=<commit count>`, so each release installs over the last, and attaches it as `sand-tv.apk`: a fixed name, so `…/releases/latest/download/sand-tv.apk` is short enough to type into Downloader |

The release build is signed with the debug key so it sideloads as it stands.
Publishing to a store would need your own key. A local build is version `dev`,
code 1.

## API

Owner routes, behind the browser session:

| Route | |
|---|---|
| `POST /api/tv/codes` `{path, vault?, minutes?, forever?, label?}` | Mint a code. Default 240 minutes; 5 minutes to 365 days. `forever: true` with no `minutes` gives a code with no end, which carries `forever: true` and no `expires_at`. |
| `GET /api/tv/codes` | `{codes, addresses, discovery_port, lan}`: the live codes, the LAN addresses a TV could be pointed at, and `lan: false` on a loopback bind |
| `DELETE /api/tv/codes/{code}` | Revoke |

TV routes, with `Authorization: Bearer <token>` (except pairing):

| Route | |
|---|---|
| `POST /api/tv/pair` `{code}` | `{token, label, expires_at}`, or `forever: true` in place of `expires_at`. `429` with `Retry-After` when throttled. |
| `GET /api/tv/session` | Label, expiry, and `locked`. Does not count as use. |
| `GET /api/tv/library?path=/sub&all=1` | Folders with videos beneath them, and the videos, relative to the grant. `all=1` lists every video below. |
| `GET /api/tv/items/{id}` | One video with its film details |
| `GET /api/tv/items/{id}/poster` | The poster JPEG |
| `POST /api/tv/items/{id}/play` | A `/stream/…` link bound to the code, and `subtitles: [{url, label, language, mime}]` |
| `GET /api/tv/folders/art?path=/sub` | A folder's picture |
| `POST /api/tv/logout` | Unpair this TV |
| `GET /tv/` | The TV page |

`401 TV_UNPAIRED` means pair again (no token, or its code ended).
`401 LOCKED` means the vault is locked. The full route list is in the
[API reference](../reference/api.md).
