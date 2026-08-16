# The wordmark's script face

The second half of the wordmark — *Vault* — is written rather than set. This is
where the face it is written in lives.

## What ships here

`wordmark-script.woff2` — **1.9 KB**, five glyphs: `V a u l t`, and nothing
else. It is a subset of [**Sacramento**](https://fonts.google.com/specimen/Sacramento)
by Brian J. Bonislawsky (Astigmatic), used under the SIL Open Font License 1.1,
whose full text is in `OFL.txt` beside it. A thin monoline script with tall
ascenders: the hand the mark was designed around.

The subset is renamed — it declares itself as **SAND Wordmark Script**, not as
Sacramento. That is not branding, it is the licence: Sacramento carries a
Reserved Font Name, and the OFL is explicit that a Modified Version may not use
one. The original copyright, designer and licence records travel with the file.

Rebuild it from the upstream face at any time:

```bash
pip install 'fonttools[woff]' brotli
curl -O https://raw.githubusercontent.com/google/fonts/main/ofl/sacramento/Sacramento-Regular.ttf
python scripts/make-wordmark-font.py Sacramento-Regular.ttf
```

That script does the subsetting and the renaming, and is the whole derivation —
nothing about this file is hand-edited.

## Using a licensed face instead

The app asks for **Nefelibata Script** first, and falls through to the face
above when it is not there. Nefelibata is by
[My Creative Land](https://www.myfonts.com/collections/my-creative-land-foundry)
(Elena Genova) and the name is the reason for wanting it: a *nefelibata* is a
cloud-walker, which is a fair description of a vault that lives on other
people's clouds.

Buy a **webfont** licence, put the file here, and the build picks it up:

```
web/fonts/nefelibata-script.woff2      (or .woff)
```

`.gitignore` keeps it out of the repository — a webfont licence covers your own
sites rather than redistribution — so a clean checkout builds with the open face
instead. Subset it the same way if you want it small; the wordmark only ever
draws five letters:

```bash
pyftsubset Nefelibata-Script.otf --text='Vault' --flavor=woff2 \
  --output-file=web/fonts/nefelibata-script.woff2
```

## Why they are embedded rather than linked

SAND fetches nothing from anywhere; opening your vault makes zero third-party
requests, and a logo is not the thing to break that for. So whatever is here is
read at build time and embedded in `index.html` as a `data:` URI rather than
linked — it cannot become a request to somebody's CDN however the app is
deployed, and there is no second round trip before the mark is drawn. That is
why size matters, and why both faces are cut to five glyphs.

The build says which it found:

```
wordmark: embedding wordmark-script.woff2 (1.9 KB)
wordmark: embedding nefelibata-script.woff2 (13.4 KB), wordmark-script.woff2 (1.9 KB)
wordmark: no font at web/fonts — falling back to the system script face
```

The last line is not a failure. Behind both faces the stack in
`web/src/theme.js` still names the handwriting face each platform ships, so the
mark is written either way.
