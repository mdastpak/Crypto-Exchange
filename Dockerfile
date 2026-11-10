# syntax=docker/dockerfile:1

# GO_VERSION must match the go directive in go.mod (major.minor); CI checks it.
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Download modules first, so that they are cached until go.mod or go.sum
# change. The cache mounts keep modules and build results across builds
# without adding them to any layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && go mod verify

# .dockerignore keeps secrets, local configuration, and keys out of this copy.
COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags="-s -w" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags="-s -w" -o /out/admin ./cmd/admin && \
    CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags="-s -w" -o /out/market-worker ./cmd/market-worker

# The runtime stage installs no packages: the base image already has the CA
# certificates, and the timetzdata tag embeds the time zone database in the
# binaries.
FROM alpine:3.24

RUN addgroup -g 10001 -S app && \
    adduser -u 10001 -S -G app -H -s /sbin/nologin app

WORKDIR /app

# Only the binaries (the server, cmd/admin, and cmd/market-worker, which
# runs with --entrypoint /app/market-worker) and the migrations, which the server reads from
# internal/db/migrations relative to /app. Configuration and keys are mounted
# at runtime and never built into the image. Without a configuration file the
# server falls back to the embedded example configuration.
COPY --from=builder /out/server /out/admin /out/market-worker /app/
COPY --from=builder /src/internal/db/migrations /app/internal/db/migrations

USER 10001:10001

# The image runs in release mode unless told otherwise (#306): an instance
# whose deployment forgot SERVER_MODE gets the release checks, never debug.
# Development sets SERVER_MODE=debug (docker-compose.yml does).
ENV SERVER_MODE=release

EXPOSE 8080

# /system/ready answers 503 while PostgreSQL or Redis is unavailable, and
# wget fails on it; without RabbitMQ it answers 200, degraded (#232).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/system/ready || exit 1

ENTRYPOINT ["/app/server"]
