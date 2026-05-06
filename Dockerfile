# syntax=docker/dockerfile:1.7
#
# Foxy-switcher cloud vault — release-driven build.
#
# Stage 1 (fetch):   alpine + curl downloads the per-architecture binary
#                    from the configured GitHub Release. The Release CI
#                    (release.yml::release-linux) bakes the React panel
#                    into //go:embed and ships static binaries for both
#                    amd64 and arm64, so nothing is built at image-build
#                    time. The fetch stage runs on $BUILDPLATFORM (no
#                    qemu emulation) and selects the asset to download
#                    based on $TARGETPLATFORM, so a single buildx
#                    invocation can produce a multi-arch manifest.
# Stage 2 (runtime): distroless/static carries the binary, mounts
#                    /workspace for state, exposes 8080.
#
# State (SQLite, leases, sessions, agent-config, password hash) lives
# in /workspace, matching muvee's auto-mounted persistent storage path.
# Outside muvee, mount any volume at /workspace; without persistence
# every redeploy wipes the admin password and every device pairing.
#
# Build:
#   docker build -t foxy-switcher .                      # latest tag
#   docker build --build-arg VERSION=v1.0.1 -t foxy-switcher:1.0.1 .
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=v1.0.2 -t ghcr.io/hoveychen/foxy-switcher:v1.0.2 --push .
#
# Run on muvee: workspace is auto-mounted, no -v needed.
#
# Run on plain docker (terminate TLS in front with caddy/traefik):
#   docker run --rm -p 8080:8080 -v foxy-vault-data:/workspace \
#     ghcr.io/hoveychen/foxy-switcher:latest

ARG VERSION=latest

# --- stage 1: fetch the released binary ------------------------------------
# Pin fetch to $BUILDPLATFORM so curl runs natively on the builder, not
# under qemu emulation when buildx targets a foreign arch. The asset URL
# is selected from $TARGETPLATFORM instead.
FROM --platform=$BUILDPLATFORM alpine:3 AS fetch
ARG VERSION
ARG TARGETPLATFORM
RUN apk add --no-cache curl ca-certificates
# Resolve "latest" lazily so a `docker build` without --build-arg picks
# up whatever tag is current. Pinned versions skip the API call.
# set -eux so a 404 / network failure aborts the layer instead of being
# swallowed by the trailing `|| true`. The previous form chained every
# step with && and ||, which let a failed fetch exit 0 and cache an
# empty layer; downstream COPY then "succeeds" with a missing binary.
#
# The sha256 manifest references the asset by its release name
# (foxy-switcher-linux-<arch>), so download under that name, verify in
# place, then rename to /foxy-switcher.
RUN set -eux; \
    case "$TARGETPLATFORM" in \
      linux/amd64) ARCH=amd64 ;; \
      linux/arm64) ARCH=arm64 ;; \
      "") ARCH=amd64 ;; \
      *) echo "unsupported TARGETPLATFORM=$TARGETPLATFORM" >&2; exit 1 ;; \
    esac; \
    asset="foxy-switcher-linux-${ARCH}"; \
    # Resolve "latest" to a concrete tag once, then use it for both the
    # binary and its sha256 sidecar. Otherwise the sha256 URL would
    # literally say `.../releases/download/latest/...` (404), and the
    # integrity check would silently fall through.
    if [ "$VERSION" = "latest" ]; then \
      VERSION=$(curl -fsSL https://api.github.com/repos/hoveychen/foxy-switcher/releases/latest \
                | grep -oE '"tag_name": *"[^"]+"' \
                | head -1 | sed -E 's/.*"([^"]+)"$/\1/'); \
      [ -n "$VERSION" ] || { echo "could not resolve latest release tag" >&2; exit 1; }; \
      echo "==> resolved latest -> $VERSION"; \
    fi; \
    base="https://github.com/hoveychen/foxy-switcher/releases/download/${VERSION}"; \
    echo "==> fetching ${base}/${asset}"; \
    cd /; \
    curl -fL --retry 3 --retry-delay 2 -o "$asset" "${base}/${asset}"; \
    if curl -fL -o "${asset}.sha256" "${base}/${asset}.sha256" 2>/dev/null; then \
      sha256sum -c "${asset}.sha256"; \
    else \
      echo "==> sha256 file unavailable for $VERSION/$ARCH, skipping integrity check"; \
    fi; \
    mv "$asset" /foxy-switcher; \
    chmod +x /foxy-switcher

# --- stage 2: distroless runtime -------------------------------------------
FROM gcr.io/distroless/static:latest
COPY --from=fetch /foxy-switcher /usr/local/bin/foxy-switcher

# /workspace is the SQLite + agent-config home. muvee auto-mounts a
# persistent volume here; outside muvee, supply -v
# foxy-vault-data:/workspace. Declared as a VOLUME so a `docker run`
# without -v gets an anonymous volume rather than silently writing
# into the image layer (which redeploys throw away).
VOLUME ["/workspace"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/foxy-switcher", \
            "--server", \
            "--mode=vault", \
            "--bind-host=0.0.0.0", \
            "--port=8080", \
            "--data-dir=/workspace"]
