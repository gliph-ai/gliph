# The Linux build environment for the desktop app. Tauri cannot cross-compile a
# Linux bundle (it links webkit2gtk and runs linuxdeploy), so bin/release-desktop
# runs the Linux step inside this image, as linux/amd64 under Docker Desktop's
# emulation. Built once and cached; the Rust target dir and cargo registry live
# in named volumes so a rebuild is incremental.
#
#   docker build --platform linux/amd64 -t gliph-linux-builder -f desktop/linux-builder.Dockerfile desktop
FROM rust:1.90-bookworm
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
    libwebkit2gtk-4.1-dev build-essential curl wget file libxdo-dev libssl-dev \
    libayatana-appindicator3-dev librsvg2-dev patchelf ca-certificates xz-utils \
    && rm -rf /var/lib/apt/lists/*
# Node only for the Tauri CLI: the web client is built on the Mac and mounted in.
RUN curl -fsSL https://nodejs.org/dist/v24.18.0/node-v24.18.0-linux-x64.tar.xz \
    | tar -xJ -C /usr/local --strip-components=1 \
    && npm i -g @tauri-apps/cli@^2.11.4
ENV NO_STRIP=true CARGO_TARGET_DIR=/target
# Non-root (Trivy DS-0002). The named volumes (/target, cargo registry, cache)
# inherit this ownership when first created; volumes created by the old root
# image must be `docker volume rm`'d once so they re-initialize as builder.
RUN useradd -m builder && mkdir -p /target /usr/local/cargo/registry /home/builder/.cache \
    && chown -R builder /target /usr/local/cargo /usr/local/rustup /home/builder/.cache
USER builder
WORKDIR /src/desktop
