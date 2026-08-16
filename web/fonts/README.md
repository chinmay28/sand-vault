# The wordmark's script face

The second half of the wordmark — *Vault* — is set in **Nefelibata Script**, by
[My Creative Land](https://www.myfonts.com/collections/my-creative-land-foundry)
(Elena Genova). The name is the reason: a *nefelibata* is a cloud-walker, which
is a fair description of a vault that lives on other people's clouds.

It is a licensed font, and this repository does not carry one. Nothing here
breaks without it: the wordmark falls back to whatever handwriting face the
platform already has (Snell Roundhand on Apple, Segoe Script on Windows), which
is what shipped before.

## Adding it

Buy a **webfont** licence and put the file here:

```
web/fonts/nefelibata-script.woff2      (or .woff)
```

Then `make build-web`. The build says which it did:

```
wordmark: embedding nefelibata-script.woff2 (14 KB)
wordmark: no font at web/fonts — falling back to the system script face
```

Nothing else changes — `FONT.script` in `web/src/theme.js` already asks for the
family first and keeps the system stack behind it.

## Why it is embedded rather than linked

SAND fetches nothing from anywhere; opening your vault makes zero third-party
requests, and a logo is not the thing to break that for. So the file is read at
build time and embedded in `index.html` as a `data:` URI rather than linked —
it cannot become a request to somebody's CDN however the app is deployed, and
there is no second round trip before the mark is drawn.

That does put the font in the page, so keep it small.

## Subset it

The wordmark is five letters. A full family carries Latin, Cyrillic and a few
hundred glyphs you will never draw, which is a lot of `index.html` for one
word. Cut it down with [`fonttools`](https://github.com/fonttools/fonttools):

```bash
pip install 'fonttools[woff]' brotli
pyftsubset NefelibataScript.otf \
  --text='Vault' \
  --flavor=woff2 \
  --output-file=web/fonts/nefelibata-script.woff2
```

That is usually a few kilobytes rather than a hundred. Add `--text='Vault SAND'`
if you ever set the first word in it too.

## Why it is not committed

`.gitignore` keeps `web/fonts/*.woff*` out of the repository on purpose: a
webfont licence generally covers *your* sites, not redistribution, and a public
repository redistributes everything in it. If your licence says otherwise, that
is your call to make — drop the ignore rule.

Release builds made from a clean checkout therefore ship the fallback. If you
want the real face in the published binaries, the font has to be present on the
machine that runs `make release`.
