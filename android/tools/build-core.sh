#!/usr/bin/env bash
# Build gliph-core for Android and generate its Kotlin bindings.
#
#   android/tools/build-core.sh
#
# The Android twin of mobile/tools/build-core.sh. Produces
#   android/core/jniLibs/<abi>/libgliph_core.so   (arm64 phones, x86_64 emulators)
#   android/core/kotlin/uniffi/gliph_core/gliph_core.kt
# Both are build artifacts and neither is committed, for the reason the iOS
# script gives: a checked-in binding goes stale unnoticed, and this one holds
# the encryption. The Gradle build runs this itself before compiling.
set -euo pipefail

ROOT="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CORE="$ROOT/core"
OUT="$ROOT/android/core"
SDK="${ANDROID_HOME:-$HOME/Library/Android/sdk}"
if [ -z "${ANDROID_NDK_HOME:-}" ]; then
  ANDROID_NDK_HOME="$(ls -d "$SDK"/ndk/* 2>/dev/null | sort -V | tail -1)"
  export ANDROID_NDK_HOME
fi
[ -x "$HOME/.cargo/bin/cargo" ] && export PATH="$HOME/.cargo/bin:$PATH"

cd "$CORE"
# 26 is the app's minSdk; the NDK links against that API level.
cargo ndk -P 26 -t arm64-v8a -t x86_64 -o "$OUT/jniLibs" build --release --features ffi

# Generated from the built library rather than a UDL, so the Kotlin and the
# Rust cannot describe different APIs.
rm -rf "$OUT/kotlin"
cargo run --release --features ffi --bin uniffi-bindgen -- \
  generate --library "target/aarch64-linux-android/release/libgliph_core.so" \
  --language kotlin --no-format --out-dir "$OUT/kotlin"

echo "core: $OUT/jniLibs + $OUT/kotlin"
