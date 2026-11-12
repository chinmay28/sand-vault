# SAND TV: watching a folder of films on a Fire TV

SAND TV puts one folder of the vault on a television, with its posters, synopses
and cast, and plays the films straight off the machine running SAND on your
network. It is in the spirit of Plex or Jellyfin, with one difference that runs
all the way through it: the TV is never given the vault. It is given **one
folder, for a while**, by a four-character code.

```
 laptop (signed in)                        Fire TV on the same network
 ──────────────────                        ───────────────────────────
 folder ⋯ → Watch on TV                    SAND TV app
   → "Good for 4 hours"  ──── K7PX ────▶     finds SAND by itself (UDP 8124)
                                             you key in K7PX with the remote
                                             ▸ posters, details, ▶ Play
```

## Setting it up

1. **Get the app onto the Fire TV.** It is not in the Amazon Appstore, so it is
   sideloaded:
   - Build it with `cd apps/firetv && ./gradlew assembleRelease` (needs the
     Android SDK), or download the `sand-tv-apk` artifact from the latest run
     of the **SAND TV (Fire TV app)** workflow on GitHub.
   - On the Fire TV: *Settings → My Fire TV → Developer options* → turn on
     **Apps from Unknown Sources** (on newer Fire OS, *Install unknown apps*,
     for the app you will install with). If Developer options is missing, open
     *Settings → My Fire TV → About* and press Select on the device name seven
     times.
   - Install the APK with Amazon's **Downloader** app (give it the APK's URL),
     or over the network with `adb connect <tv-ip> && adb install sand-tv.apk`.
2. **Have SAND listening on the network.** The quickstart service already does
   (`--bind 0.0.0.0` is the default). A `sand serve --bind 127.0.0.1` is
   reachable from nowhere but its own machine, and the dialog says so.
3. **Make a code.** In the browser, open a folder's menu (⋯) and choose
   **Watch on TV**. Pick how long it lasts — an hour to thirty days — and
   **Make a code**.
4. **Pair the TV.** Open SAND TV. It lists the SAND servers that answered on the
   network; pick yours (or **Enter address** and type the one the dialog shows).
   Key the four characters in with the remote, and the folder opens.

No Fire TV? Any TV or console with a browser can open `http://<sand-host>:8123/tv/`
and pair the same way. Films play in the browser's own player there, which
handles MP4/H.264 and WebM but usually not MKV, HEVC or surround sound — the
app's native player does.

## What the TV can and cannot do

The TV can:

- browse the folder and the folders under it that hold videos, or every video
  at once under **All films**;
- see what film lookup stored for each video — poster, year, runtime, rating,
  genres, tagline, synopsis, director and cast (turn **film lookup** on for the
  folder in the browser first, and run it, for there to be anything to see);
- play a video, seek anywhere in it, and resume where it stopped.

The TV cannot:

- see any file that is not a video, or anything outside the folder — every
  path it sends is resolved beneath the folder, and a file ID from elsewhere in
  the vault is answered as not found;
- learn where in the vault the folder is: paths it is sent start at the folder;
- change anything: no uploads, no deletes, no settings, no film lookups;
- unlock the vault. If SAND locks itself while nobody is using it, the TV says
  so and waits; unlock in the browser and it carries on, still paired.

## Ending it

A code ends when its time is up, when you **Revoke** it in the Watch on TV
dialog (which lists every live code and how many TVs used it), or when SAND
restarts — codes are only ever held in memory. Revoking a code unpairs every TV
that used it **and stops a film that is playing on one**: the stream link the
player is following is bound to the code. **Sign out of this TV** in the app's
⋯ menu unpairs just that TV.

## How the four characters stay safe

Four characters from a 32-letter alphabet (no `0`/`O`, no `1`/`I`) is about a
million codes, which is only safe if guessing is slow. So pairing is throttled:
after **5 wrong codes from one address, or 30 from anywhere, in ten minutes**,
every pairing attempt is refused — the right code too — until the window slides
past. At that ceiling somebody already on your network who spends a whole day
guessing has well under a one-in-two-hundred chance of hitting a code that is
live for that day. The cost is that a guesser can keep a real TV from pairing for
ten minutes; that is the right way round.

The code is traded once for a 256-bit device token, which is what the TV keeps
and sends (`Authorization: Bearer …`) from then on. The server stores only its
SHA-256. At most 32 codes can be live, and one code pairs at most 8 TVs.

On a home network the films cross the LAN as plain HTTP, exactly as they would
to a browser on the LAN; a reverse proxy with TLS in front of SAND covers the TV
too (point the app at `https://…`).

## Discovery

`sand serve` answers a fixed UDP query (`SAND-TV-DISCOVER/1`) on port **8124**
with its name, web port and version. It only answers the exact query, only from
private, loopback, link-local or CGNAT (Tailscale) addresses, and is not started
at all on a loopback bind. It authorizes nothing. `--tv-discovery-port 0` turns
it off; a firewall needs UDP 8124 open beside TCP 8123 for it to work.

## The app

`apps/firetv` is a small Kotlin app with three parts:

| Part | What it does |
|---|---|
| `MainActivity` | A full-screen WebView around `/tv/` on the chosen server — the screens are the server's, so they update with SAND, not with the app. Navigation is held to that server and the app's own pages. |
| `Discovery` | Broadcasts the query on every interface and collects answers for two seconds. |
| `PlayerActivity` | Media3 ExoPlayer on the stream link, using the Fire TV's hardware decoders: MKV, MP4, HEVC, AC-3/E-AC-3 and DTS passthrough, embedded subtitles. Reports where it stopped so the page can offer to resume. |

The page and the app talk through `window.SandTVNative` (`play`, `discover`,
`connect`, `changeServer`, `savedServer`) and `window.sandTV` (`back`,
`onPlaybackStopped`) — see the header of `internal/server/tvui/tv.js`.

## API

Owner, behind the browser session:

| Route | |
|---|---|
| `POST /api/tv/codes` `{path, vault?, minutes?, label?}` | mint a code (default 240 minutes; 5 minutes to 30 days) |
| `GET /api/tv/codes` | live codes, LAN addresses, discovery port |
| `DELETE /api/tv/codes/{code}` | revoke |

TV, with `Authorization: Bearer <token>`:

| Route | |
|---|---|
| `POST /api/tv/pair` `{code}` | → `{token, label, expires_at}`; no token needed; 429 with `Retry-After` when throttled |
| `GET /api/tv/session` | label, expiry, whether the vault is locked (does not count as use) |
| `GET /api/tv/library?path=/sub&all=1` | folders with videos beneath them, and the videos, relative to the grant |
| `GET /api/tv/items/{id}` | one video with its film details |
| `GET /api/tv/items/{id}/poster` | the poster JPEG |
| `POST /api/tv/items/{id}/play` | → a `/stream/…` link bound to the code |
| `POST /api/tv/logout` | unpair this TV |

`401 TV_UNPAIRED` means pair again; `401 LOCKED` means the vault is locked.
