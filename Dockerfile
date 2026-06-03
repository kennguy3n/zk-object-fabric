# syntax=docker/dockerfile:1.7

# ---------------------------------------------------------------
# Stage 1 — Go build
# ---------------------------------------------------------------
# The go.mod in this repo pins the toolchain to Go 1.25, so match
# that in the build image. An older alpine tag would fail with
# "go.mod requires go >= 1.25" on `go build`.
FROM golang:1.25-alpine AS gateway-build

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Copy dependency manifests first so the module-download layer
# caches independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the Go tree. `.dockerignore` keeps `frontend/`,
# `tests/`, `.git`, and similar non-build inputs out of this
# context.
COPY . .

# Build a statically linked gateway binary so stage 3's minimal
# Alpine runtime does not need the Go toolchain or libc shims.
#
# Version metadata is stamped into the binary via -ldflags -X.
# Callers (CI, Makefile, manual `docker build`) pass three build
# args. The Dockerfile cannot resolve VERSION/GIT_COMMIT itself —
# .dockerignore deliberately excludes .git from the build context
# (see .dockerignore line 2) so `git rev-parse HEAD` cannot run
# from inside the image build. The CI pipeline resolves them in a
# pre-build step (see .github/workflows/ci.yml "Resolve version
# metadata") and passes the resolved triple to docker/build-push-action
# via build-args. A manual `docker build` without --build-arg
# falls back to the ARG defaults below ("0.0.0-dev" / "unknown"),
# which keeps dev builds runnable but reports the dev-default
# version at /internal/version. BUILD_DATE is also resolved
# outside the image at RFC 3339 UTC so two builds at the same
# wall-clock second from different time zones still produce
# identical ldflags and the gateway binary is byte-identical.
ARG VERSION=0.0.0-dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath \
    -ldflags="-s -w \
      -X github.com/kennguy3n/zk-object-fabric/internal/version.Version=${VERSION} \
      -X github.com/kennguy3n/zk-object-fabric/internal/version.GitCommit=${GIT_COMMIT} \
      -X github.com/kennguy3n/zk-object-fabric/internal/version.BuildDate=${BUILD_DATE}" \
    -o /out/gateway ./cmd/gateway

# ---------------------------------------------------------------
# Stage 2 — Frontend build
# ---------------------------------------------------------------
# The React / Vite console lives under frontend/. It ships as a
# static bundle served separately from the Go API; the demo
# runtime image carries the dist/ tree under /app/frontend so a
# reverse-proxy can be slotted in later without rebuilding the
# gateway.
FROM node:20-alpine AS frontend-build

WORKDIR /src/frontend

COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci

COPY frontend/ ./
RUN npm run build

# ---------------------------------------------------------------
# Stage 3 — Runtime
# ---------------------------------------------------------------
# Alpine carries ca-certificates, a minimal tzdata, and wget for
# compose healthchecks. The gateway binary is static so no libc
# shims are needed.
FROM alpine:3.20 AS runtime

# envsubst ships in gettext; the entrypoint uses it to render
# demo/tenants.json from demo/tenants.json.tmpl so the repo never
# carries literal HMAC credentials.
RUN apk add --no-cache ca-certificates tzdata wget gettext

# Object data persists under /data/objects (see demo/config.json's
# providers.local_fs_dev.root_path); the embedded SQLite metadata
# store lives under /data/metadata (control_plane.embedded_db_path).
# docker-compose.yml mounts a named volume at each path so object
# bodies AND control-plane state survive container restarts. The
# gateway also creates /data/metadata on demand, but pre-creating it
# keeps a bare `docker run` (no compose volume) working too.
RUN mkdir -p /data/objects /data/metadata /app/demo /app/frontend /run/zk-fabric

COPY --from=gateway-build /out/gateway /usr/local/bin/gateway
COPY --from=frontend-build /src/frontend/dist /app/frontend
COPY demo /app/demo
RUN chmod +x /app/demo/entrypoint.sh

WORKDIR /app

EXPOSE 8080 8081

ENTRYPOINT ["/app/demo/entrypoint.sh"]
