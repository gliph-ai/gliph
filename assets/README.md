# Icons

- `icon-source.png` — the original full-bleed art (2048). Mobile pipelines mask
  icons themselves, so they take this one unpadded.
- `icon-macos.png` — production desktop icon. Apple geometry: 1024 canvas, a
  transparent 100px margin, an 824 body, 0.2251 corner radius. Anything else
  sits visibly larger than its neighbours in the Dock.
- `icon-macos-dev.png` — the SAME production art plus a gold corner badge.
  Deliberately not a recolour: tinting the plate made it muddy and stopped
  reading as the same app. The badge leaves the artwork untouched and is still
  legible at 32px, which is the size that actually matters.

Regenerate the desktop icon sets after changing either:

    cd desktop
    ./node_modules/.bin/tauri icon ../assets/icon-macos.png
    ./node_modules/.bin/tauri icon ../assets/icon-macos-dev.png -o src-tauri/icons-dev
    rm -rf src-tauri/icons/android src-tauri/icons/ios \
           src-tauri/icons-dev/android src-tauri/icons-dev/ios
