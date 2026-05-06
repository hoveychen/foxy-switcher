# syntax=docker/dockerfile:1.7
#
# Foxy-switcher cloud vault — source build.
#
# Stage 1 (webapp-builder): node + pnpm builds the React panel into dist/.
# Stage 2 (go-builder):     golang cross-compiles foxy-switcher for the
#                           target arch. The webapp dist is copied into
#                           server/vault/webapp/static/ first so //go:embed
#                           bakes it into the binary. Pinned to
#                           $BUILDPLATFORM with GOARCH=$TARGETARCH so the
#                           Go toolchain runs natively (no qemu).
# Stage 3 (runtime):        distroless/static carries the binary, mounts
#                           /workspace for state, exposes 8080.
#
# State (SQLite, leases, sessions, agent-config, password hash) lives in
# /workspace, matching muvee's auto-mounted persistent storage path.
# Outside muvee, mount any volume at /workspace; without persistence every
# redeploy wipes the admin password and every device pairing.
#
# Build:
#   docker build -t foxy-switcher .
#   docker build --build-arg VERSION=v1.0.1 -t foxy-switcher:1.0.1 .
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=v1.0.2 \
#     -t ghcr.io/hoveychen/foxy-switcher:v1.0.2 --push .
#
# Run on muvee: workspace is auto-mounted, no -v needed.
#
# Run on plain docker (terminate TLS in front with caddy/traefik):
#   docker run --rm -p 8080:8080 -v foxy-vault-data:/workspace \
#     ghcr.io/hoveychen/foxy-switcher:latest

ARG VERSION=dev
ARG NODE_VERSION=22
ARG GO_VERSION=1.26
ARG PNPM_VERSION=10

# --- stage 1: build the React webapp ----------------------------------------
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-bookworm-slim AS webapp-builder
ARG PNPM_VERSION
RUN npm install -g pnpm@${PNPM_VERSION}
WORKDIR /app

COPY package.json pnpm-lock.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile

COPY tsconfig.json tsconfig.node.json vite.config.ts index.html ./
COPY src ./src
RUN pnpm build

# --- stage 2: build the Go binary -------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS go-builder
ARG TARGETARCH
ARG VERSION
WORKDIR /src

# Warm the module cache before copying the rest of the tree so unrelated
# source edits don't invalidate `go mod download`.
COPY server/go.mod server/go.sum ./server/
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    cd server && go mod download

COPY server ./server
# Bake the React bundle into the //go:embed directory. server/vault/webapp/static
# already exists in the tree (with .gitkeep); this overlay adds index.html +
# assets/ so the vault binary serves /admin and /app from itself.
COPY --from=webapp-builder /app/dist/ ./server/vault/webapp/static/

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    set -eux; \
    cd server; \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
      go build -trimpath \
        -ldflags="-s -w -X github.com/hoveychen/foxy-switcher/server/deviceinfo.Version=${VERSION}" \
        -o /out/foxy-switcher .

# --- stage 3: distroless runtime --------------------------------------------
FROM gcr.io/distroless/static:latest
COPY --from=go-builder /out/foxy-switcher /usr/local/bin/foxy-switcher

# /workspace is the SQLite + agent-config home. muvee auto-mounts a
# persistent volume here; outside muvee, supply -v foxy-vault-data:/workspace.
# Declared as a VOLUME so a `docker run` without -v gets an anonymous volume
# rather than silently writing into the image layer (which redeploys throw away).
VOLUME ["/workspace"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/foxy-switcher", \
            "--server", \
            "--mode=vault", \
            "--bind-host=0.0.0.0", \
            "--port=8080", \
            "--data-dir=/workspace"]
