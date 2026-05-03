# syntax=docker/dockerfile:1.7
#
# Foxy-switcher cloud vault — single-image build.
#
# Stage 1 (webapp):  pnpm + vite produce dist/.
# Stage 2 (server):  Go bakes dist/ into //go:embed and links a static
#                    linux/amd64 binary (CGO=0, modernc.org/sqlite).
# Stage 3 (runtime): distroless/static; one ENTRYPOINT, one volume.
#
# State (SQLite + lease + sessions + agent-config) lives in /data;
# mount a named volume there so a container restart doesn't drop the
# admin password and every device pairing.
#
# Build:
#   docker build -t foxy-switcher .
#
# Run (vault on :8080, terminate TLS in front with caddy/traefik):
#   docker run --rm -p 8080:8080 -v foxy-vault-data:/data foxy-switcher
#
# Override the mode / flags by appending args after the image name:
#   docker run --rm -v foxy-vault-data:/data foxy-switcher \
#     --server --mode=combined --bind-host=127.0.0.1 --port=8080

# --- stage 1: React webapp --------------------------------------------------
FROM node:22-alpine AS webapp
WORKDIR /src
RUN corepack enable && corepack prepare pnpm@10 --activate

# Layer the lockfile install separately so source edits don't bust the
# npm cache. dist/ is the only output that survives into stage 2.
COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY index.html tsconfig.json tsconfig.node.json vite.config.ts ./
COPY src ./src
RUN pnpm build

# --- stage 2: Go server with embedded webapp -------------------------------
FROM golang:1.26-alpine AS server
WORKDIR /src

# go.mod / go.sum first so go mod download caches when only Go sources change.
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download

# Server source.
COPY server ./server

# Bake the React build into the embed directory before `go build`. The
# Go-side //go:embed all:static directive picks up whatever lands here.
COPY --from=webapp /src/dist /tmp/dist
RUN rm -rf server/vault/webapp/static \
 && mkdir -p server/vault/webapp/static \
 && cp -R /tmp/dist/. server/vault/webapp/static/ \
 && : > server/vault/webapp/static/.gitkeep \
 && ls -1 server/vault/webapp/static | head -5

ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN cd server \
 && go build -trimpath -ldflags="-s -w" \
       -o /out/foxy-switcher .

# --- stage 3: distroless runtime -------------------------------------------
FROM gcr.io/distroless/static:latest
COPY --from=server /out/foxy-switcher /usr/local/bin/foxy-switcher

# /data is the SQLite + agent-config home. The image declares it so
# `docker run` without `-v` still works (an anonymous volume gets
# allocated); production should map a named volume here.
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/foxy-switcher", \
            "--server", \
            "--mode=vault", \
            "--bind-host=0.0.0.0", \
            "--port=8080", \
            "--data-dir=/data"]
