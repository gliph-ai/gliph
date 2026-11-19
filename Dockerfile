# Gliph server + web client in one image: the Go binary serves /api, /ws and
# the built client off the same origin, so a deployment is one container.
#
# Both builder stages run on the BUILDPLATFORM (native on an arm64 mac) and the
# Go build cross-compiles to TARGETARCH — emulating node under qemu to build the
# client would take minutes instead of seconds. modernc.org/sqlite is pure Go,
# so CGO stays off and the binary is static.

# The client core (protocol, crypto, sync) is Rust, compiled to wasm and shared
# with the iOS app. Built here rather than committed: a checked-in binary that
# has to be regenerated whenever core/ changes is a binary that goes stale
# without anyone noticing, and this one holds the encryption.
FROM --platform=$BUILDPLATFORM rust:1.90-alpine AS core
RUN apk add --no-cache musl-dev curl bash && \
    curl -sSf https://rustwasm.github.io/wasm-pack/installer/init.sh | sh
WORKDIR /core
COPY core/Cargo.toml core/Cargo.lock ./
# wasm-pack fetches a PREBUILT wasm-bindgen, and there is no aarch64-musl build
# of it: on an arm64 builder the download lands as something that cannot be
# executed and the whole stage dies with a bare "No such file or directory".
# Build it from source instead, at the version the lockfile pins — so the CLI
# and the crate can never drift, and the image builds on any machine rather
# than only on an amd64 CI runner. Cached above the sources, so it compiles
# once and not on every push.
RUN v=$(awk '/^name = "wasm-bindgen"$/{getline; gsub(/[",]/,"",$3); print $3; exit}' Cargo.lock) &&     cargo install wasm-bindgen-cli --locked --version "$v"
COPY core/src ./src
COPY core/tests ./tests
RUN wasm-pack build --release --target web --out-dir /out --out-name gliph_core -- --features wasm

FROM --platform=$BUILDPLATFORM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
COPY --from=core /out ./src/core
# Pins the server the shipped client talks to. See web/vite.config.ts.
ARG GLIPH_SERVER=""
# `bin/version --long`, stamped into the client and the server so /status can
# say which commit is live. The image has no .git to derive it from.
ARG GLIPH_VERSION=dev
# `npm run build` would rebuild the core; the wasm is already above.
RUN GLIPH_SERVER="$GLIPH_SERVER" GLIPH_VERSION="$GLIPH_VERSION" npx tsc --noEmit && \
    GLIPH_SERVER="$GLIPH_SERVER" GLIPH_VERSION="$GLIPH_VERSION" npx vite build && \
    npm run check

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
ARG TARGETARCH
ARG GLIPH_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$GLIPH_VERSION" -o /out/gliph .

FROM alpine:3.22
# Trivy scans fixable HIGH/CRITICAL CVEs in Alpine's own packages; upgrade at
# build time so a fix lands the same day rather than whenever this base image
# tag next rebuilds upstream. CI passes the pipeline id so the layer cache
# never serves a stale upgrade (see .github/workflows/release.yml).
ARG APK_UPGRADE_AT=local
# Pushleft passes the UTC date, so the upgrade reruns daily rather than from cache.
ARG PUSHLEFT_OS_REFRESH
RUN echo "apk upgrade at $APK_UPGRADE_AT $PUSHLEFT_OS_REFRESH" && apk upgrade --no-cache && apk add --no-cache ca-certificates coturn
COPY --from=build /out/gliph /usr/local/bin/gliph
COPY --from=web /web/dist /srv/web
# Runs as an unprivileged user (uid 10001), the web role and the turn role
# alike. Pushleft creates a volume's data directory 0777 and the role writes
# only there and in its tmpfs /tmp; every port it listens on is above 1024.
# The production /data volume was handed to 10001:10001 on 2026-11-07 (it was
# owned by the importing Mac's uid 501, so even root could not write it).
RUN addgroup -S -g 10001 gliph && adduser -S -D -H -u 10001 -G gliph gliph && \
    mkdir -p /data && chown gliph:gliph /data
WORKDIR /data
USER 10001:10001
EXPOSE 8787
ENTRYPOINT ["gliph"]
CMD ["-addr", ":8787", "-data", "/data", "-web", "/srv/web"]
