# syntax=docker/dockerfile:1.7
#
# Foxy-switcher cloud vault — release-driven build.
#
# Stage 1 (fetch):   alpine + curl downloads the linux/amd64 binary
#                    from the configured GitHub Release. The Release CI
#                    (release.yml::release-linux) bakes the React panel
#                    into //go:embed and ships the static binary, so
#                    nothing is built at image-build time.
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
#
# Run on muvee: workspace is auto-mounted, no -v needed.
#
# Run on plain docker (terminate TLS in front with caddy/traefik):
#   docker run --rm -p 8080:8080 -v foxy-vault-data:/workspace foxy-switcher

ARG VERSION=v1.0.2

# --- stage 1: fetch the released binary ------------------------------------
FROM alpine:3 AS fetch
ARG VERSION
RUN apk add --no-cache curl ca-certificates
# Resolve "latest" lazily so a `docker build` without --build-arg picks
# up whatever tag is current. Pinned versions skip the API call.
# set -eux so a 404 / network failure aborts the layer instead of being
# swallowed by the trailing `|| true`. The previous form chained every
# step with && and ||, which let a failed fetch exit 0 and cache an
# empty layer; downstream COPY then "succeeds" with a missing binary.
#
# The sha256 manifest references the asset by its release name
# (foxy-switcher-linux-amd64), so download under that name, verify in
# place, then rename to /foxy-switcher.
RUN set -eux; \
    if [ "$VERSION" = "latest" ]; then \
      url=$(curl -fsSL https://api.github.com/repos/hoveychen/foxy-switcher/releases/latest \
            | grep -oE '"browser_download_url": *"[^"]+foxy-switcher-linux-amd64"' \
            | head -1 | sed -E 's/.*"(https:[^"]+)"/\1/'); \
    else \
      url="https://github.com/hoveychen/foxy-switcher/releases/download/${VERSION}/foxy-switcher-linux-amd64"; \
    fi; \
    echo "==> fetching $url"; \
    cd /; \
    curl -fL --retry 3 --retry-delay 2 -o foxy-switcher-linux-amd64 "$url"; \
    if curl -fL -o foxy-switcher-linux-amd64.sha256 \
         "https://github.com/hoveychen/foxy-switcher/releases/download/${VERSION}/foxy-switcher-linux-amd64.sha256" 2>/dev/null; then \
      sha256sum -c foxy-switcher-linux-amd64.sha256; \
    else \
      echo "==> sha256 file unavailable for $VERSION, skipping integrity check"; \
    fi; \
    mv foxy-switcher-linux-amd64 /foxy-switcher; \
    chmod +x /foxy-switcher; \
    /foxy-switcher --help 2>/dev/null | head -1 || true

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
