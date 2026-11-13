# SAND TV

SAND TV puts one folder of your vault on a television, with its posters, synopses
and cast, and plays the films straight off the machine running SAND Vault. It is
in the spirit of Plex or Jellyfin, except that the TV is never given the vault: it
gets **one folder, for a while**, through a four-character code. This page covers
installing the app, pairing, the player and its Stats panel, what plays, and
ending a pairing.

How it works inside (codes, throttling, discovery, the app's structure and the
API it uses) is in [SAND TV design](../design/sand-tv.md).

```
 laptop (signed in)                        Fire TV on the same network
 ──────────────────                        ───────────────────────────
 folder ⋯ → Watch on TV                    SAND TV app
   → "4 hours"  ─────────── K7PX ────▶       finds the server by itself (UDP 8124)
                                             you key in K7PX with the remote
                                             ▸ posters, details, ▶ Play
```

## Install the app

The app is not in the Amazon Appstore, so it is sideloaded. Every
[release](https://github.com/chinmay28/SandHarbor/releases) attaches it as
`sand-tv.apk`, and the latest is always at:

```
https://github.com/chinmay28/SandHarbor/releases/latest/download/sand-tv.apk
```

1. **Allow sideloading.** On the Fire TV, go to *Settings → My Fire TV →
   Developer options* and turn on **Apps from Unknown Sources**. On newer Fire OS
   this is **Install unknown apps**, set for the app you install with. If
   Developer options is missing, open *Settings → My Fire TV → About* and press
   Select on the device name seven times.
2. **Install Amazon's Downloader app** from the Appstore.
3. **In Downloader, enter the URL above.** It downloads the APK and offers to
   install it. A newer release installs over the old one the same way, since each
   release's version code is higher than the last.

Or install over the network from a computer:

```bash
adb connect <tv-ip>
adb install -r sand-tv.apk
```

Or build it yourself (needs JDK 17 and the Android SDK):

```bash
cd apps/firetv && ./gradlew assembleRelease
# app/build/outputs/apk/release/*.apk
```

The release APK is signed with the debug key so it sideloads as it is. Update the
server and the app together: the app relies on the server's `/api/tv` routes.

## Make sure the TV can reach the server

- **The server must listen on the network.** The quickstart service already does
  (`--bind 0.0.0.0` is the default). A `sand serve --bind 127.0.0.1` is reachable
  only from its own machine, and the Watch on TV dialog says so.
- **Discovery** uses UDP port **8124** next to the web port 8123. A firewall needs
  both open for the TV to find the server by itself. Without discovery you can
  still type the address. `--tv-discovery-port` changes the port, and `0` turns
  discovery off.
- The server answers discovery only from private, loopback, link-local and
  Tailscale (CGNAT) addresses.

## Pair the TV

1. **Make a code.** In the browser, open a folder's `⋯` menu and choose **Watch
   on TV**. Under *Good for*, pick `1 hour`, `4 hours`, `1 day`, `1 week` or
   `30 days`; **Custom** for any length from five minutes to a year; or **No end**
   for a code that lasts until you revoke it or the server restarts. Then press
   **Make a code**.
2. **Open SAND TV** on the TV. *Find your SAND Vault* lists the servers that
   answered on the network. Pick yours, or choose **Enter address** and type the
   address the dialog shows (`192.168.1.20`, `192.168.1.20:8123` and
   `http://host:8123/tv/` all work).
3. **Key in the four characters** with the remote. The folder opens.

The TV remembers the server and its pairing. One code can pair up to 8 TVs.

## Using it

The library has three tabs:

- **Folders**: the folder and the folders under it that hold videos. A folder
  shows the picture picked for it in the browser, or the poster when it holds a
  single film.
- **All films**: every video under the folder at once.
- **⋯**: when the code runs out, an About panel with the app and server versions,
  **Sign out of this TV**, and **Change server**.

A film's page shows what film lookup stored: poster (blurred behind the page too),
year, runtime, rating, genres, tagline, synopsis, director and cast. Turn
[film details](films.md) on for the folder in the browser and run the lookup
first, or there is nothing to show beyond the file name. A long synopsis scrolls
in its own box.

The buttons are **▶ Play**, or **▶ Resume from …** and **Play from the start**
when you stopped partway, plus **Mark as not watched** for a film you finished.
Progress is kept on the TV, not on the server. Where the file's extension says the
player cannot open it (WMV, M2TS, Ogg video, ISO…), the page warns before Play.

Subtitle files beside a film are found and offered automatically: `Film.mkv`
claims `Film.srt`, `Film.en.srt` and `Film.English.forced.vtt`, and `.ass`/`.ssa`
too. Nothing else in the folder is reachable.

## The player

The player is drawn for a TV: everything sits inside the safe area (48dp at the
sides, 32dp top and bottom), because TVs crop the edge of the picture. If yours
also crops the film itself, turn on the TV's own *Screen Fit*, *Just Scan* or
*1:1 pixel* setting, depending on the make.

| Remote | Action |
|---|---|
| Select, Up, Down, Menu | Show the controls; Select presses the focused button |
| Left / Right | Seek 10s a press, speeding up while held (30s, 1m, 3m, 5m). The seek is sent when you stop. |
| Play/Pause | Play or pause, any time |
| ⏪ / ⏩ | Back 10s / forward 30s, any time |
| Back | Close a menu, then the controls, then the film |

The controls are **Play/Pause**, **−10s**, **+30s**, then:

| Menu | What it offers |
|---|---|
| **Audio** | Every track by language, codec and channels ("English · E-AC-3 5.1"). Tracks the TV cannot decode are marked. |
| **Subtitles** | Off, the film's own tracks, the sidecar files, and *Text size* (Small, Normal, Large, Extra large) |
| **Zoom** | Fit, Fill screen, Stretch |
| **Speed** | 0.5× to 2× |
| **Info** | What is playing: resolution, codecs, frame rate, bitrates, position, app version |
| **Stats** | The panel described below |

Zoom, subtitle size, and the last-picked audio and subtitle languages carry over
to the next film. When the player closes it reports where it stopped, so the film's
page can offer to resume.

### Stats: what is holding a film up

A film that stops to buffer is held up by one of three things, each with a
different cure: the network (or the PC behind it) is not bringing the film in fast
enough, or the TV cannot decode the picture fast enough, or it cannot decode the
sound fast enough. **Stats** puts a panel in the corner that stays up over the
film, updates twice a second, and names the cause first:

| Headline | Meaning | What helps |
|---|---|---|
| **Keeping up** | The film arrives at least as fast as it plays, or the buffer is full | Nothing needed |
| **The network is the limit** | The player is fetching all the time and the film still arrives slower than it plays; "0.72× real time" is 72% of what it needs. "Over a 100 Mb/s Ethernet link" means Amazon's 10/100 adapter, which a UHD Blu-ray's peaks exceed. | The link between server and TV: wire the PC to the router, a gigabit USB adapter on the stick, or move the stick off the back of the TV with an HDMI extender |
| **The TV is the limit — dropping frames** | Frames drop with 5s or more buffered: the network keeps up, the decoder does not | A lower-bitrate copy of the film |
| **The TV is the limit — the sound runs dry** | The sound runs out with plenty buffered: decoding that track is too much for the stick | Another audio track, or pass it through to a receiver |

Below the headline:

| Line | What it shows |
|---|---|
| Download | How fast the film came in while the player was fetching, and what share of the time it was fetching. A full buffer stops fetching, so only fetching says anything about the link. |
| Film | The measured bitrate of the stretch being fetched (an MKV does not declare one) |
| Buffer | How far ahead is buffered and the memory it takes; *full* when the player has stopped fetching. The buffer is as big as the app's memory allows, up to three minutes of film: 349 MB when the Memory line says the app is allowed 537 MB, about a minute of a 45 Mb/s film; 144 MB when it is allowed only 201 MB. |
| Stalls | Waits once playback started (not the start or after a seek), and their total |
| Frames | Frames dropped in the last ten seconds, and in all |
| Underruns | Times the sound ran dry |
| Video, Audio | The codec, and what decodes it: the TV's hardware, software (FFmpeg, for sound the TV has no decoder for), or *passed through* to the TV or a receiver |
| Network | Wi-Fi with band, link rate and signal; or Ethernet, with its rate where the system reports it |
| Memory | The app's memory use, of what it is allowed |

Rates are over the last ten seconds, so they follow what is playing now; a seek
restarts them, and *Measuring…* shows for the first few seconds. The panel sees
only what the stick sees: when the network is the limit, it cannot tell the Wi-Fi
from the PC's own connection or the PC itself. Three checks that tell them apart
are in [Streaming performance](../design/streaming-performance.md).

## What plays

The player is Media3 (ExoPlayer) with Jellyfin's FFmpeg audio decoder. Video is
decoded by the Fire TV's hardware. Audio goes to the TV's decoders first, then
HDMI passthrough to a receiver or soundbar, then FFmpeg in software.

| | Plays | Does not |
|---|---|---|
| **Containers** | MKV, WebM, MP4, M4V, MOV, 3GP, AVI, TS, MPG (MPEG-2 program streams: DVD and TV recordings), VOB, FLV | WMV/ASF, M2TS (Blu-ray rips), Ogg video (OGV), MPEG-1 system streams (VCD-era `.mpg`), ISO disc images, RealMedia |
| **Video** | H.264, HEVC/H.265 (HDR10 where the TV supports it) and VP9 on every Fire TV; AV1 on the newer 4K models; MPEG-2, MPEG-4 part 2 (Xvid/DivX) and VC-1 where the stick's chip has a decoder, which most do | Anything the chip has no decoder for. There is no software video decoder. |
| **Audio** | AAC, MP3, AC-3, E-AC-3 (Atmos by passthrough), DTS core, TrueHD, FLAC, ALAC, Opus, Vorbis, PCM | DTS:X and DTS-HD MA play as their DTS core unless passed through; WMA; MP2 on most sticks |
| **Subtitles** | SRT, WebVTT, SSA/ASS, PGS, DVB, VobSub, CEA-608/708, embedded or beside the film | |

The container list is tested: `ContainerSupportTest` runs Media3's extractors over
a one-second clip in each container on every build. When a film's picture or
sound is beyond the TV, the player says so in words and plays what it can.

To play a file in an unsupported container, remux it. This copies the streams
without re-encoding and takes seconds:

```bash
ffmpeg -i film.m2ts -map 0 -c copy film.mkv
```

A film that plays but stops to buffer (a 4K Blu-ray rip runs 40–100 Mbit/s,
against 4–6 for 1080p) is usually limited by the network or the stick, not the
server. Use Stats, and see [Streaming performance](../design/streaming-performance.md).

## Without a Fire TV

Any TV or console with a browser can open `http://<host>:8123/tv/` and pair the
same way. Films play in the browser's own player, which handles MP4/H.264 and WebM
but usually not MKV, HEVC or surround sound; the app's native player does. In the
browser player, Left/Right seek 10s and Up/Down seek a minute.

## What the TV can and cannot do

The TV **can** browse the folder and the folders under it that hold videos, see
the film details and folder pictures, and play a video (seeking, resuming, and
using the subtitle files beside it).

The TV **cannot**:

- see any file that is not a video, or anything outside the folder;
- learn where the folder is in the vault, since paths it is sent start at the
  folder;
- change anything: no uploads, deletes, settings or film lookups;
- unlock the vault. If the vault locks itself while nobody is using it, the TV
  says *The vault is locked* and waits. Unlock it in the browser and the TV
  carries on, still paired. Browsing on the TV counts as use, so the vault does
  not lock while someone is picking a film.

On a home network the films cross the LAN as plain HTTP, exactly as they would to
a browser on the LAN. A TLS reverse proxy in front of the server covers the TV
too: point the app at the `https://` address.

## Ending it

A code ends when:

- its time is up;
- you **Revoke** it in the Watch on TV dialog, which lists every live code and how
  many TVs used it; or
- the server restarts, since codes are held only in memory.

Revoking unpairs every TV that used the code **and stops a film that is playing**,
because the stream link the player follows is bound to the code. **Sign out of
this TV** in the app's `⋯` menu unpairs just that TV. A TV whose code has ended
goes back to the pairing screen.
