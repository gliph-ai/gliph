#!/usr/bin/env bash
# Build the iOS app and put it on TestFlight.
#
#   mobile/tools/testflight.sh            # archive, export, upload
#   mobile/tools/testflight.sh export     # stop after the .ipa
#
# Auth is the App Store Connect API key already on this Mac (the one the lugha
# releases use): ~/.appstoreconnect/private_keys/AuthKey_<KEY_ID>.p8. Nothing
# here needs a password prompt or an Xcode window.
#
# The app record itself cannot be created from here — the API answers
# "The resource 'apps' does not allow 'CREATE'". Make it once at
# appstoreconnect.apple.com (iOS, bundle id chat.gliph.mobile) and this script
# works from then on.
#
# Prerequisites: xcodegen (brew install xcodegen) and a Rust toolchain with the
# aarch64-apple-ios targets — the app links the shared core.
#
# ponytail: build number = highest on App Store Connect + 1; no release notes, no submission — TestFlight
# builds are throwaway. When gliph goes to the App Store, copy the shape of
# lugha's tools/asc.rb instead of growing this one.
set -euo pipefail

KEY_ID="${ASC_KEY_ID:-H9PLG58DB9}"
ISSUER="${ASC_ISSUER:-d5f5385b-5e34-4d70-925d-0debd3a3e090}"
KEY="$HOME/.appstoreconnect/private_keys/AuthKey_$KEY_ID.p8"
ROOT="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="${TMPDIR:-/tmp}/gliph-testflight"
AUTH=(-allowProvisioningUpdates -authenticationKeyPath "$KEY"
      -authenticationKeyID "$KEY_ID" -authenticationKeyIssuerID "$ISSUER")
# A shipped build is pinned to prod: the app has no page origin to fall back on,
# so without this it opens on an empty server field.
SERVER="${GLIPH_SERVER:-https://gliph.sh}"

[ -f "$KEY" ] || { echo "no ASC key at $KEY" >&2; exit 1; }

cd "$ROOT/mobile"
# The Xcode project is generated (project.yml).
xcodegen generate
# And so is the core. The project has a pre-build phase that does this too, for
# a developer hitting Cmd-R — but Xcode PLANS a build before running that phase,
# so a core change that adds a method needs two builds there. Doing it here
# first makes a release build deterministic.
tools/build-core.sh

rm -rf "$OUT"; mkdir -p "$OUT"
cat > "$OUT/ExportOptions.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>method</key><string>app-store-connect</string>
	<key>teamID</key><string>KF53B4S5HD</string>
	<key>signingStyle</key><string>automatic</string>
	<key>uploadSymbols</key><true/>
	<key>destination</key><string>export</string>
</dict></plist>
PLIST

# Every upload needs a build number App Store Connect has not seen, or it is
# accepted by altool and then silently discarded. Ask it for the highest one
# so far and take the next (BUILD_NUMBER overrides). Fails the run rather
# than guess.
next_build() {
  KEY="$KEY" KEY_ID="$KEY_ID" ISSUER="$ISSUER" ruby -rjwt -rnet/http -rjson -ropenssl -e '
    tok = JWT.encode({ iss: ENV["ISSUER"], exp: Time.now.to_i + 600, aud: "appstoreconnect-v1" },
                     OpenSSL::PKey.read(File.read(ENV["KEY"])), "ES256", { kid: ENV["KEY_ID"], typ: "JWT" })
    url = "https://api.appstoreconnect.apple.com/v1/builds?filter[app]=6805549904&limit=200&fields[builds]=version"
    nums = []
    while url  # every page, not only the first
      uri = URI(url)
      req = Net::HTTP::Get.new(uri); req["Authorization"] = "Bearer #{tok}"
      res = Net::HTTP.start(uri.host, uri.port, use_ssl: true) { |h| h.request(req) }
      abort "App Store Connect: #{res.code}" unless res.code == "200"
      body = JSON.parse(res.body)
      nums += body["data"].map { |b| b["attributes"]["version"].split(".").first.to_i }
      url = body.dig("links", "next")
    end
    puts nums.max || 0'
}
# A build still processing is not listed yet, so this Mac also remembers the
# last number it uploaded; the next is above both.
LAST_FILE="$HOME/.gliph-testflight-build"
if [ -z "${BUILD_NUMBER:-}" ]; then
  asc=$(next_build)
  last=$(cat "$LAST_FILE" 2>/dev/null || echo 0)
  BUILD_NUMBER=$(( (asc > last ? asc : last) + 1 ))
fi
# Keep the high-water mark: an override below it must not lower it.
prev=$(cat "$LAST_FILE" 2>/dev/null || echo 0)
echo $(( ${BUILD_NUMBER%%.*} > prev ? ${BUILD_NUMBER%%.*} : prev )) > "$LAST_FILE"
BUILD="${BUILD_NUMBER:-$(next_build)}"
echo "build number $BUILD"

xcodebuild -project Gliph.xcodeproj -scheme Gliph -configuration Release \
  -destination 'generic/platform=iOS' -archivePath "$OUT/Gliph.xcarchive" \
  GLIPH_SERVER="$SERVER" CURRENT_PROJECT_VERSION="$BUILD" "${AUTH[@]}" archive
xcodebuild -exportArchive -archivePath "$OUT/Gliph.xcarchive" \
  -exportOptionsPlist "$OUT/ExportOptions.plist" -exportPath "$OUT/export" "${AUTH[@]}"
IPA=$(find "$OUT/export" -name '*.ipa' | head -1)
echo "ipa: $IPA"

[ "${1:-}" = "export" ] && exit 0

xcrun altool --upload-app --type ios -f "$IPA" \
  --apiKey "$KEY_ID" --apiIssuer "$ISSUER"
echo "uploaded — processing takes ~10 minutes before it appears in TestFlight"
