#!/usr/bin/env bash
# Build Gliph and put an install page for it on this Mac's Tailscale name, so an
# iPad (or iPhone) on the tailnet installs it from Safari, from anywhere.
#
#   mobile/tools/publish-ipad.sh    # prints https://<mac>.ts.net:8444/
#
# Same development build as deploy-device.sh. The device must be registered on
# the team (KF53B4S5HD) before the build, or iOS refuses the install: Xcode
# registers a device the first time it is paired. Keep Tailscale on, on the
# device, until the install finishes. An upgrade install keeps the app's data.
# Port 8443 is Sidra's install page; this one is 8444.
set -euo pipefail

ROOT="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TS=/Applications/Tailscale.app/Contents/MacOS/Tailscale
LOCAL_PORT=18772
HTTPS_PORT=8444
SITE="${TMPDIR:-/tmp}/gliph-ota-site"
APP="${TMPDIR:-/tmp}/gliph-device/Build/Products/${GLIPH_CONFIG:-Release}-iphoneos/Gliph.app"
ICONS="$ROOT/mobile/Resources/Assets.xcassets/AppIcon.appiconset"

# The build number is the publish time, so every publish is a newer build to iOS
# and a new IPA URL (the working tree is built, not a commit).
GLIPH_BUILD=$(date +%s) "$ROOT/mobile/tools/deploy-device.sh" --no-install

HOST=$("$TS" status --json | python3 -c "import json,sys; print(json.load(sys.stdin)['Self']['DNSName'].rstrip('.'))")
BASE="https://$HOST:$HTTPS_PORT"
VERSION=$(/usr/libexec/PlistBuddy -c "Print CFBundleShortVersionString" "$APP/Info.plist")
BUILD=$(/usr/libexec/PlistBuddy -c "Print CFBundleVersion" "$APP/Info.plist")
COMMIT=$(git -C "$ROOT" rev-parse --short HEAD)
# Which devices the profile lets install it: the iPad has to be in this list.
DEVICES=$(security cms -D -i "$APP/embedded.mobileprovision" | plutil -extract ProvisionedDevices json -o - - 2>/dev/null || true)

rm -rf "$SITE" && mkdir -p "$SITE/Payload"
cp -cR "$APP" "$SITE/Payload/"
(cd "$SITE" && zip -qry -1 Gliph.ipa Payload && rm -rf Payload)
cp "$ICONS/Icon-App-60x60@3x.png" "$SITE/icon.png"
cp "$ICONS/Icon-App-1024x1024@1x.png" "$SITE/icon-1024.png"
# Gliph's type, latin subsets from the web client's own copies (fonts.css).
cp "$ROOT/web/public/fonts/g/abrilfatface-0.woff2" "$SITE/abril.woff2"
cp "$ROOT/web/public/fonts/g/inter-6.woff2" "$SITE/inter.woff2"
cp "$ROOT/web/public/fonts/g/jetbrainsmono-8.woff2" "$SITE/mono.woff2"

cat > "$SITE/manifest.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>items</key><array><dict>
<key>assets</key><array><dict><key>kind</key><string>software-package</string><key>url</key><string>$BASE/Gliph.ipa?$COMMIT-$BUILD</string></dict><dict><key>kind</key><string>display-image</string><key>url</key><string>$BASE/icon.png</string></dict><dict><key>kind</key><string>full-size-image</string><key>url</key><string>$BASE/icon-1024.png</string></dict></array>
<key>metadata</key><dict><key>bundle-identifier</key><string>chat.gliph.mobile</string><key>bundle-version</key><string>$BUILD</string><key>kind</key><string>software</string><key>title</key><string>Gliph</string></dict>
</dict></array></dict></plist>
PLIST

SIZE=$(du -m "$SITE/Gliph.ipa" | cut -f1)
cat > "$SITE/index.html" <<HTML
<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1"><meta name=theme-color content="#1a2138"><title>Install Gliph</title><style>@font-face{font-family:"Abril Fatface";src:url(abril.woff2) format("woff2")}@font-face{font-family:Inter;font-weight:100 900;src:url(inter.woff2) format("woff2")}@font-face{font-family:"JetBrains Mono";src:url(mono.woff2) format("woff2")}.mono{font-family:"JetBrains Mono",ui-monospace,Menlo,monospace;font-size:13px}.pri{display:inline-block;background:#5b86d6;color:#f5f1e8;padding:10px 24px;border-radius:9999px;font-weight:600;text-decoration:none;box-shadow:inset 0 1px 0 rgb(255 255 255/22%);transition:transform .07s ease,background .18s ease}.pri:hover{background:color-mix(in srgb,#5b86d6 82%,#1a2138)}.pri:active{transform:scale(.965)}</style><link rel=apple-touch-icon href=icon.png>
<body style="margin:0;min-height:100vh;display:grid;place-items:center;text-align:center;background:#e9e3d2;color:#23262e;font:15px/1.5 Inter,system-ui,-apple-system,sans-serif">
<main style="background:#f5f1e8;border:1.5px solid #c2bba6;border-radius:18px;padding:32px 40px;margin:16px"><img src="icon.png" width=96 height=96 alt="" style="border-radius:22px;display:block;margin:0 auto 12px"><h1 style="font:400 34px/1 'Abril Fatface',Georgia,serif;color:#d94f75;margin:0 0 6px">gliph</h1><p class=mono style="color:#383c4a;margin:0 0 20px">$VERSION ($BUILD) · $COMMIT · $SIZE MB</p>
<a class=pri href="itms-services://?action=download-manifest&amp;url=$BASE/manifest.plist">Install</a>
<p style="color:#a29c88;font-size:13px;margin:16px 0 0">Keep Tailscale on until the install finishes.</p></main>
HTML

# The old server holds the deleted directory open, so it has to go.
pkill -f "http.server $LOCAL_PORT" 2>/dev/null || true
nohup python3 -m http.server "$LOCAL_PORT" --bind 127.0.0.1 --directory "$SITE" >"$SITE.log" 2>&1 </dev/null &
disown
# Check the server answers with this build's page before the link goes out.
for _ in $(seq 20); do
  curl -fs "http://127.0.0.1:$LOCAL_PORT/manifest.plist" | grep -q "$COMMIT-$BUILD" && break
  sleep 0.25
done
curl -fs "http://127.0.0.1:$LOCAL_PORT/manifest.plist" | grep -q "$COMMIT-$BUILD" \
  || { echo "the server on 127.0.0.1:$LOCAL_PORT is not serving this build (log: $SITE.log)" >&2; exit 1; }
"$TS" serve --bg --https="$HTTPS_PORT" "http://127.0.0.1:$LOCAL_PORT" >/dev/null
echo "devices in the profile: ${DEVICES:-none (not a development profile?)}"
echo "$BASE/"
