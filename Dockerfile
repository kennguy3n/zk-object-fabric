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
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

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
# providers.local_fs_dev.root_path). docker-compose.yml mounts a
# named volume here so object bodies survive container restarts.
RUN mkdir -p /data/objects /app/demo /app/frontend /run/zk-fabric

COPY --from=gateway-build /out/gateway /usr/local/bin/gateway
COPY --from=frontend-build /src/frontend/dist /app/frontend
COPY demo /app/demo
RUN chmod +x /app/demo/entrypoint.sh

WORKDIR /app

EXPOSE 8080 8081

ENTRYPOINT ["/app/demo/entrypoint.sh"]
