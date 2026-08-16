#!/usr/bin/env python3
"""Build the wordmark's script face: five glyphs, as a woff2.

The wordmark draws exactly one word — *Vault* — so shipping a whole script
family to draw it would be a few hundred kilobytes of glyphs nobody ever sees,
in a file the build embeds directly into index.html. This cuts the face down to
the letters that are actually drawn, which is the difference between two
kilobytes and four hundred.

A variable font is pinned to one weight first: the wordmark is set at one
weight, and carrying the axis would mean carrying every master behind it.

    pip install 'fonttools[woff]' brotli
    curl -O https://raw.githubusercontent.com/google/fonts/main/ofl/caveat/Caveat%5Bwght%5D.ttf
    python scripts/make-wordmark-font.py 'Caveat[wght].ttf' --weight 500

The source font is not kept in this repository — download it from upstream and
point this at it. The output lands at web/fonts/wordmark-script.woff2, which
the web build embeds automatically, and its family name is what
`FONT.script` in web/src/theme.js has to name.

On names: a subset is a Modified Version under the SIL Open Font License. Where
the source declares a Reserved Font Name the derivative may not use it, and
`--family` renames the whole name table accordingly; where it does not, keeping
the original name is both allowed and more honest about whose drawing it is.
Either way the copyright, designer and licence records are preserved, and the
licence text travels beside the font in web/fonts/OFL.txt.
"""
import argparse
import subprocess
import sys
from pathlib import Path

from fontTools import ttLib
from fontTools.varLib import instancer

REPO = Path(__file__).resolve().parent.parent
OUT = REPO / "web" / "fonts" / "wordmark-script.woff2"

# The word the wordmark writes. Nothing else needs to be in the file.
TEXT = "Vault"


def main() -> int:
    ap = argparse.ArgumentParser(description="Build web/fonts/wordmark-script.woff2")
    ap.add_argument("source", help="the upstream .ttf or .otf to cut down")
    ap.add_argument("--weight", type=float, default=None,
                    help="pin a variable font's wght axis to this value")
    ap.add_argument("--family", default=None,
                    help="rename the face — required when the source reserves its name")
    args = ap.parse_args()

    OUT.parent.mkdir(parents=True, exist_ok=True)
    pinned = OUT.with_suffix(".pinned.ttf")
    subset = OUT.with_suffix(".subset.ttf")
    source = Path(args.source)

    if args.weight is not None:
        font = ttLib.TTFont(source)
        instancer.instantiateVariableFont(font, {"wght": args.weight}, inplace=True)
        font.save(pinned)
        font.close()
        source = pinned

    subprocess.run(
        [
            "pyftsubset", str(source),
            f"--text={TEXT}",
            # Keep the name table: the records below are the attribution, and
            # the licence requires them to survive the subsetting.
            "--name-IDs=*",
            "--output-file=" + str(subset),
        ],
        check=True,
    )

    font = ttLib.TTFont(subset)
    if args.family:
        name = font["name"]
        # nameID 7 is the source foundry's trademark: it belongs to their face,
        # not to a renamed derivative of it.
        for record in [r for r in name.names if r.nameID == 7]:
            name.names.remove(record)
        for record in name.names:
            if record.nameID in (1, 4, 16):
                record.string = args.family
            elif record.nameID == 3:
                record.string = f"{args.family}: Regular"
            elif record.nameID == 6:
                record.string = args.family.replace(" ", "") + "-Regular"

    font.flavor = "woff2"
    font.save(OUT)
    family = font["name"].getDebugName(1)
    font.close()

    for scratch in (pinned, subset):
        scratch.unlink(missing_ok=True)

    print(f"{OUT.relative_to(REPO)} — {OUT.stat().st_size} bytes, "
          f"{TEXT!r} in {family!r}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
