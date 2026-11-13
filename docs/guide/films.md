# Films: posters, details and folder pictures

A folder of films is a folder whose file names say nothing:
`Some.Film.1982.REMASTERED.1080p.BluRay.x265-GROUP.mkv` is a fine thing to store
and a terrible thing to read. Like Plex or Jellyfin, Sand Vault can look each film
up and show its poster, summary, cast and score, but only in folders you turn it
on for. This page covers turning it on, fixing wrong matches, what it costs, and
giving folders pictures.

For how the matching and storage work inside, see
[Index design](../design/index.md). The endpoints are under `/api/movies` and
`/api/files/{id}/movie` in the [API reference](../reference/api.md).

## Before you start: a key, and what it sends

This is the one feature in Sand Vault that talks to anything except your own
accounts. Looking a film up sends a title guessed from the file name, plus this
machine's address and your API key, to
[The Movie Database](https://www.themoviedb.org) (TMDB), the service Plex and
Jellyfin use. Nothing about the file itself is sent: not its contents, size,
hash, or where its parts are.

You need a free key of your own:

1. On **themoviedb.org**, go to **Settings → API**. Either the v3 key or the v4
   read access token works.
2. In Sand Vault, paste it into **Vault settings → Film key** (at the foot of the
   sidebar, behind `☰` on a phone). The key is tested against TMDB before it is
   saved.

The key is shared by the whole vault, not set per folder. It is stored in the
vault file, sealed under your password like your cloud credentials. It is
deliberately *not* stored in the manifest, which is copied to every connected
account as a backup. Until there is a key, a folder's `🎬` dialog says so and
cannot be turned on. Removing the key stops new lookups; films already matched
keep their details.

## Turning it on for a folder

1. Open the folder and press `🎬` in the toolbar. The setting covers this folder
   and everything under it.
2. **Turn on.** This stores the setting and **sends nothing**.
3. **🎬 Look up new films** sweeps the folder. Each video's name is read for a
   title and a year and searched for, and what comes back is stored: the summary,
   score, runtime, genres, director and top of the cast go in the vault's
   encrypted index, and the poster becomes the file's thumbnail.

The sweep runs on the server, so you can close the dialog and carry on; a bar
over the file list follows it. **Stop** keeps what was found, and running it
again picks up where it left off. **⟳ Look up everything again** re-matches films
that already have details, except ones you fixed by hand.

Once a film is matched:

- The grid becomes a wall of posters captioned with the films' titles. Tiles take
  a poster's two-by-three shape across the whole grid in that folder, so rows stay
  level.
- A row shows the poster where its icon was. `🎬` on the row, or **Film details**
  in its menu, opens everything that was found.
- Opening the file shows the film (poster, summary, cast) rather than an unplayed
  black rectangle. **▶ Play** plays it in the page, and **Stream in VLC** and
  **Download** are where they always are.

A film is looked up once. After that the answer is in your vault, and opening the
folder again contacts nobody. **Turn off** stops further lookups and leaves what
was found where it is.

## When it guesses wrong

Names are read the way media servers read them: cut at the year if there is one,
otherwise at the first tag such as `1080p`, `BluRay` or `x265`. When the file's
name says nothing, the folder's name is used, so `Some Film (1982)/title00.mkv`
is matched from the folder. When two films share a title, the year decides.

To fix one, open the file's details and press **⌕ Fix the match** (**⌕ Search by
title** if it was never matched). Search by title and pick the right film. A
match made by hand is marked as such, and no later sweep replaces it, not even
**Look up everything again**. **✕ Forget** drops the details for that file; the
file and its poster stay.

## What it costs, and what survives

- **A sweep** is one search, one record and one poster per film, so a large folder
  takes a while. Every film is stored the moment it is matched, so stopping loses
  nothing.
- **Posters** are written one pack per folder rather than one per film, so a
  folder of two hundred films costs one upload instead of two hundred.
- **Changing the vault password** drops every stored thumbnail, since they are
  sealed under the retiring key. A photo's thumbnail comes back the next time you
  open it. A poster has no such source, but the film's record keeps the artwork's
  address, so sweeping the folder again brings the posters back with one image
  fetch each and no searching.

Details and artwork come from The Movie Database (TMDB). This uses the TMDB API
but is not endorsed or certified by TMDB.

## Folder pictures

A library of films is otherwise a row of identical `📁` icons. A folder can
borrow a picture from something inside it: a film's poster, or the thumbnail of a
photo or a PDF, from any depth. So a library with one folder per film can show a
poster from two levels down.

- **To pick one:** press `🖼` on the folder's row, or choose **Folder picture** in
  its menu. Everything inside that has a picture is listed. **Use no picture**
  takes it away. Both controls appear only where there is something to choose.
- **Nothing is picked for you.** A folder keeps its icon until you choose.
- **Nothing is stored for it.** The folder points at a file that already has a
  thumbnail and draws that file's picture. It costs no upload and no extra object
  on any account.
- **It follows the file.** The choice is remembered by file, not by name or
  place, so renaming the file, moving it deeper or moving the folder leaves the
  picture in place. Deleting the file puts the folder back to its icon.

Thumbnails are stored one pack per folder, so a parent whose sub-folders all have
pictures fetches one pack per sub-folder the first time it is listed. Only the
tiles on screen fetch anything, and each pack is kept until the vault locks.

Folder pictures and posters also show on a TV; see [Sand TV](sand-tv.md).
