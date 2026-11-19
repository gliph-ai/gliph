#!/usr/bin/env bash
# Build gliph-core for iOS and generate its Swift bindings.
#
#   mobile/tools/build-core.sh
#
# Produces mobile/Generated/ (the UniFFI Swift shim) and mobile/GliphCore.xcframework
# (device + simulator slices). Both are build artifacts and neither is committed:
# a checked-in binding that has to be regenerated whenever core/ changes is one
# that goes stale unnoticed, and this one holds the encryption.
#
# Xcode runs this itself as a pre-build phase, which is enough for a developer
# hitting Cmd-R. It is NOT enough on its own for a clean release build: Xcode
# plans which files to compile before that phase runs, so a core change that
# adds a method compiles Swift against the previous library and fails to link.
# The deploy scripts therefore run this first, explicitly.
set -euo pipefail

ROOT="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CORE="$ROOT/core"
OUT="$ROOT/mobile"
LIB=libgliph_core.a

cd "$CORE"
for target in aarch64-apple-ios aarch64-apple-ios-sim; do
  cargo build --release --features ffi --target "$target"
done

# The bindings are generated from the built library rather than from a UDL, so
# the Swift and the Rust cannot describe different APIs.
rm -rf "$OUT/Generated"
mkdir -p "$OUT/Generated"
cargo run --release --features ffi --bin uniffi-bindgen -- \
  generate --library "target/aarch64-apple-ios-sim/release/$LIB" \
  --language swift --out-dir "$OUT/Generated"

# UniFFI emits `<name>FFI.modulemap`; an XCFramework wants `module.modulemap`,
# and the header it names has to sit beside it.
mkdir -p "$OUT/Generated/include"
mv "$OUT/Generated"/*.h "$OUT/Generated/include/"
mv "$OUT/Generated"/*.modulemap "$OUT/Generated/include/module.modulemap"

rm -rf "$OUT/GliphCore.xcframework"
xcodebuild -create-xcframework \
  -library "target/aarch64-apple-ios/release/$LIB" -headers "$OUT/Generated/include" \
  -library "target/aarch64-apple-ios-sim/release/$LIB" -headers "$OUT/Generated/include" \
  -output "$OUT/GliphCore.xcframework"

echo "core: $OUT/GliphCore.xcframework + $OUT/Generated/gliph_core.swift"
