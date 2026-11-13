# Streaming large films: where the time goes

A 4 GB 1080p film plays in SAND TV on a Fire TV Stick without a pause; a 35 GB
Ultra HD Blu-ray of the same length, from the same vault on the same machine,
stops to buffer. This page covers what was measured to find out why, what it
found, and what to do about it. Every measurement was made on the real code
(`sand serve` and the packages it is built from), none on a real Fire TV, phone
or home network. The last section lists what that leaves unverified.

## The short answer

| Suspect | Verdict | Why |
|---|---|---|
| Decrypting and rebuilding the film | Not the limit | 70–95 ms per 32 MiB chunk on one 2.1 GHz core, 350–460 MB/s. An 80 Mbit/s film is 10 MB/s, under 5% of that one core |
| Chunking | Not the limit on a healthy disk | 0.04–0.17 s to the first byte after a seek; a model of the TV's player never stalled at 40 or 80 Mbit/s with the parts on local disks |
| The network between the server and the TV, and the TV itself | The likely limit | A UHD film runs at 8–9 times the bitrate of a 1080p one, which moves it from far inside every limit on the way to close to several of them (see [The TV and the network](#the-tv-and-the-network)) |

Two things in the server ended long films whatever their quality. Both are
fixed (see [Two timers](#two-timers-that-ended-a-film-whatever-its-quality)).
When the parts are on cloud accounts rather than local disks, a third thing
does make the server the limit: how it reads ahead (see
[Parts on cloud accounts](#parts-on-cloud-accounts)).

To find which limit a particular TV and network are hitting, see
[Telling which limit you are hitting](#telling-which-limit-you-are-hitting).

## What a UHD film asks for

A film's average bitrate is its size over its running time:

| File | 90 min | 120 min | 150 min |
|---|---|---|---|
| 35 GB (an Ultra HD Blu-ray remux or a heavy encode) | 51.9 Mbit/s | 38.9 Mbit/s | 31.1 Mbit/s |
| 4 GB (a typical 1080p encode) | 5.9 Mbit/s | 4.4 Mbit/s | 3.6 Mbit/s |

That is an average. A UHD Blu-ray's HEVC video may run at up to 100 Mbit/s,
and the whole disc stream up to about 82, 108 or 128 Mbit/s for 50, 66 and
100 GB discs ([BDA](https://us.blu-raydisc.com/blog/ultrahd_benefits/benefit-2/);
[BD-ROM format v3 white paper](https://docplayer.net/48882577-White-paper-blu-ray-disc-read-only-format-ultra-hd-blu-ray-audio-visual-application-format-specifications-for-bd-rom-version-3.html), read through a mirror).
The titles found measured with BDInfo average 54–80 Mbit/s of video, and a
lossless soundtrack adds a few Mbit/s more (one TrueHD Atmos track measured
4.0 Mbit/s). Everything between the vault and the screen has
to keep up with the peaks, not only the average.

## How a film reaches the TV

```
SAND TV player ── GET /stream/<token>/film.mkv, Range: bytes=N- ──►  sand serve
                                                                       │
            http.ServeContent ◄── ChunkedReader.ReadAt (internal/vault/reader.go)
                                       │
                 decrypted-chunk cache ─┤─ single flight per chunk
                                       │─ read-ahead: the next ReadAhead chunks
                                       ▼
                gatherChunk (internal/vault/chunked.go): every part of the chunk
                is sent for at once, and the first k that arrive are used
                                       │
               decrypt (AES-256-GCM) → rebuild (Reed–Solomon) → decompress (zstd)
```

The player makes one request for the whole film and reads it front to back,
pausing whenever its own buffer is full. A seek is a new request at a new
offset. The server answers each with the chunks the range covers, never the
whole file (see [Reading at an offset](data-pipeline.md#reading-at-an-offset)).

## Measurements

### How they were made

- The server was `sand serve` built from this repository, on a test machine
  with 4 vCPUs (2.1 GHz Xeon) and 16 GB. That is slower per core than a
  current desktop, so its CPU figures are pessimistic for one.
- The test film was 1.95 GB of incompressible data, stored 2-of-3 in 32 MiB
  chunks, which is what a machine with 32 GB or more cuts.
- The disk cache was dropped before every cold measurement.
- "The player" is a model of SAND TV's Media3 1.7.1 player with its default
  settings, written from Media3's source: one open-ended range request; it
  keeps loading until 50 s or 144 MB is buffered, whichever comes first; it
  starts with 1 s buffered and resumes after a stall with 2 s; it gives up on
  a read that delivers nothing for 8 s and reconnects. The film is constant
  bitrate, which is kinder than a real one.
- Cloud accounts were emulated by WebDAV servers that share one bandwidth
  budget (the "internet speed") and take 300 ms to answer.

The tools are not part of the repository.

### Parts on local disks

What rebuilding one chunk costs, on one core:

| Chunk | Parts used | Decrypt | Rebuild | Decompress | Total | One core sustains |
|---|---|---|---|---|---|---|
| 32 MiB | 1 and 2 (both data parts) | 48 ms | 9 ms | 17 ms | 73 ms | 458 MB/s |
| 32 MiB | 1 and 3 (one data, one parity) | 20 ms | 59 ms | 15 ms | 94 ms | 356 MB/s |
| 32 MiB | 2 and 3 | 16 ms | 60 ms | 13 ms | 89 ms | 376 MB/s |
| 16 MiB | 1 and 2 | 9 ms | 6 ms | 7 ms | 22 ms | 759 MB/s |
| 16 MiB | 1 and 3 | 8 ms | 26 ms | 9 ms | 43 ms | 391 MB/s |

A film does not compress (the stored chunk was 100.00% of the original), so
decompression is close to a copy. Rebuilding from a parity part costs more
than concatenating the two data parts, and is still far faster than any film.

End to end, with the disk cache dropped first:

| Measurement | Result |
|---|---|
| Whole film through a stream link at full speed | 520–740 MB/s across runs (4–6 Gbit/s); first byte after 0.10 s |
| Bytes read from disk per byte served | 1.50 — every part of every chunk is read, and one of the three is thrown away |
| Server CPU | 4.3 CPU-seconds per GB served |
| First byte after a seek, 8 scattered offsets | 0.04–0.17 s |
| The player, 40 Mbit/s film | first frame 0.19 s, no stalls, buffer full at 28.9 s of film |
| The player, 80 Mbit/s film | first frame 0.19 s, no stalls, buffer full at 14.4 s of film |
| The player, 80 Mbit/s film resumed 60 s in | first frame 0.36 s, no stalls |

The same, with the server's disk reads capped to stand in for a slow or busy
disk:

| Disk | Film | Read-ahead | First frame | Stalls |
|---|---|---|---|---|
| 120 MB/s | 80 Mbit/s | 6 | 0.9 s | none |
| 60 MB/s | 80 Mbit/s | 6 | 2.1 s | none |
| 35 MB/s | 80 Mbit/s | 6 | 1.4 s | one, 3.7 s |
| 35 MB/s | 40 Mbit/s | 6 | 2.3 s | none |
| 35 MB/s | 80 Mbit/s | 1 | 1.8 s | none |
| 20 MB/s | 80 Mbit/s | 1 | 3.0 s | none |

Chunking has two real costs, and neither is a limit on a healthy disk. The
1.5× is the [read race](data-pipeline.md#opening-a-file) reading all three
parts of every chunk. The burst is read-ahead: at a start or a seek the
server asks for six chunks at once — eighteen part files — and on a disk that
is slow or busy with something else the chunk the player needs next waits
behind the other five.

### Parts on cloud accounts

This is not where most people keep films they watch, but it is where the server is
most often the limit. A 40 Mbit/s film, 120 s of it, from the start:

| Internet | As shipped (read-ahead 6) | Read-ahead 1 | Prototype read path |
|---|---|---|---|
| 1000 Mbit/s | no stalls | — | no stalls |
| 300 Mbit/s | 1 stall, 17 s | no stalls | no stalls |
| 150 Mbit/s | 8 stalls, 299 s in all | no stalls | no stalls |
| 100 Mbit/s | 13 stalls, 137 s in all | no stalls | no stalls |
| 75 Mbit/s | still stalled after 8 minutes, 6 s played | no stalls, given a 30 s read timeout, a 400 MB buffer and a 10 s resume | no stalls |
| 50 Mbit/s | never started | — | no stalls, first frame after 15.7 s |

An 80 Mbit/s film fared the same way (as shipped: 21 stalls in 100 s at
150 Mbit/s; the prototype: none). Even a 1080p film stalled for 72 s at
150 Mbit/s as shipped.

What goes wrong is a livelock between the read-ahead and the player:

1. At a start or a seek, read-ahead fetches six 32 MiB chunks at once, and the
   read race sends for all three parts of each: eighteen downloads sharing one
   internet connection.
2. The chunk the player needs next shares the line with seventeen others, so
   on a 150 Mbit/s line nothing reaches the player for well over 8 s.
3. The player's read times out after 8 s and it reconnects. The request it
   left behind is cancelled, and with it every fetch that request started.
4. The new request asks for the same chunks, finds them already being fetched,
   waits for those fetches, and is handed the cancellation of the request
   before it. The single flight that collapses two readers onto one fetch also
   gives the second reader the first one's cancellation
   (`chunkFlight.do` in `internal/vault/reader.go`). The
   [appendix](#appendix-reproducing-the-single-flight-failure) describes a
   test that shows it.
5. Back to 2.

A prototype of three changes to the read path removed every stall above:

- **Read-ahead in order, at most two chunks in the air**, nearest first, and
  started by any read rather than only one that follows the read before it.
  On its own this one removed the stalls at 300 and 100 Mbit/s.
- **The data parts first, and a parity part only when one of them fails or is
  slow** (6 s here), instead of racing all three. On its own this one fixed
  300 Mbit/s and not 100; what it buys is reading two parts of each chunk
  rather than three, a third less off the accounts or off a local disk.
- **A reader that belongs to the link rather than to one request**, so a
  player that reconnects finds the chunks it was already being sent.

The prototype had no tests of its own and was not merged: the read-ahead and
the single flight in `internal/vault/reader.go` still behave as described
above. Until something like it lands, setting **Settings → This machine → Read-ahead** to 1
stopped the stalls at 300, 150 and 100 Mbit/s for the 40 Mbit/s film; it was
not tried on the 80 Mbit/s one.

## Two timers that ended a film whatever its quality

Both were reproduced against the server as it then was, and both are fixed in
`internal/server/stream_serve.go` (see
[Stream tickets](server.md#stream-tickets)).

| | What happened | Reproduced |
|---|---|---|
| The idle lock | A stream link counted as use of the vault only as its request began. A player makes one request for the whole film, so with no browser open beside it the idle timeout ran out mid-film, the vault locked, and the link was voided | With `--idle-timeout 2m`, the vault locked 2 min 25 s into a stream with 3.1 GB of it unsent; the link answered 404 afterwards |
| The half-hour cut | Every stream and every read through the app had thirty minutes in all, so that a hung provider could not hold a request open forever. Every feature film is longer than that | With the idle lock out of the way, the server closed a healthy stream after 30 min 49 s with 1.9 GB unsent |

Now:

- A stream counts as use of the vault for as long as bytes are going out to
  the player, checked about once a second. A player left paused takes
  nothing, and that is not use: a television left on pause does not keep the
  vault unlocked all night.
- The half-hour is a limit on the vault going quiet, not on the request: a
  stream is given up on only once the vault has given it nothing for thirty
  minutes at a stretch. Waiting on the player — a viewer who paused, a player
  whose buffer is full — does not count against it.
- A link minted under a TV code is checked against the code as the film goes
  out, so revoking the code stops the film already playing, as
  [`sand-tv.md`](sand-tv.md) says it does. Before, the check was made as a
  request began, which for a film is before the film.

## The TV and the network

None of this was measured on a device. The figures are from Amazon's own
pages and from people who measured them, and how well each is supported is
said beside it.

### The sticks

From Amazon's [device specifications](https://developer.amazon.com/docs/device-specs/device-specifications-fire-tv-streaming-media-player.html)
(read through search excerpts; the page was not reachable directly):

| Model | Chip, memory | Wi-Fi | Fire OS (Android API) | Wired |
|---|---|---|---|---|
| Fire TV Stick 4K, 2018 | MT8695, 1.5 GB | Wi-Fi 5, 2×2 | 6 (25) | through an adapter |
| Fire TV Stick 4K Max, 2021 | MT8696, 2 GB | Wi-Fi 6, 2×2 | 7 (28) | through an adapter, 10/100 |
| Fire TV Stick 4K, 2023 (sold since as 4K Plus) | MT8696D, 2 GB | Wi-Fi 6, 2×2 | 8 (30) | through an adapter, 10/100 |
| Fire TV Stick 4K Max, 2023 | MT8696T, 2 GB | Wi-Fi 6E | 8 (30) | through an adapter, 10/100 |
| Fire TV Stick 4K Select (2025) and 4K (2026) | MT8698, 1 GB | — | Vega OS | — |

The Vega OS sticks do not run Android apps and cannot install SAND TV
([AFTVnews](https://www.aftvnews.com/sideloading-is-truly-dead-on-the-fire-tv-stick-4k-select-with-vega-os/)).

### The link

- **Amazon's Ethernet adapter for the sticks is 10/100**
  ([Amazon](https://www.amazon.com/Amazon-Ethernet-Adapter-Fire-Devices/dp/B074TC662N)),
  and is reported at 80–95 Mbit/s in practice
  ([AFTVnews](https://www.aftvnews.com/amazon-ethernet-adapter-for-fire-tv-sticks-and-fire-tvs-is-on-sale-for-10-99-first-sale-ever/),
  [Pocket-lint](https://www.pocket-lint.com/fire-tv-stick-add-ethernet-port/)).
  That is close to an average UHD remux and below its peaks. Two independent
  reports agree; it is also what the 10/100 rating predicts.
- **A USB gigabit adapter on an OTG cable** is reported at 290–400 Mbit/s on
  the 4K and 4K Max, the stick's USB 2.0 port being the ceiling (nine reports
  across the [Kodi forum](https://forum.kodi.tv/showthread.php?tid=364384&page=8),
  AVS Forum and reviews). Some adapters do not work on some sticks; adapters
  with the ASIX AX88179 chip are reported as the most compatible.
- **Wi-Fi** on a 4K Max is reported at 225–480 Mbit/s depending on the
  router and the room. A stick behind a TV has a small antenna in a poor
  place; AFTVnews suggests the HDMI extender, to move it away from the back of
  the TV, and checking for a signal-to-noise ratio of 25 or better
  ([AFTVnews](https://www.aftvnews.com/how-to-check-understand-and-improve-your-fire-tv-wifi-speed-signal-strength-and-network-noise/)).
  A Kodi developer and several users say Wi-Fi on these sticks is not enough
  for raw UHD Blu-ray ([Kodi forum](https://forum.kodi.tv/showthread.php?pid=3169491);
  community reports, consistent with one another).
- **The PC's own connection** carries every film to every device. A PC on
  Wi-Fi, or behind a powerline adapter, is the one link a phone and a TV
  would both struggle through.

### The decoder

- Amazon's specification gives HEVC as "up to 3840x2160p (4K) @ 60fps,
  35 Mbps, Main 10 Profile Level 5.1", attached to the 4K Max by
  [CNX Software](https://www.cnx-software.com/2021/10/12/55-amazon-fire-tv-stick-4k-max-supports-4k-av1-videos-wi-fi-6/);
  which rows of Amazon's page carry it for the other sticks was not clear.
- That is below the video bitrate of most UHD Blu-rays, and yet several users
  report 60–90 Mbit/s remuxes playing on the 4K and 4K Max while others report
  stutter on the same sticks
  ([Emby forum](https://emby.media/community/topic/120116-hue-sync-kills-my-fire-tv-stick/),
  [Kodi forum](https://forum.kodi.tv/showthread.php?tid=336151&page=47)). The
  rated figure is a floor, not a ceiling; how far above it a given stick goes
  is something only that stick can say.
- **Dolby Vision profile 7**, which UHD Blu-ray remuxes often keep, is not in
  Amazon's list of profiles the sticks decode, and Media3 1.7.1 — the version
  SAND TV ships — has no fallback to the HDR10 base layer for it. That was
  read in Media3's source: `MediaCodecUtil.getAlternativeCodecMimeType` falls
  back for profiles 4, 8, 9 and 10 and not for 7. Reported results are a
  black picture with sound
  ([androidx/media#3486](https://github.com/androidx/media/issues/3486)). That
  is a film that does not play at all, rather than one that buffers.
- **Lossless audio**: the 2023 sticks list TrueHD passthrough and DTS-HD
  ("basic profile"); DTS-HD MA often arrives at a receiver as its DTS core.
  Audio that is neither passed through nor decoded by the stick is decoded in
  software by FFmpeg (see [`sand-tv.md`](sand-tv.md)). Nobody was found who
  measured what that costs on a stick's CPU.

### The player's buffer

SAND TV builds its player with Media3's defaults, which this was read from
(`DefaultLoadControl` and `DefaultHttpDataSource`, Media3 1.7.1):

| | Default | For a UHD film |
|---|---|---|
| Most it buffers | 50 s, or 144 MB (2,200 segments of 64 KiB for video and audio), whichever comes first | 144 MB is 29 s at 40 Mbit/s and 14 s at 80; a 1080p film gets the whole 50 s |
| Starts playing with | 1 s buffered | |
| Resumes after a stall with | 2 s buffered | so a slow link stalls again almost at once |
| Gives up on a read that delivers nothing for | 8 s, then reconnects | |

So a 1080p film rides out a 50-second hiccup and a UHD film one of 14–29
seconds, and after a stall a UHD film starts again on two seconds of cushion.

SAND TV no longer uses these as they are: its buffer is now sized to the app's
memory, and after a stall it waits for a runway of film rather than two
seconds of it. See [SAND TV design](sand-tv.md#player-internals). The Stats
panel on a stick in the field read *the film arrives at 0.75× real time* —
34.5 Mb/s on a 5 GHz Wi-Fi link reporting 344 Mb/s, for a 45.6 Mb/s stretch —
which is the case this is for: at that rate two seconds of cushion last eight.

## Telling which limit you are hitting

Three checks, each of which rules one part of the path in or out:

1. **The server and its disks.** On the PC, copy a film's stream address
   (the **▶** dialog in the browser shows it, with a copy button) and download
   it at full speed:

   ```bash
   curl -o /dev/null -w '%{speed_download} bytes/s\n' "<stream address>"
   ```

   (`-o NUL` on Windows.) A steady rate well above 50 MB/s says the vault and
   its disks are not the limit. The parts' own disks show up here, so a busy
   or failing one does too.

2. **The network.** Run the same command on a laptop on the same Wi-Fi, where
   the TV is. A UHD remux wants a steady 15 MB/s or more (120 Mbit/s) to have
   room for its peaks. If the PC got it and the laptop does not, it is the
   network.

3. **The device.** Play a copy that does not need the network: a few minutes
   cut out of the film without re-encoding,

   ```bash
   ffmpeg -ss 00:30:00 -t 300 -i film.mkv -map 0 -c copy sample.mkv
   ```

   played from storage on the device itself — a USB drive on the Fire TV
   through an OTG cable, in VLC or Kodi; the phone's own storage. If it still
   stutters, the device cannot decode it, and no network will fix that.

## What would help

In the order it is worth trying, for films kept on the PC's own disks:

1. **The network.** Wire the PC to the router. Give the stick a USB gigabit
   adapter rather than Amazon's 10/100 one, or failing that, move it off the
   back of the TV with the HDMI extender and onto 5 GHz.
2. **A lower-bitrate copy, made once.** If check 3 fails, or the network
   cannot be fixed, it is the one change that helps every device. Encode it
   with the machine's own hardware (Intel's Quick Sync encodes HEVC 10-bit on
   any recent CPU that has integrated graphics) or with x265, at 15–25 Mbit/s
   for 4K, and check the copy kept HDR if the original had it. Keep the
   original in the vault; play the copy.
3. **Dolby Vision profile 7.** If a film shows a black picture with sound,
   convert its Dolby Vision to profile 8.1 (dovi_tool does this) or remux it
   with the HDR10 base layer only.
4. **The player.** What the stick sees — how much is buffered, how fast it is
   arriving, how many frames it drops — is the measurement that tells checks
   2 and 3 apart while the film plays; SAND TV's Stats panel shows it. Its
   buffer is now sized to the stick's memory and a stall waits for a runway.
   What is left is to wait longer than 8 s before giving up on a read.

And, for films kept on cloud accounts, the server's read path:

5. **Read-ahead in order with a bound on fetches in flight, the data parts
   first, and readers that outlive a request**, as measured above.
6. **The single flight should not hand one reader another's cancellation.**
   A fetch that two readers share should run on a context of its own that
   ends when the last of them stops waiting.

## What was not verified

- No real Fire TV, phone, router or cloud account was used. The player is a
  model of Media3's defaults with a constant-bitrate film, and the clouds are
  emulated.
- The device facts come from search-result excerpts of the pages linked, not
  from reading the pages themselves: Amazon's developer site, AFTVnews and
  Reddit could not be reached from where this was written.
- How far above its rated 35 Mbit/s a given stick decodes HEVC, and what
  software decoding of TrueHD or DTS-HD costs it, were not found measured
  anywhere.
- The test machine is slower per core than a current desktop; the CPU figures
  are an upper bound for one.

## Appendix: reproducing the single-flight failure

Two readers of one file ask for the same chunk while the accounts are slow.
The first gives up — a player reconnecting — and the second, whose own
context is still alive, is told the read was cancelled. In outline, as a test
in `internal/vault`:

1. Register a provider whose `Get` blocks on a gate, connect three accounts
   of it, and upload a file of a few chunks.
2. Close the gate, open two readers, and have the first `ReadAtContext` at
   chunk 2 with a context of its own. Wait until it has reached the provider.
3. Have the second `ReadAtContext` at the same offset with a background
   context.
4. Cancel the first context and open the gate.

The second read fails with "could not gather 2 shards for chunk 2 …: context
canceled" instead of returning the chunk.
