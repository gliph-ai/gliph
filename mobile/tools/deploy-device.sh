#!/usr/bin/env bash
# Build Gliph for a real iPhone and install it over the air.
#
#   mobile/tools/deploy-device.sh                 # first paired device
#   mobile/tools/deploy-device.sh "Waseem's iPhone"
#   GLIPH_CONFIG=Debug mobile/tools/deploy-device.sh
#   mobile/tools/deploy-device.sh --no-install    # build only, for publish-ipad.sh
# GLIPH_BUILD sets the build number (CFBundleVersion); it defaults to 1.
#
# Installs with `devicectl`, which goes over Wi-Fi when the phone is network
# paired, so no cable is needed. This is a DEVELOPMENT build signed for that
# device, separate from TestFlight (mobile/tools/testflight.sh) — use it when
# waiting on Apple's beta review would be silly.
#
# ponytail: no manifest plist, no itms-services:// host, no ad-hoc IPA. That
# whole apparatus exists to install on a phone you cannot reach; this one is
# on the same network and already paired.
set -euo pipefail

KEY_ID="${ASC_KEY_ID:-H9PLG58DB9}"
ISSUER="${ASC_ISSUER:-d5f5385b-5e34-4d70-925d-0debd3a3e090}"
KEY="$HOME/.appstoreconnect/private_keys/AuthKey_$KEY_ID.p8"
ROOT="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CONFIG="${GLIPH_CONFIG:-Release}"
BUNDLE_ID="chat.gliph.mobile"
# A shipped build is pinned to prod: the app has no page origin to fall back on,
# so without this it opens on an empty server field (it lands in Info.plist as
# GliphServer). Triple-tap the logo on the sign-in screen to point a shipped
# build somewhere else.
SERVER="${GLIPH_SERVER:-https://gliph.sh}"

INSTALL=yes
[ "${1:-}" = --no-install ] && { INSTALL=no; shift; }

# Pick the device: the one named on the command line, else the first available.
want="${1:-}"
if [ "$INSTALL" = yes ]; then
# `|| true`: with set -e, a grep that matches nothing kills the script here,
# before the guard below gets to say why — which is how a sleeping phone
# looked like a silent success.
line=$(xcrun devicectl list devices 2>/dev/null | grep "available (paired)" | { [ -n "$want" ] && grep -F "$want" || cat; } | head -1) || true
[ -n "$line" ] || { echo "no paired device found — pair the phone in Xcode > Devices first" >&2; exit 1; }
# The identifier: a CoreDevice UUID on older Xcodes, the hardware UDID
# (00008140-000239C63CC2801C) on newer ones. Match either shape.
DEVICE=$(echo "$line" | grep -oE '[0-9A-F]{8}-([0-9A-F]{4}-){3}[0-9A-F]{12}|[0-9A-F]{8}-[0-9A-F]{16}' | head -1) || true
[ -n "$DEVICE" ] || { echo "could not read the device id from: $line" >&2; exit 1; }
echo "device: $(echo "$line" | sed 's/  .*//') ($DEVICE)"
fi

[ -f "$KEY" ] || { echo "no ASC key at $KEY" >&2; exit 1; }

cd "$ROOT/mobile"
# The Xcode project is generated (project.yml).
xcodegen generate
# And so is the core. The project has a pre-build phase that does this too, for
# a developer hitting Cmd-R — but Xcode PLANS a build before running that phase,
# so a core change that adds a method needs two builds there. Doing it here
# first makes a release build deterministic.
tools/build-core.sh

OUT="${TMPDIR:-/tmp}/gliph-device"
rm -rf "$OUT"
xcodebuild -project Gliph.xcodeproj -scheme Gliph -configuration "$CONFIG" \
  GLIPH_SERVER="$SERVER" CURRENT_PROJECT_VERSION="${GLIPH_BUILD:-1}" \
  -destination "generic/platform=iOS" -derivedDataPath "$OUT" \
  -allowProvisioningUpdates -authenticationKeyPath "$KEY" \
  -authenticationKeyID "$KEY_ID" -authenticationKeyIssuerID "$ISSUER" build

APP="$OUT/Build/Products/$CONFIG-iphoneos/Gliph.app"
[ -d "$APP" ] || { echo "no app at $APP" >&2; exit 1; }

[ "$INSTALL" = yes ] || { echo "built $APP"; exit 0; }
xcrun devicectl device install app --device "$DEVICE" "$APP"
xcrun devicectl device process launch --device "$DEVICE" "$BUNDLE_ID" || true
echo "installed on the phone"
