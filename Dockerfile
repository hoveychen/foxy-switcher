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

ARG VERSION=v1.0.1

# --- stage 1: fetch the released binary ------------------------------------
FROM alpine:3 AS fetch
ARG VERSION
RUN apk add --no-cache curl ca-certificates
# Resolve "latest" lazily so a `docker build` without --build-arg picks
# up whatever tag is current. Pinned versions skip the API call.
RUN if [ "$VERSION" = "latest" ]; then \
      url=$(curl -fsSL https://api.github.com/repos/hoveychen/foxy-switcher/releases/latest \
            | grep -oE '"browser_download_url": *"[^"]+foxy-switcher-linux-amd64"' \
            | head -1 | sed -E 's/.*"(https:[^"]+)"/\1/'); \
    else \
      url="https://github.com/hoveychen/foxy-switcher/releases/download/${VERSION}/foxy-switcher-linux-amd64"; \
    fi \
 && echo "==> fetching $url" \
 && curl -fL --retry 3 --retry-delay 2 -o /foxy-switcher "$url" \
 && curl -fL -o /foxy-switcher.sha256 \
      "https://github.com/hoveychen/foxy-switcher/releases/download/${VERSION}/foxy-switcher-linux-amd64.sha256" 2>/dev/null \
   && (cd / && sha256sum -c foxy-switcher.sha256) \
   || echo "==> sha256 file unavailable for $VERSION, skipping integrity check" \
 && chmod +x /foxy-switcher \
 && /foxy-switcher --help 2>/dev/null | head -1 || true

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
