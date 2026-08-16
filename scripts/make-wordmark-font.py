#!/usr/bin/env python3
"""Build the wordmark's script face: five glyphs, renamed, as a woff2.

The wordmark draws exactly one word — *Vault* — so shipping a whole script
family to draw it would be a hundred kilobytes of glyphs nobody ever sees, in a
file that the build embeds directly into index.html. This cuts the face down to
the letters that are actually drawn.

The rename is not decoration. The source face is under the SIL Open Font
License with a Reserved Font Name, and the OFL is explicit: a Modified Version
— which a subset is — may not carry the reserved name. So the derivative gets
a name of its own, keeps the original copyright notice, and travels with the
licence text beside it in web/fonts/OFL.txt.

Usage:

    pip install 'fonttools[woff]' brotli
    python scripts/make-wordmark-font.py Sacramento-Regular.ttf

The source TTF is not kept in this repository; download it from the upstream
project (github.com/google/fonts/tree/main/ofl/sacramento) and point this at
it. The output lands at web/fonts/wordmark-script.woff2, which the web build
embeds automatically.
"""
import subprocess
import sys
from pathlib import Path

from fontTools.ttLib import TTFont

REPO = Path(__file__).resolve().parent.parent
OUT = REPO / "web" / "fonts" / "wordmark-script.woff2"

# The word the wordmark writes. Nothing else needs to be in the file.
TEXT = "Vault"

# The derivative's own name, as the OFL requires for a Modified Version of a
# face with a Reserved Font Name.
FAMILY = "SAND Wordmark Script"
STYLE = "Regular"
VERSION = "Version 1.000; SAND subset"


def main(source: str) -> int:
    tmp = OUT.with_suffix(".subset.ttf")
    OUT.parent.mkdir(parents=True, exist_ok=True)

    subprocess.run(
        [
            "pyftsubset", source,
            f"--text={TEXT}",
            # Keep the name table so the records below survive to be rewritten.
            "--name-IDs=*",
            "--output-file=" + str(tmp),
        ],
        check=True,
    )

    font = TTFont(tmp)
    name = font["name"]

    # nameID 0 is the copyright notice, which the licence requires be kept.
    # nameID 7 is the source foundry's trademark: it belongs to their face, not
    # to this derivative, so it goes.
    for record in list(name.names):
        if record.nameID == 7:
            name.names.remove(record)

    for record in name.names:
        if record.nameID in (1, 4, 16):
            record.string = FAMILY
        elif record.nameID == 2:
            record.string = STYLE
        elif record.nameID == 3:
            record.string = f"{FAMILY}: {STYLE}"
        elif record.nameID == 5:
            record.string = VERSION
        elif record.nameID == 6:
            record.string = FAMILY.replace(" ", "") + "-" + STYLE

    font.flavor = "woff2"
    font.save(OUT)
    font.close()
    tmp.unlink()

    print(f"{OUT.relative_to(REPO)} — {OUT.stat().st_size} bytes, glyphs for {TEXT!r}")
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(__doc__)
        raise SystemExit(2)
    raise SystemExit(main(sys.argv[1]))
