#!/usr/bin/env python3
"""Regenerate the iOS app icon and launch image from assets/icon-source.png.

    mobile/tools/icons.py

The source artboard is drawn as a rounded tile on white, which is right for
macOS and wrong for iOS: iOS applies its own superellipse mask to a full-bleed
square, so a source with its own rounded corners shows white notches through the
mask. So the corners are flooded with the artwork's own background before the
sizes are cut.

ponytail: no icon-generator dependency. It is a build-time package, a config
block and a generated-file dance to do what forty lines of Pillow do once, and
the icon changes about as often as the logo does.
"""

import json
import pathlib
import sys

from PIL import Image, ImageDraw

ROOT = pathlib.Path(__file__).resolve().parents[2]
SOURCE = ROOT / "assets" / "icon-source.png"
ICONSET = ROOT / "mobile" / "Resources" / "Assets.xcassets" / "AppIcon.appiconset"
LAUNCHSET = ROOT / "mobile" / "Resources" / "Assets.xcassets" / "LaunchImage.imageset"


def flatten(img: Image.Image) -> Image.Image:
    """Full-bleed square: fill everything outside the artwork's rounded tile
    with the tile's own background colour, so iOS's mask has something to cut."""
    img = img.convert("RGB")
    w, h = img.size
    bg = img.getpixel((w // 2, h // 20))  # inside the tile, above the artwork

    # Find the tile's corner radius by walking the top edge until the background
    # starts. Measuring beats hardcoding: the artboard has been redrawn before.
    radius = 0
    for x in range(w):
        if img.getpixel((x, 0)) == bg:
            radius = x
            break

    mask = Image.new("L", (w, h), 0)
    ImageDraw.Draw(mask).rounded_rectangle([0, 0, w - 1, h - 1], radius=radius, fill=255)
    out = Image.new("RGB", (w, h), bg)
    out.paste(img, (0, 0), mask)
    return out


def main() -> int:
    if not SOURCE.exists():
        print(f"no source icon at {SOURCE}", file=sys.stderr)
        return 1
    master = flatten(Image.open(SOURCE))

    contents = json.loads((ICONSET / "Contents.json").read_text())
    for entry in contents["images"]:
        name = entry.get("filename")
        if not name:
            continue
        pt = float(entry["size"].split("x")[0])
        px = round(pt * float(entry["scale"].rstrip("x")))
        master.resize((px, px), Image.LANCZOS).save(ICONSET / name)
        print(f"icon {name} {px}px")

    # Launch image: the same square, 120pt. LaunchScreen.storyboard paints the
    # rest of the screen in the artwork's own navy, so the launch is one flat
    # colour with the mark in it rather than a white flash and then the app.
    for scale in (1, 2, 3):
        side = 120 * scale
        suffix = "" if scale == 1 else f"@{scale}x"
        master.resize((side, side), Image.LANCZOS).save(LAUNCHSET / f"LaunchImage{suffix}.png")
        print(f"launch LaunchImage{suffix}.png {side}px")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
